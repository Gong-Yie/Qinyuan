package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseTokens(t *testing.T) {
	toks, err := ParseTokens("v-token-1234567890:viewer,o-token-1234567890:operator,a-token-1234567890:admin")
	if err != nil {
		t.Fatal(err)
	}
	if len(toks) != 3 || toks["v-token-1234567890"] != RoleViewer || toks["a-token-1234567890"] != RoleAdmin {
		t.Fatalf("解析结果不对：%+v", toks)
	}
	// 不带角色 = admin（单令牌的老用法）。
	toks, err = ParseTokens("only-token-1234567890")
	if err != nil || toks["only-token-1234567890"] != RoleAdmin {
		t.Fatalf("单令牌应为 admin：%+v err=%v", toks, err)
	}
	// 配置错误必须在启动时炸，而不是静默少一个管理员。
	for _, bad := range []string{"", "short:admin", "tok-1234567890:bogus", "tok-1234567890:admin,tok-1234567890:viewer"} {
		if _, err := ParseTokens(bad); err == nil {
			t.Fatalf("非法令牌规格 %q 应报错", bad)
		}
	}
}

func TestRoleOrdering(t *testing.T) {
	if !RoleAdmin.Allows(RoleOperator) || !RoleOperator.Allows(RoleViewer) {
		t.Fatal("角色应当满足偏序 admin > operator > viewer")
	}
	if RoleViewer.Allows(RoleOperator) || RoleOperator.Allows(RoleAdmin) {
		t.Fatal("低角色不能顶替高角色")
	}
	if RequiredRole("GET", "/api/runs") != RoleViewer {
		t.Fatal("读接口应为 viewer")
	}
	if RequiredRole("POST", "/api/runs/1/control") != RoleOperator {
		t.Fatal("任务控制应为 operator")
	}
	if RequiredRole("POST", "/api/approval-policies") != RoleAdmin {
		t.Fatal("改策略（能放宽闸门）必须是 admin")
	}
	if RequiredRole("GET", "/api/approval-policies") != RoleViewer {
		t.Fatal("读策略应为 viewer")
	}
	if !IsPublicPath("/health") || !IsPublicPath("/app.js") || IsPublicPath("/api/runs") {
		t.Fatal("公开路径判断不对")
	}
}

func TestSessionExpiryAndSliding(t *testing.T) {
	a := New(Options{Tokens: map[string]Role{"tok-1234567890ab": RoleAdmin}, TTL: time.Hour})
	now := time.Now()
	a.now = func() time.Time { return now }

	id, csrf, _ := a.CreateSession(RoleAdmin)
	if s, ok := a.Lookup(id); !ok || s.Role != RoleAdmin || s.CSRF != csrf {
		t.Fatal("会话应当可读回")
	}
	// 半小时后仍有效，且被续期。
	now = now.Add(30 * time.Minute)
	if _, ok := a.Lookup(id); !ok {
		t.Fatal("滑动续期前应当有效")
	}
	// 再过 50 分钟（续期后仍在窗口内：t0+90m 到期）。
	now = now.Add(50 * time.Minute)
	if _, ok := a.Lookup(id); !ok {
		t.Fatal("续期后应当仍然有效")
	}
	// 绝对上限 24h：一直用也不能活过它。
	now = now.Add(25 * time.Hour)
	if _, ok := a.Lookup(id); ok {
		t.Fatal("超过绝对上限必须失效")
	}
	// 注销立即失效。
	id2, _, _ := a.CreateSession(RoleAdmin)
	a.DeleteSession(id2)
	if _, ok := a.Lookup(id2); ok {
		t.Fatal("注销后不该还能用")
	}
}

