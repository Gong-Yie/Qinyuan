package store

import (
	"context"
	"testing"
	"time"
)

// P3-4 的核心不变量：deny 不可被覆盖（策略 deny 盖住策略 allow，也盖住规则 allow）。
func TestApprovalPolicyDenyBeatsAllow(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	run := newTestRun(t, s)

	exp := time.Now().Add(time.Hour)
	if _, err := s.CreateApprovalPolicy(ctx, ApprovalPolicy{
		Priority: 200, Decision: "allow", MaxPerHour: 10, ExpiresAt: &exp,
		Note: "同目标只读放行",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateApprovalPolicy(ctx, ApprovalPolicy{
		Priority: 50, Decision: "deny", Note: "永不回调内网元数据",
	}); err != nil {
		t.Fatal(err)
	}

	d, err := s.EvaluateApproval(ctx, run.ID, "http_request", `{"url":"http://x/","method":"GET"}`, "x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != "deny" || d.Source != "policy" {
		t.Fatalf("deny 策略应压过 allow 策略：%+v", d)
	}

	// 规则里的 deny 也压过策略 allow：先删掉 deny 策略，再单独造一条规则 deny。
	pols, _ := s.ListApprovalPolicies(ctx)
	for _, p := range pols {
		if p.Decision == "deny" {
			_, _ = s.DeleteApprovalPolicy(ctx, p.ID)
		}
	}
	if _, err := s.CreateInterceptRule(ctx, InterceptRule{
		Priority: 999, MatchRegex: `(?i)"url"`, Decision: "deny", Note: "规则层拒绝", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	d, err = s.EvaluateApproval(ctx, run.ID, "http_request", `{"url":"http://x/","method":"GET"}`, "x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != "deny" || d.Source != "rule" {
		t.Fatalf("规则 deny 应压过策略 allow：%+v", d)
	}
}

// 预授权的限速：放到上限就转人工（ask），而不是无限放行。
func TestApprovalPolicyRateLimitTurnsAsk(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	run := newTestRun(t, s)

	exp := time.Now().Add(time.Hour)
	pid, err := s.CreateApprovalPolicy(ctx, ApprovalPolicy{
		Priority: 300, Decision: "allow", MaxPerHour: 2, ExpiresAt: &exp, Note: "低风险放行",
	})
	if err != nil {
		t.Fatal(err)
	}
	input := `{"url":"http://x/a","method":"GET"}`
	d, err := s.EvaluateApproval(ctx, run.ID, "http_request", input, "x", nil)
	if err != nil || d.Action != "allow" || d.PolicyID != pid {
		t.Fatalf("首次应放行：%+v err=%v", d, err)
	}
	for i := 0; i < 2; i++ {
		if err := s.RecordApprovalGrant(ctx, pid, run.ID, "http_request", "x"); err != nil {
			t.Fatal(err)
		}
	}
	d, err = s.EvaluateApproval(ctx, run.ID, "http_request", input, "x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != "ask" || !d.RateLimited {
		t.Fatalf("达到 max_per_hour 后应转人工：%+v", d)
	}
}

// 到期的预授权立即失效（不依赖进程重启）。
func TestApprovalPolicyExpiredIgnored(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	run := newTestRun(t, s)

	past := time.Now().Add(-time.Minute)
	if _, err := s.CreateApprovalPolicy(ctx, ApprovalPolicy{
		Priority: 300, Decision: "allow", MaxPerHour: 5, ExpiresAt: &past,
	}); err == nil {
		// 允许创建过去时间（审计/补录），但必须不生效。
	}
	d, err := s.EvaluateApproval(ctx, run.ID, "http_request", `{"url":"http://x/"`, "x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.Source == "policy" {
		t.Fatalf("过期策略不该生效：%+v", d)
	}
}

// 带范围约束的策略：inScope 说不在范围内就不命中。
func TestApprovalPolicyScopeConstraint(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	run := newTestRun(t, s)

	exp := time.Now().Add(time.Hour)
	if _, err := s.CreateApprovalPolicy(ctx, ApprovalPolicy{
		Priority: 300, Decision: "allow", MaxPerHour: 5, ExpiresAt: &exp,
		MatchScope: "in_scope", Note: "只在授权范围内预授权",
	}); err != nil {
		t.Fatal(err)
	}
	d, err := s.EvaluateApproval(ctx, run.ID, "http_request", `{"url":"http://x/"}`, "x",
		func(scope string) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if d.Source == "policy" {
		t.Fatalf("范围不满足时不该命中策略：%+v", d)
	}
	d, err = s.EvaluateApproval(ctx, run.ID, "http_request", `{"url":"http://x/"}`, "x",
		func(scope string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != "allow" || d.Source != "policy" {
		t.Fatalf("范围满足时应命中策略：%+v", d)
	}
}

// allow 策略的护栏：没有限速/到期就不许建。
func TestApprovalPolicyAllowRequiresGuards(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.CreateApprovalPolicy(ctx, ApprovalPolicy{Decision: "allow"}); err == nil {
		t.Fatal("没有 max_per_hour 的 allow 策略必须被拒")
	}
	exp := time.Now().Add(9 * time.Hour)
	if _, err := s.CreateApprovalPolicy(ctx, ApprovalPolicy{
		Decision: "allow", MaxPerHour: 1, ExpiresAt: &exp,
	}); err == nil {
		t.Fatal("到期时间超过 8 小时的 allow 策略必须被拒")
	}
}

// 批量决定：已处理的条目不覆盖，作为 conflict 返回。
func TestBatchDecideReportsConflicts(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	run := newTestRun(t, s)

	p1, err := s.CreatePendingApproval(ctx, run.ID, 0, "http_request", "c1", `{}`, "测试", 0)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := s.CreatePendingApproval(ctx, run.ID, 0, "http_request", "c2", `{}`, "测试", 0)
	if err != nil {
		t.Fatal(err)
	}
	// 人工先批了 p1。
	if ok, err := s.DecideApproval(ctx, p1.ID, "allowed", "ui", "先批一条"); err != nil || !ok {
		t.Fatalf("预置批准失败：%v", err)
	}

	ok, conflicts, err := s.BatchDecideApprovals(ctx, []int64{p1.ID, p2.ID, 999999}, "allowed", "batch", "")
	if err != nil {
		t.Fatal(err)
	}
	if ok != 1 || len(conflicts) != 2 {
		t.Fatalf("批量结果不对：ok=%d conflicts=%v（期望 1 与 2）", ok, conflicts)
	}
	if conflicts[0] != p1.ID {
		t.Fatalf("冲突列表应包含已处理的 #%d：%v", p1.ID, conflicts)
	}
	cur, err := s.Approval(ctx, p1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.DecidedBy != "ui" {
		t.Fatalf("批量不能覆盖人工决定：decided_by=%q", cur.DecidedBy)
	}
}
