# Project Story



## Inspiration: One tool call can hand over the whole host

The moment that started AgentCrate was watching an agent enthusiastically call an MCP tool — and realizing that the tool, along with any code the agent generated, was running directly on my host machine, inheriting every environment variable I own. No permission boundary on each call. No isolation on each execution. No end-to-end trace of who called what and where it stalled. And at month end, the token consumption and the real invoice simply didn't match.

These four gaps aren't an orchestration-framework problem (LangChain & co. don't touch this layer), and they aren't an LLM-API-gateway problem either (LiteLLM only proxies model APIs). We surveyed the ecosystem: LLM API gateways exist (LiteLLM / one-api), agent routing and governance exists (agentgateway, CNCF), pure sandboxes exist (the agent-sandbox series, kubernetes-sigs) — but nobody had shipped a unified **execution layer** that does **admission, isolated execution, and accounting** together. That gap is AgentCrate: a security execution layer deployed between the agent application and its tools (MCP servers / HTTP APIs). It does no task planning and never touches prompts. It does exactly three things: **authorize, execute, account** for every tool call.

## How we built it: three milestones, each one demoable

We cut the roadmap by the rule "every step must be independently demoable" and wrote it in Go as a single binary:

- **M1 — MCP gateway + full-chain tracing.** A registry mounts real MCP servers (filesystem, fetch); the gateway proxies `tools/list` / `tools/call`; OpenTelemetry emits the full span tree `agentcrate.call → authorize → route → exec`. The acceptance test was deliberately blunt: mount AgentCrate into Claude Code as its MCP server and watch the entire span tree appear in Jaeger.
- **M2 — Declarative authorization + audit.** Every tool declares its scopes (`fs:read`, `net:egress`, …); the tenant policy intersects with them and the default is **deny**. Both allow and deny decisions land in a JSONL audit log, every deny carries a reason, and the reason is also returned to the agent itself as an `IsError=true` tool result — the agent can read *why* it was refused. In our model, a denial is a first-class record, not an exception.
- **M3 — Sandbox executor.** The built-in tool `sandbox_exec` runs every job in a fresh temporary working directory that is deleted afterwards; host environment variables pass through a whitelist (anything resembling a token or key is never inherited); timeouts kill the entire process tree; stdout and stderr are each capped at \(2^{18} = 262144\) bytes and flagged on truncation — and all of it flows through the same authorize → audit → trace pipeline as every other tool call.

We deliberately left seams in the architecture: the sandbox is abstracted behind a `sandbox.Backend` interface with pluggable backends, and the v1 permission model is a plain scope intersection — no policy engine, on purpose. Get deny/audit standing first; policy expressiveness can come later.

## What we learned

**The hard boundary is the product.** We initially assumed isolation means containers. Our dev machine runs Windows with no Docker, so that path was unverifiable — and an unverifiable security claim is worthless. Instead of stalling, we split the problem: a process backend that provably closes three failure classes (runaway resources, environment leakage, zombie processes), with the container backend behind the interface, waiting for an environment where we can actually test it. Writing down what the sandbox *does not* isolate — no filesystem or network isolation in the process backend — turned out to be as important as what it does.

**Denying is harder than allowing.** Most gateways reject a call and only a human ever finds out. We committed to deny being visible in three places at once: the audit log (queryable), the `authorize` span attributes `agentcrate.decision` / `agentcrate.reason` (observable), and the MCP tool result (readable by the agent). That path took more code than the allow path, but in any governance scenario it is the actual deliverable.

**If a metric matters, define it before you build.** Our `benchmark.md` fixes the numbers up front, all relative to direct connection — gateway overhead \(P_{99} < 5\,\text{ms}\), tracing overhead \(\frac{t_{\text{otel}} - t_{\text{base}}}{t_{\text{base}}} < 10\%\), sandbox cold start \(< 800\,\text{ms}\) vs. pooled warm start \(< 50\,\text{ms}\), and ledger accuracy \(< 1\%\) error against the upstream bill, with the accounting rule that a call cancelled mid-flight by a timeout must still land in the ledger, flagged `partial: true`. Having the formulas written down before writing the code settled a surprising number of design arguments (like what "cold start" even means) in advance.

## Challenges we faced

### 1. Building an isolation layer on a machine with no Docker

Windows, no containers. The process backend had to earn its keep the hard way: killing a whole process tree on Windows means `taskkill /T /F`, a detail almost no documentation mentions; and a stray grandchild process holding the stdout pipe will hang `cmd.Wait()` forever, which we only fixed with an explicit `WaitDelay` backstop. Every one of these behaviors is now pinned by tests (`internal/sandbox/process_test.go` covers success, non-zero exit, timeout tree-kill, env whitelist, output truncation, workdir cleanup, and empty argv), plus an end-to-end test through the gateway.

### 2. Making the deny path observable end to end

Threading one decision through three channels — JSONL audit, OTel span attributes, and a Prometheus counter bucket `agentcrate_calls_total{outcome="denied"}` — without letting any single failure break the call path (audit write failures log but don't block) took the most iteration. The payoff: you can tighten a tenant's scopes and *watch the agent's behavior change* in metrics, traces, and logs simultaneously.

### 3. Keeping trust boundaries crisp in the design

The rule we settled on and enforced everywhere: the gateway trusts the agent's *identity* but never the tool's *content* — so execution must be isolated and every output is untrusted data until it leaves the sandbox. Every design debate (should the router call this tool inline? should this output be logged verbatim?) resolved to that one sentence.

## What's next

Docker and gVisor backends behind the existing `sandbox.Backend` interface, the cost ledger (M4) with multi-tenant quotas, and call recording/replay for tool-chain regression evals (M5) — with the full benchmark suite published, data included.
