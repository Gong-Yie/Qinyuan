package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"qinyuan/internal/model"
	"qinyuan/internal/store"
	"qinyuan/internal/testkit"
)

func gateRun(t *testing.T, st *store.Store) *model.Run {
	t.Helper()
	run, err := st.CreateRun(context.Background(), store.CreateRunInput{
		Goal: "审批门测试", WorkerCount: 1,
		Scopes:       []model.Scope{{Kind: "ip", Value: "127.0.0.1", Source: "manual"}},
		SeedEntities: []store.EntityInput{{Kind: "ip", IP: "127.0.0.1", Segment: "127.0.0.0/24"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// 默认策略是显式 allow 规则：不打断正常探测。
func TestGateAllowsByDefaultRule(t *testing.T) {
	st := testkit.Store(t)
	run := gateRun(t, st)
	g := &Gate{Store: st}
	ok, msg := g.Check(context.Background(), run.ID, 0, "http_request", "call_1",
		json.RawMessage(`{"method":"GET","url":"http://127.0.0.1:8080/"}`), nil)
	if !ok {
		t.Fatalf("默认应放行，实际被拦：%s", msg)
	}
}

// deny 规则命中：拦下，并且文案是"平台管控"（不是 WAF 口吻）。
func TestGateDenyBeatsAllow(t *testing.T) {
	st := testkit.Store(t)
	run := gateRun(t, st)

	// 高优先级 allow + 低优先级 deny：deny 必须赢。
	if _, err := st.CreateInterceptRule(context.Background(), store.InterceptRule{
		Priority: 900, MatchTool: "demo_tool", Decision: "allow", Note: "想放行", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInterceptRule(context.Background(), store.InterceptRule{
		Priority: 10, MatchTool: "demo_tool", Decision: "deny", Note: "测试禁止", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	g := &Gate{Store: st}
	ok, msg := g.Check(context.Background(), run.ID, 0, "demo_tool", "call_2",
		json.RawMessage(`{}`), nil)
	if ok {
		t.Fatal("deny 规则必须赢，实际放行了")
	}
	if !strings.Contains(msg, "平台管控") || !strings.Contains(msg, "测试禁止") {
		t.Fatalf("拒绝文案不对：%s", msg)
	}
}

// ask → 挂起 → 人工批准 → 放行。
func TestGateAskThenAllow(t *testing.T) {
	st := testkit.Store(t)
	run := gateRun(t, st)
	if _, err := st.CreateInterceptRule(context.Background(), store.InterceptRule{
		Priority: 800, MatchTool: "demo_tool", Decision: "ask", Note: "需要确认", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	g := &Gate{Store: st, Timeout: 5 * time.Second, Poll: 20 * time.Millisecond}
	asked := make(chan int64, 1)
	done := make(chan struct {
		ok  bool
		msg string
	}, 1)
	go func() {
		ok, msg := g.Check(context.Background(), run.ID, 0, "demo_tool", "call_3",
			json.RawMessage(`{"x":1}`), func(p *store.PendingApproval) { asked <- p.ID })
		done <- struct {
			ok  bool
			msg string
		}{ok, msg}
	}()

	var pid int64
	select {
	case pid = <-asked:
	case <-time.After(2 * time.Second):
		t.Fatal("没有挂起审批")
	}
	if pid <= 0 {
		t.Fatal("审批 id 非法")
	}
	if ok, err := st.DecideApproval(context.Background(), pid, "allowed", "test", "同意"); err != nil || !ok {
		t.Fatalf("批准失败：ok=%v err=%v", ok, err)
	}

	select {
	case res := <-done:
		if !res.ok {
			t.Fatalf("批准后应放行，实际被拦：%s", res.msg)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Check 没有返回")
	}
	cur, err := st.Approval(context.Background(), pid)
	if err != nil || cur.Status != "allowed" || cur.DecidedBy != "test" {
		t.Fatalf("审批状态不对：%+v err=%v", cur, err)
	}
}

// 等超时 = 拒绝（默认），并且行标 timeout —— 不能悬着。
func TestGateAskTimeoutDenies(t *testing.T) {
	st := testkit.Store(t)
	run := gateRun(t, st)
	if _, err := st.CreateInterceptRule(context.Background(), store.InterceptRule{
		Priority: 800, MatchTool: "demo_tool", Decision: "ask", Note: "要等人", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	g := &Gate{Store: st, Timeout: 150 * time.Millisecond, Poll: 30 * time.Millisecond}
	var pid int64
	ok, msg := g.Check(context.Background(), run.ID, 0, "demo_tool", "call_4",
		json.RawMessage(`{}`), func(p *store.PendingApproval) { pid = p.ID })
	if ok {
		t.Fatal("超时应拒绝")
	}
	if !strings.Contains(msg, "超时") {
		t.Fatalf("超时文案不对：%s", msg)
	}
	cur, err := st.Approval(context.Background(), pid)
	if err != nil || cur.Status != "timeout" {
		t.Fatalf("审批状态应为 timeout：%+v err=%v", cur, err)
	}
}

// 规则必须看"实际会执行什么"：模型省略 method 时工具默认 GET，规则要能命中。
func TestGateNormalizesDefaultHTTPMethod(t *testing.T) {
	st := testkit.Store(t)
	run := gateRun(t, st)
	if _, err := st.CreateInterceptRule(context.Background(), store.InterceptRule{
		Priority: 850, MatchTool: "http_request",
		MatchRegex: `"method"\s*:\s*"GET"`, Decision: "ask",
		Note: "GET 需要审批（测试）", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	g := &Gate{Store: st, Timeout: 120 * time.Millisecond, Poll: 25 * time.Millisecond}
	var pid int64
	ok, msg := g.Check(context.Background(), run.ID, 0, "http_request", "call_5",
		json.RawMessage(`{"url":"http://127.0.0.1:8080/"}`), // 没有 method
		func(p *store.PendingApproval) { pid = p.ID })
	if ok {
		t.Fatal("省略 method 也应命中 GET 规则（超时拒绝）")
	}
	if !strings.Contains(msg, "超时") {
		t.Fatalf("应为超时拒绝，实际：%s", msg)
	}
	if pid == 0 {
		t.Fatal("没有挂起审批")
	}
}
