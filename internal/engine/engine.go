// Package engine 是唯一的"知道现在该跑谁"的地方：
// planner 循环（事件驱动）+ N 个 worker 循环（拉取式）+ 超时收尾 + 启动恢复。
//
// 依据：实现规格/02-调度与状态机。M2 起 planner 与 worker 都在这里跑。
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
	"qinyuan/internal/trace"
)

// 具名取消原因 = 状态机输入（学习.md 1.4）。
var (
	ErrPausedByUser    = errors.New("paused_by_user")
	ErrCancelledByUser = errors.New("cancelled_by_user")
	ErrKilledByPlanner = errors.New("killed_by_planner")
	ErrTimeoutSettling = errors.New("timeout_settling")
	ErrGoalMet         = errors.New("goal_met")
	ErrShutdown        = errors.New("shutdown")
)

// Config 引擎运行参数（任务级参数在 DB）。
type Config struct {
	DataDir       string
	AllowLoopback bool
	MaxOutput     int
	MaxTurns      int
	MaxDuration   time.Duration

	IdleSleep        time.Duration // worker 抢不到活的轮询间隔
	Debounce         time.Duration // 合并突发变更
	HeartbeatFloor   time.Duration // 心跳下限（规格：≥600s）
	SettleDrainGrace time.Duration // 超时收尾等待在跑 worker 的上限
	FinalPlanTimeout time.Duration // 收尾轮独立 ctx 的超时

	// M3.5：第 0 轮目标拆解 + 差分态势。
	GoalsMaxTurns    int // 拆解 agent 的回合上限（默认 8）
	PlannerFullEvery int // 每 K 轮给一次全量态势（默认 20）
	DeltaLimit       int // 单轮增量最多多少条变化（默认 200）

	// M4：长跑可靠性。
	SettlementTurns     int           // 撞预算后的收尾轮数（默认 3）
	SettlementTimeout   time.Duration // 收尾轮独立预算（默认 2 分钟）
	ContextWindowTokens int           // 上下文窗口估计（0 = 关闭压缩）

	// M4b：并发准入与删除。
	MaxConcurrentRuns  int           // 同时跑的任务上限（0 = 不限，保持旧行为）
	DeleteDrainTimeout time.Duration // 删除时等循环退出/在跑操作静默的上限（默认 10s）

	// M6a：审批门。
	ApprovalTimeout   time.Duration // 等人审批的上限（默认 5 分钟）
	ApprovalOnTimeout string        // 超时动作：deny（默认）| allow
	ApprovalDisabled  bool          // 显式关闭审批门（默认 false = 开启，规则默认放行）

	// P3-4：按风险分级的等待预算与超时动作（0 = 用 DefaultTiers）。
	ApprovalLowSeconds  int
	ApprovalMidSeconds  int
	ApprovalHighSeconds int
	ApprovalLowAction   string
	ApprovalMidAction   string
	ApprovalHighAction  string
}

type runRuntime struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	wg     sync.WaitGroup
}

// Engine 持有全部运行时状态。map 里只是"活着的进程状态"，
// 正确性依赖的状态全在 DB 里 —— 丢了也能从 DB 重建。
type Engine struct {
	Store           *store.Store
	Providers       llm.Factory
	WorkerTools     *agent.Registry
	PlannerTools    *agent.Registry
	Cfg             Config
	Log             *slog.Logger
	planner         *agent.Planner
	plannerMaxTurns int
	goals           *agent.GoalDecomposer
	gate            *agent.Gate

	mu          sync.Mutex
	paused      map[int64]bool
	settling    map[int64]bool
	deleting    map[int64]bool
	taskCancels map[int64]context.CancelCauseFunc
	workCancels map[int64]context.CancelCauseFunc
	workRuns    map[int64]int64
	steerBox    map[int64][]string // 单条意图的纠偏队列（worker 在下一次工具调用前取走）
	runtimes    map[int64]*runRuntime
	notifies    map[int64]chan struct{}
	triggers    map[int64][]agent.Trigger
	activeLLM   map[int64]int

	inflightMu sync.Mutex
	inflight   map[int64]int

	// Publish 由编排层注入：活动落库成功后推给 SSE 广播（可丢）。
	// 不直接依赖 server 包 —— 引擎不知道"有没有控制台"。
	Publish func(*model.Activity)
}

