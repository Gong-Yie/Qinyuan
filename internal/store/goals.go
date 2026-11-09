package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"qinyuan/internal/model"
)

// AddGoal 建一个目标节点，并从 origin fact 连 spawns（目标的血缘起点）。
func (s *Store) AddGoal(ctx context.Context, runID int64, text, vulnClass, origin string) (int64, error) {
	if text == "" {
		return 0, errors.New("目标文本不能为空")
	}
	if origin == "" {
		origin = "seed"
	}
	payload, err := json.Marshal(map[string]any{"text": text, "vulnclass": vulnClass})
	if err != nil {
		return 0, err
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	var goalID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO nodes (run_id, kind, state, payload, priority, origin)
		VALUES ($1, 'goal', 'open', $2, 0, $3) RETURNING id`,
		runID, payload, origin).Scan(&goalID); err != nil {
		return 0, fmt.Errorf("建目标失败：%w", err)
	}

	var originID int64
	err = tx.QueryRow(ctx,
		`SELECT id FROM nodes WHERE run_id=$1 AND kind='fact' AND state='origin' LIMIT 1`, runID).Scan(&originID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, errors.New("本任务没有 origin fact，拒绝创建无根目标")
	}
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO edges (run_id, src_id, rel, dst_id) VALUES ($1,$2,'spawns',$3)
		ON CONFLICT DO NOTHING`, runID, originID, goalID); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return goalID, nil
}

// ProveGoal 用一条事实/发现证明目标达成，返回剩余未达成目标数。
//
// 顺序：验目标（open）→ 验证据（fact/finding）→ 连 proves → CAS 目标 met。
// 这是唯一能让任务正常结束的路径（除 goalless 分支外）。
func (s *Store) ProveGoal(ctx context.Context, runID, goalID, evidenceID int64, reason string) (int, error) {
	if runID <= 0 || goalID <= 0 || evidenceID <= 0 {
		return 0, errors.New("run_id / goal_id / evidence_id 必填")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	var goalState, goalKind string
	err = tx.QueryRow(ctx, `SELECT kind, state FROM nodes WHERE run_id=$1 AND id=$2`, runID, goalID).
		Scan(&goalKind, &goalState)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("目标 #%d 不存在于本任务", goalID)
	}
	if err != nil {
		return 0, err
	}
	if goalKind != model.KindGoal {
		return 0, fmt.Errorf("节点 #%d 是 %q，不是目标", goalID, goalKind)
	}
	if goalState != model.StateOpen {
		return 0, fmt.Errorf("目标 #%d 当前是 %q，不能重复证明", goalID, goalState)
	}

	var evKind, evState string
	err = tx.QueryRow(ctx, `SELECT kind, state FROM nodes WHERE run_id=$1 AND id=$2`, runID, evidenceID).
		Scan(&evKind, &evState)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("证据 #%d 不存在于本任务", evidenceID)
	}
	if err != nil {
		return 0, err
	}
	if evKind != model.KindFact && evKind != model.KindFinding {
		return 0, fmt.Errorf("证据 #%d 是 %q：只能用事实或发现证明目标", evidenceID, evKind)
	}
	if evKind == model.KindFact && evState == model.StateDismissed {
		return 0, fmt.Errorf("证据 #%d 已被否定，不能用来证明目标", evidenceID)
	}
	// origin fact 是任务起点（目标 + 初始范围），不是探索产出：
	// 它证明不了任何东西，否则"任务一开始就算完成"。
	if evKind == model.KindFact && evState == model.StateOrigin {
		return 0, fmt.Errorf("证据 #%d 是任务起点（origin fact），不是探索产出的证据；请用 worker 写下的事实或发现", evidenceID)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO edges (run_id, src_id, rel, dst_id) VALUES ($1,$2,'proves',$3)
		ON CONFLICT DO NOTHING`, runID, evidenceID, goalID); err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE nodes SET state='met', completed_at=now(), updated_at=now()
		 WHERE run_id=$1 AND id=$2 AND kind='goal' AND state='open'`, runID, goalID)
	if err != nil {
		return 0, err
	}
	if tag.RowsAffected() != 1 {
		return 0, ErrStateConflict
	}
	if reason != "" {
		if _, err := tx.Exec(ctx, `
			UPDATE nodes SET payload = jsonb_set(payload, '{prove_reason}', to_jsonb($3::text), true),
			       updated_at=now() WHERE run_id=$1 AND id=$2`, runID, goalID, reason); err != nil {
			return 0, err
		}
	}

	var remaining int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM nodes WHERE run_id=$1 AND kind='goal' AND state='open'`, runID).Scan(&remaining); err != nil {
		return 0, err
	}
	if err := bumpVersion(ctx, tx, runID); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return remaining, nil
}

// Goals 列出目标。
func (s *Store) Goals(ctx context.Context, runID int64) ([]*model.Node, error) {
	return s.NodesByKind(ctx, runID, model.KindGoal, 50)
}
