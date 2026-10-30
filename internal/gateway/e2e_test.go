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
	"runtime"
	"strings"
	"testing"

	"github.com/hj01857655/agentcrate/internal/audit"
	"github.com/hj01857655/agentcrate/internal/authz"
	"github.com/hj01857655/agentcrate/internal/gateway"
	"github.com/hj01857655/agentcrate/internal/observe"
	"github.com/hj01857655/agentcrate/internal/registry"
	"github.com/hj01857655/agentcrate/internal/sandbox"
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

	gw := gateway.New(reg, obs, authz.New(authz.DefaultPolicy()), mustAudit(t), sandbox.NewProcessBackend(t.TempDir()))
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

// TestAuthzDenyAndAudit 验证声明制授权的拒绝路径与审计落盘：
// default 租户只有 fs:read，fetch（net:egress）必须被拒且留痕，read_file 正常放行。
func TestAuthzDenyAndAudit(t *testing.T) {
	for _, cmd := range []string{"npx", "uvx"} {
		if _, err := exec.LookPath(cmd); err != nil {
			t.Skipf("%s 不在 PATH，跳过授权测试", cmd)
		}
	}

	ctx := context.Background()
	obs, shutdown, err := observe.Init(ctx, envOr("AGENTCRATE_OTLP", "localhost:4318"))
	if err != nil {
		t.Fatalf("observe init: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hello agentcrate"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	// 策略从磁盘加载：同时验证 yaml 解析路径。
	policyPath := filepath.Join(dir, "policy.yaml")
	policyYAML := `tenants:
  default:
    scopes: ["fs:read"]
tool_scopes:
  read_file: ["fs:read"]
  write_file: ["fs:write"]
  fetch: ["net:egress"]
`
	if err := os.WriteFile(policyPath, []byte(policyYAML), 0o644); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	az, err := authz.Load(policyPath)
	if err != nil {
		t.Fatalf("load policy: %v", err)
	}
	auditPath := filepath.Join(dir, "audit.jsonl")
	au, err := audit.New(auditPath)
	if err != nil {
		t.Fatalf("audit init: %v", err)
	}
	defer au.Close()

	reg, err := registry.New(ctx, []registry.Spec{
		{ID: "filesystem", Exe: "cmd", Args: []string{"/c", "npx", "-y", "@modelcontextprotocol/server-filesystem", root}},
		{ID: "fetch", Exe: "cmd", Args: []string{"/c", "uvx", "mcp-server-fetch"}},
	})
	if err != nil {
		t.Fatalf("registry init: %v", err)
	}
	defer reg.Close()

	gw := gateway.New(reg, obs, az, au, sandbox.NewProcessBackend(t.TempDir()))
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return gw.Server() }, nil))
	mux.Handle("/metrics", obs.Handler())
	hs := httptest.NewServer(mux)
	defer hs.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "agentcrate-authz", Version: "0.0.1"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: hs.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer cs.Close()

	// 越权调用：fetch 需要 net:egress，default 租户没有 → 必须被拒且原因可读。
	denied, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "fetch",
		Arguments: map[string]any{"url": "https://example.com"},
	})
	if err != nil {
		t.Fatalf("call fetch: %v", err)
	}
	if !denied.IsError {
		t.Fatalf("fetch 应被拒，实际 IsError=false: %+v", denied.Content)
	}
	deniedText, _ := json.Marshal(denied.Content)
	if !strings.Contains(string(deniedText), "denied") || !strings.Contains(string(deniedText), "net:egress") {
		t.Errorf("拒绝原因不可读，得到：%s", deniedText)
	}

	// 授权调用：read_file 持有 fs:read → 透传成功。
	ok, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "read_file",
		Arguments: map[string]any{"path": filepath.Join(root, "hello.txt")},
	})
	if err != nil {
		t.Fatalf("call read_file: %v", err)
	}
	if ok.IsError {
		t.Fatalf("read_file 应放行，实际 IsError=true: %+v", ok.Content)
	}

	// 审计：deny 与 allow 都落盘，deny 带 reason，记录带 traceId。
	entries := readAudit(t, auditPath)
	var sawDeny, sawAllow bool
	for _, e := range entries {
		if e.Tool == "fetch" && e.Decision == "deny" && e.Reason != "" {
			sawDeny = true
		}
		if e.Tool == "read_file" && e.Decision == "allow" && e.TraceID != "" {
			sawAllow = true
		}
	}
	if !sawDeny {
		t.Errorf("审计缺少 fetch 的 deny 记录: %+v", entries)
	}
	if !sawAllow {
		t.Errorf("审计缺少 read_file 的 allow 记录: %+v", entries)
	}

	// 指标：deny 计入 outcome="denied" 桶。
	resp, err := http.Get(hs.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	if _, err := io.Copy(&sb, resp.Body); err != nil {
		t.Fatalf("read /metrics: %v", err)
	}
	if !strings.Contains(sb.String(), `outcome="denied"`) {
		t.Errorf("/metrics 缺少 outcome=\"denied\" 样本")
	}
}

