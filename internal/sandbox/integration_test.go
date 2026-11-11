package sandbox

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 集成测试：需要 WSL2 + Docker + qy-sandbox 网络 + qinyuan/sandbox:latest 镜像。
//
//	$env:QINYUAN_SANDBOX_TEST="1"
//	$env:QINYUAN_SANDBOX_TEST_TARGET="1.2.3.4:8080"   # 白名单里要放行的目标
//	go test ./internal/sandbox -run Integration -v
//
// 守三件事：容器内不是 root、离线命令能跑、白名单外的连接出不去。
func TestWSLDockerIntegration(t *testing.T) {
	if os.Getenv("QINYUAN_SANDBOX_TEST") != "1" {
		t.Skip("设置 QINYUAN_SANDBOX_TEST=1 才跑（需要 WSL2 + Docker）")
	}
	target := os.Getenv("QINYUAN_SANDBOX_TEST_TARGET")
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		t.Fatalf("QINYUAN_SANDBOX_TEST_TARGET 必须是 host:port：%v", err)
	}
	port, _ := strconv.Atoi(portStr)
	ipStr, dst := resolveTarget(t, host)

	cfg := Config{Distro: os.Getenv("QINYUAN_SANDBOX_DISTRO")}
	r := NewWSLDocker(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	if err := r.Check(ctx); err != nil {
		t.Fatalf("沙箱自检失败：%v", err)
	}

	// 1) 非 root + 离线可跑
	res, err := r.Run(ctx, Request{RunID: 9001, Command: "id -u; echo hello-offline", Timeout: 90 * time.Second})
	if err != nil {
		t.Fatalf("离线执行失败：%v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%s", res.ExitCode, res.Stderr)
	}
	if !strings.HasPrefix(res.Stdout, "1000") {
		t.Fatalf("容器内应当是 uid 1000，实际输出：%q", res.Stdout)
	}

	allow := []Allow{{Dst: dst, Proto: "tcp", Ports: Ports{Low: port, High: port}, From: "callback"}}
	script := func(pyHost string, pyPort int) string {
		return fmt.Sprintf(`python3 - <<'PY'
import socket
try:
    socket.create_connection(('%s', %d), 5)
    print('connect-ok')
except Exception:
    print('blocked')
PY`, pyHost, pyPort)
	}

	// 2) 白名单内：能连到目标端口
	// 沙箱内不放 DNS：直接用解析好的 IP 连，避免容器侧解析到别的地址。
	res, err = r.Run(ctx, Request{RunID: 9001, Command: script(ipStr, port), Timeout: 90 * time.Second, Allows: allow})
	if err != nil {
		t.Fatalf("白名单执行失败：%v", err)
	}
	if !strings.Contains(res.Stdout, "connect-ok") {
		t.Fatalf("白名单内应能连通，stdout=%q stderr=%q", res.Stdout, res.Stderr)
	}

	// 3) 白名单外：连不出去
	res, err = r.Run(ctx, Request{RunID: 9001, Command: script("1.1.1.1", 80), Timeout: 90 * time.Second, Allows: allow})
	if err != nil {
		t.Fatalf("拒绝路径执行失败：%v", err)
	}
	if strings.Contains(res.Stdout, "connect-ok") {
		t.Fatalf("白名单外的连接不该成功：stdout=%q stderr=%q", res.Stdout, res.Stderr)
	}
}

// resolveTarget 把主机解析成"一个 IP + /32 前缀"。
func resolveTarget(t *testing.T, host string) (string, netip.Prefix) {
	t.Helper()
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.String(), netip.PrefixFrom(addr, addr.BitLen())
	}
	ips, err := net.LookupHost(host)
	if err != nil || len(ips) == 0 {
		t.Fatalf("解析 %q 失败：%v", host, err)
	}
	addr, err := netip.ParseAddr(ips[0])
	if err != nil {
		t.Fatalf("解析 IP %q 失败：%v", ips[0], err)
	}
	return addr.String(), netip.PrefixFrom(addr, addr.BitLen())
}
