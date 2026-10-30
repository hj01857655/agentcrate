// Package gateway 把 registry 聚合的工具表暴露成一个对外的 MCP server：
// tools/list 来自聚合目录，tools/call 先经声明制授权，再透传到下游并回填 traceId。
// 每次调用产出一棵 span 树：agentcrate.call → authorize / route / exec（docs/design.md 第 6 节）。
// 授权默认拒绝：工具未声明 scope、或租户 scope 无交集都会 deny；deny 也是可观测的一等公民
// （span 属性 + outcome=denied 指标 + JSONL 审计）。
package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/hj01857655/agentcrate/internal/audit"
	"github.com/hj01857655/agentcrate/internal/authz"
	"github.com/hj01857655/agentcrate/internal/observe"
	"github.com/hj01857655/agentcrate/internal/registry"
	"github.com/hj01857655/agentcrate/internal/sandbox"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// TraceIdKey 工具结果 _meta 里回填的键，客户端用它去 Jaeger 反查全链路。
const TraceIdKey = "agentcrate.dev/traceid"

// Server 装配好的网关 MCP server；由 HTTP handler 借 StreamableHTTPHandler 对外服务。
type Server struct {
	reg *registry.Registry
	obs *observe.Observe
	az  *authz.Authorizer
	au  *audit.Logger
	sb  sandbox.Backend
	srv *mcp.Server
}

// New 构建网关：授权器、审计器、沙箱后端均可为 nil（nil 审计只跳过落盘，不跳过决策；
// nil 沙箱则不注册 sandbox_exec 工具）。
func New(reg *registry.Registry, obs *observe.Observe, az *authz.Authorizer, au *audit.Logger, sb sandbox.Backend) *Server {
	g := &Server{reg: reg, obs: obs, az: az, au: au, sb: sb}
	g.srv = mcp.NewServer(&mcp.Implementation{Name: "agentcrate", Version: "0.1.0"}, nil)
	for _, e := range reg.ToolTable() {
		e := e
		g.srv.AddTool(e.Tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return g.handleCall(ctx, req, e.Orig, e.Down)
		})
	}
	g.addSandboxTool()
	return g
}

// sandboxArgs sandbox_exec 的入参。
type sandboxArgs struct {
	Argv           []string `json:"argv" jsonschema:"要执行的命令及参数，argv[0] 为可执行文件"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty" jsonschema:"超时秒数，默认 30；超时强杀整个进程树"`
}

// addSandboxTool 注册内置的 sandbox_exec：不经 registry，直接走沙箱后端执行。
// 这是 M3「执行层」对 agent 暴露的入口，span 树为 agentcrate.call → authorize / sandbox.exec。
func (g *Server) addSandboxTool() {
	if g.sb == nil {
		return
	}
	mcp.AddTool(g.srv, &mcp.Tool{
		Name: "sandbox_exec",
		Description: "在隔离的临时工作目录里执行命令，返回 stdout/stderr/退出码。" +
			"宿主机环境变量只透白名单，超时强杀整个进程树，输出超限截断。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args sandboxArgs) (*mcp.CallToolResult, string, error) {
		return g.handleSandbox(ctx, args)
	})
}

// Server 返回底层 MCP server（挂 HTTP handler 用）。
func (g *Server) Server() *mcp.Server { return g.srv }

