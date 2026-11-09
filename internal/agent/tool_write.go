package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"qinyuan/internal/domain"
	"qinyuan/internal/store"
)

// ---------- insert_assets ----------

type assetItem struct {
	Type          string   `json:"type"`
	Domain        string   `json:"domain"`
	RootDomain    string   `json:"root_domain"`
	RecordType    string   `json:"record_type"`
	IP            string   `json:"ip"`
	URL           string   `json:"url"`
	Method        string   `json:"method"`
	Port          int      `json:"port"`
	ServiceType   string   `json:"service_type"`
	ServiceName   string   `json:"service_name"`
	StatusCode    int      `json:"status_code"`
	ContentLength int64    `json:"content_length"`
	PageTitle     string   `json:"page_title"`
	Technologies  []string `json:"technologies"`
}

type insertAssetsInput struct {
	Assets []assetItem `json:"assets"`
}

type assetResult struct {
	Index   int    `json:"index"`
	ID      int64  `json:"id,omitempty"`
	Type    string `json:"type,omitempty"`
	Created bool   `json:"created,omitempty"`
	Error   string `json:"error,omitempty"`
}

// NewInsertAssetsTool 登记"世界是什么"：子域/服务/端点/指纹。
//
// 它只登记实体本身，不写结论（结论走 record_fact）。去重完全交给数据库唯一索引，
// 所以模型重复提交是安全的。
func NewInsertAssetsTool() *Tool {
	return &Tool{
		Name: "insert_assets",
		Description: `把一个或多个【实体】登记进资产图：root_domain / ip / subdomain / service / endpoint。
只登记"世界是什么"（域名、端口、URL、状态码、标题、指纹），不要在这里写结论或漏洞。
重复登记是幂等的（数据库去重），可以放心多次调用；返回里带每个实体的 id，后续写事实时可用。
范围外的条目会被逐条拒绝，不影响其余条目。`,
		Schema: toolSpecJSON(`{
  "type": "object",
  "properties": {
    "assets": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "type": {"type": "string", "enum": ["root_domain","ip","subdomain","service","endpoint"]},
          "domain": {"type": "string"},
          "root_domain": {"type": "string"},
          "record_type": {"type": "string", "description": "DNS 记录类型（A/CNAME…），subdomain 用"},
          "ip": {"type": "string", "description": "只接受 IP 字面量"},
          "url": {"type": "string"},
          "method": {"type": "string"},
          "port": {"type": "integer"},
          "service_type": {"type": "string", "enum": ["http","other"]},
          "service_name": {"type": "string"},
          "status_code": {"type": "integer"},
          "content_length": {"type": "integer"},
          "page_title": {"type": "string"},
          "technologies": {"type": "array", "items": {"type": "string"}}
        },
        "required": ["type"]
      }
    }
  },
  "required": ["assets"]
}`),
		Handle: handleInsertAssets,
	}
}

func handleInsertAssets(ctx context.Context, rc *Context, raw json.RawMessage) (Result, error) {
	var in insertAssetsInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return Errorf("参数解析失败：%v", err), nil
	}
	if len(in.Assets) == 0 {
		return Errorf("assets 不能为空"), nil
	}
	if len(in.Assets) > 50 {
		return Errorf("一次最多 50 条（收到 %d），请分批", len(in.Assets)), nil
	}

	results := make([]assetResult, 0, len(in.Assets))
	okCount := 0
	for i, item := range in.Assets {
		ent, host, ip, err := normalizeAsset(item)
		if err != nil {
			results = append(results, assetResult{Index: i, Error: err.Error()})
			continue
		}
		if err := checkScope(rc, host, ip); err != nil {
			results = append(results, assetResult{Index: i, Error: err.Error()})
			continue
		}
		id, created, err := rc.Store.UpsertEntity(ctx, rc.RunID, ent)
		if err != nil {
			results = append(results, assetResult{Index: i, Error: err.Error()})
			continue
		}
		if rc.IntentID > 0 {
			if err := rc.Store.Anchor(ctx, rc.IntentID, id); err != nil {
				results = append(results, assetResult{Index: i, Error: "锚点写入失败：" + err.Error()})
				continue
			}
		}
		results = append(results, assetResult{Index: i, ID: id, Type: ent.Kind, Created: created})
		okCount++
	}
	return JSONResult(map[string]any{
		"ok":      okCount,
		"total":   len(in.Assets),
		"results": results,
	}), nil
}

