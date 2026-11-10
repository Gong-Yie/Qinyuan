package server

import (
	"net/http"
	"strings"

	"qinyuan/internal/store"
)

// handleTraffic 列流量：?host=&method=&status=&q=&limit=
func (s *Server) handleTraffic(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	q := store.TrafficQuery{
		Host:   strings.TrimSpace(r.URL.Query().Get("host")),
		Method: strings.TrimSpace(r.URL.Query().Get("method")),
		Q:      strings.TrimSpace(r.URL.Query().Get("q")),
		Status: intQuery(r, "status", 0),
		Limit:  intQuery(r, "limit", 50),
	}
	rows, err := s.Store.SearchTraffic(r.Context(), id, q)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": rows, "count": len(rows)})
}

// handleTrafficItem 取单条交换（含头与正文预览）。
func (s *Server) handleTrafficItem(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	tid, ok := pathID(w, r, "tid")
	if !ok {
		return
	}
	e, err := s.Store.TrafficDetail(r.Context(), id, tid)
	if err != nil {
		writeError(w, http.StatusNotFound, "流量不存在")
		return
	}
	writeJSON(w, http.StatusOK, e)
}

// handleFindingTraffic 读 finding 绑定的证据快照（流量删了也能读）。
func (s *Server) handleFindingTraffic(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	fid, ok := pathID(w, r, "fid")
	if !ok {
		return
	}
	items, err := s.Store.FindingTrafficList(r.Context(), id, fid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "count": len(items)})
}
