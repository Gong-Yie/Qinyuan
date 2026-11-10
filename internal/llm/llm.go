// Package llm 是模型接入层：provider 抽象 + 事件类型 + 可编程假 provider。
//
// M1 只落地 fake provider（先把调度/工具/写回链路跑通）；
// 真实 provider（OpenAI 兼容 / Anthropic）在 M2 接入，接口不变。
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Role 消息角色。
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ToolCall 模型请求执行的一次工具调用。
type ToolCall struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// Usage 一次模型调用的 token 账。
type Usage struct {
	Input      int `json:"input_tokens"`
	Output     int `json:"output_tokens"`
	CacheRead  int `json:"cache_read_tokens"`
	CacheWrite int `json:"cache_write_tokens"`
	// Reasoning 是 output 里花在"思考"上的部分（OpenAI 兼容的
	// completion_tokens_details.reasoning_tokens）。它计入 max_tokens，
	// 所以推理型模型在"输入很重"的回合会把输出预算吃光。
	Reasoning int `json:"reasoning_tokens,omitempty"`
}

// Message 会话历史里的一条。
type Message struct {
	Role       Role       `json:"role"`
	Text       string     `json:"text,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
	IsError    bool       `json:"is_error,omitempty"`
}

// ToolSpec 给模型看的工具定义（纯数据）。
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
}

// Request 一次模型调用。
type Request struct {
	SessionKey string // 用于按会话选择脚本/provider（假 provider 靠它隔离；真 provider 可忽略）
	System     string
	Messages   []Message
	Tools      []ToolSpec
}

// Response 一次模型调用的结果（非流式：M1 用整块，M2 再引入增量事件）。
type Response struct {
	Text       string
	Thinking   string
	ToolCalls  []ToolCall
	Usage      Usage
	StopReason string // stop | tool_use | max_tokens | error
	Provider   string
	Model      string
}

// Provider 模型调用的最小接口。
type Provider interface {
	Complete(ctx context.Context, req *Request) (*Response, error)
}

// Factory 按会话键分发 provider（一条意图一个会话，互不污染）。
type Factory interface {
	For(sessionKey string) Provider
}

// 常见错误分类：上层据此决定"能不能换人"（见 05-可靠性 §2）。
var (
	ErrRateLimited = errors.New("llm: rate limited")
	ErrQuota       = errors.New("llm: quota exhausted")
	ErrAuth        = errors.New("llm: auth failed")
	ErrTransient   = errors.New("llm: transient")
	ErrBadRequest  = errors.New("llm: bad request")
	// ErrReasoningOnly：模型只吐了推理，正文与工具调用都没给。
	// 与"空流"的区别是**它不是抖动**——重放同样的请求只会再撞一次，
	// 所以要单独分类（不可重试），并让上层看到 finish_reason 与 max_tokens。
	ErrReasoningOnly = errors.New("llm: reasoning-only")
)

// EmptyStreamError 是"200 但一个内容事件都没有"的现场。
//
// 之前这里只写一句「流里没有任何内容」，排查时无法区分
// "网关零字节"（字节 0）、"只有 [DONE]"、"读了一半断了"。详见 docs/19。
type EmptyStreamError struct {
	Status    int
	Elapsed   time.Duration
	DataLines int // 收到的 data: 行数（含 [DONE]）
	Bytes     int // 原始响应字节数
	OnlyDone  bool
}

func (e *EmptyStreamError) Error() string {
	shape := "零数据事件"
	if e.OnlyDone {
		shape = "只有 [DONE]"
	}
	return fmt.Sprintf("%v: 模型没有返回任何内容（HTTP %d，%s，%d 字节，耗时 %s）——"+
		"优先查网关/网络；这是可重试的",
		ErrTransient, e.Status, shape, e.Bytes, e.Elapsed.Round(time.Millisecond))
}

// Unwrap 让它仍然算 ErrTransient：上层原有的重试策略不变。
func (e *EmptyStreamError) Unwrap() error { return ErrTransient }

// ReasoningOnlyError 是"推理把输出预算吃光"的现场。
//
// 实测（docs/19）：deepseek-flash 对 1KB hexdump 做结构分析时，
// max_tokens=4096 全部花在 reasoning_tokens 上，正文 0 字节、无工具调用。
// 这种失败重试无意义，必须换输入或调大预算。
type ReasoningOnlyError struct {
	Finish          string
	ReasoningTokens int
	ReasoningChars  int
	MaxTokens       int
}

func (e *ReasoningOnlyError) Error() string {
	finish := e.Finish
	if finish == "" {
		finish = "unknown"
	}
	return fmt.Sprintf("%v: 只有推理没有正文（finish=%s，推理 %d token / %d 字，输出上限 max_tokens=%d）——"+
		"模型把整份输出预算花在思考上；不要原样重试，换更小的输入（例如不要喂大段 hexdump）或调大 QINYUAN_LLM_MAX_TOKENS",
		ErrReasoningOnly, finish, e.ReasoningTokens, e.ReasoningChars, e.MaxTokens)
}

// Unwrap 只暴露 ErrReasoningOnly：它不是 ErrTransient，不该被重试。
func (e *ReasoningOnlyError) Unwrap() error { return ErrReasoningOnly }

// ---------- 假 provider ----------

// Step 一段脚本：第 N 次调用返回什么。
type Step struct {
	Text       string
	Thinking   string
	ToolCalls  []ToolCall
	Usage      Usage
	Err        error
	Delay      time.Duration
	StopReason string
}

// FakeFactory 为每个会话复制一份脚本，保证测试可重复、并发隔离。
type FakeFactory struct {
	script []Step

	mu       sync.Mutex
	sessions map[string]*fakeSession
	// Calls 记录每个会话被调用了几次（测试断言用）。
	Calls map[string]int
}

type fakeSession struct {
	f    *FakeFactory
	key  string
	next int
}

// NewFakeFactory 用同一份脚本构造所有会话的 provider。
func NewFakeFactory(script ...Step) *FakeFactory {
	return &FakeFactory{script: append([]Step(nil), script...), sessions: map[string]*fakeSession{}, Calls: map[string]int{}}
}

// For 实现 Factory。
func (f *FakeFactory) For(sessionKey string) Provider {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[sessionKey]
	if !ok {
		s = &fakeSession{f: f, key: sessionKey}
		f.sessions[sessionKey] = s
	}
	return s
}

// CallCount 读某个会话被调用过几次（带锁：引擎在后台跑时测试也能安全读）。
//
// 直接用 f.Calls[key] 读会和 fakeSession.Complete 的写入构成数据竞争（-race 会抓到）。
func (f *FakeFactory) CallCount(sessionKey string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Calls[sessionKey]
}

func (s *fakeSession) Complete(ctx context.Context, req *Request) (*Response, error) {
	s.f.mu.Lock()
	idx := s.next
	s.next++
	s.f.Calls[s.key]++
	var st Step
	if idx < len(s.f.script) {
		st = s.f.script[idx]
	} else {
		// 脚本用尽：返回一个空的终止响应，而不是 panic。
		st = Step{Text: "（脚本已用尽）", StopReason: "stop"}
	}
	s.f.mu.Unlock()

	if st.Delay > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(st.Delay):
		}
	}
	if st.Err != nil {
		return nil, st.Err
	}
	resp := &Response{
		Text:       st.Text,
		Thinking:   st.Thinking,
		ToolCalls:  st.ToolCalls,
		Usage:      st.Usage,
		StopReason: st.StopReason,
		Provider:   "fake",
		Model:      "fake-scripted",
	}
	if resp.StopReason == "" {
		if len(resp.ToolCalls) > 0 {
			resp.StopReason = "tool_use"
		} else {
			resp.StopReason = "stop"
		}
	}
	return resp, nil
}

// NewToolCall 是写脚本时的便捷构造。
func NewToolCall(id, name string, input any) ToolCall {
	raw, err := json.Marshal(input)
	if err != nil {
		panic(fmt.Sprintf("llm.NewToolCall: %v", err))
	}
	return ToolCall{ID: id, Name: name, Input: raw}
}

// DynamicFactory 用函数实现 provider：测试里可以读请求内容再决定响应，
// 用来验证"planner 看到上一轮产物后再行动"这类需要上下文的链路。
type DynamicFactory struct {
	Fn func(ctx context.Context, req *Request) (*Response, error)
}

// For 实现 Factory（所有会话共享同一个函数）。
func (f *DynamicFactory) For(string) Provider { return dynamicProvider{f} }

type dynamicProvider struct{ f *DynamicFactory }

func (p dynamicProvider) Complete(ctx context.Context, req *Request) (*Response, error) {
	return p.f.Fn(ctx, req)
}
