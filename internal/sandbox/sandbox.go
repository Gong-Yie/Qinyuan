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
	"io"
	"net/netip"
	"os/exec"
	"strconv"
	"strings"
	"sync"
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
}

// Request 是一次执行。
type Request struct {
	RunID    int64
	IntentID int64
	Command  string
	Timeout  time.Duration // 0 = 用 cfg.Timeout
	Allows   []Allow
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
	mu  sync.Mutex
	wsl string
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
	return &WSLDocker{cfg: cfg, wsl: "wsl.exe"}
}

// Check 做一次连通性自检（启动时可选调用）：WSL + docker 都可用才返回 nil。
func (r *WSLDocker) Check(ctx context.Context) error {
	if _, err := r.wslOut(ctx, "docker", "version", "--format", "{{.Server.Version}}"); err != nil {
		return fmt.Errorf("WSL2 里的 docker 不可用：%w", err)
	}
	if _, err := r.runRoot(ctx, "iptables", "-L", "DOCKER-USER", "-n"); err != nil {
		return fmt.Errorf("iptables DOCKER-USER 不可用：%w", err)
	}
	return nil
}

// Run 执行一次命令。
func (r *WSLDocker) Run(ctx context.Context, req Request) (Result, error) {
	if strings.TrimSpace(req.Command) == "" {
		return Result{}, errors.New("命令不能为空")
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	// 1) 先写规则；写不进去就**不执行**（宁可不跑，也不在无规则状态下跑）。
	if err := r.applyRules(ctx, req.Allows); err != nil {
		return Result{}, fmt.Errorf("沙箱规则写入失败，拒绝执行：%w", err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = r.idleRules(cleanupCtx)
	}()

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
		"--network", r.cfg.Network,
		"--user", "1000:1000",
		"--cap-drop=ALL",
		"--security-opt", "no-new-privileges",
		"--read-only",
		"--tmpfs", "/tmp:rw,size=256m",
		"--pids-limit", "256",
		"--memory", "1g",
		"--cpus", "1",
		"-v", dir + ":/job",
		"-w", "/job",
		r.cfg.Image,
		// timeout 在容器内兜底：到点先 TERM 再 KILL，退出码 124。
		"timeout", "-k", "5", strconv.Itoa(seconds), "bash", "-s",
	}
	out, errOut, code, killed, dur, err := r.docker(ctx, append([]string{"-d", r.cfg.Distro, "-u", "root", "--", "docker"}, args...), req.Command)
	res := Result{
		ExitCode:  code,
		Stdout:    out,
		Stderr:    errOut,
		Killed:    killed || code == 124,
		Duration:  dur,
		JobDir:    dir,
		Truncated: len(out) >= r.cfg.MaxOutput || len(errOut) >= r.cfg.MaxOutput,
	}
	if err != nil {
		return res, err
	}
	return res, nil
}

// applyRules 重建白名单链：ACCEPT × N + DROP。
func (r *WSLDocker) applyRules(ctx context.Context, allows []Allow) error {
	// 链不存在就建（存在会报错，忽略）。
	_, _ = r.runRoot(ctx, "iptables", "-N", "QY-SANDBOX")
	if _, err := r.runRoot(ctx, "iptables", "-F", "QY-SANDBOX"); err != nil {
		return err
	}
	for _, rule := range RenderRules(r.cfg.Subnet, allows) {
		args := append([]string{"iptables", "-A", "QY-SANDBOX"}, rule...)
		if _, err := r.runRoot(ctx, args...); err != nil {
			return err
		}
	}
	// DOCKER-USER 里只装一次跳转（-C 成功说明已有）。
	if _, err := r.runRoot(ctx, "iptables", "-C", "DOCKER-USER", "-j", "QY-SANDBOX"); err != nil {
		if _, err := r.runRoot(ctx, "iptables", "-I", "DOCKER-USER", "1", "-j", "QY-SANDBOX"); err != nil {
			return err
		}
	}
	return nil
}

// idleRules 执行完把链恢复成"只有 DROP"（空闲态 = 全拒）。
func (r *WSLDocker) idleRules(ctx context.Context) error {
	if _, err := r.runRoot(ctx, "iptables", "-F", "QY-SANDBOX"); err != nil {
		return err
	}
	_, err := r.runRoot(ctx, "iptables", "-A", "QY-SANDBOX", "-s", r.cfg.Subnet, "-j", "DROP")
	return err
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

// docker 跑容器并把 stdout/stderr 限量收集；返回退出码、是否超时、耗时。
func (r *WSLDocker) docker(ctx context.Context, args []string, script string) (string, string, int, bool, time.Duration, error) {
	start := time.Now()
	cmd := exec.CommandContext(ctx, r.wsl, args...)
	cmd.Stdin = strings.NewReader(script)

	var out, errb limitedBuffer
	out.limit = r.cfg.MaxOutput
	errb.limit = r.cfg.MaxOutput
	cmd.Stdout = &out
	cmd.Stderr = &errb

	err := cmd.Run()
	dur := time.Since(start)
	code := 0
	killed := false
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			return out.String(), errb.String(), -1, false, dur, err
		}
	}
	if ctx.Err() != nil {
		killed = true
	}
	return out.String(), errb.String(), code, killed, dur, nil
}

// limitedBuffer 只保留前 limit 字节，其余读掉但不存（避免把管道堵死）。
type limitedBuffer struct {
	buf   bytes.Buffer
	limit int
	total int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.total += len(p)
	if b.limit <= 0 || b.buf.Len() < b.limit {
		room := b.limit - b.buf.Len()
		if room > len(p) {
			room = len(p)
		}
		if room > 0 {
			b.buf.Write(p[:room])
		}
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string { return b.buf.String() }

var _ io.Writer = (*limitedBuffer)(nil)
