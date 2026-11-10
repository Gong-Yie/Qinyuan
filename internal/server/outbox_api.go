package server

import (
	"net/http"
)

// 本文件是 P2-3 的运维读口：事件积压/死信/重放。
//
// 没有它，outbox 就是个黑盒 —— "通知为什么没发"只能靠翻日志。

func (s *Server) handleOutbox(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	items, err := s.Store.ListOutbox(r.Context(), status, intQuery(r, "limit", 100))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	pending, dead, err := s.Store.CountOutbox(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items, "count": len(items),
		"pending": pending, "dead": dead,
	})
}

func (s *Server) handleReplayOutbox(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "oid")
	if !ok {
		return
	}
	replayed, err := s.Store.ReplayOutbox(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !replayed {
		writeError(w, http.StatusConflict, "事件不存在，或还在待投递状态（只有死信/已完成的事件可以重放）")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

// handleOutboxStats 是健康检查用的一行摘要（积压太多说明下游挂了）。
func (s *Server) handleOutboxStats(w http.ResponseWriter, r *http.Request) {
	pending, dead, err := s.Store.CountOutbox(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pending": pending, "dead": dead})
}
