package store

import (
	"context"
	"strings"
	"testing"
)

// 流量录制：写入 / 检索（host+正文子串）/ 详情 / 证据绑定在流量删除后仍可读。
func TestTrafficRecordSearchAndEvidence(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	run := newTestRun(t, s)
	intentID, err := s.AddIntent(ctx, AddIntentInput{RunID: run.ID, Summary: "抓登录页"})
	if err != nil {
		t.Fatal(err)
	}

	// 一条含敏感串的响应 + 一条 404。
	first, err := s.RecordTraffic(ctx, TrafficInput{
		RunID: run.ID, IntentID: intentID, ToolUseID: "call_1",
		Method: "POST", URL: "http://127.0.0.1:8080/login", Status: 200,
		ContentType: "text/html", ReqLen: 26, RespLen: 120,
		ReqBody: `{"user":"admin","pass":"P@ssw0rd"}`, RespBody: "<html>welcome admin</html>",
		ReqHeaders:  map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
		RespHeaders: map[string]string{"Server": "nginx"},
		DurationMS:  12,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordTraffic(ctx, TrafficInput{
		RunID: run.ID, IntentID: intentID, Method: "GET",
		URL: "http://127.0.0.1:8080/nope", Status: 404, RespBody: "not found",
	}); err != nil {
		t.Fatal(err)
	}

	// host 子串
	rows, err := s.SearchTraffic(ctx, run.ID, TrafficQuery{Host: "8080", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("按 host 应命中 2 条，实际 %d", len(rows))
	}
	// 正文子串（请求体里也有）
	rows, err = s.SearchTraffic(ctx, run.ID, TrafficQuery{Q: "P@ssw0rd", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != first {
		t.Fatalf("按正文子串应命中 #%d，实际 %+v", first, rows)
	}
	// 只出现在响应体里的串也要能搜到
	rows, err = s.SearchTraffic(ctx, run.ID, TrafficQuery{Q: "welcome", Limit: 10})
	if err != nil || len(rows) != 1 {
		t.Fatalf("响应体子串应命中 1 条：%+v err=%v", rows, err)
	}
	// 方法 + 状态
	rows, err = s.SearchTraffic(ctx, run.ID, TrafficQuery{Method: "get", Status: 404, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != 404 {
		t.Fatalf("按方法+状态筛选不对：%+v", rows)
	}

	// url_template 把数字抽象掉（同批探测聚成一行）
	if tmpl := NormalizeURLTemplate("http://x/user/123/order/550e8400-e29b-41d4-a716-446655440000"); !strings.Contains(tmpl, "{n}") || !strings.Contains(tmpl, "{uuid}") {
		t.Fatalf("模板归一化不对：%s", tmpl)
	}

	// 详情带上头
	d, err := s.TrafficDetail(ctx, run.ID, first)
	if err != nil {
		t.Fatal(err)
	}
	if d.Method != "POST" || d.ReqHeaders["Content-Type"] == "" || d.RespHeaders["Server"] != "nginx" {
		t.Fatalf("详情缺头/方法：%+v", d)
	}

	// 绑定证据 → 删流量 → 证据仍可读
	fid, err := s.AddFinding(ctx, AddFindingInput{
		RunID: run.ID, IntentID: intentID, Name: "弱口令", Severity: "high",
		Summary: "admin 弱口令登录成功", Evidence: "POST /login → 200 welcome admin",
	})
	if err != nil {
		t.Fatal(err)
	}
	n, err := s.BindFindingTraffic(ctx, run.ID, fid, []int64{first})
	if err != nil || n != 1 {
		t.Fatalf("绑定失败：n=%d err=%v", n, err)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM traffic_exchanges WHERE run_id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	list, err := s.FindingTrafficList(ctx, run.ID, fid)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("流量删了，证据不该丢：%+v", list)
	}
	if prev, _ := list[0]["resp_preview"].(string); !strings.Contains(prev, "welcome admin") {
		t.Fatalf("证据预览没快照下来：%+v", list[0])
	}
}
