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

const nodeColumns = `id, run_id, kind, state, payload, priority,
	COALESCE(owner,''), COALESCE(origin,''), COALESCE(blocked_reason,''),
	content_version, cold_since_round, rev, completed_at, created_at, updated_at`

func scanNode(row pgx.Row) (*model.Node, error) {
	var n model.Node
	var payload []byte
	var cold *int64
	var completed *time.Time
	err := row.Scan(&n.ID, &n.RunID, &n.Kind, &n.State, &payload, &n.Priority,
		&n.Owner, &n.Origin, &n.BlockedReason, &n.ContentVersion, &cold, &n.Rev, &completed,
		&n.CreatedAt, &n.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	n.Payload = json.RawMessage(payload)
	n.ColdSinceRound = cold
	n.CompletedAt = completed
	return &n, nil
}

// AddIntentInput 创建一条意图。
type AddIntentInput struct {
	RunID     int64
	Summary   string
	AssetIDs  []int64
	ParentIDs []int64
	Priority  int
	Origin    string
	// Budget 是 planner 给这条方向定的预算（0 = 不设）。
	// 它只是上限：执行时与任务配置取 min，撞到走收尾轮。
	BudgetTurns   int
	BudgetSeconds int
}

// AddIntent 创建意图并连边。
//
// 校验顺序不能变：parent 合法性 → 资产存在性 → 建节点 → 连边 → 无父则连 origin。
func (s *Store) AddIntent(ctx context.Context, in AddIntentInput) (int64, error) {
	if in.RunID <= 0 || in.Summary == "" {
		return 0, errors.New("run_id 与 summary 必填")
	}
	if in.Priority < 0 || in.Priority > 10 {
		in.Priority = 5
	}
	if in.Origin == "" {
		in.Origin = "planner"
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// 1) 校验 parent：只能是本 run 的 fact / finding。
	parents := make([]int64, 0, len(in.ParentIDs))
	for _, pid := range in.ParentIDs {
		var kind, state string
		err := tx.QueryRow(ctx,
			`SELECT kind, state FROM nodes WHERE run_id=$1 AND id=$2`, in.RunID, pid).Scan(&kind, &state)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, fmt.Errorf("%w: 节点 #%d 不属于本任务", ErrCrossRun, pid)
		}
		if err != nil {
			return 0, err
		}
		if kind != model.KindFact && kind != model.KindFinding {
			return 0, fmt.Errorf("%w: 节点 #%d 是 %q，意图只能锚在事实/发现上；顶层方向请留空 parent_ids",
				ErrParentNotAllowed, pid, kind)
		}
		if kind == model.KindFact && state == model.StateDismissed {
			return 0, fmt.Errorf("节点 #%d 是否定/已废弃的事实，不能作为新方向的上游", pid)
		}
		parents = append(parents, pid)
	}

	// 2) 校验 asset_ids 存在（实体跨 run 共享，不限制 run）。
	for _, aid := range in.AssetIDs {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM entities WHERE id=$1)`, aid).Scan(&exists); err != nil {
			return 0, err
		}
		if !exists {
			return 0, fmt.Errorf("asset_id #%d 不存在；请先用 insert_assets 登记实体", aid)
		}
	}

	fields := map[string]any{
		"summary":   in.Summary,
		"asset_ids": in.AssetIDs,
	}
	if in.BudgetTurns > 0 || in.BudgetSeconds > 0 {
		fields["budget"] = map[string]any{
			"max_turns":   in.BudgetTurns,
			"max_seconds": in.BudgetSeconds,
		}
	}
	payload, err := json.Marshal(fields)
	if err != nil {
		return 0, err
	}

	var intentID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO nodes (run_id, kind, state, payload, priority, origin)
		VALUES ($1, 'intent', 'open', $2, $3, $4) RETURNING id`,
		in.RunID, payload, in.Priority, in.Origin).Scan(&intentID); err != nil {
		return 0, fmt.Errorf("建意图失败：%w", err)
	}

	// 3) 无父则连 origin fact：血缘不许断在根上。
	if len(parents) == 0 {
		var originID int64
		err := tx.QueryRow(ctx,
			`SELECT id FROM nodes WHERE run_id=$1 AND kind='fact' AND state='origin' LIMIT 1`,
			in.RunID).Scan(&originID)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, errors.New("本任务没有 origin fact（数据被破坏），拒绝创建无根意图")
		}
		if err != nil {
			return 0, err
		}
		parents = []int64{originID}
	}
	for _, pid := range parents {
		if _, err := tx.Exec(ctx, `
			INSERT INTO edges (run_id, src_id, rel, dst_id) VALUES ($1,$2,'derived_from',$3)
			ON CONFLICT DO NOTHING`, in.RunID, pid, intentID); err != nil {
			return 0, err
		}
	}
	for _, aid := range in.AssetIDs {
		if _, err := tx.Exec(ctx,
			`INSERT INTO anchors (node_id, entity_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`,
			intentID, aid); err != nil {
			return 0, err
		}
	}

	rev, err := bumpVersion(ctx, tx, in.RunID)
	if err != nil {
		return 0, err
	}
	if err := stampNodes(ctx, tx, in.RunID, rev, intentID); err != nil {
		return 0, err
	}
	for _, pid := range parents {
		if err := stampEdge(ctx, tx, in.RunID, rev, pid, model.RelDerivedFrom, intentID); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return intentID, nil
}

