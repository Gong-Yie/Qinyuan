package store

import (
	"context"
	"testing"

	"qinyuan/internal/model"
)

// M5b 的两个图数据源：链路图要"全节点 + 全边"，覆盖图要"范围内 + 相关资产 + 父子边"。
func TestGraphAndCoverageGraph(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	run, err := s.CreateRun(ctx, CreateRunInput{
		Goal: "图数据", WorkerCount: 1,
		Scopes:       []model.Scope{{Kind: "ip", Value: "127.0.0.1", Source: "manual"}},
		SeedEntities: []EntityInput{{Kind: "ip", IP: "127.0.0.1", Segment: "127.0.0.0/24"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// 一条意图 + 一条锚定了 IP 的事实（覆盖图里 IP 变成"已测"）。
	intentID, err := s.AddIntent(ctx, AddIntentInput{RunID: run.ID, Summary: "抓首页"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFact(ctx, AddFactInput{
		RunID: run.ID, IntentID: intentID, Summary: "8080 开放",
		Evidence: "curl → 200", Confidence: "observed", AssetIDs: []int64{1},
	}); err != nil {
		t.Fatal(err)
	}
	// 相关但不范围内的资产：service + endpoint（同主机，靠 url 关联）。
	svcID, _, err := s.UpsertEntity(ctx, run.ID, EntityInput{
		Kind: "service", URL: "http://127.0.0.1:8080/", ServiceType: "http",
		Port: 8080, StatusCode: 200, PageTitle: "Acme",
	})
	if err != nil {
		t.Fatal(err)
	}
	epID, _, err := s.UpsertEntity(ctx, run.ID, EntityInput{
		Kind: "endpoint", URL: "http://127.0.0.1:8080/login", Method: "GET", StatusCode: 200,
	})
	if err != nil {
		t.Fatal(err)
	}

	nodes, edges, err := s.Graph(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) < 4 {
		t.Fatalf("链路图节点数 = %d，期望 >= 4（origin + goal + intent + fact）", len(nodes))
	}
	kinds := map[string]int{}
	for _, n := range nodes {
		kinds[n.Kind]++
	}
	if kinds["goal"] == 0 || kinds["intent"] == 0 || kinds["fact"] == 0 {
		t.Fatalf("链路图缺少关键 kind：%+v", kinds)
	}
	if len(edges) < 3 {
		t.Fatalf("链路图边数 = %d，期望 >= 3（spawns/derived_from/yields）", len(edges))
	}

	cn, ce, err := s.CoverageGraph(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[int64]CovNode{}
	for _, n := range cn {
		byID[n.EntityID] = n
	}
	ip, ok := byID[1]
	if !ok || !ip.InScope {
		t.Fatalf("IP 实体应在范围内：%+v", ip)
	}
	if !ip.Tested {
		t.Fatalf("被 fact 锚定的 IP 应标记为已测：%+v", ip)
	}
	if svc, ok := byID[svcID]; !ok || svc.InScope {
		t.Fatalf("service 应作为相关资产出现且 in_scope=false：%+v ok=%v", svc, ok)
	}
	if ep, ok := byID[epID]; !ok || ep.InScope {
		t.Fatalf("endpoint 应作为相关资产出现且 in_scope=false：%+v ok=%v", ep, ok)
	}
	// endpoint → service 的父子边（同 host:port）。
	want := map[string]bool{}
	for _, e := range ce {
		want[e.Src+"->"+e.Dst] = true
	}
	epKey := "a:" + itoa64(epID)
	svcKey := "a:" + itoa64(svcID)
	if !want[epKey+"->"+svcKey] {
		t.Fatalf("缺少 endpoint→service 的边，实际：%+v", ce)
	}
}
