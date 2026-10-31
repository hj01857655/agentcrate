---
inclusion: always
---

# Repository structure

```
cmd/agentcrate/main.go    # entrypoint: single process runs all modules
internal/
  gateway/                # MCP transport (Streamable HTTP /mcp), wiring, e2e_test.go
  registry/               # MCP server registration, tool catalog, scope declarations
  authz/                  # call-level authorization: scope intersection, default deny
  audit/                  # JSONL audit log (allow AND deny, with reason)
  sandbox/                # sandbox.Backend interface; process.go = process backend (M3)
  observe/                # OTel spans + Prometheus metrics
docs/
  design.md               # architecture, security model, milestone baselines — AUTHORITY
  roadmap.md              # milestones M1–M5, acceptance criteria
  benchmark.md            # metric definitions and target values
  project-story.md        # Devpost submission story
```

Conventions:

- Milestone work lands in the existing module that owns it (M4 ledger → `internal/ledger/`,
  wired in `cmd/agentcrate/main.go`, spans under `agentcrate.call`).
- Tests live next to the code (`*_test.go`); gateway-level behavior gets an e2e test in
  `internal/gateway/e2e_test.go`.
- Docs are part of the deliverable: a milestone without its design/README update is not done.
