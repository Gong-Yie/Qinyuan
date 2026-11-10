package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"

	"qinyuan/internal/domain"
	"qinyuan/internal/model"
	"qinyuan/internal/store"
)

// 本文件是第 0 轮"目标拆解"的工具面（实现规格/03 §4）。
//
// 三个工具，只给 goals agent：
//   set_constraints —— 先抽操作约束（allow/deny，必须自包含）
//   add_scope       —— 登记最小授权范围（覆盖度分母）
//   set_goals       —— 拆出最终目标（"最终可交付/可核验的结果"，不是步骤）
//
// 它拿不到 add_intent / prove_goal / http_request：拆解者不派方向、不判完成、不干活。

// NewSetGoalsTool 用拆解结果替换 seed 目标（seed 目标保留血缘但置 abandoned）。
func NewSetGoalsTool() *Tool {
	return &Tool{
		Name: "set_goals",
		Description: `登记本任务的**最终目标**（可交付/可核验的结果，不是侦察、步骤或验证过程）。
一次调用提交全部目标：目标互不依赖就分别列出，只有一个就输出一个；不要臆造用户没提的目标。
拆解结果会替换掉系统预先种下的那条粗目标（它会被标成 abandoned 保留血缘）。`,
		Schema: toolSpecJSON(`{
  "type": "object",
  "properties": {
    "goals": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "object",
        "properties": {
          "text": {"type": "string", "description": "最终可核验的结果"},
          "vulnclass": {"type": "string", "description": "可选：对应漏洞类别"}
        },
        "required": ["text"]
      }
    }
  },
  "required": ["goals"]
}`),
		Handle: handleSetGoals,
	}
}

func handleSetGoals(ctx context.Context, rc *Context, raw json.RawMessage) (Result, error) {
	var in struct {
		Goals []struct {
			Text      string `json:"text"`
			VulnClass string `json:"vulnclass"`
		} `json:"goals"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return Errorf("参数解析失败：%v", err), nil
	}
	specs := make([]store.AddGoalSpec, 0, len(in.Goals))
	for _, g := range in.Goals {
		text := strings.TrimSpace(g.Text)
		if text == "" {
			continue
		}
		specs = append(specs, store.AddGoalSpec{Text: text, VulnClass: strings.TrimSpace(g.VulnClass)})
	}
	if len(specs) == 0 {
		return Errorf("goals 不能为空：至少给一条最终目标；如果用户输入里确实没有可核验的目标，就不要调用这个工具"), nil
	}
	ids, err := rc.Store.ReplaceGoalsFromDecomposition(ctx, rc.RunID, specs)
	if err != nil {
		return Errorf("%v", err), nil
	}
	return JSONResult(map[string]any{
		"goal_ids": ids,
		"count":    len(ids),
		"note":     "seed 目标已退役（abandoned），血缘保留",
	}), nil
}

// NewSetConstraintsTool 登记操作约束（allow/deny）。
func NewSetConstraintsTool() *Tool {
	return &Tool{
		Name: "set_constraints",
		Description: `登记操作员明确规定的**操作约束**（不是目标、不是步骤）。
deny=禁止的操作，allow=明确允许/限定的范围；拿不准类型时用 deny（更保守）。
约束必须**自包含**：把"当前目标/本站/这个端口"替换成具体值 —— 约束会被单独注入执行阶段，
脱离上下文后指代词没人知道指谁。只登记用户明确写出的约束，严禁臆造；一条都没有就不要调用。`,
		Schema: toolSpecJSON(`{
  "type": "object",
  "properties": {
    "items": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "object",
        "properties": {
          "type": {"type": "string", "enum": ["allow", "deny"]},
          "text": {"type": "string", "description": "自包含的约束原文"}
        },
        "required": ["type", "text"]
      }
    }
  },
  "required": ["items"]
}`),
		Handle: handleSetConstraints,
	}
}

func handleSetConstraints(ctx context.Context, rc *Context, raw json.RawMessage) (Result, error) {
	var in struct {
		Items []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return Errorf("参数解析失败：%v", err), nil
	}
	items := make([]model.Constraint, 0, len(in.Items))
	for _, it := range in.Items {
		text := strings.TrimSpace(it.Text)
		if text == "" {
			continue
		}
		kind := strings.ToLower(strings.TrimSpace(it.Type))
		if kind != "allow" && kind != "deny" {
			kind = "deny"
		}
		items = append(items, model.Constraint{Kind: kind, Text: text})
	}
	if len(items) == 0 {
		return Errorf("items 不能为空；没有明确约束就不要调用这个工具"), nil
	}
	added, err := rc.Store.AddConstraints(ctx, rc.RunID, items)
	if err != nil {
		return Errorf("%v", err), nil
	}
	return JSONResult(map[string]any{"added": added, "total": len(items)}), nil
}

// NewAddScopeTool 登记最小授权范围。
func NewAddScopeTool() *Tool {
	return &Tool{
		Name: "add_scope",
		Description: `登记本任务的授权范围（覆盖度分母 + 越界拦截依据）。**最小范围原则**：
