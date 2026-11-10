package agent

import (
	"context"
	"fmt"

	"qinyuan/internal/llm"
	"qinyuan/internal/model"
)

// 收尾轮的执行体（设计与理由见 settlement.go）。
//
// 单独放一个文件，是因为它和主循环关心的东西不同：
// 主循环关心"这一轮做什么"，收尾轮只关心"把已经做到的东西落库"。

// runSettlement 隐藏探测类工具、注入收尾指令、给有限回合写回。
//
// 两个必须守住的点：
//  1. 用**独立预算**（Settlement.Timeout）：撞时长上限时 execCtx 已经过期，
//     沿用它会一轮都跑不了；
//  2. 不改 out.Reason：收尾轮是"把半成品交付"，不是"完成"。
func (w *Worker) runSettlement(parent context.Context, runID, intentID int64, owner string, rc *Context, emit Emitter, msgs []llm.Message, system string, out *Outcome) {
	cfg := w.Settlement.normalized()
	if cfg.MaxTurns <= 0 || w.Providers == nil {
		return
	}
	if parent.Err() != nil {
		// 被暂停/kill：不硬跑收尾（该让状态机如实记录这次结束）。
		return
	}
	ctx, cancel := context.WithTimeout(parent, cfg.Timeout)
	defer cancel()

	registry := w.Registry.Without(cfg.Hidden...)
	specs := registry.Specs()
	sessionKey := fmt.Sprintf("run%d-intent%d", runID, intentID)

	emit(context.WithoutCancel(parent), &model.Activity{
		RunID: runID, NodeID: &intentID, Worker: owner, Kind: model.ActSystem,
		Summary:  fmt.Sprintf("进入收尾轮（%s）：停止探测，先把已识别内容写回", out.Reason),
		Metadata: map[string]any{"reason": out.Reason, "hidden": cfg.Hidden, "turns": cfg.MaxTurns},
	})
	out.Settlement = true

	msgs = append(append([]llm.Message(nil), msgs...),
		llm.Message{Role: llm.RoleUser, Text: cfg.Prompt})

	for turn := 0; turn < cfg.MaxTurns; turn++ {
		if ctx.Err() != nil {
			return
		}
		resp, err := w.Providers.For(sessionKey).Complete(ctx, &llm.Request{
			SessionKey: sessionKey, System: system, Messages: msgs, Tools: specs,
		})
		if err != nil {
			emit(context.WithoutCancel(parent), &model.Activity{
				RunID: runID, NodeID: &intentID, Worker: owner, Kind: model.ActSystem,
				Summary: "收尾轮调用模型失败（结果可能不完整）",
				Detail:  err.Error(),
			})
			return
		}
		out.Usage.Input += resp.Usage.Input
		out.Usage.Output += resp.Usage.Output
		out.Usage.CacheRead += resp.Usage.CacheRead
		out.Usage.CacheWrite += resp.Usage.CacheWrite

		if resp.Thinking != "" {
			emit(ctx, &model.Activity{
				RunID: runID, NodeID: &intentID, Worker: owner, Kind: model.ActThinking,
				Summary: model.TrimSummary(resp.Thinking, 200),
				Detail:  clampOutput(rc, "thinking", resp.Thinking),
			})
		}
		if resp.Text != "" {
			emit(ctx, &model.Activity{
				RunID: runID, NodeID: &intentID, Worker: owner, Kind: model.ActText,
				Summary: model.TrimSummary(resp.Text, 200),
				Detail:  clampOutput(rc, "text", resp.Text),
			})
		}
		if len(resp.ToolCalls) == 0 {
			break
		}
		msgs = append(msgs, llm.Message{Role: llm.RoleAssistant, Text: resp.Text, ToolCalls: resp.ToolCalls})
		for _, tc := range resp.ToolCalls {
			summary, detail := describeToolCall(tc)
			emit(ctx, &model.Activity{
				RunID: runID, NodeID: &intentID, Worker: owner, Kind: model.ActToolUse,
				Tool: tc.Name, ToolUseID: tc.ID,
				Summary: summary, Detail: clampOutput(rc, "tool_input", detail),
				Metadata: map[string]any{"tool_call_id": tc.ID, "settlement": true},
			})
			res := registry.Call(ctx, rc, tc.Name, tc.Input)
			if !res.IsError {
				switch tc.Name {
				case "record_fact":
					out.Facts += countResults(res.Text)
				case "report_finding":
					out.Findings++
				case "insert_assets":
					out.Assets += countOK(res.Text)
				}
			}
			emit(ctx, &model.Activity{
				RunID: runID, NodeID: &intentID, Worker: owner, Kind: model.ActToolResult,
				Tool: tc.Name, ToolUseID: tc.ID, IsError: res.IsError,
				Summary:  model.TrimSummary(oneLine(res.Text), 200),
				Detail:   clampOutput(rc, "tool_output", res.Text),
				Metadata: map[string]any{"tool_call_id": tc.ID, "settlement": true},
			})
			msgs = append(msgs, llm.Message{
				Role: llm.RoleTool, ToolCallID: tc.ID, Name: tc.Name,
				Text: res.Text, IsError: res.IsError,
			})
		}
	}
}
