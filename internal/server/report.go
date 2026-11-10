package server

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"qinyuan/internal/agent"
	"qinyuan/internal/model"
	"qinyuan/internal/store"
)

// handleRetestFinding 复测一条 finding（重放绑定证据 + 断言）。
func (s *Server) handleRetestFinding(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	fid, ok := pathID(w, r, "fid")
	if !ok {
		return
	}
	allowLoopback := false
	if s.Engine != nil {
		allowLoopback = s.Engine.Cfg.AllowLoopback
	}
	rows, err := agent.RetestFinding(r.Context(), s.Store, id, fid, allowLoopback)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": rows, "count": len(rows)})
}

// handleRetests 复测历史（单条 finding 或整个 run）。
func (s *Server) handleRetests(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if fid := r.URL.Query().Get("finding"); fid != "" {
		var f int64
		fmt.Sscanf(fid, "%d", &f)
		rows, err := s.Store.Retests(r.Context(), id, f, intQuery(r, "limit", 20))
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": rows, "count": len(rows)})
		return
	}
	rows, err := s.Store.RetestHistory(r.Context(), id, intQuery(r, "limit", 100))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": rows, "count": len(rows)})
}

// handleReport 生成任务报告（Markdown）。带 ?download=1 时作为附件下载。
//
// 报告的两个硬要求（升级计划 P3-2）：外部工程师能只按报告复现；
// 每条 finding 都要有"结论 + 证据 + 可执行复现 + 复测结论"，而不是一段形容词。
func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	md, err := s.buildReport(r, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	if r.URL.Query().Get("download") != "" {
		w.Header().Set("Content-Disposition",
			fmt.Sprintf("attachment; filename=\"qinyuan-run-%d.md\"", id))
	}
	_, _ = w.Write([]byte(md))
}

func (s *Server) buildReport(r *http.Request, runID int64) (string, error) {
	ctx := r.Context()
	run, err := s.Store.Run(ctx, runID)
	if err != nil {
		return "", err
	}
	cov, _ := s.Store.Coverage(ctx, runID)
	scopes, _ := s.Store.Scopes(ctx, runID)
	facts, _ := s.Store.NodesByKind(ctx, runID, model.KindFact, 60)
	findings, _ := s.Store.NodesByKind(ctx, runID, model.KindFinding, 100)
	retests, _ := s.Store.RetestHistory(ctx, runID, 200)

	// 最近一次复测结论：finding → 状态
	latest := map[int64]store.Retest{}
	for _, rt := range retests {
		if _, ok := latest[rt.FindingID]; !ok {
			latest[rt.FindingID] = rt
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# 钦原 · 任务报告 #%d\n\n", run.ID)
	fmt.Fprintf(&b, "- **目标**：%s\n", run.Goal)
	status := run.Status
	if run.Task != nil && run.Task.Status != "" {
		status = run.Task.Status
	}
	fmt.Fprintf(&b, "- **状态**：%s · 轮次 %d\n", status, run.RoundNo)
	fmt.Fprintf(&b, "- **创建**：%s\n", run.CreatedAt.Format("2006-01-02 15:04:05"))
	if run.Description != "" {
		fmt.Fprintf(&b, "- **说明**：%s\n", run.Description)
	}
	if len(scopes) > 0 {
		var ss []string
		for _, sc := range scopes {
			ss = append(ss, fmt.Sprintf("%s %s%s%s", sc.Kind, sc.Domain, sc.Value, sc.Net))
		}
		fmt.Fprintf(&b, "- **授权范围**：%s\n", strings.Join(ss, "；"))
	}
	if cov != nil {
		fmt.Fprintf(&b, "- **覆盖度**：%.0f%%（%d / %d 已测；只认 fact 锚点）\n", cov.Pct, cov.Tested, cov.Total)
	}
	fmt.Fprintf(&b, "- **事实**：%d 条 · **发现**：%d 个\n\n", countNonOrigin(facts), len(findings))

	b.WriteString("## 发现\n\n")
	if len(findings) == 0 {
		b.WriteString("本轮没有已确认的漏洞。可疑但未触发的结论记在事实里（confidence=inferred）。\n\n")
	} else {
		sort.Slice(findings, func(i, j int) bool { return findings[i].ID < findings[j].ID })
		for i, f := range findings {
			var p struct {
				Name     string `json:"name"`
				Severity string `json:"severity"`
				Summary  string `json:"summary"`
				Evidence string `json:"evidence"`
				Vuln     string `json:"vuln_class"`
			}
			_ = f.DecodePayload(&p)
			fmt.Fprintf(&b, "### %d. %s（%s）\n\n", i+1, firstNonEmpty(p.Name, f.Summary()), p.Severity)
			if p.Summary != "" {
				fmt.Fprintf(&b, "%s\n\n", p.Summary)
			}
			if p.Vuln != "" {
				fmt.Fprintf(&b, "- **类别**：%s\n", p.Vuln)
			}
			if p.Evidence != "" {
				fmt.Fprintf(&b, "- **证据**：%s\n", oneLine(p.Evidence))
			}
			// 绑定流量 → 可执行复现
			binds, _ := s.Store.FindingTrafficList(ctx, runID, f.ID)
			for _, bd := range binds {
				method, _ := bd["method"].(string)
				url, _ := bd["url"].(string)
				reqPrev, _ := bd["req_preview"].(string)
				fmt.Fprintf(&b, "- **复现**：\n\n```bash\ncurl -i -X %s '%s'", method, url)
				if strings.TrimSpace(reqPrev) != "" {
					fmt.Fprintf(&b, " \\\n  -d '%s'", strings.ReplaceAll(strings.TrimSpace(reqPrev), "'", `'\''`))
				}
				b.WriteString("\n```\n")
			}
			if rt, ok := latest[f.ID]; ok {
				fmt.Fprintf(&b, "- **复测**：%s（%s）\n", rt.Status, rt.Observed)
			} else {
				b.WriteString("- **复测**：未复测（用 `qinyuan retest --run ")
				fmt.Fprintf(&b, "%d --finding %d` 或控制台按钮）\n", runID, f.ID)
			}
			b.WriteString("\n")
		}
	}

	b.WriteString("## 事实摘要（最近 20 条）\n\n")
	shown := 0
	for i := len(facts) - 1; i >= 0 && shown < 20; i-- {
		f := facts[i]
		if f.State == "origin" {
			continue
		}
		var p struct {
			Confidence string `json:"confidence"`
		}
		_ = f.DecodePayload(&p)
		fmt.Fprintf(&b, "- #%d [%s] %s\n", f.ID, firstNonEmpty(p.Confidence, "observed"), f.Summary())
		shown++
	}
	if shown == 0 {
		b.WriteString("（无）\n")
	}

	b.WriteString("\n---\n\n")
	fmt.Fprintf(&b, "由钦原生成于 %s。复测结论只对「重放当时的目标状态」成立。\n",
		time.Now().Format("2006-01-02 15:04:05"))
	return b.String(), nil
}

func countNonOrigin(nodes []*model.Node) int {
	n := 0
	for _, x := range nodes {
		if x.State != "origin" {
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

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
