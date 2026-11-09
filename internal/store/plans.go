package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"

	"qinyuan/internal/model"
)

// PlanGraph 是一个 run 的计划图视图。
//
// 依据《升级计划》P1-1：计划图不新建表，用 nodes(kind='plan'/'step') + 边表达。
type PlanGraph struct {
	PlanID   int64
	Title    string
	Strategy string
	Revision int
	Steps    []StepView
}

// StepView 是给模型看的一步。
type StepView struct {
	ID         int64   `json:"id"`
	Text       string  `json:"text"`
	Expected   string  `json:"expected_evidence"`
	State      string  `json:"state"`
	Priority   int     `json:"priority"`
	DependsOn  []int64 `json:"depends_on"`
	RealizedBy int64   `json:"realized_by,omitempty"`
	SatisfiedBy int64  `json:"satisfied_by,omitempty"`
	RuleHit    bool    `json:"satisfied_by_rule,omitempty"`
}

// EnsurePlan 返回当前活跃计划；没有就建一个（每个 run 只允许一个活跃计划）。
func (s *Store) EnsurePlan(ctx context.Context, runID int64, title, strategy string) (int64, error) {
	var planID int64
	err := s.pool.QueryRow(ctx,
		`SELECT id FROM nodes WHERE run_id=$1 AND kind='plan' AND state='active' ORDER BY id LIMIT 1`,
		runID).Scan(&planID)
	if err == nil {
		return planID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	if title == "" {
		title = "探索计划"
	}
	payload, err := json.Marshal(map[string]any{"title": title, "strategy": strategy, "revision": 1})
	if err != nil {
		return 0, err
	}
	if err := s.pool.QueryRow(ctx, `
		INSERT INTO nodes (run_id, kind, state, payload, priority, origin)
		VALUES ($1,'plan','active',$2,0,'planner') RETURNING id`,
		runID, payload).Scan(&planID); err != nil {
		return 0, fmt.Errorf("建计划失败：%w", err)
	}
	return planID, nil
}

// AddStepInput 新增一步。
type AddStepInput struct {
	RunID            int64
	Text             string
	ExpectedEvidence string
	DependsOn        []int64
	Priority         int
}

// AddStep 新增一个步骤，并连 depends_on 边。
func (s *Store) AddStep(ctx context.Context, in AddStepInput) (int64, error) {
	if in.Text == "" {
		return 0, errors.New("step 的 text 必填")
	}
	if in.Priority < 0 || in.Priority > 10 {
		in.Priority = 5
	}
	planID, err := s.EnsurePlan(ctx, in.RunID, "", "")
	if err != nil {
		return 0, err
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// 依赖必须是本 run 的 step，否则计划图会连到别的任务上。
	for _, dep := range in.DependsOn {
		var kind string
		err := tx.QueryRow(ctx, `SELECT kind FROM nodes WHERE run_id=$1 AND id=$2`, in.RunID, dep).Scan(&kind)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && kind != "step") {
			return 0, fmt.Errorf("依赖 #%d 不是本任务的步骤", dep)
		}
		if err != nil {
			return 0, err
		}
	}

	state := model.StateReady
	if len(in.DependsOn) > 0 {
		state = "todo"
	}
	payload, err := json.Marshal(map[string]any{
		"text":              in.Text,
		"expected_evidence": in.ExpectedEvidence,
		"plan_id":           planID,
	})
	if err != nil {
		return 0, err
	}
	var stepID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO nodes (run_id, kind, state, payload, priority, origin)
		VALUES ($1,'step',$2,$3,$4,'planner') RETURNING id`,
		in.RunID, state, payload, in.Priority).Scan(&stepID); err != nil {
		return 0, fmt.Errorf("建步骤失败：%w", err)
	}
	for _, dep := range in.DependsOn {
		if _, err := tx.Exec(ctx, `
			INSERT INTO edges (run_id, src_id, rel, dst_id) VALUES ($1,$2,'depends_on',$3)
			ON CONFLICT DO NOTHING`, in.RunID, stepID, dep); err != nil {
			return 0, err
		}
	}
	if err := bumpVersion(ctx, tx, in.RunID); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return stepID, nil
}

// PlanGraph 读计划图。
func (s *Store) PlanGraph(ctx context.Context, runID int64) (*PlanGraph, error) {
	pg := &PlanGraph{}
	var raw []byte
	err := s.pool.QueryRow(ctx, `
		SELECT id, payload FROM nodes
		 WHERE run_id=$1 AND kind='plan' AND state='active' ORDER BY id LIMIT 1`, runID).
		Scan(&pg.PlanID, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return pg, nil
	}
	if err != nil {
		return nil, err
	}

	var p struct {
		Title    string `json:"title"`
		Strategy string `json:"strategy"`
		Revision int    `json:"revision"`
	}
	_ = json.Unmarshal(raw, &p)
	pg.Title, pg.Strategy, pg.Revision = p.Title, p.Strategy, p.Revision

	// 步骤按创建顺序返回（计划图要按"第 1 步、第 2 步"读，不能像列表那样倒序）。
	rows, err := s.pool.Query(ctx,
		`SELECT `+nodeColumns+` FROM nodes WHERE run_id=$1 AND kind='step' ORDER BY id ASC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var steps []*model.Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		steps = append(steps, n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	edges, err := s.Edges(ctx, runID)
	if err != nil {
		return nil, err
	}
	deps := map[int64][]int64{}
	realized := map[int64]int64{}
	satisfied := map[int64]int64{}
	for _, e := range edges {
		switch e.Rel {
		case "depends_on":
			deps[e.SrcID] = append(deps[e.SrcID], e.DstID)
		case "realized_by":
			realized[e.SrcID] = e.DstID
		case "satisfies":
			satisfied[e.DstID] = e.SrcID
		}
	}
	for _, n := range steps {
		var sp struct {
			Text       string `json:"text"`
			Expected   string `json:"expected_evidence"`
			Satisfied  int64  `json:"satisfied_by_fact"`
			Rule       bool   `json:"satisfied_by_rule"`
		}
		_ = n.DecodePayload(&sp)
		sv := StepView{
			ID: n.ID, Text: sp.Text, Expected: sp.Expected, State: n.State, Priority: n.Priority,
			DependsOn: deps[n.ID], RealizedBy: realized[n.ID], SatisfiedBy: sp.Satisfied, RuleHit: sp.Rule,
		}
		if sv.SatisfiedBy == 0 {
			sv.SatisfiedBy = satisfied[n.ID]
		}
		pg.Steps = append(pg.Steps, sv)
	}
	return pg, nil
}

// RecomputeSteps 按依赖重算 todo/ready：所有依赖都 done 的步骤变 ready。
//
// 这是 P1-1 的核心不变量：**ready 由代码判定，不由模型判定**。
func (s *Store) RecomputeSteps(ctx context.Context, runID int64) (int, error) {
	pg, err := s.PlanGraph(ctx, runID)
	if err != nil {
		return 0, err
	}
	state := map[int64]string{}
	for _, st := range pg.Steps {
		state[st.ID] = st.State
	}
	changed := 0
	for _, st := range pg.Steps {
		if st.State == model.StateDone || st.State == "skipped" || st.State == "failed" || st.State == model.StateRunning {
			continue
		}
		ready := true
		for _, dep := range st.DependsOn {
			if state[dep] != model.StateDone {
				ready = false
				break
			}
		}
		want := "todo"
		if ready {
			want = model.StateReady
		}
		if want == st.State {
			continue
		}
		ok, err := s.CASStepState(ctx, runID, st.ID, st.State, want)
		if err != nil {
			return changed, err
		}
		if ok {
			state[st.ID] = want
			changed++
		}
	}
	return changed, nil
}

// CASStepState 步骤状态迁移（带 expected）。
func (s *Store) CASStepState(ctx context.Context, runID, stepID int64, expected, next string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE nodes SET state=$4, updated_at=now(),
		       completed_at=CASE WHEN $4='done' THEN now() ELSE completed_at END
		 WHERE run_id=$1 AND id=$2 AND kind='step' AND state=$3`,
		runID, stepID, expected, next)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// SatisfyStepsByFact 用规则匹配"这条事实是否满足某一步的 expected_evidence"。
//
// 规则命中优先（成本最低）；命中不了再让 planner 用 mark_step_done 显式判定。
func (s *Store) SatisfyStepsByFact(ctx context.Context, runID, factID int64, text string) ([]int64, error) {
	pg, err := s.PlanGraph(ctx, runID)
	if err != nil {
		return nil, err
	}
	var satisfied []int64
	for _, st := range pg.Steps {
		if st.State == model.StateDone || st.State == "skipped" || st.Expected == "" {
			continue
		}
		if !expectedMatches(text, st.Expected) {
			continue
		}
		if err := s.markStepSatisfied(ctx, runID, st.ID, factID, true); err != nil {
			return satisfied, err
		}
		satisfied = append(satisfied, st.ID)
	}
	if len(satisfied) > 0 {
		if _, err := s.RecomputeSteps(ctx, runID); err != nil {
			return satisfied, err
		}
	}
	return satisfied, nil
}

// MarkStepDone 由 planner 显式判定某步达成（规则匹配不上时用）。
func (s *Store) MarkStepDone(ctx context.Context, runID, stepID, factID int64) (bool, error) {
	var kind string
	if err := s.pool.QueryRow(ctx, `SELECT kind FROM nodes WHERE run_id=$1 AND id=$2`, runID, stepID).Scan(&kind); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, fmt.Errorf("步骤 #%d 不存在", stepID)
		}
		return false, err
	}
	if kind != "step" {
		return false, fmt.Errorf("节点 #%d 是 %q，不是步骤", stepID, kind)
	}
	if factID > 0 {
		var evKind string
		if err := s.pool.QueryRow(ctx, `SELECT kind FROM nodes WHERE run_id=$1 AND id=$2`, runID, factID).Scan(&evKind); err != nil {
			return false, fmt.Errorf("证据 #%d 不存在于本任务", factID)
		}
		if evKind != model.KindFact && evKind != model.KindFinding {
			return false, fmt.Errorf("证据 #%d 是 %q：只能用事实/发现证明步骤", factID, evKind)
		}
	}
	if err := s.markStepSatisfied(ctx, runID, stepID, factID, false); err != nil {
		return false, err
	}
	if _, err := s.RecomputeSteps(ctx, runID); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) markStepSatisfied(ctx context.Context, runID, stepID, factID int64, byRule bool) error {
	if factID > 0 {
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO edges (run_id, src_id, rel, dst_id) VALUES ($1,$2,'satisfies',$3)
			ON CONFLICT DO NOTHING`, runID, factID, stepID); err != nil {
			return err
		}
	}
	patch := map[string]any{"satisfied_by_fact": factID, "satisfied_by_rule": byRule}
	raw, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		UPDATE nodes SET state='done', completed_at=now(), updated_at=now(),
		       payload = payload || $3::jsonb
		 WHERE run_id=$1 AND id=$2 AND kind='step' AND state IN ('todo','ready','running')
		   AND state <> 'done'`, runID, stepID, raw)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `UPDATE runs SET version = version + 1 WHERE id=$1`, runID)
	return err
}

// LinkStepIntent 把步骤和派出去的意图连起来（step --realized_by--> intent），并把步骤置 running。
func (s *Store) LinkStepIntent(ctx context.Context, runID, stepID, intentID int64) error {
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO edges (run_id, src_id, rel, dst_id) VALUES ($1,$2,'realized_by',$3)
		ON CONFLICT DO NOTHING`, runID, stepID, intentID); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE nodes SET state='running', updated_at=now()
		 WHERE run_id=$1 AND id=$2 AND kind='step' AND state IN ('ready','todo','running')`, runID, stepID)
	return err
}

// expectedMatches 判断一条事实文本是否满足期望证据。
//
// 规则（刻意简单、可解释）：整体包含 → 命中；否则按 token（中文 2-gram）覆盖率 ≥60% 命中。
func expectedMatches(text, expected string) bool {
	exp := normalizeForMatch(expected)
	txt := normalizeForMatch(text)
	if exp == "" || txt == "" {
		return false
	}
	if strings.Contains(txt, exp) {
		return true
	}
	tokens := matchTokens(exp)
	if len(tokens) == 0 {
		return false
	}
	hit := 0
	for _, tok := range tokens {
		if strings.Contains(txt, tok) {
			hit++
		}
	}
	if len(tokens) == 1 {
		return hit == 1
	}
	return float64(hit)/float64(len(tokens)) >= 0.6
}

func normalizeForMatch(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func matchTokens(s string) []string {
	runes := []rune(s)
	if len(runes) <= 2 {
		return []string{s}
	}
	// 中文用 2-gram（整句包含对措辞太敏感）；英文/数字按整词。
	var out []string
	if containsCJK(s) {
		seen := map[string]bool{}
		for i := 0; i+1 < len(runes); i++ {
			t := string(runes[i : i+2])
			if !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		}
		return out
	}
	for _, f := range strings.FieldsFunc(s, func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r))
	}) {
		if len(f) >= 2 {
			out = append(out, f)
		}
	}
	return out
}

func containsCJK(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}
