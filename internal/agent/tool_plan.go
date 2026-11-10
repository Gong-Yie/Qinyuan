package agent

import (
	"context"
	"encoding/json"
	"strings"

	"qinyuan/internal/store"
)

// NewAddPlanStepTool 让 planner 把一次战役拆成带依赖与"可判定期望证据"的步骤。
//
// P1-1 的核心约束：expected_evidence 必须**可判定**（能被 fact 的 summary 匹配），
// 所以这里强制它非空 —— 写"完成注入"这种模糊说法会被拒。
func NewAddPlanStepTool() *Tool {
	return &Tool{
		Name: "add_plan_step",
		Description: `往计划图里加一个可执行步骤（带依赖与期望证据）。
expected_evidence 必须是一句【可被事实摘要匹配】的判据（例："拿到数据库连接串"），
不要写"完成注入"这种无法判定的说法。
depends_on 填前置步骤 id（可空）；有依赖的步骤在依赖完成前是 todo，不会出现在 ready_steps 里。
步骤的 ready/done 由代码判定：事实命中 expected_evidence 会自动完成该步，你也可以用 mark_step_done 显式判定。`,
		Schema: toolSpecJSON(`{
  "type": "object",
  "properties": {
    "text": {"type": "string", "description": "这一步要做什么"},
    "expected_evidence": {"type": "string", "description": "可被事实摘要匹配的判据"},
    "depends_on": {"type": "array", "items": {"type": "integer"}},
    "priority": {"type": "integer", "minimum": 0, "maximum": 10}
  },
  "required": ["text", "expected_evidence"]
}`),
		Handle: func(ctx context.Context, rc *Context, raw json.RawMessage) (Result, error) {
			if rc.Final {
				return Errorf("收尾轮不再扩展计划。"), nil
			}
			var in struct {
				Text      string  `json:"text"`
				Expected  string  `json:"expected_evidence"`
				DependsOn []int64 `json:"depends_on"`
				Priority  int     `json:"priority"`
			}
			if err := json.Unmarshal(raw, &in); err != nil {
				return Errorf("参数解析失败：%v", err), nil
			}
			if strings.TrimSpace(in.Expected) == "" {
				return Errorf("expected_evidence 必填：它必须是一句能被事实匹配的判据，否则 ready/done 无法由代码判定"), nil
			}
			stepID, err := rc.Store.AddStep(ctx, store.AddStepInput{
				RunID: rc.RunID, Text: in.Text, ExpectedEvidence: in.Expected,
				DependsOn: in.DependsOn, Priority: in.Priority,
			})
			if err != nil {
				return Errorf("%v", err), nil
			}
			if _, err := rc.Store.RecomputeSteps(ctx, rc.RunID); err != nil {
				return Errorf("重算就绪步骤失败：%v", err), nil
			}
			pg, _ := rc.Store.PlanGraph(ctx, rc.RunID)
			ready := 0
			if pg != nil {
				for _, s := range pg.Steps {
					if s.State == "ready" {
						ready++
					}
				}
			}
			return JSONResult(map[string]any{
				"step_id": stepID, "plan_id": planIDOf(pg), "ready_steps": ready,
			}), nil
		},
	}
}

func planIDOf(pg *store.PlanGraph) int64 {
	if pg == nil {
		return 0
	}
	return pg.PlanID
}

// NewMarkStepDoneTool 让 planner 在规则匹配不上时显式判定一步达成。
func NewMarkStepDoneTool() *Tool {
	return &Tool{
		Name: "mark_step_done",
		Description: `显式判定某个计划步骤已完成（通常不需要：事实命中 expected_evidence 会自动完成）。
fact_id 是证明它的那条事实/发现；没有证据就不要标记。`,
		Schema: toolSpecJSON(`{
  "type": "object",
  "properties": {
    "step_id": {"type": "integer"},
    "fact_id": {"type": "integer"},
    "reason": {"type": "string"}
  },
  "required": ["step_id"]
}`),
		Handle: func(ctx context.Context, rc *Context, raw json.RawMessage) (Result, error) {
			var in struct {
				StepID int64  `json:"step_id"`
				FactID int64  `json:"fact_id"`
				Reason string `json:"reason"`
			}
			if err := json.Unmarshal(raw, &in); err != nil {
				return Errorf("参数解析失败：%v", err), nil
			}
			ok, err := rc.Store.MarkStepDone(ctx, rc.RunID, in.StepID, in.FactID)
			if err != nil {
				return Errorf("%v", err), nil
			}
			return JSONResult(map[string]any{"step_id": in.StepID, "done": ok}), nil
		},
	}
}

// NewListPlanTool 让 planner 随时重读计划（overview 里已有摘要，这个给细节用）。
func NewListPlanTool() *Tool {
	return &Tool{
		Name:        "list_plan_steps",
		Description: `列出计划图全部步骤（含依赖、状态、期望证据）。overview 里只有 ready 摘要；需要全貌时用这个。`,
		ReadOnly:    true,
		Schema:      toolSpecJSON(`{"type":"object","properties":{}}`),
		Handle: func(ctx context.Context, rc *Context, _ json.RawMessage) (Result, error) {
			pg, err := rc.Store.PlanGraph(ctx, rc.RunID)
			if err != nil {
				return Errorf("读计划失败：%v", err), nil
			}
			return JSONResult(pg), nil
		},
	}
}
