package store

import (
	"context"
	"testing"

	"qinyuan/internal/model"
)

// 仪表盘/发现/资产三个列表页的数据源：口径要一致（只认 result 行、finding 读节点）。
func TestDashboardAggregates(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	run, err := s.CreateRun(ctx, CreateRunInput{
		Goal: "仪表盘聚合", WorkerCount: 1,
		Scopes:       []model.Scope{{Kind: "ip", Value: "127.0.0.1", Source: "manual"}},
		SeedEntities: []EntityInput{{Kind: "ip", IP: "127.0.0.1", Segment: "127.0.0.0/24"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	intentID, err := s.AddIntent(ctx, AddIntentInput{RunID: run.ID, Summary: "方向"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFact(ctx, AddFactInput{
		RunID: run.ID, IntentID: intentID, Summary: "8080 开放", Evidence: "curl → 200",
		Confidence: "observed",
	}); err != nil {
		t.Fatal(err)
	}
	// 一条 result 行 + 一条 usage 行：token 只能算前者。
	if _, err := s.AppendActivity(ctx, &model.Activity{
		RunID: run.ID, NodeID: &intentID, Worker: "work#1", Kind: model.ActResult,
		Summary: "执行结束", InputTokens: 1000, OutputTokens: 100, CacheReadTokens: 800,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendActivity(ctx, &model.Activity{
		RunID: run.ID, NodeID: &intentID, Worker: "work#1", Kind: "usage",
		InputTokens: 9999, OutputTokens: 9999,
	}); err != nil {
		t.Fatal(err)
	}

	st, err := s.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Runs != 1 || st.Facts != 1 || st.Intents != 1 {
		t.Fatalf("统计不对：runs=%d facts=%d intents=%d", st.Runs, st.Facts, st.Intents)
	}
	if st.InputTokens != 1000 || st.CacheRead != 800 || st.Output != 100 {
		t.Fatalf("token 只该统计 result 行：%+v", st)
	}
	if st.Entities != 1 {
		t.Fatalf("实体数 = %d，期望 1", st.Entities)
	}

	ents, err := s.ListEntities(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].IP != "127.0.0.1" || ents[0].Kind != "ip" {
		t.Fatalf("资产列表不对：%+v", ents)
	}

	// 没有 finding 时是空列表（不是一条假数据）。
	fs, err := s.ListFindings(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 0 {
		t.Fatalf("不该有发现：%+v", fs)
	}
}