// New 装配引擎。
func New(st *store.Store, providers llm.Factory, workerTools, plannerTools *agent.Registry, cfg Config, log *slog.Logger) *Engine {
	if cfg.IdleSleep <= 0 {
		cfg.IdleSleep = 500 * time.Millisecond
	}
	if cfg.Debounce <= 0 {
		cfg.Debounce = 800 * time.Millisecond
	}
	if cfg.HeartbeatFloor <= 0 {
		cfg.HeartbeatFloor = 600 * time.Second
	}
	if cfg.SettleDrainGrace <= 0 {
		cfg.SettleDrainGrace = 90 * time.Second
	}
	if cfg.FinalPlanTimeout <= 0 {
		cfg.FinalPlanTimeout = 2 * time.Minute
	}
	if cfg.DeleteDrainTimeout <= 0 {
		cfg.DeleteDrainTimeout = 10 * time.Second
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
	if plannerTools == nil {
		plannerTools = agent.NewPlannerRegistry()
	}
	e := &Engine{
		Store: st, Providers: providers,
		WorkerTools: workerTools, PlannerTools: plannerTools,
		Cfg: cfg, Log: log,
		paused:      map[int64]bool{},
		settling:    map[int64]bool{},
		deleting:    map[int64]bool{},
		taskCancels: map[int64]context.CancelCauseFunc{},
		workCancels: map[int64]context.CancelCauseFunc{},
		workRuns:    map[int64]int64{},
		steerBox:    map[int64][]string{},
		runtimes:    map[int64]*runRuntime{},
		notifies:    map[int64]chan struct{}{},
		triggers:    map[int64][]agent.Trigger{},
		activeLLM:   map[int64]int{},
		inflight:    map[int64]int{},
	}
	e.plannerMaxTurns = 6
	e.planner = &agent.Planner{
		Store: st, Providers: providers, Registry: plannerTools,
		DataDir: cfg.DataDir, MaxOutput: cfg.MaxOutput, MaxTurns: e.plannerMaxTurns, Logger: log,
		FullSnapshotEvery: cfg.PlannerFullEvery, DeltaLimit: cfg.DeltaLimit,
	}
	if cfg.GoalsMaxTurns <= 0 {
		cfg.GoalsMaxTurns = 8
	}
	e.Cfg.GoalsMaxTurns = cfg.GoalsMaxTurns
	e.goals = &agent.GoalDecomposer{
		Store: st, Providers: providers, Registry: agent.NewGoalsRegistry(),
		DataDir: cfg.DataDir, MaxOutput: cfg.MaxOutput, MaxTurns: cfg.GoalsMaxTurns, Logger: log,
	}
	// planner 的两只手（kill / steer）只能在引擎里实现：它们要摸到 work 的 ctx 与队列。
	e.planner.KillWork = e.KillWork
	e.planner.SteerWork = e.SteerWork
	if !cfg.ApprovalDisabled {
		e.gate = &agent.Gate{
			Store: st, Timeout: cfg.ApprovalTimeout, OnTimeout: cfg.ApprovalOnTimeout,
		}
		e.gate.Tiers = agent.DefaultTiers()
		if cfg.ApprovalLowSeconds > 0 || cfg.ApprovalMidSeconds > 0 || cfg.ApprovalHighSeconds > 0 {
			e.gate.Tiers = agent.TierPolicy{
				LowTimeout:  time.Duration(cfg.ApprovalLowSeconds) * time.Second,
				MidTimeout:  time.Duration(cfg.ApprovalMidSeconds) * time.Second,
				HighTimeout: time.Duration(cfg.ApprovalHighSeconds) * time.Second,
				LowAction:   cfg.ApprovalLowAction,
				MidAction:   cfg.ApprovalMidAction,
				HighAction:  cfg.ApprovalHighAction,
			}
		}
		// 预授权放行必须留痕：每一条"没问人就放过"的调用都写一条 activity。
		e.gate.Audit = func(ctx context.Context, runID, intentID int64, d *store.ApprovalDecision, tool, target string) {
			var nodeID *int64
			if intentID > 0 {
				id := intentID
				nodeID = &id
			}
			e.emit(context.WithoutCancel(ctx), &model.Activity{
				RunID: runID, NodeID: nodeID, Worker: "gate", Kind: model.ActSystem,
				Summary: fmt.Sprintf("预授权放行：%s %s（策略 #%d）", tool, target, d.PolicyID),
				Metadata: map[string]any{
					"policy_id": d.PolicyID, "tool": tool, "target": target,
					"source": d.Source, "note": d.Note,
				},
			})
		}
	}
	return e
}

func (e *Engine) worker() *agent.Worker {
	w := &agent.Worker{
		Store: e.Store, Providers: e.Providers, Registry: e.WorkerTools,
		DataDir: e.Cfg.DataDir, AllowLoopback: e.Cfg.AllowLoopback,
		MaxOutput: e.Cfg.MaxOutput, MaxTurns: e.Cfg.MaxTurns,
		MaxDuration: e.Cfg.MaxDuration, Logger: e.Log,
		Settlement: agent.SettlementConfig{
			MaxTurns: e.Cfg.SettlementTurns,
			Timeout:  e.Cfg.SettlementTimeout,
		},
		Compaction: agent.CompactionConfig{WindowTokens: e.Cfg.ContextWindowTokens},
		Gate:       e.gate,
	}
	return w
}

// ---------- 任务生命周期 ----------

// Run 为一个任务起 1 个 planner 循环 + N 个 worker 循环（幂等）。
func (e *Engine) Run(ctx context.Context, runID int64) error {
	run, err := e.Store.Run(ctx, runID)
	if err != nil {
		return err
	}
	if run.Task == nil {
		return fmt.Errorf("run #%d 没有任务行", runID)
	}
	if model.IsTerminalTaskState(run.Task.Status) || run.Task.Paused || run.Task.Queued {
		return nil
	}
	if e.isDeleting(runID) {
		return nil // 删除屏障：不再起循环
	}

	e.mu.Lock()
	if _, ok := e.runtimes[runID]; ok {
		e.mu.Unlock()
		e.Notify(runID) // 已启动：只唤醒一轮
		return nil
	}
	// 循环的生命周期必须由引擎自己拥有，不能挂在调用方（HTTP 请求/RPC）的 ctx 上：
	// 请求一返回 ctx 就会被取消，任务会"启动即停止"。
	base := context.WithoutCancel(ctx)
	rctx, cancel := context.WithCancelCause(base)
	rt := &runRuntime{ctx: rctx, cancel: cancel}
	e.runtimes[runID] = rt
	e.taskCancels[runID] = cancel
	e.mu.Unlock()

	workers := run.Task.WorkerCount
	if workers <= 0 {
		workers = 2
	}

	rt.wg.Add(1)
	go func() {
		defer rt.wg.Done()
		e.plannerLoop(rctx, runID)
	}()
	for i := 1; i <= workers; i++ {
		name := fmt.Sprintf("work#%d", i)
		rt.wg.Add(1)
		go func(n string) {
			defer rt.wg.Done()
			e.workerLoop(rctx, runID, n)
		}(name)
	}
	if run.Task.TimeoutSeconds > 0 {
		// 超时协调器不挂在 rt.wg 上：它自己会调用 settleTask → StopTask，
		// 若把自己算进等待集合就会自锁（wg.Wait 等自己）。
		go e.deadlineLoop(rctx, runID)
	}

	e.Log.Info("[engine] 任务启动", "run", runID, "workers", workers, "timeout", run.Task.TimeoutSeconds)
	// 只有完全没有活时才 kick 首轮规划（open 或 running 都算有活）。
	if active, err := e.Store.HasActiveIntent(ctx, runID); err == nil && !active {
		e.Notify(runID)
	}
	return nil
}

// StopTask 停掉一个任务的全部循环（不发终态，调用方负责语义）。
func (e *Engine) StopTask(runID int64, cause error) {
	e.mu.Lock()
	rt := e.runtimes[runID]
	delete(e.runtimes, runID)
	delete(e.taskCancels, runID)
	e.mu.Unlock()

	if rt != nil {
		rt.cancel(cause)
		rt.wg.Wait()
	}
	e.Log.Info("[engine] 任务循环停止", "run", runID, "cause", cause)
}

// shutdownRuntime 用于"循环内部自己要求退出"（目标达成）。
// 它不能在当前 goroutine 上调 StopTask —— 那样会等自己，死锁。
func (e *Engine) shutdownRuntime(runID int64, cause error) {
	e.mu.Lock()
	rt := e.runtimes[runID]
	delete(e.runtimes, runID)
	delete(e.taskCancels, runID)
	e.mu.Unlock()
	if rt == nil {
		return
	}
	rt.cancel(cause)
	go func() {
		rt.wg.Wait()
		e.Log.Info("[engine] 任务循环退出", "run", runID, "cause", cause)
		// 一个任务结束就腾出了一个并发槽：立刻补位（不依赖 5s 的兜底 ticker）。
		e.Reconcile(context.Background())
	}()
}

// StopAll 停掉所有任务循环（进程退出用）。
func (e *Engine) StopAll(cause error) {
	e.mu.Lock()
	ids := make([]int64, 0, len(e.runtimes))
	for id := range e.runtimes {
		ids = append(ids, id)
	}
	e.mu.Unlock()
	for _, id := range ids {
		e.StopTask(id, cause)
	}
}

// Pause 暂停任务：先装栅栏，再停循环（只标记不取消 = 暂停了还在烧 token）。
func (e *Engine) Pause(ctx context.Context, runID int64) error {
	e.mu.Lock()
	e.paused[runID] = true
	workCancels := make([]context.CancelCauseFunc, 0, 2)
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
	for _, c := range workCancels {
		c(ErrPausedByUser)
	}
	e.StopTask(runID, ErrPausedByUser)
	return nil
}

// Resume 恢复任务（重建循环）。
func (e *Engine) Resume(ctx context.Context, runID int64) error {
	e.mu.Lock()
	delete(e.paused, runID)
	e.mu.Unlock()
	if err := e.Store.SetTaskPaused(ctx, runID, false); err != nil {
		return err
	}
	if _, err := e.Store.CASRunTaskStatus(ctx, runID, []string{model.TaskPaused}, model.TaskRunning); err != nil {
		return err
	}
	return e.Start(ctx, runID)
}

// KillWork 杀掉一条在跑的意图（planner 的 kill_work）。
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

// SteerWork 给一条在跑的意图排一条纠偏消息（planner 的 steer_work）。
//
// 只有该意图**确实在跑**时才接受：否则消息会永远等不到人取（意图已经结束）。
func (e *Engine) SteerWork(intentID int64, message string) bool {
	if strings.TrimSpace(message) == "" {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.workCancels[intentID]; !ok {
		return false
	}
	e.steerBox[intentID] = append(e.steerBox[intentID], message)
	return true
}

// drainSteer 取走该意图的下一批纠偏消息（worker 在每次工具调用前调用）。
func (e *Engine) drainSteer(intentID int64) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	q := e.steerBox[intentID]
	if len(q) == 0 {
		return "", false
	}
	msg := q[0]
	if len(q) == 1 {
		delete(e.steerBox, intentID)
	} else {
		e.steerBox[intentID] = q[1:]
	}
	return msg, true
}

// Restore 启动恢复：把崩溃残留的 running 打回 open，并给非终态任务重建循环。
func (e *Engine) Restore(ctx context.Context) (int64, error) {
	// 先把"删了一半"的暂存目录收尾（DB 已删 → 真删；DB 还在 → 还原）。
	e.recoverStagedRemovals(ctx)

	reset, err := e.Store.ResetRunningIntents(ctx)
	if err != nil {
		return 0, err
	}
	// 崩溃残留的审批/暂停标记也要复位：这里只复位"活着的循环"该知道的部分。
	runs, err := e.Store.ListRuns(ctx, 500)
	if err != nil {
		return reset, err
	}
	started := 0
	for _, r := range runs {
		if r.Task == nil || model.IsTerminalTaskState(r.Task.Status) || r.Task.Queued {
			continue
		}
		if r.Task.Paused {
			e.mu.Lock()
			e.paused[r.ID] = true
			e.mu.Unlock()
			continue
		}
		if err := e.Run(ctx, r.ID); err != nil {
			e.Log.Error("[engine] 恢复任务失败", "run", r.ID, "err", err)
			continue
		}
		started++
	}
	// 队列里的任务不会自己启动：恢复完立刻按 FIFO 补位一次。
	e.Reconcile(ctx)
	e.Log.Info("[engine] 启动恢复完成", "reset_running_intents", reset, "restarted_runs", started)
	return reset, nil
}

// ---------- planner 循环 ----------

func (e *Engine) plannerLoop(ctx context.Context, runID int64) {
	interval := e.heartbeatInterval(ctx, runID)
	timer := time.NewTimer(interval)
	defer timer.Stop()

	stallReplans, stallFallbacks := 0, 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-e.notifyChan(runID):
			if !e.debounce(ctx, runID) {
				return
			}
			// 图变了（worker 结束 / 人工操作）：停摆计数重来。
			stallReplans, stallFallbacks = 0, 0
		case <-timer.C:
		}
		if !e.planRound(ctx, runID, false) {
			return
		}

		// 停摆：目标未达成、却没有任何 open/running 意图。
		//
		// prompt 要求这种情况下必须产出意图，但模型仍可能产出 0（run #41 第 9 轮）：
		// 旧实现会干等一个心跳（600s），10 分钟什么都不做。现在改成有界重规划：
		// 立即重试几次 → 短周期巡检几次 → 才退回心跳，并留下 system 记录。
		wait, nextReplans, nextFallbacks := nextStallWait(
			e.isStalled(ctx, runID), stallReplans, stallFallbacks, interval)
		if wait < 0 { // 状态变了：正常等心跳
			stallReplans, stallFallbacks = nextReplans, nextFallbacks
			wait = interval
		} else {
			switch {
			case nextReplans > stallReplans:
				e.emitStall(ctx, runID, "探索停摆（没有 open/running 意图）：立即重新规划",
					map[string]any{"mode": "immediate", "attempt": nextReplans})
			case nextFallbacks > stallFallbacks:
				e.emitStall(ctx, runID,
					fmt.Sprintf("停摆仍在继续：改为 %s 巡检（第 %d/%d 次）", wait, nextFallbacks, maxStallFallbacks),
					map[string]any{"mode": "patrol", "attempt": nextFallbacks, "wait_seconds": int(wait.Seconds())})
			case nextReplans == maxStallReplans && nextFallbacks == maxStallFallbacks:
				e.emitStall(ctx, runID,
					"停摆仍未恢复：退回心跳巡检 —— 连续 0 意图，说明需要人工介入或改 prompt",
					map[string]any{"mode": "heartbeat", "heartbeat_seconds": int(interval.Seconds())})
			}
			stallReplans, stallFallbacks = nextReplans, nextFallbacks
		}
		resetTimer(timer, wait)
	}
}

