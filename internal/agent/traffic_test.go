package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"qinyuan/internal/model"
	"qinyuan/internal/store"
)

// 流量在唯一出网点录制：成功与失败都留痕，且带 intent 归属。
func TestHTTPRecordsTraffic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("<html>login form P@ssw0rd</html>"))
	}))
	defer srv.Close()

	rc, run := testContext(t, []model.Scope{{Kind: "ip", Value: "127.0.0.1", Source: "manual"}},
		[]store.EntityInput{{Kind: "ip", IP: "127.0.0.1", Segment: "127.0.0.0/24"}},
		true, 0)
	tool := NewHTTPTool()
	res, err := tool.Handle(context.Background(), rc, json.RawMessage(`{"url":"`+srv.URL+`/"}`))
	if err != nil {
		t.Fatal(err)
	}
	var out httpOutput
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatal(err)
	}
	if out.TrafficID == 0 {
		t.Fatal("http_request 应该返回 traffic_id")
	}

	// 正文子串能搜到，且归属到本次意图
	rows, err := rc.Store.SearchTraffic(context.Background(), run.ID, store.TrafficQuery{Q: "P@ssw0rd", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("应命中 1 条，实际 %d", len(rows))
	}
	if rows[0].IntentID != rc.IntentID || rows[0].Method != "GET" {
		t.Fatalf("归属/方法不对：%+v", rows[0])
	}

	// 失败也录一条（status=0 + error）
	fail, err := tool.Handle(context.Background(), rc, json.RawMessage(`{"url":"http://127.0.0.1:1/"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !fail.IsError {
		t.Fatal("连不上应当报错")
	}
	rows, err = rc.Store.SearchTraffic(context.Background(), run.ID, store.TrafficQuery{Host: "127.0.0.1", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var gotErr bool
	for _, r := range rows {
		if r.Status == 0 && r.Error != "" {
			gotErr = true
		}
	}
	if !gotErr {
		t.Fatalf("失败的请求也该留痕：%+v", rows)
	}
}

// 大正文落内容寻址文件，DB 只留预览 + 路径。
func TestTrafficLargeBodySpillsToFile(t *testing.T) {
	big := strings.Repeat("A", 9000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(big))
	}))
	defer srv.Close()

	rc, run := testContext(t, []model.Scope{{Kind: "ip", Value: "127.0.0.1", Source: "manual"}},
		[]store.EntityInput{{Kind: "ip", IP: "127.0.0.1", Segment: "127.0.0.0/24"}},
		true, 0)
	res, err := NewHTTPTool().Handle(context.Background(), rc, json.RawMessage(`{"url":"`+srv.URL+`/"}`))
	if err != nil {
		t.Fatal(err)
	}
	var out httpOutput
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatal(err)
	}
	e, err := rc.Store.TrafficDetail(context.Background(), run.ID, out.TrafficID)
	if err != nil {
		t.Fatal(err)
	}
	if e.RespPath == "" {
		t.Fatalf("9000 字节正文应落盘：%+v", e)
	}
	abs := filepath.Join(rc.DataDir, filepath.FromSlash(e.RespPath))
	st, err := os.Stat(abs)
	if err != nil {
		t.Fatalf("落盘文件不存在：%v", err)
	}
	if st.Size() != 9000 {
		t.Fatalf("落盘大小 = %d，期望 9000", st.Size())
	}
	if len(e.RespBody) > trafficInlineMax {
		t.Fatalf("DB 预览不该超过 %d，实际 %d", trafficInlineMax, len(e.RespBody))
	}
	if !strings.Contains(e.RespPath, "traffic") {
		t.Fatalf("路径应在 traffic/ 下：%s", e.RespPath)
	}
}
