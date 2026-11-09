package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"qinyuan/internal/domain"
	"qinyuan/internal/model"
	"qinyuan/internal/store"
	"qinyuan/internal/testkit"
)

func testContext(t *testing.T, scopes []model.Scope, ents []store.EntityInput, allowLoopback bool, intentID int64) (*Context, *model.Run) {
	t.Helper()
	st := testkit.Store(t)
	run, assetID := testkit.Run(t, st, scopes, ents)
	if intentID == 0 {
		var assets []int64
		if assetID > 0 {
			assets = []int64{assetID}
		}
		id, err := st.AddIntent(context.Background(), store.AddIntentInput{
			RunID: run.ID, Summary: "测试意图", AssetIDs: assets,
		})
		if err != nil {
			t.Fatalf("建意图失败：%v", err)
		}
		intentID = id
	}
	sc, err := st.Scopes(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return &Context{
		Store: st, RunID: run.ID, IntentID: intentID,
		Scopes: domain.NewScopeSet(sc), DataDir: t.TempDir(),
		AllowLoopback: allowLoopback, MaxOutput: 3000,
	}, run
}

// 范围闸门必须在"发请求之前"拒绝，而不是发出去再过滤。
func TestHTTPToolScopeGate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("范围外的请求竟然被发出去了")
	}))
	defer srv.Close()

	rc, _ := testContext(t, []model.Scope{{Kind: "subdomain", Domain: "example.test", Source: "manual"}},
		[]store.EntityInput{{Kind: "subdomain", Domain: "example.test", RootDomain: "example.test"}},
		true, 0)

	tool := NewHTTPTool()
	res, err := tool.Handle(context.Background(), rc, json.RawMessage(`{"url":"`+srv.URL+`/"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "范围闸门") {
		t.Fatalf("应当被范围闸门拒绝，实际：%v %s", res.IsError, res.Text)
	}
}

// 没有登记范围 = 什么都不授权（默认拒绝）。
func TestHTTPToolNoScopeDenies(t *testing.T) {
	rc, _ := testContext(t, nil, nil, true, 0)
	tool := NewHTTPTool()
	res, err := tool.Handle(context.Background(), rc, json.RawMessage(`{"url":"http://127.0.0.1:1/"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "授权范围") {
		t.Fatalf("无范围时必须拒绝，实际：%v %s", res.IsError, res.Text)
	}
}

// 拨号层：即使范围内，回环地址默认也不许连（SSRF 防护）。
func TestHTTPToolLoopbackBlockedByDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("回环地址竟然被连上了")
	}))
	defer srv.Close()

	rc, _ := testContext(t, []model.Scope{{Kind: "ip", Value: "127.0.0.1", Source: "manual"}},
		[]store.EntityInput{{Kind: "ip", IP: "127.0.0.1", Segment: "127.0.0.0/24"}},
		false, 0)

	tool := NewHTTPTool()
	res, err := tool.Handle(context.Background(), rc, json.RawMessage(`{"url":"`+srv.URL+`/"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "被拒绝") {
		t.Fatalf("回环地址应当被拨号层拒绝，实际：%v %s", res.IsError, res.Text)
	}
}

// 允许回环时应当真的打通，并返回结构化响应。
func TestHTTPToolAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "qinyuan-test/1.0")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("<html>ok</html>"))
	}))
	defer srv.Close()

	rc, _ := testContext(t, []model.Scope{{Kind: "ip", Value: "127.0.0.1", Source: "manual"}},
		[]store.EntityInput{{Kind: "ip", IP: "127.0.0.1", Segment: "127.0.0.0/24"}},
		true, 0)

	tool := NewHTTPTool()
	res, err := tool.Handle(context.Background(), rc, json.RawMessage(`{"url":"`+srv.URL+`/"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("应当成功，实际错误：%s", res.Text)
	}
	var out httpOutput
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("返回不是结构化 JSON：%v", err)
	}
	if out.Status != 200 || !strings.Contains(out.Body, "ok") {
		t.Fatalf("响应不对：status=%d body=%q", out.Status, out.Body)
	}
}

