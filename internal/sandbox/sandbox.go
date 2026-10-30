// Package sandbox 提供工具调用的隔离执行后端。
// 接口对应 docs/design.md 第 4 节的 Sandbox：一次 job = 一条命令 + 限额，结果带
// stdout/stderr/退出码。M3 先落地进程后端（无 Docker 环境可验证）；容器后端待后续。
package sandbox

import (
	"context"
	"time"
)

// Job 一次沙箱执行请求。Argv[0] 是可执行文件，其余是参数。
type Job struct {
	Argv           []string
	Env            []string // 追加环境变量；宿主机环境只透传白名单
	Timeout        time.Duration
	MaxOutputBytes int
}

// Result 执行结果。Truncated 表示输出触达上限被截断。
type Result struct {
	Stdout    string
	Stderr    string
	ExitCode  int
	Truncated bool
	Duration  time.Duration
}

// Backend 执行后端；M3 只有 process，后续补 docker/gvisor 实现。
type Backend interface {
	Name() string
	Exec(ctx context.Context, job Job) (Result, error)
}