// 停摆策略参数（docs/21）。
//
// 注意 maxStallReplans 是"连续"次数：任何一次产出意图、来了 notify、或图变了都会归零，
// 所以正常任务不会因为偶尔一轮 0 意图就多花钱。
const (
	maxStallReplans     = 2
	maxStallFallbacks   = 3
	stallReplanBackoff  = 800 * time.Millisecond
	stallPatrolInterval = 2 * time.Minute
)

// nextStallWait 决定"这一轮之后等多久、计数怎么走"。
//
// 返回值 wait < 0 表示"没有停摆"（调用方用正常心跳间隔）。
// 抽成纯函数是为了能单测（不需要起 DB/引擎）。
func nextStallWait(stalled bool, replans, fallbacks int, interval time.Duration) (wait time.Duration, nextReplans, nextFallbacks int) {
	if !stalled {
		return -1, 0, 0
	}
	if replans < maxStallReplans {
		return stallReplanBackoff, replans + 1, fallbacks
	}
	if fallbacks < maxStallFallbacks {
		patrol := stallPatrolInterval
		if patrol > interval {
			patrol = interval
		}
		return patrol, replans, fallbacks + 1
	}
	return interval, replans, fallbacks
}

// isStalled 判断"目标未达成 + 没有任何 open/running 意图"这个死锁状态。
// 暂停/排队/终态、或目标已全部达成，都不算停摆。
func (e *Engine) isStalled(ctx context.Context, runID int64) bool {
	run, err := e.Store.Run(ctx, runID)
	if err != nil || run.Task == nil {
		return false
	}
	if model.IsTerminalTaskState(run.Task.Status) || run.Task.Paused || run.Task.Queued {
		return false
	}
	open, err := e.Store.HasOpenGoal(ctx, runID)
	if err != nil || !open {
		return false
	}
	active, err := e.Store.HasActiveIntent(ctx, runID)
	if err != nil {
		return false
	}
	return !active
}

