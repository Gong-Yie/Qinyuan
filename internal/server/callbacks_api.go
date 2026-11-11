package server

import (
	"encoding/json"
	"net/http"
	"time"

	"qinyuan/internal/model"
)

// 本文件是 M17 的回连白名单 API（docs/24 §5）。
//
// 只给 operator：POST/DELETE 走 auth.RequiredRole 的写操作角色；
// **不给模型工具** —— planner/worker 不能自己开口子（防附件提示注入）。

type callbackJSON struct {
	ID        int64  `json:"id"`
	Host      string `json:"host"`
	Proto     string `json:"proto"`
	Ports     string `json:"ports"`
	Note      string `json:"note"`
	CreatedBy string `json:"created_by"`
	CreatedAt int64  `json:"created_at"`
	ExpiresAt int64  `json:"expires_at"`
	RevokedAt *int64 `json:"revoked_at,omitempty"`
	Active    bool   `json:"active"`
}

func callbackToJSON(c *model.Callback) callbackJSON {
	out := callbackJSON{
		ID: c.ID, Host: c.Host, Proto: c.Proto, Ports: c.Ports, Note: c.Note,
		CreatedBy: c.CreatedBy, CreatedAt: c.CreatedAt.Unix(), ExpiresAt: c.ExpiresAt.Unix(),
		Active: c.Active(time.Now()),
	}
	if c.RevokedAt != nil {
		t := c.RevokedAt.Unix()
		out.RevokedAt = &t
	}
	return out
}

type createCallbackReq struct {
	Host       string `json:"host"`
	Proto      string `json:"proto"`
	Ports      string `json:"ports"`
	Note       string `json:"note"`
	TTLSeconds int    `json:"ttl_seconds"`
}

func (s *Server) handleCreateCallback(w http.ResponseWriter, r *http.Request) {
	runID, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if _, err := s.Store.Run(r.Context(), runID); err != nil {
		writeError(w, http.StatusNotFound, "任务不存在")
		return
	}
	var in createCallbackReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	ttl := time.Duration(in.TTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	cb, err := s.Store.AddCallback(r.Context(), runID, in.Host, in.Proto, in.Ports, in.Note, "operator", ttl)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, callbackToJSON(cb))
}

func (s *Server) handleListCallbacks(w http.ResponseWriter, r *http.Request) {
	runID, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	cbs, err := s.Store.Callbacks(r.Context(), runID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读回连白名单失败")
		return
	}
	out := make([]callbackJSON, 0, len(cbs))
	for _, cb := range cbs {
		out = append(out, callbackToJSON(cb))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out, "total": len(out)})
}

func (s *Server) handleRevokeCallback(w http.ResponseWriter, r *http.Request) {
	runID, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	cid, ok := pathID(w, r, "cid")
	if !ok {
		return
	}
	changed, err := s.Store.RevokeCallback(r.Context(), runID, cid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "吊销失败")
		return
	}
	if !changed {
		writeError(w, http.StatusNotFound, "白名单不存在或已吊销")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": cid, "revoked": true})
}
