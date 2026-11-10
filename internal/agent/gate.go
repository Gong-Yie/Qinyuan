package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"qinyuan/internal/store"
)

// Gate 是审批门：规则（DB）→ 人工挂起（可选判官留给后续）。
//
// 三条纪律（实现规格/05 §2、学习.md 8.1）：
//  1. **默认放行要有规则可归因**：DB 里有 catch-all allow 规则，"为什么放行"能回答；
//  2. **deny 不可被覆盖**：MatchInterceptRule 里 deny 直接短路，不参与优先级比较；
//  3. **人工决定不丢**：CAS（WHERE status='pending'），超时不覆盖已做的决定。
type Gate struct {
	Store *store.Store
	// Timeout 是等人审批的上限（默认 5 分钟）。
	Timeout time.Duration
	// OnTimeout 是超时后的动作：deny（默认，安全）或 allow。
	OnTimeout string
	// Poll 是轮询 DB 的间隔（默认 300ms）。
	Poll time.Duration
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

// Check 判定一次工具调用是否放行。ask 时会阻塞等待人工决定（或超时）。
//
// onAsk 在挂起时回调一次（worker 用它发一条"等待审批"的 activity）。
func (g *Gate) Check(ctx context.Context, runID, intentID int64, tool, toolUseID string,
	input json.RawMessage, onAsk func(*store.PendingApproval)) (bool, string) {
	if g == nil || g.Store == nil {
		return true, ""
	}
	rule, err := g.Store.MatchInterceptRule(ctx, runID, tool, normalizeToolInput(tool, input))
	if err != nil {
		// 规则查不到时**放行**会让门形同虚设；这里选保守：拦下并要求人工，但说明原因。
		return false, RefusedText(tool, "审批规则读取失败："+err.Error())
	}
	if rule == nil {
		return true, ""
	}
	switch rule.Decision {
	case "allow":
		return true, ""
	case "deny":
		return false, RefusedText(tool, rule.Note)
	}

	// ask：挂起等人工
	timeout := g.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	poll := g.Poll
	if poll <= 0 {
		poll = 300 * time.Millisecond
	}
	reason := rule.Note
	if reason == "" {
		reason = "命中审批规则，需要人工确认"
	}
	p, err := g.Store.CreatePendingApproval(ctx, runID, intentID, tool, toolUseID, string(input), reason, rule.ID)
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
				if strings.EqualFold(g.OnTimeout, "allow") {
					_, _ = g.Store.DecideApproval(context.WithoutCancel(ctx), p.ID, "allowed", "timeout", "超时按策略放行")
					return true, ""
				}
				_, _ = g.Store.TimeoutApproval(context.WithoutCancel(ctx), p.ID, "等待超时")
				return false, RefusedText(tool, fmt.Sprintf("等待人工审批超时（%s）", timeout))
			}
		}
	}
}
