# AgentCrate 架构设计

> 版本：draft-1（2026-10-30）。本文是实现的唯一权威设计，代码与设计冲突时先改设计或先提 issue。

## 1. 定位

AgentCrate 是部署在 **Agent 应用** 与 **工具（MCP servers / 本地工具 / HTTP API）** 之间的**安全执行层**：

- 上游：任何会说 MCP 或 function-calling 的 agent 运行时（Claude Code、Dify、自研 agent）
- 下游：MCP servers、容器化工具、HTTP API
- 本体：接入网关 + 注册中心 + 沙箱运行时 + 可观测管线

它**不是** agent 编排框架，不做任务规划，不碰 prompt。只做三件事：**工具调用的准入、执行、记账**。

定位锚点（竞品扫描结论，2026-10）：LLM API 网关有 LiteLLM / one-api，agent 路由与治理有 agentgateway（CNCF），纯沙箱有 agent-sandbox 系列（kubernetes-sigs 等）——但把「准入、隔离执行、记账」做成一体化执行层仍是空档。AgentCrate 以沙箱运行时为核心，网关只是它的输入面。

## 2. 模块划分

```
agentcrate/
├── cmd/agentcrate/          # 入口：单进程跑全部模块，也可 --modules 拆分部署
├── internal/
│   ├── gateway/            # 接入层：认证、多租户、限流熔断
│   ├── registry/           # MCP server 注册与工具目录、scope 声明
│   ├── authz/              # 调用级权限校验（scope + 租户策略）
│   ├── audit/              # 授权决策审计（JSONL，allow/deny 都记）
│   ├── router/             # 工具路由：本地直调 or 投递沙箱
│   ├── sandbox/            # 容器执行器（Docker / 独立进程双后端）
│   ├── observe/            # OTel spans/metrics 导出
│   └── ledger/             # 成本台账：单价表、记账、配额
├── proto/                  # 内部 gRPC 契约（gateway↔sandbox）
└── docs/
```

## 3. 核心数据流

一次工具调用的完整生命周期：

```
1. agent → gateway : tools/call {tool, args, tenant, runId}
2. authz           : 查 registry 中该工具声明的 scopes
                     ∩ 租户策略允许的 scopes → allow / deny(记审计)
3. router          : 无 side-effect 纯查询 → 进程内直调
                     不可信/有副作用 → 投递 sandbox
4. sandbox         : 起容器（镜像缓存、限额 cpu/mem/net）→ 执行 → 回收
5. observe         : 每个 span: authorize / route / exec
                     metrics: 成功率、P50/P99、并发、重试
6. ledger          : 若调用触发上游 LLM/HTTP 成本 → 按单价表记账
7. agent ← gateway : 结果 + traceId（方便用户反查全链路）
```

设计原则：**每一步都可拒绝、可观测、可回放**。deny 也是一条一等公民记录。

## 4. 关键接口（Go 草案）

```go
// registry：工具目录条目。scope 由 MCP server 注册时声明，网关不猜。
type Tool struct {
    Name       string   // "mcp:filesystem.read"
    ServerID   string
    Scopes     []string // "fs:read", "net:egress:api.github.com"
    SideEffect bool     // true 则强制走沙箱
    Timeout    time.Duration
}

// authz：调用级决策。deny 必须带 reason，进审计日志。
type Authorizer interface {
    Authorize(ctx context.Context, req CallReq, tool Tool) Decision
}

// sandbox：执行后端。DockerBackend / ProcessBackend 双实现。
type Sandbox interface {
    Exec(ctx context.Context, job Job) (Result, error)
}

// ledger：记账。单价表来源可插拔（内置 yaml，未来接各家 pricing API）。
type Ledger interface {
    Record(ctx context.Context, e UsageEvent) error
    Balance(tenant string) (UsageSummary, error)
}
```

## 5. 安全模型

- **scope 声明制**：工具能力最小化声明，默认 deny；`SideEffect` 工具永不直调宿主。
- **审计不可绕过**：所有 allow/deny 落审计日志（结构化 JSON，含 runId/traceId/租户），这是给「Agent 治理」场景的第一交付物。
- **沙箱边界**：容器无宿主挂载、egress 白名单（按 scope 展开）、cpu/mem/pid 限额、限时强杀。
- **信任链明确**：网关信任 agent 的身份，不信任工具的内容——所以执行必须隔离、输出视为不可信数据。

### M2 实现基线（已落地）

