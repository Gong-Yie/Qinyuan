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

// CreateRunInput 建一个 run 需要的一切（一次事务，要么全成要么全不成）。
type CreateRunInput struct {
	Goal           string
	Description    string
	Name           string
	TimeoutSeconds int
	WorkerCount    int
	Scopes         []model.Scope
	// SeedEntities 由调用方归一化后再传（store 不做领域归一化）。
	SeedEntities []EntityInput
}

// CreateRun 建 run + run_tasks + origin fact + 范围 + 种子实体，并锚到 origin fact。
//
// origin fact 是图的根：这样"每条意图都能追溯到根"从第一条意图起就成立。
func (s *Store) CreateRun(ctx context.Context, in CreateRunInput) (*model.Run, error) {
	if in.Goal == "" {
		return nil, errors.New("goal 不能为空")
	}
	if in.WorkerCount <= 0 {
		in.WorkerCount = 2
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	var runID int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO runs (goal, description) VALUES ($1, $2) RETURNING id`,
		in.Goal, nullIfEmpty(in.Description)).Scan(&runID); err != nil {
		return nil, fmt.Errorf("建 run 失败：%w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO run_tasks (run_id, name, status, timeout_seconds, worker_count)
		 VALUES ($1, $2, 'created', $3, $4)`,
		runID, nullIfEmpty(in.Name), in.TimeoutSeconds, in.WorkerCount); err != nil {
		return nil, fmt.Errorf("建 run_tasks 失败：%w", err)
	}

	origin := map[string]any{
		"summary":    "任务起点：" + model.TrimSummary(in.Goal, 120),
		"detail":     in.Description,
		"evidence":   "用户输入的目标与描述",
		"confidence": "observed",
		"goal":       in.Goal,
	}
	payload, err := json.Marshal(origin)
	if err != nil {
		return nil, err
	}
	var originID int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO nodes (run_id, kind, state, payload, priority, origin)
		 VALUES ($1, 'fact', 'origin', $2, 0, 'seed') RETURNING id`,
		runID, payload).Scan(&originID); err != nil {
		return nil, fmt.Errorf("建 origin fact 失败：%w", err)
	}

	// M2 简化：没有独立的 goals 拆解 agent，先把用户目标种成唯一目标节点。
	// 多目标拆解在 M3（第 0 轮 goals agent）补上，届时这里只作为兜底。
	goalPayload, err := json.Marshal(map[string]any{"text": in.Goal})
	if err != nil {
		return nil, err
	}
	var goalID int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO nodes (run_id, kind, state, payload, priority, origin)
		 VALUES ($1, 'goal', 'open', $2, 0, 'seed') RETURNING id`,
		runID, goalPayload).Scan(&goalID); err != nil {
		return nil, fmt.Errorf("建目标失败：%w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO edges (run_id, src_id, rel, dst_id) VALUES ($1,$2,'spawns',$3)
		ON CONFLICT DO NOTHING`, runID, originID, goalID); err != nil {
		return nil, err
	}

	for _, sc := range in.Scopes {
		if err := insertScope(ctx, tx, runID, sc); err != nil {
			return nil, err
		}
	}
	for _, ent := range in.SeedEntities {
		entID, _, err := upsertEntity(ctx, tx, runID, ent)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO anchors (node_id, entity_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			originID, entID); err != nil {
			return nil, err
		}
	}

	// 建 run 本身也是一次"变化"：给它一个版本并给节点/边打戳，
	// 这样"从版本 0 开始的增量"也能完整重建出初始图（属性测试会用到）。
	rev, err := bumpVersion(ctx, tx, runID)
	if err != nil {
		return nil, err
	}
	if err := stampNodes(ctx, tx, runID, rev, originID, goalID); err != nil {
		return nil, err
	}
	if err := stampEdge(ctx, tx, runID, rev, originID, model.RelSpawns, goalID); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.Run(ctx, runID)
}

// Run 读一个 run（含任务行）。
func (s *Store) Run(ctx context.Context, id int64) (*model.Run, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT r.id, r.goal, COALESCE(r.description,''), r.status, r.round_no, r.version, r.created_at, r.updated_at,
		       t.run_id, COALESCE(t.name,''), t.status, t.paused, t.queued, COALESCE(t.queued_at,0),
		       t.timeout_seconds, t.first_run_at, t.deadline_at, t.heartbeat_seconds, t.worker_count, t.completed_at,
		       t.goals_at
		  FROM runs r LEFT JOIN run_tasks t ON t.run_id = r.id
		 WHERE r.id = $1`, id)

	var r model.Run
	var t model.Task
	var desc *string
	var name *string
	var runID *int64
	err := row.Scan(&r.ID, &r.Goal, &desc, &r.Status, &r.RoundNo, &r.Version, &r.CreatedAt, &r.UpdatedAt,
		&runID, &name, &t.Status, &t.Paused, &t.Queued, &t.QueuedAt,
		&t.TimeoutSeconds, &t.FirstRunAt, &t.DeadlineAt, &t.HeartbeatSeconds, &t.WorkerCount, &t.CompletedAt,
		&t.GoalsAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if desc != nil {
		r.Description = *desc
	}
	if runID != nil {
		t.RunID = *runID
		if name != nil {
			t.Name = *name
		}
		r.Task = &t
	}
	return &r, nil
}

// ListRuns 最近的 run。
func (s *Store) ListRuns(ctx context.Context, limit int) ([]*model.Run, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT r.id, r.goal, COALESCE(r.description,''), r.status, r.round_no, r.version, r.created_at, r.updated_at,
		       t.run_id, COALESCE(t.name,''), t.status, t.paused, t.queued, COALESCE(t.queued_at,0),
		       t.timeout_seconds, t.first_run_at, t.deadline_at, t.heartbeat_seconds, t.worker_count, t.completed_at,
		       t.goals_at
		  FROM runs r LEFT JOIN run_tasks t ON t.run_id = r.id
		 ORDER BY r.id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*model.Run
	for rows.Next() {
		var r model.Run
		var t model.Task
		var desc, name *string
		var runID *int64
		if err := rows.Scan(&r.ID, &r.Goal, &desc, &r.Status, &r.RoundNo, &r.Version, &r.CreatedAt, &r.UpdatedAt,
			&runID, &name, &t.Status, &t.Paused, &t.Queued, &t.QueuedAt,
			&t.TimeoutSeconds, &t.FirstRunAt, &t.DeadlineAt, &t.HeartbeatSeconds, &t.WorkerCount, &t.CompletedAt,
			&t.GoalsAt); err != nil {
			return nil, err
		}
		if desc != nil {
			r.Description = *desc
		}
		if runID != nil {
			t.RunID = *runID
			if name != nil {
				t.Name = *name
			}
			r.Task = &t
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

// CASRunTaskStatus 任务状态迁移：只有当前状态在 expected 里才写得进去。
func (s *Store) CASRunTaskStatus(ctx context.Context, runID int64, expected []string, next string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE run_tasks
		   SET status = $3,
		       paused = CASE WHEN $3 = 'paused' THEN true WHEN $3 = 'running' THEN false ELSE paused END,
		       completed_at = CASE WHEN $3 IN ('done','failed','timeout') THEN $4 ELSE completed_at END,
		       updated_at = now()
		 WHERE run_id = $1 AND status = ANY($2)`,
		runID, expected, next, time.Now().Unix())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// SetTaskPaused 暂停/恢复（不含取消 in-flight，那是 engine 的事）。
func (s *Store) SetTaskPaused(ctx context.Context, runID int64, paused bool) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE run_tasks SET paused = $2, updated_at = now() WHERE run_id = $1`, runID, paused)
	return err
}

// StampFirstRun 首次真正执行时盖时间戳并算 deadline（幂等）。
func (s *Store) StampFirstRun(ctx context.Context, runID int64, timeoutSeconds int) error {
	now := time.Now().Unix()
	_, err := s.pool.Exec(ctx, `
		UPDATE run_tasks
		   SET first_run_at = CASE WHEN first_run_at = 0 THEN $2 ELSE first_run_at END,
		       deadline_at  = CASE WHEN first_run_at = 0 AND $3 > 0 THEN $2 + $3 ELSE deadline_at END,
		       updated_at   = now()
		 WHERE run_id = $1`, runID, now, timeoutSeconds)
	return err
}

// TouchRun 刷新心跳时间（只用于可观测，不参与正确性）。
func (s *Store) TouchRun(ctx context.Context, runID int64) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE runs SET updated_at = now() WHERE id = $1`, runID)
	return err
}

// AddScope 追加一条范围（重复则忽略）。
func (s *Store) AddScope(ctx context.Context, runID int64, sc model.Scope) error {
	return insertScope(ctx, s.pool, runID, sc)
}

func insertScope(ctx context.Context, q querier, runID int64, sc model.Scope) error {
	if sc.Source == "" {
		sc.Source = "manual"
	}
	_, err := q.Exec(ctx, `
		INSERT INTO run_scope (run_id, kind, domain, net, org_id, value, source, reason)
		VALUES ($1, $2, $3, NULLIF($4,'')::cidr, NULLIF($5,0), $6, $7, $8)
		ON CONFLICT DO NOTHING`,
		runID, sc.Kind, nullIfEmpty(sc.Domain), sc.Net, sc.OrgID, nullIfEmpty(sc.Value), sc.Source, nullIfEmpty(sc.Reason))
	if err != nil {
		return fmt.Errorf("登记范围失败：%w", err)
	}
	return nil
}

// Scopes 读一个 run 的全部范围。
func (s *Store) Scopes(ctx context.Context, runID int64) ([]model.Scope, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT kind, COALESCE(domain,''), COALESCE(net::text,''), COALESCE(org_id,0), COALESCE(value,''), source, COALESCE(reason,'')
		  FROM run_scope WHERE run_id = $1 ORDER BY id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Scope
	for rows.Next() {
		var sc model.Scope
		if err := rows.Scan(&sc.Kind, &sc.Domain, &sc.Net, &sc.OrgID, &sc.Value, &sc.Source, &sc.Reason); err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// Constraints 读操作约束（注入 prompt 用）。
func (s *Store) Constraints(ctx context.Context, runID int64) ([]model.Constraint, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, kind, text FROM constraints WHERE run_id = $1 ORDER BY id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Constraint
	for rows.Next() {
		var c model.Constraint
		if err := rows.Scan(&c.ID, &c.Kind, &c.Text); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
