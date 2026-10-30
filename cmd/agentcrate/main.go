// AgentCrate 入口：单进程 = MCP 网关 + registry + 授权/审计 + 观测面。
// /mcp 与 /metrics 共用一个 HTTP mux（docs/design.md 第 2 节的 gateway/observe 模块）。
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/hj01857655/agentcrate/internal/audit"
	"github.com/hj01857655/agentcrate/internal/authz"
	"github.com/hj01857655/agentcrate/internal/gateway"
	"github.com/hj01857655/agentcrate/internal/observe"
	"github.com/hj01857655/agentcrate/internal/registry"
	"github.com/hj01857655/agentcrate/internal/sandbox"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	var (
		addr   = flag.String("addr", ":8080", "HTTP 监听地址，/mcp 与 /metrics 共用")
		otlp   = flag.String("otlp", "localhost:4318", "OTLP/HTTP trace 上报端点（Jaeger all-in-one 默认 4318；4317 是 OTLP/gRPC）")
		fsRoot = flag.String("fs-root", "workspace", "filesystem 下游暴露给 agent 的根目录")
		policy = flag.String("policy", "", "授权策略 yaml 路径；空=内置默认策略（default 租户仅 fs:read+net:egress）")
	auditPath = flag.String("audit", "audit.jsonl", "JSONL 审计日志路径，allow/deny 都记")
	sandboxDir = flag.String("sandbox-dir", "", "沙箱工作目录基底；空=系统临时目录，每次执行建子目录并执行完删除")
	)
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	obs, shutdown, err := observe.Init(ctx, *otlp)
	if err != nil {
		log.Fatalf("observe init: %v", err)
	}

	// 授权器与审计器：策略默认 deny，审计不可绕过。
	az, err := authz.LoadOrDefault(*policy)
	if err != nil {
		log.Fatalf("policy: %v", err)
	}
	au, err := audit.New(*auditPath)
	if err != nil {
		log.Fatalf("audit: %v", err)
	}
	defer au.Close()

	// 沙箱后端：M3 先上进程后端（独立临时目录 + 环境白名单 + 超时杀树 + 输出限额）。
	sb := sandbox.NewProcessBackend(*sandboxDir)

	// 下游清单：M1 固定 filesystem + fetch；M2 起改为配置文件/注册 API。
	root, err := absDefault(*fsRoot)
	if err != nil {
		log.Fatalf("fs-root: %v", err)
	}
	reg, err := registry.New(ctx, []registry.Spec{
		{ID: "filesystem", Exe: "cmd", Args: []string{"/c", "npx", "-y", "@modelcontextprotocol/server-filesystem", root}},
		{ID: "fetch", Exe: "cmd", Args: []string{"/c", "uvx", "mcp-server-fetch"}},
	})
	if err != nil {
		log.Fatalf("registry: %v", err)
	}
	defer reg.Close()

	gw := gateway.New(reg, obs, az, au, sb)

	mux := http.NewServeMux()
	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return gw.Server() }, nil))
	mux.Handle("/metrics", obs.Handler())

	httpSrv := &http.Server{Addr: *addr, Handler: mux}
	go func() {
		log.Printf("agentcrate: mcp=/mcp metrics=/metrics addr=%s otlp=%s fs-root=%s policy=%s audit=%s",
			*addr, *otlp, root, az.Summary(), *auditPath)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	if err := shutdown(shutdownCtx); err != nil {
		log.Printf("trace shutdown: %v", err)
	}
}

// absDefault 把相对的 fs-root 定成绝对路径，下游 server 的报错信息才可读。
func absDefault(p string) (string, error) {
	if p == "" {
		p = "workspace"
	}
	return filepath.Abs(p)
}