func normalizeAsset(item assetItem) (store.EntityInput, string, netip.Addr, error) {
	var ent store.EntityInput
	var host string
	var ip netip.Addr

	switch item.Type {
	case "root_domain":
		d := domain.NormalizeHost(item.Domain)
		if d == "" {
			return ent, "", ip, fmt.Errorf("root_domain 需要 domain")
		}
		return store.EntityInput{Kind: "root_domain", Domain: d, RootDomain: d}, d, ip, nil

	case "subdomain":
		d := domain.NormalizeHost(item.Domain)
		if d == "" {
			return ent, "", ip, fmt.Errorf("subdomain 需要 domain")
		}
		ent = store.EntityInput{
			Kind:       "subdomain",
			Domain:     d,
			RootDomain: domain.RootDomain(d),
			RecordType: strings.ToUpper(strings.TrimSpace(item.RecordType)),
		}
		if strings.TrimSpace(item.IP) != "" {
			a, err := domain.ParseIP(item.IP)
			if err != nil {
				return ent, "", ip, err
			}
			ent.IP = a.String()
			ent.Segment = domain.SegmentFor(a)
			ip = a
		}
		return ent, d, ip, nil

	case "ip":
		a, err := domain.ParseIP(item.IP)
		if err != nil {
			return ent, "", ip, err
		}
		return store.EntityInput{Kind: "ip", IP: a.String(), Segment: domain.SegmentFor(a)}, a.String(), a, nil

	case "service":
		svcType := item.ServiceType
		if svcType == "" {
			if item.URL != "" {
				svcType = "http"
			} else {
				svcType = "other"
			}
		}
		ent = store.EntityInput{
			Kind:        "service",
			ServiceType: svcType,
			ServiceName: strings.ToLower(strings.TrimSpace(item.ServiceName)),
			Port:        item.Port,
		}
		if item.URL != "" {
			norm, err := domain.NormalizeURL(item.URL)
			if err != nil {
				return ent, "", ip, err
			}
			h, err := domain.HostOfURL(norm)
			if err != nil {
				return ent, "", ip, err
			}
			host = h
			ent.URL = norm
			ent.Domain = h
			ent.RootDomain = domain.RootDomain(h)
			ent.StatusCode = item.StatusCode
			ent.ContentLength = item.ContentLength
			ent.PageTitle = item.PageTitle
			ent.Technologies = item.Technologies
			if ent.Port == 0 {
				if u, err := urlPort(norm); err == nil {
					ent.Port = u
				}
			}
			if domain.HostIsIP(h) {
				if a, err := domain.ParseIP(h); err == nil {
					ip = a
				}
			}
			return ent, host, ip, nil
		}
		ent.Domain = domain.NormalizeHost(item.Domain)
		ent.RootDomain = domain.RootDomain(ent.Domain)
		if strings.TrimSpace(item.IP) != "" {
			a, err := domain.ParseIP(item.IP)
			if err != nil {
				return ent, "", ip, err
			}
			ent.IP = a.String()
			ip = a
		}
		host = firstNonEmptyStr(ent.Domain, ent.IP)
		return ent, host, ip, nil

	case "endpoint":
		if strings.TrimSpace(item.URL) == "" || strings.TrimSpace(item.Method) == "" {
			return ent, "", ip, fmt.Errorf("endpoint 需要 url 与 method")
		}
		norm, err := domain.NormalizeURL(item.URL)
		if err != nil {
			return ent, "", ip, err
		}
		h, err := domain.HostOfURL(norm)
		if err != nil {
			return ent, "", ip, err
		}
		ent = store.EntityInput{
			Kind:          "endpoint",
			URL:           norm,
			Method:        strings.ToUpper(item.Method),
			Domain:        h,
			RootDomain:    domain.RootDomain(h),
			StatusCode:    item.StatusCode,
			ContentLength: item.ContentLength,
			PageTitle:     item.PageTitle,
		}
		if domain.HostIsIP(h) {
			if a, err := domain.ParseIP(h); err == nil {
				ip = a
			}
		}
		return ent, h, ip, nil

	default:
		return ent, "", ip, fmt.Errorf("未知类型 %q（允许 root_domain/ip/subdomain/service/endpoint）", item.Type)
	}
}

