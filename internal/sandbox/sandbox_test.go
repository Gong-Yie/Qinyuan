package sandbox

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 等锁时 ctx 取消必须立刻返回，不能等前一个 120s 命令跑完。
func TestRunCancelledWhileWaiting(t *testing.T) {
	r := NewWSLDocker(Config{})
	if err := r.lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Run(ctx, Request{Command: "id"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("应返回 context.Canceled，实际 %v", err)
	}
}

func TestParsePorts(t *testing.T) {
	cases := []struct {
		in   string
		low  int
		high int
		ok   bool
	}{
		{"4444", 4444, 4444, true},
		{"40000-40100", 40000, 40100, true},
		{" 8080 ", 8080, 8080, true},
		{"", 0, 0, false},
		{"abc", 0, 0, false},
		{"0", 0, 0, false},
		{"70000", 0, 0, false},
		{"500-400", 0, 0, false},
		{"4444abc", 0, 0, false},
		{"40000-40100xyz", 0, 0, false},
	}
	for _, c := range cases {
		p, err := ParsePorts(c.in)
		if c.ok != (err == nil) {
			t.Fatalf("%q：期望 ok=%v，实际 err=%v", c.in, c.ok, err)
		}
		if err == nil && (p.Low != c.low || p.High != c.high) {
			t.Fatalf("%q：解析成 %+v", c.in, p)
		}
	}
}

// 规则顺序与内容：先逐条 ACCEPT（scope 任意端口、callback 指定端口区间），最后一定是 DROP。
func TestRenderRules(t *testing.T) {
	subnet := "10.201.0.0/24"
	scope := netip.MustParsePrefix("203.0.113.7/32")
	cb := netip.MustParsePrefix("198.51.100.9/32")
	allows := []Allow{
		{Dst: scope, From: "scope"},
		{Dst: cb, Proto: "tcp", Ports: Ports{Low: 40000, High: 40100}, From: "callback"},
		// 重复项要去重
		{Dst: cb, Proto: "tcp", Ports: Ports{Low: 40000, High: 40100}, From: "callback"},
		// 回环/无效地址必须被丢掉
		{Dst: netip.MustParsePrefix("127.0.0.1/32")},
		{Dst: netip.Prefix{}},
	}
	rules := RenderRules(subnet, allows)
	if len(rules) != 3 {
		t.Fatalf("去重后应有 2 条 ACCEPT + 1 条 DROP，实际 %d：%v", len(rules), rules)
	}
	scopeRule := strings.Join(rules[0], " ")
	if scopeRule != "-s 10.201.0.0/24 -d 203.0.113.7/32 -j ACCEPT" {
		t.Fatalf("scope 规则不对：%q", scopeRule)
	}
	cbRule := strings.Join(rules[1], " ")
	if cbRule != "-s 10.201.0.0/24 -d 198.51.100.9/32 -p tcp --dport 40000:40100 -j ACCEPT" {
		t.Fatalf("callback 规则不对：%q", cbRule)
	}
	last := strings.Join(rules[2], " ")
	if last != "-s 10.201.0.0/24 -j DROP" {
		t.Fatalf("最后一条必须是 DROP：%q", last)
	}
}

// 沙箱网络只有 IPv4；v6 前缀必须被丢掉，不能喂给 iptables。
func TestRenderRulesSkipsIPv6(t *testing.T) {
	allows := []Allow{
		{Dst: netip.MustParsePrefix("203.0.113.7/32"), From: "scope"},
		{Dst: netip.MustParsePrefix("2001:db8::/32"), From: "scope"},
	}
	rules := RenderRules("10.201.0.0/24", allows)
	if len(rules) != 2 {
		t.Fatalf("应只有 1 条 v4 ACCEPT + 1 条 DROP，实际：%v", rules)
	}
	if strings.Contains(strings.Join(rules[0], " "), "2001:") {
		t.Fatalf("v6 规则不该出现：%v", rules)
	}
}

// 空白名单也要有 DROP（空闲态 = 全拒）。
func TestRenderRulesEmptyAlwaysDrops(t *testing.T) {
	rules := RenderRules("10.201.0.0/24", nil)
	if len(rules) != 1 || strings.Join(rules[0], " ") != "-s 10.201.0.0/24 -j DROP" {
		t.Fatalf("空白名单必须只有 DROP：%v", rules)
	}
}

// Fake 只记录，不做任何系统调用（exec 工具测试用它）。
func TestFakeRunnerRecords(t *testing.T) {
	f := &Fake{Result: Result{ExitCode: 0, Stdout: "ok"}}
	res, err := f.Run(context.Background(), Request{RunID: 7, Command: "echo ok"})
	if err != nil || res.Stdout != "ok" {
		t.Fatalf("fake 结果不对：%+v %v", res, err)
	}
	if len(f.Reqs) != 1 || f.Reqs[0].RunID != 7 {
		t.Fatalf("请求没记录：%+v", f.Reqs)
	}
}

// 限量缓冲：超过上限只保留前 N，但必须把剩余读掉（不堵管道）。
func TestLimitedBuffer(t *testing.T) {
	var b limitedBuffer
	b.limit = 5
	_, _ = b.Write([]byte("abcdefg"))
	_, _ = b.Write([]byte("hij"))
	if b.String() != "abcde" {
		t.Fatalf("保留内容不对：%q", b.String())
	}
	if b.total != 10 {
		t.Fatalf("total = %d，期望 10", b.total)
	}
}

// ARTEX Capture 语义：head 截断，但完整内容落盘，且总量/截断标志准确。
func TestLimitedBufferSpillsFullOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stdout.txt")
	b := newLimitedBuffer(5, path, 100)
	_, _ = b.Write([]byte("abcdefg"))
	b.Close()
	if b.String() != "abcde" {
		t.Fatalf("head 不对：%q", b.String())
	}
	if b.Total() != 7 || !b.Truncated() {
		t.Fatalf("total/truncated 不对：%d %v", b.Total(), b.Truncated())
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "abcdefg" {
		t.Fatalf("落盘内容不对：%q %v", data, err)
	}
	if b.Path() != path {
		t.Fatalf("路径不对：%q", b.Path())
	}
}

// 落盘本身也要有上限：超过 SpillLimit 时保留前 N 并标记。
func TestLimitedBufferSpillCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stdout.txt")
	b := newLimitedBuffer(3, path, 4)
	_, _ = b.Write([]byte("abcdefg"))
	b.Close()
	data, _ := os.ReadFile(path)
	if string(data) != "abcd" {
		t.Fatalf("落盘应截到 4 字节，实际 %q", data)
	}
	if !b.SpillTruncated() {
		t.Fatal("应标记 spillTruncated")
	}
}

