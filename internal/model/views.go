package model

// 本文件是"视图 / DTO"类型：它们既被存储实现返回，也被上层（agent、report、HTTP）消费。
//
// 为什么放在 model 而不是 store（P2-4 S3）：如果这些类型留在 store，
// 那么凡是"只读态势"的接口（graphstore.Graph）就不得不 import 具体存储，
// 抽象立刻漏底。把它们挪到 model，`graphstore` 只依赖 model，
// 存储实现（store / memstore）各自满足接口 —— 依赖方向变成"实现 → 契约 → 领域类型"。
//
// 注意：这里只放**纯数据**（无可执行逻辑、无 SQL 语义），不要变成第二个 store。

// KindCoverage 是按实体类型分组的覆盖度。
type KindCoverage struct {
	Kind   string `json:"kind"`
	Total  int    `json:"total"`
	Tested int    `json:"tested"`
}

// Coverage 是范围覆盖度。
//
// 语义（实现规格/01 §6.4）：
//   - 分母：`run_scope` 命中的实体；
//   - 分子：被 `fact` 锚定的实体（沿父链上卷）；
//   - finding 是结论不是覆盖，intent 不算，origin fact 不算。
type Coverage struct {
	Total  int            `json:"total"`
	Tested int            `json:"tested"`
	Pct    float64        `json:"pct"`
	ByKind []KindCoverage `json:"by_kind"`
}

// StepView 是计划里给模型看的一步。
type StepView struct {
	ID          int64   `json:"id"`
	Text        string  `json:"text"`
	Expected    string  `json:"expected_evidence"`
	State       string  `json:"state"`
	Priority    int     `json:"priority"`
	DependsOn   []int64 `json:"depends_on"`
	RealizedBy  int64   `json:"realized_by,omitempty"`
	SatisfiedBy int64   `json:"satisfied_by,omitempty"`
	RuleHit     bool    `json:"satisfied_by_rule,omitempty"`
}

// PlanGraph 是一个 run 的活跃计划（没有就是空视图）。
type PlanGraph struct {
	PlanID   int64
	Title    string
	Strategy string
	Revision int
	Steps    []StepView
}

// CampaignDedupHit 是"这个实体在别的子 run 里已经被测过"的一条提示。
type CampaignDedupHit struct {
	EntityID int64  `json:"entity_id"`
	RunID    int64  `json:"run_id"`
	FactID   int64  `json:"fact_id"`
	Summary  string `json:"summary"`
}

// CampaignView 是注入给 planner 的战役摘要（每轮随态势一起给）。
type CampaignView struct {
	ID              int64              `json:"id"`
	Name            string             `json:"name"`
	Status          string             `json:"status"`
	Role            string             `json:"role,omitempty"`
	Runs            int                `json:"runs"`
	Total           int                `json:"scope_entities"`
	Tested          int                `json:"tested_entities"`
	CoveragePct     float64            `json:"coverage_pct"`
	Duplicates      int                `json:"duplicate_entities"`
	TestedElsewhere []CampaignDedupHit `json:"tested_in_other_runs,omitempty"`
	Note            string             `json:"note,omitempty"`
}
