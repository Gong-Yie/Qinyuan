package server

import (
	"fmt"
	"net/http"
	"strings"

	"qinyuan/internal/agent"
	"qinyuan/internal/model"
	"qinyuan/internal/report"
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

// handleReport 生成可复现报告（P3-2）。
//
// 四个形态：
//   - format=md（默认）：Markdown 源；
//   - format=html：单文件 HTML，可直接发客户（自包含、无外部依赖）；
//   - format=zip：交付包（md + html + poc/ + evidence/ + README）；
//   - format=json：结构化（给 CI/控制台）。
//
// 默认导出**脱敏版**（目标域名 → <target>、凭据打码）；full=1 才是完整版，
// 且完整版必须留审计 —— 报告里带着可执行的攻击脚本，外流即武器。
func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if format == "" {
		format = "md"
	}
	full := r.URL.Query().Get("full") == "1" || r.URL.Query().Get("full") == "true"

	rep, err := (&report.Builder{Store: s.Store}).Build(r.Context(), id, report.Options{Redact: !full})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if full {
		s.auditFullReport(r, id, format)
	}

	var data []byte
	var ctype, filename string
	switch format {
	case "md", "markdown":
		data = []byte(rep.Markdown())
		ctype, filename = "text/markdown; charset=utf-8", fmt.Sprintf("qinyuan-run-%d.md", id)
	case "html":
		if data, err = rep.HTML(); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		ctype, filename = "text/html; charset=utf-8", fmt.Sprintf("qinyuan-run-%d.html", id)
	case "zip":
		if data, err = rep.Zip(); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		ctype, filename = "application/zip", fmt.Sprintf("qinyuan-run-%d.zip", id)
	case "json":
		if data, err = rep.JSON(); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		ctype, filename = "application/json; charset=utf-8", fmt.Sprintf("qinyuan-run-%d.json", id)
	default:
		writeError(w, http.StatusBadRequest, "format 只支持 md/html/zip/json")
		return
	}

	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "no-cache")
	if r.URL.Query().Get("download") != "" {
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	}
	_, _ = w.Write(data)
}

// auditFullReport 完整版导出必须留痕（谁、什么时候、导了什么格式）。
func (s *Server) auditFullReport(r *http.Request, runID int64, format string) {
	meta := map[string]any{"format": format, "by": "http", "remote": r.RemoteAddr}
	_, _ = s.Store.AppendActivity(r.Context(), &model.Activity{
		RunID: runID, Worker: "report", Kind: model.ActSystem,
		Summary:  "导出了完整版报告（含真实目标与可执行攻击脚本）",
		Metadata: meta,
	})
	s.Log.Warn("[report] 完整版报告已导出", "run", runID, "format", format, "remote", r.RemoteAddr)
}
