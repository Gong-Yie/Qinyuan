package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// OpenAIConfig 是 OpenAI Chat Completions 兼容后端的配置
// （DeepSeek / OpenAI / 自建网关 / vLLM 都走这一套）。
type OpenAIConfig struct {
	BaseURL     string
	APIKey      string
	Model       string
	MaxTokens   int
	Temperature float64
	Timeout     time.Duration
	MaxRetries  int
	HTTPClient  *http.Client
}

// OpenAI 是 provider 实现。它是无状态的，可并发使用。
type OpenAI struct {
	cfg    OpenAIConfig
	client *http.Client
}

// NewOpenAI 校验配置并返回 provider。
func NewOpenAI(cfg OpenAIConfig) (*OpenAI, error) {
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("LLM base_url 不能为空")
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, fmt.Errorf("LLM model 不能为空")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 120 * time.Second
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = 4096
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: cfg.Timeout}
	}
	return &OpenAI{cfg: cfg, client: cfg.HTTPClient}, nil
}

// Model 返回当前模型名（日志/记账用）。
func (o *OpenAI) Model() string { return o.cfg.Model }

// OpenAIFactory 让 planner/worker 共用同一个 provider（无状态，不需要按会话复制）。
func NewOpenAIFactory(cfg OpenAIConfig) (Factory, error) {
	p, err := NewOpenAI(cfg)
	if err != nil {
		return nil, err
	}
	return &singleFactory{p: p}, nil
}

type singleFactory struct{ p Provider }

func (f *singleFactory) For(string) Provider { return f.p }

// ---------- 请求/响应结构 ----------

type oaMessage struct {
	Role       string       `json:"role"`
	Content    *string      `json:"content"`
	ToolCalls  []oaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
	Name       string       `json:"name,omitempty"`
}

type oaToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type oaRequest struct {
	Model       string      `json:"model"`
	Messages    []oaMessage `json:"messages"`
	Tools       []oaTool    `json:"tools,omitempty"`
	ToolChoice  string      `json:"tool_choice,omitempty"`
	MaxTokens   int         `json:"max_tokens,omitempty"`
	Temperature float64     `json:"temperature,omitempty"`
}