// insert_assets 幂等 + 逐条部分成功 + 越界条目拒绝。
func TestInsertAssetsPartialSuccessAndIdempotent(t *testing.T) {
	rc, _ := testContext(t, []model.Scope{
		{Kind: "subdomain", Domain: "example.test", Source: "manual"},
	}, []store.EntityInput{
		{Kind: "subdomain", Domain: "example.test", RootDomain: "example.test"},
	}, true, 0)

	tool := NewInsertAssetsTool()
	payload := json.RawMessage(`{"assets":[
		{"type":"endpoint","url":"https://example.test/api/user","method":"GET","status_code":200},
		{"type":"endpoint","url":"https://evil.test/x","method":"GET"},
		{"type":"endpoint","url":"https://example.test/api/user","method":"GET","page_title":"重复提交"}
	]}`)

	res, err := tool.Handle(context.Background(), rc, payload)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		OK      int `json:"ok"`
		Total   int `json:"total"`
		Results []struct {
			Index   int    `json:"index"`
			ID      int64  `json:"id"`
			Created bool   `json:"created"`
			Error   string `json:"error"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("返回不是 JSON：%v / %s", err, res.Text)
	}
	if out.Total != 3 || out.OK != 2 {
		t.Fatalf("部分成功语义不对：ok=%d total=%d（期望 2/3）", out.OK, out.Total)
	}
	if out.Results[1].Error == "" {
		t.Fatal("越界条目没有报错")
	}
	if out.Results[0].ID != out.Results[2].ID {
		t.Fatal("同一 endpoint 两次提交应命中同一行（幂等）")
	}
	if out.Results[2].Created {
		t.Fatal("第二次提交不该是 created")
	}

	var count int
	if err := rc.Store.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM entities WHERE kind='endpoint'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("实体行数 = %d，期望 1", count)
	}
}

// 证据门槛：没有 evidence 的 fact 与 finding 都必须被拒。
func TestEvidenceGate(t *testing.T) {
	rc, _ := testContext(t, []model.Scope{{Kind: "subdomain", Domain: "example.test", Source: "manual"}},
		[]store.EntityInput{{Kind: "subdomain", Domain: "example.test", RootDomain: "example.test"}}, true, 0)

	factRes, err := NewRecordFactTool().Handle(context.Background(), rc,
		json.RawMessage(`{"facts":[{"summary":"没有证据的结论"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	findRes, err := NewReportFindingTool().Handle(context.Background(), rc,
		json.RawMessage(`{"name":"疑似注入","summary":"看起来可注入"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(factRes.Text, "evidence") {
		t.Fatalf("无证据的 fact 应被拒：%s", factRes.Text)
	}
	if !strings.Contains(findRes.Text, "record_fact") {
		t.Fatalf("无证据的 finding 应引导去 record_fact：%s", findRes.Text)
	}
}

// planner 的 add_intent 必须在"建意图之前"过范围闸门：
// 越界实体既不能绑到意图上，也不能悄悄建出一个空意图。
func TestAddIntentScopeGate(t *testing.T) {
	rc, _ := testContext(t, []model.Scope{{Kind: "subdomain", Domain: "example.test", Source: "manual"}},
		[]store.EntityInput{{Kind: "subdomain", Domain: "example.test", RootDomain: "example.test"}}, true, 0)

	// 直接写一个范围外实体（绕过工具层，模拟"别的任务见过的资产"）。
	outID, _, err := rc.Store.UpsertEntity(context.Background(), rc.RunID, store.EntityInput{
		Kind: "subdomain", Domain: "evil.test", RootDomain: "evil.test",
	})
	if err != nil {
		t.Fatal(err)
	}

	tool := NewAddIntentTool()
	before, _ := rc.Store.CountByKind(context.Background(), rc.RunID, model.KindIntent)
	res, err := tool.Handle(context.Background(), rc, json.RawMessage(
		`{"intents":[{"summary":"越界方向","asset_ids":[`+itoa(outID)+`]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "不在本任务授权范围内") {
		t.Fatalf("应当被范围闸门拒绝：%s", res.Text)
	}
	if n, _ := rc.Store.CountByKind(context.Background(), rc.RunID, model.KindIntent); n != before {
		t.Fatalf("被拒绝的方向不该建出节点：before=%d after=%d", before, n)
	}

	// 范围内可以正常创建，并且自动连到 origin fact。
	// 注意：subdomain 规则是精确匹配，所以这里用 example.test 本身（子域要用 root_domain 规则）。
	inID, _, err := rc.Store.UpsertEntity(context.Background(), rc.RunID, store.EntityInput{
		Kind: "subdomain", Domain: "example.test", RootDomain: "example.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err = tool.Handle(context.Background(), rc, json.RawMessage(
		`{"intents":[{"summary":"范围内方向","asset_ids":[`+itoa(inID)+`]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Text, "error") {
		t.Fatalf("范围内方向不该失败：%s", res.Text)
	}
	if n, _ := rc.Store.CountByKind(context.Background(), rc.RunID, model.KindIntent); n != before+1 {
		t.Fatalf("应当只新增 1 条意图：before=%d after=%d", before, n)
	}
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}
