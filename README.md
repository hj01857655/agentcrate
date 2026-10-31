# AgentCrate

> **Agent Infra：面向生产 AI Agent 的安全执行层** —— MCP 工具网关 · 沙箱运行时 · 全链路可观测 · 成本台账。

[![status](https://img.shields.io/badge/status-design-orange)](docs/roadmap.md) [![license](https://img.shields.io/badge/license-MIT-green)](LICENSE)

## 为什么做这个

Agent 从 demo 走向生产，缺的不是又一个编排框架，而是基建层。当前普遍的四个缺口：

1. **执行环境不隔离** —— agent 生成的代码与不可信工具直接跑在宿主机上，一次工具调用就能把宿主机交出去。
2. **工具调用没有权限边界** —— MCP server 一旦挂载，agent 每次调用没有细粒度授权，也没有留痕。
3. **没有全链路追踪** —— 一次 agent run 里谁在什么时候调了什么工具、卡在哪一步，LLM 平台的 trace 看不到工具执行层。
4. **没有成本台账** —— 多模型、多供应商、多租户混跑，token 消耗和真实账单对不上。

这个空档是扫描过的：LLM API 网关有 LiteLLM / one-api，agent 路由与治理有 agentgateway（CNCF），纯沙箱有 agent-sandbox 系列（kubernetes-sigs 等）——但把「准入、隔离执行、记账」做成一体化执行层，开源界还没有成熟答案。AgentCrate 以沙箱运行时为核心，网关只是它的输入面，站在 agent 应用和工具/MCP server 之间。

## 核心特性（规划中，见 [roadmap](docs/roadmap.md)）

| 模块 | 说明 |
|---|---|
| **沙箱运行时** | 隔离执行 agent 生成代码与不可信工具，资源限额 + 环境隔离，核心差异化模块。M3 已落地进程后端：独立临时工作目录、环境变量白名单、超时杀整棵进程树、输出限额截断；容器/gVisor 后端待 Docker 环境 |
| **MCP 接入网关** | 统一接入/注册多个 MCP server，作为执行层的输入面；每次调用按声明 scope 校验，全程审计（M2 起生效：声明制 scope 授权 + 默认拒绝 + JSONL 审计） |
| **全链路追踪** | OpenTelemetry spans：`authorize → route → sandbox.exec`，成功率 / P99 开箱即得 |
| **成本台账** | 按 provider / model 单价精确记账，多租户配额与预警 |
| **评估钩子** | 录制真实调用，回放做工具链回归评测 |

## 架构总览

```
                 ┌─────────────────────────────────────────────────┐
 Agent 应用       │                    AgentCrate                     │        MCP Servers /
 (Claude Code /  │  ┌──────────┐   ┌──────────┐   ┌─────────────┐  │        Tools
 Dify / 自研) ───▶│  │ Gateway  │──▶│ Registry │──▶│   Sandbox   │  │ ────▶  ┌────────────┐
                 │  │ 认证/限流 │   │ 工具路由  │   │  容器执行    │  │        │ filesystem │
                 │  └────┬─────┘   └──────────┘   └──────┬──────┘  │        │ browser    │
                 │       │        ┌──────────────┐       │         │        │ db / api…  │
                 │       └───────▶│   Observe    │◀──────┘         │        └────────────┘
                 │                │ OTel + Ledger│                 │
                 │                └──────────────┘                 │
                 └─────────────────────────────────────────────────┘
```

## 快速开始

设计阶段，尚未发布。里程碑与验收标准见 [docs/roadmap.md](docs/roadmap.md)，模块设计与接口草案见 [docs/design.md](docs/design.md)，性能指标定义见 [docs/benchmark.md](docs/benchmark.md)。

## 本地运行

1. **构建**：在仓库根执行 `go build ./cmd/agentcrate`。
2. **启动 trace 后端（Jaeger 原生 Windows 二进制，本机不装 Docker）**：

   ```
   E:\VSCodeSpace\Agent\.tools\jaeger-2.22.0-windows-amd64\jaeger.exe
   ```

   Jaeger UI 在 http://localhost:16686；OTLP/HTTP 收 4318、OTLP/gRPC 收 4317（网关默认走 4318）。
3. **启动网关**：

   ```
   .\agentcrate.exe
   ```

   默认参数为 `-addr :8080 -otlp localhost:4318 -fs-root workspace -policy "" -audit audit.jsonl -sandbox-dir ""`（fs-root 会被转成绝对路径后传给下游 filesystem server；`-policy` 为空时用内置策略——default 租户持有 `fs:read`、`net:egress`、`sandbox:exec`，写操作默认被拒；`-sandbox-dir` 为空时用系统临时目录）。
4. **接入 MCP 客户端**：入口是 Streamable HTTP `http://localhost:8080/mcp`，Claude Code 等客户端用 `type: http` 的 `mcpServers` 配置接入。
5. **看指标**：`curl http://localhost:8080/metrics`。实际导出指标名带命名空间前缀：`agentcrate_calls_total{tool,tenant,outcome}` 与 `agentcrate_call_duration_seconds{tool,tenant}`（对应 design 里的 `calls_total` / `call_duration_seconds`，加了 `agentcrate_` 前缀）；被授权策略拒绝的调用进 `outcome="denied"` 桶。首次调用前 `/metrics` 只有 HELP/TYPE 行，属正常。
6. **看审计**：`-audit` 指定的 JSONL 文件（默认 `audit.jsonl`）每条一行，allow 与 deny 都记，含 `ts/traceId/tenant/tool/downstream/decision/reason`；deny 记录里 reason 直接说明缺哪个 scope。
7. **反查 trace**：每次工具调用的结果 `_meta` 里带 `agentcrate.dev/traceid`，拿它去 Jaeger 按 Trace ID 查全链路（span 树 `agentcrate.call` → `authorize` / `route` / `exec`，沙箱工具为 `authorize` / `sandbox.exec`）。
8. **沙箱执行**：客户端可调用内置工具 `sandbox_exec`（参数 `argv` 命令数组、`timeout_seconds` 超时秒数），在隔离的临时工作目录中执行并返回 stdout/stderr/退出码；需要 `sandbox:exec` scope（内置策略已授予 default 租户），走同一套授权、审计与 trace（span 树为 `authorize` / `sandbox.exec`）。

里程碑与验收标准见 [docs/roadmap.md](docs/roadmap.md)。

## License

[MIT](LICENSE)
