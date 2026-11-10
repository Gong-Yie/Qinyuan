// Package auth 是钦原的 HTTP 鉴权层：Bearer 令牌 + 会话 Cookie + CSRF + 登录限速。
//
// 威胁模型（写清楚才知道防护到哪）：
//   - 这个控制台能**暂停任务、审批危险操作、导出带攻击脚本的报告**，
//     所以它不能是"裸端口"：默认只监听回环，一旦监听非回环就必须有鉴权（启动时强校验）；
//   - 假设浏览器是干净的：靠 HttpOnly + SameSite=Strict 挡 XSS 偷 cookie / 跨站请求；
//   - 假设令牌可能会泄露：所以有角色（viewer < operator < admin）与可撤销的进程内会话。
//
// 三个刻意的取舍：
//  1. **令牌只存 sha256**：内存里也不留明文；比对走哈希查表（对高熵令牌足够）；
//  2. **会话在内存**：重启即失效 = 天然的"换人/撤销"语义；代价是重启后要重新登录；
//  3. **CSRF 用双提交**：`qy_csrf` cookie 可读 + `X-CSRF-Token` 请求头必须一致；
//     Bearer 令牌调用不受 CSRF 约束（它本来就不是浏览器自动携带的凭据）。
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Role 是角色，数值越大权限越高。
type Role int

const (
	RoleNone Role = iota
	RoleViewer
	RoleOperator
	RoleAdmin
)

func (r Role) String() string {
	switch r {
	case RoleViewer:
		return "viewer"
	case RoleOperator:
		return "operator"
	case RoleAdmin:
		return "admin"
	}
	return "none"
}

// ParseRole 解析角色名。
func ParseRole(s string) (Role, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "viewer", "read", "readonly":
		return RoleViewer, true
	case "operator", "ops", "write":
		return RoleOperator, true
	case "admin":
		return RoleAdmin, true
	}
	return RoleNone, false
}

// Allows 判断当前角色是否满足所需角色。
func (r Role) Allows(need Role) bool { return r >= need }

// ParseTokens 解析令牌规格：`token:role,token2:viewer`；不带角色的令牌默认 admin。
//
// 空令牌、空角色、重复令牌都直接报错 —— 配置错误应当在启动时炸，而不是静默少一个管理员。
func ParseTokens(spec string) (map[string]Role, error) {
	out := map[string]Role{}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		tok, roleStr, hasRole := strings.Cut(part, ":")
		tok = strings.TrimSpace(tok)
		if tok == "" {
			return nil, fmt.Errorf("令牌为空：%q", part)
		}
		role := RoleAdmin
		if hasRole {
			r, ok := ParseRole(roleStr)
			if !ok {
				return nil, fmt.Errorf("未知角色 %q（viewer/operator/admin）", roleStr)
			}
			role = r
		}
		if _, dup := out[tok]; dup {
			return nil, fmt.Errorf("令牌重复：%q", tok)
		}
		if len(tok) < 16 {
			return nil, fmt.Errorf("令牌太短（至少 16 字符）：%q…", tok[:min(4, len(tok))])
		}
		out[tok] = role
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("没有可用令牌")
	}
	return out, nil
}

