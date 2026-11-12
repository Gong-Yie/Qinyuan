package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"qinyuan/internal/domain"
	"qinyuan/internal/store"
)

// Gate 是审批门：策略（预授权/deny/限速）→ 规则 → 人工挂起（可选判官留给后续）。
//
// 纪律（实现规格/05 §2、学习.md 8.1、升级计划 P3-4）：
//  1. **deny 不可被覆盖**：策略 deny 与规则 deny 都在 allow 之前判定；
//  2. **预授权必须有限速与到期**：否则等于把闸门开到底（CreateApprovalPolicy 里强校验）；
//  3. **人工决定不丢**：CAS（WHERE status='pending'），超时也不覆盖已做的决定；
//  4. **按风险分级超时**：低风险等到点自动放行，中/高风险到点拒绝并留痕，任务不卡死。
type Gate struct {
	Store *store.Store
	// Timeout 是"没有风险分级配置时"的兜底等待上限（默认 5 分钟）。
	Timeout time.Duration
	// OnTimeout 是兜底超时动作：deny（默认）或 allow。
	OnTimeout string
	// Poll 是轮询 DB 的间隔（默认 300ms）。
	Poll time.Duration

	// Tiers 是按风险分级的等待预算与超时动作（P3-4）。零值走默认。
	Tiers TierPolicy
	// DefaultMode 是全局默认审批模式（manual|auto|off）；run 级 approval_mode 为空时用它。
	DefaultMode string

	// Audit 在"预授权放行"时回调一次（引擎注入 → 写一条 activity）。
	// 这是审计的要求：每条不需人工就放行的调用都要能查到。
	Audit func(ctx context.Context, runID, intentID int64, d *store.ApprovalDecision, tool, target string)
}

// TierPolicy 是分级超时策略。
type TierPolicy struct {
	LowTimeout  time.Duration
	MidTimeout  time.Duration
	HighTimeout time.Duration
	LowAction   string // allow | deny
	MidAction   string
	HighAction  string
}

// DefaultTiers 是保守默认：低风险 60s 自动放行、中风险 300s 拒绝、高风险 900s 拒绝。
func DefaultTiers() TierPolicy {
	return TierPolicy{
		LowTimeout: 60 * time.Second, MidTimeout: 5 * time.Minute, HighTimeout: 15 * time.Minute,
		LowAction: "allow", MidAction: "deny", HighAction: "deny",
	}
}

// RiskTier 把一次调用归到 low / mid / high。
//
// 口径刻意保守：**只读**与 GET/HEAD 才算低风险；任何写方法算中风险；
// DELETE/提权/命令执行算高风险。宁可多问人，也不要把破坏性操作自动放行。
func RiskTier(tool string, input json.RawMessage) string {
	method := ""
	var in struct {
		Method string `json:"method"`
		URL    string `json:"url"`
	}
	if err := json.Unmarshal(input, &in); err == nil {
		method = strings.ToUpper(strings.TrimSpace(in.Method))
	}
	if method == "" && tool == "http_request" {
		method = "GET" // 工具默认 GET：判风险要看"实际会执行什么"
	}
	lower := strings.ToLower(tool)
	switch {
	case method == "DELETE", strings.Contains(lower, "exec"), strings.Contains(lower, "shell"):
		return "high"
	case method == "POST" || method == "PUT" || method == "PATCH",
		strings.Contains(lower, "write"), strings.Contains(lower, "upload"):
		return "mid"
	default:
		return "low"
	}
}

// RefusedText 是被拦下时交回模型的文案。
//
// 刻意写成"平台管控，不是目标的防御"：裸的"禁止执行"读起来像 WAF/403，
// 渗透 agent 的本能是换 payload 绕过 —— 这一句省掉几十次无效尝试。
func RefusedText(tool, reason string) string {
	return fmt.Sprintf("【钦原平台管控·非目标防御】工具 %s 被平台拦截。\n原因：%s\n"+
		"这不是目标站点/服务的防御，而是平台策略决定；换 payload、换参数、换方法都不会改变结论。"+
		"请改用其它合法手段，或把这条限制作为事实写回（inferred）。", tool, reason)
}

