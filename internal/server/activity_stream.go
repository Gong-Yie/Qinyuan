package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"qinyuan/internal/model"
)

// activityDTO 是给控制台的活动 DTO：列表不取 detail（原文另走 /activity/{aid}）。
func activityDTO(a *model.Activity) map[string]any {
	out := map[string]any{
		"id": a.ID, "node_id": a.NodeID, "worker": a.Worker, "kind": a.Kind,
		"tool": a.Tool, "tool_use_id": a.ToolUseID, "is_error": a.IsError,
		"summary": a.Summary, "created_at": a.CreatedAt.Unix(),
	}
	if a.InputTokens > 0 || a.OutputTokens > 0 || a.CacheReadTokens > 0 {
		out["input_tokens"] = a.InputTokens
		out["output_tokens"] = a.OutputTokens
		out["cache_read_tokens"] = a.CacheReadTokens
	}
	if len(a.Metadata) > 0 {
		out["metadata"] = a.Metadata
	}
	return out
}

const activityEventName = "activity"

// handleActivityStream 是任务级的一条 SSE 流。
//
// 为什么是"任务级一条"而不是"每个会话一条"：会话数 = 意图数，会涨；单条流 + 前端按
// node_id 路由到各会话，连接数可控且全局顺序天然一致。
//
// 断线续传：客户端带 `?since=<最后收到的 id>` 或 `Last-Event-ID`；服务端先**订阅**再回放
// （since, 现在] 的缺口 —— 顺序反了会漏掉两者之间的窗口；重复由前端按 id 去重。
func (s *Server) handleActivityStream(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "当前连接不支持流式输出")
		return
	}

	since := intQuery64(r, "since", 0)
	if since == 0 {
		if v := r.Header.Get("Last-Event-ID"); v != "" {
			since, _ = strconv.ParseInt(v, 10, 64)
		}
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // 反代别缓冲，否则连上了也 0 帧
	w.WriteHeader(http.StatusOK)

	// 先写一帧注释：让客户端（和反代）立刻认为连接已建立。
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	ch, cancel := s.Bus.Subscribe(id)
	defer cancel()

	if since > 0 {
		gap, err := s.Store.ActivitiesAfter(r.Context(), id, since, 500)
		if err != nil {
			s.Log.Warn("[sse] 回放缺口失败", "run", id, "since", since, "err", err)
		}
		for _, a := range gap {
			if !writeActivityFrame(w, a) {
				return
			}
		}
		flusher.Flush()
	}

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case a, ok := <-ch:
			if !ok {
				return
			}
			if !writeActivityFrame(w, a) {
				return
			}
			flusher.Flush()
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// writeActivityFrame 写一帧；返回 false 表示对端已断，调用方应当退出。
//
// 带 `id:` 是必须的：浏览器的 EventSource 重连会自动带 Last-Event-ID。
func writeActivityFrame(w http.ResponseWriter, a *model.Activity) bool {
	raw, err := json.Marshal(activityDTO(a))
	if err != nil {
		return true
	}
	if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", a.ID, activityEventName, raw); err != nil {
		return false
	}
	return true
}

// handleActivityItem 取单条活动详情（含 detail 全文）；列表里不带的原文在这里。
func (s *Server) handleActivityItem(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	aid, ok := pathID(w, r, "aid")
	if !ok {
		return
	}
	a, err := s.Store.ActivityDetail(r.Context(), aid)
	if err != nil {
		writeError(w, http.StatusNotFound, "活动记录不存在")
		return
	}
	if a.RunID != id {
		writeError(w, http.StatusNotFound, "活动记录不存在于本任务")
		return
	}
	out := activityDTO(a)
	out["detail"] = a.Detail
	writeJSON(w, http.StatusOK, out)
}
