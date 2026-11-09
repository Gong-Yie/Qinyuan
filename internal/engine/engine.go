// Package engine 是唯一的"知道现在该跑谁"的地方：领取意图、执行、按原因落终态。
//
// M1 只实现 workerLoop（plannerLoop 在 M2 接上）；但收尾分流已经按
// 实现规格/02-调度与状态机 §3 的九分支顺序写全，避免 M2 重写。
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"qinyuan/internal/agent"
	"qinyuan/internal/llm"
	"qinyuan/internal/model"
	"qinyuan/internal/store"
)

// 具名取消原因 = 状态机输入（学习.md 1.4）。
var (
	ErrPausedByUser    = errors.New("paused_by_user")
	ErrCancelledByUser = errors.New("cancelled_by_user")
	ErrKilledByPlanner = errors.New("killed_by_planner")
	ErrTimeoutSettling = errors.New("timeout_settling")
	ErrShutdown        = errors.New("shutdown")
)

// Config 引擎运行参数（任务级参数在 DB，不在这里）。
type Config struct {
	DataDir       string
	AllowLoopback bool
	MaxOutput     int
	MaxTurns      int
	MaxDuration   time.Duration
	IdleSleep     time.Duration
}

// Engine 持有全部运行时状态。注意：这些 map 只是"活着的进程状态"，
// 正确性依赖的状态全在 DB 里 —— 丢了也能从 DB 重建。
type Engine struct {
	Store     *store.Store
	Providers llm.Factory
	Registry  *agent.Registry
	Cfg       Config
	Log       *slog.Logger

	mu          sync.Mutex
	paused      map[int64]bool
	taskCancels map[int64]context.CancelCauseFunc
	workCancels map[int64]context.CancelCauseFunc
	workRuns    map[int64]int64

	inflightMu sync.Mutex
	inflight   map[int64]int
}

// New 装配引擎。
func New(st *store.Store, providers llm.Factory, reg *agent.Registry, cfg Config, log *slog.Logger) *Engine {
	if cfg.IdleSleep <= 0 {
		cfg.IdleSleep = 500 * time.Millisecond
	}
	if cfg.MaxTurns <= 0 {
		cfg.MaxTurns = 12
	}
	if cfg.MaxDuration <= 0 {
		cfg.MaxDuration = 10 * time.Minute
	}
	if log == nil {
		log = slog.Default()
	}
	return &Engine{
		Store: st, Providers: providers, Registry: reg, Cfg: cfg, Log: log,
		paused:      map[int64]bool{},
		taskCancels: map[int64]context.CancelCauseFunc{},
		workCancels: map[int64]context.CancelCauseFunc{},
		workRuns:    map[int64]int64{},
		inflight:    map[int64]int{},
	}
}

func (e *Engine) worker() *agent.Worker {
	return &agent.Worker{
		Store:         e.Store,
		Providers:     e.Providers,
		Registry:      e.Registry,
		DataDir:       e.Cfg.DataDir,
		AllowLoopback: e.Cfg.AllowLoopback,
		MaxOutput:     e.Cfg.MaxOutput,
		MaxTurns:      e.Cfg.MaxTurns,
		MaxDuration:   e.Cfg.MaxDuration,
		Logger:        e.Log,
	}
}

// Work 执行一次"领取 + 执行 + 落终态"。返回是否领到了活。
func (e *Engine) Work(ctx context.Context, runID int64, owner string) (bool, error) {
	run, err := e.Store.Run(ctx, runID)
	if err != nil {
		return false, err
	}
	if run.Task == nil {
		return false, fmt.Errorf("run #%d 没有任务行", runID)
	}
	if e.isPaused(runID) || run.Task.Paused {
		return false, nil
	}
	if model.IsTerminalTaskState(run.Task.Status) {
		return false, nil
	}

	intent, err := e.claimNext(ctx, runID, owner)
	if err != nil {
		return false, err
	}
	if intent == nil {
		return false, nil
	}

	// 第一次真正执行：盖 first_run 并切 running。
	if err := e.Store.StampFirstRun(ctx, runID, run.Task.TimeoutSeconds); err != nil {
		e.Log.Warn("stamp first_run 失败", "run", runID, "err", err)
	}
	_, _ = e.Store.CASRunTaskStatus(ctx, runID, []string{model.TaskCreated, model.TaskPaused, "queued"}, model.TaskRunning)

	e.beginInflight(runID)
	defer e.endInflight(runID)

	workCtx, cancel := context.WithCancelCause(ctx)
	e.registerWork(intent.ID, runID, cancel)
	defer e.unregisterWork(intent.ID)

	out, execErr := e.worker().Execute(workCtx, runID, intent.ID, owner, e.emit)
	cause := context.Cause(workCtx)
	cancel(nil)

	if execErr != nil && out == nil {
		// 如果这次失败只是"因为被取消"，那不是执行错误：不能标成 blocked。
		if workCtx.Err() != nil || errors.Is(execErr, context.Canceled) || errors.Is(execErr, context.DeadlineExceeded) {
			out = &agent.Outcome{Reason: agent.ReasonCanceled}
		} else {
			out = &agent.Outcome{Reason: agent.ReasonError, Err: execErr}
		}
	}
	state := e.settle(ctx, runID, intent.ID, out, cause)
	e.emit(context.WithoutCancel(ctx), &model.Activity{
		RunID: runID, NodeID: &intent.ID, Worker: owner, Kind: model.ActSystem,
		Summary: fmt.Sprintf("意图 #%d 结束：%s（%s）", intent.ID, state, out.Reason),
		Metadata: map[string]any{
			"intent_id": intent.ID, "state": state, "reason": out.Reason,
			"turns": out.Turns, "facts": out.Facts, "findings": out.Findings,
		},
	})
	e.Log.Info("[worker] 意图结束", "run", runID, "intent", intent.ID, "state", state, "reason", out.Reason, "turns", out.Turns)
	return true, nil
}

