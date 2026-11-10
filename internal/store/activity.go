package store

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/jackc/pgx/v5"

	"qinyuan/internal/model"
)

// AppendActivity 写一条过程记录（只追加）。
//
// 写入前做两道清洗（不做的话 PostgreSQL 会整条拒收，表现是"某一步凭空消失"）：
//   - 文本转成合法 UTF-8，并去掉 0x00（22P05 / 22P21）；
//   - metadata 里同样清洗，并保证是合法 JSON。
func (s *Store) AppendActivity(ctx context.Context, a *model.Activity) (int64, error) {
	if a.RunID <= 0 {
		return 0, ErrNotFound
	}
	meta := a.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	rawMeta, err := json.Marshal(meta)
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.pool.QueryRow(ctx, `
		INSERT INTO activity (run_id, node_id, worker, kind, tool, tool_use_id, is_error,
		                      summary, detail, metadata, input_tokens, output_tokens,
		                      cache_read_tokens, cache_write_tokens, segment)
		VALUES ($1,$2,NULLIF($3,''),$4,NULLIF($5,''),NULLIF($6,''),$7,$8,$9,$10,
		        NULLIF($11,0),NULLIF($12,0),NULLIF($13,0),NULLIF($14,0),$15)
		RETURNING id`,
		a.RunID, a.NodeID, clean(a.Worker), a.Kind, clean(a.Tool), clean(a.ToolUseID), a.IsError,
		clean(a.Summary), clean(a.Detail), cleanJSON(rawMeta),
		a.InputTokens, a.OutputTokens, a.CacheReadTokens, a.CacheWriteTokens, a.Segment).Scan(&id)
	return id, err
}

// Activities 按时间正序取过程记录（列表不取 detail，详情另取）。
func (s *Store) Activities(ctx context.Context, runID int64, limit int) ([]*model.Activity, error) {
	if limit <= 0 || limit > 2000 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, run_id, COALESCE(node_id,0), COALESCE(worker,''), kind, COALESCE(tool,''),
		       COALESCE(tool_use_id,''), is_error, COALESCE(summary,''), ''::text,
		       metadata, COALESCE(input_tokens,0), COALESCE(output_tokens,0),
		       COALESCE(cache_read_tokens,0), COALESCE(cache_write_tokens,0), segment, created_at
		  FROM activity WHERE run_id=$1 ORDER BY id ASC LIMIT $2`, runID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanActivities(rows)
}

// ActivityTail 取最近 N 条（倒序取再正序返回）。
func (s *Store) ActivityTail(ctx context.Context, runID int64, limit int) ([]*model.Activity, error) {
	if limit <= 0 || limit > 2000 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT * FROM (
			SELECT id, run_id, COALESCE(node_id,0), COALESCE(worker,''), kind, COALESCE(tool,''),
			       COALESCE(tool_use_id,''), is_error, COALESCE(summary,''), ''::text,
			       metadata, COALESCE(input_tokens,0), COALESCE(output_tokens,0),
			       COALESCE(cache_read_tokens,0), COALESCE(cache_write_tokens,0), segment, created_at
			  FROM activity WHERE run_id=$1 ORDER BY id DESC LIMIT $2
		) t ORDER BY id ASC`, runID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanActivities(rows)
}

// ActivitiesAfter 取 id > afterID 的正序记录（SSE 断线续传用）。
//
// 续传语义与前端约定一致：客户端带 `since=<最后收到的 id>`，服务端从 DB 补齐缺口。
// 广播本身是可丢的（缓冲满就丢），DB 才是完整性的来源。
func (s *Store) ActivitiesAfter(ctx context.Context, runID, afterID int64, limit int) ([]*model.Activity, error) {
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, run_id, COALESCE(node_id,0), COALESCE(worker,''), kind, COALESCE(tool,''),
		       COALESCE(tool_use_id,''), is_error, COALESCE(summary,''), ''::text,
		       metadata, COALESCE(input_tokens,0), COALESCE(output_tokens,0),
		       COALESCE(cache_read_tokens,0), COALESCE(cache_write_tokens,0), segment, created_at
		  FROM activity WHERE run_id=$1 AND id > $2 ORDER BY id ASC LIMIT $3`,
		runID, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanActivities(rows)
}

// ActivityDetail 取单条（含 detail 全文）。
func (s *Store) ActivityDetail(ctx context.Context, id int64) (*model.Activity, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, run_id, COALESCE(node_id,0), COALESCE(worker,''), kind, COALESCE(tool,''),
		       COALESCE(tool_use_id,''), is_error, COALESCE(summary,''), COALESCE(detail,''),
		       metadata, COALESCE(input_tokens,0), COALESCE(output_tokens,0),
		       COALESCE(cache_read_tokens,0), COALESCE(cache_write_tokens,0), segment, created_at
		  FROM activity WHERE id=$1`, id)
	var a model.Activity
	var nodeID int64
	var meta []byte
	err := row.Scan(&a.ID, &a.RunID, &nodeID, &a.Worker, &a.Kind, &a.Tool,
		&a.ToolUseID, &a.IsError, &a.Summary, &a.Detail, &meta,
		&a.InputTokens, &a.OutputTokens, &a.CacheReadTokens, &a.CacheWriteTokens, &a.Segment, &a.CreatedAt)
	if err != nil {
		return nil, err
	}
	if nodeID != 0 {
		a.NodeID = &nodeID
	}
	_ = json.Unmarshal(meta, &a.Metadata)
	return &a, nil
}

