package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"qinyuan/internal/llm"
	"qinyuan/internal/model"
	"qinyuan/internal/store"
)

// P2-5 验收：给定 run_id，"一次执行 → LLM 调用 → 工具调用"能在时间线里逐层还原。
func TestWorkEmitsSpanTree(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "qinyuan-test/1.0")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("<html><title>trace</title></html>"))
	}))
	defer srv.Close()

	run, assetID := newLoopbackRun(t, st, srv.URL)
	intentID, err := st.AddIntent(ctx, store.AddIntentInput{
		RunID: run.ID, Summary: "抓首页", AssetIDs: []int64{assetID},
	})
	if err != nil {
		t.Fatal(err)
	}

	providers := llm.NewFakeFactory(
		llm.Step{ToolCalls: []llm.ToolCall{llm.NewToolCall("c1", "http_request",
			map[string]any{"method": "GET", "url": srv.URL + "/"})}, Usage: llm.Usage{Input: 100, Output: 20}},
		llm.Step{Text: "完成"},
	)
	eng := newEngineFor(t, st, providers)
	if _, err := eng.Work(ctx, run.ID, "w1"); err != nil {
		t.Fatalf("执行失败：%v", err)
	}

	spans, err := st.TimelineSpans(ctx, run.ID, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*model.Activity{}
	for _, sp := range spans {
		if _, ok := byName[sp.SpanName]; !ok {
			byName[sp.SpanName] = sp
		}
		if sp.TraceID != "qy-"+strconv.FormatInt(run.ID, 10) {
			t.Fatalf("trace_id 不是确定性的：%q", sp.TraceID)
		}
	}
	root, ok := byName["intent.execute"]
	if !ok {
		t.Fatalf("缺 intent.execute span：%+v", names(spans))
	}
	if root.NodeID == nil || *root.NodeID != intentID {
		t.Fatalf("intent.execute 应挂在意图 #%d 上，实际 %+v", intentID, root.NodeID)
	}
	if root.Metadata["state"] != model.StateDone || root.Metadata["reason"] != "done" {
		t.Fatalf("意图终态没写进 span 属性：%+v", root.Metadata)
	}

	llmSpan, ok := byName["llm.call"]
	if !ok {
		t.Fatalf("缺 llm.call span：%+v", names(spans))
	}
	if llmSpan.ParentSpanID != root.SpanID {
		t.Fatalf("llm.call 的父应是 intent.execute：%+v vs %+v", llmSpan, root)
	}
	if llmSpan.Metadata["input_tokens"] != float64(100) {
		t.Fatalf("llm.call 没带 token 属性：%+v", llmSpan.Metadata)
	}

	toolSpan, ok := byName["tool.call"]
	if !ok {
		t.Fatalf("缺 tool.call span：%+v", names(spans))
	}
	if toolSpan.ParentSpanID != root.SpanID {
		t.Fatalf("tool.call 的父应是 intent.execute：%+v", toolSpan)
	}
	if toolSpan.Metadata["tool"] != "http_request" || toolSpan.Metadata["read_only"] != false {
		t.Fatalf("tool.call 属性不对：%+v", toolSpan.Metadata)
	}
	if toolSpan.Metadata["error"] != false {
		t.Fatalf("成功的工具调用不该标 error：%+v", toolSpan.Metadata)
	}
}

func names(spans []*model.Activity) []string {
	out := make([]string, 0, len(spans))
	for _, s := range spans {
		out = append(out, s.SpanName)
	}
	return out
}
