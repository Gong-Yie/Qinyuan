package agent

import (
	"context"
	"strings"
	"testing"

	"qinyuan/internal/llm"
)

// 本文件是 M4a 的"上下文预算"验收：压缩只做该做的事，且绝不破坏消息结构。
//
// 三条底线：
//  1. 没超阈值 → 一个字都不改（不要为了"看起来在工作"而乱压）；
//  2. micro-compact 之后够用 → 不再花一次模型调用去摘要；
//  3. 摘要之后 → tool_call ↔ tool_result 的配对仍然完整（否则服务端直接 400）。

func TestEstimateTokensMonotonic(t *testing.T) {
	if EstimateTokens("") != 0 {
		t.Fatalf("空串应当是 0")
	}
	short := EstimateTokens("hello")
	long := EstimateTokens(strings.Repeat("hello", 100))
	if short <= 0 || long <= short {
		t.Fatalf("估计值必须随长度增长：short=%d long=%d", short, long)
	}
	cjk := EstimateTokens("中文四个字")
	if cjk < 4 {
		t.Fatalf("CJK 至少一字一 token，实际 %d", cjk)
	}
}

// buildRounds 造 head + n 轮对话，每轮一条 assistant（带 tool_call）+ 一条巨大的 tool 结果。
func buildRounds(n int, bodyChars int) []llm.Message {
	msgs := []llm.Message{{Role: llm.RoleUser, Text: "【本任务当前态势】…"}}
	for i := 0; i < n; i++ {
		id := "call-" + string(rune('a'+i))
		msgs = append(msgs, llm.Message{
			Role: llm.RoleAssistant, Text: "第 " + id + " 步说明",
			ToolCalls: []llm.ToolCall{{ID: id, Name: "http_request", Input: []byte(`{"url":"http://x/","method":"GET"}`)}},
		})
		msgs = append(msgs, llm.Message{
			Role: llm.RoleTool, ToolCallID: id, Name: "http_request",
			Text: strings.Repeat("响应正文", bodyChars/4),
		})
	}
	return msgs
}

// assertToolPairing 断言每个 tool 消息都能在前面的 assistant 里找到对应调用。
func assertToolPairing(t *testing.T, msgs []llm.Message) {
	t.Helper()
	seen := map[string]bool{}
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			seen[tc.ID] = true
		}
		if m.Role == llm.RoleTool {
			if !seen[m.ToolCallID] {
				t.Fatalf("孤立的 tool 消息（没有对应 tool_call）：%q", m.ToolCallID)
			}
		}
	}
}

// 没超阈值：一个字节都不该改。
func TestCompactionNoOpUnderThreshold(t *testing.T) {
	msgs := buildRounds(3, 40)
	before := estimateMessages("sys", msgs)
	cfg := CompactionConfig{WindowTokens: 100000} // 阈值远高于实际
	got, changed, err := compactMessages(context.Background(), llm.NewFakeFactory().For("compact"), cfg, "sys", msgs)
	if err != nil || changed {
		t.Fatalf("不该触发压缩：changed=%v err=%v", changed, err)
	}
	if estimateMessages("sys", got) != before {
		t.Fatalf("内容被改了但 beyond threshold")
	}
}

// micro-compact 够用：清老工具结果，但不该花一次模型调用去摘要。
func TestCompactionMicrosOldToolResults(t *testing.T) {
	msgs := buildRounds(6, 4000) // 每轮约 1000 token
	orig := len(msgs)

	calls := 0
	prov := &llm.DynamicFactory{Fn: func(context.Context, *llm.Request) (*llm.Response, error) {
		calls++
		return &llm.Response{Text: "不该被调用"}, nil
	}}
	// 阈值放在"6 轮全量（≈24k）"和"只留最近 2 轮（≈8k）"之间：micro 就够，
	// 不该再花一次模型调用去摘要。
	cfg := CompactionConfig{WindowTokens: 20000, Threshold: 0.6, KeepRounds: 2}
	got, changed, err := compactMessages(context.Background(), prov.For("compact"), cfg, "sys", msgs)
	if err != nil || !changed {
		t.Fatalf("应当触发压缩：changed=%v err=%v", changed, err)
	}
	if calls != 0 {
		t.Fatalf("micro-compact 阶段不该调模型（摘要才需要），实际调用 %d 次", calls)
	}
	if len(got) != orig {
		t.Fatalf("micro-compact 不该改变消息条数：%d → %d", orig, len(got))
	}
	assertToolPairing(t, got)
	if !strings.Contains(got[2].Text, microCompactMarker) {
		t.Fatalf("老工具结果没被清理：%q", headRunes(got[2].Text, 80))
	}
	last := got[len(got)-1]
	if strings.Contains(last.Text, microCompactMarker) {
		t.Fatalf("最近的工具结果必须保留原文")
	}
}

// 摘要：保留首条 user + 最近 1 轮，中段压成一段；配对必须仍完整。
func TestCompactionSummarizesAndKeepsPairing(t *testing.T) {
	msgs := buildRounds(8, 4000)
	calls := 0
	prov := &llm.DynamicFactory{Fn: func(_ context.Context, req *llm.Request) (*llm.Response, error) {
		calls++
		if !strings.Contains(req.System, "上下文压缩器") {
			t.Fatalf("摘要调用应当带压缩器提示，实际 system=%q", headRunes(req.System, 60))
		}
		return &llm.Response{Text: "已确认：首页 200；还差：登录接口的鉴权方式"}, nil
	}}
	cfg := CompactionConfig{WindowTokens: 3000, Threshold: 0.6, KeepRounds: 1}
	got, changed, err := compactMessages(context.Background(), prov.For("compact"), cfg, "sys", msgs)
	if err != nil || !changed {
		t.Fatalf("应当触发压缩：changed=%v err=%v", changed, err)
	}
	if calls != 1 {
		t.Fatalf("应当只摘要一次，实际 %d 次", calls)
	}
	if !strings.Contains(got[0].Text, "【历史摘要") {
		t.Fatalf("首条 user 消息里应当带上摘要：%q", headRunes(got[0].Text, 120))
	}
	if !strings.Contains(got[0].Text, "还差") {
		t.Fatalf("摘要正文没写进去")
	}
	if len(got) != 3 { // head + 最近 1 轮（assistant + tool）
		t.Fatalf("结构不对：期望 3 条，实际 %d", len(got))
	}
	assertToolPairing(t, got)
	if got[0].Role != llm.RoleUser {
		t.Fatalf("摘要必须挂在首条 user 消息上，实际 role=%s", got[0].Role)
	}
}

// 摘要失败不能让这一轮直接失败（退回 micro-compact 的结果）。
func TestCompactionSummarizerFailureStillCompacts(t *testing.T) {
	msgs := buildRounds(8, 4000)
	prov := &llm.DynamicFactory{Fn: func(context.Context, *llm.Request) (*llm.Response, error) {
		return nil, llm.ErrTransient
	}}
	cfg := CompactionConfig{WindowTokens: 3000, Threshold: 0.6, KeepRounds: 2}
	got, changed, err := compactMessages(context.Background(), prov.For("compact"), cfg, "sys", msgs)
	if err != nil {
		t.Fatalf("摘要失败不该把错误抛给调用方：%v", err)
	}
	if !changed {
		t.Fatalf("至少 micro-compact 应当生效")
	}
	assertToolPairing(t, got)
}
