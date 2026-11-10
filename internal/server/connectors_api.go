package server

import (
	"encoding/json"
	"net/http"
	"path/filepath"

	"qinyuan/internal/connectors"
	"qinyuan/internal/model"
)

// 本文件是 P3-3 的外部集成口子：投递台账、工单状态回写、CI 导出/回写。

// handleConnectorDeliveries 列投递台账（"这条 finding 到底推到工单系统没有"）。
func (s *Server) handleConnectorDeliveries(w http.ResponseWriter, r *http.Request) {
	items, err := s.Store.DeliveriesByConnector(r.Context(), r.URL.Query().Get("connector"), intQuery(r, "limit", 100))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	enabled := []string{}
	if s.Connectors != nil {
		for _, sink := range s.Connectors.Sinks {
			enabled = append(enabled, sink.Name())
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items, "count": len(items), "enabled": enabled,
	})
}

// handleSyncTickets 拉外部工单状态并回写 finding（≤5 分钟是设计目标，这里按需触发）。
func (s *Server) handleSyncTickets(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if s.Connectors == nil {
		writeError(w, http.StatusConflict, "没有启用任何连接器（配置 QINYUAN_CONNECTOR_TICKET_URL）")
		return
	}
	n, err := s.Connectors.SyncTickets(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "applied": n})
}

// handleCIExport 把已确认的发现导出成 CI 脚本（落在 data/runs/<id>/ci/）。
//
// 只导出 verified：未复测的 PoC 进流水线只会误报。
func (s *Server) handleCIExport(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	dir := filepath.Join(s.dataDir(), "runs", itoa64(id), "ci")
	files, err := connectors.ExportCI(r.Context(), s.Store, id, dir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "dir": dir, "files": files, "count": len(files),
		"note": "run-all.sh 是入口；脚本失败即表示该发现已修复（或断言失效）",
	})
}

// handleCIResult CI 回归结果回写（verified=仍可复现 / failed=已修复）。
func (s *Server) handleCIResult(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var in struct {
		FindingID int64  `json:"finding_id"`
		Status    string `json:"status"`
		Note      string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	if in.FindingID <= 0 {
		writeError(w, http.StatusBadRequest, "finding_id 必填")
		return
	}
	rt, err := connectors.ImportCIResult(r.Context(), s.Store, id, in.FindingID, in.Status, in.Note)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// 留一条可见的过程记录：CI 改了结论，报告分区会跟着变。
	_, _ = s.Store.AppendActivity(r.Context(), &model.Activity{
		RunID: id, Worker: "ci", Kind: model.ActSystem,
		Summary: "CI 回归结果回写：finding #" + itoa64(in.FindingID) + " → " + rt.Status,
		Detail:  in.Note,
		Metadata: map[string]any{"finding_id": in.FindingID, "status": rt.Status, "source": "ci"},
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "retest_id": rt.ID, "status": rt.Status})
}

func (s *Server) dataDir() string {
	if s.Engine != nil && s.Engine.Cfg.DataDir != "" {
		return s.Engine.Cfg.DataDir
	}
	return "./data"
}

func itoa64(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
