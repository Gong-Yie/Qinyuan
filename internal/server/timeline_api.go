package server

import (
	"net/http"
	"strconv"
	"strings"
)

// handleTimeline 返回一个 run 的执行时间线（P2-5）。
//
// 三种读法：
//   - 默认：span 树（根 = 没有父的 span，如 planner.round / intent.execute）；
//   - ?min_ms=500：只留慢 span（"P99 慢请求定位到具体 span"）；
//   - ?stats=1：按 span 名字聚合的 p50/p95/p99（先看分布，再看单条）。
func (s *Server) handleTimeline(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	since := intQuery64(r, "since", 0)
	minMs, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("min_ms")))
	name := strings.TrimSpace(r.URL.Query().Get("name"))

	spans, err := s.Store.TimelineSpans(r.Context(), id, since, intQuery(r, "limit", 1000))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	out := make([]*timelineNode, 0, len(spans))
	index := make(map[string]*timelineNode, len(spans))
	// 第一遍：建节点并过滤。
	for _, a := range spans {
		if minMs > 0 && a.DurationMs < minMs {
			continue
		}
		if name != "" && a.SpanName != name {
			continue
		}
		n := &timelineNode{
			SpanID: a.SpanID, ParentID: a.ParentSpanID, Name: a.SpanName,
			DurationMs: a.DurationMs, StartedAt: a.CreatedAt.UnixMilli(),
			Worker: a.Worker, Error: a.IsError, Attrs: a.Metadata,
		}
		if a.NodeID != nil {
			n.NodeID = *a.NodeID
		}
		index[a.SpanID] = n
	}
	// 第二遍：挂到父节点；父不在窗口里（被过滤/被 since 切掉）就当根。
	for _, a := range spans {
		n, ok := index[a.SpanID]
		if !ok {
			continue
		}
		if p, ok := index[a.ParentSpanID]; ok && a.ParentSpanID != "" {
			p.Children = append(p.Children, n)
			continue
		}
		out = append(out, n)
	}

	resp := map[string]any{
		"run_id": id,
		"trace":  traceIDForRun(id),
		"spans":  out,
		"count":  len(spans),
	}
	if r.URL.Query().Get("stats") != "" {
		if stats, err := s.Store.SpanStats(r.Context(), id); err == nil {
			resp["by_name"] = stats
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

type timelineNode struct {
	SpanID   string  `json:"span_id"`
	ParentID string  `json:"parent_span_id,omitempty"`
	Name     string  `json:"span"`
	NodeID   int64   `json:"node_id,omitempty"`
	Worker   string  `json:"worker,omitempty"`
	DurationMs int   `json:"duration_ms"`
	StartedAt  int64 `json:"started_at_ms"`
	Error      bool  `json:"error,omitempty"`
	Attrs      map[string]any `json:"attrs,omitempty"`
	Children   []*timelineNode `json:"children,omitempty"`
}

// traceIDForRun 与 trace.TraceIDFor 保持一致（server 不 import trace 只为一行字符串）。
func traceIDForRun(runID int64) string {
	return "qy-" + strconv.FormatInt(runID, 10)
}
