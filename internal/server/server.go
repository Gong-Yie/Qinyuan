// Package server 是控制台的 HTTP 入口（M1：JSON API；M3：内嵌前端）。
//
// 这里只做三件事：解析请求 → 调 store/engine → 出 DTO。业务规则不写在这一层。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"qinyuan/internal/agent"
	"qinyuan/internal/engine"
	"qinyuan/internal/model"
	"qinyuan/internal/runplan"
	"qinyuan/internal/store"
)

// Version 是构建版本（M1 手工维护，M4 起由 -ldflags 注入）。
const Version = "0.1.0-m1"

// Server 组合依赖。
type Server struct {
	Store  *store.Store
	Engine *engine.Engine
	Log    *slog.Logger
}

// New 构造。
func New(st *store.Store, eng *engine.Engine, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{Store: st, Engine: eng, Log: log}
}

// Handler 返回路由表。
//
// 路由用 Go 1.22 的 method+pattern 语法；后续按资源拆文件时不动这里的方法签名。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /api/runs", s.handleListRuns)
	mux.HandleFunc("POST /api/runs", s.handleCreateRun)
	mux.HandleFunc("GET /api/runs/{id}", s.handleGetRun)
	mux.HandleFunc("DELETE /api/runs/{id}", s.handleDeleteRun)
	mux.HandleFunc("POST /api/runs/{id}/control", s.handleControl)
	mux.HandleFunc("GET /api/runs/{id}/nodes", s.handleNodes)
	mux.HandleFunc("GET /api/runs/{id}/intents", s.handleIntents)
	mux.HandleFunc("POST /api/runs/{id}/intents", s.handleAddIntent)
	mux.HandleFunc("GET /api/runs/{id}/activity", s.handleActivity)
	mux.HandleFunc("GET /api/runs/{id}/overview", s.handleOverview)
	mux.HandleFunc("GET /api/runs/{id}/events", s.handleEvents)
	mux.HandleFunc("GET /api/runs/{id}/coverage", s.handleCoverage)
	mux.HandleFunc("POST /api/runs/{id}/work", s.handleWork)

	return withMiddleware(mux, s.Log)
}

