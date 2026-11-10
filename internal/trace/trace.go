// Package trace 给"一次执行"计时，并把 span 落成一条 activity 行。
//
// 依据《升级计划》P2-5：给定一个 run_id，要能还原"第 3 轮 → 意图 #42 → 第 2 次工具调用"
// 的完整时间线，并把慢请求定位到具体 span。
//
// 三个刻意的决定：
//
//  1. **span 不建表**：它是 activity 的一种（kind='span'），复用单一发射点，
//     于是"落库 + 推流 + 审计"永远同一份数据，不会出现时间线与过程记录对不上。
//  2. **trace_id 确定性**（qy-<runID>）：重启后给同一个任务补的 span 仍在同一条时间线上。
//  3. **父子关系走 context**：调用方只写 `ctx, sp := trace.Start(ctx, "tool.call", attrs)`，
//     不需要给每一层函数加参数；没有 session 时 Start 返回 nil span，所有方法 nil-safe，
//     所以老的测试与 demo 一行都不用改。
package trace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"time"

	"qinyuan/internal/model"
)

// Emit 是单一发射点（引擎实现：落库 + 推流）。
type Emit func(ctx context.Context, a *model.Activity)

type sessionKey struct{}

type session struct {
	traceID string
	runID   int64
	emit    Emit
	span    *Span
	// worker / nodeID 只影响后续新建的 span（意图级 span 会带 intent_id）。
	worker string
	nodeID int64
}

// Span 是一段被计时的执行。
//
// 零值/ nil 都是合法的：没有 trace session 时所有方法都是空操作，
// 这让"没开 trace 的环境"与"开了 trace 的环境"走同一条代码路径。
type Span struct {
	TraceID  string
	ID       string
	ParentID string
	Name     string
	RunID    int64
	NodeID   int64
	Worker   string
	Attrs    map[string]any

	ctx   context.Context
	emit  Emit
	start time.Time
	ended bool
}

// NewSession 在 ctx 上开一条时间线（trace_id 由 runID 确定性生成）。
//
// emit 为 nil 时 span 仍可创建但不会落库（只用于纯单测）。
func NewSession(ctx context.Context, runID int64, emit Emit) context.Context {
	s := &session{traceID: TraceIDFor(runID), runID: runID, emit: emit}
	return context.WithValue(ctx, sessionKey{}, s)
}

// TraceIDFor 是一个 run 的确定性 trace_id。
func TraceIDFor(runID int64) string {
	return "qy-" + strconv.FormatInt(runID, 10)
}

// TraceID 读当前 ctx 上的 trace_id（没开 session 时返回空串）。
func TraceID(ctx context.Context) string {
	if s := from(ctx); s != nil {
		return s.traceID
	}
	return ""
}

// WithWorker 让后续 span 带上 worker 名与节点 id（意图 id）。
// 它只影响"之后新建"的 span，已经开始的 span 不变。
func WithWorker(ctx context.Context, worker string, nodeID int64) context.Context {
	s := from(ctx)
	if s == nil {
		return ctx
	}
	cp := *s
	cp.worker = worker
	cp.nodeID = nodeID
	return context.WithValue(ctx, sessionKey{}, &cp)
}

// Start 开一个 span，并把 ctx 换成"当前 span 是它"的版本（子 span 自动挂上）。
//
// 没有 session 时返回 (ctx, nil)；所有方法对 nil 安全。
func Start(ctx context.Context, name string, attrs map[string]any) (context.Context, *Span) {
	s := from(ctx)
	if s == nil {
		return ctx, nil
	}
	sp := &Span{
		TraceID: s.traceID,
		ID:      newID(),
		Name:    name,
		RunID:   s.runID,
		NodeID:  s.nodeID,
		Worker:  s.worker,
		Attrs:   cloneAttrs(attrs),
		ctx:     ctx,
		emit:    s.emit,
		start:   time.Now(),
	}
	if s.span != nil {
		sp.ParentID = s.span.ID
	}
	cp := *s
	cp.span = sp
	return context.WithValue(ctx, sessionKey{}, &cp), sp
}

// Set 记录一个属性（在 End 之前调用都算数）。
func (s *Span) Set(key string, v any) {
	if s == nil {
		return
	}
	if s.Attrs == nil {
		s.Attrs = map[string]any{}
	}
	s.Attrs[key] = v
}

// Elapsed 返回已过去的时长（结束时也能读）。
func (s *Span) Elapsed() time.Duration {
	if s == nil {
		return 0
	}
	return time.Since(s.start)
}

// End 结束 span 并发射一条 activity（kind=span）。
//
// 用 WithoutCancel 写库：暂停/超时正是 cancel 之后才收尾，
// 用被取消的 ctx 会丢掉这段时间线（与 engine.settle 同一个教训）。
func (s *Span) End() {
	if s == nil || s.ended {
		return
	}
	s.ended = true
	if s.emit == nil {
		return
	}
	dur := int(time.Since(s.start).Milliseconds())
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	meta := s.Attrs
	if meta == nil {
		meta = map[string]any{}
	}
	var nodeID *int64
	if s.NodeID > 0 {
		id := s.NodeID
		nodeID = &id
	}
	summary := s.Name
	if dur > 0 {
		summary = s.Name + "（" + strconv.Itoa(dur) + "ms）"
	}
	s.emit(context.WithoutCancel(ctx), &model.Activity{
		RunID: s.RunID, NodeID: nodeID, Worker: s.Worker,
		Kind:     model.ActSpan,
		Summary:  summary,
		Metadata: meta,
		TraceID:  s.TraceID,
		SpanID:   s.ID,
		ParentSpanID: s.ParentID,
		SpanName: s.Name,
		DurationMs: dur,
	})
}

func from(ctx context.Context) *session {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(sessionKey{}).(*session)
	if s == nil {
		return nil
	}
	return s
}

func cloneAttrs(in map[string]any) map[string]any {
	if len(in) == 0 {
		return map[string]any{}
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 随机源不可用时退化成时间戳：宁可 id 不唯一，也不要丢 span。
		return "t" + strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return hex.EncodeToString(b[:])
}
