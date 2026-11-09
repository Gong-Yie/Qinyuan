package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"qinyuan/internal/agent"
	"qinyuan/internal/llm"
	"qinyuan/internal/model"
	"qinyuan/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv("QINYUAN_TEST_DSN")
	if dsn == "" {
		dsn = "postgres://postgres@127.0.0.1:55432/qinyuan_test?sslmode=disable"
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("测试库不可用（%v）", err)
	}
	defer admin.Close(ctx)

	b := make([]byte, 6)
	_, _ = rand.Read(b)
	schema := "e_" + hex.EncodeToString(b)
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("建 schema 失败：%v", err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()

	s, err := store.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s.Close()
		cleanup, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			return
		}
		defer cleanup.Close(context.Background())
		_, _ = cleanup.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
	})
	return s
}

func testRegistry() *agent.Registry {
	return agent.NewRegistry(
		agent.NewHTTPTool(),
		agent.NewInsertAssetsTool(),
		agent.NewRecordFactTool(),
		agent.NewReportFindingTool(),
		agent.NewNodeDetailTool(),
		agent.NewListFindingsTool(),
	)
}

func newEngineFor(t *testing.T, st *store.Store, providers llm.Factory) *Engine {
	t.Helper()
	return New(st, providers, testRegistry(), Config{
		DataDir:       t.TempDir(),
		AllowLoopback: true,
		MaxTurns:      5,
		MaxDuration:   30 * time.Second,
		IdleSleep:     10 * time.Millisecond,
	}, nil)
}

func newLoopbackRun(t *testing.T, st *store.Store, target string) (*model.Run, int64) {
	t.Helper()
	run, err := st.CreateRun(context.Background(), store.CreateRunInput{
		Goal:        "测试 " + target,
		WorkerCount: 1,
		Scopes:      []model.Scope{{Kind: "ip", Value: "127.0.0.1", Source: "manual"}},
		SeedEntities: []store.EntityInput{{
			Kind: "ip", IP: "127.0.0.1", Segment: "127.0.0.0/24",
		}},
	})
	if err != nil {
		t.Fatalf("建 run 失败：%v", err)
	}
	ents, err := st.ListEntitiesForRun(context.Background(), run.ID, 5)
	if err != nil || len(ents) == 0 {
		t.Fatalf("种子实体缺失：%v", err)
	}
	return run, ents[0].ID
}

