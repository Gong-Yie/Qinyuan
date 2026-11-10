package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"qinyuan/internal/domain"
)

// 出网防护的边界（学习.md 8.4）：
//   - 拒绝 loopback / 链路本地（含云元数据 169.254.169.254）/ 组播 / 未指定；
//   - 不拒绝 RFC1918 —— 内网自建服务是合法用法，这是刻意取舍，写在文档里；
//   - 拒绝跨主机重定向（凭据常在 URL 里），同主机允许。
var (
	httpClientsMu sync.Mutex
	httpClients   = map[bool]*http.Client{}
)

func httpClientFor(allowLoopback bool) *http.Client {
	httpClientsMu.Lock()
	defer httpClientsMu.Unlock()
	if c, ok := httpClients[allowLoopback]; ok {
		return c
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		Proxy:                 nil, // 不继承环境变量里的代理，避免把流量绕出去
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          20,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if err != nil {
				return nil, fmt.Errorf("解析 %s 失败：%w", host, err)
			}
			var lastErr error
			for _, ip := range ips {
				ip = ip.Unmap()
				if !dialableIP(ip, allowLoopback) {
					lastErr = fmt.Errorf("目标地址 %s 被拒绝（回环/链路本地/组播/未指定）", ip)
					continue
				}
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			if lastErr == nil {
				lastErr = fmt.Errorf("%s 没有可用地址", host)
			}
			return nil, lastErr
		},
	}
	c := &http.Client{
		Transport: tr,
		Timeout:   60 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("重定向超过 5 次")
			}
			if len(via) > 0 && !strings.EqualFold(via[0].URL.Hostname(), req.URL.Hostname()) {
				return fmt.Errorf("拒绝跨主机重定向：%s → %s（凭据可能随 URL 泄露）", via[0].URL.Hostname(), req.URL.Hostname())
			}
			return nil
		},
	}
	httpClients[allowLoopback] = c
	return c
}

// dialableIP 是拨号层的最后一道门。
func dialableIP(ip netip.Addr, allowLoopback bool) bool {
	if !ip.IsValid() || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false
	}
	if ip.IsLoopback() && !allowLoopback {
		return false
	}
	return true
}

type httpInput struct {
	Method    string            `json:"method"`
	URL       string            `json:"url"`
	Headers   map[string]string `json:"headers"`
	Body      string            `json:"body"`
	TimeoutMS int               `json:"timeout_ms"`
	// Full 要求把响应体按 MaxOutput 全量回给模型（默认只回开头一段 + 落盘指针）。
	Full bool `json:"full"`
}

type httpOutput struct {
	Status      int               `json:"status"`
	Headers     map[string]string `json:"headers"`
	Body        string            `json:"body"`
	BodyBytes   int64             `json:"body_bytes"`
	BodyInline  int               `json:"body_inline_limit"`
	DurationMS  int64             `json:"duration_ms"`
	FinalURL    string            `json:"final_url"`
	Truncated   bool              `json:"truncated"`
	RequestLine string            `json:"request_line"`
	// TrafficID 是这次交换在流量库里的 id（0 = 没录上）；report_finding 可以引用它当证据。
	TrafficID int64 `json:"traffic_id,omitempty"`
}

// httpInlineBodyBytes 是响应体默认回给模型的字节上限（完整内容落盘，带指针）。
//
// 为什么单独一档：一个 200KB 的 HTML 如果按 MaxOutput（30k 字符）回给模型，
// 一次调用就吃掉 1/5 个上下文窗口，而绝大多数判断只看得到前几 KB（状态码/标题/表单）。
const httpInlineBodyBytes = 4096

// NewHTTPTool 是第一个真实能力工具：发一个 HTTP 请求并把响应交回模型。
//
// 它同时承担三个职责：范围闸门（写入前）、出网防护（拨号层）、证据留痕（把完整
// 响应落盘，模型只看到 head + 指针）。
func NewHTTPTool() *Tool {
	return &Tool{
		Name: "http_request",
		Description: `对本任务授权范围内的目标发一个 HTTP 请求，返回状态码、响应头与响应体。
用途：验证页面/接口的实际行为（是否存在、返回什么、报什么错）。
规则：① 只对授权范围内主机发请求，范围外一律被拒绝；② 同一 URL 不要反复请求，先看已有事实；
③ 响应体默认只回前 4KB（完整内容落盘，路径写在 body 末尾），需要全文时传 full=true；
④ 不要用它做破坏性操作（写/删/爆破）。`,
		ReadOnly: false,
		Schema: toolSpecJSON(`{
  "type": "object",
  "properties": {
    "method": {"type": "string", "description": "HTTP 方法，默认 GET"},
    "url": {"type": "string", "description": "完整 URL（http/https）"},
    "headers": {"type": "object", "description": "请求头键值对", "additionalProperties": {"type": "string"}},
    "body": {"type": "string", "description": "请求体（POST/PUT 时用）"},
    "timeout_ms": {"type": "integer", "description": "超时毫秒，默认 20000，上限 60000"},
    "full": {"type": "boolean", "description": "要完整响应体（默认只回前 4KB + 落盘指针）"}
  },
  "required": ["url"]
}`),
		Handle: handleHTTPRequest,
	}
}

