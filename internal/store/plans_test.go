package store

import (
	"context"
	"testing"

	"qinyuan/internal/model"
)

// P1-1 的核心验收：5 步串行链只有第 1 步 ready；产出 fact 后第 2 步自动 ready（无 LLM 参与）。
func TestPlanChainReadyByDependency(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	run := newTestRun(t, s)

	var ids []int64
	for i := 1; i <= 5; i++ {
		var deps []int64
		if i > 1 {
			deps = []int64{ids[i-2]}
		}
		id, err := s.AddStep(ctx, AddStepInput{
			RunID: run.ID, Text: "第 " + itoa(i) + " 步",
			ExpectedEvidence: "证据" + itoa(i), DependsOn: deps,
		})
		if err != nil {
			t.Fatalf("第 %d 步创建失败：%v", i, err)
		}
		ids = append(ids, id)
	}

	pg, err := s.PlanGraph(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	ready := 0
	for _, st := range pg.Steps {
		if st.State == model.StateReady {
			ready++
		}
	}
	if ready != 1 {
		t.Fatalf("5 步串行链应只有 1 步 ready，实际 %d：%+v", ready, pg.Steps)
	}
	if pg.Steps[0].State != model.StateReady || pg.Steps[1].State != model.StateTodo {
		t.Fatalf("前两步状态不对：%s / %s", pg.Steps[0].State, pg.Steps[1].State)
	}

	// 第一条事实命中第 1 步的期望证据 → 第 1 步 done，第 2 步 ready（纯代码判定）。
	factID, err := s.AddFact(ctx, AddFactInput{
		RunID: run.ID, Summary: "已经拿到证据1", Evidence: "curl → 证据1", Confidence: "observed",
	})
	if err != nil {
		t.Fatal(err)
	}
	satisfied, err := s.SatisfyStepsByFact(ctx, run.ID, factID, "已经拿到证据1")
	if err != nil {
		t.Fatal(err)
	}
	if len(satisfied) != 1 || satisfied[0] != ids[0] {
		t.Fatalf("命中的步骤 = %v，期望 [%d]", satisfied, ids[0])
	}
	pg, err = s.PlanGraph(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pg.Steps[0].State != model.StateDone {
		t.Fatalf("第 1 步应为 done，实际 %s", pg.Steps[0].State)
	}
	if pg.Steps[1].State != model.StateReady {
		t.Fatalf("第 2 步应自动变 ready，实际 %s", pg.Steps[1].State)
	}
	if pg.Steps[2].State != model.StateTodo {
		t.Fatalf("第 3 步不该提前 ready，实际 %s", pg.Steps[2].State)
	}
}

// 显式判定：规则匹配不上时，planner 用 mark_step_done 也能完成一步。
func TestMarkStepDone(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	run := newTestRun(t, s)
	stepID, err := s.AddStep(ctx, AddStepInput{RunID: run.ID, Text: "人工判定的一步", ExpectedEvidence: "非常模糊的判据"})
	if err != nil {
		t.Fatal(err)
	}
	factID, err := s.AddFact(ctx, AddFactInput{
		RunID: run.ID, Summary: "看起来做完了", Evidence: "manual", Confidence: "inferred",
	})
	if err != nil {
		t.Fatal(err)
	}
	ok, err := s.MarkStepDone(ctx, run.ID, stepID, factID)
	if err != nil || !ok {
		t.Fatalf("mark 失败：ok=%v err=%v", ok, err)
	}
	pg, _ := s.PlanGraph(ctx, run.ID)
	if pg.Steps[0].State != model.StateDone {
		t.Fatalf("步骤状态 = %s，期望 done", pg.Steps[0].State)
	}
	// 拿意图当证据必须被拒。
	intentID, err := s.AddIntent(ctx, AddIntentInput{RunID: run.ID, Summary: "方向"})
	if err != nil {
		t.Fatal(err)
	}
	step2, err := s.AddStep(ctx, AddStepInput{RunID: run.ID, Text: "另一步", ExpectedEvidence: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkStepDone(ctx, run.ID, step2, intentID); err == nil {
		t.Fatal("意图不能作为步骤证据")
	}
}

// run #41 的回归：步骤派给意图 A 之后，意图 B 的黑盒探测事实不能把它判成 done。
//
// 真实事故：step#276 要求"展开 ROOT.war 并清点 WEB-INF"，被事实 #283 满足，
// 而 #283 的内容是"GET /WEB-INF/web.xml → 404" —— 同一个路径名出现在探测记录里。
func TestStepSatisfiedOnlyByItsOwnIntent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	run := newTestRun(t, s)

	stepID, err := s.AddStep(ctx, AddStepInput{
		RunID: run.ID, Text: "离线解包并清点 WEB-INF",
		ExpectedEvidence: "WEB-INF/web.xml；ROOT.war；Dockerfile",
	})
	if err != nil {
		t.Fatal(err)
	}
	owner, err := s.AddIntent(ctx, AddIntentInput{RunID: run.ID, Summary: "离线白盒：解包 ROOT.war"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.LinkStepIntent(ctx, run.ID, stepID, owner); err != nil {
		t.Fatal(err)
	}

	// 另一条意图（黑盒）的探测记录：同样的路径名，但结论是 404。
	other, err := s.AddIntent(ctx, AddIntentInput{RunID: run.ID, Summary: "黑盒探测非 /api 面"})
	if err != nil {
		t.Fatal(err)
	}
	probeText := "未知路径都落到 mvc-dispatcher；GET /WEB-INF/web.xml -> 404；GET /ROOT.war -> 404"
	factB, err := s.AddFact(ctx, AddFactInput{
		RunID: run.ID, IntentID: other, Summary: "未知路径一律 404（mvc-dispatcher 回显）",
		Evidence: probeText, Confidence: "observed",
	})
	if err != nil {
		t.Fatal(err)
	}
	satisfied, err := s.SatisfyStepsByFact(ctx, run.ID, factB, probeText)
	if err != nil {
		t.Fatal(err)
	}
	if len(satisfied) != 0 {
		t.Fatalf("旁证不该完成已派工的步骤，实际命中 %v", satisfied)
	}

	// 派出去的那条意图自己产出的事实才算。
	factA, err := s.AddFact(ctx, AddFactInput{
		RunID: run.ID, IntentID: owner, Summary: "ROOT.war 内含 WEB-INF/web.xml 与 Dockerfile",
		Evidence: "unzip -l ROOT.war → WEB-INF/web.xml；Dockerfile", Confidence: "observed",
	})
	if err != nil {
		t.Fatal(err)
	}
	satisfied, err = s.SatisfyStepsByFact(ctx, run.ID, factA, "ROOT.war 内含 WEB-INF/web.xml 与 Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	if len(satisfied) != 1 || satisfied[0] != stepID {
		t.Fatalf("本意图的事实应当完成步骤 %d，实际 %v", stepID, satisfied)
	}
}

// 覆盖度：只认 fact 的锚点，origin fact 不算"测过"。
func TestCoverageCountsOnlyFacts(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	run, err := s.CreateRun(ctx, CreateRunInput{
		Goal: "覆盖度测试", WorkerCount: 1,
		Scopes:       []model.Scope{{Kind: "root_domain", Domain: "example.test", Source: "manual"}},
		SeedEntities: []EntityInput{{Kind: "root_domain", Domain: "example.test", RootDomain: "example.test"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// 范围内第二个实体（子域，root_domain = example.test）。
	apiID, _, err := s.UpsertEntity(ctx, run.ID, EntityInput{
		Kind: "subdomain", Domain: "api.example.test", RootDomain: "example.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFact(ctx, AddFactInput{
		RunID: run.ID, Summary: "api 可访问", Evidence: "GET / → 200",
		Confidence: "observed", AssetIDs: []int64{apiID},
	}); err != nil {
		t.Fatal(err)
	}

	cov, err := s.Coverage(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cov.Total != 2 {
		t.Fatalf("范围内实体数 = %d，期望 2：%+v", cov.Total, cov.ByKind)
	}
	if cov.Tested != 1 {
		t.Fatalf("已测 = %d，期望 1（只有 fact 锚点算；origin 不算）", cov.Tested)
	}
	if cov.Pct < 49 || cov.Pct > 51 {
		t.Fatalf("覆盖率 = %.1f，期望 50", cov.Pct)
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}