// 中间件：认证、角色、CSRF 三道门。
func TestMiddlewareAuthRoleCSRF(t *testing.T) {
	tokens := map[string]Role{
		"viewer-token-123456":   RoleViewer,
		"operator-token-123456": RoleOperator,
		"admin-token-1234567":   RoleAdmin,
	}
	a := New(Options{Tokens: tokens, TTL: time.Hour})
	var gotRole Role
	h := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRole = RoleFrom(r.Context())
		w.WriteHeader(200)
	}))

	do := func(method, path, bearer, cookie, csrfCookieVal, csrfHeader string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.RemoteAddr = "127.0.0.1:5555"
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
			// 双提交：cookie 里的 csrf 与请求头必须一致。
			req.AddCookie(&http.Cookie{Name: csrfCookie, Value: csrfCookieVal})
		}
		if csrfHeader != "" {
			req.Header.Set(CSRFHeader, csrfHeader)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	if rec := do("GET", "/api/runs", "", "", "", ""); rec.Code != 401 {
		t.Fatalf("无凭据应 401，实际 %d", rec.Code)
	}
	if rec := do("GET", "/api/runs", "wrong-token", "", "", ""); rec.Code != 401 {
		t.Fatalf("错令牌应 401，实际 %d", rec.Code)
	}
	if rec := do("GET", "/health", "", "", "", ""); rec.Code != 200 {
		t.Fatalf("健康检查必须公开，实际 %d", rec.Code)
	}
	if rec := do("GET", "/api/runs", "viewer-token-123456", "", "", ""); rec.Code != 200 || gotRole != RoleViewer {
		t.Fatalf("viewer 读应放行：%d role=%v", rec.Code, gotRole)
	}
	if rec := do("POST", "/api/runs/1/control", "viewer-token-123456", "", "", ""); rec.Code != 403 {
		t.Fatalf("viewer 写应 403，实际 %d", rec.Code)
	}
	if rec := do("POST", "/api/runs/1/control", "operator-token-123456", "", "", ""); rec.Code != 200 {
		t.Fatalf("operator 写应放行，实际 %d", rec.Code)
	}
	if rec := do("POST", "/api/approval-policies", "operator-token-123456", "", "", ""); rec.Code != 403 {
		t.Fatalf("改策略必须是 admin，operator 应 403（实际 %d）", rec.Code)
	}
	if rec := do("POST", "/api/approval-policies", "admin-token-1234567", "", "", ""); rec.Code != 200 {
		t.Fatalf("admin 改策略应放行，实际 %d", rec.Code)
	}

	// Cookie 会话：缺 CSRF 的写请求必须 403；带上正确值才放行。
	id, csrf, _ := a.CreateSession(RoleOperator)
	if rec := do("POST", "/api/runs/1/control", "", id, csrf, ""); rec.Code != 403 {
		t.Fatalf("cookie 写请求缺 CSRF 应 403，实际 %d", rec.Code)
	}
	if rec := do("POST", "/api/runs/1/control", "", id, csrf, "bad"); rec.Code != 403 {
		t.Fatalf("CSRF 不匹配应 403，实际 %d", rec.Code)
	}
	if rec := do("POST", "/api/runs/1/control", "", id, csrf, csrf); rec.Code != 200 {
		t.Fatalf("CSRF 正确应放行，实际 %d", rec.Code)
	}
	// GET 不需要 CSRF。
	if rec := do("GET", "/api/runs", "", id, csrf, ""); rec.Code != 200 {
		t.Fatalf("cookie 读不需要 CSRF，实际 %d", rec.Code)
	}
}

// 登录失败限速：5 次之后 429，且成功登录会清零计数。
func TestLoginRateLimit(t *testing.T) {
	a := New(Options{Tokens: map[string]Role{"tok-1234567890ab": RoleAdmin}})
	login := func(token string) int {
		req := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"token":"`+token+`"}`))
		req.RemoteAddr = "10.0.0.1:7777"
		rec := httptest.NewRecorder()
		a.Login(rec, req)
		return rec.Code
	}
	for i := 0; i < maxLoginFails; i++ {
		if code := login("wrong"); code != 401 {
			t.Fatalf("第 %d 次失败应 401，实际 %d", i+1, code)
		}
	}
	if code := login("wrong"); code != 429 {
		t.Fatalf("超过上限应 429，实际 %d", code)
	}
	// 正确的令牌也要等窗口过去（防"先爆破再试对的"）。
	if code := login("tok-1234567890ab"); code != 429 {
		t.Fatalf("限速期间正确令牌也应 429，实际 %d", code)
	}
}

// 正确登录：下发两个 cookie，且会话可读。
func TestLoginSetsCookies(t *testing.T) {
	a := New(Options{Tokens: map[string]Role{"tok-1234567890ab": RoleOperator}, TTL: time.Hour})
	req := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"token":"tok-1234567890ab"}`))
	req.RemoteAddr = "127.0.0.1:1"
	rec := httptest.NewRecorder()
	a.Login(rec, req)
	if rec.Code != 200 {
		t.Fatalf("登录失败：%d %s", rec.Code, rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	seen := map[string]*http.Cookie{}
	for _, c := range cookies {
		seen[c.Name] = c
	}
	sess, csrf := seen[sessionCookie], seen[csrfCookie]
	if sess == nil || !sess.HttpOnly || csrf == nil || csrf.HttpOnly {
		t.Fatalf("cookie 属性不对：session=%+v csrf=%+v", sess, csrf)
	}
	if sess.SameSite != http.SameSiteStrictMode || csrf.SameSite != http.SameSiteStrictMode {
		t.Fatal("会话与 CSRF cookie 都必须是 SameSite=Strict")
	}
	if s, ok := a.Lookup(sess.Value); !ok || s.CSRF != csrf.Value || s.Role != RoleOperator {
		t.Fatalf("会话内容不对：%+v ok=%v", s, ok)
	}
}
