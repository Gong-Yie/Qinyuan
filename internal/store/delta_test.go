package store

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"qinyuan/internal/model"
)

// 本文件是 P1-2（差分态势）的验收：**增量重建 == 全量**。
//
// 这条性质比"差分的字段对不对"重要得多：只要有一条写路径忘了打 rev，
// 那条变化就会从增量里静默消失，模型随后基于过时态势做决策 —— 而且不会报错。
// 所以这里用"把每条写路径都走一遍，每走一步就重建一次并对账"的方式压它。

// deltaReplay 模拟"客户端侧"：手里有一份图，按增量把变化覆盖上去。
type deltaReplay struct {
	nodes map[int64]*model.Node
	edges map[string]model.Edge
	since int64
}

func newDeltaReplay() *deltaReplay {
	return &deltaReplay{nodes: map[int64]*model.Node{}, edges: map[string]model.Edge{}}
}

func (r *deltaReplay) sync(t *testing.T, st *Store, runID int64) {
	t.Helper()
	for i := 0; ; i++ {
		d, err := st.DeltaSince(context.Background(), runID, r.since, 1000)
		if err != nil {
			t.Fatalf("读增量失败：%v", err)
		}
		for _, n := range d.Nodes {
			r.nodes[n.ID] = n
		}
		for _, e := range d.Edges {
			r.edges[edgeKey(e)] = e
		}
		r.since = d.To
		if !d.Truncated {
			return
		}
		if i > 100 {
			t.Fatalf("增量分页没有收敛")
		}
	}
}

func edgeKey(e model.Edge) string {
	return fmt.Sprintf("%d|%s|%d", e.SrcID, e.Rel, e.DstID)
}

// assertSameAsFull 断言"重建出的图"与"全量读取"逐行相等。
func (r *deltaReplay) assertSameAsFull(t *testing.T, st *Store, runID int64, step string) {
	t.Helper()
	fullNodes, fullEdges := fullGraph(t, st, runID)
	if len(fullNodes) != len(r.nodes) {
		t.Fatalf("[%s] 节点数不一致：增量重建 %d，全量 %d", step, len(r.nodes), len(fullNodes))
	}
	for id, want := range fullNodes {
		got, ok := r.nodes[id]
		if !ok {
			t.Fatalf("[%s] 增量里缺节点 #%d（%s/%s）", step, id, want.Kind, want.State)
		}
		if got.State != want.State || got.Rev != want.Rev || got.Kind != want.Kind ||
			!bytes.Equal(got.Payload, want.Payload) {
			t.Fatalf("[%s] 节点 #%d 不一致：重建 state=%s rev=%d payload=%s，全量 state=%s rev=%d payload=%s",
				step, id, got.State, got.Rev, got.Payload, want.State, want.Rev, want.Payload)
		}
	}
	if len(fullEdges) != len(r.edges) {
		t.Fatalf("[%s] 边数不一致：增量重建 %d，全量 %d", step, len(r.edges), len(fullEdges))
	}
	for k, want := range fullEdges {
		got, ok := r.edges[k]
		if !ok {
			t.Fatalf("[%s] 增量里缺边 %s", step, k)
		}
		if got.Rev != want.Rev {
			t.Fatalf("[%s] 边 %s 的 rev 不一致：重建 %d，全量 %d", step, k, got.Rev, want.Rev)
		}
	}
}

func fullGraph(t *testing.T, st *Store, runID int64) (map[int64]*model.Node, map[string]model.Edge) {
	t.Helper()
	nodes := map[int64]*model.Node{}
	for _, kind := range []string{
		model.KindGoal, model.KindIntent, model.KindFact, model.KindFinding,
		model.KindHint, model.KindDigest, model.KindPlan, model.KindStep,
	} {
		list, err := st.NodesByKind(context.Background(), runID, kind, 5000)
		if err != nil {
			t.Fatalf("读全量 %s 失败：%v", kind, err)
		}
		for _, n := range list {
			nodes[n.ID] = n
		}
	}
	edges := map[string]model.Edge{}
	es, err := st.Edges(context.Background(), runID)
	if err != nil {
		t.Fatalf("读全量边失败：%v", err)
	}
	for _, e := range es {
		edges[edgeKey(e)] = e
	}
	return nodes, edges
}

