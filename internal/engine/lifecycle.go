package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"qinyuan/internal/agent"
	"qinyuan/internal/model"
	"qinyuan/internal/store"
)

// 本文件是任务生命周期的两件"重活"（M4b）：
//
//	删除：屏障 → 排水 → 文件暂存（可逆）→ DB 级联删 → 提交后真删
//	准入：并发上限 → 持久化 FIFO 队列 → 槽位释放后按 queued_at 补位
//
// 两条都遵守同一句话：**DB 是权威，外部资源先做可逆动作**。

// ErrDeleted 是删除时用的具名取消原因（不能和"暂停"混：两者落的状态不同）。
var ErrDeleted = errors.New("task_deleted")

// ---------- 删除 ----------

// BeginDelete 装删除屏障：此后拒绝新写入、取消在跑的执行。
// 返回 false 表示这个任务已经在删除中（重复请求幂等返回）。
func (e *Engine) BeginDelete(runID int64) bool {
	e.mu.Lock()
	if e.deleting[runID] {
		e.mu.Unlock()
		return false
	}
	e.deleting[runID] = true
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

	for _, c := range workCancels {
		c(ErrDeleted)
	}
	return true
}

// isDeleting 供 Run/Work/planRound 读（删除中不接受任何新活）。
func (e *Engine) isDeleting(runID int64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.deleting[runID]
}

// abortDelete 撤销屏障（DB 删除失败时用）。任务保持 paused：宁可让用户手动恢复，
// 也不要在"删了一半"之后偷偷继续跑。
func (e *Engine) abortDelete(runID int64) {
	e.mu.Lock()
	delete(e.deleting, runID)
	e.mu.Unlock()
}

// Delete 删掉一个 run：屏障 → 排水 → 文件暂存 → DB 级联删 → 提交后真删。
//
// 失败语义（照抄实现规格 02 §12）：
//   - DB 失败 → 逆序还原暂存文件，任务保持暂停；
//   - DB 成功、文件清理失败 → **仍然算删除成功**（返回 warning），状态不能回退。
func (e *Engine) Delete(ctx context.Context, runID int64) error {
	if !e.BeginDelete(runID) {
		return nil // 已经在删：幂等
	}

	// 1) 停循环（有上限：不要因为一个卡死的工具把删除挂住）
	stopped := make(chan struct{})
	go func() {
		e.StopTask(runID, ErrDeleted)
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(e.Cfg.DeleteDrainTimeout):
		e.Log.Warn("[delete] 等循环退出超时，继续删除（屏障已挡住新写入）", "run", runID)
	}

	// 2) 等静默：inflight 是"正在跑的 Plan/Execute"计数
	if !e.waitQuiescent(ctx, runID, e.Cfg.DeleteDrainTimeout) {
		e.Log.Warn("[delete] 等在跑操作静默超时，继续删除", "run", runID)
	}

	// 3) 外部资源先做可逆动作：目录 rename 到暂存区
	staged, err := stageRunDir(e.Cfg.DataDir, runID)
	if err != nil {
		e.abortDelete(runID)
		return fmt.Errorf("暂存任务文件失败：%w", err)
	}

	// 4) DB 是权威
	if err := e.Store.DeleteRunCascade(ctx, runID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// 已经不存在（重复删除/别的进程先删了）：视为成功，幂等。
			if staged != "" {
				_ = os.RemoveAll(staged)
			}
			e.forgetRun(runID)
			return nil
		}
		if staged != "" {
			_ = os.Rename(staged, filepath.Join(e.Cfg.DataDir, "runs", strconv.FormatInt(runID, 10)))
		}
		e.abortDelete(runID)
		return fmt.Errorf("删任务失败（文件已还原）：%w", err)
	}

	// 5) 提交后才做不可逆动作
	var cleanupErr error
	if staged != "" {
		if err := os.RemoveAll(staged); err != nil {
			cleanupErr = err
			e.Log.Warn("[delete] 任务已删除但暂存文件清理失败（下次启动会重试）", "run", runID, "err", err)
		}
	}
	e.forgetRun(runID)
	e.Log.Info("[delete] 任务已删除", "run", runID)
	go e.Reconcile(context.WithoutCancel(ctx))

	if cleanupErr != nil {
		return fmt.Errorf("任务已删除，但文件清理未完成：%w", cleanupErr)
	}
	return nil
}

// waitQuiescent 轮询等在跑操作归零（10ms 粒度，上限由调用方给）。
func (e *Engine) waitQuiescent(ctx context.Context, runID int64, timeout time.Duration) bool {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if e.Inflight(runID) == 0 {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(10 * time.Millisecond):
		}
	}
	return e.Inflight(runID) == 0
}

// forgetRun 清掉这个 run 的全部内存状态（delete 成功后调用）。
func (e *Engine) forgetRun(runID int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.paused, runID)
	delete(e.settling, runID)
	delete(e.deleting, runID)
	delete(e.notifies, runID)
	delete(e.triggers, runID)
	delete(e.activeLLM, runID)
	delete(e.taskCancels, runID)
	for intentID, rid := range e.workRuns {
		if rid == runID {
			delete(e.workRuns, intentID)
			delete(e.workCancels, intentID)
			delete(e.steerBox, intentID)
		}
	}
	e.inflightMu.Lock()
	delete(e.inflight, runID)
	e.inflightMu.Unlock()
}

