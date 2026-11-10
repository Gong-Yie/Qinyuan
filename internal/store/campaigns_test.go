package store

import (
	"context"
	"testing"
)

// P3-1 的核心验收：战役视图要同时给出总覆盖度、进度、重复投入，且注入的
// "别的子 run 已测过"提示只指向本战役内的兄弟任务。
func TestCampaignAggregatesAndDedupHint(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	camp, err := s.CreateCampaign(ctx, "客户 A 外网", "把 A 公司外网资产测完", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddCampaignScope(ctx, camp.ID, CampaignScope{Kind: "root_domain", Domain: "example.com"}); err != nil {
		t.Fatal(err)
	}

	run1 := newTestRun(t, s)
	run2 := newTestRun(t, s)
	other := newTestRun(t, s) // 不在战役里：它的 fact 不能算进来
	for _, r := range []struct {
		id   int64
		role string
	}{{run1.ID, "外网"}, {run2.ID, "内网横向"}} {
		if err := s.AttachRun(ctx, r.id, camp.ID, r.role); err != nil {
			t.Fatal(err)
		}
	}

	upsert := func(runID int64, domain string) int64 {
		t.Helper()
		id, _, err := s.UpsertEntity(ctx, runID, EntityInput{
			Kind: "subdomain", Domain: domain, RootDomain: "example.com",
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	a1 := upsert(run1.ID, "a.example.com")
	b2 := upsert(run2.ID, "b.example.com")
	outside := upsert(other.ID, "c.example.com")

	addFact := func(runID, assetID int64, summary string) int64 {
		t.Helper()
		id, err := s.AddFact(ctx, AddFactInput{
			RunID: runID, Summary: summary, Evidence: "GET / → 200", AssetIDs: []int64{assetID},
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	addFact(run1.ID, a1, "a 的首页可访问")
	factFromRun2 := addFact(run2.ID, a1, "a 的登录页存在（重复测了）")
	addFact(run2.ID, b2, "b 的指纹是 nginx")
	addFact(other.ID, outside, "c 不在战役内")

	// 覆盖度：战役范围（*.example.com）里共 3 个实体，其中 2 个被战役内 fact 锚定。
	sum, err := s.CampaignSummary(ctx, camp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Coverage.Total != 3 {
		t.Fatalf("范围实体数 = %d，期望 3（a/b/c 都在 *.example.com 下）", sum.Coverage.Total)
	}
	if sum.Coverage.Tested != 2 {
		t.Fatalf("已测实体数 = %d，期望 2（c 的 fact 不在战役内，不算）", sum.Coverage.Tested)
	}
	if len(sum.Runs) != 2 {
		t.Fatalf("子 run 数 = %d，期望 2", len(sum.Runs))
	}
	if len(sum.Duplicates) != 1 || sum.Duplicates[0].EntityID != a1 {
		t.Fatalf("重复投入应只有实体 #%d：%+v", a1, sum.Duplicates)
	}
	if len(sum.Duplicates[0].Runs) != 2 {
		t.Fatalf("重复实体的 run 列表不对：%+v", sum.Duplicates[0])
	}

	// 注入给 planner 的战役视图：提示里要有"另一个 run 测过 a"。
	view, err := s.CampaignViewForRun(ctx, run1.ID)
	if err != nil || view == nil {
		t.Fatalf("run1 属于战役，应当有视图：%v", err)
	}
	if view.Duplicates != 1 || view.CoveragePct <= 0 {
		t.Fatalf("视图聚合不对：%+v", view)
	}
	found := false
	for _, h := range view.TestedElsewhere {
		if h.EntityID == a1 && h.RunID == run2.ID && h.FactID == factFromRun2 {
			found = true
		}
		if h.RunID == other.ID {
			t.Fatalf("不该提示战役外的 run：%+v", h)
		}
	}
	if !found {
		t.Fatalf("缺少「a 已被 run2 测过」的提示：%+v", view.TestedElsewhere)
	}

	// 不在战役里的 run：零注入。
	if v, err := s.CampaignViewForRun(ctx, other.ID); err != nil || v != nil {
		t.Fatalf("独立任务不该有战役视图：%+v err=%v", v, err)
	}
}

// 战役状态与子 run 解绑语义。
func TestCampaignStatusAndDetach(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	camp, err := s.CreateCampaign(ctx, "临时战役", "测试", nil)
	if err != nil {
		t.Fatal(err)
	}
	run := newTestRun(t, s)
	if err := s.AttachRun(ctx, run.ID, camp.ID, "外网"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Run(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CampaignID != camp.ID || got.CampaignRole != "外网" {
		t.Fatalf("挂载没写进 run：campaign=%d role=%q", got.CampaignID, got.CampaignRole)
	}
	if ok, err := s.SetCampaignStatus(ctx, camp.ID, "paused"); err != nil || !ok {
		t.Fatalf("改状态失败：%v", err)
	}
	if _, err := s.SetCampaignStatus(ctx, camp.ID, "bogus"); err == nil {
		t.Fatal("非法状态必须被拒")
	}
	// 解绑：campaign_id=0 → 置空（不删 run）。
	if err := s.AttachRun(ctx, run.ID, 0, ""); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Run(ctx, run.ID)
	if got.CampaignID != 0 {
		t.Fatalf("解绑后 campaign_id 应为 0，实际 %d", got.CampaignID)
	}
	if _, err := s.Run(ctx, run.ID); err != nil {
		t.Fatalf("解绑不该删 run：%v", err)
	}
}

// 视图里 run 的覆盖度仍按单 run 口径（只认 fact 锚点）。
func TestCampaignRunCoverage(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	camp, _ := s.CreateCampaign(ctx, "c", "g", nil)
	run := newTestRun(t, s)
	if err := s.AttachRun(ctx, run.ID, camp.ID, ""); err != nil {
		t.Fatal(err)
	}
	runs, err := s.CampaignRuns(ctx, camp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].RunID != run.ID {
		t.Fatalf("子 run 列表不对：%+v", runs)
	}
	if runs[0].Status == "" {
		t.Fatalf("子 run 应带任务状态：%+v", runs[0])
	}
}
