package engine

import (
	"testing"
	"time"

	"qinyuan/internal/agent"
	"qinyuan/internal/llm"
)

// M21：全局模式要真的接到 Gate 上；off 时不应该创建 gate。
func TestEngineApprovalModeWiring(t *testing.T) {
	st := newTestStore(t)
	for _, mode := range []string{"manual", "auto", "off"} {
		e := New(st, llm.NewFakeFactory(), agent.NewWorkerRegistry(), agent.NewPlannerRegistry(), Config{
			ApprovalMode:    mode,
			ApprovalTimeout: time.Second,
		}, nil)
		if mode == "off" {
			if e.gate != nil {
				t.Fatalf("off 模式不该创建 gate")
			}
			continue
		}
		if e.gate == nil || e.gate.DefaultMode != mode {
			t.Fatalf("mode=%s 的 gate 没接上：%+v", mode, e.gate)
		}
	}
}