func (e *Engine) emitStall(ctx context.Context, runID int64, summary string, meta map[string]any) {
	e.emit(context.WithoutCancel(ctx), &model.Activity{
		RunID: runID, Worker: "system", Kind: model.ActSystem,
		Summary: summary, Metadata: meta,
	})
	e.Log.Warn("[planner] "+summary, "run", runID)
}

// resetTimer 重置定时器（先停再清空已触发的通道，避免旧 tick 抢跑）。
func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

func (e *Engine) heartbeatInterval(ctx context.Context, runID int64) time.Duration {
	run, err := e.Store.Run(ctx, runID)
	d := e.Cfg.HeartbeatFloor
	if err == nil && run.Task != nil && run.Task.HeartbeatSeconds > 0 {
		d = time.Duration(run.Task.HeartbeatSeconds) * time.Second
	}
	if d < e.Cfg.HeartbeatFloor {
		d = e.Cfg.HeartbeatFloor
	}
	return d
}

// debounce 把一串突发变更合并成一轮。
func (e *Engine) debounce(ctx context.Context, runID int64) bool {
	ch := e.notifyChan(runID)
	timer := time.NewTimer(e.Cfg.Debounce)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-ch:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(e.Cfg.Debounce)
		case <-timer.C:
			return true
		}
	}
}

