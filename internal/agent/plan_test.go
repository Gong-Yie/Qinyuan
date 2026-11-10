package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"qinyuan/internal/model"
	"qinyuan/internal/store"
)

// 计划图 + 自动满足 + add_intent 强制挂步骤（P1-1 的 S3 语义）。
func TestPlanStepAutoSatisfyAndIntentGate(t *testing.T) {
	rc, _ := testContext(t, []model.Scope{{Kind: "root_domain", Domain: "example.test", Source: "manual"}},
		[]store.EntityInput{{Kind: "root_domain", Domain: "example.test", RootDomain: "example.test"}}, true, 0)
	ctx := context.Background()

	addStep := NewAddPlanStepTool()
	res, err := addStep.Handle(ctx, rc, json.RawMessage(
		`{"text":"拿到首页标题","expected_evidence":"首页标题"}`))
	if err != nil || res.IsError {
		t.Fatalf("创建第一步失败：%v %s", err, res.Text)
	}
	var s1 struct {
		StepID int64 `json:"step_id"`
	}
	if err := json.Unmarshal([]byte(res.Text), &s1); err != nil {
		t.Fatalf("返回不是 JSON：%s", res.Text)
	}

	res, err = addStep.Handle(ctx, rc, json.RawMessage(
		`{"text":"枚举登录接口","expected_evidence":"登录接口","depends_on":[`+itoa64(s1.StepID)+`]}`))
	if err != nil || res.IsError {
		t.Fatalf("创建第二步失败：%v %s", err, res.Text)
	}
	var s2 struct {
		StepID int64 `json:"step_id"`
	}
	if err := json.Unmarshal([]byte(res.Text), &s2); err != nil {
		t.Fatalf("返回不是 JSON：%s", res.Text)
	}

	// 事实命中第一步的期望证据 → 自动完成第一步
	factRes, err := NewRecordFactTool().Handle(ctx, rc, json.RawMessage(
		`{"facts":[{"summary":"已拿到首页标题：Qinyuan Demo","evidence":"GET / → <title>Qinyuan Demo</title>","confidence":"observed"}]}`))
	if err != nil || factRes.IsError {
		t.Fatalf("写事实失败：%v %s", err, factRes.Text)
	}
	if !strings.Contains(factRes.Text, `"satisfied_steps":[`+itoa64(s1.StepID)+`]`) {
		t.Fatalf("事实应自动满足第一步：%s", factRes.Text)
	}
	pg, err := rc.Store.PlanGraph(ctx, rc.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pg.Steps) != 2 {
		t.Fatalf("步骤数 = %d", len(pg.Steps))
	}
	var first, second store.StepView
	for _, st := range pg.Steps {
		switch st.ID {
		case s1.StepID:
			first = st
		case s2.StepID:
			second = st
		}
	}
	if first.State != model.StateDone {
		t.Fatalf("第一步 = %s，期望 done", first.State)
	}
	if second.State != model.StateReady {
		t.Fatalf("第二步 = %s，期望 ready（依赖已完成）", second.State)
	}

	// 有计划图时，不挂步骤也不声明 off_plan：必须被拒
	addIntent := NewAddIntentTool()
	res, err = addIntent.Handle(ctx, rc, json.RawMessage(`{"intents":[{"summary":"计划外但又没说"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "step_id") {
		t.Fatalf("应提示必须挂步骤或 off_plan：%s", res.Text)
	}

	// 挂到第二步：成功，并且步骤变 running
	res, err = addIntent.Handle(ctx, rc, json.RawMessage(
		`{"intents":[{"summary":"枚举 /login 接口","step_id":`+itoa64(s2.StepID)+`}]}`))
	if err != nil || strings.Contains(res.Text, "error") {
		t.Fatalf("挂步骤失败：%v %s", err, res.Text)
	}
	pg, _ = rc.Store.PlanGraph(ctx, rc.RunID)
	for _, st := range pg.Steps {
		if st.ID == s2.StepID {
			if st.State != model.StateRunning {
				t.Fatalf("第二步 = %s，期望 running", st.State)
			}
			if st.RealizedBy == 0 {
				t.Fatal("缺少 realized_by 边")
			}
		}
	}

	// off_plan=true 是逃生门：允许计划外探索
	res, err = addIntent.Handle(ctx, rc, json.RawMessage(
		`{"intents":[{"summary":"计划外：发现一个可疑子域","off_plan":true}]}`))
	if err != nil || strings.Contains(res.Text, "error") {
		t.Fatalf("off_plan 应该放行：%v %s", err, res.Text)
	}
}

func itoa64(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}
