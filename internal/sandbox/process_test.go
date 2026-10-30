package sandbox

import (
	"context"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// 进程后端的行为契约：退出码、超时杀树、环境白名单、输出限额、工作目录清理。
// 命令用 Windows 自带 cmd，非 Windows 平台跳过（后端逻辑本身跨平台）。

func requireWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("进程后端测试以 cmd 构造命令，仅 Windows 运行")
	}
}

func TestExecSuccess(t *testing.T) {
	requireWindows(t)
	b := NewProcessBackend(t.TempDir())
	res, err := b.Exec(context.Background(), Job{
		Argv:    []string{"cmd", "/c", "echo hello-sandbox"},
		Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", res.ExitCode)
	}
	if !strings.Contains(res.Stdout, "hello-sandbox") {
		t.Errorf("stdout 缺内容: %q", res.Stdout)
	}
}

func TestExecNonZeroExitIsNotError(t *testing.T) {
	requireWindows(t)
	b := NewProcessBackend(t.TempDir())
	res, err := b.Exec(context.Background(), Job{
		Argv:    []string{"cmd", "/c", "exit 3"},
		Timeout: 10 * time.Second,
	})
	// 非零退出码是业务结果，不是后端错误——调用方靠 ExitCode 判断。
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3", res.ExitCode)
	}
}

func TestExecTimeoutKillsProcessTree(t *testing.T) {
	requireWindows(t)
	b := NewProcessBackend(t.TempDir())
	start := time.Now()
	// ping -n 6 约 5 秒；超时设 500ms，应在 1 秒内返回并报 timeout。
	res, err := b.Exec(context.Background(), Job{
		Argv:    []string{"cmd", "/c", "ping -n 6 127.0.0.1"},
		Timeout: 500 * time.Millisecond,
	})
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("期望 timeout 错误，得到 err=%v res=%+v", err, res)
	}
	if res.ExitCode != -1 {
		t.Errorf("timeout 时 ExitCode = %d, want -1", res.ExitCode)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("超时未被及时中断，耗时 %s", elapsed)
	}
}

func TestExecEnvWhitelist(t *testing.T) {
	requireWindows(t)
	t.Setenv("AGENTCRATE_SECRET_CANARY", "must-not-leak")
	b := NewProcessBackend(t.TempDir())
	res, err := b.Exec(context.Background(), Job{
		Argv:    []string{"cmd", "/c", "echo %AGENTCRATE_SECRET_CANARY%"},
		Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	// 变量未透传时 cmd 原样输出字面量 %NAME%。
	if strings.Contains(res.Stdout, "must-not-leak") {
		t.Errorf("宿主机环境变量泄漏进沙箱: %q", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "%AGENTCRATE_SECRET_CANARY%") {
		t.Errorf("预期未展开的字面量，得到: %q", res.Stdout)
	}
}

func TestExecOutputTruncated(t *testing.T) {
	requireWindows(t)
	b := NewProcessBackend(t.TempDir())
	payload := strings.Repeat("x", 200)
	res, err := b.Exec(context.Background(), Job{
		Argv:           []string{"cmd", "/c", "echo " + payload},
		Timeout:        10 * time.Second,
		MaxOutputBytes: 32,
	})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !res.Truncated {
		t.Errorf("期望 Truncated=true，stdout 长度 %d", len(res.Stdout))
	}
	if len(res.Stdout) > 32 {
		t.Errorf("stdout 超过限额：%d > 32", len(res.Stdout))
	}
}

func TestExecWorkdirIsolatedAndCleaned(t *testing.T) {
	requireWindows(t)
	base := t.TempDir()
	b := NewProcessBackend(base)
	if _, err := b.Exec(context.Background(), Job{
		Argv:    []string{"cmd", "/c", "echo x > marker.txt"},
		Timeout: 10 * time.Second,
	}); err != nil {
		t.Fatalf("exec: %v", err)
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatalf("read base: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("执行完未清理工作目录，残留 %d 项", len(entries))
	}
}

func TestExecEmptyArgv(t *testing.T) {
	b := NewProcessBackend(t.TempDir())
	if _, err := b.Exec(context.Background(), Job{}); err == nil {
		t.Fatal("空 argv 应报错")
	}
}
