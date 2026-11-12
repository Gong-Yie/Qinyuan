package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"qinyuan/internal/store"
	"qinyuan/internal/testkit"
)

func TestSetRunApprovalModeAPI(t *testing.T) {
	st := testkit.Store(t)
	srv := New(st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	run, err := st.CreateRun(context.Background(), store.CreateRunInput{
		Goal: "审批模式 API", WorkerCount: 1, ApprovalMode: "manual",
	})
	if err != nil {
		t.Fatal(err)
	}

	do := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", fmt.Sprintf("/api/runs/%d/approval-mode", run.ID), strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	rec := do(`{"mode":"auto"}`)
	if rec.Code != 200 {
		t.Fatalf("切 auto 应 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["mode"] != "auto" {
		t.Fatalf("响应模式不对：%+v", out)
	}
	mode, err := st.RunApprovalMode(context.Background(), run.ID)
	if err != nil || mode != "auto" {
		t.Fatalf("DB 模式应为 auto，实际 %q err=%v", mode, err)
	}

	if rec := do(`{"mode":"bogus"}`); rec.Code != 400 {
		t.Fatalf("非法模式应 400，实际 %d", rec.Code)
	}
}

// 建任务入参也要能带 approval_mode，并在 DTO 里回显。
func TestCreateRunApprovalModeAPI(t *testing.T) {
	st := testkit.Store(t)
	srv := New(st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	req := httptest.NewRequest("POST", "/api/runs", strings.NewReader(
		`{"goal":"审批模式建任务","worker_count":1,"approval_mode":"auto"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("建任务应 201，实际 %d：%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["approval_mode"] != "auto" {
		t.Fatalf("DTO 应回显 auto：%+v", out)
	}
}
