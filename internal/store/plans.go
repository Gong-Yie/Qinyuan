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

// 计划图的视图类型已迁到 model（P2-4 S3）：别名保留旧名字，调用点零改动。
// 依据《升级计划》P1-1：计划图不新建表，用 nodes(kind='plan'/'step') + 边表达。
type PlanGraph = model.PlanGraph
type StepView = model.StepView

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
	rev, err := bumpVersion(ctx, tx, in.RunID)
	if err != nil {
		return 0, err
	}
	// 计划节点可能是这一步顺手建出来的（EnsurePlan），所以一起打戳。
	if err := stampNodes(ctx, tx, in.RunID, rev, stepID, planID); err != nil {
		return 0, err
	}
	for _, dep := range in.DependsOn {
		if err := stampEdge(ctx, tx, in.RunID, rev, stepID, "depends_on", dep); err != nil {
			return 0, err
		}
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
			Text      string `json:"text"`
			Expected  string `json:"expected_evidence"`
			Satisfied int64  `json:"satisfied_by_fact"`
			Rule      bool   `json:"satisfied_by_rule"`
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
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	tag, err := tx.Exec(ctx, `
		UPDATE nodes SET state=$4, updated_at=now(),
		       completed_at=CASE WHEN $4='done' THEN now() ELSE completed_at END
		 WHERE run_id=$1 AND id=$2 AND kind='step' AND state=$3`,
		runID, stepID, expected, next)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() != 1 {
		return false, nil
	}
	rev, err := bumpVersion(ctx, tx, runID)
	if err != nil {
		return false, err
	}
	if err := stampNodes(ctx, tx, runID, rev, stepID); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
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
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if factID > 0 {
		if _, err := tx.Exec(ctx, `
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
	if _, err = tx.Exec(ctx, `
		UPDATE nodes SET state='done', completed_at=now(), updated_at=now(),
		       payload = payload || $3::jsonb
		 WHERE run_id=$1 AND id=$2 AND kind='step' AND state IN ('todo','ready','running')
		   AND state <> 'done'`, runID, stepID, raw); err != nil {
		return err
	}
	rev, err := bumpVersion(ctx, tx, runID)
	if err != nil {
		return err
	}
	if err := stampNodes(ctx, tx, runID, rev, stepID); err != nil {
		return err
	}
	if factID > 0 {
		if err := stampEdge(ctx, tx, runID, rev, factID, "satisfies", stepID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// LinkStepIntent 把步骤和派出去的意图连起来（step --realized_by--> intent），并把步骤置 running。
func (s *Store) LinkStepIntent(ctx context.Context, runID, stepID, intentID int64) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if _, err := tx.Exec(ctx, `
		INSERT INTO edges (run_id, src_id, rel, dst_id) VALUES ($1,$2,'realized_by',$3)
		ON CONFLICT DO NOTHING`, runID, stepID, intentID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE nodes SET state='running', updated_at=now()
		 WHERE run_id=$1 AND id=$2 AND kind='step' AND state IN ('ready','todo','running')`, runID, stepID); err != nil {
		return err
	}
	rev, err := bumpVersion(ctx, tx, runID)
	if err != nil {
		return err
	}
	if err := stampNodes(ctx, tx, runID, rev, stepID); err != nil {
		return err
	}
	if err := stampEdge(ctx, tx, runID, rev, stepID, "realized_by", intentID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// expectedMatches 判断一条事实文本是否满足期望证据。
//
// 规则（纯代码、可解释；用 3 次真实 run 的 15 对 (step, fact) 校准，见 docs/08）：
//
//  1. 归一化后整串包含 → 命中（最快也最安全）；
//  2. 否则把 expected 按 、，,；;/ 或 切成子句，并从子句里取"可检索签名"：
//     中文取同段内的 3/4 字片段（**不跨词边界**，否则会切出 "页u"、"r响" 这种垃圾），
//     ASCII 取 ≥4 字的整词（server / nginx / favicon）；
//  3. 命中条件：任意一个 4 字签名出现，或 ≥2 个不同的 3 字签名出现。
//
// 为什么不再用"整串 2-gram 覆盖率 ≥60%"：expected_evidence 天然是任务口吻
// （"拿到 Server 响应头或框架版本标识等服务指纹"），动词与虚词把覆盖率稀释到 20~40%，
// 规则几乎永远命中不了 —— 实测 10 个真实步骤、0 次自动完成，全靠 planner 手工 mark。
// 代价是规则更"松"：它只能证明"事实里出现了这条判据的关键片段"，不能证明语义成立，
// 所以事实本身的可信度仍然由 confidence（observed/inferred）与 evidence 决定。
func expectedMatches(text, expected string) bool {
	exp := normalizeForMatch(expected)
	txt := normalizeForMatch(text)
	if exp == "" || txt == "" {
		return false
	}
	if strings.Contains(txt, exp) {
		return true
	}
	for _, sig := range evidenceSignatures(expected, 4) {
		if strings.Contains(txt, sig) {
			return true
		}
	}
	hit := 0
	seen := map[string]bool{}
	for _, sig := range evidenceSignatures(expected, 3) {
		if seen[sig] {
			continue
		}
		seen[sig] = true
		if strings.Contains(txt, sig) {
			hit++
			if hit >= 2 {
				return true
			}
		}
	}
	return false
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

// evidenceStopwords 是"任务口吻"带进来、但不承载判据内容的包装词。
// 只整词删除，避免把 "服务器" 里的 "服务" 删掉。
var evidenceStopwords = []string{
	"拿到", "获取", "得到", "取得", "找到", "定位到", "确认", "核实", "记录", "输出", "整理",
	"及其", "以及", "相关", "具体", "完整", "原始", "证据", "可访问性", "可重放", "可复核", "特征",
}

// evidenceClauses 按 、，,；;/ 以及"或/或者"把期望证据切成子句。
func evidenceClauses(expected string) []string {
	repl := strings.NewReplacer(
		"、", "\x00", "，", "\x00", ",", "\x00", "；", "\x00", ";", "\x00",
		"/", "\x00", "或者", "\x00", "或", "\x00",
	)
	var out []string
	for _, p := range strings.Split(repl.Replace(expected), "\x00") {
		p = strings.Trim(strings.TrimSpace(p), "（）()[]【】 ")
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return []string{expected}
	}
	return out
}

// evidenceSignatures 从 expected 里抽出可检索片段：
// 中文按同一 CJK 段取 n-gram，ASCII 取 ≥4 字的整词。
func evidenceSignatures(expected string, n int) []string {
	var out []string
	for _, clause := range evidenceClauses(expected) {
		for _, w := range evidenceStopwords {
			clause = strings.ReplaceAll(clause, w, " ")
		}
		s := normalizeForMatch(clause)

		var cjk []rune
		var ascii strings.Builder
		flushCJK := func() {
			if len(cjk) >= n {
				for i := 0; i+n <= len(cjk); i++ {
					out = append(out, string(cjk[i:i+n]))
				}
			}
			cjk = cjk[:0]
		}
		flushASCII := func() {
			if w := ascii.String(); len(w) >= 4 {
				out = append(out, w)
			}
			ascii.Reset()
		}
		for _, r := range s {
			switch {
			case unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r):
				flushASCII()
				cjk = append(cjk, r)
			case unicode.IsLetter(r) || unicode.IsDigit(r):
				flushCJK()
				ascii.WriteRune(r)
			default:
				flushCJK()
				flushASCII()
			}
		}
		flushCJK()
		flushASCII()
	}
	return out
}
