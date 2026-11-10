package server

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"qinyuan/internal/model"
	"qinyuan/internal/store"
	"qinyuan/internal/testkit"
)

// M10：新建任务后控制台要能直接"启动" —— can_start 的判定必须跟着任务状态走，
// 否则用户建完任务只会看到一个躺在 created 里、没有任何按钮的死任务。
func TestRunDTOCanStart(t *testing.T) {
	st := testkit.Store(t)
	srv := New(st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	run, err := st.CreateRun(context.Background(), store.CreateRunInput{
		Goal: "确认登录入口", WorkerCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	dto := runDTO(context.Background(), st, run)
	if dto["can_start"] != true {
		t.Fatalf("新建任务 can_start 应为 true：%+v", dto)
	}
	if dto["can_pause"] != false || dto["can_resume"] != false {
		t.Fatalf("created 状态不该能暂停/恢复：%+v", dto)
	}
	_ = srv

	// 进入 running 后：能暂停，不再能启动。
	if ok, err := st.CASRunTaskStatus(context.Background(), run.ID,
		[]string{model.TaskCreated, "queued"}, model.TaskRunning); err != nil || !ok {
		t.Fatalf("改状态失败：%v", err)
	}
	run2, err := st.Run(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	dto = runDTO(context.Background(), st, run2)
	if dto["can_start"] != false || dto["can_pause"] != true {
		t.Fatalf("running 状态应能暂停、不能再启动：%+v", dto)
	}
}