// Session 是一次登录会话。
type Session struct {
	Role      Role
	CSRF      string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// Authenticator 持有令牌、会话与登录限速状态。
type Authenticator struct {
	// Enabled=false 时中间件是空操作（仅允许在回环监听时出现）。
	Enabled bool

	tokens       map[[32]byte]Role
	mu           sync.Mutex
	sessions     map[string]*Session
	loginFails   map[string]*failWindow
	ttl          time.Duration
	cookieSecure bool
	log          func(format string, args ...any)

	now func() time.Time // 测试可注入
}

// Options 构造参数。
type Options struct {
	Tokens       map[string]Role
	TTL          time.Duration
	CookieSecure bool
	Logger       func(format string, args ...any)
}

// New 构造鉴权器。
func New(opts Options) *Authenticator {
	if opts.TTL <= 0 {
		opts.TTL = 12 * time.Hour
	}
	if opts.Logger == nil {
		opts.Logger = func(string, ...any) {}
	}
	a := &Authenticator{
		Enabled:      len(opts.Tokens) > 0,
		tokens:       map[[32]byte]Role{},
		sessions:     map[string]*Session{},
		loginFails:   map[string]*failWindow{},
		ttl:          opts.TTL,
		cookieSecure: opts.CookieSecure,
		log:          opts.Logger,
		now:          time.Now,
	}
	for tok, role := range opts.Tokens {
		a.tokens[sha256.Sum256([]byte(tok))] = role
	}
	return a
}

// Disabled 返回一个空操作的鉴权器（回环开发态）。
func Disabled() *Authenticator {
	return &Authenticator{sessions: map[string]*Session{}, loginFails: map[string]*failWindow{}, ttl: time.Hour, now: time.Now}
}

const (
	sessionCookie = "qy_session"
	csrfCookie    = "qy_csrf"
	// CSRFHeader 是双提交校验用的请求头名（控制台会自动带上）。
	CSRFHeader = "X-CSRF-Token"
)

// roleForToken 用哈希查表判断令牌角色。
func (a *Authenticator) roleForToken(token string) (Role, bool) {
	sum := sha256.Sum256([]byte(token))
	role, ok := a.tokens[sum]
	return role, ok
}

// ---------- 会话 ----------

// CreateSession 建一个会话（登录成功后调用）。
func (a *Authenticator) CreateSession(role Role) (id, csrf string, expires time.Time) {
	id = randHex(32)
	csrf = randHex(16)
	expires = a.now().Add(a.ttl)
	a.mu.Lock()
	a.sessions[id] = &Session{Role: role, CSRF: csrf, CreatedAt: a.now(), ExpiresAt: expires}
	// 顺手清过期的：会话表很小，O(n) 足够。
	for k, s := range a.sessions {
		if !s.ExpiresAt.After(a.now()) {
			delete(a.sessions, k)
		}
	}
	a.mu.Unlock()
	return id, csrf, expires
}

// Lookup 读会话（滑动续期：用一次就延到 now+ttl，但不超过原来的绝对上限 24h）。
func (a *Authenticator) Lookup(id string) (*Session, bool) {
	if id == "" {
		return nil, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[id]
	if !ok {
		return nil, false
	}
	if !s.ExpiresAt.After(a.now()) {
		delete(a.sessions, id)
		return nil, false
	}
	// 滑动续期，但有绝对上限：连续使用也不可能让会话活过 24 小时（防长期驻留）。
	next := a.now().Add(a.ttl)
	if cap := s.CreatedAt.Add(24 * time.Hour); next.After(cap) {
		next = cap
	}
	if next.After(s.ExpiresAt) {
		s.ExpiresAt = next
	}
	cp := *s
	return &cp, true
}

// DeleteSession 注销。
func (a *Authenticator) DeleteSession(id string) {
	a.mu.Lock()
	delete(a.sessions, id)
	a.mu.Unlock()
}

// ---------- 登录限速 ----------

type failWindow struct {
	count int
	reset time.Time
}

const (
	maxLoginFails = 5
	failWindowDur = 5 * time.Minute
)

// allowLogin 判断这个 IP 还能不能试；返回 (ok, retryAfter)。
func (a *Authenticator) allowLogin(ip string) (bool, time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	w, ok := a.loginFails[ip]
	if !ok || !w.reset.After(a.now()) {
		return true, 0
	}
	if w.count >= maxLoginFails {
		return false, w.reset.Sub(a.now())
	}
	return true, 0
}

func (a *Authenticator) noteLoginFail(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	w, ok := a.loginFails[ip]
	if !ok || !w.reset.After(a.now()) {
		a.loginFails[ip] = &failWindow{count: 1, reset: a.now().Add(failWindowDur)}
		return
	}
	w.count++
}

func (a *Authenticator) clearLoginFails(ip string) {
	a.mu.Lock()
	delete(a.loginFails, ip)
	a.mu.Unlock()
}

// ---------- HTTP ----------

// Login 处理 POST /api/login {token}：成功则下发会话 cookie。
func (a *Authenticator) Login(w http.ResponseWriter, r *http.Request) {
	if !a.Enabled {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "auth": false, "role": "admin"})
		return
	}
	ip := clientIP(r)
	if ok, retry := a.allowLogin(ip); !ok {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", int(retry.Seconds())+1))
		writeJSON(w, http.StatusTooManyRequests, map[string]string{
			"error": "登录尝试过于频繁，请稍后再试",
		})
		return
	}
	var in struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体不是合法 JSON"})
		return
	}
	role, ok := a.roleForToken(strings.TrimSpace(in.Token))
	if !ok {
		a.noteLoginFail(ip)
		a.log("[auth] 登录失败 ip=%s", ip)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "令牌无效"})
		return
	}
	a.clearLoginFails(ip)
	id, csrf, expires := a.CreateSession(role)
	a.setCookies(w, id, csrf, expires)
	a.log("[auth] 登录成功 ip=%s role=%s", ip, role)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "auth": true, "role": role.String(), "expires_at": expires.Unix(),
	})
}