// TokenTotals 只汇总 kind='result' 的权威行（直接 SUM 全表会重复计数）。
func (s *Store) TokenTotals(ctx context.Context, runID int64) (model.TokenUsage, error) {
	var u model.TokenUsage
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(sum(input_tokens),0), COALESCE(sum(output_tokens),0),
		       COALESCE(sum(cache_read_tokens),0), COALESCE(sum(cache_write_tokens),0)
		  FROM activity WHERE run_id=$1 AND kind='result'`, runID).
		Scan(&u.Input, &u.Output, &u.CacheRead, &u.CacheWrite)
	return u, err
}

// NodeUsage 是一个节点（通常是一条意图）的用量。
//
// 「哪个意图贵」只能从 kind='result' 的权威行算：usage 帧是流式累计值，SUM 全表会重复计数。
type NodeUsage struct {
	NodeID      int64  `json:"node_id"`
	Kind        string `json:"kind,omitempty"`
	Summary     string `json:"summary,omitempty"`
	Calls       int    `json:"calls"`
	Input       int64  `json:"input_tokens"`
	Output      int64  `json:"output_tokens"`
	CacheRead   int64  `json:"cache_read_tokens"`
	CacheWrite  int64  `json:"cache_write_tokens"`
	EffectiveIn int64  `json:"effective_input_tokens"` // input - cache_read：更像"真正新读进去的"
}

// UsageByNode 按节点聚合用量，按"有效输入"降序（先看最贵的）。
//
// node_id=0 的行是 planner/goals 这类"任务级"调用（它们没有挂到某个意图上）。
func (s *Store) UsageByNode(ctx context.Context, runID int64) ([]NodeUsage, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT COALESCE(a.node_id,0),
		       COALESCE(n.kind,''),
		       COALESCE(left(n.payload->>'summary', 60), ''),
		       count(*),
		       COALESCE(sum(a.input_tokens),0), COALESCE(sum(a.output_tokens),0),
		       COALESCE(sum(a.cache_read_tokens),0), COALESCE(sum(a.cache_write_tokens),0)
		  FROM activity a
		  LEFT JOIN nodes n ON n.run_id = a.run_id AND n.id = a.node_id
		 WHERE a.run_id=$1 AND a.kind='result'
		 GROUP BY 1,2,3
		 ORDER BY GREATEST(COALESCE(sum(a.input_tokens),0) - COALESCE(sum(a.cache_read_tokens),0), 0) DESC`,
		runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []NodeUsage
	for rows.Next() {
		var u NodeUsage
		if err := rows.Scan(&u.NodeID, &u.Kind, &u.Summary, &u.Calls,
			&u.Input, &u.Output, &u.CacheRead, &u.CacheWrite); err != nil {
			return nil, err
		}
		u.EffectiveIn = u.Input - u.CacheRead
		if u.EffectiveIn < 0 {
			u.EffectiveIn = 0
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// LastIntentOutput 取一条意图最后一条 assistant 正文（planner 判断"它到底做出了什么"）。
func (s *Store) LastIntentOutput(ctx context.Context, runID, intentID int64) (string, error) {
	var out string
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(detail, summary, '') FROM activity
		 WHERE run_id=$1 AND node_id=$2 AND kind IN ('text','result')
		 ORDER BY id DESC LIMIT 1`, runID, intentID).Scan(&out)
	if err == pgx.ErrNoRows {
		return "", nil
	}
	return out, err
}

func scanActivities(rows pgx.Rows) ([]*model.Activity, error) {
	var out []*model.Activity
	for rows.Next() {
		var a model.Activity
		var nodeID int64
		var meta []byte
		if err := rows.Scan(&a.ID, &a.RunID, &nodeID, &a.Worker, &a.Kind, &a.Tool,
			&a.ToolUseID, &a.IsError, &a.Summary, &a.Detail, &meta,
			&a.InputTokens, &a.OutputTokens, &a.CacheReadTokens, &a.CacheWriteTokens, &a.Segment, &a.CreatedAt); err != nil {
			return nil, err
		}
		if nodeID != 0 {
			a.NodeID = &nodeID
		}
		_ = json.Unmarshal(meta, &a.Metadata)
		out = append(out, &a)
	}
	return out, rows.Err()
}

func clean(s string) string {
	if s == "" {
		return ""
	}
	s = strings.ToValidUTF8(s, "\uFFFD")
	return strings.ReplaceAll(s, "\x00", "")
}

// cleanJSON 去掉 JSON 里的 0x00（只处理字符串值；用再编码的方式保证合法）。
func cleanJSON(raw []byte) []byte {
	if len(raw) == 0 {
		return []byte("{}")
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return []byte("{}")
	}
	return []byte(cleanAnyJSON(v))
}

func cleanAnyJSON(v any) string {
	b, err := json.Marshal(stripNUL(v))
	if err != nil {
		return "{}"
	}
	return string(b)
}

func stripNUL(v any) any {
	switch x := v.(type) {
	case string:
		return strings.ReplaceAll(strings.ToValidUTF8(x, "\uFFFD"), "\x00", "")
	case []any:
		for i := range x {
			x[i] = stripNUL(x[i])
		}
		return x
	case map[string]any:
		for k, vv := range x {
			delete(x, k)
			x[strings.ReplaceAll(k, "\x00", "")] = stripNUL(vv)
		}
		return x
	default:
		return v
	}
}
