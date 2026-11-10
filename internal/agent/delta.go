package agent

import (
	"context"
	"time"

	"qinyuan/internal/model"
	"qinyuan/internal/store"
)

// 本文件是《升级计划》P1-2（差分态势）的 agent 侧：把"自上次以来发生了什么"
// 组装成一段**小得多**的输入，替代每轮重发全量态势。
//
// 三条规则（照抄规格，不能省）：
//  1. **首轮给全量**：没有 lastSeen 时就是全量（否则模型没有基线）；
//  2. **当前态摘要必须给全**：goals / running / frontier_open / ready_steps 这些
//     无法从"变化"里推出来，而且它们本来就小；
//  3. **变化太多就降级全量**：单轮超过上限（默认 200 条）时直接给全量并说明原因，
//     否则一次刷屏式变更会把上下文灌爆。
//
// 与全量的一致性由 store.DeltaSince + 属性测试保证（增量重建 == 全量）。

// DeltaEvent 是增量里的一条变化。节点与边共用一种形状（靠 Kind 区分）。
type DeltaEvent struct {
	Kind       string `json:"kind"` // node | edge
	NodeKind   string `json:"node_kind,omitempty"`
	ID         int64  `json:"id,omitempty"`
	State      string `json:"state,omitempty"`
	Summary    string `json:"summary,omitempty"`
	Confidence string `json:"confidence,omitempty"`
	Src        int64  `json:"src,omitempty"`
	Rel        string `json:"rel,omitempty"`
	Dst        int64  `json:"dst,omitempty"`
}

// DeltaOverview 是"增量态势"：变化列表 + 一份很小的当前态摘要。
//
// 字段名刻意与 Overview 对齐（goals / running_intents / frontier_open /
// recent_facts / plan / facts_total …），这样"读态势"的代码与测试不必分两套。
type DeltaOverview struct {
	RunID        int64        `json:"run_id"`
	Round        int64        `json:"round"`
	FromVersion  int64        `json:"from_version"`
	ToVersion    int64        `json:"to_version"`
	ChangesTotal int          `json:"changes_total"`
	Events       []DeltaEvent `json:"changes"`

	// 当前态摘要（这些必须给全）。
	Goals       []Brief    `json:"goals"`
	Running     []Brief    `json:"running_intents"`
	OpenRecent  []Brief    `json:"open_intents_recent"`
	Frontier    int        `json:"frontier_open"`
	Plan        *PlanBrief `json:"plan,omitempty"`
	RecentFacts []Brief    `json:"recent_facts"`
	RecentDone  []Brief    `json:"recent_done"`

	DoneTotal     int `json:"done_total"`
	FactsTotal    int `json:"facts_total"`
	FindingsTotal int `json:"findings_total"`

	GeneratedAt int64 `json:"generated_at"`
}

// deltaRecentOpen / deltaRecentFacts / deltaRecentDone 是"摘要里保留多少条历史"。
//
// 它们不是随便定的：opening 太多就退化成全量（省不下 token），太少又会让
// planner 忘记刚做过什么、重复派意图。K 轮一次的全量快照是兜底。
const (
	deltaRecentOpen  = 8
	deltaRecentFacts = 5
	deltaRecentDone  = 5
)

// BuildDeltaOverview 组装增量态势。truncated=true 表示变化太多，调用方应退全量。
func BuildDeltaOverview(ctx context.Context, st *store.Store, runID int64, run *model.Run, since int64, limit int) (*DeltaOverview, bool, error) {
	d, err := st.DeltaSince(ctx, runID, since, limit)
	if err != nil {
		return nil, false, err
	}
	if d.Truncated {
		return &DeltaOverview{
			RunID: runID, FromVersion: d.From, ToVersion: d.To,
			ChangesTotal: d.Total, GeneratedAt: time.Now().Unix(),
		}, true, nil
	}

	ov := &DeltaOverview{
		RunID:        runID,
		FromVersion:  d.From,
		ToVersion:    d.To,
		ChangesTotal: len(d.Nodes) + len(d.Edges),
		GeneratedAt:  time.Now().Unix(),
	}
	if run != nil {
		ov.Round = run.RoundNo
	}

	// 变化列表：节点先、边后。节点事件带"一行摘要"，边事件只给关系三元组
	// （边的两端在节点事件里已经有摘要，重复渲染等于重复计费）。
	for _, n := range d.Nodes {
		ev := DeltaEvent{
			Kind: "node", NodeKind: n.Kind, ID: n.ID, State: n.State,
			Summary: model.TrimSummary(n.Summary(), 160),
		}
		if n.Kind == model.KindFact || n.Kind == model.KindFinding {
			ev.Confidence = n.PayloadField("confidence")
		}
		ov.Events = append(ov.Events, ev)
	}
	for _, e := range d.Edges {
		ov.Events = append(ov.Events, DeltaEvent{Kind: "edge", Src: e.SrcID, Rel: e.Rel, Dst: e.DstID})
	}

	// 当前态摘要。
	if err := fillDeltaState(ctx, st, runID, ov); err != nil {
		return nil, false, err
	}
	return ov, false, nil
}

func fillDeltaState(ctx context.Context, st *store.Store, runID int64, ov *DeltaOverview) error {
	var err error
	if ov.Goals, err = briefs(ctx, st, runID, model.KindGoal, 20, nil); err != nil {
		return err
	}
	if ov.Running, err = briefsByState(ctx, st, runID, model.StateRunning, 30, nil, nil); err != nil {
		return err
	}
	open, err := briefsByState(ctx, st, runID, model.StateOpen, deltaRecentOpen, nil, nil)
	if err != nil {
		return err
	}
	ov.OpenRecent = open
	if ov.RecentFacts, err = briefs(ctx, st, runID, model.KindFact, deltaRecentFacts, nil); err != nil {
		return err
	}
	if ov.RecentDone, err = briefsByState(ctx, st, runID, model.StateDone, deltaRecentDone, nil, nil); err != nil {
		return err
	}
	if ov.Frontier, err = st.CountIntentState(ctx, runID, model.StateOpen); err != nil {
		return err
	}
	if ov.DoneTotal, err = st.CountIntentState(ctx, runID, model.StateDone); err != nil {
		return err
	}
	if ov.FactsTotal, err = st.CountByKind(ctx, runID, model.KindFact); err != nil {
		return err
	}
	if ov.FindingsTotal, err = st.CountByKind(ctx, runID, model.KindFinding); err != nil {
		return err
	}
	if pg, err := st.PlanGraph(ctx, runID); err == nil && pg.PlanID > 0 {
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
	return nil
}
