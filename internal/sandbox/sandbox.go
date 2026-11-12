// Package sandbox 是 worker 的"执行沙箱"：能跑命令，但网络默认全拒，
// 只放行「授权目标 + 显式回连白名单」。
//
// 为什么强制点不在沙箱内部：沙箱里跑的是**模型生成的代码**（不可信），
// 任何应用层检查都能被绕过。所以规则写在沙箱外的 Docker/iptables 层，
// 由钦原签发、由内核执行。
//
// 形态（见 docs/24）：Windows 上跑钦原，WSL2 里跑 Docker；
// 单槽串行 —— 规则是全局的，一次只跑一个 exec，避免 A 的放行项被 B 用上。
package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Config 是沙箱运行参数（全部有安全默认值）。
type Config struct {
	Distro    string        // WSL 发行版，默认 Ubuntu-24.04
	Image     string        // 沙箱镜像，默认 qinyuan/sandbox:latest
	Network   string        // docker 网络，默认 qy-sandbox
	Subnet    string        // 该网络的子网，默认 10.201.0.0/24
	JobRoot   string        // 作业目录根（WSL 内路径），默认 /var/lib/qinyuan/jobs
	Timeout   time.Duration // 单次默认超时，默认 120s
	MaxOutput int           // stdout/stderr 各自上限，默认 64KiB

	// DataDir 是宿主数据目录。非空时，完整 stdout/stderr 会落到
	// <DataDir>/runs/<runID>/cmd-output/，模型拿到的是路径而不是被截断的正文。
	DataDir string
	// DiskLimit 是单次 /job 的软上限（字节），默认 512MiB；超过会 kill 容器。
	DiskLimit int64
	// SpillLimit 是单个输出流落盘上限（字节），默认 50MiB。
	SpillLimit int64
	// FileSizeLimit 是容器内单文件上限（--ulimit fsize），默认 512MiB。
	FileSizeLimit int64
}

// Ports 是端口或端口区间（闭区间）。
type Ports struct {
	Low, High int
}

// ParsePorts 解析 "4444" 或 "40000-40100"。
func ParsePorts(s string) (Ports, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Ports{}, errors.New("端口不能为空")
	}
	part := strings.SplitN(s, "-", 2)
	low, err := strconv.Atoi(strings.TrimSpace(part[0]))
	if err != nil || low < 1 || low > 65535 {
		return Ports{}, fmt.Errorf("端口 %q 不合法", s)
	}
	high := low
	if len(part) == 2 {
		high, err = strconv.Atoi(strings.TrimSpace(part[1]))
		if err != nil || high < low || high > 65535 {
			return Ports{}, fmt.Errorf("端口区间 %q 不合法", s)
		}
	}
	return Ports{Low: low, High: high}, nil
}

func (p Ports) String() string {
	if p.High <= p.Low {
		return strconv.Itoa(p.Low)
	}
	return fmt.Sprintf("%d:%d", p.Low, p.High)
}

// Allow 是一条放行项。Dst 用前缀表示（单 IP 就是 /32 / /128）；
// Proto/Ports 为零值时表示"任意协议/端口"（只用于 run_scope 里的目标）。
type Allow struct {
	Dst   netip.Prefix
	Proto string // tcp | udp；空 = 任意
	Ports Ports  // 0 = 任意
	From  string // scope | callback（只用于审计/展示）
	// Host 是放行项对应的域名（IP 直连时为空）。Run 会把它写成
	// --add-host，配合禁用的 DNS，让容器仍能按域名访问授权目标。
	Host string
}

// Request 是一次执行。
type Request struct {
	RunID    int64
	IntentID int64
	Command  string
	Timeout  time.Duration // 0 = 用 cfg.Timeout
	Allows   []Allow
	// Network 是 none|target|callback，仅用于审计/展示；实际放行由 Allows 决定。
	Network string
}

