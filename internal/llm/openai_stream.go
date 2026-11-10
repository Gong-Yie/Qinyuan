package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// 本文件是 OpenAI Chat Completions 的 SSE 流式适配（M4c）。
//
// wire 格式（每行一个 `data: {...}`，以 `data: [DONE]` 收尾）：
//
//	choices[0].delta.content        —— 正文增量
//	choices[0].delta.tool_calls[]   —— 工具调用增量（按 index 合并，arguments 是**分片**）
//	usage                           —— 最后一个 chunk 里（需要 stream_options.include_usage）
//
// 两个容易写错的地方：
//  1. tool_calls 的 arguments 是**字符串分片**，必须按 index 追加，不能整块覆盖；
//  2. usage 只在带 include_usage 时出现，且不一定在最后一行 —— 见到就记，缺了就当 0。

type oaStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content          *string `json:"content"`
			ReasoningContent *string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Stream 实现 Streamer。
//
// 重试语义（与 Complete 的关键差异）：
//   - 未 committed：可以重试（网络/5xx/429），最多 MaxRetries 次；
//   - 已 committed：不再重试，返回**部分结果 + ErrStreamInterrupted**
//     （上层据此把它当作"这次 run 的部分结果"，而不是重放整条意图）。
func (o *OpenAI) Stream(ctx context.Context, req *Request, onEvent func(StreamEvent) error) (*Response, error) {
	body, err := o.buildBody(req)
	if err != nil {
		return nil, err
	}
	// 打开 include_usage：否则流式响应里没有 token 账。
	body = injectStreamOptions(body)

	var lastErr error
	for attempt := 0; attempt <= o.cfg.MaxRetries; attempt++ {
		resp, committed, err := o.streamOnce(ctx, body, onEvent)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if committed {
			// 已经吐过内容：绝不重放（否则用户会看到两遍）。
			if resp == nil {
				resp = &Response{Provider: "openai", Model: o.cfg.Model}
			}
			resp.StopReason = "error"
			return resp, fmt.Errorf("%w: %v", ErrStreamInterrupted, err)
		}
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

func injectStreamOptions(body []byte) []byte {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	m["stream"] = true
	m["stream_options"] = map[string]any{"include_usage": true}
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// streamOnce 跑一次流式请求。返回 (部分响应, 是否已 committed, error)。
func (o *OpenAI) streamOnce(ctx context.Context, body []byte, onEvent func(StreamEvent) error) (*Response, bool, error) {
	url := o.cfg.BaseURL + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, false, fmt.Errorf("%w: 构造请求失败：%v", ErrBadRequest, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if o.cfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+o.cfg.APIKey)
	}

	resp, err := o.client.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		return nil, false, fmt.Errorf("%w: %v", ErrTransient, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, false, classifyStatus(resp.StatusCode, raw)
	}

	out := &Response{Provider: "openai", Model: o.cfg.Model}
	var text strings.Builder
	var thinking strings.Builder
	calls := map[int]*ToolCall{}
	committed := false

	emit := func(kind, s string) error {
		if s == "" {
			return nil
		}
		first := !committed
		committed = true
		if onEvent == nil {
			return nil
		}
		return onEvent(StreamEvent{Kind: kind, Text: s, Committed: first})
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue // event:/id:/retry: 之类，忽略
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk oaStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			// 单行坏 JSON 不该打断整个流（网关偶尔会插一行脏数据）。
			continue
		}
		if chunk.Error != nil {
			return partial(out, &text, &thinking, calls), committed,
				classifyMessage(chunk.Error.Message)
		}
		if chunk.Usage != nil {
			out.Usage = Usage{
				Input:     chunk.Usage.PromptTokens,
				Output:    chunk.Usage.CompletionTokens,
				CacheRead: chunk.Usage.PromptTokensDetails.CachedTokens,
			}
		}
		for _, ch := range chunk.Choices {
			if ch.Delta.ReasoningContent != nil && *ch.Delta.ReasoningContent != "" {
				thinking.WriteString(*ch.Delta.ReasoningContent)
				if err := emit(StreamThinking, *ch.Delta.ReasoningContent); err != nil {
					return nil, committed, err
				}
			}
			if ch.Delta.Content != nil && *ch.Delta.Content != "" {
				text.WriteString(*ch.Delta.Content)
				if err := emit(StreamText, *ch.Delta.Content); err != nil {
					return nil, committed, err
				}
			}
			for _, tc := range ch.Delta.ToolCalls {
				cur := calls[tc.Index]
				if cur == nil {
					cur = &ToolCall{}
					calls[tc.Index] = cur
				}
				if tc.ID != "" {
					cur.ID = tc.ID
				}
				if tc.Function.Name != "" {
					cur.Name = tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					// arguments 是分片：必须追加。
					cur.Input = append(cur.Input, tc.Function.Arguments...)
				}
				// 工具调用的开始也是"交付"：之后不能再换人重放。
				if !committed {
					committed = true
					if onEvent != nil {
						_ = onEvent(StreamEvent{Kind: StreamText, Text: "", Committed: true})
					}
				}
			}
			if ch.FinishReason != "" {
				out.StopReason = ch.FinishReason
			}
		}
	}
	if err := sc.Err(); err != nil {
		if ctx.Err() != nil {
			return partial(out, &text, &thinking, calls), committed, ctx.Err()
		}
		return partial(out, &text, &thinking, calls), committed,
			fmt.Errorf("%w: 读流失败：%v", ErrTransient, err)
	}

	result := partial(out, &text, &thinking, calls)
	if result.Text == "" && len(result.ToolCalls) == 0 {
		// 一个事件都没有：这是"空响应"，可以重试（L2）。
		return nil, false, fmt.Errorf("%w: 流里没有任何内容", ErrTransient)
	}
	if result.StopReason == "" {
		result.StopReason = "stop"
	}
	if onEvent != nil {
		_ = onEvent(StreamEvent{Kind: StreamDone})
	}
	return result, committed, nil
}

func partial(out *Response, text, thinking *strings.Builder, calls map[int]*ToolCall) *Response {
	if out == nil {
		out = &Response{Provider: "openai"}
	}
	out.Text = text.String()
	out.Thinking = thinking.String()
	// 按 index 顺序还原工具调用（map 遍历是随机的）。
	idx := make([]int, 0, len(calls))
	for i := range calls {
		idx = append(idx, i)
	}
	for i := 0; i < len(idx); i++ {
		for j := i + 1; j < len(idx); j++ {
			if idx[j] < idx[i] {
				idx[i], idx[j] = idx[j], idx[i]
			}
		}
	}
	for _, i := range idx {
		tc := calls[i]
		if len(tc.Input) == 0 {
			tc.Input = json.RawMessage(`{}`)
		} else if !json.Valid(tc.Input) {
			tc.Input = json.RawMessage(`{}`)
		}
		out.ToolCalls = append(out.ToolCalls, *tc)
	}
	return out
}
