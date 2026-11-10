// Package report 生成"可复现报告"（升级计划 P3-2）。
//
// 一份报告必须能回答四件事，缺一条就不算可交付：
//  1. 结论：发现了什么、有多严重、影响哪些资产；
//  2. 复现：前提 + 编号命令 + 期望断言 + 最近一次实测（不是形容词）；
//  3. 证据：原始请求/响应与时间戳（可追溯）；
//  4. 限制：哪些没测、为什么没测（不给客户"全覆盖"的假象）。
//
// 默认导出**脱敏版**：目标域名替换成 <target>、常见凭据参数打码。
// 完整版必须显式 full=true，并由调用方写审计（报告本身带可执行攻击脚本）。
package report

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"sort"
	"strings"
	"time"

	"qinyuan/internal/model"
	"qinyuan/internal/store"
)

// Options 控制导出形态。
type Options struct {
	// Redact=true（默认）脱敏：域名替换 + 凭据打码。
	Redact bool
}

// Finding 是报告里的一条发现。
type Finding struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Severity string `json:"severity"`
	Vuln     string `json:"vuln_class,omitempty"`
	Summary  string `json:"summary"`
	Evidence string `json:"evidence"`
	// PoCs 是可直接复制执行的复现命令（由绑定流量生成）。
	PoCs []string `json:"poc"`
	// Retest 是最近一次复测结论（verified/failed/unverifiable/空）。
	Retest      string `json:"retest,omitempty"`
	RetestObs   string `json:"retest_observed,omitempty"`
	InConfirmed bool   `json:"in_confirmed"`
}

// Asset 是附录 A 的一行。
type Asset struct {
	Kind    string `json:"kind"`
	Label   string `json:"label"`
	InScope bool   `json:"in_scope"`
	Tested  bool   `json:"tested"`
}

// Report 是报告的全部数据（Markdown / HTML / zip 都从它渲染）。
type Report struct {
	RunID       int64
	Goal        string
	Description string
	Status      string
	CreatedAt   time.Time
	GeneratedAt time.Time
	Scopes      []string
	Coverage    struct {
		Total  int
		Tested int
		Pct    float64
	}
	FactsTotal int
	// 三个分区：已确认（verified）、待复核（failed）、需人工确认（未复测 / unverifiable）。
	Confirmed []Finding
	Failed    []Finding
	Manual    []Finding
	Facts     []string
	Assets    []Asset
	Redacted  bool
}

// Builder 组装报告。
type Builder struct {
	Store *store.Store
}

