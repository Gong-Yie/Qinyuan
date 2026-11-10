package store

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// TrafficInput 是一次交换的写入参数（由 http_request 工具填充）。
//
// 正文策略：≤8KB 内联（可搜），更大落内容寻址文件，只留 sha + 预览。
type TrafficInput struct {
	RunID       int64
	IntentID    int64
	ToolUseID   string
	Method      string
	URL         string
	Status      int
	ContentType string
	ReqLen      int
	RespLen     int
	DurationMS  int
	ReqHeaders  map[string]string
	RespHeaders map[string]string
	ReqBody     string // 预览（已截断到 8KB）
	RespBody    string
	ReqSHA      string
	RespSHA     string
	ReqPath     string // 相对 dataDir；空 = 未落盘
	RespPath    string
	IsBinary    bool
	Truncated   bool
	Error       string
}

// TrafficExchange 是列表/详情用的读出结构。
type TrafficExchange struct {
	ID          int64             `json:"id"`
	RunID       int64             `json:"run_id"`
	IntentID    int64             `json:"intent_id,omitempty"`
	ToolUseID   string            `json:"tool_use_id,omitempty"`
	TS          int64             `json:"ts"`
	Method      string            `json:"method"`
	URL         string            `json:"url"`
	URLTemplate string            `json:"url_template"`
	Status      int               `json:"status"`
	ContentType string            `json:"content_type"`
	ReqLen      int               `json:"req_len"`
	RespLen     int               `json:"resp_len"`
	DurationMS  int               `json:"duration_ms"`
	ReqHeaders  map[string]string `json:"req_headers,omitempty"`
	RespHeaders map[string]string `json:"resp_headers,omitempty"`
	ReqBody     string            `json:"req_body,omitempty"`
	RespBody    string            `json:"resp_body,omitempty"`
	ReqSHA      string            `json:"req_sha,omitempty"`
	RespSHA     string            `json:"resp_sha,omitempty"`
	ReqPath     string            `json:"req_path,omitempty"`
	RespPath    string            `json:"resp_path,omitempty"`
	IsBinary    bool              `json:"is_binary"`
	Truncated   bool              `json:"truncated"`
	Error       string            `json:"error,omitempty"`
	Preview     string            `json:"preview,omitempty"`
}

// TrafficQuery 是检索条件（host 按 url_template 前缀/包含匹配）。
type TrafficQuery struct {
	Host   string
	Method string
	Status int
	Q      string // 正文子串（pg_trgm / ILIKE）
	Limit  int
}

var reDigits = regexp.MustCompile(`\d+`)
var reUUID = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
var reHex = regexp.MustCompile(`\b[0-9a-f]{16,}\b`)

// NormalizeURLTemplate 把**路径/查询**里的易变段抽象掉：UUID → {uuid}，长 hex → {hash}，
// 数字 → {n}。**scheme 与 authority 保持原样** —— 否则 `http://127.0.0.1:8080/` 会变成
// `http://{n}.{n}.{n}.{n}:{n}/`，按端口/主机检索就永远搜不到。
//
// 目的：同一批探测（/user/1、/user/2 …）在列表里聚成一行，而不是刷屏。
func NormalizeURLTemplate(raw string) string {
	start := len(raw)
	if i := strings.Index(raw, "://"); i >= 0 {
		if s := strings.IndexAny(raw[i+3:], "/?#"); s >= 0 {
			start = i + 3 + s
		}
	}
	tail := raw[start:]
	tail = reUUID.ReplaceAllString(tail, "{uuid}")
	tail = reHex.ReplaceAllString(tail, "{hash}")
	tail = reDigits.ReplaceAllString(tail, "{n}")
	return raw[:start] + tail
}

