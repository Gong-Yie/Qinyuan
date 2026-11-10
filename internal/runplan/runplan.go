// Package runplan 把"用户输入的目标"翻译成建 run 所需的结构（范围 + 种子实体）。
//
// 放在这里而不是 CLI/server 里，是因为两条入口必须用同一份规则：
// 否则命令行建的任务和 API 建的任务，范围与种子实体会不一样。
package runplan

import (
	"fmt"
	"net/netip"
	"net/url"
	"strings"

	"qinyuan/internal/domain"
	"qinyuan/internal/model"
	"qinyuan/internal/store"
)

// Request 建 run 的用户输入。
type Request struct {
	Goal           string
	Description    string
	Name           string
	Target         string
	TimeoutSeconds int
	WorkerCount    int
}

// Build 生成 CreateRunInput。
//
// 目标解析规则（最小范围原则，来自实现规格/03 §4 第三步）：
//   - URL / 带主机名的地址 → 取完整主机名作 subdomain 范围（不缩成根域）；
//   - 纯 IP → ip 范围；
//   - CIDR → cidr 范围；
//   - 裸根域且明确说"整站"才用 root_domain —— 这里只在 target 以 "." 开头时允许（".example.com"）。
func Build(in Request) (store.CreateRunInput, error) {
	out := store.CreateRunInput{
		Goal:           strings.TrimSpace(in.Goal),
		Description:    strings.TrimSpace(in.Description),
		Name:           strings.TrimSpace(in.Name),
		TimeoutSeconds: in.TimeoutSeconds,
		WorkerCount:    in.WorkerCount,
	}
	if out.Goal == "" {
		return out, fmt.Errorf("goal 不能为空")
	}
	target := strings.TrimSpace(in.Target)
	if target == "" {
		return out, nil
	}

	// URL（scheme 大小写不敏感：HTTP:// 也是 URL，不能当裸主机名处理）
	lowerTarget := strings.ToLower(target)
	if strings.HasPrefix(lowerTarget, "http://") || strings.HasPrefix(lowerTarget, "https://") {
		norm, err := domain.NormalizeURL(target)
		if err != nil {
			return out, err
		}
		// 入口 URL 必须留下来：范围只回答"允许打哪"，不回答"从哪开始"。
		// 丢掉端口时 worker 会去猜 80/443/3000…（真实 run 里 49% 的请求打错端口）。
		out.Seeds = append(out.Seeds, norm)
		u, _ := url.Parse(norm)
		host := domain.NormalizeHost(u.Hostname())
		// 主机名是 IP 字面量时必须按 ip 登记：subdomain 规则不参与 IP 匹配。
		if ip, err := netip.ParseAddr(host); err == nil {
			ip = ip.Unmap()
			out.Scopes = append(out.Scopes, model.Scope{
				Kind: "ip", Value: ip.String(), Source: "manual",
				Reason: "目标 URL 的 IP（最小范围）",
			})
			out.SeedEntities = append(out.SeedEntities, store.EntityInput{
				Kind: "ip", IP: ip.String(), Segment: domain.SegmentFor(ip),
			})
			return out, nil
		}
		out.Scopes = append(out.Scopes, model.Scope{
			Kind: "subdomain", Domain: host, Source: "manual",
			Reason: "目标 URL 的完整主机名（最小范围）",
		})
		out.SeedEntities = append(out.SeedEntities, seedForHost(host))
		return out, nil
	}

	// CIDR
	if strings.Contains(target, "/") {
		if p, err := netip.ParsePrefix(target); err == nil {
			out.Scopes = append(out.Scopes, model.Scope{
				Kind: "cidr", Net: p.Masked().String(), Source: "manual",
				Reason: "目标网段",
			})
			return out, nil
		}
	}

	// 通配根域
	if strings.HasPrefix(target, ".") {
		root := domain.RootDomain(strings.TrimPrefix(target, "."))
		out.Scopes = append(out.Scopes, model.Scope{
			Kind: "root_domain", Domain: root, Source: "manual",
			Reason: "用户指定整站（通配根域）",
		})
		out.SeedEntities = append(out.SeedEntities, store.EntityInput{Kind: "root_domain", Domain: root, RootDomain: root})
		return out, nil
	}

	// IP
	if ip, err := netip.ParseAddr(target); err == nil {
		ip = ip.Unmap()
		out.Scopes = append(out.Scopes, model.Scope{
			Kind: "ip", Value: ip.String(), Source: "manual", Reason: "目标 IP",
		})
		out.SeedEntities = append(out.SeedEntities, store.EntityInput{
			Kind: "ip", IP: ip.String(), Segment: domain.SegmentFor(ip),
		})
		return out, nil
	}

	// 普通主机名
	host := domain.NormalizeHost(target)
	if host == "" {
		return out, fmt.Errorf("无法解析目标 %q", target)
	}
	out.Scopes = append(out.Scopes, model.Scope{
		Kind: "subdomain", Domain: host, Source: "manual",
		Reason: "目标主机名（最小范围）",
	})
	out.SeedEntities = append(out.SeedEntities, seedForHost(host))
	return out, nil
}

func seedForHost(host string) store.EntityInput {
	if ip, err := netip.ParseAddr(host); err == nil {
		return store.EntityInput{Kind: "ip", IP: ip.Unmap().String(), Segment: domain.SegmentFor(ip.Unmap())}
	}
	return store.EntityInput{
		Kind: "subdomain", Domain: host, RootDomain: domain.RootDomain(host),
	}
}
