package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"qinyuan/internal/model"
)

// T1：并发领取不重复（I1/I2）。
func TestClaimConcurrentNoDuplicates(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	run := newTestRun(t, s)

	const (
		total    = 60
		workers  = 10
		maxClaim = 20
	)
	for i := 0; i < total; i++ {
		if _, err := s.AddIntent(ctx, AddIntentInput{
			RunID: run.ID, Summary: fmt.Sprintf("方向 #%d", i), Priority: 5,
		}); err != nil {
			t.Fatalf("建意图失败：%v", err)
		}
	}

	var (
		mu      sync.Mutex
		claimed int
		wg      sync.WaitGroup
	)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			owner := fmt.Sprintf("w%d", w)
			for {
				rows, err := s.Frontier(ctx, run.ID, maxClaim)
				if err != nil {
					t.Errorf("frontier 失败：%v", err)
					return
				}
				gotAny := false
				for _, n := range rows {
					ok, err := s.ClaimIntent(ctx, run.ID, n.ID, owner)
					if err != nil {
						t.Errorf("claim 失败：%v", err)
						return
					}
					if ok {
						gotAny = true
						mu.Lock()
						claimed++
						mu.Unlock()
					}
				}
				if !gotAny {
					return
				}
			}
		}(w)
	}
	wg.Wait()

	if claimed != total {
		t.Fatalf("领取次数 = %d，期望 %d（说明有意图没被领到或领了两次）", claimed, total)
	}

	// 每条意图只能有一个 owner，且状态为 running。
	rows, err := s.Pool().Query(ctx, `
		SELECT count(*), count(DISTINCT id), count(owner)
		  FROM nodes WHERE run_id=$1 AND kind='intent' AND state='running'`, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var n, distinct, owners int
	if !rows.Next() {
		t.Fatalf("聚合查询没有返回行：%v", rows.Err())
	}
	if err := rows.Scan(&n, &distinct, &owners); err != nil {
		t.Fatal(err)
	}
	if n != total || distinct != total || owners != total {
		t.Fatalf("running=%d distinct=%d owners=%d，期望都是 %d", n, distinct, owners, total)
	}
}

// T2：状态迁移带 expected，冲突不得覆盖（I1）。
func TestCASTransitionRejectsWrongExpected(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	run := newTestRun(t, s)

	intentID, err := s.AddIntent(ctx, AddIntentInput{RunID: run.ID, Summary: "一条方向"})
	if err != nil {
		t.Fatal(err)
	}
	ok, err := s.ClaimIntent(ctx, run.ID, intentID, "w1")
	if err != nil || !ok {
		t.Fatalf("领取失败 ok=%v err=%v", ok, err)
	}

	// 用错误的 expected（open）迁移：必须失败。
	ok, err = s.CASIntent(ctx, run.ID, intentID, model.StateOpen, model.StateDone)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected=open 不该成功：意图已经被领成 running")
	}
	n, err := s.Intent(ctx, run.ID, intentID)
	if err != nil {
		t.Fatal(err)
	}
	if n.State != model.StateRunning || n.Owner != "w1" {
		t.Fatalf("冲突后状态被改坏了：state=%s owner=%s", n.State, n.Owner)
	}

	// 正确的 expected 才能迁移。
	ok, err = s.CASIntent(ctx, run.ID, intentID, model.StateRunning, model.StateDone)
	if err != nil || !ok {
		t.Fatalf("正确的 CAS 失败 ok=%v err=%v", ok, err)
	}
}

