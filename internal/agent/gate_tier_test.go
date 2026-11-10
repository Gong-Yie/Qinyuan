package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"qinyuan/internal/store"
	"qinyuan/internal/testkit"
)

// P3-4：风险分级必须保守 —— 只读/GET 才是低风险，DELETE 与命令执行是高危。
func TestRiskTier(t *testing.T) {
	cases := []struct {
		tool, input, want string
	}{
		{"http_request", `{"method":"GET","url":"http://x/"}`, "low"},
		{"http_request", `{"url":"http://x/"}`, "low"}, // 省略 method 时默认 GET
		{"http_request", `{"method":"POST","url":"http://x/"}`, "mid"},
		{"http_request", `{"method":"PUT","url":"http://x/"}`, "mid"},
		{"http_request", `{"method":"DELETE","url":"http://x/"}`, "high"},
		{"shell_exec", `{}`, "high"},
		{"node_detail", `{}`, "low"},
	}
	for _, c := range cases {
		if got := RiskTier(c.tool, json.RawMessage(c.input)); got != c.want {
			t.Fatalf("RiskTier(%s, %s) = %s，期望 %s", c.tool, c.input, got, c.want)
		}
	}
}

// 预授权放行：不产生 pending 行，但必须留下审计（grant + Audit 回调）。
func TestGatePreAuthAllowRecordsGrant(t *testing.T) {
	st := testkit.Store(t)
	run := gateRun(t, st)
	exp := time.Now().Add(time.Hour)
	pid, err := st.CreateApprovalPolicy(context.Background(), store.ApprovalPolicy{
		Priority: 500, Decision: "allow", MaxPerHour: 10, ExpiresAt: &exp,
		MatchTool: "http_request", Note: "低风险预授权",
	})
	if err != nil {
		t.Fatal(err)
	}

	var audits int
	g := &Gate{Store: st, Tiers: DefaultTiers(),
		Audit: func(context.Context, int64, int64, *store.ApprovalDecision, string, string) { audits++ },
	}
	ok, msg := g.Check(context.Background(), run.ID, 0, "http_request", "c1",
		json.RawMessage(`{"method":"GET","url":"http://127.0.0.1:8080/"}`), nil)
	if !ok {
		t.Fatalf("预授权应放行，实际被拦：%s", msg)
	}
	if audits != 1 {
		t.Fatalf("预授权放行必须留审计，实际回调 %d 次", audits)
	}
	n, err := st.CountGrantsLastHour(context.Background(), pid)
	if err != nil || n != 1 {
		t.Fatalf("放行计数 = %d（%v），期望 1（限速靠它）", n, err)
	}
	if items, _ := st.ListApprovals(context.Background(), "pending", 10); len(items) != 0 {
		t.Fatalf("预授权不该产生待审批行，实际 %d 条", len(items))
	}
}

// 低风险等到点自动放行（夜间无人值守不卡死）。
func TestGateLowTierTimeoutAllows(t *testing.T) {
	st := testkit.Store(t)
	run := gateRun(t, st)
	if _, err := st.CreateInterceptRule(context.Background(), store.InterceptRule{
		Priority: 900, MatchTool: "http_request", MatchRegex: `"method":\s*"GET"`,
		Decision: "ask", Note: "要求人工确认", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	g := &Gate{Store: st, Poll: 20 * time.Millisecond,
		Tiers: TierPolicy{LowTimeout: 120 * time.Millisecond, LowAction: "allow"}}

	start := time.Now()
	var pid int64
	ok, msg := g.Check(context.Background(), run.ID, 0, "http_request", "c2",
		json.RawMessage(`{"method":"GET","url":"http://127.0.0.1:8080/"}`),
		func(p *store.PendingApproval) { pid = p.ID })
	if !ok {
		t.Fatalf("低风险超时应放行，实际被拦：%s", msg)
	}
	if time.Since(start) < 100*time.Millisecond {
		t.Fatalf("没有等到超时就放行了（%v）", time.Since(start))
	}
	cur, err := st.Approval(context.Background(), pid)
	if err != nil {
		t.Fatal(err)
	}
	if cur.Status != "allowed" || !strings.HasPrefix(cur.DecidedBy, "timeout") {
		t.Fatalf("超时放行必须落成 allowed + timeout 决策者：%+v", cur)
	}
}

// 高风险到点拒绝（并留记录），不会被自动放开。
func TestGateHighTierTimeoutDenies(t *testing.T) {
	st := testkit.Store(t)
	run := gateRun(t, st)
	g := &Gate{Store: st, Poll: 20 * time.Millisecond,
		Tiers: TierPolicy{HighTimeout: 100 * time.Millisecond, HighAction: "deny"}}

	var pid int64
	ok, msg := g.Check(context.Background(), run.ID, 0, "http_request", "c3",
		json.RawMessage(`{"method":"DELETE","url":"http://127.0.0.1:8080/admin"}`),
		func(p *store.PendingApproval) { pid = p.ID })
	if ok {
		t.Fatal("高风险超时必须拒绝")
	}
	if !strings.Contains(msg, "风险等级 high") {
		t.Fatalf("拒绝文案应说明风险等级：%s", msg)
	}
	cur, err := st.Approval(context.Background(), pid)
	if err != nil || cur.Status != "timeout" {
		t.Fatalf("审批应标 timeout：%+v err=%v", cur, err)
	}
}
