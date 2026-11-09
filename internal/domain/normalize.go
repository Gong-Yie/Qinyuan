// Package domain 是领域归一化 + 范围闸门。
//
// 依据 01-数据模型 §6.2：归一化必须由程序做，不能让模型做
// （否则 SSH / ssh、example.com. / example.com 会分裂成两行）。
package domain

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// NormalizeHost 小写、去空白、去尾部点。
func NormalizeHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	h = strings.TrimSuffix(h, ".")
	return h
}

// RootDomain 取 eTLD+1；IP 字面量或没有公共后缀的（内网域名）原样返回。
func RootDomain(host string) string {
	h := NormalizeHost(host)
	if h == "" {
		return ""
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return h
	}
	if root, err := publicsuffix.EffectiveTLDPlusOne(h); err == nil {
		return root
	}
	return h
}

// SplitHostPort 宽松地拆 host:port，缺端口时按 scheme 补默认值。
func SplitHostPort(raw, scheme string) (host string, port int, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", 0, fmt.Errorf("空主机")
	}
	if h, p, e := net.SplitHostPort(raw); e == nil {
		port, err = parsePort(p)
		if err != nil {
			return "", 0, err
		}
		return NormalizeHost(h), port, nil
	}
	// 没有端口：可能是 IPv6 字面量，也可能是普通主机名
	if ip, e := netip.ParseAddr(strings.Trim(raw, "[]")); e == nil {
		return ip.String(), defaultPort(scheme), nil
	}
	return NormalizeHost(raw), defaultPort(scheme), nil
}

func parsePort(p string) (int, error) {
	var n int
	if _, err := fmt.Sscanf(p, "%d", &n); err != nil {
		return 0, fmt.Errorf("非法端口 %q", p)
	}
	if n < 1 || n > 65535 {
		return 0, fmt.Errorf("端口越界 %d", n)
	}
	return n, nil
}

func defaultPort(scheme string) int {
	if strings.EqualFold(scheme, "https") {
		return 443
	}
	return 80
}

// NormalizeURL 统一 scheme/host 大小写、去掉默认端口、空路径补 "/"、去 fragment。
func NormalizeURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("URL 解析失败：%w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("只支持 http/https，收到 %q", u.Scheme)
	}
	host := NormalizeHost(u.Hostname())
	if host == "" {
		return "", fmt.Errorf("URL 缺少 host")
	}
	port := u.Port()
	if port == "" || (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		u.Host = host
	} else {
		u.Host = net.JoinHostPort(host, port)
	}
	u.Scheme = scheme
	u.Fragment = ""
	if u.Path == "" {
		u.Path = "/"
	}
	return u.String(), nil
}

// HostOfURL 取 URL 的 host（已归一化）。
func HostOfURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	h := NormalizeHost(u.Hostname())
	if h == "" {
		return "", fmt.Errorf("URL 缺少 host")
	}
	return h, nil
}

// ParseIP 只接受 IP 字面量（主机名必须走 subdomain，否则网段归属会静默失效）。
func ParseIP(s string) (netip.Addr, error) {
	ip, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil {
		return netip.Addr{}, fmt.Errorf("%q 不是 IP 字面量（主机名请按 subdomain 登记）", s)
	}
	return ip.Unmap(), nil
}

// SegmentFor 返回网段：IPv4 取 /24，IPv6 取 /48。
func SegmentFor(ip netip.Addr) string {
	if !ip.IsValid() {
		return ""
	}
	bits := 24
	if ip.Is6() {
		bits = 48
	}
	p, err := ip.Prefix(bits)
	if err != nil {
		return ""
	}
	return p.String()
}

// HostIsIP 判断 host 是否是 IP 字面量。
func HostIsIP(host string) bool {
	_, err := netip.ParseAddr(strings.Trim(host, "[]"))
	return err == nil
}
