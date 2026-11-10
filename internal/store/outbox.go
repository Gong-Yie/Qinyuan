package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// 本文件是《升级计划》P2-3 的数据层：outbox（写库 + 发事件不分叉）。
//
// 唯一的纪律：**任何"写库后必须发生的副作用"都要在同一个事务里入队**。
// 提交了就一定有人投递；回滚了就一定没有这条事件。

// 已投入使用的 topic。常量放在 store（不是 outbox 包）：
// store 才是"谁在什么事务里产生事件"的权威，outbox 只负责投递。
const (
	// TopicFindingRecorded：一条 finding 入库（供通知 / 工单 / CI 等下游消费）。
	TopicFindingRecorded = "finding.recorded"
	// TopicAssetRecorded：实体写回外部资产系统（P3-3 的资产回写用）。
	TopicAssetRecorded = "asset.recorded"
)

// OutboxEvent 是一条待投递/已投递的事件。
type OutboxEvent struct {
	ID          int64           `json:"id"`
	RunID       int64           `json:"run_id,omitempty"`
	Topic       string          `json:"topic"`
	Payload     json.RawMessage `json:"payload"`
	Attempts    int             `json:"attempts"`
	LastError   string          `json:"last_error,omitempty"`
	CreatedAt   time.Time       `json:"-"`
	CreatedSec  int64           `json:"created_at"`
	AvailableAt time.Time       `json:"-"`
	DoneAt      *time.Time      `json:"-"`
	DeadAt      *time.Time      `json:"-"`
	Status      string          `json:"status"`
}

// EnqueueOutbox 用连接池入队（给"没有业务事务"的调用方用）。
//
// 有业务事务时**必须**用 EnqueueOutboxTx，否则会出现"业务回滚了、事件还在"。
func (s *Store) EnqueueOutbox(ctx context.Context, runID int64, topic string, payload any) (int64, bool, error) {
	return enqueueOutbox(ctx, s.pool, runID, topic, payload)
}

// EnqueueOutboxTx 在调用方的事务里入队（与业务写入同生共死）。
func (s *Store) EnqueueOutboxTx(ctx context.Context, tx pgx.Tx, runID int64, topic string, payload any) (int64, bool, error) {
	return enqueueOutbox(ctx, tx, runID, topic, payload)
}

func enqueueOutbox(ctx context.Context, q querier, runID int64, topic string, payload any) (int64, bool, error) {
	if topic == "" {
		return 0, false, errors.New("outbox: topic 必填")
	}
	if payload == nil {
		payload = map[string]any{}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, false, fmt.Errorf("outbox: 序列化 payload 失败：%w", err)
	}
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])

	var id int64
	var inserted bool
	err = q.QueryRow(ctx, `
		INSERT INTO outbox (run_id, topic, payload, payload_hash)
		VALUES (NULLIF($1,0), $2, $3::jsonb, $4)
		ON CONFLICT (topic, payload_hash) DO NOTHING
		RETURNING id, true`, runID, topic, string(raw), hash).Scan(&id, &inserted)
	if errors.Is(err, pgx.ErrNoRows) {
		// 已存在（同一事件重复入队）：返回已有 id，inserted=false。
		var existing int64
		if qerr := q.QueryRow(ctx,
			`SELECT id FROM outbox WHERE topic=$1 AND payload_hash=$2`, topic, hash).Scan(&existing); qerr != nil {
			return 0, false, qerr
		}
		return existing, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("outbox: 入队失败：%w", err)
	}
	return id, true, nil
}

