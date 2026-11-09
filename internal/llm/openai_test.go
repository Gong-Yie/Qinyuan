package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 一次完整的请求/响应映射：system、消息、工具 schema、tool_calls、usage。
func TestOpenAICompleteMapsTools(t *testing.T) {
	var body map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("路径 = %q，期望 /chat/completions", r.URL.Path)
		}
		auth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("请求体不是 JSON：%v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
  "choices": [{
    "message": {
      "content": "先看一下当前态势",
      "tool_calls": [{
        "id": "call_1",
        "type": "function",
        "function": {"name": "add_intent", "arguments": "{\"intents\":[{\"summary\":\"探测首页\"}]}"}
      }]
    },
    "finish_reason": "tool_calls"
  }],
  "usage": {"prompt_tokens": 123, "completion_tokens": 45, "prompt_tokens_details": {"cached_tokens": 10}}
}`)
	}))
	defer srv.Close()

	p, err := NewOpenAI(OpenAIConfig{BaseURL: srv.URL + "/", APIKey: "sk-test", Model: "test-model"})
	if err != nil {
		t.Fatal(err)
	}
	schema := json.RawMessage(`{"type":"object","properties":{"intents":{"type":"array"}}}`)
	resp, err := p.Complete(context.Background(), &Request{
		System:   "你是规划者",
		Messages: []Message{{Role: RoleUser, Text: "开始"}},
		Tools:    []ToolSpec{{Name: "add_intent", Description: "派意图", Schema: schema}},
	})
	if err != nil {
		t.Fatalf("调用失败：%v", err)
	}

	if auth != "Bearer sk-test" {
		t.Errorf("Authorization = %q", auth)
	}
	if body["model"] != "test-model" {
		t.Errorf("model = %v", body["model"])
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages 数量 = %d，期望 2（system + user）", len(msgs))
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "你是规划者" {
		t.Errorf("system 消息不对：%v", first)
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools 数量 = %d", len(tools))
	}
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "add_intent" {
		t.Errorf("工具名 = %v", fn["name"])
	}
	if _, ok := fn["parameters"].(map[string]any); !ok {
		t.Errorf("工具 parameters 不是对象：%T", fn["parameters"])
	}

	if resp.Text != "先看一下当前态势" {
		t.Errorf("Text = %q", resp.Text)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "add_intent" {
		t.Fatalf("tool_calls 解析失败：%+v", resp.ToolCalls)
	}
	var args struct{ Intents []struct{ Summary string } }
	if err := json.Unmarshal(resp.ToolCalls[0].Input, &args); err != nil {
		t.Fatalf("工具参数不是合法 JSON：%v", err)
	}
	if len(args.Intents) != 1 || args.Intents[0].Summary != "探测首页" {
		t.Errorf("工具参数 = %+v", args)
	}
	if resp.Usage.Input != 123 || resp.Usage.Output != 45 || resp.Usage.CacheRead != 10 {
		t.Errorf("usage = %+v", resp.Usage)
	}
	if resp.StopReason != "tool_calls" {
		t.Errorf("stop reason = %q", resp.StopReason)
	}
}

// 状态码 → 错误类：这决定了上层能不能换人/能不能重试。
func TestOpenAIErrorClassification(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"余额耗尽", 429, `{"error":{"message":"You exceeded your current quota: insufficient_quota"}}`, ErrQuota},
		{"普通限流", 429, `{"error":{"message":"rate limit reached, 100 rpm"}}`, ErrRateLimited},
		{"服务端错误", 500, `{"error":{"message":"boom"}}`, ErrTransient},
		{"鉴权失败", 401, `{"error":{"message":"bad key"}}`, ErrAuth},
		{"请求错误", 400, `{"error":{"message":"bad request"}}`, ErrBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
				fmt.Fprint(w, c.body)
			}))
			defer srv.Close()

			p, err := NewOpenAI(OpenAIConfig{BaseURL: srv.URL, Model: "m", MaxRetries: 0})
			if err != nil {
				t.Fatal(err)
			}
			_, err = p.Complete(context.Background(), &Request{
				Messages: []Message{{Role: RoleUser, Text: "hi"}},
			})
			if !errors.Is(err, c.want) {
				t.Fatalf("错误 = %v，期望 %v", err, c.want)
			}
		})
	}
}

// quota 关键词不能把普通限流误判成余额问题（否则会白白换人）。
func TestLooksLikeQuota(t *testing.T) {
	quota := []string{"insufficient_quota", "quota exhausted", "余额不足", "账户配额已用尽"}
	notQuota := []string{"rate limit reached", "429 too many requests", "rpm exceeded", "tpm limit"}
	for _, s := range quota {
		if !looksLikeQuota(s) {
			t.Errorf("%q 应识别为配额问题", s)
		}
	}
	for _, s := range notQuota {
		if looksLikeQuota(s) {
			t.Errorf("%q 不该识别为配额问题", s)
		}
	}
}

// 服务端持续 500 时，重试到上限后返回 ErrTransient（不无限转圈）。
func TestOpenAIRetriesTransientThenGivesUp(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(500)
		fmt.Fprint(w, `{"error":{"message":"boom"}}`)
	}))
	defer srv.Close()

	p, err := NewOpenAI(OpenAIConfig{BaseURL: srv.URL, Model: "m", MaxRetries: 1})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Complete(context.Background(), &Request{
		Messages: []Message{{Role: RoleUser, Text: "hi"}},
	})
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("错误 = %v，期望 ErrTransient", err)
	}
	if hits != 2 {
		t.Fatalf("请求次数 = %d，期望 2（首次 + 1 次重试）", hits)
	}
}
