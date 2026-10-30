// Package audit 落 JSONL 审计：每一次授权决策（allow 与 deny）都是一条记录，
// 不可绕过——这是 docs/design.md 第 5 节给「Agent 治理」场景的第一交付物。
package audit

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// Entry 一条审计记录；decision 取 allow/deny，deny 必带 reason。
type Entry struct {
	Time       string `json:"ts"`
	TraceID    string `json:"traceId"`
	Tenant     string `json:"tenant"`
	Tool       string `json:"tool"`
	Downstream string `json:"downstream,omitempty"`
	Decision   string `json:"decision"`
	Reason     string `json:"reason,omitempty"`
}

// Logger 追加写 JSONL；进程内串行化，多 goroutine 安全。
type Logger struct {
	mu sync.Mutex
	f  *os.File
}

// New 打开（必要时创建）审计文件，追加模式。
func New(path string) (*Logger, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &Logger{f: f}, nil
}

// Record 写一条记录；Time 为空时补当前 UTC 时间。
func (l *Logger) Record(e Entry) error {
	if e.Time == "" {
		e.Time = time.Now().UTC().Format(time.RFC3339Nano)
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	_, err = l.f.Write(b)
	return err
}

// Close 关闭审计文件。
func (l *Logger) Close() error { return l.f.Close() }
