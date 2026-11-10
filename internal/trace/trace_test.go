package trace

import (
	"context"
	"strings"
	"testing"

	"qinyuan/internal/model"
)

// 没有 trace session 时，Start 返回 nil span，所有方法必须是空操作 ——
// 这样老的测试与 demo 一行都不用改。
func TestNoSessionIsNoop(t *testing.T) {
	ctx, sp := Start(context.Background(), "tool.call", map[string]any{"tool": "x"})
	if sp != nil {
		t.Fatalf("无 session 时应当返回 nil span，实际 %+v", sp)
	}
	sp.Set("k", "v") // 不 panic
	sp.End()
	sp.End()
	if TraceID(ctx) != "" {
		t.Fatalf("无 session 时 trace_id 应为空")
	}
}

// span 树：父子的 trace/span/parent 关系、属性、kind 都要对。
func TestSpanTreeAndEmit(t *testing.T) {
	var got []*model.Activity
	emit := func(_ context.Context, a *model.Activity) { got = append(got, a) }

	ctx := NewSession(context.Background(), 42, emit)
	if TraceID(ctx) != "qy-42" {
		t.Fatalf("trace_id = %q，期望 qy-42（确定性：重启后同一条线）", TraceID(ctx))
	}
	ctx = WithWorker(ctx, "work#2", 7)

	ctx, parent := Start(ctx, "intent.execute", map[string]any{"intent_id": 7})
	_, child := Start(ctx, "tool.call", map[string]any{"tool": "http_request"})
	child.Set("error", false)
	child.End()
	grandCtx, grand := Start(context.WithValue(ctx, ctxKeyProbe{}, 1), "llm.call", nil)
	_ = grandCtx
	grand.End()
	parent.End()

	if len(got) != 3 {
		t.Fatalf("应当发射 3 个 span，实际 %d", len(got))
	}
	for _, a := range got {
		if a.Kind != model.ActSpan {
			t.Fatalf("kind 应为 span，实际 %q", a.Kind)
		}
		if a.TraceID != "qy-42" || a.SpanID == "" {
			t.Fatalf("span 缺 trace/span id：%+v", a)
		}
		if a.RunID != 42 || a.Worker != "work#2" || a.NodeID == nil || *a.NodeID != 7 {
			t.Fatalf("span 归属不对：%+v", a)
		}
		if a.DurationMs < 0 {
			t.Fatalf("duration_ms 为负：%d", a.DurationMs)
		}
		if !strings.Contains(a.Summary, a.SpanName) {
			t.Fatalf("summary 应包含 span 名：%q", a.Summary)
		}
	}

	childAct, llmAct, parentAct := got[0], got[1], got[2]
	if childAct.ParentSpanID != parentAct.SpanID {
		t.Fatalf("tool.call 的父应为 intent.execute：%+v", childAct)
	}
	if llmAct.ParentSpanID != parentAct.SpanID {
		t.Fatalf("llm.call 的父应为 intent.execute：%+v", llmAct)
	}
	if parentAct.ParentSpanID != "" {
		t.Fatalf("根 span 不该有父：%+v", parentAct)
	}
	if childAct.Metadata["tool"] != "http_request" {
		t.Fatalf("属性没带上：%+v", childAct.Metadata)
	}
}

// End 幂等：重复调用不能发射第二条（收尾路径常常 defer + 显式各一次）。
func TestEndIdempotent(t *testing.T) {
	var n int
	ctx := NewSession(context.Background(), 1, func(context.Context, *model.Activity) { n++ })
	_, sp := Start(ctx, "x", nil)
	sp.End()
	sp.End()
	if n != 1 {
		t.Fatalf("End 应幂等，实际发射 %d 次", n)
	}
}

type ctxKeyProbe struct{}
