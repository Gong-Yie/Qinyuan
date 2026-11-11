package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 本文件是 M4c 的流式验收：合并增量、committed 边界、以及"未提交才重试"。

// sseServer 起一个假网关：按 chunks 逐条写 SSE，然后结束响应。
func sseServer(t *testing.T, chunks []string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			if fl != nil {
				fl.Flush()
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestProvider(t *testing.T, baseURL string, retries int) *OpenAI {
	t.Helper()
	p, err := NewOpenAI(OpenAIConfig{
		BaseURL: baseURL, Model: "test-model", MaxRetries: retries,
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// 增量合并：正文拼接、tool_calls 按 index 合并、arguments 是分片、usage 取到。
func TestOpenAIStreamMergesDeltas(t *testing.T) {
	srv := sseServer(t, []string{
		`{"choices":[{"delta":{"reasoning_content":"先想想"}}]}`,
		`{"choices":[{"delta":{"content":"你好"}}]}`,
		`{"choices":[{"delta":{"content":"，世界"}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"http_request","arguments":"{\"url\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"http://x/\"}"}}]}}]}`,
		`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":2}}}`,
		`[DONE]`,
	})
	p := newTestProvider(t, srv.URL, 0)

	var textEvents []string
	firstCommitted := false
	resp, err := p.Stream(context.Background(), &Request{
		Messages: []Message{{Role: RoleUser, Text: "hi"}},
	}, func(ev StreamEvent) error {
		if ev.Text != "" && ev.Kind == StreamText {
			textEvents = append(textEvents, ev.Text)
		}
		if ev.Committed && !firstCommitted {
			firstCommitted = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Stream 失败：%v", err)
	}
	if resp.Text != "你好，世界" {
		t.Fatalf("正文没合并对：%q", resp.Text)
	}
	if resp.Thinking != "先想想" {
		t.Fatalf("thinking 没合并：%q", resp.Thinking)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("应当有 1 个工具调用，实际 %d", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "call_1" || tc.Name != "http_request" {
		t.Fatalf("工具调用字段不对：%+v", tc)
	}
	var args struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(tc.Input, &args); err != nil || args.URL != "http://x/" {
		t.Fatalf("工具参数没拼完整：%s (err=%v)", tc.Input, err)
	}
	if resp.Usage.Input != 10 || resp.Usage.Output != 5 || resp.Usage.CacheRead != 2 {
		t.Fatalf("usage 不对：%+v", resp.Usage)
	}
	if len(textEvents) != 2 {
		t.Fatalf("应当收到 2 条正文增量，实际 %v", textEvents)
	}
	if !firstCommitted {
		t.Fatalf("第一条增量就应当是 committed")
	}
}

// 已提交后断流：不重试、返回部分结果 + ErrStreamInterrupted。
func TestOpenAIStreamInterruptedAfterCommitDoesNotRetry(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"content":"已经说了半句"}}]}`)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		// 模拟网络中断：不是"正常结束"，而是把连接掐掉（客户端读到 unexpected EOF）。
		panic(http.ErrAbortHandler)
	}))
	defer srv.Close()
	p := newTestProvider(t, srv.URL, 2) // 允许重试，但 committed 之后不该用

	resp, err := p.Stream(context.Background(), &Request{
		Messages: []Message{{Role: RoleUser, Text: "hi"}},
	}, nil)
	if !errors.Is(err, ErrStreamInterrupted) {
		t.Fatalf("应当是 ErrStreamInterrupted，实际 %v", err)
	}
	if resp == nil || resp.Text != "已经说了半句" {
		t.Fatalf("必须保留已收到的部分结果，实际 %+v", resp)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("已提交后不该重试（请求数 %d）", n)
	}
}

// 未提交（连内容都没有）时允许重试：第一次空流，第二次成功。
func TestOpenAIStreamRetriesBeforeCommit(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if n == 1 {
			// 一个内容事件都没有：可以安全重试（L2）。
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"content":"第二次成功"}}]}`)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	p := newTestProvider(t, srv.URL, 2)

	resp, err := p.Stream(context.Background(), &Request{
		Messages: []Message{{Role: RoleUser, Text: "hi"}},
	}, nil)
	if err != nil {
		t.Fatalf("第二次应当成功：%v", err)
	}
	if resp.Text != "第二次成功" {
		t.Fatalf("内容不对：%q", resp.Text)
	}
	if n := atomic.LoadInt32(&hits); n != 2 {
		t.Fatalf("应当重试一次（请求数 %d）", n)
	}
}

// 只有推理、没有正文（推理把 max_tokens 吃光）：先升一次预算，仍不行才报错。
//
// 实测见 docs/19 / docs/22：deepseek-flash 对 1KB hexdump 做结构分析时，
// 4096 个输出 token 全部花在 reasoning 上，正文 0 字节、无工具调用。
func TestOpenAIStreamReasoningOnlyEscalatesThenStops(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"reasoning_content":"先分析一下这 1024 字节…"}}]}`)
		fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{},"finish_reason":"length"}]}`)
		fmt.Fprintf(w, "data: %s\n\n", `{"choices":[],"usage":{"prompt_tokens":2220,"completion_tokens":4096,"completion_tokens_details":{"reasoning_tokens":4096}}}`)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	p := newTestProvider(t, srv.URL, 2) // 普通重试额度不该被用掉

	resp, err := p.Stream(context.Background(), &Request{
		Messages: []Message{{Role: RoleUser, Text: "分析这段 hex"}},
	}, nil)
	if !errors.Is(err, ErrReasoningOnly) {
		t.Fatalf("应当是 ErrReasoningOnly，实际 %v", err)
	}
	if errors.Is(err, ErrTransient) {
		t.Fatalf("推理吃光预算不是抖动，不该被当成 ErrTransient：%v", err)
	}
	// 第 1 次 4096 被推理吃光 → 升到 8192 再试一次；仍然只有推理 → 停（不再重试）。
	if n := atomic.LoadInt32(&hits); n != 2 {
		t.Fatalf("应当只升一次预算（请求数 %d，期望 2）", n)
	}
	if resp == nil || resp.Thinking == "" {
		t.Fatalf("必须保留已收到的推理，实际 %+v", resp)
	}
	msg := err.Error()
	if !strings.Contains(msg, "max_tokens") || !strings.Contains(msg, "4096") {
		t.Fatalf("错误里要带输出预算，实际 %q", msg)
	}
}