// I4/T9：无父意图必须自动连到 origin fact；把意图当父必须拒绝且不留孤儿。
func TestAddIntentLineage(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	run := newTestRun(t, s)

	id, err := s.AddIntent(ctx, AddIntentInput{RunID: run.ID, Summary: "顶层方向"})
	if err != nil {
		t.Fatal(err)
	}
	edges, err := s.Edges(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	linkedOrigin := false
	originID := int64(0)
	for _, e := range edges {
		if e.DstID == id && e.Rel == model.RelDerivedFrom {
			linkedOrigin = true
			originID = e.SrcID
		}
	}
	if !linkedOrigin {
		t.Fatal("顶层意图没有连到 origin fact，血缘断在根上")
	}
	origin, err := s.Node(ctx, run.ID, originID)
	if err != nil {
		t.Fatal(err)
	}
	if origin.Kind != model.KindFact || origin.State != model.StateOrigin {
		t.Fatalf("父节点不是 origin fact：kind=%s state=%s", origin.Kind, origin.State)
	}

	// 拿意图当父：整条拒绝，且不新建节点。
	before, _ := s.CountByKind(ctx, run.ID, model.KindIntent)
	_, err = s.AddIntent(ctx, AddIntentInput{RunID: run.ID, Summary: "非法父", ParentIDs: []int64{id}})
	if !errors.Is(err, ErrParentNotAllowed) {
		t.Fatalf("期望 ErrParentNotAllowed，实际 %v", err)
	}
	after, _ := s.CountByKind(ctx, run.ID, model.KindIntent)
	if before != after {
		t.Fatalf("非法父竟然建出了节点：before=%d after=%d", before, after)
	}
}

// T7：写入幂等 + 合并语义（I17）。
func TestUpsertEntityIdempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	runA := newTestRun(t, s)
	runB := newTestRun(t, s)

	in := EntityInput{
		Kind: "endpoint", URL: "https://example.test/api/user", Method: "GET",
		Domain: "example.test", RootDomain: "example.test", StatusCode: 200, PageTitle: "第一次",
	}
	id1, created1, err := s.UpsertEntity(ctx, runA.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	if !created1 {
		t.Fatal("首次写入应当是 created")
	}
	id2, created2, err := s.UpsertEntity(ctx, runA.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	if created2 || id2 != id1 {
		t.Fatalf("重复写入不该新建：created=%v id=%d/%d", created2, id1, id2)
	}

	// 空值不覆盖，非空覆盖
	in.PageTitle = ""
	if _, _, err := s.UpsertEntity(ctx, runA.ID, in); err != nil {
		t.Fatal(err)
	}
	e, err := s.Entity(ctx, id1)
	if err != nil {
		t.Fatal(err)
	}
	if e.Title != "第一次" {
		t.Fatalf("空值覆盖了旧值：%q", e.Title)
	}

	// 另一个 run 见到同一个实体：run_ids 并集去重
	if _, _, err := s.UpsertEntity(ctx, runB.ID, in); err != nil {
		t.Fatal(err)
	}
	e, err = s.Entity(ctx, id1)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.RunIDs) != 2 {
		t.Fatalf("run_ids 应为 2 个，实际 %v", e.RunIDs)
	}

	var count int
	if err := s.Pool().QueryRow(ctx, `SELECT count(*) FROM entities`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("实体重复了：%d 行", count)
	}
}

// I6 + 清洗：activity 写入要能吞掉 NUL / 非法 UTF-8，而不是整条被 PG 拒收。
func TestActivitySanitizesNUL(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	run := newTestRun(t, s)

	a := &model.Activity{
		RunID: run.ID, Kind: model.ActToolResult, Tool: "http_request",
		ToolUseID: "call_1", Summary: "a\x00b", Detail: "x\x00y",
		Metadata: map[string]any{"k": "v\x00"},
	}
	id, err := s.AppendActivity(ctx, a)
	if err != nil {
		t.Fatalf("写入带 NUL 的 activity 失败：%v", err)
	}
	got, err := s.ActivityDetail(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Summary != "ab" || got.Detail != "xy" {
		t.Fatalf("清洗不彻底：summary=%q detail=%q", got.Summary, got.Detail)
	}
}

// T5 的一部分：崩溃残留的 running 必须被打回 open。
func TestResetRunningIntents(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	run := newTestRun(t, s)

	for i := 0; i < 3; i++ {
		id, err := s.AddIntent(ctx, AddIntentInput{RunID: run.ID, Summary: fmt.Sprintf("方向%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		if ok, err := s.ClaimIntent(ctx, run.ID, id, "w1"); err != nil || !ok {
			t.Fatalf("claim 失败：%v", err)
		}
	}
	n, err := s.ResetRunningIntents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("重置数量 = %d，期望 3", n)
	}
	active, err := s.HasActiveIntent(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !active {
		t.Fatal("重置后应当仍有 open 意图（可继续推进）")
	}
}