// Build 读库并组装报告。
func (b *Builder) Build(ctx context.Context, runID int64, opts Options) (*Report, error) {
	run, err := b.Store.Run(ctx, runID)
	if err != nil {
		return nil, err
	}
	rep := &Report{
		RunID: run.ID, Goal: run.Goal, Description: run.Description,
		CreatedAt: run.CreatedAt, GeneratedAt: time.Now(), Redacted: opts.Redact,
	}
	if run.Task != nil {
		rep.Status = model.DeriveStatus(run.Task, false, false)
	}
	scopes, _ := b.Store.Scopes(ctx, runID)
	for _, sc := range scopes {
		rep.Scopes = append(rep.Scopes, scopeLabel(sc))
	}
	if cov, err := b.Store.Coverage(ctx, runID); err == nil && cov != nil {
		rep.Coverage.Total, rep.Coverage.Tested, rep.Coverage.Pct = cov.Total, cov.Tested, cov.Pct
	}

	// 附录 A：资产清单（范围内 / 已测 / 未测）。
	if nodes, _, err := b.Store.CoverageGraph(ctx, runID); err == nil {
		for _, n := range nodes {
			rep.Assets = append(rep.Assets, Asset{
				Kind: n.Kind, Label: n.Label, InScope: n.InScope, Tested: n.Tested,
			})
		}
		sort.Slice(rep.Assets, func(i, j int) bool { return rep.Assets[i].Label < rep.Assets[j].Label })
	}

	// 事实摘要（最近 20 条，排除 origin）。
	facts, _ := b.Store.NodesByKind(ctx, runID, model.KindFact, 60)
	for i := len(facts) - 1; i >= 0 && len(rep.Facts) < 20; i-- {
		if facts[i].State == model.StateOrigin {
			continue
		}
		rep.Facts = append(rep.Facts, fmt.Sprintf("#%d [%s] %s",
			facts[i].ID, firstNonEmpty(facts[i].PayloadField("confidence"), "observed"),
			facts[i].Summary()))
	}
	rep.FactsTotal = countNonOrigin(facts)

	// 发现：按最近一次复测结论分区。
	retestStatus, _ := b.Store.RetestStatusByFinding(ctx, runID)
	findings, _ := b.Store.NodesByKind(ctx, runID, model.KindFinding, 200)
	sort.Slice(findings, func(i, j int) bool { return findings[i].ID < findings[j].ID })
	for _, n := range findings {
		var p struct {
			Name     string `json:"name"`
			Severity string `json:"severity"`
			Summary  string `json:"summary"`
			Evidence string `json:"evidence"`
			Vuln     string `json:"vuln_class"`
		}
		_ = n.DecodePayload(&p)
		f := Finding{
			ID: n.ID, Name: firstNonEmpty(p.Name, n.Summary()), Severity: p.Severity,
			Vuln: p.Vuln, Summary: p.Summary, Evidence: p.Evidence,
		}
		if binds, _ := b.Store.FindingTrafficList(ctx, runID, n.ID); len(binds) > 0 {
			for _, bd := range binds {
				method, _ := bd["method"].(string)
				url, _ := bd["url"].(string)
				reqPrev, _ := bd["req_preview"].(string)
				f.PoCs = append(f.PoCs, curlFor(method, url, reqPrev))
			}
		}
		if st := retestStatus[n.ID]; st != "" {
			f.Retest = st
			if rows, _ := b.Store.Retests(ctx, runID, n.ID, 1); len(rows) > 0 {
				f.RetestObs = rows[0].Observed
			}
		}
		switch f.Retest {
		case "verified":
			f.InConfirmed = true
			rep.Confirmed = append(rep.Confirmed, f)
		case "failed":
			rep.Failed = append(rep.Failed, f)
		default:
			rep.Manual = append(rep.Manual, f)
		}
	}

	if opts.Redact {
		red := NewRedactor(b.hosts(ctx, runID))
		rep.redact(red)
	}
	return rep, nil
}

// hosts 收集本任务出现过的目标主机（脱敏用）。
func (b *Builder) hosts(ctx context.Context, runID int64) []string {
	run, err := b.Store.Run(ctx, runID)
	if err != nil {
		return nil
	}
	out := append([]string(nil), run.Seeds...)
	scopes, _ := b.Store.Scopes(ctx, runID)
	for _, sc := range scopes {
		out = append(out, sc.Domain, sc.Value)
	}
	ents, _ := b.Store.ListEntitiesForRun(ctx, runID, 500)
	for _, e := range ents {
		out = append(out, e.Domain, e.IP, e.URL)
	}
	return out
}

func (r *Report) redact(red *Redactor) {
	for i := range r.Confirmed {
		r.Confirmed[i].redact(red)
	}
	for i := range r.Failed {
		r.Failed[i].redact(red)
	}
	for i := range r.Manual {
		r.Manual[i].redact(red)
	}
	for i := range r.Assets {
		r.Assets[i].Label = red.Text(r.Assets[i].Label)
	}
	for i := range r.Facts {
		r.Facts[i] = red.Text(r.Facts[i])
	}
	r.Goal = red.Text(r.Goal)
	r.Description = red.Text(r.Description)
	for i := range r.Scopes {
		r.Scopes[i] = red.Text(r.Scopes[i])
	}
}

func (f *Finding) redact(red *Redactor) {
	f.Name = red.Text(f.Name)
	f.Summary = red.Text(f.Summary)
	f.Evidence = red.Text(f.Evidence)
	f.RetestObs = red.Text(f.RetestObs)
	for i := range f.PoCs {
		f.PoCs[i] = red.Text(f.PoCs[i])
	}
}

