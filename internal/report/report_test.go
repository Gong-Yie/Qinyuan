package report

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"qinyuan/internal/model"
	"qinyuan/internal/store"
	"qinyuan/internal/testkit"
)

// 脱敏：目标主机替换、凭据打码、边界不误伤。
func TestRedactorHostAndSecrets(t *testing.T) {
	red := NewRedactor([]string{"https://api.example.com:8443/login?x=1", "example.com", "10.1.2.3"})

	in := `GET https://api.example.com/login?password=Hunter2&next=/a HTTP/1.1
Host: api.example.com
Authorization: Bearer sk-live-abcdef
Cookie: sid=abc123
还有 notexample.com 与 example.com.cn 不该被误伤；内网 10.1.2.3 也应打码。
数据库串 postgres://user:pw@10.1.2.3:5432/db`
	out := red.Text(in)

	for _, bad := range []string{"Hunter2", "sk-live-abcdef", "sid=abc123", "api.example.com", "10.1.2.3"} {
		if strings.Contains(out, bad) {
			t.Fatalf("脱敏后仍包含 %q：\n%s", bad, out)
		}
	}
	if !strings.Contains(out, "<target>") {
		t.Fatalf("目标主机应替换成 <target>：\n%s", out)
	}
	if !strings.Contains(out, "notexample.com") || !strings.Contains(out, "example.com.cn") {
		t.Fatalf("边界判断误伤：\n%s", out)
	}
	if !strings.Contains(out, "password=***") {
		t.Fatalf("凭据参数应保留键名并打码：\n%s", out)
	}
}

// 报告分区 + 脱敏版不泄露目标 + zip 自包含。
func TestReportPartitionsAndZip(t *testing.T) {
	st := testkit.Store(t)
	ctx := context.Background()

	r, err := st.CreateRun(ctx, store.CreateRunInput{
		Goal: "确认 api.example.com 的登录入口", WorkerCount: 1,
		Scopes: []model.Scope{{Kind: "root_domain", Domain: "example.com", Source: "manual"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ent, _, err := st.UpsertEntity(ctx, r.ID, store.EntityInput{
		Kind: "subdomain", Domain: "api.example.com", RootDomain: "example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	confirmedID, err := st.AddFinding(ctx, store.AddFindingInput{
		RunID: r.ID, Name: "反射型 XSS", Severity: "high", VulnClass: "xss",
		Summary:  "搜索参数 q 原样回显",
		Evidence: "GET https://api.example.com/search?q=<script> → 200 且原样回显",
		AssetIDs: []int64{ent},
	})
	if err != nil {
		t.Fatal(err)
	}
	manualID, err := st.AddFinding(ctx, store.AddFindingInput{
		RunID: r.ID, Name: "越权读取", Severity: "critical",
		Summary:  "普通用户可读他人订单",
		Evidence: "GET https://api.example.com/order/1001 → 200 且返回别人的订单号",
		AssetIDs: []int64{ent},
	})
	if err != nil {
		t.Fatal(err)
	}
	// 给第一条绑一条流量并做一次 verified 复测。
	exID, err := st.RecordTraffic(ctx, store.TrafficInput{
		RunID: r.ID, Method: "GET",
		URL: "https://api.example.com/search?q=%3Cscript%3E", Status: 200,
		RespBody: "<script>alert(1)</script>",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.BindFindingTraffic(ctx, r.ID, confirmedID, []int64{exID}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddRetest(ctx, store.Retest{
		RunID: r.ID, FindingID: confirmedID, ExchangeID: exID, Status: "verified",
		Expected: "200 + sha 一致", Observed: "200，响应 sha 完全一致", CreatedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatal(err)
	}

	b := &Builder{Store: st}
	red, err := b.Build(ctx, r.ID, Options{Redact: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(red.Confirmed) != 1 || red.Confirmed[0].ID != confirmedID {
		t.Fatalf("已确认区应只有 #%d：%+v", confirmedID, red.Confirmed)
	}
	if len(red.Manual) != 1 || red.Manual[0].ID != manualID {
		t.Fatalf("未复测的应进「需人工确认」：%+v", red.Manual)
	}
	if len(red.Confirmed[0].PoCs) == 0 || !strings.Contains(red.Confirmed[0].PoCs[0], "curl") {
		t.Fatalf("已确认项必须带可执行复现命令：%+v", red.Confirmed[0])
	}

	md := red.Markdown()
	if strings.Contains(md, "api.example.com") {
		t.Fatalf("脱敏版不该出现目标域名：\n%s", md)
	}
	if !strings.Contains(md, "<target>") || !strings.Contains(md, "已确认") {
		t.Fatalf("报告缺少目标占位或分区：\n%s", md)
	}
	if !strings.Contains(md, "脱敏版") {
		t.Fatalf("报告应标明是脱敏版：\n%s", md)
	}

	// 完整版：保留真实目标，并标明是完整版。
	full, err := b.Build(ctx, r.ID, Options{Redact: false})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(full.Markdown(), "api.example.com") {
		t.Fatal("完整版应保留真实目标")
	}
	if !strings.Contains(full.Markdown(), "完整版") {
		t.Fatal("完整版应显式标注（避免误发）")
	}

	// HTML / zip 都要能出，且 zip 里有 poc 与 evidence。
	html, err := red.HTML()
	if err != nil {
		t.Fatalf("HTML 渲染失败：%v", err)
	}
	if !strings.Contains(string(html), "<html") || !strings.Contains(string(html), "反射型 XSS") {
		t.Fatalf("HTML 内容不对：%.200s", string(html))
	}
	zipData, err := red.Zip()
	if err != nil {
		t.Fatalf("打包失败：%v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	for _, want := range []string{
		"report.md", "report.html", "README.txt",
		fmt.Sprintf("poc/finding-%d.sh", confirmedID),
		fmt.Sprintf("evidence/finding-%d.txt", confirmedID),
	} {
		if !names[want] {
			t.Fatalf("zip 缺 %s（实际：%v）", want, names)
		}
	}
}
