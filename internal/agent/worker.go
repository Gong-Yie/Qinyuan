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
	"qinyuan/internal/store"
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
		Run:         run,
		Intent:      intent,
		Entities:    ents,
		Constraints: constraints,
		Scopes:      scopeSet.Describe(),
		RunDir:      runDir,
	})

	ov, err := BuildOverview(ctx, w.Store, runID)
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
	}

	maxTurns := w.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 12
	}
	maxDuration := w.MaxDuration
	if maxDuration <= 0 {
		maxDuration = 10 * time.Minute
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
		resp, err := provider.Complete(execCtx, &llm.Request{
			SessionKey: fmt.Sprintf("run%d-intent%d", runID, intentID),
			System:     system,
			Messages:   msgs,
			Tools:      specs,
		})
		if err != nil {
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

			res := w.Registry.Call(execCtx, rc, tc.Name, tc.Input)
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
