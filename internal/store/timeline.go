package store

import (
	"context"
	"encoding/json"

	"qinyuan/internal/model"
)

// 本文件是《升级计划》P2-5 的读口：把 span 行取出来，供时间线 API 还原"轮→意图→工具"。

// TimelineSpans 取一个 run 的 span 行（id > since，正序）。
//
// 只查 span_id <> '' 的行：普通活动（thinking/text/tool_use…）不参与时间线，
// 它们的 id 仍然可以用来做 SSE 游标，两者共用同一张表。
func (s *Store) TimelineSpans(ctx context.Context, runID, sinceID int64, limit int) ([]*model.Activity, error) {
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, run_id, COALESCE(node_id,0), COALESCE(worker,''), COALESCE(span_name,''),
		       COALESCE(trace_id,''), COALESCE(span_id,''), COALESCE(parent_span_id,''),
		       duration_ms, is_error, metadata, created_at
		  FROM activity
		 WHERE run_id=$1 AND span_id <> '' AND id > $2
		 ORDER BY id ASC LIMIT $3`, runID, sinceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []*model.Activity{}
	for rows.Next() {
		var a model.Activity
		var nodeID int64
		var meta []byte
		if err := rows.Scan(&a.ID, &a.RunID, &nodeID, &a.Worker, &a.SpanName,
			&a.TraceID, &a.SpanID, &a.ParentSpanID, &a.DurationMs, &a.IsError, &meta, &a.CreatedAt); err != nil {
			return nil, err
		}
		a.Kind = model.ActSpan
		if nodeID != 0 {
			a.NodeID = &nodeID
		}
		a.Detail = ""
		_ = json.Unmarshal(meta, &a.Metadata)
		out = append(out, &a)
	}
	return out, rows.Err()
}

// SpanStat 是按 span 名字聚合出来的耗时分布（排障时先看这里，再看单个 span）。
type SpanStat struct {
	Name   string  `json:"span"`
	Count  int     `json:"count"`
	P50Ms  float64 `json:"p50_ms"`
	P95Ms  float64 `json:"p95_ms"`
	P99Ms  float64 `json:"p99_ms"`
	MaxMs  int     `json:"max_ms"`
	Errors int     `json:"errors"`
}

// SpanStats 按 span 名字聚合并发耗时（PostgreSQL 的 percentile_disc 直接算分位）。
//
// 口径：只看 span 行；errors 只统计 is_error=true 的 span（工具被拦/LLM 报错）。
func (s *Store) SpanStats(ctx context.Context, runID int64) ([]SpanStat, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT span_name,
		       count(*),
		       COALESCE(percentile_disc(0.5)  WITHIN GROUP (ORDER BY duration_ms), 0),
		       COALESCE(percentile_disc(0.95) WITHIN GROUP (ORDER BY duration_ms), 0),
		       COALESCE(percentile_disc(0.99) WITHIN GROUP (ORDER BY duration_ms), 0),
		       COALESCE(max(duration_ms), 0),
		       count(*) FILTER (WHERE is_error)
		  FROM activity
		 WHERE run_id=$1 AND span_id <> ''
		 GROUP BY span_name
		 ORDER BY sum(duration_ms) DESC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []SpanStat{}
	for rows.Next() {
		var st SpanStat
		if err := rows.Scan(&st.Name, &st.Count, &st.P50Ms, &st.P95Ms, &st.P99Ms, &st.MaxMs, &st.Errors); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}
