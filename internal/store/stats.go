package store

import (
	"context"
)

// Stats 是控制台首页的聚合数字（跨 run）。
type Stats struct {
	Runs        int   `json:"runs"`
	Running     int   `json:"running"`
	Paused      int   `json:"paused"`
	Queued      int   `json:"queued"`
	Done        int   `json:"done"`
	Failed      int   `json:"failed"`
	Facts       int   `json:"facts"`
	Findings    int   `json:"findings"`
	Entities    int   `json:"entities"`
	Intents     int   `json:"intents"`
	InputTokens int64 `json:"input_tokens"`
	CacheRead   int64 `json:"cache_read_tokens"`
	Output      int64 `json:"output_tokens"`
}

// Stats 汇总首页指标。
//
// 口径：状态按 run_tasks 的原始状态数（不做派生，派生是 UI 的事）；
// token 只认 activity.kind='result'（usage 帧是累计值，SUM 会重复计数）。
func (s *Store) Stats(ctx context.Context) (*Stats, error) {
	var st Stats
	err := s.pool.QueryRow(ctx, `
		SELECT
		  count(*),
		  count(*) FILTER (WHERE status = 'running'),
		  count(*) FILTER (WHERE status = 'paused'),
		  count(*) FILTER (WHERE queued),
		  count(*) FILTER (WHERE status = 'done'),
		  count(*) FILTER (WHERE status IN ('failed','timeout'))
		FROM run_tasks`).
		Scan(&st.Runs, &st.Running, &st.Paused, &st.Queued, &st.Done, &st.Failed)
	if err != nil {
		return nil, err
	}
	if err := s.pool.QueryRow(ctx, `
		SELECT
		  count(*) FILTER (WHERE kind='fact' AND state<>'origin'),
		  count(*) FILTER (WHERE kind='finding'),
		  count(*) FILTER (WHERE kind='intent')
		FROM nodes`).Scan(&st.Facts, &st.Findings, &st.Intents); err != nil {
		return nil, err
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM entities`).Scan(&st.Entities); err != nil {
		return nil, err
	}
	if err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(sum(input_tokens),0), COALESCE(sum(cache_read_tokens),0), COALESCE(sum(output_tokens),0)
		  FROM activity WHERE kind='result'`).
		Scan(&st.InputTokens, &st.CacheRead, &st.Output); err != nil {
		return nil, err
	}
	return &st, nil
}
