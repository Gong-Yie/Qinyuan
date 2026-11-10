package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"qinyuan/internal/agent"
	"qinyuan/internal/llm"
	"qinyuan/internal/model"
	"qinyuan/internal/store"
)

// 本文件是 M3.5 第一半（第 0 轮目标拆解）的验收。
//
// 三条边界：
//  1. 只跑一次（goals_at 是 CAS，重复调用不再烧模型）；
//  2. 拆解结果替换 seed 目标（seed 目标置 abandoned，血缘保留）；
//  3. 拆解失败不阻塞任务：seed 目标兜底，下一轮重试。

// keyFactory 把会话键路由到不同脚本（goals / planner / worker 各一份）。
type keyFactory struct {
	goals   llm.Factory
	planner llm.Factory
	worker  llm.Factory
}

func (k keyFactory) For(key string) llm.Provider {
	switch {
	case strings.Contains(key, "goals"):
		return k.goals.For(key)
	case strings.Contains(key, "planner"):
		return k.planner.For(key)
	default:
		return k.worker.For(key)
	}
}

func newRunForGoals(t *testing.T, st *store.Store) *model.Run {
	t.Helper()
	run, err := st.CreateRun(context.Background(), store.CreateRunInput{
		Goal: "确认示例站点的登录入口，并证明存在弱口令", WorkerCount: 1,
	})
	if err != nil {
		t.Fatalf("建 run 失败：%v", err)
	}
	return run
}

