package config

import "testing"

// M9：监听非回环却没有鉴权 = 拒绝启动。这条不能只写在文档里。
func TestValidateServe(t *testing.T) {
	cases := []struct {
		name    string
		addr    string
		mode    string
		spec    string
		wantErr bool
	}{
		{"回环 + 无令牌", "127.0.0.1:8787", "auto", "", false},
		{"回环 + localhost", "localhost:8787", "auto", "", false},
		{"对外 + 无令牌", "0.0.0.0:8787", "auto", "", true},
		{"对外 + 空 host", ":8787", "auto", "", true},
		{"对外 + 有令牌", "0.0.0.0:8787", "auto", "tok-1234567890ab", false},
		{"强制开但没令牌", "127.0.0.1:8787", "on", "", true},
		{"强制关 + 对外", "0.0.0.0:8787", "off", "tok-1234567890ab", true},
	}
	for _, c := range cases {
		cfg := &Config{Addr: c.addr, Auth: AuthConfig{Mode: c.mode, TokenSpec: c.spec}}
		err := cfg.ValidateServe()
		if (err != nil) != c.wantErr {
			t.Fatalf("%s：addr=%q mode=%q spec=%q → err=%v（期望 err=%v）",
				c.name, c.addr, c.mode, c.spec, err, c.wantErr)
		}
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8787":  true,
		"127.0.0.2:1":     true,
		"[::1]:8787":      true,
		"localhost:8787":  true,
		"10.0.0.5:8787":   false,
		"0.0.0.0:8787":    false,
		":8787":           false,
		"example.com:80":  false,
	} {
		if got := IsLoopbackAddr(addr); got != want {
			t.Fatalf("IsLoopbackAddr(%q) = %v，期望 %v", addr, got, want)
		}
	}
}
