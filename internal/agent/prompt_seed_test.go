package agent

import (
	"strings"
	"testing"

	"qinyuan/internal/model"
)

// P0：入口 URL 必须进 planner 与 worker 的 prompt —— 否则执行者只能猜端口。
func TestPromptsRenderSeedURL(t *testing.T) {
	const seed = "http://127.0.0.1:8080/login"
	run := &model.Run{ID: 1, Goal: "确认服务指纹", Seeds: []string{seed}}

	worker := BuildWorkerSystem(WorkerPromptInput{Run: run})
	if !strings.Contains(worker, seed) {
		t.Fatalf("worker system prompt 里没有入口 URL：\n%s", worker)
	}

	planner := BuildPlannerSystem(run, nil, "ip 127.0.0.1", false)
	if !strings.Contains(planner, seed) {
		t.Fatalf("planner system prompt 里没有入口 URL：\n%s", planner)
	}
}

// 没有 seeds 时不要多出一行空话。
func TestPromptsOmitSeedLineWithoutSeeds(t *testing.T) {
	run := &model.Run{ID: 1, Goal: "确认服务指纹"}
	if strings.Contains(BuildWorkerSystem(WorkerPromptInput{Run: run}), "起点 URL") {
		t.Fatal("没有 seeds 时不该出现起点 URL 行")
	}
	if strings.Contains(BuildPlannerSystem(run, nil, "", false), "起点 URL") {
		t.Fatal("没有 seeds 时不该出现起点 URL 行")
	}
}
