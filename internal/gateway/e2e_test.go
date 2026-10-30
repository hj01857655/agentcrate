// e2e 集成测试：真实拉起 npx/uvx 下游，走 Streamable HTTP 全链路
// （ListTools → CallTool → traceId 回填 → /metrics 暴露）。
// 依赖本机有 npx 与 uvx；首次拉包需 1-2 分钟，缺任一命令则跳过。
package gateway_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hj01857655/agentcrate/internal/gateway"
	"github.com/hj01857655/agentcrate/internal/observe"
	"github.com/hj01857655/agentcrate/internal/registry"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func TestEndToEnd(t *testing.T) {
	for _, cmd := range []string{"npx", "uvx"} {
		if _, err := exec.LookPath(cmd); err != nil {
			t.Skipf("%s 不在 PATH，跳过端到端测试", cmd)
		}
	}

	ctx := context.Background()
	obs, shutdown, err := observe.Init(ctx, envOr("AGENTCRATE_OTLP", "localhost:4318"))
	if err != nil {
		t.Fatalf("observe init: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hello agentcrate"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	reg, err := registry.New(ctx, []registry.Spec{
		{ID: "filesystem", Exe: "cmd", Args: []string{"/c", "npx", "-y", "@modelcontextprotocol/server-filesystem", root}},
		{ID: "fetch", Exe: "cmd", Args: []string{"/c", "uvx", "mcp-server-fetch"}},
	})
	if err != nil {
		t.Fatalf("registry init: %v", err)
	}
	defer reg.Close()

	gw := gateway.New(reg, obs)
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return gw.Server() }, nil))
	mux.Handle("/metrics", obs.Handler())
	hs := httptest.NewServer(mux)
	defer hs.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "agentcrate-e2e", Version: "0.0.1"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: hs.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer cs.Close()

	// tools/list：聚合目录必须同时含 filesystem 与 fetch 的代表工具。
	lr, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	names := map[string]bool{}
	for _, tl := range lr.Tools {
		names[tl.Name] = true
	}
	for _, want := range []string{"read_file", "fetch"} {
		if !names[want] {
			t.Errorf("tools/list 缺少 %q，实际：%v", want, keysOf(names))
		}
	}

	// tools/call：读文件 → 结果内容、traceId 回填都要在。
	ct, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "read_file",
		Arguments: map[string]any{"path": filepath.Join(root, "hello.txt")},
	})
	if err != nil {
		t.Fatalf("call read_file: %v", err)
	}
	if ct.IsError {
		t.Errorf("read_file 返回 IsError=true: %+v", ct.Content)
	}
	blob, err := json.Marshal(ct.Content)
	if err != nil {
		t.Fatalf("marshal content: %v", err)
	}
	if !strings.Contains(string(blob), "hello agentcrate") {
		t.Errorf("read_file 结果不含文件内容，得到：%s", blob)
	}
	if traceID, _ := ct.Meta[gateway.TraceIdKey].(string); traceID == "" {
		t.Errorf("_meta 缺少 %s（或非字符串）：%+v", gateway.TraceIdKey, ct.Meta)
	}

	// /metrics：调用计过数，指标名与 tool 维度标签都要暴露。
	resp, err := http.Get(hs.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	if _, err := io.Copy(&sb, resp.Body); err != nil {
		t.Fatalf("read /metrics: %v", err)
	}
	body := sb.String()
	for _, want := range []string{
		"agentcrate_calls_total",
		"agentcrate_call_duration_seconds",
		`tool="read_file"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics 缺少 %q", want)
		}
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
