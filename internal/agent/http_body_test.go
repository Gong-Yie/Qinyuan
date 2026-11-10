package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"qinyuan/internal/model"
	"qinyuan/internal/store"
)

// P4：http_request 两档返回 —— 默认只回前 4KB + 落盘指针，full=true 才回全文。
func TestHTTPBodyHeadByDefaultFullOnDemand(t *testing.T) {
	body := strings.Repeat("A", 9000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	rc, _ := testContext(t, []model.Scope{{Kind: "ip", Value: "127.0.0.1", Source: "manual"}},
		[]store.EntityInput{{Kind: "ip", IP: "127.0.0.1", Segment: "127.0.0.0/24"}},
		true, 0)
	rc.MaxOutput = 20000 // 允许 full 时回更多

	tool := NewHTTPTool()
	res, err := tool.Handle(context.Background(), rc, json.RawMessage(`{"url":"`+srv.URL+`/"}`))
	if err != nil {
		t.Fatal(err)
	}
	var head httpOutput
	if err := json.Unmarshal([]byte(res.Text), &head); err != nil {
		t.Fatalf("返回不是结构化 JSON：%v", err)
	}
	// 截断后的内容是"前 N 字节 + 一段落盘指针"，所以比 N 略大。
	if len(head.Body) < httpInlineBodyBytes || len(head.Body) > httpInlineBodyBytes+600 {
		t.Fatalf("默认应只回 ~%d 字节（含指针），实际 %d", httpInlineBodyBytes, len(head.Body))
	}
	if !strings.Contains(head.Body, "完整内容见") {
		t.Fatalf("截断后必须带落盘指针，实际：%q", head.Body[len(head.Body)-80:])
	}
	if head.BodyBytes != 9000 {
		t.Fatalf("body_bytes 应为 9000，实际 %d", head.BodyBytes)
	}

	res, err = tool.Handle(context.Background(), rc, json.RawMessage(`{"url":"`+srv.URL+`/","full":true}`))
	if err != nil {
		t.Fatal(err)
	}
	var full httpOutput
	if err := json.Unmarshal([]byte(res.Text), &full); err != nil {
		t.Fatalf("返回不是结构化 JSON：%v", err)
	}
	if len(full.Body) != 9000 {
		t.Fatalf("full=true 应回全文 9000 字节，实际 %d", len(full.Body))
	}
	if strings.Contains(full.Body, "完整内容见") {
		t.Fatal("full=true 且未超上限时不该带截断提示")
	}
}
