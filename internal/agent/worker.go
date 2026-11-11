package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"qinyuan/internal/domain"
	"qinyuan/internal/llm"
	"qinyuan/internal/model"
	"qinyuan/internal/sandbox"
	"qinyuan/internal/store"
	"qinyuan/internal/trace"
)

// Outcome 一次执行的结论。engine 据此决定意图落哪个终态。
type Outcome struct {
	Reason   string
	Err      error
	Turns    int
	Facts    int
	Findings int
	Assets   int
	Usage    llm.Usage
	Summary  string
	// Settlement=true 表示这次执行跑过收尾轮（撞预算后把结果写回）。
	Settlement bool
	// Compacted 记录执行期间触发过几次上下文压缩（0 = 没触发）。
	Compacted int
	// TTFTMs 首次拿到内容的时间（毫秒）；0 = 没测到（非流式 provider）。
	TTFTMs int64
}

// Reason 取值：撞上限 ≠ 完成，必须分开。
const (
	ReasonDone     = "done"
	ReasonMaxTurns = "max_turns"
	ReasonTimeout  = "timeout"
	ReasonCanceled = "canceled"
	ReasonError    = "error"
)

// Worker 执行一条意图：组装 prompt → 模型循环 → 工具调用 → 写回 → 停。
type Worker struct {
	Store         *store.Store
	Providers     llm.Factory
	Registry      *Registry
	DataDir       string
	AllowLoopback bool
	MaxOutput     int
	MaxTurns      int
	MaxDuration   time.Duration
	Logger        *slog.Logger

	// Settlement 撞预算时的收尾轮配置（零值 = 默认 3 轮、隐藏 http_request）。
	Settlement SettlementConfig
	// Compaction 上下文压缩配置（WindowTokens=0 表示关闭）。
	Compaction CompactionConfig
	// Steer 由编排层注入：每次工具调用前问一次"有没有实时纠偏"。
	// 返回非空消息时，**本次工具调用不执行**，把消息作为工具结果交给模型。
	Steer func() (string, bool)
	// Gate 是审批门（规则 → 人工）；nil = 不启用。
	Gate *Gate
	// Sandbox 是执行沙箱（nil = 未启用；此时 exec 工具会明确报错）。
	Sandbox sandbox.Runner
	// Egress 由编排层注入：按 network 级别返回放行项（target / callback）。
	Egress func(ctx context.Context, runID, intentID int64, network string) []sandbox.Allow
}

