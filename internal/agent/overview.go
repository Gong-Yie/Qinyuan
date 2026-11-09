package agent

import (
	"context"
	"fmt"
	"time"

	"qinyuan/internal/model"
	"qinyuan/internal/store"
)

// Brief 是给模型看的"一行摘要"，不是完整节点。
type Brief struct {
	ID         int64   `json:"id"`
	Kind       string  `json:"kind,omitempty"`
	State      string  `json:"state,omitempty"`
	Summary    string  `json:"summary"`
	Priority   int     `json:"priority,omitempty"`
	Confidence string  `json:"confidence,omitempty"`
	AssetIDs   []int64 `json:"asset_ids,omitempty"`
	FromIntent int64   `json:"from_intent,omitempty"`
	Yields     []int64 `json:"yields,omitempty"`
	Parents    []int64 `json:"parents,omitempty"`
}

// Overview 是 planner 每轮预取、worker 开局看一眼的态势。
//
// 三条渲染规则（01-数据模型 §10.5）：
//  1. 每个列表都有硬上限 + 总数（键名自解释：recent_done vs done_total）；
//  2. 摘要 ≤160 字，详情一律 node_detail 另取；
//  3. worker 版本会删掉 coverage —— 覆盖度是规划者信号，给执行者会诱导它去追未覆盖点。
type Overview struct {
	RunID         int64    `json:"run_id"`
	Goal          string   `json:"goal"`
	Status        string   `json:"status"`
	Round         int64    `json:"round"`
	Goals         []Brief  `json:"goals"`
	Hints         []Brief  `json:"hints"`
	Open          []Brief  `json:"open_intents"`
	Running       []Brief  `json:"running_intents"`
	RecentDone    []Brief  `json:"recent_done"`
	DoneTotal     int      `json:"done_total"`
	Frontier      int      `json:"frontier_open"`
	Facts         []Brief  `json:"recent_facts"`
	FactsTotal    int      `json:"facts_total"`
	Findings      []Brief  `json:"finding_list"`
	FindingsTotal int      `json:"findings_total"`
	Entities      int      `json:"entities"`
	Scopes        []string `json:"scopes"`
	GeneratedAt   int64    `json:"generated_at"`
}

// BuildOverview 组装态势。这就是 planner 每轮"预取"的那份 JSON：
// 预取的返回与 overview 工具的返回必须是同一份代码，才能保证"看到的"和"调到的"一致。
func BuildOverview(ctx context.Context, st *store.Store, runID int64) (*Overview, error) {
	run, err := st.Run(ctx, runID)
	if err != nil {
		return nil, err
	}
	scopes, err := st.Scopes(ctx, runID)
	if err != nil {
		return nil, err
	}
	edges, err := st.Edges(ctx, runID)
	if err != nil {
		return nil, err
	}

	fromIntent := map[int64]int64{}
	yields := map[int64][]int64{}
	parents := map[int64][]int64{}
	for _, e := range edges {
		switch e.Rel {
		case model.RelYields:
			fromIntent[e.DstID] = e.SrcID
			yields[e.SrcID] = append(yields[e.SrcID], e.DstID)
		case model.RelDerivedFrom:
			parents[e.DstID] = append(parents[e.DstID], e.SrcID)
		}
	}

	ov := &Overview{
		RunID:       run.ID,
		Goal:        run.Goal,
		Status:      run.Status,
		Round:       run.RoundNo,
		GeneratedAt: time.Now().Unix(),
	}
	if run.Task != nil {
		// 派生状态必须用真实计数，不能写死 —— 否则 overview 与列表页会给出两个状态。
		active, err := st.HasActiveIntent(ctx, runID)
		if err != nil {
			return nil, err
		}
		ov.Status = model.DeriveStatus(run.Task, active, false)
	}
	for _, sc := range scopes {
		ov.Scopes = append(ov.Scopes, describeScope(sc))
	}
	if ov.Scopes == nil {
		ov.Scopes = []string{}
	}

	if ov.Goals, err = briefs(ctx, st, runID, model.KindGoal, 20, nil); err != nil {
		return nil, err
	}
	if ov.Hints, err = briefs(ctx, st, runID, model.KindHint, 10, nil); err != nil {
		return nil, err
	}
	if ov.Facts, err = briefs(ctx, st, runID, model.KindFact, 20, fromIntent); err != nil {
		return nil, err
	}
	if ov.Findings, err = briefs(ctx, st, runID, model.KindFinding, 10, fromIntent); err != nil {
		return nil, err
	}
	if ov.Open, err = briefsByState(ctx, st, runID, model.StateOpen, 30, parents, yields); err != nil {
		return nil, err
	}
	if ov.Running, err = briefsByState(ctx, st, runID, model.StateRunning, 30, parents, yields); err != nil {
		return nil, err
	}
	if ov.RecentDone, err = briefsByState(ctx, st, runID, model.StateDone, 12, parents, yields); err != nil {
		return nil, err
	}

	if ov.Frontier, err = st.CountIntentState(ctx, runID, model.StateOpen); err != nil {
		return nil, err
	}
	if ov.DoneTotal, err = st.CountIntentState(ctx, runID, model.StateDone); err != nil {
		return nil, err
	}
	if ov.FactsTotal, err = st.CountByKind(ctx, runID, model.KindFact); err != nil {
		return nil, err
	}
	if ov.FindingsTotal, err = st.CountByKind(ctx, runID, model.KindFinding); err != nil {
		return nil, err
	}
	ents, err := st.ListEntitiesForRun(ctx, runID, 500)
	if err != nil {
		return nil, err
	}
	ov.Entities = len(ents)
	return ov, nil
}

func briefs(ctx context.Context, st *store.Store, runID int64, kind string, limit int, fromIntent map[int64]int64) ([]Brief, error) {
	nodes, err := st.NodesByKind(ctx, runID, kind, limit)
	if err != nil {
		return nil, err
	}
	out := make([]Brief, 0, len(nodes))
	for _, n := range nodes {
		b := Brief{
			ID:      n.ID,
			Kind:    n.Kind,
			State:   n.State,
			Summary: model.TrimSummary(n.Summary(), 160),
		}
		if kind == model.KindFact || kind == model.KindFinding {
			b.Confidence = n.PayloadField("confidence")
			b.FromIntent = fromIntent[n.ID]
		}
		out = append(out, b)
	}
	return out, nil
}

func briefsByState(ctx context.Context, st *store.Store, runID int64, state string, limit int, parents, yields map[int64][]int64) ([]Brief, error) {
	nodes, err := st.NodesByKind(ctx, runID, model.KindIntent, 500)
	if err != nil {
		return nil, err
	}
	out := make([]Brief, 0, limit)
	for _, n := range nodes {
		if n.State != state {
			continue
		}
		out = append(out, Brief{
			ID:       n.ID,
			Kind:     n.Kind,
			State:    n.State,
			Summary:  model.TrimSummary(n.Summary(), 160),
			Priority: n.Priority,
			Parents:  parents[n.ID],
			Yields:   yields[n.ID],
		})
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func describeScope(sc model.Scope) string {
	switch sc.Kind {
	case "root_domain":
		return "*." + sc.Domain
	case "subdomain":
		return sc.Domain
	case "cidr":
		return sc.Net
	case "ip":
		return firstNonEmptyStr(sc.Value, sc.Domain)
	case "org":
		return fmt.Sprintf("org#%d", sc.OrgID)
	default:
		return firstNonEmptyStr(sc.Value, sc.Domain)
	}
}
