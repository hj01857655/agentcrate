// Package gateway 把 registry 聚合的工具表暴露成一个对外的 MCP server：
// tools/list 来自聚合目录，tools/call 透传到对应下游并回填 traceId。
// 每次调用产出一棵 span 树：agentcrate.call → authorize / route / exec（docs/design.md 第 6 节）。
// M1 的 authorize 是恒 allow 的占位——决策已成 span 成指标，M2 换声明制实现时只动这一个函数。
package gateway

import (
	"context"
	"encoding/json"
	"time"

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
	reg  *registry.Registry
	obs  *observe.Observe
	srv  *mcp.Server
}

// New 构建网关：把每个下游工具注册进对外 server，handler 闭包持有路由目标。
func New(reg *registry.Registry, obs *observe.Observe) *Server {
	g := &Server{reg: reg, obs: obs}
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
	const tenant = "default" // M1 无多租户身份；指标与 span 先留维度
	name := req.Params.Name
	start := time.Now()

	ctx, root := g.obs.Tracer().Start(ctx, "agentcrate.call",
		trace.WithAttributes(
			attribute.String("agentcrate.tool", name),
			attribute.String("agentcrate.tenant", tenant)))
	defer root.End()

	// authorize：M1 恒 allow，但决策必须留痕（span + 之后的 metrics 扩维）。
	_, as := g.obs.Tracer().Start(ctx, "authorize")
	as.SetAttributes(attribute.String("agentcrate.decision", "allow"))
	as.End()

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

	if err != nil {
		root.RecordError(err)
		root.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	// traceId 回填 _meta：工具结果与 trace 可互查，这是验收的硬性动作。
	if res.Meta == nil {
		res.Meta = mcp.Meta{}
	}
	res.Meta[TraceIdKey] = root.SpanContext().TraceID().String()
	return res, nil
}
