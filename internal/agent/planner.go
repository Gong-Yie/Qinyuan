package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"qinyuan/internal/domain"
	"qinyuan/internal/llm"
	"qinyuan/internal/model"
	"qinyuan/internal/store"
)

// Trigger 是"本轮为什么被唤醒"的一条记录。
//
// planner 每轮都是全新会话，它靠这个知道世界刚刚发生了什么 —— 而不是靠历史。
type Trigger struct {
	Kind     string // done | finding | goal | goal_edited | goal_deleted | hint | cancelled | heartbeat
	IntentID int64
	Summary  string
	NodeID   int64
	Text     string
}

// Line 渲染成一行"点名具体"的触发说明（不允许只说"图变了"）。
func (t Trigger) Line() string {
	switch t.Kind {
	case "done":
		s := fmt.Sprintf("- 意图 #%d（%s）的 worker 结束", t.IntentID, t.Summary)
		if t.Text != "" {
			s += "，输出结论：" + model.TrimSummary(t.Text, 300)
		}
		return s
	case "finding":
		return fmt.Sprintf("- 意图 #%d 报告了一个发现：%s（节点 #%d）", t.IntentID, t.Summary, t.NodeID)
	case "goal":
		return "- 人新增了一个目标：" + t.Text + " —— 新的待达成目标，请据此补充方向（若尚无对应意图）"
	case "goal_edited":
		return "- 人修改了目标：" + t.Text + " —— 按新目标调整方向（原方向不适用请停派）"
	case "goal_deleted":
		return "- 人删除了目标：" + t.Text + " —— 不必再为它派意图"
	case "hint":
		return "- 人新增了一条战略提示：" + t.Text
	case "cancelled":
		return fmt.Sprintf("- 意图 #%d 已被用户删除：%s", t.IntentID, t.Summary)
	default:
		return "- 无具体变动信号"
	}
}

// Planner 是唯一的意图生产者：每轮新建会话，读态势、判目标、补方向。
type Planner struct {
	Store     *store.Store
	Providers llm.Factory
	Registry  *Registry
	DataDir   string
	MaxOutput int
	MaxTurns  int
	Logger    *slog.Logger
}

// PlanResult 是一轮规划的结果。
type PlanResult struct {
	Met       bool
	Intents   int
	ToolCalls int
	Summary   string
	Usage     llm.Usage
}

// Plan 跑一轮规划。
//
// hooks.killWork 由编排层注入：planner 的 kill_work 只应作用于本任务在跑的 work。
func (p *Planner) Plan(ctx context.Context, runID int64, triggers []Trigger, final bool, killWork func(int64) bool, emit Emitter) (*PlanResult, error) {
	if p.Store == nil || p.Providers == nil || p.Registry == nil {
		return nil, fmt.Errorf("planner 未装配")
	}
	if emit == nil {
		emit = func(context.Context, *model.Activity) {}
	}
	run, err := p.Store.Run(ctx, runID)
	if err != nil {
		return nil, err
	}
	scopes, err := p.Store.Scopes(ctx, runID)
	if err != nil {
		return nil, err
	}
	constraints, err := p.Store.Constraints(ctx, runID)
	if err != nil {
		return nil, err
	}
	ov, err := BuildOverview(ctx, p.Store, runID, true)
	if err != nil {
		return nil, err
	}

	scopeSet := domain.NewScopeSet(scopes)
	rc := &Context{
		Store: p.Store, RunID: runID,
		Scopes: scopeSet, Constraints: constraints,
		DataDir: p.DataDir, MaxOutput: p.MaxOutput,
		Final: final, KillWork: killWork,
	}

	maxTurns := p.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 6
	}
	sessionKey := fmt.Sprintf("run%d-planner", runID)
	provider := p.Providers.For(sessionKey)

	msgs := []llm.Message{{
		Role: llm.RoleUser,
		Text: BuildPlannerUserMessage(run, triggers, ov, final),
	}}
	system := BuildPlannerSystem(run, constraints, scopeSet.Describe(), final)
	specs := p.Registry.Specs()

	out := &PlanResult{}
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
				RunID: runID, Worker: "planner", Kind: model.ActThinking,
				Summary: model.TrimSummary(resp.Thinking, 200),
				Detail:  clampOutput(rc, "thinking", resp.Thinking),
			})
		}
		if resp.Text != "" {
			lastText = resp.Text
			emit(ctx, &model.Activity{
				RunID: runID, Worker: "planner", Kind: model.ActText,
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
				RunID: runID, Worker: "planner", Kind: model.ActToolUse,
				Tool: tc.Name, ToolUseID: tc.ID,
				Summary: summary, Detail: clampOutput(rc, "tool_input", detail),
				Metadata: map[string]any{"tool_call_id": tc.ID},
			})

			res := p.Registry.Call(ctx, rc, tc.Name, tc.Input)
			if tc.Name == "add_intent" && !res.IsError {
				out.Intents += countIntents(res.Text)
			}
			emit(ctx, &model.Activity{
				RunID: runID, Worker: "planner", Kind: model.ActToolResult,
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

	goals, err := p.Store.Goals(ctx, runID)
	if err != nil {
		return out, err
	}
	open := 0
	for _, g := range goals {
		if g.State == model.StateOpen {
			open++
		}
	}
	// 只有"本来就有目标、且全部达成"才算完成；没有目标不等于完成（那是 goalless 分支的事）。
	out.Met = len(goals) > 0 && open == 0
	out.Summary = model.TrimSummary(lastText, 800)

	emit(context.WithoutCancel(ctx), &model.Activity{
		RunID: runID, Worker: "planner", Kind: model.ActResult,
		Summary:     fmt.Sprintf("本轮规划结束：产出 %d 条意图，%d/%d 目标未达成", out.Intents, open, len(goals)),
		Detail:      out.Summary,
		InputTokens: out.Usage.Input, OutputTokens: out.Usage.Output,
		CacheReadTokens: out.Usage.CacheRead, CacheWriteTokens: out.Usage.CacheWrite,
		Metadata: map[string]any{
			"intents": out.Intents, "tool_calls": out.ToolCalls,
			"goals_open": open, "goals_total": len(goals), "met": out.Met,
		},
	})
	return out, nil
}

// countIntents 从 add_intent 的返回里数成功条数。
func countIntents(text string) int {
	var v struct {
		Results []struct {
			ID int64 `json:"id"`
		} `json:"results"`
	}
	if json.Unmarshal([]byte(text), &v) != nil {
		return 0
	}
	n := 0
	for _, r := range v.Results {
		if r.ID > 0 {
			n++
		}
	}
	return n
}

var _ = strings.TrimSpace
