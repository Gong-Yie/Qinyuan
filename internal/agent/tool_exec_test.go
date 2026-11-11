package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"qinyuan/internal/sandbox"
)

// exec 工具：沙箱未启用时必须明确报错（不能让模型以为命令跑了）。
func TestExecToolWithoutSandbox(t *testing.T) {
	rc := &Context{RunID: 1}
	res, err := NewExecTool().Handle(context.Background(), rc, json.RawMessage(`{"command":"id"}`))
	if err != nil {
		t.Fatalf("不该返回 Go error：%v", err)
	}
	if !res.IsError || !strings.Contains(res.Text, "沙箱未启用") {
		t.Fatalf("应报「沙箱未启用」：%s", res.Text)
	}
}

// exec 工具：正常路径 + 网络级别 + 超时参数，全部透传给 Runner。
func TestExecToolWithFakeRunner(t *testing.T) {
	f := &sandbox.Fake{Result: sandbox.Result{
		ExitCode: 0, Stdout: "uid=1000\n", Stderr: "", Duration: 12 * time.Millisecond, JobDir: "/job/1",
	}}
	rc := &Context{RunID: 42, IntentID: 7, Sandbox: f}
	res, err := NewExecTool().Handle(context.Background(), rc, json.RawMessage(
		`{"command":"id","network":"callback","timeout_seconds":30}`))
	if err != nil || res.IsError {
		t.Fatalf("执行失败：%+v %v", res, err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("结果不是 JSON：%v（%s）", err, res.Text)
	}
	if out["exit_code"].(float64) != 0 || out["stdout"] != "uid=1000\n" {
		t.Fatalf("结果字段不对：%+v", out)
	}
	if len(f.Reqs) != 1 {
		t.Fatalf("Runner 没被调用")
	}
	req := f.Reqs[0]
	if req.RunID != 42 || req.IntentID != 7 || req.Command != "id" {
		t.Fatalf("请求字段不对：%+v", req)
	}
	if req.Timeout != 30*time.Second {
		t.Fatalf("超时没透传：%v", req.Timeout)
	}
}

// 参数校验：空命令 / 非法 network / 超时越界都要是可读错误，且不调用 Runner。
func TestExecToolValidation(t *testing.T) {
	f := &sandbox.Fake{}
	rc := &Context{RunID: 1, Sandbox: f}
	for _, raw := range []string{
		`{"command":"   "}`,
		`{"command":"id","network":"internet"}`,
		`{}`,
	} {
		res, err := NewExecTool().Handle(context.Background(), rc, json.RawMessage(raw))
		if err != nil {
			t.Fatalf("%s 不该返回 Go error：%v", raw, err)
		}
		if !res.IsError {
			t.Fatalf("%s 应当报错：%s", raw, res.Text)
		}
	}
	if len(f.Reqs) != 0 {
		t.Fatalf("参数非法时不该调用 Runner：%+v", f.Reqs)
	}
	// 超时越界：夹到默认值，但照常执行。
	if _, err := NewExecTool().Handle(context.Background(), rc, json.RawMessage(
		`{"command":"id","timeout_seconds":99999}`)); err != nil {
		t.Fatal(err)
	}
	if f.Reqs[len(f.Reqs)-1].Timeout != 120*time.Second {
		t.Fatalf("越界超时应回落到 120s，实际 %v", f.Reqs[len(f.Reqs)-1].Timeout)
	}
}

// Runner 报错时：必须说"未执行/未完成"，不能伪装成执行结果。
func TestExecToolRunnerError(t *testing.T) {
	f := &sandbox.Fake{Err: errors.New("沙箱规则写入失败")}
	rc := &Context{RunID: 1, Sandbox: f}
	res, err := NewExecTool().Handle(context.Background(), rc, json.RawMessage(`{"command":"id"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "沙箱执行失败") {
		t.Fatalf("应报执行失败：%s", res.Text)
	}
}
