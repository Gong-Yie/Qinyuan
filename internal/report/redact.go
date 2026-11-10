package report

import (
	"regexp"
	"strings"
)

// Redactor 做两件事（P3-2 的"默认导出脱敏版"）：
//
//  1. 把目标主机/URL 替换成 <target>：报告外流时不该带着客户域名；
//  2. 把凭据参数打码：password/token/cookie 这类值一旦进报告就是事故。
//
// 它是**保守的**：宁可多打码，也不要漏。完整版必须显式 full=true（调用方写审计）。
type Redactor struct {
	hosts []string
}

// NewRedactor 从一堆"目标字符串"（URL/域名/IP/seed）里提取主机名。
func NewRedactor(targets []string) *Redactor {
	seen := map[string]bool{}
	var hosts []string
	for _, t := range targets {
		h := hostOf(t)
		if h == "" || len(h) < 3 || seen[h] {
			continue
		}
		seen[h] = true
		hosts = append(hosts, h)
	}
	// 长的先替换：先换 api.example.com 再换 example.com，避免留下 "api.<target>"。
	sortByLenDesc(hosts)
	return &Redactor{hosts: hosts}
}

// 常见凭据参数：值一律打码（保留键名，读报告的人才知道"这里本该是什么"）。
var secretParam = regexp.MustCompile(`(?i)\b(password|passwd|pwd|pass|token|access_token|refresh_token|secret|api[_-]?key|apikey|authorization|cookie|session|sessionid|csrf|xsrf|sig|signature)=([^&\s'"<>]{1,512})`)

// Authorization / Cookie 头。
var secretHeader = regexp.MustCompile(`(?i)\b(authorization|cookie|set-cookie|x-api-key)\s*:\s*([^\r\n]{1,512})`)

// URL 里的 userinfo（http://user:pass@host/）。
var userInfo = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^/\s@]+@`)

// Text 脱敏一段文本。
func (r *Redactor) Text(s string) string {
	if s == "" {
		return s
	}
	if r != nil {
		for _, h := range r.hosts {
			s = replaceHost(s, h)
		}
	}
	s = secretParam.ReplaceAllString(s, "$1=***")
	s = secretHeader.ReplaceAllString(s, "$1: ***")
	s = userInfo.ReplaceAllString(s, "$1")
	return s
}

// replaceHost 把主机名替换成 <target>，但不要误伤更长的域名。
//
// 与 P2-3 的 outbox 判据同理：先处理长串（NewRedactor 已排序），
// 再检查边界（前后不是字母数字或 . -），防止 example.com 出现在
// notexample.com.cn 里被打坏。
func replaceHost(s, host string) string {
	var b strings.Builder
	i := 0
	for {
		j := strings.Index(strings.ToLower(s[i:]), strings.ToLower(host))
		if j < 0 {
			b.WriteString(s[i:])
			break
		}
		start := i + j
		end := start + len(host)
		if start > 0 && isHostChar(s[start-1]) {
			b.WriteString(s[i:end])
			i = end
			continue
		}
		if end < len(s) && isHostChar(s[end]) {
			b.WriteString(s[i:end])
			i = end
			continue
		}
		b.WriteString(s[i:start])
		b.WriteString("<target>")
		i = end
	}
	return b.String()
}

func isHostChar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c == '-' || c == '_' || c == '.':
		return true
	}
	return false
}

// hostOf 从 URL/域名/IP 里取出主机名（去端口、去路径、去 userinfo）。
func hostOf(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndex(s, "@"); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimSuffix(s, ".")
	// host:port（IPv6 字面量少见，报告里不强求）。
	if i := strings.LastIndex(s, ":"); i > 0 && !strings.Contains(s[i+1:], ":") {
		if _, ok := atoiSafe(s[i+1:]); ok {
			s = s[:i]
		}
	}
	return strings.ToLower(s)
}

func atoiSafe(s string) (int, bool) {
	n := 0
	if s == "" {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, true
}

func sortByLenDesc(ss []string) {
	for i := 1; i < len(ss); i++ {
		for j := i; j > 0 && len(ss[j]) > len(ss[j-1]); j-- {
			ss[j], ss[j-1] = ss[j-1], ss[j]
		}
	}
}
