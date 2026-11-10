package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"qinyuan/internal/domain"
	"qinyuan/internal/llm"
	"qinyuan/internal/model"
	"qinyuan/internal/store"
	"qinyuan/internal/trace"
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

	// FullSnapshotEvery：每 K 轮强制给一次全量态势（0 = 用默认 20）。
	// 只靠增量会让模型慢慢忘记更早的事实；全量快照是"记忆校准"。
	FullSnapshotEvery int
	// DeltaLimit：单轮增量最多多少条变化（0 = 默认 200），超过降级为全量。
	DeltaLimit int

	// KillWork / SteerWork：planner 对"在跑的 work"的两只手。
	// 它们只能作用于本任务，由编排层注入（planner 不 import engine）。
	KillWork  func(intentID int64) bool
	SteerWork func(intentID int64, message string) bool

	deltaMu  sync.Mutex
	lastSeen map[int64]int64 // runID -> 已经给模型看过的版本
}

// PlanOptions 是"这一轮怎么给态势"的开关。
type PlanOptions struct {
	// Final=true：收尾轮（只判定目标、不派意图），强制全量态势。
	Final bool
	// ForceFull=true：本轮必须给全量（重试、重启、显式要求）。
	ForceFull bool
}

// PlanResult 是一轮规划的结果。
type PlanResult struct {
	Met       bool
	Intents   int
	ToolCalls int
	Summary   string
	Usage     llm.Usage

	// Situation 记录本轮给的是全量还是增量（full | delta），用于验收与排障。
	Situation   string
	Changes     int
	PromptBytes int
}

// Plan 跑一轮规划。
//
// hooks.killWork 由编排层注入：planner 的 kill_work 只应作用于本任务在跑的 work。
func (p *Planner) Plan(ctx context.Context, runID int64, triggers []Trigger, final bool, killWork func(int64) bool, emit Emitter) (*PlanResult, error) {
	return p.PlanRound(ctx, runID, triggers, PlanOptions{Final: final}, killWork, emit)
}