// Markdown 渲染成 Markdown（报告源格式）。
func (r *Report) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# 钦原 · 任务报告 #%d\n\n", r.RunID)
	fmt.Fprintf(&b, "> 生成于 %s", r.GeneratedAt.Format("2006-01-02 15:04:05"))
	if r.Redacted {
		b.WriteString("（**脱敏版**：目标域名已替换为 <target>，凭据参数已打码）")
	} else {
		b.WriteString("（**完整版**：含真实目标与可执行攻击脚本，注意分发范围）")
	}
	b.WriteString("\n\n")

	b.WriteString("## 1. 摘要\n\n")
	fmt.Fprintf(&b, "- **目标**：%s\n", r.Goal)
	if r.Description != "" {
		fmt.Fprintf(&b, "- **说明**：%s\n", r.Description)
	}
	fmt.Fprintf(&b, "- **状态**：%s · 创建于 %s\n", r.Status, r.CreatedAt.Format("2006-01-02 15:04:05"))
	if len(r.Scopes) > 0 {
		fmt.Fprintf(&b, "- **授权范围**：%s\n", strings.Join(r.Scopes, "；"))
	}
	fmt.Fprintf(&b, "- **覆盖度**：%.0f%%（%d / %d，只认事实锚点）\n", r.Coverage.Pct, r.Coverage.Tested, r.Coverage.Total)
	fmt.Fprintf(&b, "- **结论**：已确认 %d · 待复核 %d · 需人工确认 %d（事实 %d 条）\n\n",
		len(r.Confirmed), len(r.Failed), len(r.Manual), r.FactsTotal)

	b.WriteString("## 2. 漏洞详情\n\n")
	if len(r.Confirmed)+len(r.Failed)+len(r.Manual) == 0 {
		b.WriteString("本轮没有已确认的漏洞。可疑但未触发的结论记在事实里（confidence=inferred）。\n\n")
	}
	writeSection := func(title string, items []Finding, note string) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(&b, "### %s（%d）\n\n", title, len(items))
		if note != "" {
			fmt.Fprintf(&b, "> %s\n\n", note)
		}
		for i, f := range items {
			fmt.Fprintf(&b, "#### %d. %s（%s）\n\n", i+1, f.Name, f.Severity)
			if f.Summary != "" {
				fmt.Fprintf(&b, "%s\n\n", f.Summary)
			}
			if f.Vuln != "" {
				fmt.Fprintf(&b, "- **类别**：%s\n", f.Vuln)
			}
			if f.Evidence != "" {
				fmt.Fprintf(&b, "- **证据**：%s\n", oneLine(f.Evidence))
			}
			if len(f.PoCs) > 0 {
				b.WriteString("- **复现**（可直接复制执行）：\n\n```bash\n")
				b.WriteString(strings.Join(f.PoCs, "\n\n"))
				b.WriteString("\n```\n")
			} else {
				b.WriteString("- **复现**：未绑定流量快照，无法给出可执行命令（需重新取证）\n")
			}
			if f.Retest != "" {
				fmt.Fprintf(&b, "- **复测**：%s（%s）\n", f.Retest, oneLine(f.RetestObs))
			} else {
				b.WriteString("- **复测**：未复测（结论只对取证当时成立）\n")
			}
			b.WriteString("\n")
		}
	}
	writeSection("已确认", r.Confirmed, "断言通过、可复现的项；对外交付以这一区为准。")
	writeSection("待复核", r.Failed, "复测断言未通过：可能已修复、可能依赖时间/会话，需人工确认。")
	writeSection("需人工确认", r.Manual, "未自动验证（无流量快照 / 需交互 / 破坏性操作），不能当作已确认。")

	b.WriteString("## 3. 附录 A · 资产清单\n\n")
	fmt.Fprintf(&b, "范围内 %d 个实体，已测 %d 个。\n\n", r.Coverage.Total, r.Coverage.Tested)
	b.WriteString("| 资产 | 类型 | 范围内 | 已测 |\n|---|---|---|---|\n")
	shown := 0
	for _, a := range r.Assets {
		if shown >= 100 {
			b.WriteString("| …（更多见控制台资产页） | | | |\n")
			break
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", escapePipe(a.Label), a.Kind,
			yesNo(a.InScope), yesNo(a.Tested))
		shown++
	}
	if len(r.Assets) == 0 {
		b.WriteString("| （无） | | | |\n")
	}

	b.WriteString("\n## 4. 附录 B · 方法与限制\n\n")
	b.WriteString("- 覆盖度只认**事实锚点**：仅登记资产或仅报漏洞不算测过。\n")
	b.WriteString("- 复测只重放**当时抓到的那一次请求**：不执行页面 JS、不跟随多步利用链。\n")
	b.WriteString("- 时间相关的接口（nonce/时间戳）可能误判为 failed，以复核结论为准。\n")
	b.WriteString("- 破坏性操作（写/删/提权）不自动复测，标注为「需人工确认」。\n")
	fmt.Fprintf(&b, "\n---\n\n由钦原生成于 %s。复测结论只对「重放当时的目标状态」成立。\n",
		r.GeneratedAt.Format("2006-01-02 15:04:05"))
	return b.String()
}