// stageRunDir 把 data/runs/<id> rename 到 data/.deleting/<id>-<ts>（可逆）。
// 目录不存在时返回空串（没什么可删的）。
func stageRunDir(dataDir string, runID int64) (string, error) {
	if dataDir == "" {
		return "", nil
	}
	src := filepath.Join(dataDir, "runs", strconv.FormatInt(runID, 10))
	if _, err := os.Stat(src); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	stageRoot := filepath.Join(dataDir, ".deleting")
	if err := os.MkdirAll(stageRoot, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(stageRoot, fmt.Sprintf("%d-%d", runID, time.Now().UnixNano()))
	if err := os.Rename(src, dst); err != nil {
		return "", err
	}
	return dst, nil
}

// recoverStagedRemovals 启动时处理上次崩溃留下的暂存目录：
//   - DB 里已经没有这个 run → 真删（说明删除其实已经提交，只差收尾）；
//   - DB 里还在 → 还原（说明删除在提交前就崩了）。
func (e *Engine) recoverStagedRemovals(ctx context.Context) {
	stageRoot := filepath.Join(e.Cfg.DataDir, ".deleting")
	entries, err := os.ReadDir(stageRoot)
	if err != nil {
		return // 没有暂存目录：正常
	}
	for _, ent := range entries {
		name := ent.Name()
		runIDStr, _, _ := strings.Cut(name, "-")
		runID, err := strconv.ParseInt(runIDStr, 10, 64)
		if err != nil || runID <= 0 {
			continue
		}
		exists, err := e.Store.RunExists(ctx, runID)
		if err != nil {
			e.Log.Warn("[delete] 恢复暂存目录时读库失败", "dir", name, "err", err)
			continue
		}
		stage := filepath.Join(stageRoot, name)
		if !exists {
			if err := os.RemoveAll(stage); err != nil {
				e.Log.Warn("[delete] 清理暂存目录失败", "dir", name, "err", err)
			}
			continue
		}
		target := filepath.Join(e.Cfg.DataDir, "runs", runIDStr)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			e.Log.Warn("[delete] 还原暂存目录失败（建父目录）", "dir", name, "err", err)
			continue
		}
		if err := os.Rename(stage, target); err != nil {
			e.Log.Warn("[delete] 还原暂存目录失败", "dir", name, "err", err)
		}
	}
}

// ---------- 准入队列 ----------

// Start 是"启动一个任务"的唯一入口：有并发上限时先排队。
func (e *Engine) Start(ctx context.Context, runID int64) error {
	started, err := e.admit(ctx, runID)
	if err != nil {
		return err
	}
	if !started {
		e.addTrigger(runID, agent.Trigger{Kind: "queued"})
	}
	return nil
}

// admit 尝试占一个并发槽；满了就入队（持久化，重启后还能按 FIFO 补位）。
func (e *Engine) admit(ctx context.Context, runID int64) (bool, error) {
	limit := e.Cfg.MaxConcurrentRuns
	if limit <= 0 {
		return true, e.Run(ctx, runID)
	}
	active, err := e.Store.CountActiveRunsExcept(ctx, runID)
	if err != nil {
		return false, err
	}
	e.Log.Info("[admit] 判定", "run", runID, "active", active, "limit", limit)
	if active >= limit {
		if err := e.Store.EnqueueRun(ctx, runID); err != nil {
			return false, err
		}
		e.emit(context.WithoutCancel(ctx), &model.Activity{
			RunID: runID, Worker: "system", Kind: model.ActSystem,
			Summary: fmt.Sprintf("排队中：并发上限 %d，当前已有 %d 个任务在跑", limit, active),
		})
		e.Log.Info("[admit] 入队", "run", runID, "active", active, "limit", limit)
		return false, nil
	}
	if err := e.Store.DequeueRun(ctx, runID); err != nil {
		return false, err
	}
	// 占住槽位：created/queued → running。这样"占坑"是 DB 事实，重启后仍准确。
	if _, err := e.Store.CASRunTaskStatus(ctx, runID,
		[]string{model.TaskCreated, "queued"}, model.TaskRunning); err != nil {
		return false, err
	}
	return true, e.Run(ctx, runID)
}

// Reconcile 在槽位空出来时按 FIFO 补位。可被定时器或事件调用（幂等）。
func (e *Engine) Reconcile(ctx context.Context) {
	limit := e.Cfg.MaxConcurrentRuns
	if limit <= 0 {
		return
	}
	for {
		active, err := e.Store.CountActiveRuns(ctx)
		if err != nil || active >= limit {
			return
		}
		runID, ok, err := e.Store.NextQueuedRun(ctx)
		if err != nil || !ok {
			return
		}
		if err := e.Store.DequeueRun(ctx, runID); err != nil {
			return
		}
		if err := e.Run(ctx, runID); err != nil {
			e.Log.Error("[admit] 出队启动失败", "run", runID, "err", err)
			continue
		}
		e.emit(context.WithoutCancel(ctx), &model.Activity{
			RunID: runID, Worker: "system", Kind: model.ActSystem,
			Summary: "已出队启动（前面腾出了并发槽）",
		})
		e.Log.Info("[admit] 出队启动", "run", runID)
	}
}

// ReconcileLoop 每 5s 补位一次（兜底：事件漏掉时也不会永远排队）。
func (e *Engine) ReconcileLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.Reconcile(ctx)
		}
	}
}