func urlPort(raw string) (int, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return 0, err
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return 0, err
		}
		return n, nil
	}
	if strings.EqualFold(u.Scheme, "https") {
		return 443, nil
	}
	return 80, nil
}

func checkScope(rc *Context, host string, ip netip.Addr) error {
	if rc.Scopes == nil || rc.Scopes.Empty() {
		return fmt.Errorf("本任务尚未登记任何授权范围，拒绝写入")
	}
	if ip.IsValid() {
		if rc.Scopes.AllowsIP(ip) {
			return nil
		}
	}
	if host != "" && rc.Scopes.AllowsHost(host) {
		return nil
	}
	return fmt.Errorf("范围闸门拒绝：%q 不在本任务授权范围内（%s）", firstNonEmptyStr(host, ip.String()), rc.Scopes.Describe())
}

// ---------- record_fact ----------

type factItem struct {
	Summary    string  `json:"summary"`
	Detail     string  `json:"detail"`
	Evidence   string  `json:"evidence"`
	Confidence string  `json:"confidence"`
	AssetIDs   []int64 `json:"asset_ids"`
}

type recordFactInput struct {
	Facts    []factItem `json:"facts"`
	IntentID int64      `json:"intent_id"`
}

// NewRecordFactTool 写探索结论（含否定结论）。
func NewRecordFactTool() *Tool {
	return &Tool{
		Name: "record_fact",
		Description: `把本次探索得到的【结论】写进探索图。
规则（很重要）：① 多个观察汇总成一条（summary 一句话总结 + detail 写细节），不要一个属性一条；
② 只写增量，别把已有事实换措辞重记；③ 必须给 evidence（一行：命令 + 最能证明的一两行输出）；
④ confidence：observed=直接看到，inferred=据现象推断；⑤ 否定结论也要写（"端口关闭""无回显"），
但只写"观察 + 试探性读法"，方向是否放弃由规划者定 —— 你只负责如实汇报。`,
		Schema: toolSpecJSON(`{
  "type": "object",
  "properties": {
    "facts": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "summary": {"type": "string", "description": "一句话结论"},
          "detail": {"type": "string", "description": "细节、数据、判断依据"},
          "evidence": {"type": "string", "description": "命令 + 关键输出"},
          "confidence": {"type": "string", "enum": ["observed","inferred"]},
          "asset_ids": {"type": "array", "items": {"type": "integer"}}
        },
        "required": ["summary", "evidence"]
      }
    },
    "intent_id": {"type": "integer", "description": "产出这条事实的意图 id"}
  },
  "required": ["facts"]
}`),
		Handle: handleRecordFact,
	}
}

