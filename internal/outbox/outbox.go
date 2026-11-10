// Package outbox 是 P2-3 的投递器：把"提交了就必须发生"的副作用可靠地送到处理器手上。
//
// 语义（与《升级计划》P2-3 对齐）：
//   - 租约式领取（FOR UPDATE SKIP LOCKED），多实例并发安全；
//   - 失败退避重试，超过上限进死信（可查、可重放）；
//   - 处理器按 topic 注册；没有处理器 = 配置错误，直接进死信（不要静默丢弃）。
//
// 刻意不做的：不给 handler 提供事务（它可能调外部系统，事务开着只会拖死库）。
package outbox

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"qinyuan/internal/store"
)

// Handler 处理一条事件。返回 error 表示可重试失败（外部系统抖动/限流）。
type Handler func(ctx context.Context, ev store.OutboxEvent) error

// Dispatcher 是一个（或几个）投递循环。
type Dispatcher struct {
	Store       *store.Store
	Handlers    map[string]Handler
	Lease       time.Duration // 租约时长（默认 180s）
	Batch       int           // 每次领取条数（默认 50）
	MaxAttempts int           // 超过就进死信（默认 5）
	Interval    time.Duration // 空闲轮询间隔（默认 3s）
	Log         *slog.Logger

	// Backoff 可注入（测试里调小，生产用指数退避）。
	Backoff func(attempts int) time.Duration
}

// New 装配投递器（默认值见字段注释）。
func New(st *store.Store, handlers map[string]Handler, log *slog.Logger) *Dispatcher {
	if log == nil {
		log = slog.Default()
	}
	if handlers == nil {
		handlers = map[string]Handler{}
	}
	return &Dispatcher{
		Store: st, Handlers: handlers, Log: log,
		Lease: 3 * time.Minute, Batch: 50, MaxAttempts: 5, Interval: 3 * time.Second,
		Backoff: DefaultBackoff,
	}
}

// DefaultBackoff 指数退避，封顶 5 分钟。
func DefaultBackoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	if attempts > 8 {
		attempts = 8
	}
	d := time.Duration(1<<uint(attempts)) * time.Second
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	return d
}

// Run 是常驻循环；ctx 取消即退出（正在处理的那一批会做完）。
func (d *Dispatcher) Run(ctx context.Context) {
	if d.Store == nil {
		return
	}
	ticker := time.NewTicker(d.Interval)
	defer ticker.Stop()
	for {
		n, err := d.Once(ctx)
		if err != nil && ctx.Err() == nil {
			d.Log.Warn("[outbox] 本轮投递出错", "err", err)
		}
		// 领到活了就立刻再来一轮（不用等 ticker）：积压时吞吐才不会只有 1 批/3s。
		if n > 0 && ctx.Err() == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Once 领取并处理一批，返回处理条数（测试直接调它，不必等 ticker）。
func (d *Dispatcher) Once(ctx context.Context) (int, error) {
	if d.Store == nil {
		return 0, nil
	}
	events, err := d.Store.ClaimOutbox(ctx, d.Batch, d.Lease)
	if err != nil {
		return 0, err
	}
	if len(events) == 0 {
		return 0, nil
	}
	for _, ev := range events {
		d.dispatch(ctx, ev)
	}
	return len(events), nil
}

func (d *Dispatcher) dispatch(ctx context.Context, ev store.OutboxEvent) {
	h, ok := d.Handlers[ev.Topic]
	if !ok {
		d.fail(ctx, ev, fmt.Errorf("没有注册 topic %q 的处理器（配置错误，不重试）", ev.Topic))
		return
	}
	// 单条事件失败不能拖垮整批：handler 的 panic 也要收住（它会带走整个进程）。
	err := func() (err error) {
		defer func() {
			if rec := recover(); rec != nil {
				err = fmt.Errorf("handler panic: %v", rec)
			}
		}()
		return h(ctx, ev)
	}()
	if err != nil {
		d.fail(ctx, ev, err)
		return
	}
	if err := d.Store.MarkOutboxDone(ctx, ev.ID); err != nil {
		d.Log.Warn("[outbox] 标记完成失败（租约到期后会重投）", "id", ev.ID, "err", err)
		return
	}
	d.Log.Info("[outbox] 事件已投递", "id", ev.ID, "topic", ev.Topic, "attempts", ev.Attempts)
}

func (d *Dispatcher) fail(ctx context.Context, ev store.OutboxEvent, cause error) {
	backoff := d.Backoff
	if backoff == nil {
		backoff = DefaultBackoff
	}
	dead, err := d.Store.FailOutbox(ctx, ev.ID, cause, backoff(ev.Attempts), d.MaxAttempts)
	if err != nil {
		d.Log.Warn("[outbox] 记录失败失败", "id", ev.ID, "err", err)
		return
	}
	if dead {
		d.Log.Error("[outbox] 事件进死信", "id", ev.ID, "topic", ev.Topic,
			"attempts", ev.Attempts, "err", cause)
		return
	}
	d.Log.Warn("[outbox] 投递失败，稍后重试", "id", ev.ID, "topic", ev.Topic,
		"attempts", ev.Attempts, "err", cause)
}
