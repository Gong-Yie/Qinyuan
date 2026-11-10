package store

import (
	"context"
	"testing"

	"qinyuan/internal/model"
)

// P1：意图落终态时，挂它的 running 步骤必须松手，否则计划图永远显示"正在跑"。
func TestIntentTerminalReleasesLinkedStep(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	run := newTestRun(t, s)

	stepID, err := s.AddStep(ctx, AddStepInput{RunID: run.ID, Text: "一步", ExpectedEvidence: "永远不会命中的判据"})
	if err != nil {
		t.Fatal(err)
	}
	intentID, err := s.AddIntent(ctx, AddIntentInput{RunID: run.ID, Summary: "方向"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.LinkStepIntent(ctx, run.ID, stepID, intentID); err != nil {
		t.Fatal(err)
	}
	if pg, _ := s.PlanGraph(ctx, run.ID); stateOf(t, pg, stepID) != model.StateRunning {
		t.Fatalf("派工后步骤应为 running，实际 %s", stateOf(t, pg, stepID))
	}

	if ok, err := s.CASIntent(ctx, run.ID, intentID, "open", "running"); err != nil || !ok {
		t.Fatalf("领取失败：ok=%v err=%v", ok, err)
	}
	if ok, err := s.CASIntent(ctx, run.ID, intentID, "running", "done"); err != nil || !ok {
		t.Fatalf("收尾失败：ok=%v err=%v", ok, err)
	}

	pg, err := s.PlanGraph(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := stateOf(t, pg, stepID); got != model.StateReady {
		t.Fatalf("意图 done 后步骤应回到 ready（可重派），实际 %s", got)
	}
}

// 但有别的意图还在跑同一件事时，不能抢它的状态。
func TestIntentTerminalKeepsStepWhileSiblingRuns(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	run := newTestRun(t, s)

	stepID, err := s.AddStep(ctx, AddStepInput{RunID: run.ID, Text: "一步", ExpectedEvidence: "x"})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := s.AddIntent(ctx, AddIntentInput{RunID: run.ID, Summary: "方向 A"})
	b, _ := s.AddIntent(ctx, AddIntentInput{RunID: run.ID, Summary: "方向 B"})
	if err := s.LinkStepIntent(ctx, run.ID, stepID, a); err != nil {
		t.Fatal(err)
	}
	if err := s.LinkStepIntent(ctx, run.ID, stepID, b); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CASIntent(ctx, run.ID, a, "open", "running"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CASIntent(ctx, run.ID, b, "open", "running"); err != nil {
		t.Fatal(err)
	}

	// A 结束，B 还在跑 → 步骤保持 running。
	if _, err := s.CASIntent(ctx, run.ID, a, "running", "stopped"); err != nil {
		t.Fatal(err)
	}
	pg, _ := s.PlanGraph(ctx, run.ID)
	if got := stateOf(t, pg, stepID); got != model.StateRunning {
		t.Fatalf("还有意图在跑时应保持 running，实际 %s", got)
	}

	// B 也结束 → 松手。
	if _, err := s.CASIntent(ctx, run.ID, b, "running", "blocked"); err != nil {
		t.Fatal(err)
	}
	pg, _ = s.PlanGraph(ctx, run.ID)
	if got := stateOf(t, pg, stepID); got != model.StateReady {
		t.Fatalf("两条意图都结束后应回到 ready，实际 %s", got)
	}
}

// 已经有事实 satisfies 它时，松手的结果是 done，不是 ready。
func TestIntentTerminalMarksStepDoneWhenSatisfied(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	run := newTestRun(t, s)

	stepID, err := s.AddStep(ctx, AddStepInput{RunID: run.ID, Text: "一步", ExpectedEvidence: "x"})
	if err != nil {
		t.Fatal(err)
	}
	intentID, _ := s.AddIntent(ctx, AddIntentInput{RunID: run.ID, Summary: "方向"})
	if err := s.LinkStepIntent(ctx, run.ID, stepID, intentID); err != nil {
		t.Fatal(err)
	}
	factID, err := s.AddFact(ctx, AddFactInput{
		RunID: run.ID, Summary: "拿到了", Evidence: "curl → 200", Confidence: "observed",
	})
	if err != nil {
		t.Fatal(err)
	}
	// 模拟"事实已 satisfies，但步骤仍是 running"（规则匹配晚于派工）。
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO edges (run_id, src_id, rel, dst_id) VALUES ($1,$2,'satisfies',$3)
		ON CONFLICT DO NOTHING`, run.ID, factID, stepID); err != nil {
		t.Fatal(err)
	}

	if _, err := s.CASIntent(ctx, run.ID, intentID, "open", "running"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CASIntent(ctx, run.ID, intentID, "running", "done"); err != nil {
		t.Fatal(err)
	}
	pg, _ := s.PlanGraph(ctx, run.ID)
	if got := stateOf(t, pg, stepID); got != model.StateDone {
		t.Fatalf("已有 satisfies 时应落 done，实际 %s", got)
	}
}

// 依赖没做完时松手到 todo（不能提前 ready，否则绕过依赖）。
func TestIntentTerminalRespectsDependencies(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	run := newTestRun(t, s)

	first, err := s.AddStep(ctx, AddStepInput{RunID: run.ID, Text: "第一步", ExpectedEvidence: "a"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.AddStep(ctx, AddStepInput{RunID: run.ID, Text: "第二步", ExpectedEvidence: "b", DependsOn: []int64{first}})
	if err != nil {
		t.Fatal(err)
	}
	intentID, _ := s.AddIntent(ctx, AddIntentInput{RunID: run.ID, Summary: "做第二步"})
	if err := s.LinkStepIntent(ctx, run.ID, second, intentID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CASIntent(ctx, run.ID, intentID, "open", "running"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CASIntent(ctx, run.ID, intentID, "running", "exhausted"); err != nil {
		t.Fatal(err)
	}
	pg, _ := s.PlanGraph(ctx, run.ID)
	if got := stateOf(t, pg, second); got != model.StateTodo {
		t.Fatalf("前置未完成时应回 todo，实际 %s", got)
	}
}

// paused 不算终态：它还能 resume，步骤要保持 running。
func TestIntentPausedKeepsStepRunning(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	run := newTestRun(t, s)

	stepID, err := s.AddStep(ctx, AddStepInput{RunID: run.ID, Text: "一步", ExpectedEvidence: "x"})
	if err != nil {
		t.Fatal(err)
	}
	intentID, _ := s.AddIntent(ctx, AddIntentInput{RunID: run.ID, Summary: "方向"})
	if err := s.LinkStepIntent(ctx, run.ID, stepID, intentID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CASIntent(ctx, run.ID, intentID, "open", "running"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CASIntent(ctx, run.ID, intentID, "running", "paused"); err != nil {
		t.Fatal(err)
	}
	pg, _ := s.PlanGraph(ctx, run.ID)
	if got := stateOf(t, pg, stepID); got != model.StateRunning {
		t.Fatalf("paused 时步骤应保持 running（等 resume），实际 %s", got)
	}
}

func stateOf(t *testing.T, pg *PlanGraph, stepID int64) string {
	t.Helper()
	for _, st := range pg.Steps {
		if st.ID == stepID {
			return st.State
		}
	}
	t.Fatalf("计划图里没有步骤 #%d", stepID)
	return ""
}
