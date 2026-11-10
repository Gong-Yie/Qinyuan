package agent

import (
	"context"
	"fmt"
	"log/slog"

	"qinyuan/internal/domain"
	"qinyuan/internal/llm"
	"qinyuan/internal/model"
	"qinyuan/internal/store"
)

// 本文件是第 0 轮的"目标拆解者"（实现规格/03 §4、05-API §5.1 的 [run 启动] 一步）。
//
// 为什么需要它：CreateRun 只能把用户那**一句话**种成唯一目标。真实任务里
// "确认登录入口 + 拿到数据库连接串 + 证明可横向"是三个独立的最终交付物，
// 一个 goal 装不下；而"目标是否达成"是任务唯一的正常结束路径，粗目标会让
// 它永远证明不了，或者被一句"差不多了"糊过去。
//
// 它**只做一次**（run_tasks.goals_at 标记），失败不标记 → 下一轮重试，
// 期间 CreateRun 种下的 seed 目标兜底，任务不会因为拆解失败而停摆。

// GoalDecomposer 是拆解 agent 的执行体。
type GoalDecomposer struct {
	Store     *store.Store
	Providers llm.Factory
	Registry  *Registry
	DataDir   string
	MaxOutput int
	MaxTurns  int
	Logger    *slog.Logger
}

// GoalsResult 一次拆解的产出（给引擎写 activity 与日志用）。
type GoalsResult struct {
	Goals       int
	Constraints int
	Scopes      int
	ToolCalls   int
	Summary     string
	Usage       llm.Usage
}

// Decompose 跑一次目标拆解。它不改变意图/事实，只写 goal / constraints / run_scope。
func (d *GoalDecomposer) Decompose(ctx context.Context, runID int64, emit Emitter) (*GoalsResult, error) {
	if d.Store == nil || d.Providers == nil || d.Registry == nil {
		return nil, fmt.Errorf("goals agent 未装配")
	}
	if emit == nil {
		emit = func(context.Context, *model.Activity) {}
	}
	run, err := d.Store.Run(ctx, runID)
	if err != nil {
		return nil, err
	}
	scopes, err := d.Store.Scopes(ctx, runID)
	if err != nil {
		return nil, err
	}
	constraints, err := d.Store.Constraints(ctx, runID)
	if err != nil {
		return nil, err
	}

	maxTurns := d.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 8
	}
	sessionKey := fmt.Sprintf("run%d-goals", runID)
	provider := d.Providers.For(sessionKey)

	rc := &Context{
		Store: d.Store, RunID: runID,
		Scopes: domain.NewScopeSet(scopes), Constraints: constraints,
		DataDir: d.DataDir, MaxOutput: d.MaxOutput,
	}
	system := BuildGoalsSystem(run, domain.NewScopeSet(scopes).Describe())
	msgs := []llm.Message{{Role: llm.RoleUser, Text: BuildGoalsUserMessage(run, scopes)}}
	specs := d.Registry.Specs()

	out := &GoalsResult{}
	var lastText string

	for turn := 0; turn < maxTurns; turn++ {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		resp, err := provider.Complete(ctx, &llm.Request{
			SessionKey: sessionKey, System: system, Messages: msgs, Tools: specs,
		})
		if err != nil {
			return out, err
		}
		out.Usage.Input += resp.Usage.Input
		out.Usage.Output += resp.Usage.Output
		out.Usage.CacheRead += resp.Usage.CacheRead
		out.Usage.CacheWrite += resp.Usage.CacheWrite

		if resp.Thinking != "" {
			emit(ctx, &model.Activity{
				RunID: runID, Worker: "goals", Kind: model.ActThinking,
				Summary: model.TrimSummary(resp.Thinking, 200),
				Detail:  clampOutput(rc, "thinking", resp.Thinking),
			})
		}
		if resp.Text != "" {
			lastText = resp.Text
			emit(ctx, &model.Activity{
				RunID: runID, Worker: "goals", Kind: model.ActText,
				Summary: model.TrimSummary(resp.Text, 200),
				Detail:  clampOutput(rc, "text", resp.Text),
			})
		}
		if len(resp.ToolCalls) == 0 {
			break
		}

		msgs = append(msgs, llm.Message{Role: llm.RoleAssistant, Text: resp.Text, ToolCalls: resp.ToolCalls})
		for _, tc := range resp.ToolCalls {
			out.ToolCalls++
			summary, detail := describeToolCall(tc)
			emit(ctx, &model.Activity{
				RunID: runID, Worker: "goals", Kind: model.ActToolUse,
				Tool: tc.Name, ToolUseID: tc.ID,
				Summary: summary, Detail: clampOutput(rc, "tool_input", detail),
				Metadata: map[string]any{"tool_call_id": tc.ID},
			})
			res := d.Registry.Call(ctx, rc, tc.Name, tc.Input)
			emit(ctx, &model.Activity{
				RunID: runID, Worker: "goals", Kind: model.ActToolResult,
				Tool: tc.Name, ToolUseID: tc.ID, IsError: res.IsError,
				Summary:  model.TrimSummary(oneLine(res.Text), 200),
				Detail:   clampOutput(rc, "tool_output", res.Text),
				Metadata: map[string]any{"tool_call_id": tc.ID},
			})
			msgs = append(msgs, llm.Message{
				Role: llm.RoleTool, ToolCallID: tc.ID, Name: tc.Name,
				Text: res.Text, IsError: res.IsError,
			})
		}
	}

	// 统计口径直接从库里读回来：模型说它建了几条不算数，DB 里有几条才算。
	goals, err := d.Store.Goals(ctx, runID)
	if err != nil {
		return out, err
	}
	for _, g := range goals {
		if g.Origin == "goals" && g.State == model.StateOpen {
			out.Goals++
		}
	}
	if cs, err := d.Store.Constraints(ctx, runID); err == nil {
		out.Constraints = len(cs)
	}
	if ss, err := d.Store.Scopes(ctx, runID); err == nil {
		out.Scopes = len(ss)
	}
	out.Summary = model.TrimSummary(lastText, 800)

	emit(context.WithoutCancel(ctx), &model.Activity{
		RunID: runID, Worker: "goals", Kind: model.ActResult,
		Summary: fmt.Sprintf("目标拆解结束：%d 条目标，%d 条约束，%d 条范围",
			out.Goals, out.Constraints, out.Scopes),
		Detail:          out.Summary,
		InputTokens:     out.Usage.Input,
		OutputTokens:    out.Usage.Output,
		CacheReadTokens: out.Usage.CacheRead, CacheWriteTokens: out.Usage.CacheWrite,
		Metadata: map[string]any{
			"goals": out.Goals, "constraints": out.Constraints, "scopes": out.Scopes,
			"tool_calls": out.ToolCalls,
		},
	})
	return out, nil
}
