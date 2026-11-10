package store

import (
	"context"
)

// 复测结论三档（与 docs/14 一致）：
//
//	verified     —— 重放后触发条件仍成立（状态码一致且响应指纹匹配）
//	failed       —— 重放后不再成立（状态码变了 / 响应明显不同）
//	unverifiable —— 无法判定（流量已被清理、超出当前范围、网络失败、二进制）
const (
	RetestVerified     = "verified"
	RetestFailed       = "failed"
	RetestUnverifiable = "unverifiable"
)

// Retest 是一次复测记录。
type Retest struct {
	ID         int64  `json:"id"`
	RunID      int64  `json:"run_id"`
	FindingID  int64  `json:"finding_id"`
	ExchangeID int64  `json:"exchange_id,omitempty"`
	Status     string `json:"status"`
	Expected   string `json:"expected"`
	Observed   string `json:"observed"`
	Error      string `json:"error,omitempty"`
	DurationMS int    `json:"duration_ms"`
	CreatedAt  int64  `json:"created_at"`
}

// AddRetest 落一条复测结果。
func (s *Store) AddRetest(ctx context.Context, r Retest) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO finding_retests (run_id, finding_id, exchange_id, status, expected, observed, error, duration_ms)
		VALUES ($1,$2,NULLIF($3,0),$4,$5,$6,$7,$8) RETURNING id`,
		r.RunID, r.FindingID, r.ExchangeID, r.Status, r.Expected, r.Observed, r.Error, r.DurationMS).
		Scan(&id)
	return id, err
}

// Retests 取某个 finding 的复测历史（倒序）。
func (s *Store) Retests(ctx context.Context, runID, findingID int64, limit int) ([]Retest, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, run_id, finding_id, COALESCE(exchange_id,0), status, expected, observed,
		       error, duration_ms, EXTRACT(EPOCH FROM created_at)::bigint
		  FROM finding_retests WHERE run_id=$1 AND finding_id=$2
		 ORDER BY id DESC LIMIT $3`, runID, findingID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Retest{}
	for rows.Next() {
		var r Retest
		if err := rows.Scan(&r.ID, &r.RunID, &r.FindingID, &r.ExchangeID, &r.Status,
			&r.Expected, &r.Observed, &r.Error, &r.DurationMS, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RetestStatusByFinding 给列表用：每个 finding 最近一次复测的结论。
func (s *Store) RetestStatusByFinding(ctx context.Context, runID int64) (map[int64]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (finding_id) finding_id, status
		  FROM finding_retests WHERE run_id=$1
		 ORDER BY finding_id, id DESC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var fid int64
		var st string
		if err := rows.Scan(&fid, &st); err != nil {
			return nil, err
		}
		out[fid] = st
	}
	return out, rows.Err()
}

// RetestHistory 跨 finding 的复测历史（任务详情/报告用）。
func (s *Store) RetestHistory(ctx context.Context, runID int64, limit int) ([]Retest, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, run_id, finding_id, COALESCE(exchange_id,0), status, expected, observed,
		       error, duration_ms, EXTRACT(EPOCH FROM created_at)::bigint
		  FROM finding_retests WHERE run_id=$1 ORDER BY id DESC LIMIT $2`, runID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Retest{}
	for rows.Next() {
		var r Retest
		if err := rows.Scan(&r.ID, &r.RunID, &r.FindingID, &r.ExchangeID, &r.Status,
			&r.Expected, &r.Observed, &r.Error, &r.DurationMS, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