func (e *Engine) claimNext(ctx context.Context, runID int64, owner string) (*model.Node, error) {
	rows, err := e.Store.Frontier(ctx, runID, 20)
	if err != nil {
		return nil, err
	}
	for _, n := range rows {
		ok, err := e.Store.ClaimIntent(ctx, runID, n.ID, owner)
		if err != nil {
			return nil, err
		}
		if ok {
			return n, nil
		}
	}
	return nil, nil
}

// settle 是九分支收尾分流的唯一实现。顺序不能改：
// 顺序错一个，UI 上就会大面积"失败"或"未完成"。
func (e *Engine) settle(ctx context.Context, runID, intentID int64, out *agent.Outcome, cause error) string {
	// ★ 收尾必须用"不随取消而失效"的 ctx：暂停/超时正是 cancel 之后才收尾，
	//   用原 ctx 写库会失败，意图就永远卡在 running（07 的经典错误 #2）。
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	run, err := e.Store.Run(writeCtx, runID)
	if err != nil {
		e.Log.Error("收尾时读 run 失败", "run", runID, "err", err)
	}

	next := ""
	switch {
	case errors.Is(cause, ErrPausedByUser):
		// 任务级暂停：意图放回 frontier，恢复后续跑。
		next = model.StateOpen
	case errors.Is(cause, ErrCancelledByUser):
		next = model.StatePaused
	case errors.Is(cause, ErrKilledByPlanner):
		next = model.StateStopped
	case errors.Is(cause, ErrTimeoutSettling):
		next = model.StateExhausted
	case errors.Is(cause, ErrShutdown):
		next = model.StateOpen
	case out.Reason == agent.ReasonCanceled && run != nil && run.Task != nil && run.Task.Paused:
		next = model.StateOpen
	case run != nil && run.Task != nil && model.IsTerminalTaskState(run.Task.Status):
		next = model.StateStopped
	case out.Err != nil:
		next = model.StateBlocked
	case out.Reason == agent.ReasonMaxTurns || out.Reason == agent.ReasonTimeout:
		next = model.StateExhausted
	default:
		next = model.StateDone
	}

	ok, err := e.Store.CASIntent(writeCtx, runID, intentID, model.StateRunning, next)
	if err != nil {
		e.Log.Error("CAS 意图终态失败", "run", runID, "intent", intentID, "next", next, "err", err)
		return model.StateRunning
	}
	if !ok {
		e.Log.Warn("CAS 冲突：意图已不在 running", "run", runID, "intent", intentID, "next", next)
		return model.StateRunning
	}
	if next == model.StateBlocked && isLLMChainExhausted(out.Err) {
		_ = e.Store.SetBlockedReason(writeCtx, runID, intentID, "quota")
	}
	return next
}

// RunLoop 起一个 worker 循环（M1：CLI 手动；M2：随 run 启动）。
func (e *Engine) RunLoop(ctx context.Context, runID int64, owner string) error {
	taskCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	e.mu.Lock()
	e.taskCancels[runID] = cancel
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.taskCancels, runID)
		e.mu.Unlock()
	}()

	for {
		if taskCtx.Err() != nil {
			return nil
		}
		claimed, err := e.Work(taskCtx, runID, owner)
		if err != nil {
			e.Log.Error("worker 执行异常", "run", runID, "err", err)
		}
		if claimed {
			continue
		}
		active, err := e.Store.HasActiveIntent(ctx, runID)
		if err == nil && !active {
			return nil
		}
		select {
		case <-taskCtx.Done():
			return nil
		case <-time.After(e.Cfg.IdleSleep):
		}
	}
}