// Execute 跑一条已领取（state=running）的意图。
func (w *Worker) Execute(ctx context.Context, runID, intentID int64, owner string, emit Emitter) (*Outcome, error) {
	if w.Store == nil || w.Providers == nil || w.Registry == nil {
		return nil, errors.New("worker 未装配（store/provider/registry 缺失）")
	}
	if emit == nil {
		emit = func(context.Context, *model.Activity) {}
	}
	run, err := w.Store.Run(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("读 run 失败：%w", err)
	}
	intent, err := w.Store.Intent(ctx, runID, intentID)
	if err != nil {
		return nil, fmt.Errorf("读意图失败：%w", err)
	}

	scopes, err := w.Store.Scopes(ctx, runID)
	if err != nil {
		return nil, err
	}
	constraints, err := w.Store.Constraints(ctx, runID)
	if err != nil {
		return nil, err
	}
	scopeSet := domain.NewScopeSet(scopes)
	ents, err := w.Store.EntitiesByNode(ctx, intentID)
	if err != nil {
		return nil, err
	}

	runDir := fmt.Sprintf("%s/runs/%d", w.DataDir, runID)
	system := BuildWorkerSystem(WorkerPromptInput{
		Run:            run,
		Intent:         intent,
		Entities:       ents,
		Constraints:    constraints,
		Scopes:         scopeSet.Describe(),
		RunDir:         runDir,
		TrafficEnabled: true,
	})

	ov, err := BuildOverview(ctx, w.Store, runID, false)
	if err != nil {
		return nil, err
	}

	rc := &Context{
		Store:         w.Store,
		RunID:         runID,
		IntentID:      intentID,
		Scopes:        scopeSet,
		Constraints:   constraints,
		DataDir:       w.DataDir,
		AllowLoopback: w.AllowLoopback,
		MaxOutput:     w.MaxOutput,
		Sandbox:       w.Sandbox,
	}
	if w.Egress != nil {
		rc.Egress = func(network string) []sandbox.Allow { return w.Egress(ctx, runID, intentID, network) }
	}

	maxTurns, maxDuration, budgetApplied := resolveBudget(w.MaxTurns, w.MaxDuration, intent.Payload)
	if budgetApplied {
		emit(ctx, &model.Activity{
			RunID: runID, NodeID: &intentID, Worker: owner, Kind: model.ActSystem,
			Summary:  fmt.Sprintf("本意图预算（上限）：%d 回合 / %s（planner 在 add_intent 里指定）", maxTurns, maxDuration),
			Metadata: map[string]any{"budgeted": true, "max_turns": maxTurns, "max_seconds": int(maxDuration.Seconds())},
		})
	}
	execCtx, cancel := context.WithTimeout(ctx, maxDuration)
	defer cancel()

	provider := w.Providers.For(fmt.Sprintf("run%d-intent%d", runID, intentID))
	msgs := []llm.Message{{Role: llm.RoleUser, Text: BuildWorkerUserMessage(ov)}}
	specs := w.Registry.Specs()

	out := &Outcome{Reason: ReasonDone}
	var usage llm.Usage
	var lastText string

	for turn := 1; turn <= maxTurns; turn++ {
		if execCtx.Err() != nil {
			out.Reason = reasonForCtx(ctx, execCtx)
			break
		}
		out.Turns = turn

		// 上下文预算：超阈值就先压（micro-compact → 摘要）。压不动也继续跑，
		// 由模型自己撞 413 —— 那会比"直接失败"更容易在日志里定位。
		if w.Compaction.enabled() {
			if compacted, changed, cerr := compactMessages(execCtx, provider, w.Compaction, system, msgs); changed {
				msgs = compacted
				out.Compacted++
				emit(execCtx, &model.Activity{
					RunID: runID, NodeID: &intentID, Worker: owner, Kind: model.ActSystem,
					Summary: fmt.Sprintf("上下文压缩：%d 条消息 → 约 %d token（第 %d 次）",
						len(msgs), estimateMessages(system, msgs), out.Compacted),
					Metadata: map[string]any{"compacted": out.Compacted, "estimated_tokens": estimateMessages(system, msgs)},
				})
			} else if cerr != nil {
				emit(execCtx, &model.Activity{
					RunID: runID, NodeID: &intentID, Worker: owner, Kind: model.ActSystem,
					Summary: "上下文压缩失败（本轮继续，可能触发 413）",
					Detail:  cerr.Error(),
				})
			}
		}

		req := &llm.Request{
			SessionKey: fmt.Sprintf("run%d-intent%d", runID, intentID),
			System:     system,
			Messages:   msgs,
			Tools:      specs,
		}
		callCtx, csp := trace.Start(execCtx, "llm.call", map[string]any{
			"turn": turn, "session": req.SessionKey, "messages": len(msgs),
		})
		resp, err := completeTurn(callCtx, provider, req, func(d time.Duration) {
			if out.TTFTMs != 0 {
				return
			}
			out.TTFTMs = d.Milliseconds()
			csp.Set("ttft_ms", out.TTFTMs)
			emit(execCtx, &model.Activity{
				RunID: runID, NodeID: &intentID, Worker: owner, Kind: model.ActSystem,
				Summary:  fmt.Sprintf("首字延迟 %dms（流式）", out.TTFTMs),
				Metadata: map[string]any{"ttft_ms": out.TTFTMs, "turn": turn},
			})
		})
		if err != nil {
			csp.Set("error", err.Error())
		}
		if resp != nil {
			csp.Set("input_tokens", resp.Usage.Input)
			csp.Set("output_tokens", resp.Usage.Output)
			csp.Set("cache_read_tokens", resp.Usage.CacheRead)
			csp.Set("tool_calls", len(resp.ToolCalls))
		}
		csp.End()
		if err != nil {
			// 已提交后断流：保留已经拿到的部分结果（不再重放）。
			if resp != nil && resp.Text != "" {
				lastText = resp.Text
			}
			if errors.Is(err, llm.ErrStreamInterrupted) {
				emit(context.WithoutCancel(ctx), &model.Activity{
					RunID: runID, NodeID: &intentID, Worker: owner, Kind: model.ActSystem,
					Summary: "流式输出中途断开（已提交，不重放；保留已收到的部分结果）",
					Detail:  err.Error(),
				})
			}
			// 每一次模型失败都留一条可读记录：否则 planner 通过 get_worker_output
			// 只能看到一句"执行结束：error"、output 为空，就不知道是"没探成"还是"做不了"，
			// 于是把同一个方向反复重派（run #41 就这样重派了 5 次白盒）。
			// 暂停/取消导致的 ctx 错误不算"模型失败"，别误导 planner。
			if execCtx.Err() == nil {
				emit(context.WithoutCancel(ctx), &model.Activity{
					RunID: runID, NodeID: &intentID, Worker: owner, Kind: model.ActSystem,
					Summary:  fmt.Sprintf("模型调用失败（第 %d 回合）：%s", turn, model.TrimSummary(oneLine(err.Error()), 160)),
					Detail:   err.Error(),
					Metadata: map[string]any{"llm_error": true, "turn": turn},
				})
			}
			if execCtx.Err() != nil {
				out.Reason = reasonForCtx(ctx, execCtx)
			} else {
				out.Reason = ReasonError
				out.Err = err
			}
			break
		}
		usage.Input += resp.Usage.Input
		usage.Output += resp.Usage.Output
		usage.CacheRead += resp.Usage.CacheRead
		usage.CacheWrite += resp.Usage.CacheWrite

		if resp.Thinking != "" {
			emit(execCtx, &model.Activity{
				RunID: runID, NodeID: &intentID, Worker: owner, Kind: model.ActThinking,
				Summary: model.TrimSummary(resp.Thinking, 200),
				Detail:  clampOutput(rc, "thinking", resp.Thinking),
			})
		}
		if resp.Text != "" {
			lastText = resp.Text
			emit(execCtx, &model.Activity{
				RunID: runID, NodeID: &intentID, Worker: owner, Kind: model.ActText,
				Summary: model.TrimSummary(resp.Text, 200),
				Detail:  clampOutput(rc, "text", resp.Text),
			})
		}

		if len(resp.ToolCalls) == 0 {
			out.Reason = ReasonDone
			break
		}

		msgs = append(msgs, llm.Message{
			Role: llm.RoleAssistant, Text: resp.Text, ToolCalls: resp.ToolCalls,
		})

		for _, tc := range resp.ToolCalls {
			summary, detail := describeToolCall(tc)
			emit(execCtx, &model.Activity{
				RunID: runID, NodeID: &intentID, Worker: owner, Kind: model.ActToolUse,
				Tool: tc.Name, ToolUseID: tc.ID,
				Summary: summary, Detail: clampOutput(rc, "tool_input", detail),
				Metadata: map[string]any{"tool_call_id": tc.ID},
			})

			// 实时纠偏（steer_work）：在工具真正执行**之前**问一次。
			// 有消息就拦下这次调用，把消息当工具结果交回模型 —— 不打断、不丢进展。
			if w.Steer != nil {
				if msg, ok := w.Steer(); ok {
					res := Errorf("【规划者实时纠偏】%s\n（本次工具调用 %s 未执行，请据此调整下一步；若与你原本打算冲突，以此为准。）",
						msg, tc.Name)
					emit(execCtx, &model.Activity{
						RunID: runID, NodeID: &intentID, Worker: owner, Kind: model.ActSystem,
						Summary:  "收到规划者纠偏，已拦下本次工具调用：" + tc.Name,
						Detail:   msg,
						Metadata: map[string]any{"tool_call_id": tc.ID, "steer": true},
					})
					emit(execCtx, &model.Activity{
						RunID: runID, NodeID: &intentID, Worker: owner, Kind: model.ActToolResult,
						Tool: tc.Name, ToolUseID: tc.ID, IsError: true,
						Summary:  model.TrimSummary(oneLine(res.Text), 200),
						Detail:   clampOutput(rc, "tool_output", res.Text),
						Metadata: map[string]any{"tool_call_id": tc.ID, "steer": true},
					})
					msgs = append(msgs, llm.Message{
						Role: llm.RoleTool, ToolCallID: tc.ID, Name: tc.Name,
						Text: res.Text, IsError: true,
					})
					continue
				}
			}

			// 审批门：在真正执行**之前**判定。被拦下时把拒绝文案当工具结果交回模型，
			// 让模型自己改道，而不是让整条意图 failed。
			if w.Gate != nil {
				allowed, refuse := w.Gate.Check(execCtx, runID, intentID, tc.Name, tc.ID, tc.Input,
					func(p *store.PendingApproval) {
						emit(execCtx, &model.Activity{
							RunID: runID, NodeID: &intentID, Worker: owner, Kind: model.ActSystem,
							Summary:  fmt.Sprintf("等待人工审批：%s（审批 #%d）", tc.Name, p.ID),
							Detail:   p.Reason + "\n参数：" + string(tc.Input),
							Metadata: map[string]any{"approval_id": p.ID, "tool_call_id": tc.ID},
						})
					})
				if !allowed {
					res := Errorf("%s", refuse)
					emit(execCtx, &model.Activity{
						RunID: runID, NodeID: &intentID, Worker: owner, Kind: model.ActToolResult,
						Tool: tc.Name, ToolUseID: tc.ID, IsError: true,
						Summary:  model.TrimSummary(oneLine(res.Text), 200),
						Detail:   clampOutput(rc, "tool_output", res.Text),
						Metadata: map[string]any{"tool_call_id": tc.ID, "blocked_by": "intercept"},
					})
					msgs = append(msgs, llm.Message{
						Role: llm.RoleTool, ToolCallID: tc.ID, Name: tc.Name,
						Text: res.Text, IsError: true,
					})
					continue
				}
			}

			rc.ToolUseID = tc.ID
			toolCtx, tsp := trace.Start(execCtx, "tool.call", map[string]any{
				"tool": tc.Name, "tool_use_id": tc.ID, "read_only": w.Registry.IsReadOnly(tc.Name),
			})
			res := w.Registry.Call(toolCtx, rc, tc.Name, tc.Input)
			tsp.Set("error", res.IsError)
			tsp.Set("output_bytes", len(res.Text))
			tsp.End()
			rc.ToolUseID = ""
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
			emit(execCtx, &model.Activity{
				RunID: runID, NodeID: &intentID, Worker: owner, Kind: model.ActToolResult,
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

		if turn == maxTurns {
			out.Reason = ReasonMaxTurns
		}
	}

	// 撞预算 → 收尾轮：不再探测，把已经拿到的写回去。
	// 只有"被掐断"（撞步数/撞时长）才收尾；正常跑完、报错、被取消都不做。
	if out.Reason == ReasonMaxTurns || out.Reason == ReasonTimeout {
		w.runSettlement(ctx, runID, intentID, owner, rc, emit, msgs, system, out)
	}

	out.Usage = usage
	out.Summary = model.TrimSummary(lastText, 800)

	// 终态 result 行是 token 的唯一权威来源（只认这一行）。
	emit(context.WithoutCancel(ctx), &model.Activity{
		RunID: runID, NodeID: &intentID, Worker: owner, Kind: model.ActResult,
		Summary:     "执行结束：" + out.Reason,
		Detail:      clampOutput(rc, "result", out.Summary),
		InputTokens: usage.Input, OutputTokens: usage.Output,
		CacheReadTokens: usage.CacheRead, CacheWriteTokens: usage.CacheWrite,
		Metadata: map[string]any{
			"reason": out.Reason, "turns": out.Turns,
			"facts": out.Facts, "findings": out.Findings, "assets": out.Assets,
			"settlement": out.Settlement, "compacted": out.Compacted,
			"ttft_ms": out.TTFTMs,
		},
	})
	return out, nil
}

func reasonForCtx(parent, execCtx context.Context) string {
	if parent.Err() != nil {
		return ReasonCanceled
	}
	if errors.Is(execCtx.Err(), context.DeadlineExceeded) {
		return ReasonTimeout
	}
	return ReasonCanceled
}

// describeToolCall 给前端一张卡片的一行标题 + 完整入参。
func describeToolCall(tc llm.ToolCall) (summary, detail string) {
	var in struct {
		URL    string `json:"url"`
		Method string `json:"method"`
		Type   string `json:"type"`
	}
	_ = json.Unmarshal(tc.Input, &in)
	var b strings.Builder
	b.WriteString(tc.Name)
	if in.Method != "" || in.URL != "" {
		b.WriteString(" " + strings.ToUpper(in.Method) + " " + in.URL)
	}
	if in.Type != "" {
		b.WriteString(" " + in.Type)
	}
	summary = strings.TrimSpace(b.String())
	return model.TrimSummary(summary, 200), string(tc.Input)
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

// countResults / countOK 从工具返回的 JSON 里数成功条数（只用于统计，不参与正确性）。
func countResults(text string) int {
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

func countOK(text string) int {
	var v struct {
		OK int `json:"ok"`
	}
	if json.Unmarshal([]byte(text), &v) != nil {
		return 0
	}
	return v.OK
}
