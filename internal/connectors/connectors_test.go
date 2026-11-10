package connectors

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"qinyuan/internal/store"
	"qinyuan/internal/testkit"
)

func seedFinding(t *testing.T, st *store.Store) (*store.OutboxEvent, int64, int64) {
	t.Helper()
	ctx := context.Background()
	run, err := st.CreateRun(ctx, store.CreateRunInput{Goal: "连接器测试", WorkerCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	fid, err := st.AddFinding(ctx, store.AddFindingInput{
		RunID: run.ID, Name: "SQL 注入", Severity: "high",
		Summary: "id 参数可注入", Evidence: "GET /item?id=1' → 500，报 SQL 语法错误",
	})
	if err != nil {
		t.Fatal(err)
	}
	items, err := st.ListOutbox(ctx, "pending", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("finding 入库应产生 1 条事件，实际 %d", len(items))
	}
	return &items[0], run.ID, fid
}

// 推 5 次只产生一个外部对象（台账幂等）。
func TestFanoutIsIdempotent(t *testing.T) {
	st := testkit.Store(t)
	ctx := context.Background()
	ev, _, _ := seedFinding(t, st)

	sink := &Memory{Label: "ticket"}
	f := &Fanout{Store: st, Sinks: []Sink{sink}}
	for i := 0; i < 5; i++ {
		if err := f.Handle(ctx, *ev); err != nil {
			t.Fatalf("第 %d 次推送失败：%v", i+1, err)
		}
	}
	if sink.Calls() != 1 {
		t.Fatalf("重复投递应只推 1 次，实际 %d 次", sink.Calls())
	}
	if len(sink.Pushed()) != 1 {
		t.Fatalf("外部系统只该有一个对象：%v", sink.Pushed())
	}
	d, ok, err := st.Delivery(ctx, "ticket", EventKey(*ev))
	if err != nil || !ok {
		t.Fatalf("台账缺记录：ok=%v err=%v", ok, err)
	}
	if d.ExternalID == "" || d.Status != "pushed" {
		t.Fatalf("台账内容不对：%+v", d)
	}
}

// 失败向外抛（交给 outbox 重试），下次成功后不再重复推。
func TestFanoutRetriesThenSucceeds(t *testing.T) {
	st := testkit.Store(t)
	ctx := context.Background()
	ev, _, _ := seedFinding(t, st)

	sink := &Memory{Label: "ticket", FailN: 1}
	f := &Fanout{Store: st, Sinks: []Sink{sink}}
	if err := f.Handle(ctx, *ev); err == nil {
		t.Fatal("第一次应当失败（由 outbox 退避重试）")
	}
	if err := f.Handle(ctx, *ev); err != nil {
		t.Fatalf("第二次应当成功：%v", err)
	}
	if sink.Calls() != 2 {
		t.Fatalf("调用次数 = %d，期望 2", sink.Calls())
	}
	d, _, _ := st.Delivery(ctx, "ticket", EventKey(*ev))
	if d == nil || d.Status != "pushed" {
		t.Fatalf("成功后台账应为 pushed：%+v", d)
	}
}

// 通用 webhook：带幂等键、解析外部 id、能拉状态。
func TestWebhookPushAndPull(t *testing.T) {
	var mu sync.Mutex
	keys := []string{}
	body := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/hook":
			mu.Lock()
			keys = append(keys, r.Header.Get("Idempotency-Key"))
			mu.Unlock()
			b, _ := io.ReadAll(r.Body)
			body = string(b)
			_, _ = w.Write([]byte(`{"id":"T-100"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/hook/status":
			_, _ = w.Write([]byte(`[{"id":"T-100","status":"fixed","note":"已上线修复"}]`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	wh := &Webhook{Label: "ticket", URL: srv.URL + "/hook"}
	ev := store.OutboxEvent{Topic: store.TopicFindingRecorded, Payload: json.RawMessage(`{"finding_id":7}`)}
	id, err := wh.Push(context.Background(), ev)
	if err != nil || id != "T-100" {
		t.Fatalf("推送失败：id=%q err=%v", id, err)
	}
	if !strings.Contains(body, `"event_key"`) || !strings.Contains(body, `"finding_id":7`) {
		t.Fatalf("推送体缺幂等键或 payload：%s", body)
	}
	if len(keys) != 1 || keys[0] != "finding:7" {
		t.Fatalf("幂等键应为 finding:7，实际 %v", keys)
	}
	updates, err := wh.Pull(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 || updates[0].Status != "fixed" {
		t.Fatalf("拉取结果不对：%+v", updates)
	}
}

// 外部把单改成 fixed → finding 的 payload 里 ticket_status 跟着变。
func TestSyncTicketsWritesBackFinding(t *testing.T) {
	st := testkit.Store(t)
	ctx := context.Background()
	ev, runID, fid := seedFinding(t, st)

	sink := &Memory{Label: "ticket"}
	f := &Fanout{Store: st, Sinks: []Sink{sink}}
	if err := f.Handle(ctx, *ev); err != nil {
		t.Fatal(err)
	}
	ext := sink.Pushed()[EventKey(*ev)]
	if ext == "" {
		t.Fatal("没有外部 id，无法映射回 finding")
	}
	sink.SetUpdates([]Update{{ExternalID: ext, Status: "fixed", Note: "开发已修复"}})

	n, err := f.SyncTickets(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应回写 1 条，实际 %d", n)
	}
	node, err := st.Node(ctx, runID, fid)
	if err != nil {
		t.Fatal(err)
	}
	if got := node.PayloadField("ticket_status"); got != "fixed" {
		t.Fatalf("ticket_status = %q，期望 fixed（payload=%s）", got, string(node.Payload))
	}
	if got := node.PayloadField("ticket_id"); got != ext {
		t.Fatalf("ticket_id = %q，期望 %q", got, ext)
	}
}

// CI 导出：只导出已确认（verified）的发现，且脚本可执行。
func TestExportCIOnlyVerified(t *testing.T) {
	st := testkit.Store(t)
	ctx := context.Background()
	run, err := st.CreateRun(ctx, store.CreateRunInput{Goal: "CI", WorkerCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	mk := func(name string) int64 {
		t.Helper()
		id, err := st.AddFinding(ctx, store.AddFindingInput{
			RunID: run.ID, Name: name, Severity: "high",
			Summary: "s", Evidence: "GET /x → 200",
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	verifiedID := mk("已验证的 XSS")
	unverifiedID := mk("没复测的越权")
	// 两条都绑流量（否则拿不到可执行命令）。
	for _, fid := range []int64{verifiedID, unverifiedID} {
		exID, err := st.RecordTraffic(ctx, store.TrafficInput{
			RunID: run.ID, Method: "GET", URL: "https://t.example/x", Status: 200, RespBody: "ok",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.BindFindingTraffic(ctx, run.ID, fid, []int64{exID}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.AddRetest(ctx, store.Retest{
		RunID: run.ID, FindingID: verifiedID, Status: "verified",
		Expected: "200", Observed: "200 sha 一致", CreatedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	files, err := ExportCI(ctx, st, run.ID, dir)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(files, "\n")
	if !strings.Contains(joined, "finding-"+itoa(verifiedID)+".sh") {
		t.Fatalf("已确认的发现应被导出：%v", files)
	}
	if strings.Contains(joined, "finding-"+itoa(unverifiedID)+".sh") {
		t.Fatalf("未复测的发现不该进 CI：%v", files)
	}
	if !strings.Contains(joined, "run-all.sh") {
		t.Fatalf("缺少入口脚本：%v", files)
	}

	// CI 回归结果回写：不再复现 → failed（报告会把它移到「待复核」）。
	rt, err := ImportCIResult(ctx, st, run.ID, verifiedID, "failed", "回归：目标已返回 404")
	if err != nil {
		t.Fatal(err)
	}
	if rt.Status != "failed" {
		t.Fatalf("回归结果状态 = %q", rt.Status)
	}
	statuses, err := st.RetestStatusByFinding(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if statuses[verifiedID] != "failed" {
		t.Fatalf("复测状态应更新为 failed，实际 %q", statuses[verifiedID])
	}
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}
