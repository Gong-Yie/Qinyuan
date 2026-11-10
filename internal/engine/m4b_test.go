package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"qinyuan/internal/agent"
	"qinyuan/internal/llm"
	"qinyuan/internal/store"
)

// 本文件是 M4b 的验收：删除流水线与并发准入。
//
// 三条底线：
//  1. 删除是"屏障 → 排水 → 可逆暂存 → DB 提交 → 真删"，任何一步失败都能还原；
//  2. 并发上限生效时，超出的任务进持久化 FIFO 队列，槽位空出后按 queued_at 补位；
//  3. 崩溃留下的 .deleting 暂存目录能在启动时按 DB 真相收尾。

func newLifecycleEngine(t *testing.T, st *store.Store, dataDir string, cfg Config) *Engine {
	t.Helper()
	cfg.DataDir = dataDir
	if cfg.IdleSleep <= 0 {
		cfg.IdleSleep = 10 * time.Millisecond
	}
	if cfg.Debounce <= 0 {
		cfg.Debounce = 5 * time.Millisecond
	}
	return New(st, llm.NewFakeFactory(llm.Step{Text: "空转"}), agent.NewWorkerRegistry(), agent.NewPlannerRegistry(), cfg, nil)
}

// TestDeleteRemovesRunAndFiles：删除后 DB 与文件都不留；重复删除幂等。
func TestDeleteRemovesRunAndFiles(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	dataDir := t.TempDir()
	eng := newLifecycleEngine(t, st, dataDir, Config{})
	defer eng.StopAll(ErrShutdown)

	run := newRunForGoals(t, st) // 与 M3.5 的测试共用建 run 的辅助
	// 造一份"任务产物"，验证它会被两阶段清理掉。
	runDir := filepath.Join(dataDir, "runs", fmt.Sprintf("%d", run.ID))
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "cmd-output.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := eng.Delete(ctx, run.ID); err != nil {
		t.Fatalf("删除失败：%v", err)
	}
	if _, err := st.Run(ctx, run.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("run 应当已被删除，实际 err=%v", err)
	}
	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Fatalf("任务目录应当被清理，实际 err=%v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(dataDir, ".deleting")); len(entries) != 0 {
		t.Fatalf("暂存目录应当是空的，实际 %v", entries)
	}

	// 幂等：再删一次不报错。
	if err := eng.Delete(ctx, run.ID); err != nil {
		t.Fatalf("重复删除应当幂等，实际 %v", err)
	}
}

// TestDeleteWhileRunning：正在跑的任务也能被删掉，且删完没有在跑的操作。
func TestDeleteWhileRunning(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	dataDir := t.TempDir()

	run, _ := newLoopbackRun(t, st, "http://127.0.0.1:1")
	intentID, err := st.AddIntent(ctx, store.AddIntentInput{RunID: run.ID, Summary: "慢任务"})
	if err != nil {
		t.Fatal(err)
	}

	slow := llm.NewFakeFactory(llm.Step{Delay: 5 * time.Second, Text: "想了很久"})
	eng := New(st, slow, agent.NewWorkerRegistry(), agent.NewPlannerRegistry(), Config{
		DataDir: dataDir, IdleSleep: 10 * time.Millisecond, Debounce: 5 * time.Millisecond,
		MaxTurns: 3, MaxDuration: 30 * time.Second, DeleteDrainTimeout: 3 * time.Second,
	}, nil)
	if err := eng.Run(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	// 等它真的领到活（界面上的"运行中"）。
	waitIntentState(t, st, run.ID, intentID, "running", 10*time.Second)

	start := time.Now()
	if err := eng.Delete(ctx, run.ID); err != nil {
		t.Fatalf("删除失败：%v", err)
	}
	if d := time.Since(start); d > 8*time.Second {
		t.Fatalf("删除耗时 %s：排水没生效（慢调用应当被取消）", d)
	}
	if _, err := st.Run(ctx, run.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("run 应当已被删除，实际 err=%v", err)
	}
	if n := eng.Inflight(run.ID); n != 0 {
		t.Fatalf("删除后在跑操作应当归零，实际 %d", n)
	}
}

// TestAdmissionQueueFIFO：上限 1 时第二个任务排队，第一个删掉后按 FIFO 补位。
func TestAdmissionQueueFIFO(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	eng := newLifecycleEngine(t, st, t.TempDir(), Config{MaxConcurrentRuns: 1})
	defer eng.StopAll(ErrShutdown)

	runA := newRunForGoals(t, st)
	runB := newRunForGoals(t, st)

	if err := eng.Start(ctx, runA.ID); err != nil {
		t.Fatalf("启动 A 失败：%v", err)
	}
	if err := eng.Start(ctx, runB.ID); err != nil {
		t.Fatalf("启动 B 失败：%v", err)
	}

	b, _ := st.Run(ctx, runB.ID)
	if b.Task == nil || !b.Task.Queued {
		t.Fatalf("A 占着槽位时，B 应当排队；实际 queued=%v", b.Task != nil && b.Task.Queued)
	}

	// 删掉 A → 腾出槽位 → B 应当被补位。
	if err := eng.Delete(ctx, runA.ID); err != nil {
		t.Fatalf("删除 A 失败：%v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		rb, err := st.Run(ctx, runB.ID)
		if err != nil {
			t.Fatal(err)
		}
		if rb.Task != nil && !rb.Task.Queued {
			eng.StopTask(runB.ID, ErrShutdown)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("B 没有被补位启动（仍然 queued）")
}

// TestRecoverStagedRemovals：崩溃残留的暂存目录按 DB 真相收尾。
func TestRecoverStagedRemovals(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	dataDir := t.TempDir()
	eng := newLifecycleEngine(t, st, dataDir, Config{})
	defer eng.StopAll(ErrShutdown)

	stageRoot := filepath.Join(dataDir, ".deleting")
	if err := os.MkdirAll(stageRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	// ① DB 里没有的 run（删除已提交，只差收尾）→ 应当被真删。
	goneDir := filepath.Join(stageRoot, "999-111")
	if err := os.MkdirAll(goneDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// ② DB 里还在的 run（删除在提交前崩了）→ 应当被还原。
	run := newRunForGoals(t, st)
	liveDir := filepath.Join(stageRoot, fmt.Sprintf("%d-222", run.ID))
	if err := os.MkdirAll(liveDir, 0o755); err != nil {
		t.Fatal(err)
	}

	eng.recoverStagedRemovals(ctx)

	if _, err := os.Stat(goneDir); !os.IsNotExist(err) {
		t.Fatalf("DB 里已不存在的 run，其暂存目录应当被删掉")
	}
	if _, err := os.Stat(filepath.Join(dataDir, "runs", fmt.Sprintf("%d", run.ID))); err != nil {
		t.Fatalf("DB 里还在的 run，其目录应当被还原：%v", err)
	}
}