// planRound 跑一轮规划。返回 false 表示循环应当结束。
func (e *Engine) planRound(ctx context.Context, runID int64, final bool) bool {
	// ── 六道闸门（顺序固定）──
	if e.Providers == nil {
		return false // 闸门 1：LLM 未就绪
	}
	if e.isPaused(runID) {
		return false // 闸门 2
	}
	if e.isDeleting(runID) {
		return false // 闸门 3：正在删除
	}
	if e.isSettling(runID) && !final {
		return false // 闸门 5（收尾中；终局轮由协调器直接调）
	}

	run, err := e.Store.Run(ctx, runID)
	if err != nil {
		e.Log.Error("[planner] 读任务失败", "run", runID, "err", err)
		return false
	}
	if run.Task == nil || model.IsTerminalTaskState(run.Task.Status) {
		return false // 闸门 4
	}

	// 第 0 轮：目标拆解（只跑一次）。
	// 必须排在 gate 6（goalless）之前：拆解还没跑时 goal 是 CreateRun 种的 seed 目标，
	// 如果先判 goalless 就会把"还没拆解"误判成"目标都已达成"。
	if !final && !e.ensureGoals(ctx, runID, run) {
		return false
	}

	if !final {
		open, err := e.Store.HasOpenGoal(ctx, runID)
		if err != nil {
			e.Log.Error("[planner] 读目标失败", "run", runID, "err", err)
			return false
		}
		if !open { // 闸门 6：goalless
			e.drainTriggers(runID)
			active, err := e.Store.HasActiveIntent(ctx, runID)
			if err != nil {
				return false
			}
			if !active {
				if ok, err := e.Store.CASRunTaskStatus(ctx, runID, []string{model.TaskRunning, model.TaskCreated, model.TaskPaused}, model.TaskDone); err == nil && ok {
					e.emit(context.WithoutCancel(ctx), &model.Activity{
						RunID: runID, Worker: "system", Kind: model.ActSystem,
						Summary: "目标已达成，直投意图已执行完毕",
					})
				}
				// 任务已终态：必须同时停掉 worker 循环，否则它们会一直空转。
				e.shutdownRuntime(runID, ErrGoalMet)
				return false
			}
			return true
		}
	}

	if !e.beginInflightOp(runID) {
		return false
	}
	defer e.endInflightOp(runID)

	// P2-5：这一轮的时间线。子 span（LLM 调用 / 工具调用）自动挂在它下面。
	ctx = trace.NewSession(ctx, runID, e.emit)
	ctx = trace.WithWorker(ctx, "planner", 0)
	ctx, sp := trace.Start(ctx, "planner.round", map[string]any{"final": final})
	defer sp.End()

	if err := e.Store.StampFirstRun(ctx, runID, run.Task.TimeoutSeconds); err != nil {
		e.Log.Warn("[planner] 盖 first_run 失败", "run", runID, "err", err)
	}
	_ = e.Store.TouchRun(ctx, runID)

	round, _ := e.nextRound(ctx, runID)
	e.emit(ctx, &model.Activity{
		RunID: runID, Worker: "planner", Kind: model.ActRound,
		Summary: fmt.Sprintf("第 %d 轮规划", round),
	})

	triggers := e.drainTriggers(runID)
	sp.Set("triggers", len(triggers))
	sp.Set("round", round)
	e.beginLLMCall(runID)
	res, err := e.planner.PlanRound(ctx, runID, triggers, agent.PlanOptions{Final: final}, e.KillWork, e.emit)
	e.endLLMCall(runID)

	if err != nil {
		sp.Set("error", err.Error())
		if ctx.Err() == nil {
			e.Log.Error("[planner] 规划出错", "run", runID, "err", err)
		}
		return ctx.Err() == nil
	}
	sp.Set("intents", res.Intents)
	sp.Set("tool_calls", res.ToolCalls)
	sp.Set("met", res.Met)
	sp.Set("situation", res.Situation)
	sp.Set("changes", res.Changes)
	sp.Set("prompt_bytes", res.PromptBytes)
	e.Log.Info("[planner] 本轮结果", "run", runID, "triggers", len(triggers),
		"intents", res.Intents, "tool_calls", res.ToolCalls, "met", res.Met,
		"situation", res.Situation, "changes", res.Changes, "prompt_bytes", res.PromptBytes)
	if res.Met {
		if ok, err := e.Store.CASRunTaskStatus(ctx, runID, []string{model.TaskRunning, model.TaskCreated, model.TaskPaused}, model.TaskDone); err == nil && ok {
			e.emit(context.WithoutCancel(ctx), &model.Activity{
				RunID: runID, Worker: "system", Kind: model.ActSystem,
				Summary: "所有目标已达成，任务完成",
			})
		}
		e.shutdownRuntime(runID, ErrGoalMet)
		return false
	}
	_ = e.Store.TouchRun(ctx, runID)
	return true
}

func (e *Engine) nextRound(ctx context.Context, runID int64) (int64, error) {
	var round int64
	err := e.Store.Pool().QueryRow(ctx,
		`UPDATE runs SET round_no = round_no + 1, updated_at=now() WHERE id=$1 RETURNING round_no`,
		runID).Scan(&round)
	return round, err
}

