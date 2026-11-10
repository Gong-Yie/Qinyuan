package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"qinyuan/internal/model"
	"qinyuan/internal/store"
	"qinyuan/internal/testkit"
)

func retestFixture(t *testing.T, srvURL string, status int, body string) (*store.Store, *model.Run, int64) {
	t.Helper()
	st := testkit.Store(t)
	ctx := context.Background()
	run, err := st.CreateRun(ctx, store.CreateRunInput{
		Goal: "复测", WorkerCount: 1,
		Scopes:       []model.Scope{{Kind: "ip", Value: "127.0.0.1", Source: "manual"}},
		SeedEntities: []store.EntityInput{{Kind: "ip", IP: "127.0.0.1", Segment: "127.0.0.0/24"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	intentID, err := st.AddIntent(ctx, store.AddIntentInput{RunID: run.ID, Summary: "打一下"})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(body))
	exID, err := st.RecordTraffic(ctx, store.TrafficInput{
		RunID: run.ID, IntentID: intentID, Method: "GET", URL: srvURL + "/x",
		Status: status, ContentType: "text/plain", RespLen: len(body), RespBody: body,
		RespSHA: hex.EncodeToString(sum[:]), DurationMS: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	fid, err := st.AddFinding(ctx, store.AddFindingInput{
		RunID: run.ID, IntentID: intentID, Name: "信息泄露", Severity: "low",
		Summary: "响应里带内部标识", Evidence: "GET /x → " + body,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.BindFindingTraffic(ctx, run.ID, fid, []int64{exID}); err != nil {
		t.Fatal(err)
	}
	return st, run, fid
}

// 重放结果一致 → verified；目标变了 → failed。
func TestRetestVerifiedAndFailed(t *testing.T) {
	var fail atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		if fail.Load() {
			w.WriteHeader(500)
			_, _ = w.Write([]byte("boom"))
			return
		}
		_, _ = w.Write([]byte("internal-id-123"))
	}))
	defer srv.Close()

	st, run, fid := retestFixture(t, srv.URL, 200, "internal-id-123")
	rows, err := RetestFinding(context.Background(), st, run.ID, fid, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != store.RetestVerified {
		t.Fatalf("应 verified，实际 %+v", rows)
	}
	if rows[0].Observed == "" || rows[0].Expected == "" {
		t.Fatalf("expected/observed 必须落库：%+v", rows[0])
	}

	// 目标变了（500）→ failed
	fail.Store(true)
	rows, err = RetestFinding(context.Background(), st, run.ID, fid, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != store.RetestFailed {
		t.Fatalf("应 failed，实际 %+v", rows)
	}

	// 历史留了两条，倒序
	hist, err := st.Retests(context.Background(), run.ID, fid, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 2 || hist[0].Status != store.RetestFailed {
		t.Fatalf("复测历史不对：%+v", hist)
	}
}

// 没有绑定证据 → unverifiable，并且明确说"没传 traffic_ids"。
func TestRetestWithoutEvidenceIsUnverifiable(t *testing.T) {
	st := testkit.Store(t)
	ctx := context.Background()
	run, err := st.CreateRun(ctx, store.CreateRunInput{Goal: "无证据", WorkerCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	fid, err := st.AddFinding(ctx, store.AddFindingInput{
		RunID: run.ID, Name: "疑似", Severity: "low", Summary: "只有文字证据",
		Evidence: "目测像漏洞",
	})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := RetestFinding(ctx, st, run.ID, fid, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != store.RetestUnverifiable {
		t.Fatalf("应 unverifiable，实际 %+v", rows)
	}
}
