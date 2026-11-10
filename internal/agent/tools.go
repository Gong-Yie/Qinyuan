// Package agent 是"给模型的动作空间"：工具定义、prompt 组装、单次执行循环。
//
// 依赖规则（实现规格/00 §3）：agent 不导入编排层；它只认识 store + context，
// 跑完就返回一个 Outcome。
package agent

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"

	"qinyuan/internal/domain"
	"qinyuan/internal/llm"
	"qinyuan/internal/model"
	"qinyuan/internal/store"
)

// Emitter 是"单一发射点"的入口：落库 + 推流都由引擎实现，工具层只负责调用。
type Emitter func(ctx context.Context, a *model.Activity)

// Context 一次工具调用能看到的全部上下文（没有 store 就没有写能力）。
type Context struct {
	Store         *store.Store
	RunID         int64
	IntentID      int64
	Scopes        *domain.ScopeSet
	Constraints   []model.Constraint
	DataDir       string
	AllowLoopback bool
	MaxOutput     int
	// ToolUseID 是"当前正在执行的这一次工具调用"的 id（worker 每次调用前设置）。
	// 流量录制用它把交换和模型那次 tool_use 对上。
	ToolUseID string

	// Final=true 表示这一轮是收尾轮：只判定目标，不再派新意图。
	Final bool
	// KillWork 由编排层注入（planner 的 kill_work 用；worker 为 nil）。
	KillWork func(intentID int64) bool
	// SteerWork 由编排层注入（planner 的 steer_work 用；worker 为 nil）。
	SteerWork func(intentID int64, message string) bool
	// RefreshRequested 由 refresh_overview 置位：下一轮 planner 重新给全量态势。
	// planner 为 nil（worker 没有这个工具）。
	RefreshRequested *bool
}

// Result 工具返回给模型的文本。
type Result struct {
	Text    string
	IsError bool
}

// Errorf 返回一条"模型能据此改"的错误文本（不是 Go error）。
func Errorf(format string, args ...any) Result {
	return Result{Text: fmt.Sprintf(format, args...), IsError: true}
}

// Textf 返回普通文本。
func Textf(format string, args ...any) Result {
	return Result{Text: fmt.Sprintf(format, args...)}
}

// JSONResult 把结构化结果序列化给模型看（成功也要给 id，模型下一步靠它）。
func JSONResult(v any) Result {
	b, err := json.Marshal(v)
	if err != nil {
		return Errorf("结果序列化失败：%v", err)
	}
	return Result{Text: string(b)}
}

// Tool 是纯数据 + 一个 handler。
type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage
	ReadOnly    bool
	Handle      func(ctx context.Context, rc *Context, input json.RawMessage) (Result, error)
}

// Registry 按角色组装工具面。
type Registry struct {
	tools map[string]*Tool
	order []string
}

// NewRegistry 组装一个工具集（顺序即展示顺序）。
func NewRegistry(tools ...*Tool) *Registry {
	r := &Registry{tools: map[string]*Tool{}}
	for _, t := range tools {
		if t == nil {
			continue
		}
		r.tools[t.Name] = t
		r.order = append(r.order, t.Name)
	}
	return r
}

// Names 返回工具名（按注册顺序）。
func (r *Registry) Names() []string { return append([]string(nil), r.order...) }

// IsReadOnly 报告某个工具是否只读（span 属性与"哪些调用不该要审批"都用它）。
func (r *Registry) IsReadOnly(name string) bool {
	if r == nil {
		return false
	}
	t, ok := r.tools[name]
	return ok && t.ReadOnly
}

// Without 返回一个去掉指定工具的新 registry（收尾轮用它隐藏危险工具）。
//
// 返回新对象而不是原地删：同一个 registry 会被别的回合复用，改掉了会污染下一轮。
func (r *Registry) Without(names ...string) *Registry {
	if r == nil {
		return nil
	}
	drop := make(map[string]bool, len(names))
	for _, n := range names {
		drop[n] = true
	}
	out := &Registry{tools: make(map[string]*Tool, len(r.tools))}
	for _, name := range r.order {
		if drop[name] {
			continue
		}
		out.tools[name] = r.tools[name]
		out.order = append(out.order, name)
	}
	return out
}

// Specs 给模型看的定义。
func (r *Registry) Specs() []llm.ToolSpec {
	out := make([]llm.ToolSpec, 0, len(r.order))
	for _, name := range r.order {
		t := r.tools[name]
		out = append(out, llm.ToolSpec{Name: t.Name, Description: t.Description, Schema: t.Schema})
	}
	return out
}

// Call 执行一次工具调用。
//
// 两个生存必需的保护（学习.md 5.6）：
//  1. recover：工具跑在独立 goroutine 上，panic 会带走整个进程，而且重启后复现；
//  2. store 为 nil：返回错误文本，不 panic。
func (r *Registry) Call(ctx context.Context, rc *Context, name string, input json.RawMessage) (res Result) {
	t, ok := r.tools[name]
	if !ok {
		return Errorf("未知工具 %q。可用工具：%s", name, strings.Join(r.order, ", "))
	}
	if rc == nil || rc.Store == nil {
		return Errorf("工具 %s 需要任务上下文（store 未启用），请在任务会话里使用", name)
	}
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("工具 panic", "tool", name, "panic", rec, "stack", string(debug.Stack()))
			res = Errorf("工具 %s 内部错误：%v（本次调用已失败，请换个参数或换个工具）", name, rec)
		}
	}()

	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	if !json.Valid(input) {
		return Errorf("工具 %s 的入参不是合法 JSON", name)
	}
	res, err := t.Handle(ctx, rc, input)
	if err != nil {
		return Errorf("工具 %s 执行失败：%v", name, err)
	}
	return res
}

// ---------- 大输出落盘 ----------

// persistOutput 把超长输出落到 run 目录，返回路径（幂等：同一内容同一路径）。
func persistOutput(rc *Context, kind, content string) (string, error) {
	sum := sha1.Sum([]byte(content))
	name := hex.EncodeToString(sum[:]) + ".txt"
	dir := filepath.Join(rc.DataDir, "runs", fmt.Sprintf("%d", rc.RunID), "cmd-output")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, name)
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return path, nil
}

// clampOutput 按 MaxOutput 截断，并保证截断后带"怎么取全"的指针。
func clampOutput(rc *Context, kind, content string) string {
	limit := rc.MaxOutput
	if limit <= 0 {
		limit = 30000
	}
	return clampOutputN(rc, kind, content, limit)
}

// clampOutputN 是 clampOutput 的"指定更小上限"版本：用于 http 响应这类
// "大多数时候只需要看开头"的内容（两档返回：head + 落盘全文）。
func clampOutputN(rc *Context, kind, content string, limit int) string {
	if limit <= 0 {
		limit = 30000
	}
	if len(content) <= limit {
		return content
	}
	path, err := persistOutput(rc, kind, content)
	if err != nil {
		return content[:limit] + fmt.Sprintf("\n\n…（已截断到 %d 字节；落盘失败：%v）", limit, err)
	}
	return content[:limit] + "\n\n…（已截断，完整内容见 " + path + "；可用 Read 工具读取）"
}

// toolSpecJSON 是写 schema 时的小工具（保证 JSON 合法）。
func toolSpecJSON(s string) json.RawMessage {
	if !json.Valid([]byte(s)) {
		panic("tool schema 不是合法 JSON：" + s)
	}
	return json.RawMessage(s)
}
