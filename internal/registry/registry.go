// Package registry 维护下游 MCP server 的生命周期与工具目录：
// 按 Spec 以 stdio 子进程拉起（npx/uvx），聚合其工具列表为网关侧路由表。
// 工具名冲突时用 "serverID-原名" 去重——MCP 工具名不允许点号，分隔符取 '-'。
// Windows 上 npx 是 .cmd 批处理，显式经 cmd /c 执行最可控（uvx 本身是 .exe，
// 统一走 cmd /c 让 Spec 形状保持一致）。
package registry

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Spec 描述一个下游 MCP server 的拉起命令。Windows 上 npx 是 .cmd 批处理，
// 必须经 cmd /c 间接执行（exec.Command 无法直接 CreateProcess 批处理）。
type Spec struct {
	ID   string   // 注册名，同时是冲突前缀
	Exe  string   // 例如 "cmd"
	Args []string // 例如 {"/c", "npx", "-y", "@modelcontextprotocol/server-filesystem", root}
}

// Downstream 一条活着的下游连接及其工具目录快照。
type Downstream struct {
	Spec   Spec
	Tools  []*mcp.Tool // 下游原生工具定义（原命名）
	session *mcp.ClientSession
}

// Exposed 网关对外暴露的工具条目：Tool 是改了名字的副本，Orig 留原名用于透传。
type Exposed struct {
	Tool *mcp.Tool
	Orig string
	Down *Downstream
}

type Registry struct {
	downstreams []*Downstream
	table       []Exposed
	byExposed   map[string]*Downstream
}

// New 依次拉起所有下游并抓取工具目录；任一下游失败则关闭已启动的并返回错误。
func New(ctx context.Context, specs []Spec) (*Registry, error) {
	r := &Registry{byExposed: map[string]*Downstream{}}
	for _, spec := range specs {
		d, err := r.start(ctx, spec)
		if err != nil {
			r.Close()
			return nil, err
		}
		r.downstreams = append(r.downstreams, d)
	}
	return r, nil
}

func (r *Registry) start(ctx context.Context, spec Spec) (*Downstream, error) {
	cmd := exec.Command(spec.Exe, spec.Args...)
	client := mcp.NewClient(&mcp.Implementation{Name: "agentcrate-registry", Version: "0.1.0"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return nil, fmt.Errorf("registry: %s connect: %w", spec.ID, err)
	}
	res, err := session.ListTools(ctx, nil)
	if err != nil {
		session.Close()
		return nil, fmt.Errorf("registry: %s list tools: %w", spec.ID, err)
	}
	d := &Downstream{Spec: spec, Tools: res.Tools, session: session}
	for _, t := range d.Tools {
		exposed := t.Name
		if _, clash := r.byExposed[exposed]; clash {
			exposed = spec.ID + "-" + t.Name
			// 前缀化后仍撞名（下游自带同形前缀）则继续加下划线，保证路由表无覆盖。
			for {
				if _, again := r.byExposed[exposed]; !again {
					break
				}
				exposed = "_" + exposed
			}
		}
		cp := *t
		cp.Name = exposed
		r.table = append(r.table, Exposed{Tool: &cp, Orig: t.Name, Down: d})
		r.byExposed[exposed] = d
	}
	return d, nil
}

// ToolTable 返回聚合后的网关工具表，顺序与下游声明顺序一致。
func (r *Registry) ToolTable() []Exposed { return r.table }

// Route 按网关暴露名解析路由目标。
func (r *Registry) Route(exposed string) (*Downstream, bool) {
	d, ok := r.byExposed[exposed]
	return d, ok
}

// Downstreams 返回全部下游（含未暴露任何工具的）。
func (r *Registry) Downstreams() []*Downstream { return r.downstreams }

// Session 返回下游 client session，gateway 经它透传 tools/call。
func (d *Downstream) Session() *mcp.ClientSession { return d.session }

// Close 关闭全部下游会话（SDK 负责收尾子进程：关 stdin→等退→SIGTERM）。
func (r *Registry) Close() {
	for _, d := range r.downstreams {
		_ = d.session.Close()
	}
}