// ClaimOutbox 租约式领取：多实例并发时同一条只会被一个实例拿到。
//
// FOR UPDATE SKIP LOCKED 是这里唯一正确的写法：跨进程竞争不能用一行 UPDATE 的 CAS
// （那是同进程 goroutine 的玩法），否则两个实例会同时投递同一条事件。
func (s *Store) ClaimOutbox(ctx context.Context, limit int, lease time.Duration) ([]OutboxEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	secs := lease.Seconds()
	if secs <= 0 {
		secs = 180
	}
	rows, err := s.pool.Query(ctx, `
		WITH due AS (
			SELECT id FROM outbox
			 WHERE done_at IS NULL AND dead_at IS NULL
			   AND available_at <= now()
			   AND (locked_until IS NULL OR locked_until < now())
			 ORDER BY id
			 LIMIT $1
			 FOR UPDATE SKIP LOCKED
		)
		UPDATE outbox o
		   SET locked_until = now() + make_interval(secs => $2),
		       attempts = attempts + 1
		  FROM due
		 WHERE o.id = due.id
		RETURNING o.id, COALESCE(o.run_id,0), o.topic, o.payload, o.attempts, o.last_error, o.created_at`,
		limit, secs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []OutboxEvent{}
	for rows.Next() {
		var ev OutboxEvent
		var raw []byte
		if err := rows.Scan(&ev.ID, &ev.RunID, &ev.Topic, &raw, &ev.Attempts,
			&ev.LastError, &ev.CreatedAt); err != nil {
			return nil, err
		}
		ev.Payload = json.RawMessage(raw)
		ev.CreatedSec = ev.CreatedAt.Unix()
		out = append(out, ev)
	}
	return out, rows.Err()
}

// MarkOutboxDone 投递成功。
func (s *Store) MarkOutboxDone(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE outbox SET done_at=now(), locked_until=NULL, last_error='' WHERE id=$1`, id)
	return err
}

// FailOutbox 投递失败：退避重试，超过 maxAttempts 进死信（dead_at）。
//
// 死信不是"放弃"，而是"停止自动重试、等人看"：可查、可重放（ReplayOutbox）。
func (s *Store) FailOutbox(ctx context.Context, id int64, cause error, backoff time.Duration, maxAttempts int) (bool, error) {
	if maxAttempts <= 0 {
		maxAttempts = 5
	}
	secs := backoff.Seconds()
	if secs < 0 {
		secs = 0 // 0 = 立刻重试（测试用；生产由 Dispatcher.Backoff 给指数退避）
	}
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	var dead bool
	err := s.pool.QueryRow(ctx, `
		UPDATE outbox
		   SET locked_until = NULL,
		       last_error = $2,
		       available_at = now() + make_interval(secs => $3),
		       dead_at = CASE WHEN attempts >= $4 THEN now() ELSE dead_at END
		 WHERE id=$1
		 RETURNING dead_at IS NOT NULL`, id, msg, secs, maxAttempts).Scan(&dead)
	return dead, err
}

// ListOutbox 列事件：status = pending | dead | done | 空（全部）。
func (s *Store) ListOutbox(ctx context.Context, status string, limit int) ([]OutboxEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	where := ""
	switch status {
	case "pending":
		where = "WHERE done_at IS NULL AND dead_at IS NULL"
	case "dead":
		where = "WHERE dead_at IS NOT NULL"
	case "done":
		where = "WHERE done_at IS NOT NULL"
	case "":
	default:
		return nil, fmt.Errorf("status 只能是 pending/dead/done")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, COALESCE(run_id,0), topic, payload, attempts, last_error,
		       created_at, available_at, done_at, dead_at
		  FROM outbox `+where+`
		 ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []OutboxEvent{}
	for rows.Next() {
		var ev OutboxEvent
		var raw []byte
		if err := rows.Scan(&ev.ID, &ev.RunID, &ev.Topic, &raw, &ev.Attempts, &ev.LastError,
			&ev.CreatedAt, &ev.AvailableAt, &ev.DoneAt, &ev.DeadAt); err != nil {
			return nil, err
		}
		ev.Payload = json.RawMessage(raw)
		ev.CreatedSec = ev.CreatedAt.Unix()
		switch {
		case ev.DoneAt != nil:
			ev.Status = "done"
		case ev.DeadAt != nil:
			ev.Status = "dead"
		default:
			ev.Status = "pending"
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// ReplayOutbox 把死信/已完成的事件重新放回队列（人工重放入口）。
func (s *Store) ReplayOutbox(ctx context.Context, id int64) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE outbox
		   SET dead_at=NULL, done_at=NULL, available_at=now(), attempts=0, locked_until=NULL
		 WHERE id=$1 AND (dead_at IS NOT NULL OR done_at IS NOT NULL)`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// CountOutbox 计数（控制台/健康检查用）。
func (s *Store) CountOutbox(ctx context.Context) (pending, dead int, err error) {
	err = s.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE done_at IS NULL AND dead_at IS NULL),
		       count(*) FILTER (WHERE dead_at IS NOT NULL)
		  FROM outbox`).Scan(&pending, &dead)
	return pending, dead, err
}
