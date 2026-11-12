package store

import (
	"context"
	"testing"
)

func TestRunApprovalModeCRUD(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	run := newTestRun(t, s)

	mode, err := s.RunApprovalMode(ctx, run.ID)
	if err != nil || mode != "" {
		t.Fatalf("新任务应跟随全局（空），实际 %q err=%v", mode, err)
	}
	ok, err := s.SetRunApprovalMode(ctx, run.ID, "auto")
	if err != nil || !ok {
		t.Fatalf("设置 auto 失败：%v %v", ok, err)
	}
	mode, _ = s.RunApprovalMode(ctx, run.ID)
	if mode != "auto" {
		t.Fatalf("模式应为 auto，实际 %q", mode)
	}
	if _, err := s.SetRunApprovalMode(ctx, run.ID, "bypass"); err == nil {
		t.Fatal("非法模式应被拒绝")
	}
}

func TestRecordAutoApproval(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	run := newTestRun(t, s)

	if err := s.RecordAutoApproval(ctx, run.ID, 0, "demo_tool", "call_1", `{}`, "auto 测试"); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListApprovals(ctx, "allowed", 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range rows {
		if p.Tool == "demo_tool" && p.DecidedBy == "auto" {
			found = true
		}
	}
	if !found {
		t.Fatalf("应有一条 auto 批准的记录：%+v", rows)
	}
}

func TestApprovePendingForRun(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	run := newTestRun(t, s)

	p, err := s.CreatePendingApproval(ctx, run.ID, 0, "demo_tool", "call_2", `{}`, "等人", 0)
	if err != nil {
		t.Fatal(err)
	}
	n, err := s.ApprovePendingForRun(ctx, run.ID, "auto-mode", "切换 auto")
	if err != nil || n != 1 {
		t.Fatalf("应放行 1 条：n=%d err=%v", n, err)
	}
	cur, err := s.Approval(ctx, p.ID)
	if err != nil || cur.Status != "allowed" || cur.DecidedBy != "auto-mode" {
		t.Fatalf("审批状态不对：%+v err=%v", cur, err)
	}
}