func (g *Server) handleCall(ctx context.Context, req *mcp.CallToolRequest, orig string, d *registry.Downstream) (*mcp.CallToolResult, error) {
	const tenant = "default" // M2 尚无多租户身份来源，策略按 default 租户裁决
	name := req.Params.Name
	start := time.Now()

	ctx, root := g.obs.Tracer().Start(ctx, "agentcrate.call",
		trace.WithAttributes(
			attribute.String("agentcrate.tool", name),
			attribute.String("agentcrate.tenant", tenant)))
	defer root.End()
	traceID := root.SpanContext().TraceID().String()

	// authorize：声明制 scope 交集，默认 deny。deny 不做下游调用，但要留痕、计数、审计。
	dec := g.az.Authorize(tenant, name)
	_, as := g.obs.Tracer().Start(ctx, "authorize")
	as.SetAttributes(
		attribute.String("agentcrate.decision", decisionWord(dec.Allow)),
		attribute.String("agentcrate.reason", dec.Reason))
	as.End()

	if !dec.Allow {
		g.obs.CallsTotal.WithLabelValues(name, tenant, "denied").Inc()
		g.obs.CallDuration.WithLabelValues(name, tenant).Observe(time.Since(start).Seconds())
		g.record(audit.Entry{TraceID: traceID, Tenant: tenant, Tool: name,
			Downstream: d.Spec.ID, Decision: "deny", Reason: dec.Reason})
		return deniedResult(traceID, dec.Reason), nil
	}

	// route：M1 无沙箱后端，全部直调下游 stdio session。
	_, rs := g.obs.Tracer().Start(ctx, "route")
	rs.SetAttributes(attribute.String("agentcrate.route", "downstream:"+d.Spec.ID))
	rs.End()

	// exec：透传原始参数；空参 nil 会被协议层序列化省略，统一成空对象。
	ec, es := g.obs.Tracer().Start(ctx, "exec",
		trace.WithAttributes(attribute.String("agentcrate.downstream", d.Spec.ID)))
	// 空参 RawMessage 会被协议层序列化省略，统一成空对象透传。
	args := req.Params.Arguments
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	res, err := d.Session().CallTool(ec, &mcp.CallToolParams{Name: orig, Arguments: args})
	outcome := "ok"
	if err != nil {
		outcome = "error"
	} else if res.IsError {
		outcome = "tool_error"
	}
	es.SetAttributes(observe.OutcomeAttrs(outcome))
	if err != nil {
		es.RecordError(err)
		es.SetStatus(codes.Error, err.Error())
	}
	es.End()

	g.obs.CallsTotal.WithLabelValues(name, tenant, outcome).Inc()
	g.obs.CallDuration.WithLabelValues(name, tenant).Observe(time.Since(start).Seconds())
	g.record(audit.Entry{TraceID: traceID, Tenant: tenant, Tool: name,
		Downstream: d.Spec.ID, Decision: "allow", Reason: dec.Reason})

	if err != nil {
		root.RecordError(err)
		root.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	// traceId 回填 _meta：工具结果与 trace 可互查，这是验收的硬性动作。
	if res.Meta == nil {
		res.Meta = mcp.Meta{}
	}
	res.Meta[TraceIdKey] = traceID
	return res, nil
}

// handleSandbox 执行 sandbox_exec：授权、span、指标、审计与下游透传同构。
func (g *Server) handleSandbox(ctx context.Context, args sandboxArgs) (*mcp.CallToolResult, string, error) {
	const tenant = "default"
	const name = "sandbox_exec"
	start := time.Now()

	ctx, root := g.obs.Tracer().Start(ctx, "agentcrate.call",
		trace.WithAttributes(
			attribute.String("agentcrate.tool", name),
			attribute.String("agentcrate.tenant", tenant)))
	defer root.End()
	traceID := root.SpanContext().TraceID().String()

	dec := g.az.Authorize(tenant, name)
	_, as := g.obs.Tracer().Start(ctx, "authorize")
	as.SetAttributes(
		attribute.String("agentcrate.decision", decisionWord(dec.Allow)),
		attribute.String("agentcrate.reason", dec.Reason))
	as.End()

	if !dec.Allow {
		g.obs.CallsTotal.WithLabelValues(name, tenant, "denied").Inc()
		g.obs.CallDuration.WithLabelValues(name, tenant).Observe(time.Since(start).Seconds())
		g.record(audit.Entry{TraceID: traceID, Tenant: tenant, Tool: name,
			Downstream: "sandbox", Decision: "deny", Reason: dec.Reason})
		return deniedResult(traceID, dec.Reason), "", nil
	}

	ec, es := g.obs.Tracer().Start(ctx, "sandbox.exec",
		trace.WithAttributes(attribute.String("agentcrate.sandbox.backend", g.sb.Name())))
	res, execErr := g.sb.Exec(ec, sandbox.Job{
		Argv:    args.Argv,
		Timeout: time.Duration(args.TimeoutSeconds) * time.Second,
	})
	outcome := "ok"
	if execErr != nil {
		outcome = "error"
		es.RecordError(execErr)
		es.SetStatus(codes.Error, execErr.Error())
	}
	es.SetAttributes(
		attribute.Int("agentcrate.exit_code", res.ExitCode),
		attribute.Bool("agentcrate.truncated", res.Truncated),
		attribute.Float64("agentcrate.duration_seconds", res.Duration.Seconds()))
	es.End()

	g.obs.CallsTotal.WithLabelValues(name, tenant, outcome).Inc()
	g.obs.CallDuration.WithLabelValues(name, tenant).Observe(time.Since(start).Seconds())
	g.record(audit.Entry{TraceID: traceID, Tenant: tenant, Tool: name,
		Downstream: "sandbox", Decision: "allow", Reason: dec.Reason})

	return &mcp.CallToolResult{
		IsError: execErr != nil,
		Content: []mcp.Content{&mcp.TextContent{Text: formatSandboxResult(res, execErr)}},
		Meta:    mcp.Meta{TraceIdKey: traceID},
	}, "", nil
}

// formatSandboxResult 把执行结果拼成 agent 可读的文本；非零退出码不算错误，超时才算。
func formatSandboxResult(res sandbox.Result, execErr error) string {
	var b strings.Builder
	fmt.Fprintf(&b, "exit=%d duration=%s", res.ExitCode, res.Duration.Round(time.Millisecond))
	if res.Truncated {
		b.WriteString(" (output truncated)")
	}
	b.WriteString("\n--- stdout ---\n")
	b.WriteString(res.Stdout)
	b.WriteString("\n--- stderr ---\n")
	b.WriteString(res.Stderr)
	if execErr != nil {
		b.WriteString("\n--- error ---\n")
		b.WriteString(execErr.Error())
	}
	return b.String()
}

// deniedResult 统一的拒绝结果：MCP 工具级错误（IsError）而非协议错误，
// agent 能读到原因自行调整，同时 traceId 仍可反查。
func deniedResult(traceID, reason string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: "denied: " + reason}},
		Meta:    mcp.Meta{TraceIdKey: traceID},
	}
}

// decisionWord 把布尔决策变成审计/span 用的字面值。
func decisionWord(allow bool) string {
	if allow {
		return "allow"
	}
	return "deny"
}

// record 落一条审计；审计器缺失时只跳过落盘（决策本身不依赖它）。
func (g *Server) record(e audit.Entry) {
	if g.au == nil {
		return
	}
	if err := g.au.Record(e); err != nil {
		log.Printf("audit write: %v", err)
	}
}
