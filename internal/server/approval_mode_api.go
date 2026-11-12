package server

import (
	"encoding/json"
	"net/http"

	"qinyuan/internal/model"
)

// handleSetRunApprovalMode 切 run 级审批模式（operator/admin）。
//
// 切到 auto/off 时顺手把该 run 当前 pending 的审批全部放行：
// 否则已经卡在 Gate.ask 里的工具调用会继续等，切模式看起来"没生效"。
func (s *Server) handleSetRunApprovalMode(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var in struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	mode, ok := model.NormalizeApprovalMode(in.Mode)
	if !ok {
		writeError(w, http.StatusBadRequest, "mode 只能是 manual/auto/off/inherit")
		return
	}
	changed, err := s.Store.SetRunApprovalMode(r.Context(), id, mode)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !changed {
		writeError(w, http.StatusNotFound, "任务不存在")
		return
	}

	approved := 0
	if mode == model.ApprovalModeAuto || mode == model.ApprovalModeOff {
		approved, _ = s.Store.ApprovePendingForRun(r.Context(), id, "auto-mode",
			"审批模式切换为 "+mode+"，自动放行")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "mode": mode, "approved_pending": approved,
	})
}
