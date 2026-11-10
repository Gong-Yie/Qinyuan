package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

// 本文件是《升级计划》P3-4 的策略层：预授权（allow）/ 永久拒绝（deny）/ 批量 / 限速。
//
// 与 intercept_rules 的分工：
//   - intercept_rules：老规则，回答"要不要问人"（默认 catch-all allow）；
//   - approval_policies：策略，回答"能不能不问人就直接放行"（带限速与到期），
//     以及"无论规则怎么说都必须拒绝"（deny 不可被覆盖）。
//
// 判定顺序（两段式，deny 永远赢）：
//  1. 任何启用的 deny 策略命中 → deny
//  2. 规则里的 deny 命中 → deny
//  3. 其余策略按 priority 降序的第一条 → allow / ask（allow 受 max_per_hour 限制）
//  4. 没命中 → 回到规则表的结论（含 catch-all allow）

// ApprovalPolicy 是一条审批策略。
type ApprovalPolicy struct {
	ID         int64      `json:"id"`
	RunID      *int64     `json:"run_id,omitempty"`
	Priority   int        `json:"priority"`
	MatchTool  string     `json:"match_tool"`
	MatchRegex string     `json:"match_regex"`
	MatchScope string     `json:"match_scope"` // any | same_target | in_scope
	Decision   string     `json:"decision"`    // allow | deny | ask
	MaxPerHour int        `json:"max_per_hour,omitempty"`
	ExpiresAt  *time.Time `json:"-"`
	ExpiresSec int64      `json:"expires_at,omitempty"`
	Note       string     `json:"note"`
	Enabled    bool       `json:"enabled"`
}

// ApprovalDecision 是一次审批判定的最终结果（不含"等人"这一步）。
type ApprovalDecision struct {
	Action      string `json:"action"` // allow | ask | deny
	Source      string `json:"source"` // policy | rule | none
	PolicyID    int64  `json:"policy_id,omitempty"`
	RuleID      int64  `json:"rule_id,omitempty"`
	Note        string `json:"note,omitempty"`
	RateLimited bool   `json:"rate_limited,omitempty"`
}

