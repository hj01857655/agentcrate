// 进程后端：每次执行建独立临时工作目录，宿主机环境仅透传白名单（密钥不落地），
// 超时强杀整棵进程树，stdout/stderr 各自限额截断。
// 已知边界（写进 design.md M3 基线）：进程后端不具备文件系统与网络隔离能力，
// 真正隔离需容器/gVisor 后端；它解决的是「资源失控 + 环境泄露 + 僵尸进程」。
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultTimeoutSeconds = 30
	// maxOutput 单路输出上限；超出即截断，避免一条命令撑爆网关内存。
	maxOutput = 256 * 1024
)

// ProcessBackend 进程级执行后端。
type ProcessBackend struct {
	baseDir string // 每次执行在其下建子目录，执行完删除
}

// NewProcessBackend baseDir 为空时落到系统临时目录。
func NewProcessBackend(baseDir string) *ProcessBackend {
	if strings.TrimSpace(baseDir) == "" {
		baseDir = os.TempDir()
	}
	return &ProcessBackend{baseDir: baseDir}
}

// Name 实现 Backend。
func (b *ProcessBackend) Name() string { return "process" }

// Exec 启动命令并等待完成；超时返回 Result（ExitCode=-1）与错误。
func (b *ProcessBackend) Exec(ctx context.Context, job Job) (Result, error) {
	if len(job.Argv) == 0 || strings.TrimSpace(job.Argv[0]) == "" {
		return Result{}, errors.New("sandbox: empty argv")
	}
	timeout := job.Timeout
	if timeout <= 0 {
		timeout = defaultTimeoutSeconds * time.Second
	}
	limit := job.MaxOutputBytes
	if limit <= 0 {
		limit = maxOutput
	}

	if err := os.MkdirAll(b.baseDir, 0o755); err != nil {
		return Result{}, fmt.Errorf("sandbox: base dir: %w", err)
	}
	workdir, err := os.MkdirTemp(b.baseDir, "job-*")
	if err != nil {
		return Result{}, fmt.Errorf("sandbox: workdir: %w", err)
	}
	defer os.RemoveAll(workdir)

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, job.Argv[0], job.Argv[1:]...)
	cmd.Dir = workdir
	cmd.Env = append(minEnv(), job.Env...)
	stdout := newLimitedBuffer(limit)
	stderr := newLimitedBuffer(limit)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// 超时/取消时连子孙进程一起杀；WaitDelay 兜底：孙进程占着管道时 Wait 也能按时返回。
	cmd.Cancel = func() error {
		killTree(cmd.Process)
		return nil
	}
	cmd.WaitDelay = 1500 * time.Millisecond

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("sandbox: start %q: %w", job.Argv[0], err)
	}
	waitErr := cmd.Wait()
	runErr := runCtx.Err()

	res := Result{
		Stdout:    stdout.String(),
		Stderr:    stderr.String(),
		Truncated: stdout.truncated || stderr.truncated,
		Duration:  time.Since(start),
	}
	if runErr != nil {
		res.ExitCode = -1
		return res, fmt.Errorf("sandbox: timeout after %s (process tree killed): %w", timeout, runErr)
	}
	if waitErr != nil {
		if ee, ok := waitErr.(*exec.ExitError); ok {
			res.ExitCode = ee.ExitCode()
			return res, nil
		}
		return res, fmt.Errorf("sandbox: wait: %w", waitErr)
	}
	return res, nil
}

// minEnv 只透传系统必需变量：PATH 类 + 临时目录 + 用户目录，其余一律不继承，
// 避免下游进程拿到 API key / token 等宿主机密钥。
func minEnv() []string {
	keep := map[string]bool{
		"PATH": true, "PATHEXT": true, "SystemRoot": true, "SYSTEMROOT": true,
		"WINDIR": true, "COMSPEC": true, "TEMP": true, "TMP": true,
		"HOME": true, "USERPROFILE": true, "NUMBER_OF_PROCESSORS": true,
	}
	out := make([]string, 0, len(keep))
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 && keep[kv[:i]] {
			out = append(out, kv)
		}
	}
	return out
}

// killTree Windows 上 taskkill /T 连子进程一起杀；其他平台直接 Kill。
func killTree(p *os.Process) {
	if p == nil {
		return
	}
	if runtime.GOOS == "windows" {
		_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(p.Pid)).Run()
		return
	}
	_ = p.Kill()
}

// limitedBuffer 有上限的写缓冲；超限后丢弃并标记 truncated。
type limitedBuffer struct {
	mu        sync.Mutex
	buf       strings.Builder
	limit     int
	truncated bool
}

func newLimitedBuffer(limit int) *limitedBuffer { return &limitedBuffer{limit: limit} }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.buf.Len()+len(p) > b.limit {
		room := b.limit - b.buf.Len()
		if room > 0 {
			b.buf.Write(p[:room])
		}
		b.truncated = true
		return len(p), nil // 丢弃但不报错，避免写侧错误中断进程
	}
	b.buf.Write(p)
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