// Result 是一次执行的结果。
type Result struct {
	ExitCode  int
	Stdout    string
	Stderr    string
	Truncated bool
	Killed    bool
	Duration  time.Duration
	JobDir    string

	// 完整输出落盘路径（宿主可读）；空 = 没落盘。
	StdoutPath  string
	StderrPath  string
	StdoutBytes int64
	StderrBytes int64
	// SpillTruncated=true 表示完整输出超过落盘上限，文件本身也不完整。
	SpillTruncated bool
	// SpillError 记录落盘失败原因（磁盘满/权限等），不影响命令本身的执行。
	SpillError string
	// FailureClass 是给模型/审计看的失败归因：
	// ok | timeout | oom | disk_exceeded | nonzero_exit | sandbox_error
	FailureClass string
}

// Runner 是沙箱执行器（测试用 Fake）。
type Runner interface {
	Run(ctx context.Context, req Request) (Result, error)
}

// ---------- 规则渲染（纯函数，可单测） ----------

// RenderRules 渲染一串 iptables 规则参数（不含 -A 链名），末尾一定是 DROP。
//
// 顺序即语义：先逐条 ACCEPT，最后兜底 DROP。去重按 (IP, proto, ports)，
// 同一目的多段端口会保留多行（iptables 没有"多段端口"）。
func RenderRules(subnet string, allows []Allow) [][]string {
	var out [][]string
	seen := map[string]bool{}
	for _, a := range allows {
		if !a.Dst.IsValid() || a.Dst.Addr().IsLoopback() || a.Dst.Addr().IsUnspecified() {
			continue
		}
		// 沙箱网络只有 IPv4；把 v6 前缀喂给 iptables 会直接报
		// "invalid mask / host/network not found"，并让整次规则写入失败。
		if !a.Dst.Addr().Is4() {
			continue
		}
		proto := strings.ToLower(strings.TrimSpace(a.Proto))
		if proto != "tcp" && proto != "udp" {
			proto = ""
		}
		key := fmt.Sprintf("%s|%s|%s", a.Dst, proto, a.Ports.String())
		if seen[key] {
			continue
		}
		seen[key] = true
		rule := []string{"-s", subnet, "-d", a.Dst.String()}
		if proto != "" && a.Ports.Low > 0 {
			rule = append(rule, "-p", proto, "--dport", a.Ports.String())
		}
		rule = append(rule, "-j", "ACCEPT")
		out = append(out, rule)
	}
	out = append(out, []string{"-s", subnet, "-j", "DROP"})
	return out
}

// ---------- 假执行器（测试用） ----------

// Fake 记录请求并返回预设结果；不碰 WSL/Docker。
type Fake struct {
	Result Result
	Err    error
	Reqs   []Request
	mu     sync.Mutex
}

func (f *Fake) Run(_ context.Context, req Request) (Result, error) {
	f.mu.Lock()
	f.Reqs = append(f.Reqs, req)
	f.mu.Unlock()
	if f.Err != nil {
		return Result{}, f.Err
	}
	return f.Result, nil
}

// ---------- WSL2 + Docker 执行器 ----------

// WSLDocker 用 WSL2 里的 Docker 跑命令，并用 iptables DOCKER-USER 做白名单。
type WSLDocker struct {
	cfg Config
	wsl string

	// sem 是 ctx-aware 的单槽信号量：等锁的 worker 在自己的 ctx 取消时能立刻退出，
	// 不会因为前一个 120s 命令把暂停/删除拖住。semOnce 兼容零值/测试构造。
	semOnce sync.Once
	sem     chan struct{}
}

