package agent_test

import (
	"context"
	"testing"

	"qinyuan/internal/agent"
	"qinyuan/internal/graphstore/memstore"
	"qinyuan/internal/model"
	"qinyuan/internal/store"
)

// P2-4 的验收：planner 的态势组装不依赖 PostgreSQL —— 在 memstore 上就能跑，
// 并且计划链的 ready 判定仍然正确（这是 planner"本轮只能从 ready 里选"的依据）。
func TestBuildOverviewOnMemstore(t *testing.T) {
	ctx := context.Background()
	ms := memstore.New()
	ms.AddRun(&model.Run{
		ID: 7, Goal: "打穿 example.com 的登录入口",
		Task: &model.Task{RunID: 7, Status: model.TaskRunning, Paused: false},
	}, []model.Scope{{Kind: "root_domain", Domain: "example.com", Source: "manual"}})

	// 三个目标：一个 open，一个 met（应体现在 status/goals 里）。
	ms.AddNode(7, &model.Node{Kind: model.KindGoal, State: model.StateOpen, Payload: []byte(`{"text":"拿到后台入口"}`)})
	ms.AddNode(7, &model.Node{Kind: model.KindGoal, State: model.StateMet, Payload: []byte(`{"text":"确认指纹"}`)})
	// 两条意图：一条 open（frontier）、一条 done。
	openID := ms.AddNode(7, &model.Node{Kind: model.KindIntent, State: model.StateOpen, Payload: []byte(`{"summary":"试 /admin 路径"}`)})
	ms.AddNode(7, &model.Node{Kind: model.KindIntent, State: model.StateDone, Payload: []byte(`{"summary":"抓首页"}`)})
	// 一条事实 + yields 边（让 recent_facts 能带上 from_intent）。
	factID := ms.AddNode(7, &model.Node{Kind: model.KindFact, State: model.StateConfirmed,
		Payload: []byte(`{"summary":"首页是 nginx","confidence":"observed"}`)})
	ms.AddEdge(7, model.Edge{SrcID: openID, Rel: model.RelYields, DstID: factID})
	ms.AddEntity(7, &model.Entity{ID: 1, Kind: "subdomain", Domain: "api.example.com"})

	// 计划图：3 步，只有第 2 步 ready。
	ms.SetPlan(7, &store.PlanGraph{
		PlanID: 11, Title: "外网打点", Strategy: "先指纹后入口", Revision: 2,
		Steps: []store.StepView{
			{ID: 21, Text: "指纹", Expected: "server 头", State: model.StateDone},
			{ID: 22, Text: "找登录入口", Expected: "登录表单", State: model.StateReady, Priority: 5},
			{ID: 23, Text: "试默认口令", Expected: "登录成功", State: model.StateTodo},
		},
	})
	ms.SetCoverage(7, &store.Coverage{Total: 10, Tested: 4, Pct: 40})
	ms.SetCampaignView(7, &store.CampaignView{ID: 3, Name: "客户 A", Status: "active", Runs: 2, Duplicates: 1})

	ov, err := agent.BuildOverview(ctx, ms, 7, true)
	if err != nil {
		t.Fatalf("在 memstore 上组装态势失败：%v", err)
	}

	if ov.Status != model.TaskRunning {
		t.Fatalf("状态 = %q，期望 running（有 open 意图）", ov.Status)
	}
	if len(ov.Goals) != 2 {
		t.Fatalf("目标数 = %d，期望 2", len(ov.Goals))
	}
	if ov.Frontier != 1 || ov.DoneTotal != 1 {
		t.Fatalf("意图计数不对：frontier=%d done=%d", ov.Frontier, ov.DoneTotal)
	}
	if ov.FactsTotal != 1 || len(ov.Facts) != 1 || ov.Facts[0].FromIntent != openID {
		t.Fatalf("事实与血缘不对：total=%d facts=%+v", ov.FactsTotal, ov.Facts)
	}
	if ov.Plan == nil {
		t.Fatal("计划图摘要缺失")
	}
	if ov.Plan.StepsTotal != 3 || ov.Plan.StepsDone != 1 {
		t.Fatalf("计划计数不对：%+v", ov.Plan)
	}
	if len(ov.Plan.Ready) != 1 || ov.Plan.Ready[0].ID != 22 {
		t.Fatalf("ready 步骤应只有 #22：%+v", ov.Plan.Ready)
	}
	if ov.Coverage == nil || ov.Coverage.Pct != 40 {
		t.Fatalf("覆盖度没带上：%+v", ov.Coverage)
	}
	if ov.Campaign == nil || ov.Campaign.ID != 3 {
		t.Fatalf("战役视图没带上：%+v", ov.Campaign)
	}
	if ov.Entities != 1 {
		t.Fatalf("实体数 = %d，期望 1", ov.Entities)
	}
}