// Frontier 取待领意图（priority DESC, id ASC）。
func (s *Store) Frontier(ctx context.Context, runID int64, limit int) ([]*model.Node, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+nodeColumns+` FROM nodes
		 WHERE run_id=$1 AND kind='intent' AND state='open'
		 ORDER BY priority DESC, id ASC LIMIT $2`, runID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ClaimIntent 领取一条意图：CAS，RowsAffected==1 才算抢到。
//
// 写在一个事务里，赢了才分配版本号并打戳 —— 失败的 CAS 不该浪费版本，
// 更不该让增量里出现一条"其实没发生"的变化。
func (s *Store) ClaimIntent(ctx context.Context, runID, intentID int64, owner string) (bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	tag, err := tx.Exec(ctx, `
		UPDATE nodes SET state='running', owner=$3, updated_at=now()
		 WHERE run_id=$1 AND id=$2 AND kind='intent' AND state='open'`,
		runID, intentID, owner)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() != 1 {
		return false, nil
	}
	rev, err := bumpVersion(ctx, tx, runID)
	if err != nil {
		return false, err
	}
	if err := stampNodes(ctx, tx, runID, rev, intentID); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// CASIntent 意图状态迁移（带 expected）。返回 false = 冲突，调用方不得覆盖。
func (s *Store) CASIntent(ctx context.Context, runID, intentID int64, expected, next string) (bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	tag, err := tx.Exec(ctx, `
		UPDATE nodes
		   SET state=$4,
		       owner=CASE WHEN $4='open' THEN NULL ELSE owner END,
		       updated_at=now(),
		       completed_at=CASE WHEN $4 IN ('done','blocked','exhausted','stopped','deleted') THEN now() ELSE completed_at END
		 WHERE run_id=$1 AND id=$2 AND kind='intent' AND state=$3`,
		runID, intentID, expected, next)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() != 1 {
		return false, nil
	}
	// 意图落终态时，挂它的计划步骤要跟着松手：否则计划图会永远显示 "running"
	// （真实 run 里抓到过：意图 done，步骤卡 running，看起来还有活在跑）。
	var stepIDs []int64
	if isTerminalIntentState(next) {
		var rerr error
		stepIDs, rerr = reconcileStepsAfterIntent(ctx, tx, runID, intentID)
		if rerr != nil {
			return false, rerr
		}
	}
	rev, err := bumpVersion(ctx, tx, runID)
	if err != nil {
		return false, err
	}
	nodes := append([]int64{intentID}, stepIDs...)
	if err := stampNodes(ctx, tx, runID, rev, nodes...); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// SetBlockedReason 给 blocked 的意图加原因（quota / 网络 …）。
func (s *Store) SetBlockedReason(ctx context.Context, runID, intentID int64, reason string) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	tag, err := tx.Exec(ctx,
		`UPDATE nodes SET blocked_reason=$3, updated_at=now() WHERE run_id=$1 AND id=$2`,
		runID, intentID, reason)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return nil // 目标不存在：不是一次变化，不值得分配版本
	}
	rev, err := bumpVersion(ctx, tx, runID)
	if err != nil {
		return err
	}
	if err := stampNodes(ctx, tx, runID, rev, intentID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Intent 读一条意图。
func (s *Store) Intent(ctx context.Context, runID, intentID int64) (*model.Node, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+nodeColumns+` FROM nodes WHERE run_id=$1 AND id=$2 AND kind='intent'`, runID, intentID)
	return scanNode(row)
}

// Node 读任意节点（限定 run，跨 run 一律 ErrNotFound，防越权）。
func (s *Store) Node(ctx context.Context, runID, nodeID int64) (*model.Node, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+nodeColumns+` FROM nodes WHERE run_id=$1 AND id=$2`, runID, nodeID)
	return scanNode(row)
}

// NodesByKind 按 kind 取最新 N 条。
func (s *Store) NodesByKind(ctx context.Context, runID int64, kind string, limit int) ([]*model.Node, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+nodeColumns+` FROM nodes WHERE run_id=$1 AND kind=$2 ORDER BY id DESC LIMIT $3`,
		runID, kind, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// CountByKind 统计某个 kind 的总数（列表要"看到的是 N / 总数"）。
func (s *Store) CountByKind(ctx context.Context, runID int64, kind string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM nodes WHERE run_id=$1 AND kind=$2`, runID, kind).Scan(&n)
	return n, err
}

// CountIntentState 统计意图某状态数量。
func (s *Store) CountIntentState(ctx context.Context, runID int64, state string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM nodes WHERE run_id=$1 AND kind='intent' AND state=$2`, runID, state).Scan(&n)
	return n, err
}

// HasActiveIntent 是否还有活干（open 或 running）。
func (s *Store) HasActiveIntent(ctx context.Context, runID int64) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM nodes WHERE run_id=$1 AND kind='intent' AND state IN ('open','running'))`,
		runID).Scan(&ok)
	return ok, err
}

// HasOpenGoal 是否还有未达成目标。
func (s *Store) HasOpenGoal(ctx context.Context, runID int64) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM nodes WHERE run_id=$1 AND kind='goal' AND state='open')`,
		runID).Scan(&ok)
	return ok, err
}