// NewWSLDocker 构造执行器；不做任何系统调用（规则在第一次 Run 时安装）。
func NewWSLDocker(cfg Config) *WSLDocker {
	if cfg.Distro == "" {
		cfg.Distro = "Ubuntu-24.04"
	}
	if cfg.Image == "" {
		cfg.Image = "qinyuan/sandbox:latest"
	}
	if cfg.Network == "" {
		cfg.Network = "qy-sandbox"
	}
	if cfg.Subnet == "" {
		cfg.Subnet = "10.201.0.0/24"
	}
	if cfg.JobRoot == "" {
		cfg.JobRoot = "/var/lib/qinyuan/jobs"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 120 * time.Second
	}
	if cfg.MaxOutput <= 0 {
		cfg.MaxOutput = 64 << 10
	}
	if cfg.DiskLimit <= 0 {
		cfg.DiskLimit = 512 << 20
	}
	if cfg.SpillLimit <= 0 {
		cfg.SpillLimit = 50 << 20
	}
	if cfg.FileSizeLimit <= 0 {
		cfg.FileSizeLimit = 512 << 20
	}
	return &WSLDocker{cfg: cfg, wsl: "wsl.exe"}
}

// Check 做一次连通性自检（启动时调用）：WSL + docker + 专用网络/镜像/iptables 都可用才返回 nil。
//
// 它同时做两件“启动清理”：
//  1. 把 FORWARD/INPUT 两条链恢复成只有 DROP（崩溃残留的 ACCEPT 不会带进新进程）；
//  2. 删掉上一次崩溃留下的 qinyuan.job 容器。
func (r *WSLDocker) Check(ctx context.Context) error {
	switch r.cfg.Network {
	case "host", "bridge", "none":
		return fmt.Errorf("QINYUAN_SANDBOX_NETWORK=%q 不能用于沙箱：会绕过 QY-SANDBOX 规则；请使用专用网络", r.cfg.Network)
	}
	if _, err := r.wslOut(ctx, "docker", "version", "--format", "{{.Server.Version}}"); err != nil {
		return fmt.Errorf("WSL2 里的 docker 不可用：%w", err)
	}
	out, err := r.runRoot(ctx, "docker", "network", "inspect", r.cfg.Network,
		"--format", "{{(index .IPAM.Config 0).Subnet}}")
	if err != nil {
		return fmt.Errorf("docker 网络 %s 不可用：%w", r.cfg.Network, err)
	}
	want, err := netip.ParsePrefix(strings.TrimSpace(r.cfg.Subnet))
	if err != nil {
		return fmt.Errorf("QINYUAN_SANDBOX_SUBNET=%q 不合法：%w", r.cfg.Subnet, err)
	}
	got, err := netip.ParsePrefix(strings.TrimSpace(out))
	if err != nil {
		return fmt.Errorf("读不出 docker 网络 %s 的 subnet（%q）：%w", r.cfg.Network, strings.TrimSpace(out), err)
	}
	if got.Masked() != want.Masked() {
		return fmt.Errorf("沙箱网络 %s 的 subnet=%s，与 QINYUAN_SANDBOX_SUBNET=%s 不一致；规则会匹配不到容器",
			r.cfg.Network, got.Masked(), want.Masked())
	}
	if _, err := r.runRoot(ctx, "docker", "image", "inspect", r.cfg.Image, "--format", "{{.Id}}"); err != nil {
		return fmt.Errorf("沙箱镜像 %s 不可用：%w", r.cfg.Image, err)
	}
	if _, err := r.runRoot(ctx, ipt("-L", "DOCKER-USER", "-n")...); err != nil {
		return fmt.Errorf("iptables DOCKER-USER 不可用：%w", err)
	}

	// 启动时清理上一次崩溃/强杀留下的容器（单实例假设；容器都带 qinyuan.job label）。
	if ids, err := r.runRoot(ctx, "docker", "ps", "-aq", "--filter", "label=qinyuan.job"); err == nil {
		for _, id := range strings.Fields(ids) {
			_, _ = r.runRoot(ctx, "docker", "rm", "-f", id)
		}
	}

	if err := r.idleRules(ctx); err != nil {
		return fmt.Errorf("初始化沙箱默认拒绝规则失败：%w", err)
	}
	return nil
}

