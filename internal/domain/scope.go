package domain

import (
	"fmt"
	"net/netip"
	"strings"

	"qinyuan/internal/model"
)

// ScopeSet 是某个 run 的授权范围（从 run_scope 加载后构建一次，反复使用）。
//
// 语义：范围为空 = 什么都没授权（默认拒绝，不是默认放行）。
type ScopeSet struct {
	raw      []model.Scope
	domains  []domainRule
	prefixes []netip.Prefix
	ips      []netip.Addr
}

type domainRule struct {
	domain string
	suffix bool // root_domain: 允许子域；subdomain: 必须精确相等
}

// NewScopeSet 构建范围集合。
func NewScopeSet(scopes []model.Scope) *ScopeSet {
	s := &ScopeSet{raw: scopes}
	for _, sc := range scopes {
		switch sc.Kind {
		case "root_domain":
			if d := NormalizeHost(sc.Domain); d != "" {
				s.domains = append(s.domains, domainRule{domain: d, suffix: true})
			}
		case "subdomain":
			if d := NormalizeHost(sc.Domain); d != "" {
				s.domains = append(s.domains, domainRule{domain: d})
			}
		case "cidr":
			if p, err := netip.ParsePrefix(strings.TrimSpace(sc.Net)); err == nil {
				s.prefixes = append(s.prefixes, p.Masked())
			}
		case "ip":
			if a, err := netip.ParseAddr(strings.TrimSpace(sc.Value)); err == nil {
				s.ips = append(s.ips, a.Unmap())
			} else if a, err := netip.ParseAddr(strings.TrimSpace(sc.Domain)); err == nil {
				s.ips = append(s.ips, a.Unmap())
			}
		}
	}
	return s
}

// Empty 没有任何可用范围规则。
func (s *ScopeSet) Empty() bool {
	return len(s.domains) == 0 && len(s.prefixes) == 0 && len(s.ips) == 0
}

// AllowsHost 判断主机名是否在范围内。
func (s *ScopeSet) AllowsHost(host string) bool {
	h := NormalizeHost(host)
	if h == "" {
		return false
	}
	if ip, err := netip.ParseAddr(h); err == nil {
		return s.AllowsIP(ip.Unmap())
	}
	for _, r := range s.domains {
		if h == r.domain {
			return true
		}
		if r.suffix && strings.HasSuffix(h, "."+r.domain) {
			return true
		}
	}
	return false
}

// AllowsIP 判断 IP 是否在范围内。
func (s *ScopeSet) AllowsIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	for _, a := range s.ips {
		if a == ip {
			return true
		}
	}
	for _, p := range s.prefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// AllowsURL 判断 URL 的 host 是否在范围内。
func (s *ScopeSet) AllowsURL(raw string) (bool, string) {
	host, err := HostOfURL(raw)
	if err != nil {
		return false, err.Error()
	}
	if !s.AllowsHost(host) {
		return false, fmt.Sprintf("host %q 不在本任务授权范围内", host)
	}
	return true, ""
}

// Describe 给模型看的范围摘要（拒绝时用，别让模型瞎猜）。
func (s *ScopeSet) Describe() string {
	if s.Empty() {
		return "（本任务尚未登记任何范围）"
	}
	var parts []string
	for _, r := range s.domains {
		if r.suffix {
			parts = append(parts, "*."+r.domain)
		} else {
			parts = append(parts, r.domain)
		}
	}
	for _, p := range s.prefixes {
		parts = append(parts, p.String())
	}
	for _, a := range s.ips {
		parts = append(parts, a.String())
	}
	return strings.Join(parts, ", ")
}

// Raw 返回原始规则（覆盖度计算/权限展示用）。
func (s *ScopeSet) Raw() []model.Scope { return s.raw }