// normalizeToolInput 把"工具默认值"补进匹配串。
//
// 规则作者看到的应该是"**实际会执行什么**"，而不是"模型省掉了哪些字段"：
// 真实 run 里模型常写 {"url": "..."} 而省略 method（工具默认 GET），
// 若规则只匹配原始入参，一条"GET 需要审批"的规则就永远不会命中。
func normalizeToolInput(tool string, raw json.RawMessage) string {
	if tool != "http_request" || len(raw) == 0 {
		return string(raw)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return string(raw)
	}
	if s, _ := m["method"].(string); strings.TrimSpace(s) == "" {
		m["method"] = "GET"
	}
	b, err := json.Marshal(m)
	if err != nil {
		return string(raw)
	}
	return string(b)
}

// targetOf 取这次调用的目标（http 的 host），给范围约束与审计用。
func targetOf(tool string, input json.RawMessage) string {
	if tool != "http_request" {
		return ""
	}
	var in struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return ""
	}
	if host, err := domain.HostOfURL(in.URL); err == nil {
		return host
	}
	return in.URL
}

// Check 判定一次工具调用是否放行。ask 时会阻塞等待人工决定（或按风险分级超时）。
//
// onAsk 在挂起时回调一次（worker 用它发一条"等待审批"的 activity）。
func (g *Gate) Check(ctx context.Context, runID, intentID int64, tool, toolUseID string,
	input json.RawMessage, onAsk func(*store.PendingApproval)) (bool, string) {
	if g == nil || g.Store == nil {
		return true, ""
	}
	mode := g.modeFor(ctx, runID)
	// off：整个审批门跳过（包括 deny），只给 operator 在受控任务上用。
	if mode == "off" {
		return true, ""
	}
	norm := normalizeToolInput(tool, input)
	target := targetOf(tool, input)

	d, err := g.Store.EvaluateApproval(ctx, runID, tool, norm, target, g.scopeChecker(ctx, runID, target))
	if err != nil {
		// 策略读不到时放行等于门形同虚设；这里选保守：拦下并要求人工，但说明原因。
		return false, RefusedText(tool, "审批策略读取失败："+err.Error())
	}

	switch d.Action {
	case "allow":
		// 预授权放行要留痕（审计 + 限速计数）。
		if d.Source == "policy" && d.PolicyID > 0 {
			_ = g.Store.RecordApprovalGrant(ctx, d.PolicyID, runID, tool, target)
			if g.Audit != nil {
				g.Audit(ctx, runID, intentID, d, tool, target)
			}
		}
		return true, ""
	case "deny":
		return false, RefusedText(tool, firstNonEmptyStr(d.Note, "命中拒绝策略"))
	}

	// ask：auto 模式直接放行（留审计）；manual 仍然挂起等人工。
	if mode == "auto" {
		reason := "审批模式=auto，自动放行"
		if d.Note != "" {
			reason = "自动审批放行（" + d.Note + "）"
		}
		// 审计行写失败不阻塞自动化；工具调用已经允许。
		_ = g.Store.RecordAutoApproval(ctx, runID, intentID, tool, toolUseID, norm, reason)
		if g.Audit != nil {
			g.Audit(ctx, runID, intentID, &store.ApprovalDecision{
				Action: "allow", Source: "auto", Note: reason,
			}, tool, target)
		}
		return true, ""
	}
	return g.ask(ctx, runID, intentID, tool, toolUseID, input, norm, d, onAsk)
}

// modeFor 取 run 级模式；空/非法/读不到时回落到全局默认，最后兜底 manual。
func (g *Gate) modeFor(ctx context.Context, runID int64) string {
	if g.Store != nil {
		if m, err := g.Store.RunApprovalMode(ctx, runID); err == nil {
			switch m {
			case "manual", "auto", "off":
				return m
			}
		}
	}
	switch g.DefaultMode {
	case "manual", "auto", "off":
		return g.DefaultMode
	default:
		return "manual"
	}
}