func withMiddleware(next http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		defer func() {
			if rec := recover(); rec != nil {
				log.Error("HTTP panic", "path", r.URL.Path, "panic", rec)
				writeError(w, http.StatusInternalServerError, "服务内部错误")
			}
			log.Info("[http]", "method", r.Method, "path", r.URL.Path, "status", rec.status,
				"ms", time.Since(start).Milliseconds())
		}()
		next.ServeHTTP(rec, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	err := s.Store.Ping(ctx)
	writeJSON(w, http.StatusOK, map[string]any{
		"version": Version,
		"ready":   err == nil,
		"db":      err == nil,
		// M1 只有假 provider；真实模型在 M2 接入。
		"llm_configured": false,
	})
}

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := s.Store.ListRuns(r.Context(), 100)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	items := make([]map[string]any, 0, len(runs))
	for _, run := range runs {
		items = append(items, runDTO(r.Context(), s.Store, run))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": len(items)})
}

type createRunReq struct {
	Goal           string `json:"goal"`
	Description    string `json:"description"`
	Name           string `json:"name"`
	Target         string `json:"target"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	WorkerCount    int    `json:"worker_count"`
}

func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	var req createRunReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	in, err := runplan.Build(runplan.Request{
		Goal: req.Goal, Description: req.Description, Name: req.Name,
		Target: req.Target, TimeoutSeconds: req.TimeoutSeconds, WorkerCount: req.WorkerCount,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	run, err := s.Store.CreateRun(r.Context(), in)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, runDTO(r.Context(), s.Store, run))
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	run, err := s.Store.Run(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "任务不存在")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := runDTO(r.Context(), s.Store, run)
	out["intents"] = nodeList(r, s, id, model.KindIntent, 100)
	out["goals"] = nodeList(r, s, id, model.KindGoal, 50)
	writeJSON(w, http.StatusOK, out)
}

// handleDeleteRun 删任务：屏障 → 排水 → 文件暂存 → DB 级联删 → 提交后真删。
//
// 与 pause 的区别：pause 可恢复、保留全部数据；delete 不可回退。
// 幂等：已经在删/已经删掉都返回 200（调用方重试安全）。
func (s *Server) handleDeleteRun(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if err := s.Engine.Delete(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": false, "note": "任务不存在（视为已删除）"})
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": true})
}

func (s *Server) handleControl(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var req struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	ctx := r.Context()
	var err error
	switch req.Action {
	case "start":
		err = s.Engine.Start(ctx, id)
	case "pause":
		err = s.Engine.Pause(ctx, id)
	case "resume":
		err = s.Engine.Resume(ctx, id)
	case "stop":
		// 钦原没有"停机保号"这个状态：stop 就是删除（屏障 → 排水 → 两阶段清理）。
		// 想保留数据请用 pause（可恢复）。
		err = s.Engine.Delete(ctx, id)
		if err == nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "action": "deleted"})
			return
		}
	default:
		writeError(w, http.StatusBadRequest, "action 只支持 start/pause/resume/stop（stop = 删除）")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "action": req.Action})
}

func (s *Server) handleNodes(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	kind := r.URL.Query().Get("kind")
	limit := intQuery(r, "limit", 100)
	nodes, err := s.Store.NodesByKind(r.Context(), id, kind, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	items := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		items = append(items, map[string]any{
			"id": n.ID, "kind": n.Kind, "state": n.State, "priority": n.Priority,
			"summary": model.TrimSummary(n.Summary(), 160), "created_at": n.CreatedAt.Unix(),
		})
	}
	total, _ := s.Store.CountByKind(r.Context(), id, kind)
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": total})
}

func (s *Server) handleIntents(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": nodeList(r, s, id, model.KindIntent, 100)})
}

func (s *Server) handleAddIntent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var req struct {
		Summary   string  `json:"summary"`
		AssetIDs  []int64 `json:"asset_ids"`
		ParentIDs []int64 `json:"parent_ids"`
		Priority  int     `json:"priority"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	intentID, err := s.Store.AddIntent(r.Context(), store.AddIntentInput{
		RunID: id, Summary: req.Summary, AssetIDs: req.AssetIDs,
		ParentIDs: req.ParentIDs, Priority: req.Priority, Origin: "manual",
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": intentID})
}

func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	limit := intQuery(r, "limit", 200)
	acts, err := s.Store.Activities(r.Context(), id, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	items := make([]map[string]any, 0, len(acts))
	for _, a := range acts {
		items = append(items, map[string]any{
			"id": a.ID, "node_id": a.NodeID, "worker": a.Worker, "kind": a.Kind,
			"tool": a.Tool, "tool_use_id": a.ToolUseID, "is_error": a.IsError,
			"summary": a.Summary, "created_at": a.CreatedAt.Unix(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "count": len(items)})
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	ov, err := agent.BuildOverview(r.Context(), s.Store, id, true)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ov)
}

// handleEvents 是差分态势的读口（P1-2）：返回 (since, 当前版本] 内的变化。
//
// planner 内部用的是同一份 store.DeltaSince；这里暴露出来是为了排障与控制台
// （"这一轮到底发生了什么"不该只能靠猜 prompt）。
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	since, _ := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("since")), 10, 64)
	d, err := s.Store.DeltaSince(r.Context(), id, since, intQuery(r, "limit", 200))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "任务不存在")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	nodes := make([]map[string]any, 0, len(d.Nodes))
	for _, n := range d.Nodes {
		nodes = append(nodes, map[string]any{
			"id": n.ID, "kind": n.Kind, "state": n.State, "rev": n.Rev,
			"priority": n.Priority, "summary": model.TrimSummary(n.Summary(), 160),
			"payload": json.RawMessage(n.Payload),
		})
	}
	edges := make([]map[string]any, 0, len(d.Edges))
	for _, e := range d.Edges {
		edges = append(edges, map[string]any{
			"src": e.SrcID, "rel": e.Rel, "dst": e.DstID, "rev": e.Rev,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"from": d.From, "to": d.To, "total": d.Total, "truncated": d.Truncated,
		"nodes": nodes, "edges": edges,
	})
}

func (s *Server) handleCoverage(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	cov, err := s.Store.Coverage(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, cov)
}

func (s *Server) handleWork(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	claimed, err := s.Engine.Work(r.Context(), id, "http#1")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"claimed": claimed})
}

// runDTO 是列表/详情共用的 DTO：派生状态、计数、能力布尔都在这里算好，
// 前端不再自己推导（升级计划 P2-2：全站一份）。
func runDTO(ctx context.Context, st *store.Store, run *model.Run) map[string]any {
	activeIntent, _ := st.HasActiveIntent(ctx, run.ID)
	status := model.DeriveStatus(run.Task, activeIntent, false)
	out := map[string]any{
		"id":          run.ID,
		"goal":        run.Goal,
		"description": run.Description,
		"status":      status,
		"round":       run.RoundNo,
		"version":     run.Version,
		"created_at":  run.CreatedAt.Unix(),
		"updated_at":  run.UpdatedAt.Unix(),
		"can_pause":   status == model.TaskRunning || status == "idle",
		"can_resume":  status == model.TaskPaused,
	}
	if run.Task != nil {
		out["name"] = run.Task.Name
		out["task_status"] = run.Task.Status
		out["paused"] = run.Task.Paused
		out["queued"] = run.Task.Queued
		out["worker_count"] = run.Task.WorkerCount
		out["timeout_seconds"] = run.Task.TimeoutSeconds
		out["goals_at"] = run.Task.GoalsAt
	}
	return out
}

func nodeList(r *http.Request, s *Server, runID int64, kind string, limit int) []map[string]any {
	nodes, err := s.Store.NodesByKind(r.Context(), runID, kind, limit)
	if err != nil {
		return []map[string]any{}
	}
	out := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, map[string]any{
			"id": n.ID, "kind": n.Kind, "state": n.State, "priority": n.Priority,
			"summary": model.TrimSummary(n.Summary(), 160), "updated_at": n.UpdatedAt.Unix(),
		})
	}
	return out
}

func pathID(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	raw := r.PathValue(name)
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("非法 %s：%q", name, raw))
		return 0, false
	}
	return id, true
}

func intQuery(r *http.Request, name string, def int) int {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
