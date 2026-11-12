package engine

import (
	"context"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"qinyuan/internal/model"
	"qinyuan/internal/sandbox"
)

// egressFor 把"这次 exec 允许连哪"翻译成沙箱放行项。
//
// 语义（docs/24）：
//   - network=target：只放行 run_scope；范围为空 = 什么都不放行（沙箱只能离线）；
//   - network=callback：再叠加当前生效的回连白名单（operator 显式登记、带 TTL）；
//   - 域名在这里解析成 IP（沙箱内不放 DNS，避免用 DNS 做外带）。
func (e *Engine) egressFor(ctx context.Context, runID, _ int64, network string) []sandbox.Allow {
	if network == "" || network == "none" {
		return nil
	}
	var out []sandbox.Allow
	scopes, err := e.Store.Scopes(ctx, runID)
	if err != nil {
		e.Log.Warn("[sandbox] 读授权范围失败（本次不放行任何目标）", "run", runID, "err", err)
		return nil
	}
	for _, sc := range scopes {
		out = append(out, scopeAllows(ctx, sc)...)
	}
	if network == "callback" {
		cbs, err := e.Store.ActiveCallbacks(ctx, runID)
		if err != nil {
			e.Log.Warn("[sandbox] 读回连白名单失败（本次不放行回连）", "run", runID, "err", err)
			return out
		}
		for _, cb := range cbs {
			out = append(out, callbackAllows(ctx, cb)...)
		}
	}
	return out
}

// scopeAllows 把一条 run_scope 转成放行项。语义上"允许打"= 任意端口。
func scopeAllows(ctx context.Context, sc model.Scope) []sandbox.Allow {
	switch sc.Kind {
	case "ip":
		if a, err := netip.ParseAddr(firstNonEmpty(sc.Domain, sc.Value)); err == nil {
			return []sandbox.Allow{{Dst: netip.PrefixFrom(a, a.BitLen()), From: "scope"}}
		}
	case "cidr":
		if p, err := netip.ParsePrefix(firstNonEmpty(sc.Net, sc.Value)); err == nil {
			return []sandbox.Allow{{Dst: p.Masked(), From: "scope"}}
		}
	case "subdomain", "root_domain":
		return hostAllows(ctx, firstNonEmpty(sc.Domain, sc.Value), "", sandbox.Ports{}, "scope")
	case "url":
		if u, err := url.Parse(firstNonEmpty(sc.Value, sc.Domain)); err == nil && u.Hostname() != "" {
			return hostAllows(ctx, u.Hostname(), "", sandbox.Ports{}, "scope")
		}
	}
	return nil
}

// callbackAllows 把一条回连白名单转成放行项（协议 + 端口区间是显式的）。
func callbackAllows(ctx context.Context, cb *model.Callback) []sandbox.Allow {
	if cb == nil || !cb.Active(time.Now()) {
		return nil
	}
	ports, err := sandbox.ParsePorts(cb.Ports)
	if err != nil {
		return nil
	}
	return hostAllows(ctx, cb.Host, cb.Proto, ports, "callback")
}

// hostAllows 把主机名解析成 IP 前缀（最多 8 个，避免 DNS 返回一大串）。
func hostAllows(ctx context.Context, host, proto string, ports sandbox.Ports, from string) []sandbox.Allow {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "" {
		return nil
	}
	if a, err := netip.ParseAddr(host); err == nil {
		return []sandbox.Allow{{Dst: netip.PrefixFrom(a, a.BitLen()), Proto: proto, Ports: ports, From: from}}
	}
	rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupNetIP(rctx, "ip", host)
	if err != nil {
		return nil
	}
	var out []sandbox.Allow
	for i, a := range ips {
		if i >= 8 {
			break
		}
		out = append(out, sandbox.Allow{
			Dst: netip.PrefixFrom(a, a.BitLen()), Proto: proto, Ports: ports,
			From: from, Host: host,
		})
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