var htmlTmpl = template.Must(template.New("report").Funcs(template.FuncMap{
	"join": func(ss []string, sep string) string { return strings.Join(ss, sep) },
	"dict": dictOf,
}).Parse(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8">
<title>钦原报告 #{{.RunID}}</title>
<style>
body{font:15px/1.7 -apple-system,"Segoe UI","Noto Sans SC",sans-serif;max-width:900px;margin:40px auto;padding:0 20px;color:#1f2328}
h1{border-bottom:2px solid #1f2328;padding-bottom:8px}
h2{margin-top:32px;border-bottom:1px solid #d0d7de;padding-bottom:4px}
.finding{border:1px solid #d0d7de;border-radius:8px;padding:12px 16px;margin:12px 0}
.sev-critical{color:#b91c1c;font-weight:700}.sev-high{color:#c2410c;font-weight:600}
.sev-medium{color:#a16207}.sev-low{color:#166534}
pre{background:#f6f8fa;padding:12px;border-radius:6px;overflow:auto;font-size:13px}
table{border-collapse:collapse;width:100%}td,th{border:1px solid #d0d7de;padding:4px 8px;font-size:13px}
.muted{color:#57606a;font-size:13px}
</style></head><body>
<h1>钦原 · 任务报告 #{{.RunID}}</h1>
<p class="muted">生成于 {{.GeneratedAt}} · {{if .Redacted}}脱敏版（目标域名替换为 &lt;target&gt;，凭据打码）{{else}}完整版（含真实目标与可执行脚本，注意分发范围）{{end}}</p>
<h2>1. 摘要</h2>
<ul>
<li><b>目标</b>：{{.Goal}}</li>
{{if .Description}}<li><b>说明</b>：{{.Description}}</li>{{end}}
<li><b>状态</b>：{{.Status}}</li>
<li><b>授权范围</b>：{{join .Scopes "；"}}</li>
<li><b>覆盖度</b>：{{printf "%.0f" .Coverage.Pct}}%（{{.Coverage.Tested}} / {{.Coverage.Total}}）</li>
<li><b>结论</b>：已确认 {{len .Confirmed}} · 待复核 {{len .Failed}} · 需人工确认 {{len .Manual}}</li>
</ul>
{{define "section"}}{{if .Items}}<h2>{{.Title}}（{{len .Items}}）</h2>
{{if .Note}}<p class="muted">{{.Note}}</p>{{end}}
{{range $i, $f := .Items}}<div class="finding">
<h3>{{$f.Name}} <span class="sev-{{$f.Severity}}">[{{$f.Severity}}]</span></h3>
{{if $f.Summary}}<p>{{$f.Summary}}</p>{{end}}
{{if $f.Vuln}}<p class="muted">类别：{{$f.Vuln}}</p>{{end}}
{{if $f.Evidence}}<p><b>证据</b>：{{$f.Evidence}}</p>{{end}}
{{if $f.PoCs}}<p><b>复现</b></p><pre>{{range $f.PoCs}}{{.}}
{{end}}</pre>{{else}}<p class="muted">未绑定流量快照，无法给出可执行命令。</p>{{end}}
{{if $f.Retest}}<p><b>复测</b>：{{$f.Retest}}（{{$f.RetestObs}}）</p>{{else}}<p class="muted">未复测：结论只对取证当时成立。</p>{{end}}
</div>{{end}}{{end}}{{end}}
{{template "section" dict "Title" "已确认" "Items" .Confirmed "Note" "断言通过、可复现；对外交付以这一区为准。"}}
{{template "section" dict "Title" "待复核" "Items" .Failed "Note" "复测未通过：可能已修复或依赖时间/会话。"}}
{{template "section" dict "Title" "需人工确认" "Items" .Manual "Note" "未自动验证（无快照/需交互/破坏性），不能当作已确认。"}}
<h2>3. 附录 A · 资产清单</h2>
<p class="muted">共 {{len .Assets}} 条（最多展示 100 条）。</p>
<table><tr><th>资产</th><th>类型</th><th>范围内</th><th>已测</th></tr>
{{range .Assets}}<tr><td>{{.Label}}</td><td>{{.Kind}}</td><td>{{if .InScope}}是{{else}}否{{end}}</td><td>{{if .Tested}}是{{else}}否{{end}}</td></tr>
{{end}}</table>
<h2>4. 附录 B · 方法与限制</h2>
<ul>
<li>覆盖度只认事实锚点：仅登记资产或仅报漏洞不算测过。</li>
<li>复测只重放当时抓到的那一次请求：不执行页面 JS、不跟随多步利用链。</li>
<li>时间相关接口可能误判为 failed，以复核结论为准。</li>
<li>破坏性操作不自动复测，标注为「需人工确认」。</li>
</ul>
<p class="muted">报告由钦原生成；复测结论只对重放当时的目标状态成立。</p>
</body></html>`))

// HTML 渲染成可直接发客户的单文件 HTML。
func (r *Report) HTML() ([]byte, error) {
	view := map[string]any{
		"RunID": r.RunID, "Goal": r.Goal, "Description": r.Description,
		"Status": r.Status, "GeneratedAt": r.GeneratedAt.Format("2006-01-02 15:04:05"),
		"Scopes": r.Scopes, "Coverage": r.Coverage, "Redacted": r.Redacted,
		"Confirmed": r.Confirmed, "Failed": r.Failed, "Manual": r.Manual,
		"Assets": r.Assets,
	}
	// dict 供子模板传参（html/template 没有内置）。
	tmpl := template.Must(htmlTmpl.Clone())
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, view); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// dictOf 给子模板传参用（html/template 没有内置 dict）。
func dictOf(kv ...any) map[string]any {
	m := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		k, _ := kv[i].(string)
		m[k] = kv[i+1]
	}
	return m
}

// Zip 打一个自包含的交付包：report.md + report.html + poc/ + evidence/。
//
// 自包含的意思是"不依赖平台就能看/能跑"（验收要求）。
func (r *Report) Zip() ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	write := func(name string, data []byte) error {
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	}
	if err := write("report.md", []byte(r.Markdown())); err != nil {
		return nil, err
	}
	html, err := r.HTML()
	if err != nil {
		return nil, err
	}
	if err := write("report.html", html); err != nil {
		return nil, err
	}
	all := append(append(append([]Finding{}, r.Confirmed...), r.Failed...), r.Manual...)
	for _, f := range all {
		if len(f.PoCs) == 0 {
			continue
		}
		script := "#!/bin/sh\n# 钦原复现脚本 · finding #" + fmt.Sprint(f.ID) + "\nset -e\n\n" + strings.Join(f.PoCs, "\n\n") + "\n"
		if err := write(fmt.Sprintf("poc/finding-%d.sh", f.ID), []byte(script)); err != nil {
			return nil, err
		}
		ev := "finding #" + fmt.Sprint(f.ID) + " · " + f.Name + "\n\n证据：\n" + f.Evidence + "\n\n复测：" + f.Retest + "\n" + f.RetestObs + "\n"
		if err := write(fmt.Sprintf("evidence/finding-%d.txt", f.ID), []byte(ev)); err != nil {
			return nil, err
		}
	}
	readme := "钦原报告包\n\n- report.html：直接双击打开（自包含，无外部依赖）\n- report.md：Markdown 源\n- poc/：可执行复现脚本（curl）\n- evidence/：每条发现的证据正文\n\n" +
		"注意：复测结论只对重放当时的目标状态成立；脱敏版里目标域名替换为 <target>。\n"
	if err := write("README.txt", []byte(readme)); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// JSON 给控制台/CI 用（结构化，不是渲染件）。
func (r *Report) JSON() ([]byte, error) { return json.Marshal(r) }

// ---------- 小工具 ----------

func curlFor(method, url, body string) string {
	if method == "" {
		method = "GET"
	}
	cmd := fmt.Sprintf("curl -i -X %s '%s'", method, strings.ReplaceAll(url, "'", `'\''`))
	if strings.TrimSpace(body) != "" {
		cmd += fmt.Sprintf(" \\\n  -d '%s'", strings.ReplaceAll(strings.TrimSpace(body), "'", `'\''`))
	}
	return cmd
}

func scopeLabel(sc model.Scope) string {
	switch sc.Kind {
	case "root_domain":
		return "*." + sc.Domain
	case "subdomain":
		return sc.Domain
	case "cidr":
		return sc.Net
	case "ip":
		return firstNonEmpty(sc.Value, sc.Domain)
	default:
		return firstNonEmpty(sc.Value, sc.Domain, sc.Net)
	}
}

func countNonOrigin(nodes []*model.Node) int {
	n := 0
	for _, x := range nodes {
		if x.State != model.StateOrigin {
			n++
		}
	}
	return n
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func yesNo(b bool) string {
	if b {
		return "是"
	}
	return "否"
}

func escapePipe(s string) string { return strings.ReplaceAll(s, "|", `\|`) }
