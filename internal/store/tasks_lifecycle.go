package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// 本文件是任务生命周期的"破坏性"与"排队"两类操作（M4b）。
//
// 删除的语义：**PG 是权威**。文件先 rename 到暂存区（可逆），DB 事务提交后才真删；
// DB 失败就逆序还原。任何"外部资源先删、DB 后改"的顺序都会在失败时留下不一致。

// DeleteRunCascade 删掉一个 run 及其任务行。
//
// nodes/edges/anchors/activity/constraints/run_scope 都是 ON DELETE CASCADE 挂在 runs 上，
// 但 run_tasks 是 RESTRICT（M1 的刻意选择：任务活着就删不掉 exploration），
// 所以必须先删任务行。
func (s *Store) DeleteRunCascade(ctx context.Context, runID int64) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if _, err := tx.Exec(ctx, `DELETE FROM run_tasks WHERE run_id = $1`, runID); err != nil {
		return fmt.Errorf("删任务行失败：%w", err)
	}
	tag, err := tx.Exec(ctx, `DELETE FROM runs WHERE id = $1`, runID)
	if err != nil {
		return fmt.Errorf("删 run 失败：%w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return tx.Commit(ctx)
}

// RunExists 判断 run 是否还在（删除恢复用）。
func (s *Store) RunExists(ctx context.Context, runID int64) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM runs WHERE id=$1)`, runID).Scan(&ok)
	return ok, err
}

// EnqueueRun 把一个 run 放进准入队列（FIFO 按 queued_at）。
//
// 重复入队**保留首次位置**（COALESCE）：否则每次 reconcil 都会把它排到队尾，
// 饿死一个反复被检查的任务。
func (s *Store) EnqueueRun(ctx context.Context, runID int64) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE run_tasks
		   SET queued = true,
		       queued_at = COALESCE(queued_at, $2),
		       updated_at = now()
		 WHERE run_id = $1`, runID, time.Now().UnixNano())
	return err
}

// DequeueRun 把 run 从队列里拿出来（真正启动时调用）。
func (s *Store) DequeueRun(ctx context.Context, runID int64) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE run_tasks SET queued = false, queued_at = NULL, updated_at = now()
		 WHERE run_id = $1`, runID)
	return err
}

// NextQueuedRun 取队首（queued_at 升序；老行没有 queued_at 时按 run_id 兜底）。
func (s *Store) NextQueuedRun(ctx context.Context) (int64, bool, error) {
	var runID int64
	err := s.pool.QueryRow(ctx, `
		SELECT run_id FROM run_tasks
		 WHERE queued = true
		   AND paused = false
		   AND status IN ('created','running')
		 ORDER BY queued_at ASC NULLS FIRST, run_id ASC
		 LIMIT 1`).Scan(&runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return runID, true, nil
}

// CountActiveRuns 统计"占并发槽"的任务 = 状态 running 的任务。
//
// 只认 running（不认 created）：一个刚建好、还没被准入启动的 run 不该占坑，
// 否则"同时建 10 个任务"会互相把对方挤进队列。启动时才把 created CAS 成 running。
func (s *Store) CountActiveRuns(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM run_tasks
		 WHERE queued = false
		   AND paused = false
		   AND status = 'running'`).Scan(&n)
	return n, err
}

// CountActiveRunsExcept 同上，但不把自己算进去（准入判定用：
// 一个还没启动的 run 本身也满足 created/running，不排除就会"自己把自己挤掉"）。
func (s *Store) CountActiveRunsExcept(ctx context.Context, runID int64) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM run_tasks
		 WHERE run_id <> $1
		   AND queued = false
		   AND paused = false
		   AND status = 'running'`, runID).Scan(&n)
	return n, err
}

// SetRunTaskStatus 直接改任务状态（删除后不再需要 CAS，但保留给恢复路径用）。
func (s *Store) SetRunTaskStatus(ctx context.Context, runID int64, status string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE run_tasks SET status = $2, updated_at = now() WHERE run_id = $1`, runID, status)
	return err
}
