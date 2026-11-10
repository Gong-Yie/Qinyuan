package server

import (
	"encoding/json"
	"net/http"
	"time"

	"qinyuan/internal/store"
)

// 本文件是 P3-4 的三个人机协作口子：策略（预授权）、批量决定、放行审计。

// handleBatchDecide 批量批准/拒绝：逐条 CAS，冲突项单独返回，不覆盖别人的决定。
//
// 语义：一次点 50 条，其中 5 条已被别人处理 → 返回 ok=45 + conflict=[5 个 id]，
// 而不是整体失败（否则审批人会以为"一条都没生效"）。
func (s *Server) handleBatchDecide(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs      []int64 `json:"ids"`
		Decision string  `json:"decision"`
		Reason   string  `json:"reason"`
		By       string  `json:"by"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	if in.By == "" {
		in.By = "ui-batch"
	}
	ok, conflicts, err := s.Store.BatchDecideApprovals(r.Context(), in.IDs, in.Decision, in.By, in.Reason)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": ok, "conflict": conflicts, "count": len(in.IDs),
	})
}

// handleApprovalPolicies 列出策略（含过期/停用的，审计要看得见历史）。
func (s *Server) handleApprovalPolicies(w http.ResponseWriter, r *http.Request) {
	items, err := s.Store.ListApprovalPolicies(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "count": len(items)})
}

// handleCreateApprovalPolicy 建策略。
//
// 预授权的护栏在 store 里强制（allow 必须有 max_per_hour 与 expires_at ≤ 8h），
// 这里只做 JSON 解析与错误翻译。
func (s *Server) handleCreateApprovalPolicy(w http.ResponseWriter, r *http.Request) {
	var in struct {
		RunID      int64  `json:"run_id"`
		Priority   int    `json:"priority"`
		MatchTool  string `json:"match_tool"`
		MatchRegex string `json:"match_regex"`
		MatchScope string `json:"match_scope"`
		Decision   string `json:"decision"`
		MaxPerHour int    `json:"max_per_hour"`
		ExpiresAt  int64  `json:"expires_at"` // unix 秒
		Note       string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	var runID *int64
	if in.RunID > 0 {
		runID = &in.RunID
	}
	var expires *time.Time
	if in.ExpiresAt > 0 {
		t := time.Unix(in.ExpiresAt, 0)
		expires = &t
	}
	id, err := s.Store.CreateApprovalPolicy(r.Context(), store.ApprovalPolicy{
		RunID: runID, Priority: in.Priority, MatchTool: in.MatchTool,
		MatchRegex: in.MatchRegex, MatchScope: in.MatchScope, Decision: in.Decision,
		MaxPerHour: in.MaxPerHour, ExpiresAt: expires, Note: in.Note, Enabled: true,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

// handleDeleteApprovalPolicy 一键撤销（撤销即时失效，不依赖重启）。
func (s *Server) handleDeleteApprovalPolicy(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "pid")
	if !ok {
		return
	}
	deleted, err := s.Store.DeleteApprovalPolicy(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !deleted {
		writeError(w, http.StatusNotFound, "策略不存在")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

// handleApprovalGrants 放行审计（"这条为什么没问人就过了"）。
func (s *Server) handleApprovalGrants(w http.ResponseWriter, r *http.Request) {
	items, err := s.Store.ListApprovalGrants(r.Context(), intQuery(r, "limit", 100))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "count": len(items)})
}