func handleRecordFact(ctx context.Context, rc *Context, raw json.RawMessage) (Result, error) {
	var in recordFactInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return Errorf("参数解析失败：%v", err), nil
	}
	if len(in.Facts) == 0 {
		return Errorf("facts 不能为空；没有结论就不要调用本工具"), nil
	}
	intentID, err := resolveIntentID(rc, in.IntentID)
	if err != nil {
		return Errorf("%v", err), nil
	}
	type out struct {
		ID      int64  `json:"id,omitempty"`
		Summary string `json:"summary,omitempty"`
		Error   string `json:"error,omitempty"`
	}
	results := make([]out, 0, len(in.Facts))
	for _, f := range in.Facts {
		id, err := rc.Store.AddFact(ctx, store.AddFactInput{
			RunID: rc.RunID, IntentID: intentID,
			Summary: f.Summary, Detail: f.Detail, Evidence: f.Evidence,
			Confidence: f.Confidence, AssetIDs: f.AssetIDs,
		})
		if err != nil {
			results = append(results, out{Summary: f.Summary, Error: err.Error()})
			continue
		}
		results = append(results, out{ID: id, Summary: f.Summary})
	}
	return JSONResult(map[string]any{"results": results}), nil
}

// ---------- report_finding ----------

type findingInput struct {
	VulnClass string  `json:"vulnclass"`
	Severity  string  `json:"severity"`
	Name      string  `json:"name"`
	Summary   string  `json:"summary"`
	Evidence  string  `json:"evidence"`
	IntentID  int64   `json:"intent_id"`
	AssetIDs  []int64 `json:"asset_ids"`
}

// NewReportFindingTool 上报已确认漏洞。
func NewReportFindingTool() *Tool {
	return &Tool{
		Name: "report_finding",
		Description: `上报一条【已确认】漏洞。只有在本次真实触发、拿到可复现证据（请求/响应或命令输出）时才用。
严禁把"版本/指纹匹配到 CVE""参数看起来可注入""从更新日志推断"当已确认 —— 那只能 record_fact（inferred）。
触发不了但有嫌疑：用 record_fact 记一条 inferred 事实（嫌疑点 + 为何未触发）交给规划者。`,
		Schema: toolSpecJSON(`{
  "type": "object",
  "properties": {
    "vulnclass": {"type": "string", "description": "漏洞类别，如 sqli/xss/ssrf/info-leak"},
    "severity": {"type": "string", "enum": ["critical","high","medium","low"]},
    "name": {"type": "string", "description": "漏洞标题"},
    "summary": {"type": "string", "description": "一句话说明影响面"},
    "evidence": {"type": "string", "description": "可复现证据：完整请求 + 关键响应"},
    "intent_id": {"type": "integer"},
    "asset_ids": {"type": "array", "items": {"type": "integer"}}
  },
  "required": ["name", "summary", "evidence"]
}`),
		Handle: handleReportFinding,
	}
}

func handleReportFinding(ctx context.Context, rc *Context, raw json.RawMessage) (Result, error) {
	var in findingInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return Errorf("参数解析失败：%v", err), nil
	}
	if strings.TrimSpace(in.Evidence) == "" {
		return Errorf("evidence 必填：没有可复现证据只能 record_fact（confidence=inferred），不能 report_finding"), nil
	}
	intentID, err := resolveIntentID(rc, in.IntentID)
	if err != nil {
		return Errorf("%v", err), nil
	}
	id, err := rc.Store.AddFinding(ctx, store.AddFindingInput{
		RunID: rc.RunID, IntentID: intentID, Name: in.Name, Severity: in.Severity,
		VulnClass: in.VulnClass, Summary: in.Summary, Evidence: in.Evidence, AssetIDs: in.AssetIDs,
	})
	if err != nil {
		return Errorf("%v", err), nil
	}
	return JSONResult(map[string]any{
		"node_id":    id,
		"finding_id": id, // findings 是视图，两个 id 同源（见 docs/00-设计定稿.md）
		"status":     "confirmed",
	}), nil
}

// resolveIntentID 只允许写自己的意图：跨意图写入会污染别人的血缘（I20）。
func resolveIntentID(rc *Context, requested int64) (int64, error) {
	if requested == 0 {
		return rc.IntentID, nil
	}
	if rc.IntentID > 0 && requested != rc.IntentID {
		return 0, fmt.Errorf("intent_id #%d 不是你领到的意图（#%d）；只能写自己的意图", requested, rc.IntentID)
	}
	return requested, nil
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