// ensureGoals 跑第 0 轮目标拆解（只跑一次）。返回 false 表示循环该退出（ctx 取消/暂停）。
//
// 失败语义：拆解报错**不写** goals_at，下一轮会重试；此期间 CreateRun 种下的 seed
// 目标仍在，planner 照常工作 —— 拆解是增强，不是前置条件。
func (e *Engine) ensureGoals(ctx context.Context, runID int64, run *model.Run) bool {
	if run.Task == nil || run.Task.GoalsAt > 0 || e.goals == nil {
		return true
	}
	if !e.beginInflightOp(runID) {
		return false
	}
	defer e.endInflightOp(runID)

	if err := e.Store.StampFirstRun(ctx, runID, run.Task.TimeoutSeconds); err != nil {
		e.Log.Warn("[goals] 盖 first_run 失败", "run", runID, "err", err)
	}
	_ = e.Store.TouchRun(ctx, runID)
	e.emit(ctx, &model.Activity{
		RunID: runID, Worker: "goals", Kind: model.ActRound,
		Summary: "第 0 轮目标拆解",
	})

	ctx = trace.NewSession(ctx, runID, e.emit)
	ctx = trace.WithWorker(ctx, "goals", 0)
	ctx, sp := trace.Start(ctx, "goals.round", nil)
	defer sp.End()

	e.beginLLMCall(runID)
	res, err := e.goals.Decompose(ctx, runID, e.emit)
	e.endLLMCall(runID)
	if err != nil {
		sp.Set("error", err.Error())
		if ctx.Err() != nil {
			return false
		}
		e.Log.Error("[goals] 目标拆解失败（沿用 seed 目标，下一轮重试）", "run", runID, "err", err)
		e.emit(context.WithoutCancel(ctx), &model.Activity{
			RunID: runID, Worker: "system", Kind: model.ActSystem,
			Summary: "目标拆解失败，先沿用原始目标继续（下一轮会重试）",
			Detail:  err.Error(),
		})
		return true
	}
	sp.Set("goals", res.Goals)
	sp.Set("constraints", res.Constraints)
	sp.Set("scopes", res.Scopes)

	ok, err := e.Store.MarkGoalsDecomposed(ctx, runID)
	if err != nil {
		e.Log.Warn("[goals] 写拆解标记失败（下轮会重复拆解）", "run", runID, "err", err)
	} else if !ok {
		e.Log.Info("[goals] 拆解标记已被别的路径写入，跳过", "run", runID)
	}
	e.emit(context.WithoutCancel(ctx), &model.Activity{
		RunID: runID, Worker: "system", Kind: model.ActSystem,
		Summary: fmt.Sprintf("目标拆解完成：%d 条目标、%d 条约束、%d 条范围",
			res.Goals, res.Constraints, res.Scopes),
		Metadata: map[string]any{
			"goals": res.Goals, "constraints": res.Constraints, "scopes": res.Scopes,
		},
	})
	e.Log.Info("[goals] 目标拆解完成", "run", runID, "goals", res.Goals,
		"constraints", res.Constraints, "scopes", res.Scopes)
	_ = e.Store.TouchRun(ctx, runID)
	return true
}

// ---------- worker 循环 ----------

func (e *Engine) workerLoop(ctx context.Context, runID int64, name string) {
	for {
		if ctx.Err() != nil {
			return
		}
		claimed, err := e.Work(ctx, runID, name)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			e.Log.Error("[worker] 执行异常", "run", runID, "worker", name, "err", err)
		}
		if claimed {
			continue
		}
		// 没活干就等，**不要因为"此刻没有 open 意图"退出**：
		// planner 可能正在产出意图，提前退出会让任务永久停摆。
		select {
		case <-ctx.Done():
			return
		case <-time.After(e.Cfg.IdleSleep):
		}
	}
}