// CreateApprovalPolicy 建一条策略。
//
// 护栏（没有这些就别上预授权）：
//   - allow 策略必须有 max_per_hour 与 expires_at（防"预授权 = 无限放行"）；
//   - 写/删/提权类命令永远不该被 allow 策略覆盖 —— 由 deny 段强制。
func (s *Store) CreateApprovalPolicy(ctx context.Context, p ApprovalPolicy) (int64, error) {
	switch p.Decision {
	case "allow", "deny", "ask":
	default:
		return 0, fmt.Errorf("decision 只能是 allow/deny/ask")
	}
	switch p.MatchScope {
	case "":
		p.MatchScope = "any"
	case "any", "same_target", "in_scope":
	default:
		return 0, fmt.Errorf("match_scope 只能是 any/same_target/in_scope")
	}
	if p.MatchRegex != "" {
		if _, err := regexp.Compile(p.MatchRegex); err != nil {
			return 0, fmt.Errorf("正则非法：%w", err)
		}
	}
	if p.Decision == "allow" {
		if p.MaxPerHour <= 0 {
			return 0, fmt.Errorf("allow 策略必须给 max_per_hour（防预授权变成无限放行）")
		}
		if p.ExpiresAt == nil {
			return 0, fmt.Errorf("allow 策略必须给 expires_at（最长 8 小时，过期立即失效）")
		}
		if time.Until(*p.ExpiresAt) > 8*time.Hour {
			return 0, fmt.Errorf("expires_at 最多 8 小时")
		}
	}
	// 新建即启用：撤销走 DeleteApprovalPolicy（HANDOFF 坑 #45：默认 false 会让策略被静默跳过）。
	p.Enabled = true
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO approval_policies
			(run_id, priority, match_tool, match_regex, match_scope, decision,
			 max_per_hour, expires_at, note, enabled)
		VALUES (NULLIF($1,0), $2, $3, $4, $5, $6, NULLIF($7,0), $8, $9, $10)
		RETURNING id`,
		derefOrZero(p.RunID), p.Priority, p.MatchTool, p.MatchRegex, p.MatchScope,
		p.Decision, p.MaxPerHour, p.ExpiresAt, p.Note, p.Enabled).Scan(&id)
	return id, err
}

// ListApprovalPolicies 列出策略（含过期/停用的：审计要看得见历史）。
func (s *Store) ListApprovalPolicies(ctx context.Context) ([]ApprovalPolicy, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, COALESCE(run_id,0), priority, match_tool, match_regex, match_scope,
		       decision, COALESCE(max_per_hour,0), expires_at, note, enabled
		  FROM approval_policies ORDER BY priority DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ApprovalPolicy{}
	for rows.Next() {
		var p ApprovalPolicy
		var runID int64
		if err := rows.Scan(&p.ID, &runID, &p.Priority, &p.MatchTool, &p.MatchRegex, &p.MatchScope,
			&p.Decision, &p.MaxPerHour, &p.ExpiresAt, &p.Note, &p.Enabled); err != nil {
			return nil, err
		}
		if runID > 0 {
			id := runID
			p.RunID = &id
		}
		if p.ExpiresAt != nil {
			p.ExpiresSec = p.ExpiresAt.Unix()
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeleteApprovalPolicy 一键撤销（人发现放行不对时的止损口）。
//
// 删除即失效，不依赖进程重启；正在等待的同类请求不会因此被放行
// （它走的是 pending 行，仍要人处理或按超时策略）。
func (s *Store) DeleteApprovalPolicy(ctx context.Context, id int64) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM approval_policies WHERE id=$1`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// EvaluateApproval 做一次完整判定（不含"等人"）。
//
// inScope 由调用方提供：store 不认识 domain.ScopeSet，也不该认识。
// 传 nil 时，带范围约束的策略一律视为不命中（保守）。
func (s *Store) EvaluateApproval(ctx context.Context, runID int64, tool, input, target string,
	inScope func(scope string) bool) (*ApprovalDecision, error) {

	policies, err := s.matchingPolicies(ctx, runID, tool, input, target, inScope)
	if err != nil {
		return nil, err
	}

	// 第一段：deny 不可被覆盖。
	for _, p := range policies {
		if p.Decision == "deny" {
			return &ApprovalDecision{Action: "deny", Source: "policy", PolicyID: p.ID, Note: p.Note}, nil
		}
	}

	// 第二段：规则里的 deny 同样是绝对的。
	rule, err := s.MatchInterceptRule(ctx, runID, tool, input)
	if err != nil {
		return nil, err
	}
	if rule != nil && rule.Decision == "deny" {
		return &ApprovalDecision{Action: "deny", Source: "rule", RuleID: rule.ID, Note: rule.Note}, nil
	}

	// 第三段：策略（已按 priority 降序）里的第一条 allow/ask。
	if len(policies) > 0 {
		p := policies[0]
		if p.Decision == "allow" && p.MaxPerHour > 0 {
			n, err := s.CountGrantsLastHour(ctx, p.ID)
			if err != nil {
				return nil, err
			}
			if n >= p.MaxPerHour {
				return &ApprovalDecision{
					Action: "ask", Source: "policy", PolicyID: p.ID, RateLimited: true,
					Note: fmt.Sprintf("预授权 #%d 已达上限（%d 次/小时），转人工审批", p.ID, p.MaxPerHour),
				}, nil
			}
		}
		return &ApprovalDecision{Action: p.Decision, Source: "policy", PolicyID: p.ID, Note: p.Note}, nil
	}

	// 第四段：回到规则表。
	if rule != nil {
		return &ApprovalDecision{Action: rule.Decision, Source: "rule", RuleID: rule.ID, Note: rule.Note}, nil
	}
	return &ApprovalDecision{Action: "allow", Source: "none", Note: "没有命中任何规则/策略"}, nil
}

// matchingPolicies 取本 run 适用的、命中的、未过期的策略（priority DESC）。
func (s *Store) matchingPolicies(ctx context.Context, runID int64, tool, input, target string,
	inScope func(string) bool) ([]ApprovalPolicy, error) {

	rows, err := s.pool.Query(ctx, `
		SELECT id, COALESCE(run_id,0), priority, match_tool, match_regex, match_scope,
		       decision, COALESCE(max_per_hour,0), expires_at, note
		  FROM approval_policies
		 WHERE enabled AND (run_id IS NULL OR run_id=$1)
		   AND (expires_at IS NULL OR expires_at > now())
		 ORDER BY priority DESC, id DESC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ApprovalPolicy
	for rows.Next() {
		var p ApprovalPolicy
		var rid int64
		if err := rows.Scan(&p.ID, &rid, &p.Priority, &p.MatchTool, &p.MatchRegex, &p.MatchScope,
			&p.Decision, &p.MaxPerHour, &p.ExpiresAt, &p.Note); err != nil {
			return nil, err
		}
		if rid > 0 {
			id := rid
			p.RunID = &id
		}
		if p.MatchTool != "" && p.MatchTool != tool {
			continue
		}
		if p.MatchRegex != "" {
			re, cerr := regexp.Compile(p.MatchRegex)
			if cerr != nil {
				continue
			}
			if !re.MatchString(input) {
				continue
			}
		}
		switch p.MatchScope {
		case "same_target", "in_scope":
			if inScope == nil || !inScope(p.MatchScope) {
				continue
			}
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CountGrantsLastHour 最近一小时该策略放行了几次（限速口径）。
func (s *Store) CountGrantsLastHour(ctx context.Context, policyID int64) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM approval_grants
		 WHERE policy_id=$1 AND created_at > now() - interval '1 hour'`, policyID).Scan(&n)
	return n, err
}

// RecordApprovalGrant 记一次"预授权放行"（既是审计，也是限速计数）。
func (s *Store) RecordApprovalGrant(ctx context.Context, policyID, runID int64, tool, target string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO approval_grants (policy_id, run_id, tool, target) VALUES ($1,$2,$3,$4)`,
		policyID, runID, tool, target)
	return err
}

// ListApprovalGrants 列出放行审计（按时间倒序）。
func (s *Store) ListApprovalGrants(ctx context.Context, limit int) ([]map[string]any, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT g.id, g.policy_id, g.run_id, g.tool, g.target, g.created_at
		  FROM approval_grants g ORDER BY g.id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, policyID, runID int64
		var tool, target string
		var created time.Time
		if err := rows.Scan(&id, &policyID, &runID, &tool, &target, &created); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"id": id, "policy_id": policyID, "run_id": runID,
			"tool": tool, "target": target, "created_at": created.Unix(),
		})
	}
	return out, rows.Err()
}

// BatchDecideApprovals 批量做决定：逐条 CAS，冲突的返回 id 列表（不覆盖别人已做的决定）。
func (s *Store) BatchDecideApprovals(ctx context.Context, ids []int64, decision, by, reason string) (int, []int64, error) {
	if decision != "allowed" && decision != "denied" {
		return 0, nil, errors.New("decision 只能是 allowed/denied")
	}
	if len(ids) == 0 {
		return 0, nil, errors.New("ids 不能为空")
	}
	if len(ids) > 500 {
		return 0, nil, errors.New("一次最多 500 条")
	}
	ok := 0
	conflicts := []int64{}
	for _, id := range ids {
		done, err := s.DecideApproval(ctx, id, decision, by, reason)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				conflicts = append(conflicts, id)
				continue
			}
			return ok, conflicts, err
		}
		if done {
			ok++
		} else {
			conflicts = append(conflicts, id)
		}
	}
	return ok, conflicts, nil
}
