package runplan

import "testing"

// P0：入口 URL 必须留下（含端口/路径）。范围只回答"允许打哪"，不回答"从哪开始"。
// 没有它，worker 只能猜端口 —— 真实 run 里 49% 的请求打在 80/443/3000/8000 上。
func TestBuildKeepsSeedURL(t *testing.T) {
	in, err := Build(Request{Goal: "确认服务指纹", Target: "http://127.0.0.1:8080/login?next=%2F"})
	if err != nil {
		t.Fatalf("Build 失败：%v", err)
	}
	if len(in.Seeds) != 1 || in.Seeds[0] != "http://127.0.0.1:8080/login?next=%2F" {
		t.Fatalf("Seeds = %#v，期望保留带端口的完整 URL", in.Seeds)
	}
	// 范围仍然是"最小范围"：IP 字面量按 ip 登记，不会因为带了 URL 就放宽成根域。
	if len(in.Scopes) != 1 || in.Scopes[0].Kind != "ip" || in.Scopes[0].Value != "127.0.0.1" {
		t.Fatalf("Scopes = %#v，期望只有 ip 127.0.0.1", in.Scopes)
	}
	if len(in.SeedEntities) != 1 || in.SeedEntities[0].Kind != "ip" {
		t.Fatalf("SeedEntities = %#v，期望只有 ip 实体", in.SeedEntities)
	}
}

// 默认端口要归一化掉，否则 seeds 里会同时出现 :80 与不带端口的两种写法。
func TestBuildNormalizesSeedURL(t *testing.T) {
	in, err := Build(Request{Goal: "指纹", Target: "HTTP://Example.TEST:80/"})
	if err != nil {
		t.Fatalf("Build 失败：%v", err)
	}
	if len(in.Seeds) != 1 || in.Seeds[0] != "http://example.test/" {
		t.Fatalf("Seeds = %#v，期望 http://example.test/", in.Seeds)
	}
}

// 没有 URL 的输入不该凭空造出一个入口。
func TestBuildWithoutURLHasNoSeeds(t *testing.T) {
	in, err := Build(Request{Goal: "指纹", Target: "example.test"})
	if err != nil {
		t.Fatalf("Build 失败：%v", err)
	}
	if len(in.Seeds) != 0 {
		t.Fatalf("Seeds = %#v，期望为空（裸域名不猜协议）", in.Seeds)
	}
}
