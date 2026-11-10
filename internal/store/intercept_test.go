package store

import (
	"context"
	"testing"
)

// 规则匹配：deny 最高优先；其次按 priority 降序；正则只匹配参数串。
func TestInterceptRuleMatching(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	run := newTestRun(t, s)

	mustCreate := func(r InterceptRule) {
		t.Helper()
		if _, err := s.CreateInterceptRule(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	// 默认种子里有 catch-all allow（priority 0）+ DELETE ask（100）。
	mustCreate(InterceptRule{Priority: 500, MatchTool: "demo_tool", Decision: "ask", Note: "demo 要问", Enabled: true})
	mustCreate(InterceptRule{Priority: 50, MatchTool: "demo_tool", MatchRegex: `"danger":true`, Decision: "deny", Note: "危险参数", Enabled: true})

	// 1) 未命中 demo 规则、也不是 DELETE → 落 catch-all allow。
	r, err := s.MatchInterceptRule(ctx, run.ID, "http_request", `{"method":"GET"}`)
	if err != nil {
		t.Fatal(err)
	}
	if r == nil || r.Decision != "allow" {
		t.Fatalf("默认应命中 catch-all allow，实际 %+v", r)
	}

	// 2) DELETE 命中种子 ask 规则。
	r, err = s.MatchInterceptRule(ctx, run.ID, "http_request", `{"method":"DELETE","url":"http://x/"}`)
	if err != nil {
		t.Fatal(err)
	}
	if r == nil || r.Decision != "ask" {
		t.Fatalf("DELETE 应命中 ask，实际 %+v", r)
	}

	// 3) demo_tool 命中 ask（priority 500）。
	r, err = s.MatchInterceptRule(ctx, run.ID, "demo_tool", `{"a":1}`)
	if err != nil {
		t.Fatal(err)
	}
	if r == nil || r.Decision != "ask" {
		t.Fatalf("demo_tool 应命中 ask，实际 %+v", r)
	}

	// 4) deny 永远赢：即使 allow 的优先级更高。
	mustCreate(InterceptRule{Priority: 900, MatchTool: "demo_tool", Decision: "allow", Note: "更高优先级的 allow", Enabled: true})
	r, err = s.MatchInterceptRule(ctx, run.ID, "demo_tool", `{"danger":true}`)
	if err != nil {
		t.Fatal(err)
	}
	if r == nil || r.Decision != "deny" {
		t.Fatalf("deny 必须赢，实际 %+v", r)
	}
}

// 人工决定是 CAS：重复决定不覆盖，超时也不覆盖已做的决定。
func TestApprovalDecisionIsCAS(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	run := newTestRun(t, s)

	p, err := s.CreatePendingApproval(ctx, run.ID, 0, "http_request", "call_1",
		`{"method":"DELETE"}`, "DELETE 需要审批", 0)
	if err != nil {
		t.Fatal(err)
	}

	ok, err := s.DecideApproval(ctx, p.ID, "allowed", "alice", "同意")
	if err != nil || !ok {
		t.Fatalf("第一次决定应成功：ok=%v err=%v", ok, err)
	}
	// 第二次（另一个决定/超时）必须失败，不覆盖。
	if ok, _ := s.DecideApproval(ctx, p.ID, "denied", "bob", "反悔"); ok {
		t.Fatal("重复决定不该成功")
	}
	if ok, _ := s.TimeoutApproval(ctx, p.ID, "超时"); ok {
		t.Fatal("已决定的审批不该被改写成 timeout")
	}

	cur, err := s.Approval(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.Status != "allowed" || cur.DecidedBy != "alice" {
		t.Fatalf("状态被覆盖了：%+v", cur)
	}
	// 侧边栏红点只算 pending。
	if n, _ := s.CountPendingApprovals(ctx); n != 0 {
		t.Fatalf("pending 计数 = %d，期望 0", n)
	}
}
