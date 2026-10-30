// Package gateway 把 registry 聚合的工具表暴露成一个对外的 MCP server：
// tools/list 来自聚合目录，tools/call 先经声明制授权，再透传到下游并回填 traceId。
// 每次调用产出一棵 span 树：agentcrate.call → authorize / route / exec（docs/design.md 第 6 节）。
// 授权默认拒绝：工具未声明 scope、或租户 scope 无交集都会 deny；deny 也是可观测的一等公民
// （span 属性 + outcome=denied 指标 + JSONL 审计）。
package gateway

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/hj01857655/agentcrate/internal/audit"
	"github.com/hj01857655/agentcrate/internal/authz"
	"github.com/hj01857655/agentcrate/internal/observe"
	"github.com/hj01857655/agentcrate/internal/registry"
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
	srv *mcp.Server
}

// New 构建网关：授权器与审计器可为 nil（nil 审计则只跳过落盘，不跳过决策）。
func New(reg *registry.Registry, obs *observe.Observe, az *authz.Authorizer, au *audit.Logger) *Server {
	g := &Server{reg: reg, obs: obs, az: az, au: au}
	g.srv = mcp.NewServer(&mcp.Implementation{Name: "agentcrate", Version: "0.1.0"}, nil)
	for _, e := range reg.ToolTable() {
		e := e
		g.srv.AddTool(e.Tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return g.handleCall(ctx, req, e.Orig, e.Down)
		})
	}
	return g
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
		// MCP 工具级错误（IsError）而非协议错误：agent 能看懂被拒原因并自行调整。
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: "denied: " + dec.Reason}},
		}, nil
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
