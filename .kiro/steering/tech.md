---
inclusion: always
---

# Tech stack & commands

- **Go 1.26** (module `github.com/hj01857655/agentcrate`), single binary, no codegen step.
- Key direct deps: `github.com/modelcontextprotocol/go-sdk` (MCP protocol both sides),
  `go.opentelemetry.io/otel` + OTLP/HTTP exporter (tracing),
  `github.com/prometheus/client_golang` (metrics), `gopkg.in/yaml.v3` (tenant policy files).
- Build: `go build ./cmd/agentcrate`
- Test everything: `go test ./...`
- Vet: `go vet ./...`
- Run (defaults): `.\agentcrate.exe` → `:8080`, OTLP → `localhost:4318`,
  fs-root `workspace`, builtin default policy, audit → `audit.jsonl`.
- Trace backend: Jaeger v2.22.0 native binary (no Docker on this machine),
  UI at http://localhost:16686, OTLP/HTTP on 4318.

## Invariants

- Deny is a first-class record: any new rejection path must write an audit entry,
  set the `outcome="denied"` metric bucket, and return the reason to the agent.
- Sandbox output is untrusted data; never log it verbatim into trusted stores.
- Benchmarks in `docs/benchmark.md` are targets — label them as targets in docs/README.
- Keep the single-binary story: new modules go under `internal/`, wired in `cmd/agentcrate/main.go`.