// 推理吃光预算（finish=length）：把 max_tokens 翻倍再试一次，通常就能出正文。
// run #42 实测：两个最值钱的意图（读 web.xml / 读 .class 常量池）都因此 blocked。
func TestOpenAIStreamEscalatesReasoningBudget(t *testing.T) {
	var hits int32
	var secondMaxTokens int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		var req struct {
			MaxTokens int `json:"max_tokens"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if n == 1 {
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"reasoning_content":"想了很久"}}]}`)
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{},"finish_reason":"length"}]}`)
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":4096,"completion_tokens_details":{"reasoning_tokens":4096}}}`)
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		secondMaxTokens = req.MaxTokens
		fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"content":"终于出正文了"}}]}`)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	// MaxRetries=0：升级不该占用普通重试额度。
	p := newTestProvider(t, srv.URL, 0)

	resp, err := p.Stream(context.Background(), &Request{
		Messages: []Message{{Role: RoleUser, Text: "hi"}},
	}, nil)
	if err != nil {
		t.Fatalf("升级后应当成功：%v", err)
	}
	if resp.Text != "终于出正文了" {
		t.Fatalf("正文不对：%q", resp.Text)
	}
	if n := atomic.LoadInt32(&hits); n != 2 {
		t.Fatalf("应当只多试一次（请求数 %d）", n)
	}
	if secondMaxTokens != 8192 {
		t.Fatalf("升级后 max_tokens=%d，期望 8192", secondMaxTokens)
	}
}

// 真·空流：200 但一个内容事件都没有。可重试，且错误里要能看出"零字节/只有 [DONE]"。
func TestOpenAIStreamEmptyStreamDiagnostics(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	p := newTestProvider(t, srv.URL, 0)

	_, err := p.Stream(context.Background(), &Request{
		Messages: []Message{{Role: RoleUser, Text: "hi"}},
	}, nil)
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("空流仍应是可重试的 ErrTransient，实际 %v", err)
	}
	var es *EmptyStreamError
	if !errors.As(err, &es) {
		t.Fatalf("应当是 *EmptyStreamError，实际 %T", err)
	}
	if !es.OnlyDone || es.Status != http.StatusOK {
		t.Fatalf("现场字段不对：%+v", es)
	}
	if !strings.Contains(err.Error(), "只有 [DONE]") {
		t.Fatalf("错误里要说清是哪种空，实际 %q", err)
	}
}

// 非 200：走错误分类，不进 committed。
func TestOpenAIStreamQuotaNoRetry(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"余额不足"}}`))
	}))
	defer srv.Close()
	p := newTestProvider(t, srv.URL, 2)

	_, err := p.Stream(context.Background(), &Request{Messages: []Message{{Role: RoleUser, Text: "hi"}}}, nil)
	if !errors.Is(err, ErrQuota) {
		t.Fatalf("应当是 ErrQuota，实际 %v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("配额错误不该重试（请求数 %d）", n)
	}
	if strings.Contains(err.Error(), "Bearer") {
		t.Fatalf("错误里不该出现凭据")
	}
}