func handleHTTPRequest(ctx context.Context, rc *Context, raw json.RawMessage) (Result, error) {
	var in httpInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return Errorf("参数解析失败：%v", err), nil
	}
	if strings.TrimSpace(in.URL) == "" {
		return Errorf("url 必填"), nil
	}
	if rc.Scopes == nil || rc.Scopes.Empty() {
		return Errorf("本任务尚未登记任何授权范围（run_scope），拒绝出网。请先登记范围再探测。"), nil
	}
	if ok, why := rc.Scopes.AllowsURL(in.URL); !ok {
		return Errorf("范围闸门拒绝：%s。当前授权范围：%s", why, rc.Scopes.Describe()), nil
	}
	norm, err := domain.NormalizeURL(in.URL)
	if err != nil {
		return Errorf("URL 非法：%v", err), nil
	}
	method := strings.ToUpper(strings.TrimSpace(in.Method))
	if method == "" {
		method = http.MethodGet
	}
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead, http.MethodOptions:
	default:
		return Errorf("不支持的 HTTP 方法 %q", method), nil
	}

	timeout := 20 * time.Second
	if in.TimeoutMS > 0 {
		timeout = time.Duration(in.TimeoutMS) * time.Millisecond
		if timeout > 60*time.Second {
			timeout = 60 * time.Second
		}
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var bodyReader io.Reader
	if in.Body != "" {
		bodyReader = strings.NewReader(in.Body)
	}
	req, err := http.NewRequestWithContext(reqCtx, method, norm, bodyReader)
	if err != nil {
		return Errorf("构造请求失败：%v", err), nil
	}
	for k, v := range in.Headers {
		req.Header.Set(k, v)
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "qinyuan/0.1 (authorized security testing)")
	}

	start := time.Now()
	resp, err := httpClientFor(rc.AllowLoopback).Do(req)
	if err != nil {
		// 失败也是一次可复核的观察：录一条 status=0 + error（证据里能看到"打不通"）。
		recordTraffic(ctx, rc, trafficRecord{
			Method: method, URL: norm, ReqHeaders: in.Headers, ReqBody: []byte(in.Body),
			DurationMS: int(time.Since(start).Milliseconds()),
			Err:        err.Error(), ContentType: "error",
		})
		return Errorf("请求失败（%s %s）：%v", method, norm, err), nil
	}
	defer resp.Body.Close()
	elapsed := time.Since(start)

	// 最多读 4MB，避免一条响应把内存打爆。
	const maxRead = 4 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRead+1))
	if err != nil {
		return Errorf("读响应体失败：%v", err), nil
	}
	truncated := len(body) > maxRead
	if truncated {
		body = body[:maxRead]
	}

	headers := map[string]string{}
	for k := range resp.Header {
		headers[k] = resp.Header.Get(k)
	}
	text := string(body)
	inlineLimit := httpInlineBodyBytes
	if in.Full {
		inlineLimit = rc.MaxOutput
		if inlineLimit < httpInlineBodyBytes {
			inlineLimit = httpInlineBodyBytes
		}
	}
	out := httpOutput{
		Status:      resp.StatusCode,
		Headers:     headers,
		Body:        clampOutputN(rc, "http_body", text, inlineLimit),
		BodyBytes:   int64(len(body)),
		BodyInline:  inlineLimit,
		DurationMS:  elapsed.Milliseconds(),
		FinalURL:    resp.Request.URL.String(),
		Truncated:   truncated,
		RequestLine: method + " " + norm,
	}
	out.TrafficID = recordTraffic(ctx, rc, trafficRecord{
		Method: method, URL: out.FinalURL, Status: resp.StatusCode,
		ContentType: resp.Header.Get("Content-Type"),
		ReqHeaders:  in.Headers, RespHeaders: headers,
		ReqBody: []byte(in.Body), RespBody: body,
		DurationMS: int(elapsed.Milliseconds()),
	})
	return JSONResult(out), nil
}