// Pause 暂停任务：先装栅栏，再 cancel 在跑的（只标记不取消 = 暂停了还在烧 token）。
func (e *Engine) Pause(ctx context.Context, runID int64) error {
	e.mu.Lock()
	e.paused[runID] = true
	cancel := e.taskCancels[runID]
	// 直接调用 Work（HTTP /work、CLI --once）时没有 taskCtx，必须能取消到在跑的 work。
	var workCancels []context.CancelCauseFunc
	for intentID, rid := range e.workRuns {
		if rid == runID {
			if c, ok := e.workCancels[intentID]; ok {
				workCancels = append(workCancels, c)
			}
		}
	}
	e.mu.Unlock()

	if err := e.Store.SetTaskPaused(ctx, runID, true); err != nil {
		return err
	}
	if _, err := e.Store.CASRunTaskStatus(ctx, runID, []string{model.TaskRunning, model.TaskCreated, "queued"}, model.TaskPaused); err != nil {
		return err
	}
	if cancel != nil {
		cancel(ErrPausedByUser)
	}
	for _, c := range workCancels {
		c(ErrPausedByUser)
	}
	return nil
}

// Resume 恢复任务。
func (e *Engine) Resume(ctx context.Context, runID int64) error {
	e.mu.Lock()
	delete(e.paused, runID)
	e.mu.Unlock()
	if err := e.Store.SetTaskPaused(ctx, runID, false); err != nil {
		return err
	}
	_, err := e.Store.CASRunTaskStatus(ctx, runID, []string{model.TaskPaused}, model.TaskRunning)
	return err
}

// KillWork 杀掉一条在跑的意图（planner kill_work 的实现；M2 接 planner 时用）。
func (e *Engine) KillWork(intentID int64) bool {
	e.mu.Lock()
	cancel := e.workCancels[intentID]
	e.mu.Unlock()
	if cancel == nil {
		return false
	}
	cancel(ErrKilledByPlanner)
	return true
}

func (e *Engine) isPaused(runID int64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.paused[runID]
}

func (e *Engine) registerWork(intentID, runID int64, cancel context.CancelCauseFunc) {
	e.mu.Lock()
	e.workCancels[intentID] = cancel
	e.workRuns[intentID] = runID
	e.mu.Unlock()
}

func (e *Engine) unregisterWork(intentID int64) {
	e.mu.Lock()
	delete(e.workCancels, intentID)
	delete(e.workRuns, intentID)
	e.mu.Unlock()
}

func (e *Engine) beginInflight(runID int64) {
	e.inflightMu.Lock()
	e.inflight[runID]++
	e.inflightMu.Unlock()
}

func (e *Engine) endInflight(runID int64) {
	e.inflightMu.Lock()
	if e.inflight[runID] > 0 {
		e.inflight[runID]--
	}
	e.inflightMu.Unlock()
}

// Inflight 当前在跑的 Plan/Execute 数（删除屏障与超时收尾要等它归零）。
func (e *Engine) Inflight(runID int64) int {
	e.inflightMu.Lock()
	defer e.inflightMu.Unlock()
	return e.inflight[runID]
}

// emit 是单一发射点：落库（带重试）成功后才算数；失败必须打日志，不能静默。
func (e *Engine) emit(ctx context.Context, a *model.Activity) {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	for attempt := 1; attempt <= 3; attempt++ {
		id, err := e.Store.AppendActivity(writeCtx, a)
		if err == nil {
			a.ID = id
			return
		}
		e.Log.Warn("[activity] 写入失败", "run", a.RunID, "kind", a.Kind, "tool", a.Tool,
			"attempt", attempt, "err", err)
		time.Sleep(time.Duration(attempt) * 25 * time.Millisecond)
	}
	// 丢一条 tool_result，UI 上那张卡片会永远"执行中"；丢一条 result，会话就没有总结。
	e.Log.Error("[activity] 丢弃活动记录（重试 3 次仍失败）",
		"run", a.RunID, "worker", a.Worker, "kind", a.Kind,
		"tool", a.Tool, "tool_use_id", a.ToolUseID, "summary", model.TrimSummary(a.Summary, 80))
}

func isLLMChainExhausted(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, llm.ErrQuota) || errors.Is(err, llm.ErrAuth) ||
		strings.Contains(strings.ToLower(err.Error()), "quota")
}
