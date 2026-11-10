package engine

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"qinyuan/internal/agent"
	"qinyuan/internal/llm"
)

// 停摆等待策略是纯函数：把"等多久/计数怎么走"从循环里拆出来单测。
func TestNextStallWait(t *testing.T) {
	interval := 10 * time.Minute

	// 没停摆：不接管等待（返回负值），计数归零。
	if w, r, f := nextStallWait(false, 2, 3, interval); w >= 0 || r != 0 || f != 0 {
		t.Fatalf("未停摆不该接管等待：w=%v r=%d f=%d", w, r, f)
	}
	// 第 1、2 次停摆：立即重试。
	if w, r, f := nextStallWait(true, 0, 0, interval); w != stallReplanBackoff || r != 1 || f != 0 {
		t.Fatalf("第 1 次停摆应短退避：w=%v r=%d f=%d", w, r, f)
	}
	if w, r, f := nextStallWait(true, 1, 0, interval); w != stallReplanBackoff || r != 2 || f != 0 {
		t.Fatalf("第 2 次停摆应短退避：w=%v r=%d f=%d", w, r, f)
	}
	// 立即重试用尽 → 2 分钟巡检。
	if w, r, f := nextStallWait(true, 2, 0, interval); w != stallPatrolInterval || r != 2 || f != 1 {
		t.Fatalf("应立即重试后转巡检：w=%v r=%d f=%d", w, r, f)
	}
	// 巡检用尽 → 退回心跳。
	if w, r, f := nextStallWait(true, 2, maxStallFallbacks, interval); w != interval || r != 2 || f != maxStallFallbacks {
		t.Fatalf("巡检用尽应退回心跳：w=%v r=%d f=%d", w, r, f)
	}
	// 心跳比巡检还短时，巡检不能比心跳更慢。
	if w, _, _ := nextStallWait(true, 2, 0, 30*time.Second); w != 30*time.Second {
		t.Fatalf("巡检间隔不该超过心跳：%v", w)
	}
}

// run #41 第 9 轮：planner 产出 0 意图、且没有任何 open/running 意图，旧实现干等 600s 心跳。
// 现在必须在几秒内再问一次，而不是等满一个心跳。
func TestPlannerReplansImmediatelyWhenStalled(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	run, _ := newLoopbackRun(t, st, "http://127.0.0.1:1")
	// 跳过第 0 轮拆解，免得把模型调用算进来。
	if _, err := st.MarkGoalsDecomposed(ctx, run.ID); err != nil {
		t.Fatal(err)
	}

	var calls int32
	f := &llm.DynamicFactory{Fn: func(context.Context, *llm.Request) (*llm.Response, error) {
		atomic.AddInt32(&calls, 1)
		// 模拟"模型违反停摆规则"：只说一句，不派意图。
		return &llm.Response{Text: "本轮先不派意图。"}, nil
	}}

	eng := New(st, f, agent.NewWorkerRegistry(), agent.NewPlannerRegistry(), Config{
		DataDir: t.TempDir(), AllowLoopback: true,
		MaxTurns: 5, MaxDuration: 30 * time.Second, IdleSleep: 10 * time.Millisecond,
		Debounce: 5 * time.Millisecond,
		// 心跳故意设 1 小时：如果还靠心跳，6 秒内绝对不会出现第二轮。
		HeartbeatFloor: time.Hour,
	}, nil)
	if err := eng.Run(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	defer eng.StopTask(run.ID, ErrShutdown)

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && atomic.LoadInt32(&calls) < 2 {
		time.Sleep(50 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&calls); got < 2 {
		t.Fatalf("停摆后应立即重规划：8 秒内只调了 %d 次模型（心跳是 1 小时）", got)
	}

	acts, err := st.ActivityTail(ctx, run.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range acts {
		if strings.Contains(a.Summary, "探索停摆") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("停摆重规划必须留下 system 记录（否则控制台看不出来）")
	}
}

// 暂停/排队不算停摆：别在人工暂停时反复唤醒模型。
func TestIsStalledFalseWhenPaused(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	run, _ := newLoopbackRun(t, st, "http://127.0.0.1:1")
	if _, err := st.MarkGoalsDecomposed(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	eng := newEngineFor(t, st, llm.NewFakeFactory())
	if err := eng.Pause(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if eng.isStalled(ctx, run.ID) {
		t.Fatal("暂停的任务不该算停摆")
	}
}