// Work 执行一次"领取 + 执行 + 落终态"。返回是否领到了活。
func (e *Engine) Work(ctx context.Context, runID int64, owner string) (bool, error) {
	// P2-5：一次领取 = 一条时间线的根（intent.execute）；worker 的 LLM/工具 span 挂在它下面。
	ctx = trace.NewSession(ctx, runID, e.emit)
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
	if e.isDeleting(runID) {
		return false, nil // 删除屏障：不再领活
	}
	if model.IsTerminalTaskState(run.Task.Status) || e.isSettling(runID) {
		return false, nil
	}

	intent, err := e.claimNext(ctx, runID, owner)
	if err != nil {
		return false, err
	}
	if intent == nil {
		return false, nil
	}

	if err := e.Store.StampFirstRun(ctx, runID, run.Task.TimeoutSeconds); err != nil {
		e.Log.Warn("stamp first_run 失败", "run", runID, "err", err)
	}
	// 只允许从 created/queued 进 running：**不能把 paused 也算进来**，
	// 否则会和一次真实的暂停赛跑，把刚设上的 paused 又覆盖成 running。
	_, _ = e.Store.CASRunTaskStatus(ctx, runID, []string{model.TaskCreated, "queued"}, model.TaskRunning)

	e.beginInflight(runID)
	defer e.endInflight(runID)

	// 意图级 span：worker 的 llm.call / tool.call 都挂在它下面，
	// 于是"第 3 轮 → 意图 #42 → 第 2 次工具调用"能从时间线直接读出来。
	ctx = trace.WithWorker(ctx, owner, intent.ID)
	ctx, sp := trace.Start(ctx, "intent.execute", map[string]any{
		"intent_id": intent.ID, "owner": owner, "priority": intent.Priority,
	})
	defer sp.End()

	workCtx, cancel := context.WithCancelCause(ctx)
	e.registerWork(intent.ID, runID, cancel)
	defer e.unregisterWork(intent.ID)
	// 领取 → 注册之间有一个窗口，Pause 可能正好落在这里：注册后再查一次，
	// 否则这次暂停会被"漏掉"，意图会一直跑到自然结束。
	if e.isPaused(runID) {
		cancel(ErrPausedByUser)
	}

	w := e.worker()
	w.Steer = func() (string, bool) { return e.drainSteer(intent.ID) }
	out, execErr := w.Execute(workCtx, runID, intent.ID, owner, e.emit)
	cause := context.Cause(workCtx)
	cancel(nil)

	if execErr != nil && out == nil {
		if workCtx.Err() != nil || errors.Is(execErr, context.Canceled) || errors.Is(execErr, context.DeadlineExceeded) {
			out = &agent.Outcome{Reason: agent.ReasonCanceled}
		} else {
			out = &agent.Outcome{Reason: agent.ReasonError, Err: execErr}
		}
	}
	state := e.settle(ctx, runID, intent.ID, out, cause)
	sp.Set("state", state)
	sp.Set("reason", out.Reason)
	sp.Set("turns", out.Turns)
	sp.Set("facts", out.Facts)
	sp.Set("findings", out.Findings)
	sp.Set("assets", out.Assets)
	if out.Err != nil {
		sp.Set("error", out.Err.Error())
	}
	e.emit(context.WithoutCancel(ctx), &model.Activity{
		RunID: runID, NodeID: &intent.ID, Worker: owner, Kind: model.ActSystem,
		Summary: fmt.Sprintf("意图 #%d 结束：%s（%s）", intent.ID, state, out.Reason),
		Metadata: map[string]any{
			"intent_id": intent.ID, "state": state, "reason": out.Reason,
			"turns": out.Turns, "facts": out.Facts, "findings": out.Findings,
		},
	})
	e.Log.Info("[worker] 意图结束", "run", runID, "intent", intent.ID, "state", state, "reason", out.Reason, "turns", out.Turns)

	// 结果改变了图 → 唤醒 planner（带上"为什么"）。
	if out.Findings > 0 {
		e.addTrigger(runID, agent.Trigger{Kind: "finding", IntentID: intent.ID, Summary: intent.Summary(), NodeID: intent.ID})
	}
	e.addTrigger(runID, agent.Trigger{
		Kind: "done", IntentID: intent.ID, Summary: intent.Summary(),
		Text: out.Summary,
	})
	e.Notify(runID)
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

// ---------- 超时收尾 ----------

func (e *Engine) deadlineLoop(ctx context.Context, runID int64) {
	for {
		run, err := e.Store.Run(ctx, runID)
		if err != nil || run.Task == nil || model.IsTerminalTaskState(run.Task.Status) {
			return
		}
		if run.Task.FirstRunAt == 0 || run.Task.DeadlineAt == 0 {
			if !sleepCtx(ctx, 2*time.Second) {
				return
			}
			continue
		}
		remain := time.Until(time.Unix(run.Task.DeadlineAt, 0))
		if remain > 0 {
			if remain > 30*time.Second {
				remain = 30 * time.Second
			}
			if !sleepCtx(ctx, remain) {
				return
			}
			continue
		}
		e.settleTask(ctx, runID)
		return
	}
}

// settleTask 是超时收尾时序：停止派/领 → 等在跑的收尾 → 硬兜底 → 终局规划 → 落 timeout。
func (e *Engine) settleTask(ctx context.Context, runID int64) {
	e.mu.Lock()
	if e.settling[runID] {
		e.mu.Unlock()
		return
	}
	e.settling[runID] = true
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.settling, runID)
		e.mu.Unlock()
	}()

	ctx = trace.NewSession(ctx, runID, e.emit)
	ctx = trace.WithWorker(ctx, "system", 0)
	ctx, sp := trace.Start(ctx, "task.settle", nil)
	defer sp.End()

	e.emit(context.WithoutCancel(ctx), &model.Activity{
		RunID: runID, Worker: "system", Kind: model.ActSystem,
		Summary: "任务时间到，进入收尾",
	})

	// 1) 让在跑的 worker 自己收尾（settlement 会把结果写回）。
	deadline := time.Now().Add(e.Cfg.SettleDrainGrace)
	for time.Now().Before(deadline) {
		if e.Inflight(runID) == 0 {
			break
		}
		if !sleepCtx(ctx, 10*time.Millisecond) {
			break
		}
	}
	// 2) 硬兜底：还没停的直接 cancel（意图会归 exhausted）。
	sp.Set("inflight_at_stop", e.Inflight(runID))
	e.StopTask(runID, ErrTimeoutSettling)
	// StopTask 已经 cancel 了 rctx；后面几步必须用"不随取消失效"的 ctx，
	// 否则收尾会停在半路，任务永远落不了 timeout。
	baseCtx := context.WithoutCancel(ctx)
	if !sleepCtx(baseCtx, 3*time.Second) {
		return
	}

	// 3) 终局规划：独立 ctx（暂停/硬 cancel 都打不断它），只判定目标、不派新意图。
	finalCtx, cancel := context.WithTimeout(context.Background(), e.Cfg.FinalPlanTimeout)
	if _, err := e.planner.Plan(finalCtx, runID, nil, true, e.KillWork, e.emit); err != nil {
		e.Log.Warn("[planner] 终局判定失败", "run", runID, "err", err)
	}
	cancel()

	// 4) 落终态（CAS：只写入者赢）。
	ok, err := e.Store.CASRunTaskStatus(baseCtx, runID, []string{model.TaskRunning, model.TaskCreated, model.TaskPaused}, model.TaskTimeout)
	if err != nil || !ok {
		e.Log.Warn("[engine] 落 timeout 失败或冲突", "run", runID, "ok", ok, "err", err)
	}
	e.emit(baseCtx, &model.Activity{
		RunID: runID, Worker: "system", Kind: model.ActSystem,
		Summary: "任务超时收尾完成",
	})
}

// ---------- 收尾分流 ----------

