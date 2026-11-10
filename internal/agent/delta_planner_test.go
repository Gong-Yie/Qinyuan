package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"qinyuan/internal/llm"
	"qinyuan/internal/model"
	"qinyuan/internal/store"
	"qinyuan/internal/testkit"
)

// 本文件是 P1-2 在 planner 侧的三条验收：
//  1. 首轮给全量、之后给增量（且增量里确实带上了"上一轮之后发生的事"）；
//  2. 每 K 轮校准一次全量；
//  3. refresh_overview 能让下一轮回到全量。
//
// 另外用一份"更像真实长任务"的图量一次 prompt 体积（验收要求 −60%）。

func bumpRound(t *testing.T, st *store.Store, runID int64) {
	t.Helper()
	tag, err := st.Pool().Exec(context.Background(),
		`UPDATE runs SET round_no = round_no + 1 WHERE id=$1`, runID)
	if err != nil {
		t.Fatalf("推进轮次失败：%v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("推进轮次没有命中任何行：run=%d rows=%d", runID, tag.RowsAffected())
	}
	r, err := st.Run(context.Background(), runID)
	if err != nil {
		t.Fatalf("推进后读 run 失败：%v", err)
	}
	t.Logf("轮次推进到 %d（version=%d）", r.RoundNo, r.Version)
}

func plannerUserText(req *llm.Request) string {
	for _, m := range req.Messages {
		if m.Role == llm.RoleUser {
			return m.Text
		}
	}
	return ""
}

func newTestPlanner(st *store.Store, fn func(ctx context.Context, req *llm.Request) (*llm.Response, error), fullEvery int) *Planner {
	return &Planner{
		Store: st, Providers: &llm.DynamicFactory{Fn: fn},
		Registry: NewPlannerRegistry(), MaxTurns: 3,
		FullSnapshotEvery: fullEvery,
	}
}

// TestPlannerDeltaAfterFirstRound 首轮全量 → 增量带出新事实。
func TestPlannerDeltaAfterFirstRound(t *testing.T) {
	st := testkit.Store(t)
	ctx := context.Background()
	run, _ := testkit.Run(t, st, nil, nil)

	var msgs []string
	p := newTestPlanner(st, func(_ context.Context, req *llm.Request) (*llm.Response, error) {
		msgs = append(msgs, plannerUserText(req))
		return &llm.Response{Text: "本轮不派方向"}, nil
	}, 20)

	bumpRound(t, st, run.ID) // 引擎在每轮 Plan 之前推进轮次，这里照做
	if _, err := p.PlanRound(ctx, run.ID, nil, PlanOptions{}, nil, nil); err != nil {
		t.Fatalf("第一轮：%v", err)
	}
	if len(msgs) != 1 || !strings.Contains(msgs[0], "【本轮态势（overview 预取") {
		t.Fatalf("第一轮必须是全量态势，实际：%s", clipMsg(msgs))
	}

	// 模拟 worker 在两轮之间写了一条事实。
	factID, err := st.AddFact(ctx, store.AddFactInput{
		RunID: run.ID, Summary: "登录页响应里没有 CSRF token", Evidence: "GET /login → 无 csrf 字段",
		Confidence: "observed",
	})
	if err != nil {
		t.Fatalf("写事实失败：%v", err)
	}
	bumpRound(t, st, run.ID)

	if _, err := p.PlanRound(ctx, run.ID, nil, PlanOptions{}, nil, nil); err != nil {
		t.Fatalf("第二轮：%v", err)
	}
	second := msgs[len(msgs)-1]
	if !strings.Contains(second, "增量（版本") {
		t.Fatalf("第二轮应当是增量态势，实际：%s", clipMsg(msgs))
	}
	if !strings.Contains(second, "登录页响应里没有 CSRF token") {
		t.Fatalf("增量里没有带上新写的事实：%s", second)
	}
	if !strings.Contains(second, fmt.Sprintf("fact#%d", factID)) {
		t.Fatalf("增量事件缺少 fact#%d：%s", factID, second)
	}
	if !strings.Contains(second, "【当前态摘要") {
		t.Fatalf("增量轮也必须给当前态摘要（goals/running/ready 不能省）：%s", second)
	}
}

// TestPlannerFullSnapshotEveryK 每 K 轮校准一次全量。
func TestPlannerFullSnapshotEveryK(t *testing.T) {
	st := testkit.Store(t)
	ctx := context.Background()
	run, _ := testkit.Run(t, st, nil, nil)

	var msgs []string
	p := newTestPlanner(st, func(_ context.Context, req *llm.Request) (*llm.Response, error) {
		msgs = append(msgs, plannerUserText(req))
		return &llm.Response{Text: "ok"}, nil
	}, 3)

	for i := 0; i < 6; i++ {
		bumpRound(t, st, run.ID)
		if _, err := p.PlanRound(ctx, run.ID, nil, PlanOptions{}, nil, nil); err != nil {
			t.Fatalf("第 %d 轮：%v", i+1, err)
		}
	}
	// 轮次 1、4 是全量（(round-1)%3==0），2/3/5/6 是增量。
	want := []string{"full", "delta", "delta", "full", "delta", "delta"}
	for i, w := range want {
		got := "delta"
		if strings.Contains(msgs[i], "【本轮态势（overview 预取") {
			got = "full"
		}
		if got != w {
			t.Fatalf("第 %d 轮的态势形态 = %s，期望 %s", i+1, got, w)
		}
	}
}

// TestPlannerRefreshOverviewForcesFull 模型主动要全量时，下一轮回到全量。
func TestPlannerRefreshOverviewForcesFull(t *testing.T) {
	st := testkit.Store(t)
	ctx := context.Background()
	run, _ := testkit.Run(t, st, nil, nil)

	var msgs []string
	call := 0
	p := newTestPlanner(st, func(_ context.Context, req *llm.Request) (*llm.Response, error) {
		msgs = append(msgs, plannerUserText(req))
		call++
		switch call {
		case 1:
			return &llm.Response{Text: "ok"}, nil
		case 2:
			// 第二轮：模型觉得增量不够，要全量。
			return &llm.Response{ToolCalls: []llm.ToolCall{
				llm.NewToolCall("r1", "refresh_overview", map[string]any{}),
			}}, nil
		default:
			return &llm.Response{Text: "ok"}, nil
		}
	}, 20)

	bumpRound(t, st, run.ID)
	if _, err := p.PlanRound(ctx, run.ID, nil, PlanOptions{}, nil, nil); err != nil {
		t.Fatal(err)
	}
	bumpRound(t, st, run.ID)
	if _, err := p.PlanRound(ctx, run.ID, nil, PlanOptions{}, nil, nil); err != nil {
		t.Fatal(err)
	}
	// 这一轮的第一条消息是增量，但工具刷新后 planner 的基线被清空。
	if !strings.Contains(msgs[len(msgs)-1], "增量（版本") {
		t.Fatalf("第二轮应当先是增量：%s", clipMsg(msgs))
	}
	bumpRound(t, st, run.ID)
	if _, err := p.PlanRound(ctx, run.ID, nil, PlanOptions{}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msgs[len(msgs)-1], "【本轮态势（overview 预取") {
		t.Fatalf("refresh_overview 之后应当回到全量：%s", clipMsg(msgs))
	}
}

// TestDeltaPromptIsMuchSmaller 用一张"长任务"规模的图量一次体积差：
// 全量 vs 增量，验收要求单轮态势 token 下降 ≥60%（这里量字符数，等价）。
func TestDeltaPromptIsMuchSmaller(t *testing.T) {
	st := testkit.Store(t)
	ctx := context.Background()
	run, _ := testkit.Run(t, st, nil, nil)

	// 造一张"长任务"规模的图：把概览里每个列表都顶到上限
	// （facts 20 / done 12 / open 30 / findings 10）。真实跑到 200 轮时就是这个形态：
	// 全量的体积由上限决定（不再随轮次涨），而增量只跟"这一轮变了多少"有关。
	for i := 0; i < 20; i++ {
		if _, err := st.AddFact(ctx, store.AddFactInput{
			RunID:      run.ID,
			Summary:    fmt.Sprintf("第 %d 条历史结论：服务 %d 暴露了管理接口并返回版本指纹", i, i),
			Detail:     strings.Repeat("细节 ", 30),
			Evidence:   fmt.Sprintf("curl /api/%d → 200", i),
			Confidence: "observed",
		}); err != nil {
			t.Fatalf("造事实失败：%v", err)
		}
	}
	for i := 0; i < 10; i++ {
		if _, err := st.AddFinding(ctx, store.AddFindingInput{
			RunID: run.ID, Name: fmt.Sprintf("历史漏洞 %d", i), Severity: "medium",
			Summary:  fmt.Sprintf("第 %d 个已确认漏洞：某接口可越权读取他人订单", i),
			Evidence: fmt.Sprintf("GET /order/%d → 200 他人数据", i),
		}); err != nil {
			t.Fatalf("造漏洞失败：%v", err)
		}
	}
	for i := 0; i < 12; i++ {
		id, err := st.AddIntent(ctx, store.AddIntentInput{
			RunID: run.ID, Summary: fmt.Sprintf("历史方向 %d：探测 /admin 分支 %d 的口令策略", i, i),
		})
		if err != nil {
			t.Fatalf("造意图失败：%v", err)
		}
		if ok, err := st.ClaimIntent(ctx, run.ID, id, "w"); err != nil || !ok {
			t.Fatalf("claim: %v", err)
		}
		if ok, err := st.CASIntent(ctx, run.ID, id, model.StateRunning, model.StateDone); err != nil || !ok {
			t.Fatalf("cas: %v", err)
		}
	}
	for i := 0; i < 30; i++ {
		if _, err := st.AddIntent(ctx, store.AddIntentInput{
			RunID: run.ID, Summary: fmt.Sprintf("排队中的方向 %d：核查 /v%d 接口的鉴权与越权面", i, i),
		}); err != nil {
			t.Fatalf("造 open 意图失败：%v", err)
		}
	}

	full, err := BuildOverview(ctx, st, run.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	current, err := st.Run(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	fullMsg := BuildPlannerUserMessage(current, nil, full, nil, false)

	// 增量窗口：只取最后两三次变化（真实轮次里的量级）。
	since := current.Version - 3
	d, truncated, err := BuildDeltaOverview(ctx, st, run.ID, current, since, 200)
	if err != nil || truncated {
		t.Fatalf("读增量失败：err=%v truncated=%v", err, truncated)
	}
	deltaMsg := BuildPlannerUserMessage(current, nil, nil, d, false)

	if len(deltaMsg) >= len(fullMsg) {
		t.Fatalf("增量没有更小：delta=%d full=%d", len(deltaMsg), len(fullMsg))
	}
	ratio := float64(len(deltaMsg)) / float64(len(fullMsg))
	t.Logf("态势体积：全量 %d 字符，增量 %d 字符（%.0f%%）", len(fullMsg), len(deltaMsg), ratio*100)
	if ratio > 0.6 {
		t.Fatalf("增量体积 %.0f%% 仍超过 60%% 的验收线（delta=%d full=%d）", ratio*100, len(deltaMsg), len(fullMsg))
	}
}

func clipMsg(all []string) string {
	if len(all) == 0 {
		return "（没有捕获到消息）"
	}
	s := all[len(all)-1]
	if len([]rune(s)) > 400 {
		return string([]rune(s)[:400]) + "…"
	}
	return s
}
