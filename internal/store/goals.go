package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

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
	rev, err := bumpVersion(ctx, tx, runID)
	if err != nil {
		return 0, err
	}
	if err := stampNodes(ctx, tx, runID, rev, goalID); err != nil {
		return 0, err
	}
	if err := stampEdge(ctx, tx, runID, rev, originID, model.RelSpawns, goalID); err != nil {
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
	rev, err := bumpVersion(ctx, tx, runID)
	if err != nil {
		return 0, err
	}
	if err := stampNodes(ctx, tx, runID, rev, goalID); err != nil {
		return 0, err
	}
	if err := stampEdge(ctx, tx, runID, rev, evidenceID, model.RelProves, goalID); err != nil {
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

// AddGoalSpec 是第 0 轮拆解出来的一条目标。
type AddGoalSpec struct {
	Text      string
	VulnClass string
}

// ReplaceGoalsFromDecomposition 用拆解结果替换掉 CreateRun 种下的 seed 目标。
//
// 为什么要"替换"而不是"追加"：seed 目标就是用户的原始一句话，拆解后的目标
// 通常是它的细化；两者并存会让 planner 同时为粗目标和细目标各自 prove，
// 也会把"最后一个目标达成"的判定拖长。seed 目标被置 abandoned（不删，保留血缘）。
//
// 拆解结果为空时**什么也不做**：调用方会继续用 seed 目标兜底。
func (s *Store) ReplaceGoalsFromDecomposition(ctx context.Context, runID int64, specs []AddGoalSpec) ([]int64, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	var originID int64
	err = tx.QueryRow(ctx,
		`SELECT id FROM nodes WHERE run_id=$1 AND kind='fact' AND state='origin' LIMIT 1`, runID).Scan(&originID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errors.New("本任务没有 origin fact，拒绝创建无根目标")
	}
	if err != nil {
		return nil, err
	}

	// 先退役上一版目标（只动还 open 的，已经 met 的不去篡改）：
	//   seed  —— CreateRun 种下的原话；
	//   goals —— 上一次拆解的产物（重复拆解时不能叠加，否则目标会越滚越多）。
	if _, err := tx.Exec(ctx, `
		UPDATE nodes SET state='abandoned', updated_at=now()
		 WHERE run_id=$1 AND kind='goal' AND state='open' AND COALESCE(origin,'') IN ('seed','goals')`,
		runID); err != nil {
		return nil, err
	}

	ids := make([]int64, 0, len(specs))
	for _, sp := range specs {
		payload, err := json.Marshal(map[string]any{"text": sp.Text, "vulnclass": sp.VulnClass})
		if err != nil {
			return nil, err
		}
		var goalID int64
		if err := tx.QueryRow(ctx, `
			INSERT INTO nodes (run_id, kind, state, payload, priority, origin)
			VALUES ($1,'goal','open',$2,0,'goals') RETURNING id`,
			runID, payload).Scan(&goalID); err != nil {
			return nil, fmt.Errorf("建目标失败：%w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO edges (run_id, src_id, rel, dst_id) VALUES ($1,$2,'spawns',$3)
			ON CONFLICT DO NOTHING`, runID, originID, goalID); err != nil {
			return nil, err
		}
		ids = append(ids, goalID)
	}

	rev, err := bumpVersion(ctx, tx, runID)
	if err != nil {
		return nil, err
	}
	if err := stampNodes(ctx, tx, runID, rev, ids...); err != nil {
		return nil, err
	}
	for _, id := range ids {
		if err := stampEdge(ctx, tx, runID, rev, originID, model.RelSpawns, id); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return ids, nil
}

// MarkGoalsDecomposed 记录"第 0 轮拆解已跑过"。
//
// 只在 goals_at=0 时写得进去（CAS）：重启/并发都不会重复拆解。
// 返回 true 表示本次是赢家。
func (s *Store) MarkGoalsDecomposed(ctx context.Context, runID int64) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE run_tasks SET goals_at = $2, updated_at = now()
		 WHERE run_id = $1 AND goals_at = 0`, runID, time.Now().Unix())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// GoalsDecomposedAt 读拆解完成时刻（0 = 还没拆过）。
func (s *Store) GoalsDecomposedAt(ctx context.Context, runID int64) (int64, error) {
	var at int64
	err := s.pool.QueryRow(ctx, `SELECT goals_at FROM run_tasks WHERE run_id=$1`, runID).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return at, err
}

// AddConstraints 追加操作约束（拆解 agent 产出的 allow/deny）。
//
// 同一句约束重复提交只保留一条（"文本 + 类型"相同即视为同一条）。
func (s *Store) AddConstraints(ctx context.Context, runID int64, items []model.Constraint) (int, error) {
	added := 0
	for _, c := range items {
		if c.Text == "" {
			continue
		}
		kind := c.Kind
		if kind != "allow" && kind != "deny" {
			kind = "deny" // 拿不准类型时更保守
		}
		tag, err := s.pool.Exec(ctx, `
			INSERT INTO constraints (run_id, kind, text, origin)
			SELECT $1, $2, $3, 'goals'
			 WHERE NOT EXISTS (
			   SELECT 1 FROM constraints WHERE run_id=$1 AND kind=$2 AND text=$3)`,
			runID, kind, c.Text)
		if err != nil {
			return added, err
		}
		added += int(tag.RowsAffected())
	}
	return added, nil
}
