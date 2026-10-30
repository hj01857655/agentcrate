# AgentCrate 路线图

> 里程碑按「每一步都能独立演示、独立写文章」切分。日期从 2026-10-30 起算。

## M1 · MCP 网关 + 全链路 trace（第 1–2 周）

- registry：挂载 2+ 个真实 MCP server（filesystem、fetch），工具目录生效
- gateway：`tools/list` / `tools/call` 透传 + 调用日志
- observe：OTel span 树（authorize→route→exec）+ Prometheus 指标
- 验收：用 Claude Code 把 AgentCrate 当 MCP server 挂上，Jaeger 里看到完整 span 树
- 产物：架构文章第一篇《给 Agent 工具调用做全链路追踪》

## M2 · 声明制授权 + 审计（第 3–4 周）

- scope 声明、租户策略、默认 deny
- 审计日志（JSONL）+ deny 原因
- 验收：未授权调用被拒且审计可查；scope 收紧后 agent 行为可观测地变化

## M3 · 沙箱执行器（第 5–7 周）

- Docker 后端：限额、egress 白名单、限时强杀、镜像缓存
- Process 后端：独立进程 + rlimit（低配环境 fallback）
- 验收：SideEffect 工具强制沙箱；沙箱内攻击面演示（写宿主文件失败）

## M4 · 成本台账 + 配额（第 8–9 周）

- 单价表（yaml 起步）、UsageEvent 记账、多租户配额与预警
- 台账精度对账：重试/超时取消的归账规则落地
- 验收：mock 多供应商流量下，台账 vs 人工对账误差 < 1%

## M5 · 评估回放 + Benchmark 发布（第 10+ 周）

- 调用录制/回放，回归评测钩子
- benchmark.md 定义的全套指标跑分并公开数据
- 产物：benchmark 文章 + v0.1.0 release

## 长期候选（不承诺）

- gVisor / firecracker 后端；WASM 工具
- MCP server 安全扫描（静态分析 scope 声明与实际行为差）
- 多节点部署（gateway 无状态化 + sandbox 池化调度）
