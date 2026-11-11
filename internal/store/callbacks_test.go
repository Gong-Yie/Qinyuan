package store

import (
	"context"
	"testing"
	"time"
)

// M17：回连白名单是"开口"，CRUD 与过期/吊销语义必须精确。
func TestRunCallbacksCRUD(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	run := newTestRun(t, s)

	cb, err := s.AddCallback(ctx, run.ID, "198.51.100.9", "tcp", "40000-40100", "反弹 shell", "operator", 30*time.Minute)
	if err != nil {
		t.Fatalf("创建失败：%v", err)
	}
	if cb.ExpiresAt.Before(time.Now()) {
		t.Fatalf("过期时间不对：%v", cb.ExpiresAt)
	}

	active, err := s.ActiveCallbacks(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].Host != "198.51.100.9" {
		t.Fatalf("生效列表不对：%+v", active)
	}

	// 吊销后立刻从"生效"里消失，但审计列表里还在。
	ok, err := s.RevokeCallback(ctx, run.ID, cb.ID)
	if err != nil || !ok {
		t.Fatalf("吊销失败：ok=%v err=%v", ok, err)
	}
	active, _ = s.ActiveCallbacks(ctx, run.ID)
	if len(active) != 0 {
		t.Fatalf("吊销后不该生效：%+v", active)
	}
	all, _ := s.Callbacks(ctx, run.ID)
	if len(all) != 1 || all[0].RevokedAt == nil {
		t.Fatalf("审计列表应保留已吊销项：%+v", all)
	}
	// 幂等：再吊销一次返回 false。
	if ok, _ := s.RevokeCallback(ctx, run.ID, cb.ID); ok {
		t.Fatal("重复吊销应返回 false")
	}
}

// 过期的不算生效。
func TestRunCallbacksExpired(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	run := newTestRun(t, s)

	cb, err := s.AddCallback(ctx, run.ID, "203.0.113.5", "tcp", "4444", "", "operator", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// 直接把过期时间推到过去（不睡）。
	if _, err := s.Pool().Exec(ctx, `UPDATE run_callbacks SET expires_at=now() - interval '1 minute' WHERE id=$1`, cb.ID); err != nil {
		t.Fatal(err)
	}
	active, err := s.ActiveCallbacks(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("过期的不该生效：%+v", active)
	}
}

// 校验：端口/协议非法一律拒绝（白名单不允许"差不多"）。
func TestAddCallbackValidation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	run := newTestRun(t, s)

	cases := []struct{ host, proto, ports string }{
		{"", "tcp", "4444"},
		{"1.2.3.4", "icmp", "4444"},
		{"1.2.3.4", "tcp", "abc"},
		{"1.2.3.4", "tcp", "0"},
		{"1.2.3.4", "tcp", "70000"},
		{"1.2.3.4", "tcp", "500-400"},
	}
	for _, c := range cases {
		if _, err := s.AddCallback(ctx, run.ID, c.host, c.proto, c.ports, "", "operator", time.Minute); err == nil {
			t.Fatalf("%+v 应当被拒绝", c)
		}
	}
}