// RecordTraffic 写一条交换。失败不应影响工具返回（调用方只记日志）。
func (s *Store) RecordTraffic(ctx context.Context, in TrafficInput) (int64, error) {
	if in.URL == "" {
		return 0, fmt.Errorf("url 必填")
	}
	method := strings.ToUpper(strings.TrimSpace(in.Method))
	if method == "" {
		method = "GET"
	}
	tmpl := NormalizeURLTemplate(in.URL)
	reqH, _ := json.Marshal(in.ReqHeaders)
	respH, _ := json.Marshal(in.RespHeaders)

	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO traffic_exchanges
		  (run_id, intent_id, tool_use_id, method, url, url_template, status, content_type,
		   req_len, resp_len, duration_ms, req_headers, resp_headers,
		   req_body, resp_body, req_sha, resp_sha, req_path, resp_path, is_binary, truncated, error)
		VALUES ($1, NULLIF($2,0), $3, $4, $5, $6, $7, $8, $9, $10, $11, $12::jsonb, $13::jsonb,
		        $14, $15, $16, $17, $18, $19, $20, $21, $22)
		RETURNING id`,
		in.RunID, in.IntentID, in.ToolUseID, method, in.URL, tmpl, in.Status, in.ContentType,
		in.ReqLen, in.RespLen, in.DurationMS, string(reqH), string(respH),
		in.ReqBody, in.RespBody, in.ReqSHA, in.RespSHA, in.ReqPath, in.RespPath,
		in.IsBinary, in.Truncated, in.Error).
		Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("写流量失败：%w", err)
	}
	return id, nil
}

// SearchTraffic 按条件检索（列表只回预览，不回全文）。
func (s *Store) SearchTraffic(ctx context.Context, runID int64, q TrafficQuery) ([]TrafficExchange, error) {
	limit := q.Limit
	if limit <= 0 || limit > 50 {
		limit = 5
	}
	cond := []string{"run_id=$1"}
	args := []any{runID}
	add := func(clause string, v any) {
		args = append(args, v)
		cond = append(cond, fmt.Sprintf(clause, len(args)))
	}
	if q.Host != "" {
		add("url_template ILIKE '%%' || $%d || '%%'", q.Host)
	}
	if q.Method != "" {
		add("method = $%d", strings.ToUpper(q.Method))
	}
	if q.Status > 0 {
		add("status = $%d", q.Status)
	}
	if q.Q != "" {
		args = append(args, q.Q)
		n := len(args)
		cond = append(cond, fmt.Sprintf(
			"(resp_body ILIKE '%%' || $%d || '%%' OR req_body ILIKE '%%' || $%d || '%%')", n, n))
	}
	args = append(args, limit)
	query := `SELECT id, run_id, COALESCE(intent_id,0), tool_use_id, EXTRACT(EPOCH FROM ts)::bigint,
	                 method, url, url_template, status, content_type, req_len, resp_len, duration_ms,
	                 req_sha, resp_sha, is_binary, truncated, error,
	                 COALESCE(left(resp_body, 400), '')
	            FROM traffic_exchanges WHERE ` +
		strings.Join(cond, " AND ") + ` ORDER BY id DESC LIMIT $` + strconv.Itoa(len(args))
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []TrafficExchange{}
	for rows.Next() {
		var e TrafficExchange
		if err := rows.Scan(&e.ID, &e.RunID, &e.IntentID, &e.ToolUseID, &e.TS,
			&e.Method, &e.URL, &e.URLTemplate, &e.Status, &e.ContentType,
			&e.ReqLen, &e.RespLen, &e.DurationMS, &e.ReqSHA, &e.RespSHA,
			&e.IsBinary, &e.Truncated, &e.Error, &e.Preview); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// TrafficDetail 取单条完整记录（含正文预览与头）。
func (s *Store) TrafficDetail(ctx context.Context, runID, id int64) (*TrafficExchange, error) {
	var e TrafficExchange
	var reqH, respH []byte
	var ts time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT id, run_id, COALESCE(intent_id,0), tool_use_id, ts, method, url, url_template,
		       status, content_type, req_len, resp_len, duration_ms, req_headers, resp_headers,
		       req_body, resp_body, req_sha, resp_sha, req_path, resp_path, is_binary, truncated, error
		  FROM traffic_exchanges WHERE run_id=$1 AND id=$2`, runID, id).
		Scan(&e.ID, &e.RunID, &e.IntentID, &e.ToolUseID, &ts, &e.Method, &e.URL, &e.URLTemplate,
			&e.Status, &e.ContentType, &e.ReqLen, &e.RespLen, &e.DurationMS, &reqH, &respH,
			&e.ReqBody, &e.RespBody, &e.ReqSHA, &e.RespSHA, &e.ReqPath, &e.RespPath,
			&e.IsBinary, &e.Truncated, &e.Error)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, err
	}
	e.TS = ts.Unix()
	_ = json.Unmarshal(reqH, &e.ReqHeaders)
	_ = json.Unmarshal(respH, &e.RespHeaders)
	return &e, nil
}

// BindFindingTraffic 把流量快照绑定到 finding。
//
// 绑定是"复制预览 + sha"，不是外键引用：流量可以被删/被清理，证据不能因此消失。
func (s *Store) BindFindingTraffic(ctx context.Context, runID, findingID int64, exchangeIDs []int64) (int, error) {
	if findingID <= 0 || len(exchangeIDs) == 0 {
		return 0, nil
	}
	n := 0
	for _, eid := range exchangeIDs {
		e, err := s.TrafficDetail(ctx, runID, eid)
		if err != nil {
			continue // 单条找不到就跳过，不整体失败
		}
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO finding_traffic
			  (run_id, finding_id, exchange_id, method, url, status, req_sha, resp_sha, req_preview, resp_preview)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			runID, findingID, eid, e.Method, e.URL, e.Status, e.ReqSHA, e.RespSHA,
			truncateRunes(e.ReqBody, 1200), truncateRunes(e.RespBody, 1200)); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// FindingTrafficList 读 finding 绑定的证据（流量删了也能读）。
func (s *Store) FindingTrafficList(ctx context.Context, runID, findingID int64) ([]map[string]any, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, COALESCE(exchange_id,0), method, url, status, req_sha, resp_sha,
		       req_preview, resp_preview, EXTRACT(EPOCH FROM created_at)::bigint
		  FROM finding_traffic WHERE run_id=$1 AND finding_id=$2 ORDER BY id`, runID, findingID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, eid, ts int64
		var method, url, reqSHA, respSHA, reqPrev, respPrev string
		var status int
		if err := rows.Scan(&id, &eid, &method, &url, &status, &reqSHA, &respSHA,
			&reqPrev, &respPrev, &ts); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"id": id, "exchange_id": eid, "method": method, "url": url, "status": status,
			"req_sha": reqSHA, "resp_sha": respSHA,
			"req_preview": reqPrev, "resp_preview": respPrev, "created_at": ts,
		})
	}
	return out, rows.Err()
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
