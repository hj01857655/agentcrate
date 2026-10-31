---
inclusion: always
---

# Product: AgentCrate

AgentCrate is a **security execution layer** deployed between AI agent applications
(Claude Code, Dify, custom runtimes) and their tools (MCP servers, containerized tools,
HTTP APIs). It is not an orchestration framework: it does no task planning and never
touches prompts. For every tool call it does exactly three things:

1. **Admission** — declarative scope authorization (`fs:read`, `net:egress`, `sandbox:exec`, …).
   Tools declare their scopes, tenant policy intersects with them, **default is deny**.
   Every allow AND deny lands in a JSONL audit log with a reason; denials also reach the
   agent as `IsError=true` tool results.
2. **Execution** — untrusted work runs in a sandbox (`sandbox.Backend` interface).
   The process backend (M3, shipped) isolates workdir, env vars (whitelist), kills the
   whole process tree on timeout, caps stdout/stderr. It does NOT provide filesystem or
   network isolation — that is reserved for the planned Docker/gVisor backends. Never
   imply more isolation than the active backend provides.
3. **Accounting** — per-call metrics (`agentcrate_calls_total{tool,tenant,outcome}`,
   `agentcrate_call_duration_seconds`) now; a cost ledger with pricing tables and
   multi-tenant quotas is the M4 milestone.

Design authority: `docs/design.md` (single source of truth; code conflicts → change
design or file an issue first). Roadmap: `docs/roadmap.md`. Benchmark targets (goal
values, not measurements): `docs/benchmark.md`.