// lock 获取单槽；等待期间 ctx 取消时立即返回，不拖住暂停/删除。
func (r *WSLDocker) lock(ctx context.Context) error {
	r.semOnce.Do(func() {
		if r.sem == nil {
			r.sem = make(chan struct{}, 1)
		}
	})
	select {
	case r.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *WSLDocker) unlock() { <-r.sem }

// Run 执行一次命令。
func (r *WSLDocker) Run(ctx context.Context, req Request) (Result, error) {
	if strings.TrimSpace(req.Command) == "" {
		return Result{}, errors.New("命令不能为空")
	}
	if err := r.lock(ctx); err != nil {
		return Result{}, err
	}
	defer r.unlock()

	// 1) 先写规则；写不进去就**不执行**，并把半成品规则清成 DROP。
	if err := r.applyRules(ctx, req.Allows); err != nil {
		r.cleanupRules()
		return Result{}, fmt.Errorf("沙箱规则写入失败，拒绝执行：%w", err)
	}
	defer r.cleanupRules()

	// 2) 作业目录（WSL 内），给容器当 /job。
	id := fmt.Sprintf("%d-%d", req.RunID, time.Now().UnixNano())
	dir := fmt.Sprintf("%s/%d/%s", r.cfg.JobRoot, req.RunID, id)
	if _, err := r.runRoot(ctx, "mkdir", "-p", dir); err != nil {
		return Result{}, fmt.Errorf("建作业目录失败：%w", err)
	}
	if _, err := r.runRoot(ctx, "chown", "-R", "1000:1000", dir); err != nil {
		return Result{}, fmt.Errorf("设置作业目录权限失败：%w", err)
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = r.cfg.Timeout
	}
	if timeout < 5*time.Second {
		timeout = 5 * time.Second
	}
	if timeout > 15*time.Minute {
		timeout = 15 * time.Minute
	}
	seconds := int(timeout.Seconds())

	args := []string{
		"run", "-i", "--rm",
		"--name", "qy-" + id,
		"--label", "qinyuan.run=" + strconv.FormatInt(req.RunID, 10),
		"--label", "qinyuan.job=" + id,
		"--network", r.cfg.Network,
		// 禁掉 Docker 内嵌 DNS 的外网上游；授权域名走 --add-host。
		"--dns", "127.0.0.1",
		"--user", "1000:1000",
		"--cap-drop=ALL",
		"--security-opt", "no-new-privileges",
		"--read-only",
		"--tmpfs", "/tmp:rw,size=256m",
		"--tmpfs", "/home/sandbox:rw,size=64m",
		"--pids-limit", "256",
		"--memory", "1g",
		"--memory-swap", "1g",
		"--cpus", "1",
		"--init",
		"--ulimit", "fsize=" + strconv.FormatInt(r.cfg.FileSizeLimit, 10),
	}
	for _, h := range r.hostEntries(req.Allows) {
		args = append(args, "--add-host", h)
	}
	args = append(args,
		"-v", dir+":/job",
		"-w", "/job",
		r.cfg.Image,
		// timeout 在容器内兜底：到点先 TERM 再 KILL，退出码 124。
		"timeout", "-k", "5", strconv.Itoa(seconds), "bash", "-s",
	)

	stdoutPath, stderrPath := r.outputPaths(req.RunID, id)
	diskKilled := &atomic.Bool{}
	stopWatch := make(chan struct{})
	go r.watchDisk(ctx, dir, "qy-"+id, diskKilled, stopWatch)
	defer close(stopWatch)

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			r.killContainer("qy-" + id)
		case <-done:
		}
	}()
	defer close(done)

	out, errOut, code, killed, dur, err := r.docker(
		ctx, append([]string{"-d", r.cfg.Distro, "-u", "root", "--", "docker"}, args...),
		req.Command, stdoutPath, stderrPath)
	res := Result{
		ExitCode:       code,
		Stdout:         out.String(),
		Stderr:         errOut.String(),
		Killed:         killed || code == 124 || diskKilled.Load(),
		Duration:       dur,
		JobDir:         dir,
		StdoutPath:     out.Path(),
		StderrPath:     errOut.Path(),
		StdoutBytes:    out.Total(),
		StderrBytes:    errOut.Total(),
		Truncated:      out.Truncated() || errOut.Truncated(),
		SpillTruncated: out.SpillTruncated() || errOut.SpillTruncated(),
		SpillError:     firstNonEmpty(out.SpillError(), errOut.SpillError()),
	}
	res.FailureClass = classifyExec(code, res.Killed, diskKilled.Load())
	if err != nil {
		return res, err
	}
	return res, nil
}