// ResetRunningIntents 把崩溃残留的 running 打回 open（启动恢复必做）。
func (s *Store) ResetRunningIntents(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE nodes SET state='open', owner=NULL, updated_at=now()
		  WHERE kind='intent' AND state='running'`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// AddFactInput 写一条探索结论。
type AddFactInput struct {
	RunID      int64
	IntentID   int64
	Summary    string
	Detail     string
	Evidence   string
	Confidence string // observed | inferred
	AssetIDs   []int64
}

// AddFact 建 fact 节点 + yields 边 + 锚点（一个事务）。
func (s *Store) AddFact(ctx context.Context, in AddFactInput) (int64, error) {
	if in.Summary == "" {
		return 0, errors.New("summary 必填")
	}
	if in.Confidence != "inferred" {
		in.Confidence = "observed"
	}
	if in.Evidence == "" {
		return 0, errors.New("evidence 必填：写一句能证明这条结论的命令与输出")
	}
	return s.addKnowledge(ctx, knowledgeInput{
		RunID: in.RunID, IntentID: in.IntentID, Kind: model.KindFact,
		Payload: map[string]any{
			"summary":    in.Summary,
			"detail":     in.Detail,
			"evidence":   in.Evidence,
			"confidence": in.Confidence,
			"asset_ids":  in.AssetIDs,
		},
		AssetIDs: in.AssetIDs,
	})
}

// AddFindingInput 写一条已确认漏洞。
type AddFindingInput struct {
	RunID     int64
	IntentID  int64
	Name      string
	Severity  string
	VulnClass string
	Summary   string
	Evidence  string
	AssetIDs  []int64
}

// AddFinding 建 finding 节点（findings 是它的视图）+ yields + 锚点。
func (s *Store) AddFinding(ctx context.Context, in AddFindingInput) (int64, error) {
	if in.Name == "" || in.Summary == "" {
		return 0, errors.New("name 与 summary 必填")
	}
	if in.Evidence == "" {
		return 0, errors.New("evidence 必填：没有可复现证据只能记 fact（inferred），不能记 finding")
	}
	switch in.Severity {
	case "critical", "high", "medium", "low":
	default:
		in.Severity = "medium"
	}
	return s.addKnowledge(ctx, knowledgeInput{
		RunID: in.RunID, IntentID: in.IntentID, Kind: model.KindFinding,
		Payload: map[string]any{
			"name":      in.Name,
			"severity":  in.Severity,
			"vulnclass": in.VulnClass,
			"summary":   in.Summary,
			"evidence":  in.Evidence,
			"asset_ids": in.AssetIDs,
		},
		AssetIDs: in.AssetIDs,
	})
}

type knowledgeInput struct {
	RunID    int64
	IntentID int64
	Kind     string
	Payload  map[string]any
	AssetIDs []int64
}

func (s *Store) addKnowledge(ctx context.Context, in knowledgeInput) (int64, error) {
	if in.RunID <= 0 {
		return 0, errors.New("run_id 必填")
	}
	payload, err := json.Marshal(in.Payload)
	if err != nil {
		return 0, err
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	state := model.StateConfirmed
	var nodeID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO nodes (run_id, kind, state, payload, priority, origin)
		VALUES ($1, $2, $3, $4, 0, 'worker') RETURNING id`,
		in.RunID, in.Kind, state, payload).Scan(&nodeID); err != nil {
		return 0, fmt.Errorf("写 %s 失败：%w", in.Kind, err)
	}

	if in.IntentID > 0 {
		var kind string
		err := tx.QueryRow(ctx,
			`SELECT kind FROM nodes WHERE run_id=$1 AND id=$2`, in.RunID, in.IntentID).Scan(&kind)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, fmt.Errorf("%w: 意图 #%d 不属于本任务", ErrCrossRun, in.IntentID)
		}
		if err != nil {
			return 0, err
		}
		if kind != model.KindIntent {
			return 0, fmt.Errorf("yields 的源必须是 intent，实际是 %q", kind)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO edges (run_id, src_id, rel, dst_id) VALUES ($1,$2,'yields',$3)
			ON CONFLICT DO NOTHING`, in.RunID, in.IntentID, nodeID); err != nil {
			return 0, err
		}
	}
	for _, aid := range in.AssetIDs {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM entities WHERE id=$1)`, aid).Scan(&exists); err != nil {
			return 0, err
		}
		if !exists {
			return 0, fmt.Errorf("asset_id #%d 不存在", aid)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO anchors (node_id, entity_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`,
			nodeID, aid); err != nil {
			return 0, err
		}
	}

	rev, err := bumpVersion(ctx, tx, in.RunID)
	if err != nil {
		return 0, err
	}
	if err := stampNodes(ctx, tx, in.RunID, rev, nodeID); err != nil {
		return 0, err
	}
	if in.IntentID > 0 {
		if err := stampEdge(ctx, tx, in.RunID, rev, in.IntentID, model.RelYields, nodeID); err != nil {
			return 0, err
		}
	}

	// P2-3：finding 入库与"通知/回写外部系统"是同一个事务。
	// 提交了 → 一定有人会去投递；回滚了 → 一定没有这条事件（不会出现"通知说有、库里没有"）。
	if in.Kind == model.KindFinding {
		summary, _ := in.Payload["summary"].(string)
		name, _ := in.Payload["name"].(string)
		severity, _ := in.Payload["severity"].(string)
		if _, _, err := s.EnqueueOutboxTx(ctx, tx, in.RunID, TopicFindingRecorded, map[string]any{
			"finding_id": nodeID,
			"run_id":     in.RunID,
			"name":       name,
			"severity":   severity,
			"summary":    summary,
		}); err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return nodeID, nil
}

// Link 建一条边（重复忽略）。
//
// 注意：它自己分配版本并打戳，所以业务代码要么用它（边+版本一起成），
// 要么在自己的事务里显式 stampEdge —— 否则这条边不会出现在增量里。
func (s *Store) Link(ctx context.Context, runID, src, dst int64, rel string) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	tag, err := tx.Exec(ctx, `
		INSERT INTO edges (run_id, src_id, rel, dst_id) VALUES ($1,$2,$3,$4)
		ON CONFLICT DO NOTHING`, runID, src, rel, dst)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return nil // 已存在：不是一次"变化"
	}
	rev, err := bumpVersion(ctx, tx, runID)
	if err != nil {
		return err
	}
	if err := stampEdge(ctx, tx, runID, rev, src, rel, dst); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Edges 取一个 run 的全部边（图算法在应用层做）。
func (s *Store) Edges(ctx context.Context, runID int64) ([]model.Edge, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT run_id, src_id, rel, dst_id, rev FROM edges WHERE run_id=$1`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Edge
	for rows.Next() {
		var e model.Edge
		if err := rows.Scan(&e.RunID, &e.SrcID, &e.Rel, &e.DstID, &e.Rev); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
