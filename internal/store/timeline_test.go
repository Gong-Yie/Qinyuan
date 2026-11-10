package store

import (
	"context"
	"testing"

	"qinyuan/internal/model"
)

// P2-5：span 行能被读出来，普通活动不混进时间线，分位聚合按名字分组。
func TestTimelineSpansAndStats(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	run := newTestRun(t, s)

	appendSpan := func(name, spanID, parent string, dur int, isErr bool) {
		t.Helper()
		if _, err := s.AppendActivity(ctx, &model.Activity{
			RunID: run.ID, Worker: "w1", Kind: model.ActSpan, SpanName: name,
			TraceID: "qy-x", SpanID: spanID, ParentSpanID: parent, DurationMs: dur,
			IsError: isErr, Summary: name,
			Metadata: map[string]any{"k": "v"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	appendSpan("intent.execute", "s1", "", 900, false)
	appendSpan("tool.call", "s2", "s1", 300, true)
	appendSpan("tool.call", "s3", "s1", 500, false)
	// 普通活动：不该出现在时间线里。
	if _, err := s.AppendActivity(ctx, &model.Activity{
		RunID: run.ID, Worker: "w1", Kind: model.ActText, Summary: "一段正文",
	}); err != nil {
		t.Fatal(err)
	}

	spans, err := s.TimelineSpans(ctx, run.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 3 {
		t.Fatalf("span 行数 = %d，期望 3（普通活动不算）", len(spans))
	}
	if spans[0].SpanName != "intent.execute" || spans[0].DurationMs != 900 {
		t.Fatalf("第一条不对：%+v", spans[0])
	}
	if spans[1].ParentSpanID != "s1" {
		t.Fatalf("父子关系没落库：%+v", spans[1])
	}
	if spans[1].Metadata["k"] != "v" {
		t.Fatalf("metadata 没读回：%+v", spans[1].Metadata)
	}

	// since 只取新行（给增量轮询用）。
	last := spans[2].ID
	more, err := s.TimelineSpans(ctx, run.ID, last, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(more) != 0 {
		t.Fatalf("since=%d 之后不该有新 span，实际 %d 条", last, len(more))
	}

	stats, err := s.SpanStats(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 2 {
		t.Fatalf("按名字应聚合出 2 组，实际 %d：%+v", len(stats), stats)
	}
	// 按总耗时降序：intent.execute(900) 在 tool.call(800) 前面。
	if stats[0].Name != "intent.execute" || stats[0].Count != 1 || stats[0].P99Ms != 900 {
		t.Fatalf("intent.execute 聚合不对：%+v", stats[0])
	}
	if stats[1].Name != "tool.call" || stats[1].Count != 2 || stats[1].MaxMs != 500 || stats[1].Errors != 1 {
		t.Fatalf("tool.call 聚合不对：%+v", stats[1])
	}
}