// PlanRound 是 Plan 的完整版：额外决定"这一轮给全量还是增量"。
func (p *Planner) PlanRound(ctx context.Context, runID int64, triggers []Trigger, opts PlanOptions, killWork func(int64) bool, emit Emitter) (*PlanResult, error) {
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

	// ── 决定本轮态势的形态：全量 or 增量（P1-2）──
	//
	// 首轮必须全量（模型还没有基线）；收尾轮必须全量（要判目标，不能靠增量猜）；
	// 每 K 轮校准一次；变化太多（> DeltaLimit）降级全量。
	var ov *Overview
	var delta *DeltaOverview
	situation := "full"
	since, hasSince := p.lastSeenVersion(runID)
	full := opts.ForceFull || opts.Final || !hasSince || p.fullSnapshotDue(run.RoundNo)
	if !full {
		limit := p.DeltaLimit
		if limit <= 0 {
			limit = 200
		}
		d, truncated, derr := BuildDeltaOverview(ctx, p.Store, runID, run, since, limit)
		switch {
		case derr != nil:
			// 读增量失败不该让整轮规划失败：退回全量。
			if p.Logger != nil {
				p.Logger.Warn("[planner] 读增量态势失败，退回全量", "run", runID, "err", derr)
			}
		case truncated:
			emit(ctx, &model.Activity{
				RunID: runID, Worker: "planner", Kind: model.ActSystem,
				Summary: fmt.Sprintf("自版本 %d 以来变化过多（%d 条），本轮降级为全量态势", since, d.ChangesTotal),
			})
		default:
			delta = d
			situation = "delta"
		}
	}
	if delta == nil {
		ov, err = BuildOverview(ctx, p.Store, runID, true)
		if err != nil {
			return nil, err
		}
	}

	scopeSet := domain.NewScopeSet(scopes)
	forceNext := false
	kill := killWork
	if kill == nil {
		kill = p.KillWork
	}
	rc := &Context{
		Store: p.Store, RunID: runID,
		Scopes: scopeSet, Constraints: constraints,
		DataDir: p.DataDir, MaxOutput: p.MaxOutput,
		Final: opts.Final, KillWork: kill, SteerWork: p.SteerWork,
		RefreshRequested: &forceNext,
	}

	maxTurns := p.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 6
	}
	sessionKey := fmt.Sprintf("run%d-planner", runID)
	provider := p.Providers.For(sessionKey)

	userMsg := BuildPlannerUserMessage(run, triggers, ov, delta, opts.Final)
	msgs := []llm.Message{{Role: llm.RoleUser, Text: userMsg}}
	system := BuildPlannerSystem(run, constraints, scopeSet.Describe(), opts.Final)
	specs := p.Registry.Specs()

	out := &PlanResult{Situation: situation, PromptBytes: len(userMsg) + len(system)}
	if delta != nil {
		out.Changes = len(delta.Events)
	}
	var lastText string

	for turn := 0; turn < maxTurns; turn++ {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		callCtx, csp := trace.Start(ctx, "llm.call", map[string]any{
			"role": "planner", "turn": turn, "session": sessionKey, "messages": len(msgs),
		})
		resp, err := provider.Complete(callCtx, &llm.Request{
			SessionKey: sessionKey, System: system, Messages: msgs, Tools: specs,
		})
		if err != nil {
			csp.Set("error", err.Error())
			csp.End()
			return out, err
		}
		csp.Set("input_tokens", resp.Usage.Input)
		csp.Set("output_tokens", resp.Usage.Output)
		csp.Set("cache_read_tokens", resp.Usage.CacheRead)
		csp.Set("tool_calls", len(resp.ToolCalls))
		csp.End()
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

			toolCtx, tsp := trace.Start(ctx, "tool.call", map[string]any{
				"tool": tc.Name, "tool_use_id": tc.ID, "read_only": p.Registry.IsReadOnly(tc.Name),
				"role": "planner",
			})
			res := p.Registry.Call(toolCtx, rc, tc.Name, tc.Input)
			tsp.Set("error", res.IsError)
			tsp.Set("output_bytes", len(res.Text))
			tsp.End()
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

	// 记录"模型已经看到哪个版本了"：
	//   - 增量：用本次窗口的上界；
	//   - 全量：用本轮开始时读到的版本（期间的新写入会在下一轮作为增量补上，
	//     宁可多补一次，也不能漏）。
	//   - 模型显式要求刷新：清掉基线，下一轮重新给全量。
	if forceNext {
		p.forgetDelta(runID)
	} else if delta != nil {
		p.setLastSeen(runID, delta.ToVersion)
	} else {
		p.setLastSeen(runID, run.Version)
	}

	emit(context.WithoutCancel(ctx), &model.Activity{
		RunID: runID, Worker: "planner", Kind: model.ActResult,
		Summary:     fmt.Sprintf("本轮规划结束：产出 %d 条意图，%d/%d 目标未达成", out.Intents, open, len(goals)),
		Detail:      out.Summary,
		InputTokens: out.Usage.Input, OutputTokens: out.Usage.Output,
		CacheReadTokens: out.Usage.CacheRead, CacheWriteTokens: out.Usage.CacheWrite,
		Metadata: map[string]any{
			"intents": out.Intents, "tool_calls": out.ToolCalls,
			"goals_open": open, "goals_total": len(goals), "met": out.Met,
			"situation": out.Situation, "changes": out.Changes, "prompt_bytes": out.PromptBytes,
		},
	})
	return out, nil
}

// ---------- 差分态势的基线（进程内）----------
//
// 基线只在内存里：它是"这一轮模型看过什么"的缓存，丢了最多让下一轮多给一次
// 全量（安全方向），所以不需要落库 —— 与"状态全在 DB"的主原则不冲突。

func (p *Planner) lastSeenVersion(runID int64) (int64, bool) {
	p.deltaMu.Lock()
	defer p.deltaMu.Unlock()
	v, ok := p.lastSeen[runID]
	return v, ok
}

func (p *Planner) setLastSeen(runID, version int64) {
	p.deltaMu.Lock()
	defer p.deltaMu.Unlock()
	if p.lastSeen == nil {
		p.lastSeen = map[int64]int64{}
	}
	p.lastSeen[runID] = version
}

func (p *Planner) forgetDelta(runID int64) {
	p.deltaMu.Lock()
	defer p.deltaMu.Unlock()
	delete(p.lastSeen, runID)
}

// fullSnapshotDue 判断本轮是否该做一次全量校准（首轮 + 每 K 轮）。
func (p *Planner) fullSnapshotDue(round int64) bool {
	every := p.FullSnapshotEvery
	if every <= 0 {
		every = 20
	}
	if round <= 1 {
		return true
	}
	return (round-1)%int64(every) == 0
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