// TestDeltaReconstructsFullGraph 把每条写路径都走一遍，每步之后对账。
func TestDeltaReconstructsFullGraph(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	run := newTestRun(t, st)
	rep := newDeltaReplay()

	verify := func(step string) {
		rep.sync(t, st, run.ID)
		rep.assertSameAsFull(t, st, run.ID, step)
	}
	verify("建 run（origin + seed 目标）")

	intent1, err := st.AddIntent(ctx, AddIntentInput{RunID: run.ID, Summary: "第一步：请求首页"})
	if err != nil {
		t.Fatalf("add_intent: %v", err)
	}
	verify("add_intent")

	if ok, err := st.ClaimIntent(ctx, run.ID, intent1, "work#1"); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	verify("claim")

	fact1, err := st.AddFact(ctx, AddFactInput{
		RunID: run.ID, IntentID: intent1,
		Summary: "首页返回 200，Server: nginx", Evidence: "curl -sI / → 200", Confidence: "observed",
	})
	if err != nil {
		t.Fatalf("add_fact: %v", err)
	}
	verify("record_fact")

	if ok, err := st.CASIntent(ctx, run.ID, intent1, model.StateRunning, model.StateDone); err != nil || !ok {
		t.Fatalf("cas done: ok=%v err=%v", ok, err)
	}
	verify("CAS 意图 done")

	finding1, err := st.AddFinding(ctx, AddFindingInput{
		RunID: run.ID, IntentID: intent1, Name: "目录遍历", Severity: "high",
		Summary: "下载接口可遍历", Evidence: "GET /dl?f=../../etc/passwd → root:x:0:0",
	})
	if err != nil {
		t.Fatalf("report_finding: %v", err)
	}
	verify("report_finding")

	// 计划图：AddStep 会顺手建 plan 节点，两者都必须在同一个窗口里出现。
	step1, err := st.AddStep(ctx, AddStepInput{
		RunID: run.ID, Text: "确认入口", ExpectedEvidence: "首页返回 200",
	})
	if err != nil {
		t.Fatalf("add_plan_step: %v", err)
	}
	verify("add_plan_step（含 plan 节点）")

	step2, err := st.AddStep(ctx, AddStepInput{
		RunID: run.ID, Text: "确认可利用", ExpectedEvidence: "拿到 /etc/passwd 内容", DependsOn: []int64{step1},
	})
	if err != nil {
		t.Fatalf("add_plan_step 2: %v", err)
	}
	verify("add_plan_step（带依赖）")

	intent2, err := st.AddIntent(ctx, AddIntentInput{RunID: run.ID, Summary: "第二步：读 /etc/passwd", ParentIDs: []int64{fact1}})
	if err != nil {
		t.Fatalf("add_intent 2: %v", err)
	}
	verify("add_intent（带 parent）")

	if err := st.LinkStepIntent(ctx, run.ID, step2, intent2); err != nil {
		t.Fatalf("link step intent: %v", err)
	}
	verify("realized_by + 步骤 running")

	// 规则命中：事实文本包含 expected_evidence → 步骤自动 done，下一步 ready 由代码重新判定。
	satisfied, err := st.SatisfyStepsByFact(ctx, run.ID, fact1, "首页返回 200，Server: nginx")
	if err != nil {
		t.Fatalf("satisfy: %v", err)
	}
	if len(satisfied) == 0 {
		t.Fatalf("规则命中失败：至少应该满足 step1")
	}
	verify("satisfies（规则命中）")

	if ok, err := st.MarkStepDone(ctx, run.ID, step2, finding1); err != nil || !ok {
		t.Fatalf("mark_step_done: ok=%v err=%v", ok, err)
	}
	verify("mark_step_done（显式判定）")

	goals, err := st.Goals(ctx, run.ID)
	if err != nil || len(goals) == 0 {
		t.Fatalf("读目标失败：%v", err)
	}
	if _, err := st.ProveGoal(ctx, run.ID, goals[0].ID, fact1, "首页指纹确认"); err != nil {
		t.Fatalf("prove_goal: %v", err)
	}
	verify("prove_goal")

	if _, err := st.AddGoal(ctx, run.ID, "第二个目标：拿到管理后台入口", "", "human"); err != nil {
		t.Fatalf("add_goal: %v", err)
	}
	verify("add_goal")

	if _, err := st.ReplaceGoalsFromDecomposition(ctx, run.ID, []AddGoalSpec{
		{Text: "证明下载接口可遍历（/etc/passwd 可读）", VulnClass: "path-traversal"},
		{Text: "确认后台登录入口存在"},
	}); err != nil {
		t.Fatalf("replace goals: %v", err)
	}
	verify("目标拆解替换")

	if err := st.Link(ctx, run.ID, finding1, fact1, model.RelDerivedFrom); err != nil {
		t.Fatalf("link: %v", err)
	}
	verify("link（裸边）")

	blocked, err := st.AddIntent(ctx, AddIntentInput{RunID: run.ID, Summary: "会被 blocked 的方向"})
	if err != nil {
		t.Fatalf("add_intent 3: %v", err)
	}
	if ok, _ := st.ClaimIntent(ctx, run.ID, blocked, "work#2"); !ok {
		t.Fatalf("claim blocked 意图失败")
	}
	if err := st.SetBlockedReason(ctx, run.ID, blocked, "quota"); err != nil {
		t.Fatalf("set_blocked_reason: %v", err)
	}
	verify("set_blocked_reason")

	if ok, err := st.CASIntent(ctx, run.ID, blocked, model.StateRunning, model.StateBlocked); err != nil || !ok {
		t.Fatalf("cas blocked: ok=%v err=%v", ok, err)
	}
	verify("CAS 意图 blocked")

	// 没有新变化时：to == since，且不产生任何事件。
	d, err := st.DeltaSince(ctx, run.ID, rep.since, 1000)
	if err != nil {
		t.Fatalf("空增量读取失败：%v", err)
	}
	if !d.Empty() || d.From != d.To {
		t.Fatalf("空窗口不该有事件：from=%d to=%d nodes=%d edges=%d", d.From, d.To, len(d.Nodes), len(d.Edges))
	}

	// 版本必须单调到"当前版本"：不能有 rev > runs.version 的行。
	full, _ := st.Run(ctx, run.ID)
	if rep.since != full.Version {
		t.Fatalf("重建后的游标 %d != run.version %d（说明有写入没分配版本）", rep.since, full.Version)
	}
	for _, n := range rep.nodes {
		if n.Rev > full.Version {
			t.Fatalf("节点 #%d 的 rev=%d 超过 runs.version=%d", n.ID, n.Rev, full.Version)
		}
	}
}