// settle 是九分支收尾分流的唯一实现。顺序不能改。
func (e *Engine) settle(ctx context.Context, runID, intentID int64, out *agent.Outcome, cause error) string {
	// 收尾必须用"不随取消而失效"的 ctx：暂停/超时正是 cancel 之后才收尾。
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	run, err := e.Store.Run(writeCtx, runID)
	if err != nil {
		e.Log.Error("收尾时读 run 失败", "run", runID, "err", err)
	}

	next := ""
	switch {
	case errors.Is(cause, ErrPausedByUser):
		next = model.StateOpen
	case errors.Is(cause, ErrCancelledByUser):
		next = model.StatePaused
	case errors.Is(cause, ErrKilledByPlanner):
		next = model.StateStopped
	case errors.Is(cause, ErrTimeoutSettling):
		next = model.StateExhausted
	case errors.Is(cause, ErrGoalMet):
		next = model.StateStopped
	case errors.Is(cause, ErrShutdown):
		next = model.StateOpen
	case out.Reason == agent.ReasonCanceled && run != nil && run.Task != nil && run.Task.Paused:
		next = model.StateOpen
	case run != nil && run.Task != nil && model.IsTerminalTaskState(run.Task.Status):
		next = model.StateStopped
	case out.Err != nil:
		// 一次"网关抖了"不该让整条方向永久 blocked：worker 什么都没写回时，
		// 把它放回队列重试一次；第二次仍失败才 blocked。
		// "只有推理没有正文"（llm.ErrReasoningOnly）不是抖动，不会走到这里。
		if out.Facts == 0 && out.Findings == 0 && isTransientLLMError(out.Err) &&
			e.transientRetryAllowed(writeCtx, runID, intentID) {
			next = model.StateOpen
			e.emit(writeCtx, &model.Activity{
				RunID: runID, NodeID: &intentID, Worker: "system", Kind: model.ActSystem,
				Summary: "模型/网络偶发失败（尚未写回任何事实）：意图放回队列重试一次",
				Detail:  out.Err.Error(),
			})
		} else {
			next = model.StateBlocked
		}
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

// transientRetryLimit 一条意图最多因"偶发模型/网络失败"重排队几次。
const transientRetryLimit = 1

// isTransientLLMError 判断是不是"重跑一次大概率能过"的失败。
// ErrReasoningOnly 不是 ErrTransient（推理吃光输出预算，重放必然再撞），所以不会命中。
func isTransientLLMError(err error) bool {
	return errors.Is(err, llm.ErrTransient) || errors.Is(err, llm.ErrRateLimited)
}

// transientRetryAllowed 读这条意图历史失败次数（含本次）判断还能不能再试一次。
func (e *Engine) transientRetryAllowed(ctx context.Context, runID, intentID int64) bool {
	n, err := e.Store.CountIntentTransientFailures(ctx, runID, intentID)
	if err != nil {
		e.Log.Warn("读偶发失败次数失败（按不放行处理）", "run", runID, "intent", intentID, "err", err)
		return false
	}
	return n <= transientRetryLimit
}

// ---------- 通知与触发 ----------

// Notify 唤醒 planner：通道容量 1，故意丢信号（"有变化"不需要计数）。
func (e *Engine) Notify(runID int64) {
	ch := e.notifyChan(runID)
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (e *Engine) notifyChan(runID int64) chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	ch, ok := e.notifies[runID]
	if !ok {
		ch = make(chan struct{}, 1)
		e.notifies[runID] = ch
	}
	return ch
}

func (e *Engine) addTrigger(runID int64, t agent.Trigger) {
	e.mu.Lock()
	e.triggers[runID] = append(e.triggers[runID], t)
	e.mu.Unlock()
}

// AddTrigger 供 API/人工路径注入（目标变更、提示、删除意图）。
func (e *Engine) AddTrigger(runID int64, t agent.Trigger) {
	e.addTrigger(runID, t)
	e.Notify(runID)
}

func (e *Engine) drainTriggers(runID int64) []agent.Trigger {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := e.triggers[runID]
	e.triggers[runID] = nil
	return out
}

// ---------- 内部工具 ----------

func (e *Engine) isPaused(runID int64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.paused[runID]
}

func (e *Engine) isSettling(runID int64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.settling[runID]
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
	delete(e.steerBox, intentID) // 意图已经结束：留着的纠偏消息再也没人取
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

// beginInflightOp / endInflightOp 是 planner 轮次的操作登记（与 worker 共用计数）。
func (e *Engine) beginInflightOp(runID int64) bool {
	if e.isSettling(runID) {
		return false
	}
	e.beginInflight(runID)
	return true
}

func (e *Engine) endInflightOp(runID int64) { e.endInflight(runID) }

func (e *Engine) beginLLMCall(runID int64) {
	e.mu.Lock()
	e.activeLLM[runID]++
	e.mu.Unlock()
}

func (e *Engine) endLLMCall(runID int64) {
	e.mu.Lock()
	if e.activeLLM[runID] > 0 {
		e.activeLLM[runID]--
	}
	e.mu.Unlock()
}

// ActiveLLMCalls 给派生状态用（"有活但没在调模型" vs "正在调模型"）。
func (e *Engine) ActiveLLMCalls(runID int64) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.activeLLM[runID]
}

// emit 是单一发射点：落库（带重试）成功后才算数；失败必须打日志，不能静默。
func (e *Engine) emit(ctx context.Context, a *model.Activity) {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	for attempt := 1; attempt <= 3; attempt++ {
		id, err := e.Store.AppendActivity(writeCtx, a)
		if err == nil {
			a.ID = id
			// 推给 SSE 的是内存对象：created_at 由 DB 填，这里不补就是零值，
			// 前端会把公元 1 年显示出来（上海 LMT 恰好是 08:05:43）。
			if a.CreatedAt.IsZero() {
				a.CreatedAt = time.Now()
			}
			if e.Publish != nil {
				e.Publish(a)
			}
			return
		}
		e.Log.Warn("[activity] 写入失败", "run", a.RunID, "kind", a.Kind, "tool", a.Tool,
			"attempt", attempt, "err", err)
		time.Sleep(time.Duration(attempt) * 25 * time.Millisecond)
	}
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

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