策略文件由 `-policy` 指定（缺省用内置默认策略）：

```yaml
tenants:
  default:
    scopes: ["fs:read", "net:egress"]
tool_scopes:
  read_file: ["fs:read"]
  write_file: ["fs:write"]
  fetch: ["net:egress"]
```

- **决策规则**：工具未声明 scope → deny；租户与工具 scope 交集为空 → deny（reason 列出缺失 scope）；否则 allow。工具名取网关对外暴露名，租户身份 M2 阶段固定为 `default`（身份认证属后续里程碑）。
- **审计**：`-audit` 指定 JSONL 路径（默认 `audit.jsonl`），每条含 `ts/traceId/tenant/tool/downstream/decision/reason`；allow 与 deny 都记，写盘失败只记日志、不阻断调用。
- **deny 的可观测性**：`authorize` span 带 `agentcrate.decision` / `agentcrate.reason` 属性；`agentcrate_calls_total` 增加 `outcome="denied"` 桶；MCP 侧以 `IsError=true` 的工具结果返回，agent 能读到拒绝原因。
- **升级路径**：scope 声明目前在策略文件里；后续可从 MCP server 注册声明（registry 的 `Tool.Scopes`）自动推导，策略只保留租户侧授权。

### M3 实现基线（已落地：进程后端）

- **入口**：内置工具 `sandbox_exec`，参数 `argv`（命令数组）+ `timeout_seconds`；默认策略给它 `sandbox:exec` scope，走同一套 authorize → 审计 → 指标链路。
- **隔离手段**（进程后端 `internal/sandbox`）：每次执行建独立临时工作目录并在结束后删除；宿主机环境变量只透白名单（PATH/TEMP/HOME 等，密钥类一律不继承）；超时连整棵进程树一起杀（Windows `taskkill /T /F`），并用 `WaitDelay` 兜底防止孙进程占管道导致 `Wait` 挂死；stdout/stderr 各自限额 256 KiB，超限截断并标记。
- **非零退出码不算错误**：`ExitCode` 作为业务结果返回；只有启动失败/超时/等待异常才是后端错误（映射为 `IsError=true` 的工具结果）。
- **已知边界**：进程后端不提供文件系统与网络隔离，只解决「资源失控、环境泄露、僵尸进程」三类问题；真正的容器级隔离需 Docker/gVisor 后端，接口已按 `sandbox.Backend` 预留，等有 Docker 的环境再补。
- **测试基线**：`internal/sandbox/process_test.go` 覆盖成功/非零退出/超时杀树/环境白名单/输出截断/工作目录清理/空 argv；`TestSandboxExec` 验证 MCP 端到端与超时路径。

## 6. 可观测设计

- Span 树：`agentcrate.call` → `authorize` / `route` / `sandbox.pull` / `sandbox.exec` / `ledger.record`
- Metrics（Prometheus 原生）：`calls_total{tool,tenant,outcome}`、`call_duration_seconds`（histogram）、`sandbox_pool_size`、`ledger_spend_cents{tenant,model}`
- 台账精度目标：与上游账单误差 < 1%（含重试、超时半途取消的归账规则，见 benchmark.md）
- 全部走 OTel SDK，不绑死 exporter。

## 7. 与现有方案的差异化

| 方案 | 缺口 | AgentCrate 补位 |
|---|---|---|
| one-api / LiteLLM | 只管 LLM API 代理，不管工具执行层与沙箱 | 工具调用级网关 + 执行隔离 |
| agentgateway（CNCF） | agent/MCP 路由、治理、可观测，但不隔离执行——工具仍在宿主进程里跑 | 沙箱运行时 + 调用级准入与记账 |
| agent-sandbox 系列 | 有隔离执行，缺网关/声明制授权/台账的一体化 | 单二进制串联：准入→沙箱→trace→台账 |
| MCP 官方参考实现 | 无多租户、无权限、无台账 | 声明制授权 + 成本核算 |
| Langfuse 等 tracing | 只有 LLM span，工具执行是黑盒 | 工具层 span + 沙箱内部事件 |

## 8. 已定决策

- 语言 **Go**：并发模型、单二进制部署、企业后端岗位匹配。
- 沙箱第一后端 Docker（最普及），第二后端独立进程（低配环境），接口留出 gVisor/firecracker 扩展位。
- 权限模型 v1 只做 scope 交集，故意不做复杂策略引擎——先让 deny/audit 立起来。