URL / 带主机名的地址 → kind=subdomain 且取完整主机名（例：https://a1b2.lab.example.net/x → a1b2.lab.example.net，
严禁缩成 example.net）；只有裸根域且明确说"整站/所有子域"才用 root_domain；纯 IP/网段 → ip / cidr。
只登记用户明确写出的范围；没有就不要调用。`,
		Schema: toolSpecJSON(`{
  "type": "object",
  "properties": {
    "entries": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "object",
        "properties": {
          "kind": {"type": "string", "enum": ["root_domain", "subdomain", "ip", "cidr", "icp", "keyword"]},
          "value": {"type": "string", "description": "主机名 / IP / 网段 / ICP / 关键词"},
          "reason": {"type": "string"}
        },
        "required": ["kind", "value"]
      }
    }
  },
  "required": ["entries"]
}`),
		Handle: handleAddScope,
	}
}

func handleAddScope(ctx context.Context, rc *Context, raw json.RawMessage) (Result, error) {
	var in struct {
		Entries []struct {
			Kind   string `json:"kind"`
			Value  string `json:"value"`
			Reason string `json:"reason"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return Errorf("参数解析失败：%v", err), nil
	}
	type result struct {
		Index int    `json:"index"`
		Kind  string `json:"kind,omitempty"`
		Value string `json:"value,omitempty"`
		Error string `json:"error,omitempty"`
	}
	results := make([]result, 0, len(in.Entries))
	for i, e := range in.Entries {
		sc, err := scopeFromInput(e.Kind, e.Value, e.Reason)
		if err != nil {
			results = append(results, result{Index: i, Error: err.Error()})
			continue
		}
		if err := rc.Store.AddScope(ctx, rc.RunID, sc); err != nil {
			results = append(results, result{Index: i, Error: err.Error()})
			continue
		}
		results = append(results, result{Index: i, Kind: sc.Kind, Value: firstNonEmptyStr(sc.Domain, sc.Net, sc.Value)})
	}
	return JSONResult(map[string]any{"results": results}), nil
}

// scopeFromInput 把模型给的一条范围归一化成 run_scope 行。
//
// 归一化必须在这里做（模型只会给"人类写法"）：小写、去尾点、eTLD+1、/24 由 domain 包负责。
func scopeFromInput(kind, value, reason string) (model.Scope, error) {
	v := strings.TrimSpace(value)
	kind = strings.ToLower(strings.TrimSpace(kind))
	if v == "" {
		return model.Scope{}, fmt.Errorf("value 不能为空")
	}
	sc := model.Scope{Source: "agent", Reason: strings.TrimSpace(reason)}
	if sc.Reason == "" {
		sc.Reason = "目标拆解登记的范围"
	}
	switch kind {
	case "root_domain":
		host := domain.NormalizeHost(v)
		if host == "" {
			return sc, fmt.Errorf("无效域名 %q", v)
		}
		sc.Kind, sc.Domain = "root_domain", domain.RootDomain(host)
	case "subdomain":
		host := domain.NormalizeHost(v)
		if host == "" {
			return sc, fmt.Errorf("无效主机名 %q", v)
		}
		if _, err := netip.ParseAddr(host); err == nil {
			return sc, fmt.Errorf("%q 是 IP 字面量：请用 kind=ip（subdomain 规则不参与 IP 匹配）", v)
		}
		sc.Kind, sc.Domain = "subdomain", host
	case "ip":
		ip, err := netip.ParseAddr(v)
		if err != nil {
			return sc, fmt.Errorf("%q 不是 IP 字面量；主机名请用 subdomain", v)
		}
		sc.Kind, sc.Value = "ip", ip.Unmap().String()
	case "cidr":
		p, err := netip.ParsePrefix(v)
		if err != nil {
			return sc, fmt.Errorf("%q 不是合法网段（形如 10.0.0.0/24）", v)
		}
		sc.Kind, sc.Net = "cidr", p.Masked().String()
	case "icp", "keyword":
		sc.Kind, sc.Value = kind, v
	default:
		return sc, fmt.Errorf("不支持的 kind=%q（root_domain/subdomain/ip/cidr/icp/keyword）", kind)
	}
	return sc, nil
}

// NewGoalsRegistry 是第 0 轮拆解者的工具面：只登记目标/约束/范围。
func NewGoalsRegistry() *Registry {
	return NewRegistry(
		NewSetGoalsTool(),
		NewSetConstraintsTool(),
		NewAddScopeTool(),
	)
}
