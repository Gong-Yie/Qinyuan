package store

import (
	"context"
	"strings"
	"testing"

	"qinyuan/internal/model"
)

// origin fact 是任务起点，不能当证据；真实事实可以。
func TestProveGoalRejectsOriginFact(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	run := newTestRun(t, s)

	goals, err := s.Goals(ctx, run.ID)
	if err != nil || len(goals) != 1 {
		t.Fatalf("目标缺失：%v", err)
	}
	facts, _ := s.NodesByKind(ctx, run.ID, model.KindFact, 10)
	var originID int64
	for _, f := range facts {
		if f.State == model.StateOrigin {
			originID = f.ID
		}
	}
	if originID == 0 {
		t.Fatal("origin fact 缺失")
	}

	_, err = s.ProveGoal(ctx, run.ID, goals[0].ID, originID, "拿起点当证据")
	if err == nil || !strings.Contains(err.Error(), "origin") {
		t.Fatalf("应当拒绝 origin fact 作为证据，实际 err=%v", err)
	}

	factID, err := s.AddFact(ctx, AddFactInput{
		RunID: run.ID, Summary: "端口 8080 开放且返回 200", Evidence: "GET / → 200", Confidence: "observed",
	})
	if err != nil {
		t.Fatal(err)
	}
	remaining, err := s.ProveGoal(ctx, run.ID, goals[0].ID, factID, "已实测")
	if err != nil {
		t.Fatalf("用真实事实证明失败：%v", err)
	}
	if remaining != 0 {
		t.Fatalf("剩余未达成目标 = %d，期望 0", remaining)
	}
	got, err := s.Node(ctx, run.ID, goals[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.StateMet {
		t.Fatalf("目标状态 = %s，期望 met", got.State)
	}
	// 幂等性：重复证明同一个目标必须失败，而不是反复改状态。
	if _, err := s.ProveGoal(ctx, run.ID, goals[0].ID, factID, "再来一次"); err == nil {
		t.Fatal("重复证明同一个目标应当报错")
	}
}

// 用意图（而不是事实/发现）当证据必须被拒。
func TestProveGoalRejectsIntentEvidence(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	run := newTestRun(t, s)
	goals, _ := s.Goals(ctx, run.ID)
	intentID, err := s.AddIntent(ctx, AddIntentInput{RunID: run.ID, Summary: "一条方向"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProveGoal(ctx, run.ID, goals[0].ID, intentID, "拿意图当证据"); err == nil {
		t.Fatal("意图不能作为目标证据")
	}
}