const (
	chainForward = "QY-SANDBOX"
	chainInput   = "QY-SANDBOX-IN"
)

// ipt 给所有 iptables 调用带上 -w，避免和 Docker 并发时被 xtables lock 直接拒。
func ipt(args ...string) []string {
	return append([]string{"iptables", "-w", "5"}, args...)
}

func (r *WSLDocker) ensureChain(ctx context.Context, chain string) error {
	// 链不存在就建；已存在会报错，忽略。
	_, _ = r.runRoot(ctx, ipt("-N", chain)...)
	return nil
}

func (r *WSLDocker) ensureJump(ctx context.Context, parent, chain string) error {
	if _, err := r.runRoot(ctx, ipt("-C", parent, "-j", chain)...); err == nil {
		return nil
	}
	if _, err := r.runRoot(ctx, ipt("-I", parent, "1", "-j", chain)...); err != nil {
		return fmt.Errorf("把 %s 挂到 %s 失败：%w", chain, parent, err)
	}
	return nil
}

// applyRules 重建白名单链：DROP 先落，再用 -I 1 把 ACCEPT 插到它前面。
//
// 这样即使某条 ACCEPT 失败/ctx 被取消，链里也始终有兜底 DROP；
// Run 的错误路径还会再调一次 cleanupRules。旧实现是“先 -F，再逐条 -A”，
// 第一条失败就会留下空链（不过滤）。
func (r *WSLDocker) applyRules(ctx context.Context, allows []Allow) error {
	for _, chain := range []string{chainForward, chainInput} {
		if err := r.ensureChain(ctx, chain); err != nil {
			return err
		}
	}
	if err := r.ensureJump(ctx, "DOCKER-USER", chainForward); err != nil {
		return err
	}
	if err := r.ensureJump(ctx, "INPUT", chainInput); err != nil {
		return err
	}
	if _, err := r.runRoot(ctx, ipt("-F", chainForward)...); err != nil {
		return err
	}
	if _, err := r.runRoot(ctx, ipt("-A", chainForward, "-s", r.cfg.Subnet, "-j", "DROP")...); err != nil {
		return err
	}
	rules := RenderRules(r.cfg.Subnet, allows)
	for i := len(rules) - 2; i >= 0; i-- {
		args := append([]string{"-I", chainForward, "1"}, rules[i]...)
		if _, err := r.runRoot(ctx, ipt(args...)...); err != nil {
			return err
		}
	}

	// INPUT 链只保留“子网 → DROP”，阻断容器访问宿主服务。
	if _, err := r.runRoot(ctx, ipt("-F", chainInput)...); err != nil {
		return err
	}
	if _, err := r.runRoot(ctx, ipt("-A", chainInput, "-s", r.cfg.Subnet, "-j", "DROP")...); err != nil {
		return err
	}
	return nil
}

// idleRules 把两条链恢复成“只有 DROP”（空闲态 = 全拒）。
func (r *WSLDocker) idleRules(ctx context.Context) error {
	for _, chain := range []string{chainForward, chainInput} {
		if err := r.ensureChain(ctx, chain); err != nil {
			return err
		}
		if _, err := r.runRoot(ctx, ipt("-F", chain)...); err != nil {
			return err
		}
		if _, err := r.runRoot(ctx, ipt("-A", chain, "-s", r.cfg.Subnet, "-j", "DROP")...); err != nil {
			return err
		}
	}
	if err := r.ensureJump(ctx, "DOCKER-USER", chainForward); err != nil {
		return err
	}
	if err := r.ensureJump(ctx, "INPUT", chainInput); err != nil {
		return err
	}
	return nil
}

func (r *WSLDocker) cleanupRules() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = r.idleRules(ctx)
}

