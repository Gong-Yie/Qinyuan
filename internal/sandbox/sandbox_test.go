package sandbox

import (
	"context"
	"net/netip"
	"strings"
	"testing"
)

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
