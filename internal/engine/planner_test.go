package engine

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"qinyuan/internal/agent"
	"qinyuan/internal/llm"
	"qinyuan/internal/model"
	"qinyuan/internal/store"
)

// routeFactory 把 planner 与 worker 的模型调用分开：planner 用动态逻辑，worker 用脚本。
type routeFactory struct {
	planner llm.Factory
	worker  llm.Factory
}

func (r routeFactory) For(key string) llm.Provider {
	if strings.Contains(key, "planner") {
		return r.planner.For(key)
	}
	return r.worker.For(key)
}

// overviewFrom 从 planner 的 user 消息里把预取的态势 JSON 解出来。
func overviewFrom(req *llm.Request) (*agent.Overview, bool) {
	for _, m := range req.Messages {
		i := strings.Index(m.Text, "【本轮态势")
		if i < 0 {
			continue
		}
		j := strings.Index(m.Text[i:], "{")
		if j < 0 {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(m.Text[i+j:]))
		var ov agent.Overview
		if err := dec.Decode(&ov); err == nil {
			return &ov, true
		}
	}
	return nil, false
}

func waitTaskStatus(t *testing.T, st *store.Store, runID int64, want []string, within time.Duration) *model.Run {
	t.Helper()
	deadline := time.Now().Add(within)
	var last *model.Run
	for time.Now().Before(deadline) {
		r, err := st.Run(context.Background(), runID)
		if err != nil {
			t.Fatalf("读任务失败：%v", err)
		}
		last = r
		if r.Task != nil {
			for _, w := range want {
				if r.Task.Status == w {
					return r
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	status := "nil"
	if last != nil && last.Task != nil {
		status = last.Task.Status
	}
	t.Fatalf("等待任务状态 %v 超时（当前 %s）", want, status)
	return nil
}

func waitIntentState(t *testing.T, st *store.Store, runID, intentID int64, want string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		n, err := st.Intent(context.Background(), runID, intentID)
		if err == nil && n.State == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	n, _ := st.Intent(context.Background(), runID, intentID)
	state := "nil"
	if n != nil {
		state = n.State
	}
	t.Fatalf("等待意图 %d 到 %s 超时（当前 %s）", intentID, want, state)
}

// 端到端：planner 派意图 → worker 写事实 → planner 看到事实后 prove_goal → 任务 done。
func TestPlannerWorkerGoalLoop(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	run, _ := newLoopbackRun(t, st, "http://127.0.0.1:1")

	var mu sync.Mutex
	calls := map[string]int{}
	plannerFn := func(_ context.Context, req *llm.Request) (*llm.Response, error) {
		mu.Lock()
		n := calls[req.SessionKey]
		calls[req.SessionKey]++
		mu.Unlock()

		if n == 0 {
			return &llm.Response{
				ToolCalls: []llm.ToolCall{llm.NewToolCall("p1", "add_intent", map[string]any{
					"intents": []map[string]any{{
						"summary": "请求首页，确认服务指纹与登录入口", "priority": 6,
					}},
				})},
				Usage: llm.Usage{Input: 200, Output: 40},
			}, nil
		}

		ov, ok := overviewFrom(req)
		if !ok {
			return &llm.Response{Text: "（测试）读不到态势"}, nil
		}
		var openGoalID int64
		for _, g := range ov.Goals {
			if g.State == model.StateOpen {
				openGoalID = g.ID
				break
			}
		}
		if openGoalID == 0 {
			return &llm.Response{Text: "目标已达成。"}, nil
		}
		if len(ov.Facts) == 0 {
			return &llm.Response{Text: "等 worker 产出事实。"}, nil
		}
		// 最新的 fact 排在前面（id DESC），用它作证据。
		return &llm.Response{
			ToolCalls: []llm.ToolCall{llm.NewToolCall("p2", "prove_goal", map[string]any{
				"goal_id": openGoalID, "evidence_id": ov.Facts[0].ID,
				"reason": "首页已可访问且有服务指纹，目标达成",
			})},
			Usage: llm.Usage{Input: 300, Output: 30},
		}, nil
	}

	workerFake := llm.NewFakeFactory(
		llm.Step{ToolCalls: []llm.ToolCall{llm.NewToolCall("w1", "record_fact", map[string]any{
			"facts": []map[string]any{{
				"summary": "首页可访问，服务头为 qinyuan-test/1.0", "evidence": "GET / → 200 Server: qinyuan-test/1.0",
				"confidence": "observed",
			}},
		})}, Usage: llm.Usage{Input: 100, Output: 20}},
		llm.Step{Text: "完成：已写回一条事实。"},
	)

	eng := New(st, routeFactory{planner: &llm.DynamicFactory{Fn: plannerFn}, worker: workerFake},
		agent.NewWorkerRegistry(), agent.NewPlannerRegistry(), Config{
			DataDir: t.TempDir(), AllowLoopback: true,
			MaxTurns: 5, MaxDuration: 30 * time.Second, IdleSleep: 10 * time.Millisecond,
			Debounce: 5 * time.Millisecond, HeartbeatFloor: 30 * time.Second,
		}, nil)

	if err := eng.Run(ctx, run.ID); err != nil {
		t.Fatalf("启动失败：%v", err)
	}
	defer eng.StopTask(run.ID, ErrShutdown)

	waitTaskStatus(t, st, run.ID, []string{model.TaskDone}, 15*time.Second)

	goals, err := st.Goals(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(goals) != 1 || goals[0].State != model.StateMet {
		t.Fatalf("目标应已达成：%+v", goals)
	}
	intents, _ := st.NodesByKind(ctx, run.ID, model.KindIntent, 10)
	if len(intents) != 1 || intents[0].State != model.StateDone {
		t.Fatalf("意图应只有一条且 done：len=%d state=%s summary=%q", len(intents), intents[0].State, intents[0].Summary())
	}
	facts, _ := st.NodesByKind(ctx, run.ID, model.KindFact, 10)
	confirmed := 0
	for _, f := range facts {
		if f.State == model.StateConfirmed {
			confirmed++
		}
	}
	if confirmed != 1 {
		t.Fatalf("confirmed 事实数 = %d，期望 1", confirmed)
	}

	// planner 的每次工具调用都要有配对结果，且能读出"派意图 + 判目标"两类。
	acts, _ := st.ActivityTail(ctx, run.ID, 300)
	paired := map[string]bool{}
	tools := map[string]bool{}
	for _, a := range acts {
		if a.Kind == model.ActToolResult {
			paired[a.ToolUseID] = true
			tools[a.Tool] = true
		}
	}
	for _, tuid := range []string{"p1", "p2", "w1"} {
		if !paired[tuid] {
			t.Fatalf("tool_use %s 没有配对结果", tuid)
		}
	}
	if !tools["add_intent"] || !tools["prove_goal"] {
		t.Fatalf("planner 的工具调用不完整：%v", tools)
	}
}

// 没有 open goal 且没有在跑的意图：任务应直接 done，且不再调模型。
func TestGoallessBranchMarksDoneWithoutLLM(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	run, _ := newLoopbackRun(t, st, "http://127.0.0.1:1")

	goals, err := st.Goals(ctx, run.ID)
	if err != nil || len(goals) != 1 {
		t.Fatalf("目标缺失：%v", err)
	}
	// origin fact 不能当证据（它是任务起点），所以先真的写一条事实出来。
	factID, err := st.AddFact(ctx, store.AddFactInput{
		RunID: run.ID, Summary: "测试事实：目标已达成", Evidence: "test evidence", Confidence: "observed",
	})
	if err != nil {
		t.Fatalf("写事实失败：%v", err)
	}
	if _, err := st.ProveGoal(ctx, run.ID, goals[0].ID, factID, "测试直接达成"); err != nil {
		t.Fatalf("prove 失败：%v", err)
	}

	var calls int
	counting := llm.Factory(countingFactory{fn: func() { calls++ }})
	eng := New(st, counting, agent.NewWorkerRegistry(), agent.NewPlannerRegistry(), Config{
		DataDir: t.TempDir(), IdleSleep: 10 * time.Millisecond,
		Debounce: 5 * time.Millisecond, HeartbeatFloor: 30 * time.Second,
	}, nil)
	if err := eng.Run(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	waitTaskStatus(t, st, run.ID, []string{model.TaskDone}, 5*time.Second)
	if calls != 0 {
		t.Fatalf("goalless 分支不该调模型，实际调用 %d 次", calls)
	}
}

type countingFactory struct{ fn func() }

func (c countingFactory) For(string) llm.Provider { return countingProvider{c.fn} }

type countingProvider struct{ fn func() }

func (c countingProvider) Complete(context.Context, *llm.Request) (*llm.Response, error) {
	c.fn()
	return &llm.Response{Text: "（不应被调用）"}, nil
}

// 启动恢复：running 意图打回 open，并重建循环继续推进。
func TestRestoreResetsRunningAndContinues(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	run, _ := newLoopbackRun(t, st, "http://127.0.0.1:1")
	intentID, err := st.AddIntent(ctx, store.AddIntentInput{RunID: run.ID, Summary: "恢复测试"})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := st.ClaimIntent(ctx, run.ID, intentID, "dead-worker"); err != nil || !ok {
		t.Fatalf("模拟领取失败：%v", err)
	}

	workerFake := llm.NewFakeFactory(llm.Step{Text: "恢复后跑完了这条意图。"})
	eng := New(st, workerFake, agent.NewWorkerRegistry(), agent.NewPlannerRegistry(), Config{
		DataDir: t.TempDir(), IdleSleep: 10 * time.Millisecond,
		Debounce: 5 * time.Millisecond, HeartbeatFloor: 30 * time.Second,
	}, nil)
	reset, err := eng.Restore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if reset < 1 {
		t.Fatalf("应至少重置 1 条 running 意图，实际 %d", reset)
	}
	defer eng.StopTask(run.ID, ErrShutdown)
	waitIntentState(t, st, run.ID, intentID, model.StateDone, 10*time.Second)
}

// 任务级超时：到点收尾并落 timeout，不把在跑的意图误标成 done。
func TestDeadlineSettlesTimeout(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	run, err := st.CreateRun(ctx, store.CreateRunInput{
		Goal: "超时收尾测试", WorkerCount: 1, TimeoutSeconds: 2,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 模型永远在"思考"，不发工具：让收尾时确有在跑的东西。
	slow := llm.NewFakeFactory(llm.Step{Delay: 3 * time.Second, Text: "太慢了"})
	eng := New(st, slow, agent.NewWorkerRegistry(), agent.NewPlannerRegistry(), Config{
		DataDir: t.TempDir(), IdleSleep: 10 * time.Millisecond,
		Debounce: 5 * time.Millisecond, HeartbeatFloor: 30 * time.Second,
		SettleDrainGrace: 500 * time.Millisecond, FinalPlanTimeout: 2 * time.Second,
	}, nil)
	if err := eng.Run(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	defer eng.StopTask(run.ID, ErrShutdown)

	got := waitTaskStatus(t, st, run.ID, []string{model.TaskTimeout}, 20*time.Second)
	if got.Task.Status != model.TaskTimeout {
		t.Fatalf("状态 = %s，期望 timeout", got.Task.Status)
	}
}
