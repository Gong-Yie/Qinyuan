package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"qinyuan/internal/auth"
	"qinyuan/internal/testkit"
)

// M9 的端到端验收：走真实的 Handler（中间件 + 路由 + cookie），而不是只测中间件。
func TestServerAuthEndToEnd(t *testing.T) {
	st := testkit.Store(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(st, nil, log)
	srv.Auth = auth.New(auth.Options{
		Tokens: map[string]auth.Role{
			"viewer-token-123456": auth.RoleViewer,
			"admin-token-1234567": auth.RoleAdmin,
		},
		TTL: time.Hour,
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	do := func(method, path, bearer, csrf, body string) (int, string) {
		req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		if csrf != "" {
			req.Header.Set(auth.CSRFHeader, csrf)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}
	csrfFromJar := func() string {
		u, err := url.Parse(ts.URL)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range jar.Cookies(u) {
			if c.Name == "qy_csrf" {
				return c.Value
			}
		}
		return ""
	}

	// 1) 未认证：业务接口 401；健康检查仍公开。
	if code, _ := do("GET", "/api/runs", "", "", ""); code != 401 {
		t.Fatalf("未认证读应 401，实际 %d", code)
	}
	if code, _ := do("GET", "/health", "", "", ""); code != 200 {
		t.Fatalf("健康检查必须公开，实际 %d", code)
	}

	// 2) 错误令牌 → 401；正确令牌 → 200 并下发 cookie。
	if code, _ := do("POST", "/api/login", "", "", `{"token":"nope"}`); code != 401 {
		t.Fatalf("错误令牌应 401，实际 %d", code)
	}
	if code, body := do("POST", "/api/login", "", "", `{"token":"admin-token-1234567"}`); code != 200 {
		t.Fatalf("登录失败：%d %s", code, body)
	}
	if csrfFromJar() == "" {
		t.Fatal("登录后应下发 qy_csrf cookie（双提交要用）")
	}

	// 3) 带会话 cookie 的读：不需要 CSRF。
	if code, _ := do("GET", "/api/runs", "", "", ""); code != 200 {
		t.Fatalf("登录后读应 200，实际 %d", code)
	}

	// 4) 写请求缺 CSRF → 403；带上 → 200（admin 能改策略）。
	policy := `{"match_tool":"demo_tool","decision":"deny","note":"鉴权测试"}`
	if code, body := do("POST", "/api/approval-policies", "", "", policy); code != 403 {
		t.Fatalf("cookie 写请求缺 CSRF 应 403，实际 %d %s", code, body)
	}
	if code, body := do("POST", "/api/approval-policies", "", csrfFromJar(), policy); code != 200 {
		t.Fatalf("带 CSRF 的写应 200，实际 %d %s", code, body)
	}
	// 策略真的落库了。
	pols, err := st.ListApprovalPolicies(t.Context())
	if err != nil || len(pols) != 1 {
		t.Fatalf("策略未落库：%v %+v", err, pols)
	}

	// 5) Bearer：viewer 能读不能写；admin 能写。
	if code, _ := do("GET", "/api/runs", "viewer-token-123456", "", ""); code != 200 {
		t.Fatalf("viewer 读应 200，实际 %d", code)
	}
	if code, _ := do("POST", "/api/approval-policies", "viewer-token-123456", "", policy); code != 403 {
		t.Fatalf("viewer 改策略应 403，实际 %d", code)
	}
	if code, _ := do("DELETE", "/api/approval-policies/999999", "admin-token-1234567", "", ""); code != 404 {
		t.Fatalf("admin 删除不存在的策略应 404（而不是 403），实际 %d", code)
	}

	// 6) 登出后会话立即失效。
	if code, _ := do("POST", "/api/logout", "", csrfFromJar(), ""); code != 200 {
		t.Fatalf("登出应 200，实际 %d", code)
	}
	if code, _ := do("GET", "/api/runs", "", "", ""); code != 401 {
		t.Fatalf("登出后应 401，实际 %d", code)
	}
}

func TestServerAuthDisabledAllowsAll(t *testing.T) {
	st := testkit.Store(t)
	srv := New(st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/runs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("未启用鉴权时（回环开发态）读应 200，实际 %d", resp.StatusCode)
	}
	var me map[string]any
	r2, err := http.Get(ts.URL + "/api/me")
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	if err := json.NewDecoder(r2.Body).Decode(&me); err != nil {
		t.Fatal(err)
	}
	if me["auth"] != false {
		t.Fatalf("未启用时 /api/me 应回 auth=false：%+v", me)
	}
}