func TestEnsureGoalsDecomposesOnce(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	run := newRunForGoals(t, st)

	goalsFactory := llm.NewFakeFactory(
		llm.Step{ToolCalls: []llm.ToolCall{llm.NewToolCall("g1", "add_scope", map[string]any{
			"entries": []map[string]any{{"kind": "subdomain", "value": "portal.example.com", "reason": "登录入口"}},
		})}},
		llm.Step{ToolCalls: []llm.ToolCall{llm.NewToolCall("g2", "set_constraints", map[string]any{
			"items": []map[string]any{{"type": "deny", "text": "禁止对 portal.example.com 做口令爆破"}},
		})}},
		llm.Step{ToolCalls: []llm.ToolCall{llm.NewToolCall("g3", "set_goals", map[string]any{
			"goals": []map[string]any{
				{"text": "确认 portal.example.com 的登录入口存在且可访问"},
				{"text": "证明登录接口存在弱口令（至少一组可登录凭据）", "vulnclass": "weak-password"},
			},
		})}},
		llm.Step{Text: "拆出 2 条目标、1 条约束、1 条范围"},
	)
	eng := New(st, keyFactory{goals: goalsFactory, planner: llm.NewFakeFactory(), worker: llm.NewFakeFactory()},
		agent.NewWorkerRegistry(), agent.NewPlannerRegistry(), Config{
			DataDir: t.TempDir(), IdleSleep: 10 * time.Millisecond, Debounce: 5 * time.Millisecond,
		}, nil)

	// 第一次：真的拆解。
	r1, _ := st.Run(ctx, run.ID)
	if !eng.ensureGoals(ctx, run.ID, r1) {
		t.Fatalf("ensureGoals 返回 false（循环被意外终止）")
	}

	goals, err := st.Goals(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var open, abandoned, fromGoals int
	for _, g := range goals {
		switch {
		case g.State == model.StateOpen:
			open++
		case g.State == model.StateAbandoned:
			abandoned++
		}
		if g.Origin == "goals" {
			fromGoals++
		}
	}
	if fromGoals != 2 {
		t.Fatalf("拆解后应有 2 条 goals 目标，实际 %d（全部：%+v）", fromGoals, goals)
	}
	if open != 2 {
		t.Fatalf("应有 2 条 open 目标，实际 %d", open)
	}
	if abandoned != 1 {
		t.Fatalf("seed 目标应被置 abandoned（保留血缘），实际 abandoned=%d", abandoned)
	}
	if at, _ := st.GoalsDecomposedAt(ctx, run.ID); at == 0 {
		t.Fatalf("goals_at 没有被写入")
	}
	if cs, _ := st.Constraints(ctx, run.ID); len(cs) != 1 || cs[0].Kind != "deny" {
		t.Fatalf("约束没写进去：%+v", cs)
	}
	if ss, _ := st.Scopes(ctx, run.ID); len(ss) != 1 || ss[0].Domain != "portal.example.com" {
		t.Fatalf("范围没写进去：%+v", ss)
	}

	// 第二次：goals_at 已写，必须短路（不再调模型）。
	r2, _ := st.Run(ctx, run.ID)
	if r2.Task.GoalsAt == 0 {
		t.Fatalf("第二次读到的 goals_at 仍是 0")
	}
	if !eng.ensureGoals(ctx, run.ID, r2) {
		t.Fatalf("第二次 ensureGoals 返回 false")
	}
	if n := goalsFactory.CallCount(sessionKey(run.ID, "goals")); n != 4 {
		// 脚本 4 步：add_scope / set_constraints / set_goals / 收尾文本
		t.Fatalf("第二次调用不该再打模型：goals 会话调用了 %d 次（期望 4）", n)
	}
}

// sessionKey 拼出与生产一致的会话键（run<id>-goals / run<id>-planner）。
func sessionKey(runID int64, role string) string {
	return fmt.Sprintf("run%d-%s", runID, role)
}

// TestEnsureGoalsFailureKeepsSeedGoal 拆解失败：不写 goals_at，seed 目标继续兜底。
func TestEnsureGoalsFailureKeepsSeedGoal(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	run := newRunForGoals(t, st)

	failing := &llm.DynamicFactory{Fn: func(context.Context, *llm.Request) (*llm.Response, error) {
		return nil, llm.ErrTransient
	}}
	eng := New(st, keyFactory{goals: failing, planner: llm.NewFakeFactory(), worker: llm.NewFakeFactory()},
		agent.NewWorkerRegistry(), agent.NewPlannerRegistry(), Config{
			DataDir: t.TempDir(), IdleSleep: 10 * time.Millisecond, Debounce: 5 * time.Millisecond,
		}, nil)

	r, _ := st.Run(ctx, run.ID)
	if !eng.ensureGoals(ctx, run.ID, r) {
		t.Fatalf("拆解失败不应该终止循环（任务要继续跑）")
	}
	if at, _ := st.GoalsDecomposedAt(ctx, run.ID); at != 0 {
		t.Fatalf("失败时不该写 goals_at（否则不会重试），实际 %d", at)
	}
	open, err := st.HasOpenGoal(ctx, run.ID)
	if err != nil || !open {
		t.Fatalf("seed 目标必须仍然 open（兜底），open=%v err=%v", open, err)
	}
	// 失败要留下可观测的痕迹（否则只能靠猜）。
	acts, err := st.Activities(ctx, run.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range acts {
		if a.Kind == model.ActSystem && strings.Contains(a.Summary, "目标拆解失败") {
			found = true
		}
	}
	if !found {
		t.Fatalf("失败时应当写一条 system activity，实际：%+v", acts)
	}
}

// TestEngineGoalsRoundBeforePlanner 端到端：引擎启动后先拆解，再让 planner 基于新目标规划。
func TestEngineGoalsRoundBeforePlanner(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	run := newRunForGoals(t, st)

	goalsFactory := llm.NewFakeFactory(
		llm.Step{ToolCalls: []llm.ToolCall{llm.NewToolCall("g1", "set_goals", map[string]any{
			"goals": []map[string]any{{"text": "确认登录入口存在"}},
		})}},
		llm.Step{Text: "拆解完成"},
	)
	plannerFactory := llm.NewFakeFactory(llm.Step{Text: "先观察一轮"})

	eng := New(st, keyFactory{goals: goalsFactory, planner: plannerFactory, worker: llm.NewFakeFactory()},
		agent.NewWorkerRegistry(), agent.NewPlannerRegistry(), Config{
			DataDir: t.TempDir(), IdleSleep: 10 * time.Millisecond, Debounce: 5 * time.Millisecond,
		}, nil)
	if err := eng.Run(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	defer eng.StopTask(run.ID, ErrShutdown)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if at, _ := st.GoalsDecomposedAt(ctx, run.ID); at > 0 {
			// 拆解完成后，planner 至少被唤醒过一次。
			for time.Now().Before(deadline) && plannerFactory.CallCount(sessionKey(run.ID, "planner")) == 0 {
				time.Sleep(20 * time.Millisecond)
			}
			if plannerFactory.CallCount(sessionKey(run.ID, "planner")) == 0 {
				t.Fatalf("拆解完成后 planner 没有被唤醒")
			}
			goals, _ := st.Goals(ctx, run.ID)
			for _, g := range goals {
				if g.Origin == "goals" && g.PayloadField("text") == "确认登录入口存在" {
					return
				}
			}
			t.Fatalf("拆解出的目标没有出现在图里：%+v", goals)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等待目标拆解超时")
}
