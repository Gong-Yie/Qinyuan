package store

import (
	"context"
	"testing"

	"qinyuan/internal/model"
)

// IP 目标的覆盖度：runplan 把 IP 字面量写进 run_scope.value（net 为空），
// 早先 Coverage 只匹配 sc.net，于是这类任务永远是 0/0（控制台上就是"覆盖率 0%"）。
func TestCoverageMatchesIPScopeByValue(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	run, err := s.CreateRun(ctx, CreateRunInput{
		Goal: "IP 覆盖度", WorkerCount: 1,
		Scopes:       []model.Scope{{Kind: "ip", Value: "127.0.0.1", Source: "manual"}},
		SeedEntities: []EntityInput{{Kind: "ip", IP: "127.0.0.1", Segment: "127.0.0.0/24"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	cov, err := s.Coverage(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cov.Total != 1 {
		t.Fatalf("范围内实体数 = %d，期望 1（ip 127.0.0.1）", cov.Total)
	}
	// 只有 origin fact 的锚点：不算"测过"。
	if cov.Tested != 0 {
		t.Fatalf("已测 = %d，期望 0（origin 不算）", cov.Tested)
	}

	// 写一条带锚点的事实 → 覆盖率 100%。
	if _, err := s.AddFact(ctx, AddFactInput{
		RunID: run.ID, Summary: "8080 开放", Evidence: "curl → 200",
		Confidence: "observed", AssetIDs: []int64{1},
	}); err != nil {
		t.Fatal(err)
	}
	cov, err = s.Coverage(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cov.Total != 1 || cov.Tested != 1 || cov.Pct != 100 {
		t.Fatalf("覆盖度 = %+v，期望 1/1 = 100%%", cov)
	}
}
