package agent

import (
	"strings"
	"testing"

	"qinyuan/internal/model"
)

// P2/P3：prompt 与工具描述里必须写明"判据词条"与"confidence 两档"的边界 ——
// 这两条是规则命中率与事实可信度的上游，容易被后续改动悄悄删掉。
func TestPlannerPromptExplainsEvidenceFormat(t *testing.T) {
	sys := BuildPlannerSystem(&model.Run{ID: 1, Goal: "x"}, nil, "", false)
	for _, want := range []string{"expected_evidence", "判据词条", "Server 头含 nginx"} {
		if !strings.Contains(sys, want) {
			t.Fatalf("planner system prompt 缺少 %q", want)
		}
	}
}

func TestWorkerPromptDefinesConfidence(t *testing.T) {
	sys := BuildWorkerSystem(WorkerPromptInput{Run: &model.Run{ID: 1, Goal: "x"}})
	for _, want := range []string{"observed", "inferred", "缺失、差异、时序、排除法"} {
		if !strings.Contains(sys, want) {
			t.Fatalf("worker system prompt 缺少 %q", want)
		}
	}
}

func TestAddPlanStepToolExplainsEvidenceFormat(t *testing.T) {
	tool := NewAddPlanStepTool()
	for _, want := range []string{"可检索的判据词条", "Server 头含 nginx"} {
		if !strings.Contains(tool.Description, want) {
			t.Fatalf("add_plan_step 描述缺少 %q", want)
		}
	}
}

func TestRecordFactToolDefinesConfidence(t *testing.T) {
	tool := NewRecordFactTool()
	for _, want := range []string{"observed", "inferred", "缺失、差异、时序、排除法"} {
		if !strings.Contains(tool.Description, want) {
			t.Fatalf("record_fact 描述缺少 %q", want)
		}
	}
}
