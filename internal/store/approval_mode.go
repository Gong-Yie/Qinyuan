package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"qinyuan/internal/model"
)

// SetRunApprovalMode 改一个 run 的审批模式；空 = 跟随全局。
func (s *Store) SetRunApprovalMode(ctx context.Context, runID int64, mode string) (bool, error) {
	m, ok := model.NormalizeApprovalMode(mode)
	if !ok {
		return false, fmt.Errorf("approval_mode 只能是 manual/auto/off/inherit")
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE run_tasks SET approval_mode=$2, updated_at=now() WHERE run_id=$1`, runID, m)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// RunApprovalMode 读 run 级模式；返回空表示"跟随全局"。
func (s *Store) RunApprovalMode(ctx context.Context, runID int64) (string, error) {
	var mode string
	err := s.pool.QueryRow(ctx, `SELECT approval_mode FROM run_tasks WHERE run_id=$1`, runID).Scan(&mode)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return mode, err
}

// RecordAutoApproval 在 auto 模式下给"本来要问人"的调用留一条已批准的审计行。
//
// 它不阻塞调用：写失败只意味着少一条审计，不能让自动化停下。
func (s *Store) RecordAutoApproval(ctx context.Context, runID, intentID int64,
	tool, toolUseID, input, reason string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO intercept_pending
			(run_id, intent_id, tool, tool_use_id, tool_input, reason, status, decided_by, decided_at)
		VALUES ($1, NULLIF($2,0), $3, $4, $5::jsonb, $6, 'allowed', 'auto', now())`,
		runID, intentID, tool, toolUseID, input, reason)
	return err
}

// ApprovePendingForRun 把一个 run 当前所有 pending 审批一次性放行。
// 切换 auto/off 时用它把"已经卡在等待"的工具调用解出来（Gate 轮询 DB 会看到 allowed）。
func (s *Store) ApprovePendingForRun(ctx context.Context, runID int64, by, reason string) (int, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE intercept_pending
		   SET status='allowed', decided_by=$2, decided_at=now(),
		       reason = CASE WHEN $3 <> '' THEN reason || ' | ' || $3 ELSE reason END
		 WHERE run_id=$1 AND status='pending'`, runID, by, reason)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}
