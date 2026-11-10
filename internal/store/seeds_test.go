package store

import (
	"context"
	"testing"
)

// P0：seeds 是任务输入的一部分，建完要能读回来（单条 + 列表两条读路径）。
func TestCreateRunKeepsSeeds(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	want := "http://127.0.0.1:8080/"
	run, err := s.CreateRun(ctx, CreateRunInput{
		Goal: "指纹", WorkerCount: 1, Seeds: []string{want},
	})
	if err != nil {
		t.Fatalf("建 run 失败：%v", err)
	}

	got, err := s.Run(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Seeds) != 1 || got.Seeds[0] != want {
		t.Fatalf("Run().Seeds = %#v，期望 [%q]", got.Seeds, want)
	}

	list, err := s.ListRuns(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range list {
		if r.ID != run.ID {
			continue
		}
		found = true
		if len(r.Seeds) != 1 || r.Seeds[0] != want {
			t.Fatalf("ListRuns().Seeds = %#v，期望 [%q]", r.Seeds, want)
		}
	}
	if !found {
		t.Fatalf("列表里没有 run #%d", run.ID)
	}
}

// 没有 seeds 的 run 必须读成空切片（不是 NULL、也不是 panic）。
func TestCreateRunWithoutSeeds(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	run, err := s.CreateRun(ctx, CreateRunInput{Goal: "无入口", WorkerCount: 1})
	if err != nil {
		t.Fatalf("建 run 失败：%v", err)
	}
	got, err := s.Run(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Seeds) != 0 {
		t.Fatalf("Seeds = %#v，期望空", got.Seeds)
	}
}
