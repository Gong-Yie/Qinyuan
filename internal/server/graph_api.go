package server

import (
	"net/http"
)

// handleGraph 是链路图数据源：一次给全节点与全边（布局在前端算）。
func (s *Server) handleGraph(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	nodes, edges, err := s.Store.Graph(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"nodes": nodes, "edges": edges, "total_nodes": len(nodes), "total_edges": len(edges),
	})
}

// handleCoverageGraph 是覆盖图数据源：范围内实体 + 已测标记 + 父子边。
func (s *Server) handleCoverageGraph(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	nodes, edges, err := s.Store.CoverageGraph(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"nodes": nodes, "edges": edges, "total_nodes": len(nodes), "total_edges": len(edges),
	})
}
