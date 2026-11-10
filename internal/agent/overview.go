package agent

import (
	"context"
	"fmt"
	"time"

	"qinyuan/internal/graphstore"
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

// PlanBrief 是计划图给模型看的摘要：只给标题 + ready 步骤 + 计数。
type PlanBrief struct {
	PlanID     int64   `json:"plan_id"`
	Title      string  `json:"title"`
	Strategy   string  `json:"strategy,omitempty"`
	Revision   int     `json:"revision"`
	StepsDone  int     `json:"steps_done"`
	StepsTotal int     `json:"steps_total"`
	Ready      []Brief `json:"ready_steps"`
}

// Overview 是 planner 每轮预取、worker 开局看一眼的态势。
//
// 三条渲染规则（01-数据模型 §10.5）：
//  1. 每个列表都有硬上限 + 总数（键名自解释：recent_done vs done_total）；
//  2. 摘要 ≤160 字，详情一律 node_detail 另取；
//  3. worker 版本不带 coverage —— 覆盖度是规划者信号，给执行者会诱导它去追未覆盖点。
type Overview struct {
	RunID         int64           `json:"run_id"`
	Goal          string          `json:"goal"`
	Status        string          `json:"status"`
	Round         int64           `json:"round"`
	Goals         []Brief         `json:"goals"`
	Hints         []Brief         `json:"hints"`
	Plan          *PlanBrief      `json:"plan,omitempty"`
	Open          []Brief         `json:"open_intents"`
	Running       []Brief         `json:"running_intents"`
	RecentDone    []Brief         `json:"recent_done"`
	DoneTotal     int             `json:"done_total"`
	Frontier      int             `json:"frontier_open"`
	Facts         []Brief         `json:"recent_facts"`
	FactsTotal    int             `json:"facts_total"`
	Findings      []Brief         `json:"finding_list"`
	FindingsTotal int             `json:"findings_total"`
	Entities      int             `json:"entities"`
	Scopes        []string        `json:"scopes"`
	Coverage      *store.Coverage `json:"coverage,omitempty"`
	// Campaign 只在这个 run 属于某个战役时出现（零注入：独立任务看不到它）。
	Campaign    *store.CampaignView `json:"campaign,omitempty"`
	GeneratedAt   int64           `json:"generated_at"`
}

// BuildOverview 组装态势。withCoverage=false（worker）时不带覆盖度。
// 预取的返回与 overview 工具的返回必须是同一份代码，才能保证"看到的"和"调到的"一致。
func BuildOverview(ctx context.Context, g graphstore.Graph, runID int64, withCoverage bool) (*Overview, error) {
	run, err := g.Run(ctx, runID)
	if err != nil {
		return nil, err
	}
	scopes, err := g.Scopes(ctx, runID)
	if err != nil {
		return nil, err
	}
	edges, err := g.Edges(ctx, runID)
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
		active, err := g.HasActiveIntent(ctx, runID)
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

	if ov.Goals, err = briefs(ctx, g, runID, model.KindGoal, 20, nil); err != nil {
		return nil, err
	}
	if ov.Hints, err = briefs(ctx, g, runID, model.KindHint, 10, nil); err != nil {
		return nil, err
	}
	if ov.Facts, err = briefs(ctx, g, runID, model.KindFact, 20, fromIntent); err != nil {
		return nil, err
	}
	if ov.Findings, err = briefs(ctx, g, runID, model.KindFinding, 10, fromIntent); err != nil {
		return nil, err
	}
	if ov.Open, err = briefsByState(ctx, g, runID, model.StateOpen, 30, parents, yields); err != nil {
		return nil, err
	}
	if ov.Running, err = briefsByState(ctx, g, runID, model.StateRunning, 30, parents, yields); err != nil {
		return nil, err
	}
	if ov.RecentDone, err = briefsByState(ctx, g, runID, model.StateDone, 12, parents, yields); err != nil {
		return nil, err
	}

	if ov.Frontier, err = g.CountIntentState(ctx, runID, model.StateOpen); err != nil {
		return nil, err
	}
	if ov.DoneTotal, err = g.CountIntentState(ctx, runID, model.StateDone); err != nil {
		return nil, err
	}
	if ov.FactsTotal, err = g.CountByKind(ctx, runID, model.KindFact); err != nil {
		return nil, err
	}
	if ov.FindingsTotal, err = g.CountByKind(ctx, runID, model.KindFinding); err != nil {
		return nil, err
	}
	ents, err := g.ListEntitiesForRun(ctx, runID, 500)
	if err != nil {
		return nil, err
	}
	ov.Entities = len(ents)

	// 计划图摘要：只给标题、计数与 ready 步骤 —— planner 的"本轮只能从 ready 里选"靠它。
	if pg, err := g.PlanGraph(ctx, runID); err == nil && pg.PlanID > 0 {
		pb := &PlanBrief{PlanID: pg.PlanID, Title: pg.Title, Strategy: pg.Strategy, Revision: pg.Revision}
		for _, s := range pg.Steps {
			pb.StepsTotal++
			if s.State == model.StateDone {
				pb.StepsDone++
			}
			if s.State == model.StateReady && len(pb.Ready) < 20 {
				pb.Ready = append(pb.Ready, Brief{
					ID: s.ID, Kind: "step", State: s.State,
					Summary: model.TrimSummary(stepSummary(s), 160), Priority: s.Priority,
				})
			}
		}
		ov.Plan = pb
	}

	if withCoverage {
		cov, err := g.Coverage(ctx, runID)
		if err != nil {
			return nil, err
		}
		ov.Coverage = cov
	}
	// P3-1：同战役上下文（别的子 run 已经测过什么、重复投入有多少）。
	// 读失败不该让规划失败 —— 战役是增强，不是前置条件。
	if cv, cerr := g.CampaignViewForRun(ctx, runID); cerr == nil {
		ov.Campaign = cv
	}
	return ov, nil
}

// stepSummary 把"做什么 + 期望证据"拼成一行，供列表展示。
func stepSummary(s store.StepView) string {
	if s.Expected == "" {
		return s.Text
	}
	return s.Text + "（期望证据：" + s.Expected + "）"
}

func briefs(ctx context.Context, g graphstore.Graph, runID int64, kind string, limit int, fromIntent map[int64]int64) ([]Brief, error) {
	nodes, err := g.NodesByKind(ctx, runID, kind, limit)
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

func briefsByState(ctx context.Context, g graphstore.Graph, runID int64, state string, limit int, parents, yields map[int64][]int64) ([]Brief, error) {
	nodes, err := g.NodesByKind(ctx, runID, model.KindIntent, 500)
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
