// Package observe 汇聚 AgentCrate 的两条观测面：
// trace —— OTel SDK，span 经 OTLP/HTTP 上报（Jaeger all-in-one 默认 4318；4317 是 OTLP/gRPC）；
// metrics —— Prometheus registry，/metrics 端点直接暴露。
// 不绑死 exporter：换后端只动 Init 的参数，不改埋点。
package observe

import (
	"context"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// Observe 持有网关进程内共享的 tracer 与指标族。
type Observe struct {
	tracer trace.Tracer

	registry     *prometheus.Registry
	CallsTotal   *prometheus.CounterVec   // agentcrate_calls_total{tool,tenant,outcome}
	CallDuration *prometheus.HistogramVec // agentcrate_call_duration_seconds{tool,tenant}
}

// Init 建 tracer provider 与指标 registry。otlpEndpoint 形如 "localhost:4318"；
// 上报是异步批量的，端点不可达不影响业务调用，只影响 trace 落地。
// 返回的 shutdown 必须在进程退出前调用，把缓冲中的 span 冲洗出去。
func Init(ctx context.Context, otlpEndpoint string) (*Observe, func(context.Context) error, error) {
	exporter, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint(otlpEndpoint),
		otlptracehttp.WithInsecure(),
	)
	if err != nil {
		return nil, nil, err
	}
	// 不声明 schema URL：Default 自带新版本 schema（1.43+），Merge 遇到两个不同
	// schema URL 会直接报错，属性面（service.name/version）不受影响。
	res, err := resource.Merge(resource.Default(),
		resource.NewSchemaless(
			semconv.ServiceName("agentcrate"),
			semconv.ServiceVersion("0.1.0"),
		))
	if err != nil {
		return nil, nil, err
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)

	reg := prometheus.NewRegistry()
	o := &Observe{
		tracer:   tp.Tracer("github.com/hj01857655/agentcrate"),
		registry: reg,
		CallsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "agentcrate",
				Name:      "calls_total",
				Help:      "工具调用计数，按 tool/tenant/outcome（ok|tool_error|error）分桶",
			},
			[]string{"tool", "tenant", "outcome"},
		),
		CallDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: "agentcrate",
				Name:      "call_duration_seconds",
				Help:      "工具调用端到端耗时（含下游执行），P50/P99 由此直方图推导",
				Buckets:   []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
			},
			[]string{"tool", "tenant"},
		),
	}
	reg.MustRegister(o.CallsTotal, o.CallDuration)
	return o, tp.Shutdown, nil
}

// Tracer 供各模块建 span；span 树命名约束见 docs/design.md 第 6 节。
func (o *Observe) Tracer() trace.Tracer { return o.tracer }

// Handler 返回 Prometheus 抓取端点。
func (o *Observe) Handler() http.Handler {
	return promhttp.HandlerFor(o.registry, promhttp.HandlerOpts{EnableOpenMetrics: true})
}

// Outcome 三值：ok 成功；tool_error 下游以 IsError 返回（业务失败，链路本身通）；error 传输/协议层失败。
func OutcomeAttrs(outcome string) attribute.KeyValue {
	return attribute.String("agentcrate.outcome", outcome)
}
