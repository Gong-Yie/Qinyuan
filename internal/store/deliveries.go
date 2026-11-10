package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// 本文件是《升级计划》P3-3 的交付台账：outbox 保"至少一次"，台账保"恰好一次"。

// Delivery 是一次成功投递的记录。
type Delivery struct {
	Connector  string
	EventKey   string
	ExternalID string
	Status     string
	LastError  string
}

// Delivery 查台账（ok=false 表示还没投过）。
func (s *Store) Delivery(ctx context.Context, connector, eventKey string) (*Delivery, bool, error) {
	var d Delivery
	err := s.pool.QueryRow(ctx, `
		SELECT connector, event_key, external_id, status, last_error
		  FROM connector_deliveries WHERE connector=$1 AND event_key=$2`,
		connector, eventKey).
		Scan(&d.Connector, &d.EventKey, &d.ExternalID, &d.Status, &d.LastError)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &d, true, nil
}

// PutDelivery 记一次投递（重复投递用同一 (connector,event_key) 覆盖状态）。
func (s *Store) PutDelivery(ctx context.Context, connector, eventKey, externalID, status, lastError string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO connector_deliveries (connector, event_key, external_id, status, last_error)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (connector, event_key) DO UPDATE
		   SET external_id = COALESCE(NULLIF(EXCLUDED.external_id,''), connector_deliveries.external_id),
		       status = EXCLUDED.status,
		       last_error = EXCLUDED.last_error,
		       updated_at = now()`,
		connector, eventKey, externalID, status, lastError)
	return err
}

// DeliveriesByConnector 列某个连接器的投递记录（排障："到底推到哪了"）。
func (s *Store) DeliveryByExternalID(ctx context.Context, connector, externalID string) (*Delivery, bool, error) {
	if externalID == "" {
		return nil, false, nil
	}
	var d Delivery
	err := s.pool.QueryRow(ctx, `
		SELECT connector, event_key, external_id, status, last_error
		  FROM connector_deliveries WHERE connector=$1 AND external_id=$2
		 ORDER BY id DESC LIMIT 1`, connector, externalID).
		Scan(&d.Connector, &d.EventKey, &d.ExternalID, &d.Status, &d.LastError)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &d, true, nil
}

// DeliveriesByConnector 列某个连接器的投递记录（排障："到底推到哪了"）。
func (s *Store) DeliveriesByConnector(ctx context.Context, connector string, limit int) ([]Delivery, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT connector, event_key, external_id, status, last_error FROM connector_deliveries`
	args := []any{}
	if connector != "" {
		q += ` WHERE connector=$1`
		args = append(args, connector)
	}
	q += ` ORDER BY id DESC LIMIT $` + itoaArg(len(args)+1)
	args = append(args, limit)

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Delivery{}
	for rows.Next() {
		var d Delivery
		if err := rows.Scan(&d.Connector, &d.EventKey, &d.ExternalID, &d.Status, &d.LastError); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// SetFindingTicketStatus 把外部工单状态写回 finding 的 payload（不新增一份副本）。
//
// 只写 payload 里一个字段（ticket_status / ticket_id / ticket_note），
// 于是"发现"仍然是单一真相源（HANDOFF §4.2 的偏离 1）。
func (s *Store) SetFindingTicketStatus(ctx context.Context, runID, findingID int64, status, ticketID, note string) (bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	tag, err := tx.Exec(ctx, `
		UPDATE nodes
		   SET payload = payload || jsonb_strip_nulls(jsonb_build_object(
		           'ticket_status', NULLIF($3,''),
		           'ticket_id',     NULLIF($4,''),
		           'ticket_note',   NULLIF($5,''))),
		       updated_at = now()
		 WHERE run_id=$1 AND id=$2 AND kind='finding'`,
		runID, findingID, status, ticketID, note)
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
	if err := stampNodes(ctx, tx, runID, rev, findingID); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// itoaArg 只用于拼 SQL 占位符序号（不做业务计算）。
func itoaArg(n int) string {
	if n <= 0 {
		return "0"
	}
	var b [4]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
