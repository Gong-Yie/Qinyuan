package outbox

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"qinyuan/internal/store"
	"qinyuan/internal/testkit"
)

// 这些用例需要一个真实 PG（store 的方法都是 SQL）。
// 与 store 包一样：库不可用就 skip，CI 上设 QINYUAN_TEST_DSN。
func testStore(t *testing.T) *store.Store {
	t.Helper()
	return testkit.Store(t)
}

// 成功投递：事件变 done，handler 只被调一次。
func TestDispatcherDeliversOnce(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	run, err := st.CreateRun(ctx, store.CreateRunInput{Goal: "outbox", WorkerCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.EnqueueOutbox(ctx, run.ID, "demo.topic", map[string]any{"n": 1}); err != nil {
		t.Fatal(err)
	}

	var calls int32
	d := New(st, map[string]Handler{
		"demo.topic": func(context.Context, store.OutboxEvent) error {
			atomic.AddInt32(&calls, 1)
			return nil
		},
	}, nil)

	n, err := d.Once(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("本轮应处理 1 条，实际 %d", n)
	}
	// 再跑一轮：已完成的不能再投一次。
	if n, err := d.Once(ctx); err != nil || n != 0 {
		t.Fatalf("已完成的事件不该再投：n=%d err=%v", n, err)
	}
	pending, dead, err := st.CountOutbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pending != 0 || dead != 0 {
		t.Fatalf("投递后应无积压/死信：pending=%d dead=%d", pending, dead)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("handler 调用次数 = %d，期望 1", calls)
	}
}

// 失败到上限：进死信，不再自动重试；重放后能再投。
func TestDispatcherRetriesThenDead(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	run, err := st.CreateRun(ctx, store.CreateRunInput{Goal: "outbox", WorkerCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.EnqueueOutbox(ctx, run.ID, "flaky.topic", map[string]any{"n": 1}); err != nil {
		t.Fatal(err)
	}

	var calls int32
	d := New(st, map[string]Handler{
		"flaky.topic": func(context.Context, store.OutboxEvent) error {
			atomic.AddInt32(&calls, 1)
			return errors.New("下游 503")
		},
	}, nil)
	d.MaxAttempts = 2
	d.Backoff = func(int) time.Duration { return 0 }

	// 投到死信（每次 Once 领 1 条；失败不阻塞下一轮）。
	for i := 0; i < 5; i++ {
		if _, err := d.Once(ctx); err != nil {
			t.Fatal(err)
		}
	}
	deadItems, err := st.ListOutbox(ctx, "dead", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(deadItems) != 1 {
		t.Fatalf("应有 1 条死信，实际 %d：%+v", len(deadItems), deadItems)
	}
	if calls < 1 {
		t.Fatal("handler 应当至少被调过一次")
	}
	if deadItems[0].LastError == "" {
		t.Fatal("死信必须带上最后一次错误（否则不知道该找谁）")
	}

	// 修好下游后重放：这次成功。
	if ok, err := st.ReplayOutbox(ctx, deadItems[0].ID); err != nil || !ok {
		t.Fatalf("重放失败：%v", err)
	}
	d.Handlers["flaky.topic"] = func(context.Context, store.OutboxEvent) error { return nil }
	if _, err := d.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if pending, dead, _ := st.CountOutbox(ctx); pending != 0 || dead != 0 {
		t.Fatalf("重放并成功后应清空：pending=%d dead=%d", pending, dead)
	}
}

// 没有注册处理器 = 配置错误：直接进死信，而不是静默丢弃或无限重试。
func TestDispatcherMissingHandlerIsDeadLetter(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	run, err := st.CreateRun(ctx, store.CreateRunInput{Goal: "outbox", WorkerCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.EnqueueOutbox(ctx, run.ID, "unknown.topic", map[string]any{"n": 1}); err != nil {
		t.Fatal(err)
	}
	d := New(st, nil, nil)
	d.MaxAttempts = 1
	d.Backoff = func(int) time.Duration { return 0 }
	if _, err := d.Once(ctx); err != nil {
		t.Fatal(err)
	}
	deadItems, err := st.ListOutbox(ctx, "dead", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(deadItems) != 1 {
		t.Fatalf("缺处理器应进死信，实际 %d 条", len(deadItems))
	}
}

// handler panic 不能带走进程，也不能把整批卡住。
func TestDispatcherPanicIsContained(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	run, err := st.CreateRun(ctx, store.CreateRunInput{Goal: "outbox", WorkerCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.EnqueueOutbox(ctx, run.ID, "boom.topic", map[string]any{"n": 1}); err != nil {
		t.Fatal(err)
	}
	d := New(st, map[string]Handler{
		"boom.topic": func(context.Context, store.OutboxEvent) error { panic("handler 崩了") },
	}, nil)
	d.MaxAttempts = 1
	d.Backoff = func(int) time.Duration { return 0 }
	if _, err := d.Once(ctx); err != nil {
		t.Fatalf("panic 应当被收住，Once 不该返回错误：%v", err)
	}
	if _, dead, _ := st.CountOutbox(ctx); dead != 1 {
		t.Fatalf("panic 的事件应进死信（可重放），实际 dead=%d", dead)
	}
}
