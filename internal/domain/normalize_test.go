package domain

import (
	"net/netip"
	"testing"

	"qinyuan/internal/model"
)

func TestNormalizeHostAndRootDomain(t *testing.T) {
	cases := []struct{ in, host, root string }{
		{"Example.COM.", "example.com", "example.com"},
		{"  a.b.Example.co.uk ", "a.b.example.co.uk", "example.co.uk"},
		{"127.0.0.1", "127.0.0.1", "127.0.0.1"},
		{"intranet", "intranet", "intranet"},
	}
	for _, c := range cases {
		if got := NormalizeHost(c.in); got != c.host {
			t.Errorf("NormalizeHost(%q)=%q，期望 %q", c.in, got, c.host)
		}
		if got := RootDomain(c.in); got != c.root {
			t.Errorf("RootDomain(%q)=%q，期望 %q", c.in, got, c.root)
		}
	}
}

func TestNormalizeURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"HTTP://Example.COM:80", "http://example.com/"},
		{"https://Example.com:443/a/b?q=1#frag", "https://example.com/a/b?q=1"},
		{"https://example.com:8443", "https://example.com:8443/"},
	}
	for _, c := range cases {
		got, err := NormalizeURL(c.in)
		if err != nil {
			t.Fatalf("NormalizeURL(%q) 失败：%v", c.in, err)
		}
		if got != c.want {
			t.Errorf("NormalizeURL(%q)=%q，期望 %q", c.in, got, c.want)
		}
	}
	if _, err := NormalizeURL("ftp://x/"); err == nil {
		t.Error("非 http(s) 应当报错")
	}
}

func TestSegmentFor(t *testing.T) {
	cases := map[string]string{
		"192.168.1.77": "192.168.1.0/24",
		"2001:db8::1":  "2001:db8::/48",
	}
	for in, want := range cases {
		ip, err := ParseIP(in)
		if err != nil {
			t.Fatal(err)
		}
		if got := SegmentFor(ip); got != want {
			t.Errorf("SegmentFor(%s)=%s，期望 %s", in, got, want)
		}
	}
	if _, err := ParseIP("example.com"); err == nil {
		t.Error("主机名不该被当成 IP")
	}
}

func TestScopeSet(t *testing.T) {
	set := NewScopeSet([]model.Scope{
		{Kind: "root_domain", Domain: "example.com"},
		{Kind: "subdomain", Domain: "api.other.test"},
		{Kind: "cidr", Net: "10.0.0.0/8"},
		{Kind: "ip", Value: "203.0.113.9"},
	})

	allowHost := []string{"example.com", "a.example.com", "api.other.test", "10.1.2.3", "203.0.113.9"}
	for _, h := range allowHost {
		if !set.AllowsHost(h) {
			t.Errorf("%q 应当在范围内", h)
		}
	}
	denyHost := []string{"evil.com", "example.com.evil.com", "other.test", "api.other.test.evil.com", "11.0.0.1"}
	for _, h := range denyHost {
		if set.AllowsHost(h) {
			t.Errorf("%q 不该在范围内（后缀匹配必须按点边界）", h)
		}
	}

	// subdomain 是精确匹配，不覆盖子域。
	if set.AllowsHost("x.api.other.test") {
		t.Error("subdomain 规则不该匹配它的子域")
	}

	// 空范围 = 默认拒绝。
	empty := NewScopeSet(nil)
	if !empty.Empty() || empty.AllowsHost("example.com") {
		t.Error("空范围必须拒绝一切")
	}
}

func TestSplitHostPort(t *testing.T) {
	cases := []struct {
		in     string
		scheme string
		host   string
		port   int
	}{
		{"example.com:8443", "https", "example.com", 8443},
		{"Example.COM.", "http", "example.com", 80},
		{"[2001:db8::1]:8080", "http", "2001:db8::1", 8080},
		{"10.0.0.1", "https", "10.0.0.1", 443},
	}
	for _, c := range cases {
		h, p, err := SplitHostPort(c.in, c.scheme)
		if err != nil {
			t.Fatalf("SplitHostPort(%q) 失败：%v", c.in, err)
		}
		if h != c.host || p != c.port {
			t.Errorf("SplitHostPort(%q)=(%s,%d)，期望 (%s,%d)", c.in, h, p, c.host, c.port)
		}
	}
	if _, _, err := SplitHostPort("example.com:99999", "http"); err == nil {
		t.Error("越界端口应当报错")
	}
}

func TestDialableIP(t *testing.T) {
	// 这个函数在 agent 包里，这里只验证 domain 层的 IP 语义边界（回环/链路本地）。
	for _, s := range []string{"127.0.0.1", "169.254.169.254", "::1"} {
		ip := netip.MustParseAddr(s)
		if !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
			t.Errorf("%s 应被识别为回环/链路本地", s)
		}
	}
}