func (a *Authenticator) setCookies(w http.ResponseWriter, id, csrf string, expires time.Time) {
	maxAge := int(time.Until(expires).Seconds())
	if maxAge <= 0 {
		maxAge = 1
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: id, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: a.cookieSecure, MaxAge: maxAge,
	})
	// CSRF cookie 故意可读：前端要把它放进请求头（双提交）。
	http.SetCookie(w, &http.Cookie{
		Name: csrfCookie, Value: csrf, Path: "/",
		SameSite: http.SameSiteStrictMode, Secure: a.cookieSecure, MaxAge: maxAge,
	})
}

// Logout 处理 POST /api/logout：删会话 + 清 cookie。
func (a *Authenticator) Logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		a.DeleteSession(c.Value)
	}
	clear := func(name string) {
		http.SetCookie(w, &http.Cookie{
			Name: name, Value: "", Path: "/", MaxAge: -1, HttpOnly: name == sessionCookie,
			SameSite: http.SameSiteStrictMode, Secure: a.cookieSecure,
		})
	}
	clear(sessionCookie)
	clear(csrfCookie)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// Me 返回当前身份（未登录由中间件挡住，走到这里一定有身份）。
func (a *Authenticator) Me(w http.ResponseWriter, r *http.Request) {
	role := RoleFrom(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"auth": a.Enabled, "role": role.String(),
	})
}

type ctxKey struct{}

// RoleFrom 读中间件放进 ctx 的角色。
func RoleFrom(ctx context.Context) Role {
	r, _ := ctx.Value(ctxKey{}).(Role)
	return r
}

// WithRole 只给测试/内部调用用。
func WithRole(ctx context.Context, r Role) context.Context {
	return context.WithValue(ctx, ctxKey{}, r)
}

// Middleware 是鉴权主入口。
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.Enabled || IsPublicPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		need := RequiredRole(r.Method, r.URL.Path)

		role, viaCookie, ok := a.identify(r)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error": "未认证：请携带 Bearer 令牌，或先在控制台登录",
			})
			return
		}
		if !role.Allows(need) {
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error": fmt.Sprintf("权限不足：该接口需要 %s，当前是 %s", need, role),
			})
			return
		}
		// 只有"浏览器自动携带的凭据"（cookie）才需要 CSRF；Bearer 不需要。
		if viaCookie && isUnsafe(r.Method) {
			csrf, err := r.Cookie(csrfCookie)
			if err != nil || csrf.Value == "" ||
				subtle.ConstantTimeCompare([]byte(csrf.Value), []byte(r.Header.Get(CSRFHeader))) != 1 {
				writeJSON(w, http.StatusForbidden, map[string]string{
					"error": "CSRF 校验失败：请从 " + csrfCookie + " cookie 取值并放进 " + CSRFHeader + " 请求头",
				})
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, role)))
	})
}

// identify 先看 Bearer，再看会话 cookie。
func (a *Authenticator) identify(r *http.Request) (Role, bool, bool) {
	if h := r.Header.Get("Authorization"); h != "" {
		tok, ok := strings.CutPrefix(h, "Bearer ")
		if ok {
			if role, found := a.roleForToken(strings.TrimSpace(tok)); found {
				return role, false, true
			}
			return RoleNone, false, false
		}
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		if s, ok := a.Lookup(c.Value); ok {
			return s.Role, true, true
		}
	}
	return RoleNone, false, false
}

// ---------- 路径与角色 ----------

// IsPublicPath 判断是否免认证。
//
// 公开的只有：健康检查、登录/登出、控制台静态资源（否则登录页本身都加载不出来）。
func IsPublicPath(path string) bool {
	switch path {
	case "/", "/index.html", "/app.js", "/graph.js", "/styles.css", "/favicon.ico",
		"/health", "/api/login", "/api/logout":
		return true
	}
	return false
}

// RequiredRole 是"方法 + 路径 → 所需角色"的唯一实现。
//
// 规则（越靠前越优先）：
//   - 改策略/规则（预授权、拦截规则）→ admin：它们能放宽闸门，等于改安全边界；
//   - 其余读请求 → viewer；
//   - 其余写请求 → operator（任务生命周期、审批、重放、CI 导出）。
func RequiredRole(method, path string) Role {
	if strings.HasPrefix(path, "/api/approval-policies") || strings.HasPrefix(path, "/api/intercept/rules") {
		if method == http.MethodGet || method == http.MethodHead {
			return RoleViewer
		}
		return RoleAdmin
	}
	if method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions {
		return RoleViewer
	}
	return RoleOperator
}

func isUnsafe(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

// clientIP 取客户端 IP（不信任 X-Forwarded-For：本服务默认直连，反代场景另行配置）。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func randHex(nBytes int) string {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		// 随机源不可用：宁可拒绝登录，也不要发一个弱会话 id。
		panic("auth: 随机源不可用：" + err.Error())
	}
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