func TestClassifyExec(t *testing.T) {
	cases := []struct {
		code         int
		killed, disk bool
		want         string
	}{
		{0, false, false, "ok"},
		{1, false, false, "nonzero_exit"},
		{124, false, false, "timeout"},
		{137, false, false, "oom"},
		{143, true, false, "timeout"},
		{137, false, true, "disk_exceeded"},
	}
	for _, c := range cases {
		if got := classifyExec(c.code, c.killed, c.disk); got != c.want {
			t.Fatalf("classifyExec(%d,%v,%v)=%q，期望 %q", c.code, c.killed, c.disk, got, c.want)
		}
	}
}

// 域名→IPv4 的 --add-host 映射：v6/空 Host 不进，去重且排序。
func TestHostEntries(t *testing.T) {
	r := &WSLDocker{}
	entries := r.hostEntries([]Allow{
		{Dst: netip.MustParsePrefix("203.0.113.7/32"), Host: "Target.Test"},
		{Dst: netip.MustParsePrefix("203.0.113.8/32"), Host: "target.test."},
		{Dst: netip.MustParsePrefix("2001:db8::/32"), Host: "v6.test"},
		{Dst: netip.MustParsePrefix("203.0.113.9/32")},
	})
	got := strings.Join(entries, ",")
	if got != "target.test:203.0.113.7,target.test:203.0.113.8" {
		t.Fatalf("hostEntries 不对：%q", got)
	}
}
