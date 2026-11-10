package store

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
)

// 同一个事件重复入队只留一行（幂等键 = topic + payload_hash）。
func TestOutboxEnqueueIdempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	run := newTestRun(t, s)

	id1, ins1, err := s.EnqueueOutbox(ctx, run.ID, TopicFindingRecorded, map[string]any{
		"finding_id": 7, "name": "SQL 注入",
	})
	if err != nil {
		t.Fatal(err)
	}
	id2, ins2, err := s.EnqueueOutbox(ctx, run.ID, TopicFindingRecorded, map[string]any{
		"finding_id": 7, "name": "SQL 注入",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ins1 || ins2 || id1 != id2 {
		t.Fatalf("重复入队应返回同一 id 且 inserted=false：id1=%d ins1=%v id2=%d ins2=%v", id1, ins1, id2, ins2)
	}
	items, err := s.ListOutbox(ctx, "pending", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("事件行数 = %d，期望 1", len(items))
	}

	// 不同 payload = 不同事件。
	if _, ins, err := s.EnqueueOutbox(ctx, run.ID, TopicFindingRecorded, map[string]any{
		"finding_id": 8, "name": "XSS",
	}); err != nil || !ins {
		t.Fatalf("不同 payload 应当是新的插入：ins=%v err=%v", ins, err)
	}
}

// 业务事务回滚时，outbox 不能留下事件（否则会出现"通知说有、库里没有"）。
func TestOutboxRollbackLeavesNoEvent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	run := newTestRun(t, s)

	tx, err := s.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.EnqueueOutboxTx(ctx, tx, run.ID, TopicFindingRecorded, map[string]any{"finding_id": 1}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	items, err := s.ListOutbox(ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("事务回滚后不该有事件，实际 %d 条：%+v", len(items), items)
	}
}

// finding 入库必须在同一事务里带上事件（P2-3 的核心要求）。
func TestAddFindingEnqueuesEvent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	run := newTestRun(t, s)

	fid, err := s.AddFinding(ctx, AddFindingInput{
		RunID: run.ID, Name: "反射型 XSS", Severity: "medium",
		Summary: "搜索参数未转义", Evidence: "GET /search?q=<script> → 200 且原样回显",
	})
	if err != nil {
		t.Fatal(err)
	}
	items, err := s.ListOutbox(ctx, "pending", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("finding 入库后应有 1 条 outbox 事件，实际 %d", len(items))
	}
	if items[0].Topic != TopicFindingRecorded {
		t.Fatalf("topic = %q", items[0].Topic)
	}
	if items[0].RunID != run.ID {
		t.Fatalf("run_id = %d，期望 %d", items[0].RunID, run.ID)
	}
	var got map[string]any
	if err := json.Unmarshal(items[0].Payload, &got); err != nil {
		t.Fatalf("payload 不是合法 JSON：%v", err)
	}
	if idNum, _ := got["finding_id"].(float64); int64(idNum) != fid {
		t.Fatalf("payload 里应带 finding_id=%d：%s", fid, string(items[0].Payload))
	}
}

// 多实例/多 goroutine 并发领取：同一条只能被一个拿到。
func TestOutboxClaimConcurrentNoDuplicates(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	run := newTestRun(t, s)

	const total = 60
	for i := 0; i < total; i++ {
		if _, _, err := s.EnqueueOutbox(ctx, run.ID, TopicAssetRecorded, map[string]any{"n": i}); err != nil {
			t.Fatal(err)
		}
	}

	var mu sync.Mutex
	seen := map[int64]int{}
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				evs, err := s.ClaimOutbox(ctx, 5, 0)
				if err != nil {
					t.Errorf("领取失败：%v", err)
					return
				}
				if len(evs) == 0 {
					return
				}
				mu.Lock()
				for _, ev := range evs {
					seen[ev.ID]++
				}
				mu.Unlock()
				// 不标记完成：正是要验证"同一条不会被领两次"。
			}
		}()
	}
	wg.Wait()

	if len(seen) != total {
		t.Fatalf("领取到的事件数 = %d，期望 %d", len(seen), total)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("事件 #%d 被领取了 %d 次（应恰好 1 次）", id, n)
		}
	}
}

// 失败到上限进死信；重放能把它放回队列。
func TestOutboxDeadThenReplay(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	run := newTestRun(t, s)

	if _, _, err := s.EnqueueOutbox(ctx, run.ID, TopicFindingRecorded, map[string]any{"k": 1}); err != nil {
		t.Fatal(err)
	}
	var id int64
	dead := false
	for i := 0; i < 5; i++ {
		evs, err := s.ClaimOutbox(ctx, 1, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(evs) == 0 {
			t.Fatalf("第 %d 次重试时没领到事件（退避改成 0 后应立刻可领）", i+1)
		}
		id = evs[0].ID
		dead, err = s.FailOutbox(ctx, id, errSample{}, 0, 5)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !dead {
		t.Fatal("重试到上限后应进死信")
	}
	deadItems, err := s.ListOutbox(ctx, "dead", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(deadItems) != 1 || deadItems[0].Status != "dead" {
		t.Fatalf("死信列表不对：%+v", deadItems)
	}

	ok, err := s.ReplayOutbox(ctx, id)
	if err != nil || !ok {
		t.Fatalf("重放失败：ok=%v err=%v", ok, err)
	}
	pending, deadCount, err := s.CountOutbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pending != 1 || deadCount != 0 {
		t.Fatalf("重放后应回到 pending：pending=%d dead=%d", pending, deadCount)
	}
}

type errSample struct{}

func (errSample) Error() string { return "模拟投递失败" }
