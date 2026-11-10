package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// InterceptRule 是一条审批规则（优先级从高到低匹配，deny 永远赢）。
type InterceptRule struct {
	ID         int64  `json:"id"`
	RunID      *int64 `json:"run_id,omitempty"`
	Priority   int    `json:"priority"`
	MatchTool  string `json:"match_tool"`
	MatchRegex string `json:"match_regex"`
	Decision   string `json:"decision"`
	Note       string `json:"note"`
	Enabled    bool   `json:"enabled"`
}

// PendingApproval 是一次挂起的人工审批。
type PendingApproval struct {
	ID         int64     `json:"id"`
	RunID      int64     `json:"run_id"`
	IntentID   int64     `json:"intent_id,omitempty"`
	Tool       string    `json:"tool"`
	ToolUseID  string    `json:"tool_use_id,omitempty"`
	ToolInput  string    `json:"tool_input"`
	RuleID     int64     `json:"rule_id,omitempty"`
	Reason     string    `json:"reason"`
	Status     string    `json:"status"`
	DecidedBy  string    `json:"decided_by,omitempty"`
	DecidedAt  int64     `json:"decided_at,omitempty"`
	CreatedAt  time.Time `json:"-"`
	CreatedSec int64     `json:"created_at"`
}

// MatchInterceptRule 按"优先级降序、deny 优先"找第一条命中的规则。
//
// deny 永远赢：只要有任何启用的 deny 命中，就不参与优先级比较（审批门的安全底线）。
// 规则里的正则用 Go 编译（DB 里只存字符串，避免各库正则方言不一致）。
func (s *Store) MatchInterceptRule(ctx context.Context, runID int64, tool, input string) (*InterceptRule, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, COALESCE(run_id,0), priority, match_tool, match_regex, decision, note, enabled
		  FROM intercept_rules
		 WHERE enabled AND (run_id IS NULL OR run_id=$1)
		 ORDER BY priority DESC, id DESC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var firstAsk *InterceptRule
	var firstAllow *InterceptRule
	for rows.Next() {
		var r InterceptRule
		var runIDVal int64
		if err := rows.Scan(&r.ID, &runIDVal, &r.Priority, &r.MatchTool, &r.MatchRegex,
			&r.Decision, &r.Note, &r.Enabled); err != nil {
			return nil, err
		}
		if runIDVal > 0 {
			id := runIDVal
			r.RunID = &id
		}
		if r.MatchTool != "" && r.MatchTool != tool {
			continue
		}
		if r.MatchRegex != "" {
			re, cerr := regexp.Compile(r.MatchRegex)
			if cerr != nil {
				continue // 坏规则跳过，但别让整条审批链挂掉
			}
			if !re.MatchString(input) {
				continue
			}
		}
		switch r.Decision {
		case "deny":
			return &r, nil // deny 最高优先，直接短路
		case "ask":
			if firstAsk == nil {
				cp := r
				firstAsk = &cp
			}
		case "allow":
			if firstAllow == nil {
				cp := r
				firstAllow = &cp
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if firstAsk != nil {
		return firstAsk, nil
	}
	return firstAllow, nil
}

// CreatePendingApproval 落一条待审批（工具调用会阻塞等它）。
func (s *Store) CreatePendingApproval(ctx context.Context, runID, intentID int64, tool, toolUseID, input, reason string, ruleID int64) (*PendingApproval, error) {
	if tool == "" {
		return nil, errors.New("tool 必填")
	}
	var p PendingApproval
	var created time.Time
	err := s.pool.QueryRow(ctx, `
		INSERT INTO intercept_pending (run_id, intent_id, tool, tool_use_id, tool_input, rule_id, reason)
		VALUES ($1, NULLIF($2,0), $3, $4, $5::jsonb, NULLIF($6,0), $7)
		RETURNING id, run_id, COALESCE(intent_id,0), tool, tool_use_id, tool_input::text,
		          COALESCE(rule_id,0), reason, status, decided_by, created_at`,
		runID, intentID, tool, toolUseID, input, ruleID, reason).
		Scan(&p.ID, &p.RunID, &p.IntentID, &p.Tool, &p.ToolUseID, &p.ToolInput,
			&p.RuleID, &p.Reason, &p.Status, &p.DecidedBy, &created)
	if err != nil {
		return nil, fmt.Errorf("建审批失败：%w", err)
	}
	p.CreatedAt = created
	p.CreatedSec = created.Unix()
	return &p, nil
}

// Approval 读一条审批。
func (s *Store) Approval(ctx context.Context, id int64) (*PendingApproval, error) {
	var p PendingApproval
	var created time.Time
	var decidedAt *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT id, run_id, COALESCE(intent_id,0), tool, tool_use_id, tool_input::text,
		       COALESCE(rule_id,0), reason, status, decided_by, decided_at, created_at
		  FROM intercept_pending WHERE id=$1`, id).
		Scan(&p.ID, &p.RunID, &p.IntentID, &p.Tool, &p.ToolUseID, &p.ToolInput,
			&p.RuleID, &p.Reason, &p.Status, &p.DecidedBy, &decidedAt, &created)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.CreatedAt = created
	p.CreatedSec = created.Unix()
	if decidedAt != nil {
		p.DecidedAt = decidedAt.Unix()
	}
	return &p, nil
}

// DecideApproval 做决定（CAS：只有 pending 能被改）。
//
// 返回 false = 别人已经处理过 / 已超时 —— 调用方不能覆盖。
func (s *Store) DecideApproval(ctx context.Context, id int64, decision, by, reason string) (bool, error) {
	if decision != "allowed" && decision != "denied" {
		return false, fmt.Errorf("decision 只能是 allowed/denied")
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE intercept_pending
		   SET status=$2, decided_by=$3, decided_at=now(),
		       reason = CASE WHEN $4 <> '' THEN reason || ' | 决定说明：' || $4 ELSE reason END
		 WHERE id=$1 AND status='pending'`, id, decision, by, reason)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// TimeoutApproval 把超时的审批标记为 timeout（仍是 CAS，不覆盖人工决定）。
func (s *Store) TimeoutApproval(ctx context.Context, id int64, reason string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE intercept_pending
		   SET status='timeout', decided_by='timeout', decided_at=now(),
		       reason = CASE WHEN $2 <> '' THEN reason || ' | 超时：' || $2 ELSE reason END
		 WHERE id=$1 AND status='pending'`, id, reason)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ListApprovals 列审批：status 为空 = 全部；pending 按时间正序（老的先处理）。
func (s *Store) ListApprovals(ctx context.Context, status string, limit int) ([]PendingApproval, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	order := "id DESC"
	if status == "pending" {
		order = "id ASC"
	}
	q := `SELECT id, run_id, COALESCE(intent_id,0), tool, tool_use_id, tool_input::text,
	             COALESCE(rule_id,0), reason, status, decided_by, decided_at, created_at
	        FROM intercept_pending`
	args := []any{}
	if status != "" {
		q += ` WHERE status=$1`
		args = append(args, status)
	}
	q += ` ORDER BY ` + order + ` LIMIT $` + strconv.Itoa(len(args)+1)
	args = append(args, limit)

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []PendingApproval{}
	for rows.Next() {
		var p PendingApproval
		var created time.Time
		var decidedAt *time.Time
		if err := rows.Scan(&p.ID, &p.RunID, &p.IntentID, &p.Tool, &p.ToolUseID, &p.ToolInput,
			&p.RuleID, &p.Reason, &p.Status, &p.DecidedBy, &decidedAt, &created); err != nil {
			return nil, err
		}
		p.CreatedAt = created
		p.CreatedSec = created.Unix()
		if decidedAt != nil {
			p.DecidedAt = decidedAt.Unix()
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CountPendingApprovals 给侧边栏红点用。
func (s *Store) CountPendingApprovals(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM intercept_pending WHERE status='pending'`).Scan(&n)
	return n, err
}

// ListInterceptRules 列出规则（设置页只读展示 + 审计）。
func (s *Store) ListInterceptRules(ctx context.Context) ([]InterceptRule, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, COALESCE(run_id,0), priority, match_tool, match_regex, decision, note, enabled
		  FROM intercept_rules ORDER BY priority DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []InterceptRule{}
	for rows.Next() {
		var r InterceptRule
		var runID int64
		if err := rows.Scan(&r.ID, &runID, &r.Priority, &r.MatchTool, &r.MatchRegex,
			&r.Decision, &r.Note, &r.Enabled); err != nil {
			return nil, err
		}
		if runID > 0 {
			id := runID
			r.RunID = &id
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CreateInterceptRule 加一条规则（上限校验在 server 层做）。
func (s *Store) CreateInterceptRule(ctx context.Context, r InterceptRule) (int64, error) {
	if r.Decision != "allow" && r.Decision != "deny" && r.Decision != "ask" {
		return 0, fmt.Errorf("decision 只能是 allow/deny/ask")
	}
	if r.MatchRegex != "" {
		if _, err := regexp.Compile(r.MatchRegex); err != nil {
			return 0, fmt.Errorf("正则非法：%w", err)
		}
	}
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO intercept_rules (run_id, priority, match_tool, match_regex, decision, note, enabled)
		VALUES (NULLIF($1,0), $2, $3, $4, $5, $6, $7) RETURNING id`,
		derefOrZero(r.RunID), r.Priority, r.MatchTool, r.MatchRegex, r.Decision, r.Note, r.Enabled).
		Scan(&id)
	return id, err
}

func derefOrZero(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}
