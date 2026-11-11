package agent

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"qinyuan/internal/sandbox"
)

// NewExecTool 是 worker 的"动手"能力：在沙箱里跑命令。
//
// 边界不靠 prompt：
//   - 容器内非 root、无 cap、根文件系统只读；
//   - 网络默认全拒，只有 network=target/callback 才按白名单放行；
//   - 命令写不了宿主文件（只挂 /job）、起不了守护进程（pids 限制）、跑不出时限。
//
// 它只给 worker，planner 拿不到（规划者不亲自干活，与 http_request 同一条纪律）。
func NewExecTool() *Tool {
	return &Tool{
		Name: "exec",
		Description: `在沙箱里执行一条 shell 命令（用于构造/验证 payload、跑脚本、反编译辅助）。
沙箱边界（由服务端强制，命令改不了）：
  - 非 root、无 sudo、无 docker socket；根文件系统只读，只有 /job 可写；
  - 网络默认【全拒】。network=target 放行本任务授权范围；network=callback 额外放行
    操作者显式登记的回连白名单（反弹 shell / JNDI 回连用）；
  - 单次默认 120 秒、1 核、1 GiB、256 进程；stdout/stderr 各截断 64 KiB。
用法要点：
  - 需要联网就先在命令里说明走哪个网（默认 none）；DNS 不开放，请直接用 IP 或让服务端解析；
  - 命令是一次性的（新容器），文件要留在 /job 才能跨调用看到（同一 run 的 /job 独立）；
  - 失败会返回 exit_code / stderr 原文，先读错误再改命令。`,
		Schema: toolSpecJSON(`{
  "type": "object",
  "properties": {
    "command": {"type": "string", "description": "要执行的 shell 命令（bash -s 读入）"},
    "network": {"type": "string", "enum": ["none", "target", "callback"], "description": "默认 none（全断网）；target=只放行授权范围；callback=再放行回连白名单"},
    "timeout_seconds": {"type": "integer", "description": "超时秒数，默认 120，范围 5..900"}
  },
  "required": ["command"]
}`),
		Handle: handleExec,
	}
}

func handleExec(ctx context.Context, rc *Context, raw json.RawMessage) (Result, error) {
	if rc.Sandbox == nil {
		return Errorf("沙箱未启用（服务端 QINYUAN_SANDBOX=off）。需要执行能力请让操作者开启，不要改用别的方式绕。"), nil
	}
	var in struct {
		Command        string `json:"command"`
		Network        string `json:"network"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return Errorf("参数解析失败：%v", err), nil
	}
	if strings.TrimSpace(in.Command) == "" {
		return Errorf("command 必填"), nil
	}
	network := strings.ToLower(strings.TrimSpace(in.Network))
	if network == "" {
		network = "none"
	}
	switch network {
	case "none", "target", "callback":
	default:
		return Errorf(`network 只支持 none / target / callback`), nil
	}
	if in.TimeoutSeconds == 0 {
		in.TimeoutSeconds = 120
	}
	if in.TimeoutSeconds < 5 || in.TimeoutSeconds > 900 {
		in.TimeoutSeconds = 120
	}

	var allows []sandbox.Allow
	if network != "none" && rc.Egress != nil {
		allows = rc.Egress(network)
	}

	res, err := rc.Sandbox.Run(ctx, sandbox.Request{
		RunID:    rc.RunID,
		IntentID: rc.IntentID,
		Command:  in.Command,
		Timeout:  time.Duration(in.TimeoutSeconds) * time.Second,
		Allows:   allows,
	})
	if err != nil {
		// 规则写不进去等"没跑"的情况：明确说没执行，别让模型以为命令跑了。
		return Errorf("沙箱执行失败（未执行或未完成）：%v", err), nil
	}

	payload := map[string]any{
		"exit_code":   res.ExitCode,
		"stdout":      res.Stdout,
		"stderr":      res.Stderr,
		"truncated":   res.Truncated,
		"killed":      res.Killed,
		"duration_ms": res.Duration.Milliseconds(),
		"job_dir":     res.JobDir,
		"network":     network,
		"untrusted":   true,
	}
	if res.Killed {
		payload["note"] = "命令被超时终止（exit 124）；调大 timeout_seconds 或改小任务"
	}
	return JSONResult(payload), nil
}