// mustAudit 为「只想跑通链路」的用例建一个一次性审计器。
func mustAudit(t *testing.T) *audit.Logger {
	t.Helper()
	au, err := audit.New(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatalf("audit init: %v", err)
	}
	t.Cleanup(func() { _ = au.Close() })
	return au
}

// readAudit 解析 JSONL 审计文件。
func readAudit(t *testing.T, path string) []audit.Entry {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	var out []audit.Entry
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e audit.Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("bad audit line %q: %v", line, err)
		}
		out = append(out, e)
	}
	return out
}

// TestSandboxExec 验证内置 sandbox_exec 工具的端到端行为：正常执行、超时中断、
// 输出里带 traceId；不依赖 npx/uvx 下游，因此总是运行。
func TestSandboxExec(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("以 cmd 构造命令，仅 Windows 运行")
	}
	ctx := context.Background()
	obs, shutdown, err := observe.Init(ctx, envOr("AGENTCRATE_OTLP", "localhost:4318"))
	if err != nil {
		t.Fatalf("observe init: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	// 沙箱执行不需要下游 MCP server，空 registry 即可。
	reg, err := registry.New(ctx, nil)
	if err != nil {
		t.Fatalf("registry init: %v", err)
	}
	defer reg.Close()

	gw := gateway.New(reg, obs, authz.New(authz.DefaultPolicy()), mustAudit(t),
		sandbox.NewProcessBackend(t.TempDir()))
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return gw.Server() }, nil))
	mux.Handle("/metrics", obs.Handler())
	hs := httptest.NewServer(mux)
	defer hs.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "agentcrate-sandbox", Version: "0.0.1"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: hs.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer cs.Close()

	// tools/list 应包含内置 sandbox_exec。
	lr, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	seen := false
	for _, tl := range lr.Tools {
		if tl.Name == "sandbox_exec" {
			seen = true
		}
	}
	if !seen {
		t.Fatal("tools/list 缺 sandbox_exec")
	}

	ok, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "sandbox_exec",
		Arguments: map[string]any{"argv": []string{"cmd", "/c", "echo sandbox-ok"}, "timeout_seconds": 10},
	})
	if err != nil {
		t.Fatalf("call sandbox_exec: %v", err)
	}
	if ok.IsError {
		t.Fatalf("sandbox_exec 应成功，得到 IsError: %+v", ok.Content)
	}
	blob, _ := json.Marshal(ok.Content)
	if !strings.Contains(string(blob), "sandbox-ok") || !strings.Contains(string(blob), "exit=0") {
		t.Errorf("输出不含预期内容: %s", blob)
	}
	if traceID, _ := ok.Meta[gateway.TraceIdKey].(string); traceID == "" {
		t.Errorf("sandbox_exec 结果缺 traceId")
	}

	// 超时路径：1 秒超时 + 5 秒命令 → IsError 且提示 timeout。
	tmo, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "sandbox_exec",
		Arguments: map[string]any{"argv": []string{"cmd", "/c", "ping -n 6 127.0.0.1"}, "timeout_seconds": 1},
	})
	if err != nil {
		t.Fatalf("call sandbox_exec (timeout case): %v", err)
	}
	if !tmo.IsError {
		t.Errorf("超时用例应 IsError=true")
	}
	tmoBlob, _ := json.Marshal(tmo.Content)
	if !strings.Contains(string(tmoBlob), "timeout") {
		t.Errorf("超时提示缺失: %s", tmoBlob)
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
