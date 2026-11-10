package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"qinyuan/internal/agent"
	"qinyuan/internal/llm"
	"qinyuan/internal/model"
	"qinyuan/internal/store"
)

// 本文件是 M4a 的调度侧验收：
//   - 撞预算不白跑：收尾轮把结果写回，但终态仍是 exhausted；
//   - 收尾轮隐藏"继续探测"的工具；
//   - steer_work 在下一次工具调用前生效，并能拦下那次调用。

func toolNames(req *llm.Request) []string {
	out := make([]string, 0, len(req.Tools))
	for _, t := range req.Tools {
		out = append(out, t.Name)
	}
	return out
}

func hasTool(req *llm.Request, name string) bool {
	for _, t := range req.Tools {
		if t.Name == name {
			return true
		}
	}
	return false
}

func countConfirmedFacts(t *testing.T, st *store.Store, runID int64) int {
	t.Helper()
	nodes, err := st.NodesByKind(context.Background(), runID, model.KindFact, 100)
	if err != nil {
		t.Fatalf("读事实失败：%v", err)
	}
	n := 0
	for _, x := range nodes {
		if x.State == model.StateConfirmed {
			n++
		}
	}
	return n
}

// TestSettlementWritesBackOnMaxTurns：MaxTurns=1，主循环只够写一条事实；
// 收尾轮再写一条 —— 这正是"撞上限 ≠ 白跑"。
func TestSettlementWritesBackOnMaxTurns(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	run, _ := newLoopbackRun(t, st, "http://127.0.0.1:1")

	intentID, err := st.AddIntent(ctx, store.AddIntentInput{
		RunID: run.ID, Summary: "请求首页并写回指纹与登录入口",
	})
	if err != nil {
		t.Fatal(err)
	}

	var toolsPerCall [][]string
	var calls int
	workerFn := func(_ context.Context, req *llm.Request) (*llm.Response, error) {
		toolsPerCall = append(toolsPerCall, toolNames(req))
		n := calls
		calls++
		switch n {
		case 0:
			return &llm.Response{ToolCalls: []llm.ToolCall{llm.NewToolCall("w1", "record_fact", map[string]any{
				"facts": []map[string]any{{
					"summary": "主循环写回：首页 200", "evidence": "curl -sI / → 200", "confidence": "observed",
				}},
				"intent_id": intentID,
			})}}, nil
		case 1:
			return &llm.Response{ToolCalls: []llm.ToolCall{llm.NewToolCall("w2", "record_fact", map[string]any{
				"facts": []map[string]any{{
					"summary": "收尾轮写回：登录表单存在", "evidence": "GET /login → <form>", "confidence": "observed",
				}},
				"intent_id": intentID,
			})}}, nil
		default:
			return &llm.Response{Text: "收尾完成：已写回两条事实"}, nil
		}
	}

	eng := New(st, routeFactory{
		planner: llm.NewFakeFactory(llm.Step{Text: "等 worker 产出"}),
		worker:  &llm.DynamicFactory{Fn: workerFn},
	}, agent.NewWorkerRegistry(), agent.NewPlannerRegistry(), Config{
		DataDir: t.TempDir(), IdleSleep: 10 * time.Millisecond, Debounce: 5 * time.Millisecond,
		MaxTurns: 1, MaxDuration: 30 * time.Second,
		SettlementTurns:   3,
		SettlementTimeout: 10 * time.Second,
	}, nil)
	if err := eng.Run(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	defer eng.StopTask(run.ID, ErrShutdown)

	waitIntentState(t, st, run.ID, intentID, model.StateExhausted, 15*time.Second)

	if got := countConfirmedFacts(t, st, run.ID); got != 2 {
		t.Fatalf("收尾轮没有把结果写回：期望 2 条事实，实际 %d", got)
	}
	if len(toolsPerCall) < 2 {
		t.Fatalf("至少要有主循环 + 收尾轮两次调用，实际 %d", len(toolsPerCall))
	}
	if !containsStr(toolsPerCall[0], "http_request") {
		t.Fatalf("主循环应当拿得到 http_request：%v", toolsPerCall[0])
	}
	if containsStr(toolsPerCall[1], "http_request") {
		t.Fatalf("收尾轮必须隐藏 http_request：%v", toolsPerCall[1])
	}
	if !containsStr(toolsPerCall[1], "record_fact") {
		t.Fatalf("收尾轮必须保留写回工具：%v", toolsPerCall[1])
	}

	acts, err := st.Activities(ctx, run.ID, 200)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range acts {
		if strings.Contains(a.Summary, "进入收尾轮") {
			found = true
		}
	}
	if !found {
		t.Fatalf("过程里应当能看到「进入收尾轮」")
	}
}

// TestSteerWorkBlocksNextToolCall：纠偏在"下一次工具调用前"送达，
// 并拦下那次调用（模型看到的是工具结果，而不是执行结果）。
func TestSteerWorkBlocksNextToolCall(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	run, _ := newLoopbackRun(t, st, "http://127.0.0.1:1")
	intentID, err := st.AddIntent(ctx, store.AddIntentInput{
		RunID: run.ID, Summary: "探测登录接口的鉴权方式",
	})
	if err != nil {
		t.Fatal(err)
	}

	var eng *Engine
	var calls int
	var sawSteer bool

	workerFn := func(_ context.Context, req *llm.Request) (*llm.Response, error) {
		n := calls
		calls++
		switch n {
		case 0:
			// 注意 record_fact 的入参形状：facts[] + intent_id（不是顶层 summary）。
			return &llm.Response{ToolCalls: []llm.ToolCall{llm.NewToolCall("s1", "record_fact", map[string]any{
				"facts": []map[string]any{{
					"summary": "主循环写回：登录接口返回 200", "evidence": "GET /login → 200", "confidence": "observed",
				}},
				"intent_id": intentID,
			})}}, nil
		case 1:
			// 在把这次响应交回去之前排一条纠偏：worker 处理这次工具调用前会取走它。
			if !eng.SteerWork(intentID, "改用只读探测，不要写任何东西") {
				t.Errorf("SteerWork 应当接受正在跑的意图")
			}
			return &llm.Response{ToolCalls: []llm.ToolCall{llm.NewToolCall("s2", "record_fact", map[string]any{
				"facts": []map[string]any{{
					"summary": "这条应当被拦下", "evidence": "不该执行", "confidence": "observed",
				}},
				"intent_id": intentID,
			})}}, nil
		default:
			// 最后一条 tool 消息应当是纠偏文本。
			for i := len(req.Messages) - 1; i >= 0; i-- {
				if req.Messages[i].Role == llm.RoleTool {
					if strings.Contains(req.Messages[i].Text, "【规划者实时纠偏】") {
						sawSteer = true
					}
					break
				}
			}
			return &llm.Response{Text: "已改道：只做只读探测"}, nil
		}
	}

	eng = New(st, routeFactory{
		planner: llm.NewFakeFactory(llm.Step{Text: "等 worker 产出"}),
		worker:  &llm.DynamicFactory{Fn: workerFn},
	}, agent.NewWorkerRegistry(), agent.NewPlannerRegistry(), Config{
		DataDir: t.TempDir(), IdleSleep: 10 * time.Millisecond, Debounce: 5 * time.Millisecond,
		MaxTurns: 5, MaxDuration: 30 * time.Second,
	}, nil)
	if err := eng.Run(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	defer eng.StopTask(run.ID, ErrShutdown)

	waitIntentState(t, st, run.ID, intentID, model.StateDone, 15*time.Second)

	if !sawSteer {
		t.Fatalf("模型没有看到纠偏文本（应当作为被拦下的工具结果）")
	}
	if got := countConfirmedFacts(t, st, run.ID); got != 1 {
		t.Fatalf("被拦下的写入不该执行：期望 1 条事实，实际 %d", got)
	}
	// 不存在的意图：直接拒绝（消息不能排给没人取的队列）。
	if eng.SteerWork(999999, "任何消息") {
		t.Fatalf("对不存在的意图 SteerWork 应当返回 false")
	}
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
