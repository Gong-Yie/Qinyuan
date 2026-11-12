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
    操作者显式登记的回连白名单（沙箱→操作者出站；目标→沙箱的入站回连不在当前版本）；
  - 单次默认超时由服务端配置（默认 120 秒，可传 5..900）；1 核、1 GiB、256 进程；
    stdout/stderr 各截断到服务端上限（默认约 30KB），完整内容落盘并返回 stdout_path/stderr_path；
  - 域名由服务端解析后注入 /etc/hosts（DNS 默认关闭）：直接用域名即可；返回的 allows/hosts
    里也会有解析结果，拿不到时改用 IP。
用法要点：
  - 命令是一次性的（新容器），/job 只保证本次调用内可写；跨调用要持久化就写在命令里一次做完；
  - 失败会返回 exit_code / stderr 原文 / failure_class，先读错误再改命令。`,
		Schema: toolSpecJSON(`{
  "type": "object",
  "properties": {
    "command": {"type": "string", "description": "要执行的 shell 命令（bash -s 读入）"},
    "network": {"type": "string", "enum": ["none", "target", "callback"], "description": "默认 none（全断网）；target=只放行授权范围；callback=再放行回连白名单"},
    "timeout_seconds": {"type": "integer", "description": "超时秒数，默认按服务端配置（默认 120），范围 5..900"}
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
	// 0 = 用服务端默认（sandbox.Run 会填 cfg.Timeout）；越界也回落到服务端默认，
	// 避免“改配置不生效”的死配置。
	if in.TimeoutSeconds != 0 && (in.TimeoutSeconds < 5 || in.TimeoutSeconds > 900) {
		in.TimeoutSeconds = 0
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
		Network:  network,
	})
	if err != nil {
		// 规则写不进去等"没跑"的情况：明确说没执行，别让模型以为命令跑了。
		return Errorf("沙箱执行失败（未执行或未完成）：%v", err), nil
	}

	effective := network
	if network != "none" && len(allows) == 0 {
		// 范围为空 / 解析失败时，实际就是离线跑；别让模型以为在打目标。
		effective = "none"
	}
	allowList, hosts := execAllows(allows)
	failure := execFailureClass(res)

	payload := map[string]any{
		"exit_code":         res.ExitCode,
		"stdout":            res.Stdout,
		"stderr":            res.Stderr,
		"truncated":         res.Truncated,
		"spill_truncated":   res.SpillTruncated,
		"stdout_path":       res.StdoutPath,
		"stderr_path":       res.StderrPath,
		"stdout_bytes":      res.StdoutBytes,
		"stderr_bytes":      res.StderrBytes,
		"killed":            res.Killed,
		"duration_ms":       res.Duration.Milliseconds(),
		"job_dir":           res.JobDir,
		"network":           network,
		"effective_network": effective,
		"allows":            allowList,
		"hosts":             hosts,
		"failure_class":     failure,
		"untrusted":         true,
	}

	var notes []string
	if effective != network {
		notes = append(notes, "本任务没有可用放行项（范围为空或解析失败），本次实际按离线执行")
	}
	if res.Killed {
		notes = append(notes, "命令被超时/超限终止；调大 timeout_seconds 或拆小任务")
	}
	if failure == "oom" {
		notes = append(notes, "容器内存超限（exit 137）；拆小任务或减少并发")
	}
	if res.SpillTruncated {
		notes = append(notes, "完整输出超过落盘上限，文件本身也被截断")
	}
	if res.SpillError != "" {
		notes = append(notes, "完整输出落盘失败："+res.SpillError)
	}
	if len(notes) > 0 {
		payload["note"] = strings.Join(notes, "；")
	}
	return JSONResult(payload), nil
}

// execAllows 把放行项变成给模型看的审计摘要 + 域名→IP 映射。
func execAllows(allows []sandbox.Allow) ([]map[string]any, map[string][]string) {
	list := make([]map[string]any, 0, len(allows))
	hosts := map[string][]string{}
	for _, a := range allows {
		m := map[string]any{"dst": a.Dst.String()}
		if a.From != "" {
			m["from"] = a.From
		}
		if a.Host != "" {
			m["host"] = a.Host
		}
		if a.Proto != "" {
			m["proto"] = a.Proto
		}
		if a.Ports.Low > 0 {
			m["ports"] = a.Ports.String()
		}
		list = append(list, m)
		if a.Host != "" && a.Dst.IsValid() && a.Dst.Addr().Is4() {
			ip := a.Dst.Addr().String()
			hosts[a.Host] = appendUnique(hosts[a.Host], ip)
		}
	}
	return list, hosts
}

func appendUnique(in []string, v string) []string {
	for _, s := range in {
		if s == v {
			return in
		}
	}
	return append(in, v)
}

func execFailureClass(res sandbox.Result) string {
	if res.FailureClass != "" {
		return res.FailureClass
	}
	switch {
	case res.Killed:
		return "timeout"
	case res.ExitCode == 137:
		return "oom"
	case res.ExitCode == 0:
		return "ok"
	default:
		return "nonzero_exit"
	}
}
