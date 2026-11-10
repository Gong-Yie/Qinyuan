package server

import (
	"net/http"
)

// handleStats 是仪表盘首页的聚合数字（跨 run）。
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	st, err := s.Store.Stats(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// handleFindings 跨 run 列发现（发现页）。
func (s *Server) handleFindings(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Store.ListFindings(r.Context(), intQuery(r, "limit", 100))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": rows, "count": len(rows)})
}

// handleEntities 列资产（跨 run 共享的实体图）。
func (s *Server) handleEntities(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Store.ListEntities(r.Context(), intQuery(r, "limit", 200))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": rows, "count": len(rows)})
}
