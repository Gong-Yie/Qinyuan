package server

import (
	"encoding/json"
	"net/http"

	"qinyuan/internal/model"
	"qinyuan/internal/store"
)

// 本文件是 P3-1 的战役读口：建战役、挂子 run、看聚合、级联暂停/恢复。

func (s *Server) handleCreateCampaign(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name   string  `json:"name"`
		Goal   string  `json:"goal"`
		OrgIDs []int64 `json:"org_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	c, err := s.Store.CreateCampaign(r.Context(), in.Name, in.Goal, in.OrgIDs)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (s *Server) handleListCampaigns(w http.ResponseWriter, r *http.Request) {
	items, err := s.Store.ListCampaigns(r.Context(), intQuery(r, "limit", 50))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "count": len(items)})
}

// handleCampaign 是战役视图：三个聚合（覆盖度/进度/重复实体）+ 子 run 列表。
func (s *Server) handleCampaign(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "cid")
	if !ok {
		return
	}
	sum, err := s.Store.CampaignSummary(r.Context(), id)
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "战役不存在")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sum)
}

// handleCampaignControl 级联暂停/恢复：战役一停，所有子 run 停（3 秒内，见验收）。
func (s *Server) handleCampaignControl(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "cid")
	if !ok {
		return
	}
	var in struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	ctx := r.Context()
	runs, err := s.Store.CampaignRuns(ctx, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	affected := 0
	switch in.Action {
	case "pause":
		if _, err := s.Store.SetCampaignStatus(ctx, id, "paused"); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		for _, cr := range runs {
			if model.IsTerminalTaskState(cr.Status) {
				continue
			}
			if err := s.Engine.Pause(ctx, cr.RunID); err == nil {
				affected++
			}
		}
	case "resume":
		if _, err := s.Store.SetCampaignStatus(ctx, id, "active"); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		for _, cr := range runs {
			if model.IsTerminalTaskState(cr.Status) {
				continue
			}
			if err := s.Engine.Resume(ctx, cr.RunID); err == nil {
				affected++
			}
		}
	case "done", "archive":
		ok, err := s.Store.SetCampaignStatus(ctx, id, in.Action)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, "战役不存在")
			return
		}
	default:
		writeError(w, http.StatusBadRequest, "action 只支持 pause/resume/done/archive")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "action": in.Action, "affected_runs": affected})
}

// handleAttachCampaignRun 把子 run 挂到战役（或解绑：campaign_id=0）。
func (s *Server) handleAttachCampaignRun(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "cid")
	if !ok {
		return
	}
	var in struct {
		RunID int64  `json:"run_id"`
		Role  string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	if in.RunID <= 0 {
		writeError(w, http.StatusBadRequest, "run_id 必填")
		return
	}
	if err := s.Store.AttachRun(r.Context(), in.RunID, id, in.Role); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "run_id": in.RunID, "campaign_id": id})
}

// handleCampaignScope 登记战役范围（子 run 继承；只许减不许加由人把关）。
func (s *Server) handleCampaignScope(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "cid")
	if !ok {
		return
	}
	if r.Method == http.MethodGet {
		items, err := s.Store.CampaignScopes(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items, "count": len(items)})
		return
	}
	var in store.CampaignScope
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	if err := s.Store.AddCampaignScope(r.Context(), id, in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
