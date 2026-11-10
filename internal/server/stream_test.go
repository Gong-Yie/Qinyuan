package server

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"qinyuan/internal/model"
	"qinyuan/internal/store"
	"qinyuan/internal/testkit"
)

// 广播可丢：缓冲满了不能阻塞引擎（慢客户端不拖死服务）。
func TestBusDoesNotBlockWhenFull(t *testing.T) {
	b := NewBus()
	_, cancel := b.Subscribe(1)
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			b.Publish(&model.Activity{RunID: 1, Kind: model.ActResult})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish 被慢订阅者阻塞了")
	}

	if got := b.Subscribers(1); got != 1 {
		t.Fatalf("订阅者数 = %d，期望 1", got)
	}
	cancel()
	if got := b.Subscribers(1); got != 0 {
		t.Fatalf("退订后订阅者数 = %d，期望 0", got)
	}
}

// 断线补齐：带 since 时先回放缺口，再接实时。
func TestSSEReplaysGapFromSince(t *testing.T) {
	st := testkit.Store(t)
	srv := New(st, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	run := newStreamTestRun(t, st)
	for i := 1; i <= 3; i++ {
		appendStreamActivity(t, st, run.ID, fmt.Sprintf("第 %d 条", i))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/api/runs/%d/activity/stream?since=1", ts.URL, run.ID), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	ids := readFrameIDs(t, resp.Body, 2, cancel)
	if len(ids) != 2 || ids[0] != 2 || ids[1] != 3 {
		t.Fatalf("回放的 id = %v，期望 [2 3]（只补 since 之后的）", ids)
	}
}

// 实时：先订阅再接流，Publish 之后客户端应收到那一帧。
func TestSSEDeliversLivePublish(t *testing.T) {
	st := testkit.Store(t)
	srv := New(st, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	run := newStreamTestRun(t, st)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/api/runs/%d/activity/stream", ts.URL, run.ID), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// 等到订阅真的建立（handler 会先写一帧 ": connected"）。
	waitForSubscriber(t, srv.Bus, run.ID)

	act := appendStreamActivity(t, st, run.ID, "实时一条")
	act.ID = 999 // 广播用内存对象，id 由 DB 决定；这里跟着改，便于断言
	srv.Bus.Publish(act)

	ids := readFrameIDs(t, resp.Body, 1, cancel)
	if len(ids) != 1 || ids[0] != 999 {
		t.Fatalf("实时帧 = %v，期望 [999]", ids)
	}
}

// 控制台首页必须由二进制自己发出来（go:embed）。
func TestStaticIndexServed(t *testing.T) {
	srv := New(nil, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d，期望 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), "钦原") || !strings.Contains(string(body), "app.js") {
		t.Fatalf("首页内容不对：%s", string(body[:min(len(body), 200)]))
	}
}

// 未知 API 不能落到 index.html（否则前端会拿 HTML 当 JSON 解析）。
func TestUnknownAPIReturnsJSON404(t *testing.T) {
	srv := New(nil, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/nope")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d，期望 404", resp.StatusCode)
	}
	if !strings.Contains(string(body), "error") {
		t.Fatalf("应当是 JSON 错误体，实际：%s", body)
	}
}

/* ── 测试辅助 ───────────────────────────────────────── */

func newStreamTestRun(t *testing.T, st *store.Store) *model.Run {
	t.Helper()
	run, err := st.CreateRun(context.Background(), store.CreateRunInput{Goal: "流测试", WorkerCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func appendStreamActivity(t *testing.T, st *store.Store, runID int64, summary string) *model.Activity {
	t.Helper()
	act := &model.Activity{
		RunID: runID, Worker: "planner", Kind: model.ActResult, Summary: summary,
	}
	id, err := st.AppendActivity(context.Background(), act)
	if err != nil {
		t.Fatal(err)
	}
	act.ID = id
	act.CreatedAt = time.Now()
	return act
}

// readFrameIDs 读 n 帧（"id: N" 行），读完或超时就返回。
func readFrameIDs(t *testing.T, r io.Reader, n int, cancel context.CancelFunc) []int64 {
	t.Helper()
	var ids []int64
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "id: ") {
			continue
		}
		var id int64
		if _, err := fmt.Sscanf(line, "id: %d", &id); err != nil {
			continue
		}
		ids = append(ids, id)
		if len(ids) >= n {
			cancel()
			return ids
		}
	}
	return ids
}

func waitForSubscriber(t *testing.T, b *Bus, runID int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if b.Subscribers(runID) > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("订阅没有建立")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