func (r *WSLDocker) runRoot(ctx context.Context, args ...string) (string, error) {
	full := append([]string{"-d", r.cfg.Distro, "-u", "root", "--"}, args...)
	return r.wslOut(ctx, full...)
}

func (r *WSLDocker) wslOut(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, r.wsl, args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("%v（stderr：%s）", err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// outputPaths 给完整 stdout/stderr 选宿主路径；DataDir 为空时不落盘。
func (r *WSLDocker) outputPaths(runID int64, id string) (string, string) {
	if r.cfg.DataDir == "" {
		return "", ""
	}
	dir := filepath.Join(r.cfg.DataDir, "runs", strconv.FormatInt(runID, 10), "cmd-output")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", ""
	}
	return filepath.Join(dir, id+"-stdout.txt"), filepath.Join(dir, id+"-stderr.txt")
}

// hostEntries 把 Allow 里的域名→IPv4 转成 docker --add-host 参数。
// 配合 --dns 127.0.0.1：域名仍可用，但 DNS 外带被关掉。
func (r *WSLDocker) hostEntries(allows []Allow) []string {
	seen := map[string]map[string]bool{}
	for _, a := range allows {
		if a.Host == "" || !a.Dst.IsValid() || !a.Dst.Addr().Is4() {
			continue
		}
		h := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(a.Host), "."))
		if h == "" {
			continue
		}
		if seen[h] == nil {
			seen[h] = map[string]bool{}
		}
		seen[h][a.Dst.Addr().String()] = true
	}
	var out []string
	for h, ips := range seen {
		for ip := range ips {
			out = append(out, h+":"+ip)
		}
	}
	sort.Strings(out)
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

func classifyExec(code int, killed, disk bool) string {
	switch {
	case disk:
		return "disk_exceeded"
	case killed || code == 124:
		return "timeout"
	case code == 137:
		return "oom"
	case code == 0:
		return "ok"
	default:
		return "nonzero_exit"
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func (r *WSLDocker) killContainer(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = r.runRoot(ctx, "docker", "kill", name)
}

// watchDisk 是 /job 的软配额：超过 DiskLimit 就 kill 容器。
// 这是 ARTEX task.Manager.watchSize 的钦原版本；没有它，一条 dd 就能填满 WSL 磁盘。
func (r *WSLDocker) watchDisk(ctx context.Context, dir, name string, hit *atomic.Bool, stop <-chan struct{}) {
	if r.cfg.DiskLimit <= 0 {
		return
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			out, err := r.runRoot(ctx, "du", "-sb", dir)
			if err != nil {
				continue
			}
			fields := strings.Fields(out)
			if len(fields) == 0 {
				continue
			}
			n, err := strconv.ParseInt(fields[0], 10, 64)
			if err != nil {
				continue
			}
			if n > r.cfg.DiskLimit {
				hit.Store(true)
				r.killContainer(name)
				return
			}
		}
	}
}

// docker 跑容器并把 stdout/stderr 限量收集；完整输出由 limitedBuffer 落盘。
func (r *WSLDocker) docker(ctx context.Context, args []string, script, stdoutPath, stderrPath string) (*limitedBuffer, *limitedBuffer, int, bool, time.Duration, error) {
	start := time.Now()
	cmd := exec.CommandContext(ctx, r.wsl, args...)
	cmd.Stdin = strings.NewReader(script)

	out := newLimitedBuffer(r.cfg.MaxOutput, stdoutPath, r.cfg.SpillLimit)
	errb := newLimitedBuffer(r.cfg.MaxOutput, stderrPath, r.cfg.SpillLimit)
	defer out.Close()
	defer errb.Close()
	cmd.Stdout = out
	cmd.Stderr = errb

	err := cmd.Run()
	dur := time.Since(start)
	code := 0
	killed := false
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			return out, errb, -1, false, dur, err
		}
	}
	if ctx.Err() != nil {
		killed = true
	}
	return out, errb, code, killed, dur, nil
}