// T16 的一半：一段正常执行要把事实写回，并把意图落 done。
func TestWorkWritesFactAndDone(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Server", "qinyuan-test/1.0")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("<html><title>login</title></html>"))
	}))
	defer srv.Close()

	run, assetID := newLoopbackRun(t, st, srv.URL)
	intentID, err := st.AddIntent(ctx, store.AddIntentInput{
		RunID: run.ID, Summary: "请求首页并记录服务指纹", AssetIDs: []int64{assetID},
	})
	if err != nil {
		t.Fatal(err)
	}

	providers := llm.NewFakeFactory(
		llm.Step{ToolCalls: []llm.ToolCall{llm.NewToolCall("c1", "http_request",
			map[string]any{"method": "GET", "url": srv.URL + "/"})}, Usage: llm.Usage{Input: 100, Output: 20}},
		llm.Step{ToolCalls: []llm.ToolCall{llm.NewToolCall("c2", "record_fact", map[string]any{
			"facts": []map[string]any{{
				"summary": "首页可访问，Server 头为 qinyuan-test/1.0", "detail": "200",
				"evidence": "GET / → 200 Server: qinyuan-test/1.0", "confidence": "observed",
				"asset_ids": []int64{assetID},
			}},
		})}, Usage: llm.Usage{Input: 150, Output: 40}},
		llm.Step{Text: "完成：确认首页可访问。"},
	)
	eng := newEngineFor(t, st, providers)

	claimed, err := eng.Work(ctx, run.ID, "w1")
	if err != nil {
		t.Fatalf("执行失败：%v", err)
	}
	if !claimed {
		t.Fatal("应当领到意图")
	}
	if hits != 1 {
		t.Fatalf("http_request 实际请求次数 = %d，期望 1", hits)
	}

	intent, err := st.Intent(ctx, run.ID, intentID)
	if err != nil {
		t.Fatal(err)
	}
	if intent.State != model.StateDone {
		t.Fatalf("意图终态 = %s，期望 done", intent.State)
	}

	facts, err := st.NodesByKind(ctx, run.ID, model.KindFact, 10)
	if err != nil {
		t.Fatal(err)
	}
	var confirmed int
	for _, f := range facts {
		if f.State == model.StateConfirmed {
			confirmed++
		}
	}
	if confirmed != 1 {
		t.Fatalf("confirmed 事实数 = %d，期望 1", confirmed)
	}

	// activity：tool_use 与 tool_result 必须按 tool_use_id 配对。
	acts, err := st.ActivityTail(ctx, run.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	paired := map[string]bool{}
	for _, a := range acts {
		if a.Kind == model.ActToolResult && a.ToolUseID != "" {
			paired[a.ToolUseID] = true
		}
	}
	if !paired["c1"] || !paired["c2"] {
		t.Fatalf("tool_result 未配对：%v", paired)
	}

	usage, err := st.TokenTotals(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Input != 250 || usage.Output != 60 {
		t.Fatalf("token 记账 = %d/%d，期望 250/60", usage.Input, usage.Output)
	}
}

// 撞回合上限 ≠ 完成：必须落 exhausted，且之前写回的事实不能丢。
func TestMaxTurnsExhausted(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	run, assetID := newLoopbackRun(t, st, srv.URL)
	intentID, err := st.AddIntent(ctx, store.AddIntentInput{
		RunID: run.ID, Summary: "一直探下去", AssetIDs: []int64{assetID},
	})
	if err != nil {
		t.Fatal(err)
	}

	// 每回合都先写一条事实，再继续调工具 —— 撞上限时这些事实必须已经落库。
	steps := make([]llm.Step, 0, 8)
	for i := 0; i < 8; i++ {
		steps = append(steps,
			llm.Step{ToolCalls: []llm.ToolCall{llm.NewToolCall(fmt.Sprintf("f%d", i), "record_fact", map[string]any{
				"facts": []map[string]any{{
					"summary": fmt.Sprintf("第 %d 条观察", i), "evidence": "curl -i", "confidence": "observed",
					"asset_ids": []int64{assetID},
				}},
			})}},
			llm.Step{ToolCalls: []llm.ToolCall{llm.NewToolCall(fmt.Sprintf("h%d", i), "http_request",
				map[string]any{"url": srv.URL + "/"})}},
		)
	}
	eng := newEngineFor(t, st, llm.NewFakeFactory(steps...))
	eng.Cfg.MaxTurns = 3

	if _, err := eng.Work(ctx, run.ID, "w1"); err != nil {
		t.Fatalf("执行失败：%v", err)
	}
	intent, err := st.Intent(ctx, run.ID, intentID)
	if err != nil {
		t.Fatal(err)
	}
	if intent.State != model.StateExhausted {
		t.Fatalf("撞回合上限应落 exhausted，实际 %s", intent.State)
	}
	if n, _ := st.CountByKind(ctx, run.ID, model.KindFact); n < 3 {
		t.Fatalf("撞上限前写回的事实丢了：只有 %d 条", n)
	}
}

// 九分支收尾映射：每条分支单独验一次（顺序错了这里就会红）。
func TestSettleMapping(t *testing.T) {
	cases := []struct {
		name  string
		out   *agent.Outcome
		cause error
		want  string
	}{
		{"任务暂停", &agent.Outcome{Reason: agent.ReasonCanceled}, ErrPausedByUser, model.StateOpen},
		{"用户取消单条", &agent.Outcome{Reason: agent.ReasonCanceled}, ErrCancelledByUser, model.StatePaused},
		{"planner 杀掉", &agent.Outcome{Reason: agent.ReasonCanceled}, ErrKilledByPlanner, model.StateStopped},
		{"超时收尾", &agent.Outcome{Reason: agent.ReasonCanceled}, ErrTimeoutSettling, model.StateExhausted},
		{"进程退出", &agent.Outcome{Reason: agent.ReasonCanceled}, ErrShutdown, model.StateOpen},
		{"执行报错", &agent.Outcome{Reason: agent.ReasonError, Err: fmt.Errorf("boom")}, nil, model.StateBlocked},
		{"撞回合上限", &agent.Outcome{Reason: agent.ReasonMaxTurns}, nil, model.StateExhausted},
		{"正常完成", &agent.Outcome{Reason: agent.ReasonDone}, nil, model.StateDone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newTestStore(t)
			ctx := context.Background()
			run, _ := newLoopbackRun(t, st, "http://127.0.0.1:1")
			intentID, err := st.AddIntent(ctx, store.AddIntentInput{RunID: run.ID, Summary: "x"})
			if err != nil {
				t.Fatal(err)
			}
			if ok, err := st.ClaimIntent(ctx, run.ID, intentID, "w1"); err != nil || !ok {
				t.Fatalf("claim 失败：%v", err)
			}
			eng := newEngineFor(t, st, llm.NewFakeFactory())
			got := eng.settle(ctx, run.ID, intentID, tc.out, tc.cause)
			if got != tc.want {
				t.Fatalf("落状态 = %s，期望 %s", got, tc.want)
			}
			n, err := st.Intent(ctx, run.ID, intentID)
			if err != nil {
				t.Fatal(err)
			}
			if n.State != tc.want {
				t.Fatalf("DB 里是 %s，期望 %s", n.State, tc.want)
			}
		})
	}
}

// 暂停后不得领新活（暂停必须真的拦住执行，而不只是改个标记）。
func TestPausedDoesNotClaim(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	run, _ := newLoopbackRun(t, st, "http://127.0.0.1:1")
	if _, err := st.AddIntent(ctx, store.AddIntentInput{RunID: run.ID, Summary: "x"}); err != nil {
		t.Fatal(err)
	}
	eng := newEngineFor(t, st, llm.NewFakeFactory())
	if err := eng.Pause(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	claimed, err := eng.Work(ctx, run.ID, "w1")
	if err != nil {
		t.Fatal(err)
	}
	if claimed {
		t.Fatal("已暂停的任务不该再领意图")
	}
	active, _ := st.HasActiveIntent(ctx, run.ID)
	if !active {
		t.Fatal("暂停不该丢掉 open 意图")
	}
}
