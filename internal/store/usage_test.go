package store

import (
	"context"
	"testing"

	"qinyuan/internal/model"
)

// P4 观测：每节点的边际成本只从 kind='result' 的权威行算。
func TestUsageByNode(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	run := newTestRun(t, s)

	intentID, err := s.AddIntent(ctx, AddIntentInput{RunID: run.ID, Summary: "贵的那条"})
	if err != nil {
		t.Fatal(err)
	}
	appendResult := func(nodeID *int64, in, out, cache int) {
		t.Helper()
		if _, err := s.AppendActivity(ctx, &model.Activity{
			RunID: run.ID, NodeID: nodeID, Worker: "w", Kind: model.ActResult,
			Summary: "执行结束", InputTokens: in, OutputTokens: out, CacheReadTokens: cache,
		}); err != nil {
			t.Fatal(err)
		}
	}
	appendResult(&intentID, 10000, 800, 7000)
	appendResult(&intentID, 5000, 400, 4500)
	appendResult(nil, 2000, 200, 0) // 任务级（planner）

	// 非 result 行（usage 帧）必须被忽略，否则会重复计数。
	if _, err := s.AppendActivity(ctx, &model.Activity{
		RunID: run.ID, NodeID: &intentID, Worker: "w", Kind: "usage",
		InputTokens: 999999, OutputTokens: 999999,
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := s.UsageByNode(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("应聚合出 2 个节点，实际 %d：%+v", len(rows), rows)
	}
	// 最贵的排在最前：意图（有效输入 15000-11500=3500）vs 任务级（2000）。
	if rows[0].NodeID != intentID {
		t.Fatalf("排序不对，第一条应是意图 #%d，实际 %+v", intentID, rows[0])
	}
	if rows[0].Calls != 2 || rows[0].Input != 15000 || rows[0].Output != 1200 || rows[0].CacheRead != 11500 {
		t.Fatalf("意图聚合不对：%+v", rows[0])
	}
	if rows[0].EffectiveIn != 3500 {
		t.Fatalf("有效输入 = %d，期望 3500（input - cache_read）", rows[0].EffectiveIn)
	}
	if rows[0].Kind != model.KindIntent || rows[0].Summary != "贵的那条" {
		t.Fatalf("没带上节点语义：%+v", rows[0])
	}
	if rows[1].NodeID != 0 || rows[1].Calls != 1 {
		t.Fatalf("任务级聚合不对：%+v", rows[1])
	}
}
