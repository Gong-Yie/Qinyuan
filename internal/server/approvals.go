package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"qinyuan/internal/store"
)

// handleApprovals 列审批：?status=pending|allowed|denied|timeout（空 = 全部）。
func (s *Server) handleApprovals(w http.ResponseWriter, r *http.Request) {
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	switch status {
	case "", "pending", "allowed", "denied", "timeout":
	default:
		writeError(w, http.StatusBadRequest, "status 只能是 pending/allowed/denied/timeout")
		return
	}
	rows, err := s.Store.ListApprovals(r.Context(), status, intQuery(r, "limit", 100))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": rows, "count": len(rows)})
}

// handleApprovalCount 给侧边栏红点用。
func (s *Server) handleApprovalCount(w http.ResponseWriter, r *http.Request) {
	n, err := s.Store.CountPendingApprovals(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pending": n})
}

// handleDecideApproval 人工批准/拒绝（CAS：只有 pending 能被改）。
func (s *Server) handleDecideApproval(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var in struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
		By       string `json:"by"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	if in.Decision != "allowed" && in.Decision != "denied" {
		writeError(w, http.StatusBadRequest, "decision 只能是 allowed 或 denied")
		return
	}
	if in.By == "" {
		in.By = "ui"
	}
	okDecide, err := s.Store.DecideApproval(r.Context(), id, in.Decision, in.By, in.Reason)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !okDecide {
		// 已被别人处理 / 已超时：不覆盖，明确告诉前端刷新。
		writeError(w, http.StatusConflict, "这条审批已被处理（或已超时），请刷新")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id, "decision": in.Decision})
}

// handleInterceptRules 列出规则（设置页只读 + 审计）。
func (s *Server) handleInterceptRules(w http.ResponseWriter, r *http.Request) {
	rules, err := s.Store.ListInterceptRules(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": rules, "count": len(rules)})
}

// handleCreateInterceptRule 加一条规则（demo/自测用；没有 UI 编辑器）。
func (s *Server) handleCreateInterceptRule(w http.ResponseWriter, r *http.Request) {
	var in struct {
		RunID      int64  `json:"run_id"`
		Priority   int    `json:"priority"`
		MatchTool  string `json:"match_tool"`
		MatchRegex string `json:"match_regex"`
		Decision   string `json:"decision"`
		Note       string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	if in.Decision != "allow" && in.Decision != "deny" && in.Decision != "ask" {
		writeError(w, http.StatusBadRequest, "decision 只能是 allow/deny/ask")
		return
	}
	var runID *int64
	if in.RunID > 0 {
		runID = &in.RunID
	}
	id, err := s.Store.CreateInterceptRule(r.Context(), store.InterceptRule{
		RunID: runID, Priority: in.Priority, MatchTool: in.MatchTool,
		MatchRegex: in.MatchRegex, Decision: in.Decision, Note: in.Note, Enabled: true,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}