// TestDeltaTruncatesWhenTooManyChanges 变化太多时必须报 Truncated（调用方退全量）。
func TestDeltaTruncatesWhenTooManyChanges(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	run := newTestRun(t, st)

	for i := 0; i < 5; i++ {
		if _, err := st.AddIntent(ctx, AddIntentInput{
			RunID: run.ID, Summary: fmt.Sprintf("方向 %d", i),
		}); err != nil {
			t.Fatalf("add_intent: %v", err)
		}
	}

	d, err := st.DeltaSince(ctx, run.ID, 0, 3)
	if err != nil {
		t.Fatalf("读增量失败：%v", err)
	}
	if !d.Truncated {
		t.Fatalf("超过上限时应当 Truncated（total=%d limit=3）", d.Total)
	}
	if d.Total < 5 {
		t.Fatalf("total 统计不对：%d", d.Total)
	}
	if len(d.Nodes) != 0 || len(d.Edges) != 0 {
		t.Fatalf("截断时不该返回半截数据（否则调用方容易误用）")
	}
}

// TestDeltaWindowIsExclusive 窗口是 (since, to]：同一个版本不能被读两次。
func TestDeltaWindowIsExclusive(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	run := newTestRun(t, st)

	intentID, err := st.AddIntent(ctx, AddIntentInput{RunID: run.ID, Summary: "A"})
	if err != nil {
		t.Fatal(err)
	}
	d1, err := st.DeltaSince(ctx, run.ID, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if d1.Empty() {
		t.Fatalf("第一次增量不该为空")
	}
	d2, err := st.DeltaSince(ctx, run.ID, d1.To, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if !d2.Empty() {
		t.Fatalf("同一窗口被读了两次：第二次仍有 %d 条", len(d2.Nodes)+len(d2.Edges))
	}

	if _, err := st.CASIntent(ctx, run.ID, intentID, model.StateOpen, model.StateRunning); err != nil {
		t.Fatal(err)
	}
	d3, err := st.DeltaSince(ctx, run.ID, d1.To, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(d3.Nodes) != 1 || d3.Nodes[0].ID != intentID || d3.Nodes[0].State != model.StateRunning {
		t.Fatalf("第二次写入没被增量捕获：%+v", d3.Nodes)
	}
}