type oaResponse struct {
	Choices []struct {
		Message struct {
			Content   *string      `json:"content"`
			ToolCalls []oaToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error"`
}

// Complete 调一次模型。
//
// 重试只覆盖"没有交付任何内容之前"的失败（L1/L2）：429/5xx/网络。
// 因为非流式调用要么整块返回、要么失败，不存在"吐了一半再换人"的问题。
func (o *OpenAI) Complete(ctx context.Context, req *Request) (*Response, error) {
	body, err := o.buildBody(req)
	if err != nil {
		return nil, err
	}

	var lastErr error
	for attempt := 0; attempt <= o.cfg.MaxRetries; attempt++ {
		resp, err := o.roundTrip(ctx, body)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !retryable(err) || ctx.Err() != nil {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff(attempt)):
		}
	}
	return nil, lastErr
}

func backoff(attempt int) time.Duration {
	d := time.Duration(1<<attempt) * time.Second
	if d > 8*time.Second {
		d = 8 * time.Second
	}
	return d
}

func retryable(err error) bool {
	return errors.Is(err, ErrRateLimited) || errors.Is(err, ErrTransient)
}

func (o *OpenAI) buildBody(req *Request) ([]byte, error) {
	msgs := make([]oaMessage, 0, len(req.Messages)+1)
	if strings.TrimSpace(req.System) != "" {
		s := req.System
		msgs = append(msgs, oaMessage{Role: "system", Content: &s})
	}
	for _, m := range req.Messages {
		om := oaMessage{Role: string(m.Role), ToolCallID: m.ToolCallID, Name: m.Name}
		if m.Text != "" {
			t := m.Text
			om.Content = &t
		}
		for _, tc := range m.ToolCalls {
			om.ToolCalls = append(om.ToolCalls, oaToolCall{
				ID: tc.ID, Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{Name: tc.Name, Arguments: string(tc.Input)},
			})
		}
		msgs = append(msgs, om)
	}

	tools := make([]oaTool, 0, len(req.Tools))
	for _, t := range req.Tools {
		var ot oaTool
		ot.Type = "function"
		ot.Function.Name = t.Name
		ot.Function.Description = t.Description
		ot.Function.Parameters = t.Schema
		tools = append(tools, ot)
	}

	body := oaRequest{
		Model:       o.cfg.Model,
		Messages:    msgs,
		Tools:       tools,
		MaxTokens:   o.cfg.MaxTokens,
		Temperature: o.cfg.Temperature,
	}
	if len(tools) > 0 {
		body.ToolChoice = "auto"
	}
	return json.Marshal(body)
}

func (o *OpenAI) roundTrip(ctx context.Context, body []byte) (*Response, error) {
	url := o.cfg.BaseURL + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: 构造请求失败：%v", ErrBadRequest, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if o.cfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+o.cfg.APIKey)
	}

	resp, err := o.client.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %v", ErrTransient, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: 读响应失败：%v", ErrTransient, err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, classifyStatus(resp.StatusCode, raw)
	}

	var parsed oaResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("%w: 响应不是合法 JSON：%v", ErrTransient, err)
	}
	if parsed.Error != nil {
		return nil, classifyMessage(parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return nil, fmt.Errorf("%w: 响应里没有 choices", ErrTransient)
	}

	choice := parsed.Choices[0]
	out := &Response{
		Usage: Usage{
			Input:     parsed.Usage.PromptTokens,
			Output:    parsed.Usage.CompletionTokens,
			CacheRead: parsed.Usage.PromptTokensDetails.CachedTokens,
		},
		StopReason: choice.FinishReason,
		Provider:   "openai",
		Model:      o.cfg.Model,
	}
	if choice.Message.Content != nil {
		out.Text = *choice.Message.Content
	}
	for _, tc := range choice.Message.ToolCalls {
		rawArgs := json.RawMessage(tc.Function.Arguments)
		if len(rawArgs) == 0 || !json.Valid(rawArgs) {
			// 模型偶尔吐坏 JSON：交给工具层报"参数解析失败"，而不是在这里 panic。
			rawArgs = json.RawMessage(`{}`)
		}
		out.ToolCalls = append(out.ToolCalls, ToolCall{
			ID: tc.ID, Name: tc.Function.Name, Input: rawArgs,
		})
	}
	return out, nil
}

// classifyStatus 把 HTTP 状态码映射成可重试/不可重试的错误类。
func classifyStatus(status int, body []byte) error {
	snippet := string(body)
	if len(snippet) > 300 {
		snippet = snippet[:300]
	}
	msg := fmt.Sprintf("HTTP %d: %s", status, snippet)
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return fmt.Errorf("%w: %s", ErrAuth, msg)
	case status == http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrBadRequest, msg)
	case status == http.StatusTooManyRequests:
		if looksLikeQuota(snippet) {
			// 429 既可能是限流也可能是余额耗尽：余额问题重试没用，必须让它立刻换人。
			return fmt.Errorf("%w: %s", ErrQuota, msg)
		}
		return fmt.Errorf("%w: %s", ErrRateLimited, msg)
	case status >= 500 || status == http.StatusRequestTimeout:
		return fmt.Errorf("%w: %s", ErrTransient, msg)
	default:
		return fmt.Errorf("%w: %s", ErrBadRequest, msg)
	}
}

func classifyMessage(m string) error {
	if looksLikeQuota(m) {
		return fmt.Errorf("%w: %s", ErrQuota, m)
	}
	return fmt.Errorf("%w: %s", ErrBadRequest, m)
}

// looksLikeQuota 只认"余额/配额"类关键词，刻意排除 rate limit / rpm / tpm。
func looksLikeQuota(s string) bool {
	l := strings.ToLower(s)
	quotaWords := []string{
		"insufficient_quota", "insufficient quota", "quota exhausted", "quota exceeded",
		"余额", "额度不足", "配额",
	}
	for _, w := range quotaWords {
		if strings.Contains(l, w) {
			return true
		}
	}
	return false
}