func (g *Gate) ask(ctx context.Context, runID, intentID int64, tool, toolUseID string,
	input json.RawMessage, norm string, d *store.ApprovalDecision,
	onAsk func(*store.PendingApproval)) (bool, string) {

	tier := RiskTier(tool, input)
	timeout, onTimeout := g.tierSettings(tier)
	poll := g.Poll
	if poll <= 0 {
		poll = 300 * time.Millisecond
	}
	reason := firstNonEmptyStr(d.Note, "命中审批规则，需要人工确认")
	if d.RateLimited {
		reason = d.Note
	}
	ruleID := d.RuleID
	p, err := g.Store.CreatePendingApproval(ctx, runID, intentID, tool, toolUseID, norm, reason, ruleID)
	if err != nil {
		return false, RefusedText(tool, "创建审批失败："+err.Error())
	}
	if onAsk != nil {
		onAsk(p)
	}

	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// 任务被暂停/取消：撤销这次审批，别让它悬着。
			_, _ = g.Store.TimeoutApproval(context.WithoutCancel(ctx), p.ID, "任务暂停/取消")
			return false, RefusedText(tool, "任务暂停或取消，审批已撤销（审批 #"+strconv.FormatInt(p.ID, 10)+"）")
		case <-ticker.C:
			cur, aerr := g.Store.Approval(ctx, p.ID)
			if aerr != nil {
				continue
			}
			switch cur.Status {
			case "allowed":
				return true, ""
			case "denied":
				return false, RefusedText(tool, "人工拒绝："+cur.Reason)
			case "timeout":
				return false, RefusedText(tool, "审批超时未处理："+cur.Reason)
			}
			if time.Now().After(deadline) {
				if onTimeout == "allow" {
					_, _ = g.Store.DecideApproval(context.WithoutCancel(ctx), p.ID, "allowed", "timeout-"+tier, "超时按风险分级放行")
					if g.Audit != nil {
						g.Audit(ctx, runID, intentID, &store.ApprovalDecision{
							Action: "allow", Source: "timeout", Note: "超过 " + timeout.String() + " 未处理，按风险分级自动放行",
						}, tool, targetOf(tool, input))
					}
					return true, ""
				}
				// 中/高风险：到点拒绝并留记录（夜间无人值守时不卡死，也不会被放开）。
				_, _ = g.Store.TimeoutApproval(context.WithoutCancel(ctx), p.ID,
					fmt.Sprintf("%s 风险等待超时（%s）", tier, timeout))
				return false, RefusedText(tool, fmt.Sprintf("等待人工审批超时（%s，风险等级 %s）", timeout, tier))
			}
		}
	}
}

// tierSettings 取某个风险档的等待预算与超时动作（零值走 DefaultTiers）。
func (g *Gate) tierSettings(tier string) (time.Duration, string) {
	t := g.Tiers
	// 完全没配分级（老调用方/老测试）→ 回到单档兜底，行为与 M6a 逐字节一致。
	// 生产路径由 engine.New 显式注入 DefaultTiers，所以这里不必替它做决定。
	configured := t.LowTimeout > 0 || t.MidTimeout > 0 || t.HighTimeout > 0
	switch tier {
	case "high":
		if configured && t.HighTimeout > 0 {
			return t.HighTimeout, firstNonEmptyStr(t.HighAction, "deny")
		}
	case "mid":
		if configured && t.MidTimeout > 0 {
			return t.MidTimeout, firstNonEmptyStr(t.MidAction, "deny")
		}
	default:
		if configured && t.LowTimeout > 0 {
			return t.LowTimeout, firstNonEmptyStr(t.LowAction, "allow")
		}
	}
	// 没配分级：回到兜底单档（M6a 的老行为，便于回归）。
	timeout := g.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	return timeout, firstNonEmptyStr(g.OnTimeout, "deny")
}

// scopeChecker 给策略的 match_scope 提供判定（惰性加载范围与起点，只在需要时查库）。
func (g *Gate) scopeChecker(ctx context.Context, runID int64, target string) func(string) bool {
	if target == "" {
		return nil
	}
	var loaded bool
	var inScope, sameTarget bool
	load := func() {
		if loaded {
			return
		}
		loaded = true
		scopes, err := g.Store.Scopes(ctx, runID)
		if err != nil {
			return
		}
		inScope = domain.NewScopeSet(scopes).AllowsHost(target)
		run, err := g.Store.Run(ctx, runID)
		if err != nil || run == nil {
			return
		}
		for _, seed := range run.Seeds {
			host, herr := domain.HostOfURL(seed)
			if herr == nil && strings.EqualFold(host, target) {
				sameTarget = true
				break
			}
		}
	}
	return func(scope string) bool {
		load()
		switch scope {
		case "in_scope":
			return inScope
		case "same_target":
			return sameTarget
		}
		return false
	}
}
