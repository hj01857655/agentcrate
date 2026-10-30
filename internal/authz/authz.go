// Package authz 实现声明制授权：工具在策略里声明自己需要的 scope，租户声明自己持有的
// scope，两者交集非空才放行；未声明的工具、无交集的租户一律拒绝（默认 deny）。
// 这是 docs/design.md 第 5 节「scope 声明制」的 M2 落地实现，策略来源是 yaml 文件。
package authz

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Tenant 租户及其被授予的 scope 集合。
type Tenant struct {
	Scopes []string `yaml:"scopes"`
}

// Policy 声<b>明制</b>授权的全部输入：谁（租户）能用什么（工具声明的 scope）。
type Policy struct {
	Tenants    map[string]Tenant   `yaml:"tenants"`
	ToolScopes map[string][]string `yaml:"tool_scopes"`
}

// Decision 一次授权决策；deny 必须带 reason，直接进审计日志。
type Decision struct {
	Allow  bool
	Reason string
}

// Authorizer 只依赖不可变的策略快照，并发安全。
type Authorizer struct {
	policy Policy
}

// New 用内存策略构造授权器（默认策略与测试用）。
func New(p Policy) *Authorizer { return &Authorizer{policy: p} }

// Load 从 yaml 文件加载策略。
func Load(path string) (*Authorizer, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("authz: read policy: %w", err)
	}
	var p Policy
	if err := yaml.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("authz: parse policy %s: %w", path, err)
	}
	return New(p), nil
}

// LoadOrDefault 空路径回落内置默认策略，否则加载指定文件。
func LoadOrDefault(path string) (*Authorizer, error) {
	if strings.TrimSpace(path) == "" {
		return New(DefaultPolicy()), nil
	}
	return Load(path)
}

// DefaultPolicy 内置策略：读类工具给 fs:read，写类给 fs:write，fetch 给 net:egress，
// 沙箱执行器给 sandbox:exec；default 租户持有读/出网/沙箱执行——写操作默认被拒，
// 正好演示「默认 deny + 审计可查」。
func DefaultPolicy() Policy {
	readTools := []string{
		"read_file", "read_multiple_files", "list_directory", "directory_tree",
		"search_files", "get_file_info", "list_allowed_directories",
	}
	writeTools := []string{
		"write_file", "edit_file", "create_directory", "move_file",
	}
	tools := make(map[string][]string, len(readTools)+len(writeTools)+1)
	for _, t := range readTools {
		tools[t] = []string{"fs:read"}
	}
	for _, t := range writeTools {
		tools[t] = []string{"fs:write"}
	}
	tools["fetch"] = []string{"net:egress"}
	tools["sandbox_exec"] = []string{"sandbox:exec"}
	return Policy{
		Tenants: map[string]Tenant{
			"default": {Scopes: []string{"fs:read", "net:egress", "sandbox:exec"}},
		},
		ToolScopes: tools,
	}
}

// Authorize 默认拒绝，三条规则按顺序短路：
// 未知租户 → deny；工具没有 scope 声明 → deny；scope 无交集 → deny（列出缺的 scope）。
func (a *Authorizer) Authorize(tenant, tool string) Decision {
	t, ok := a.policy.Tenants[tenant]
	if !ok {
		return Decision{Allow: false, Reason: "unknown tenant: " + tenant}
	}
	need := a.policy.ToolScopes[tool]
	if len(need) == 0 {
		return Decision{Allow: false, Reason: "tool declares no scope: " + tool}
	}
	have := make(map[string]bool, len(t.Scopes))
	for _, s := range t.Scopes {
		have[s] = true
	}
	for _, s := range need {
		if have[s] {
			return Decision{Allow: true, Reason: "scope granted: " + s}
		}
	}
	sorted := append([]string(nil), need...)
	sort.Strings(sorted)
	return Decision{Allow: false, Reason: "missing scope: " + strings.Join(sorted, ",")}
}

// Summary 给启动日志用的一行策略摘要。
func (a *Authorizer) Summary() string {
	return fmt.Sprintf("tenants=%d tool_scopes=%d", len(a.policy.Tenants), len(a.policy.ToolScopes))
}
