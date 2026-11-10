// Package model 定义钦原的领域类型。
//
// 这一层不依赖任何上层：只有纯数据 + 纯函数（例如 DeriveStatus）。
// 依据：实现规格/00-总览与不变量.md 的术语表、01-数据模型.md 的字段语义。
package model

import (
	"encoding/json"
	"strings"
	"time"
)

// 节点 kind。与 schema.sql 的 ck_node_kind 一一对应。
const (
	KindGoal    = "goal"
	KindIntent  = "intent"
	KindFact    = "fact"
	KindFinding = "finding"
	KindHint    = "hint"
	KindDigest  = "digest"
	KindPlan    = "plan"
	KindStep    = "step"
)

// 节点 state。不同 kind 的合法集合不同，由数据库 CHECK 兜底。
const (
	StateOpen       = "open"
	StateRunning    = "running"
	StatePaused     = "paused"
	StateDone       = "done"
	StateBlocked    = "blocked"
	StateExhausted  = "exhausted"
	StateStopped    = "stopped"
	StateDeleted    = "deleted"
	StateMet        = "met"
	StateAbandoned  = "abandoned"
	StateConfirmed  = "confirmed"
	StateDismissed  = "dismissed"
	StateOrigin     = "origin"
	StateActive     = "active"
	StateConsumed   = "consumed"
	StateSuperseded = "superseded"

	// 计划步骤状态（M3）：ready 由代码按依赖判定，不由模型判定。
	StateTodo    = "todo"
	StateReady   = "ready"
	StateSkipped = "skipped"
	StateFailed  = "failed"
)

// 边关系。src = 上游/父，dst = 下游/子（全项目统一，不许反）。
const (
	RelSpawns      = "spawns"
	RelDerivedFrom = "derived_from"
	RelYields      = "yields"
	RelProves      = "proves"
	RelCovers      = "covers"
)

// 任务业务状态。
const (
	TaskCreated = "created"
	TaskRunning = "running"
	TaskPaused  = "paused"
	TaskDone    = "done"
	TaskFailed  = "failed"
	TaskTimeout = "timeout"
)

// 意图终态集合。
func IsTerminalIntentState(s string) bool {
	switch s {
	case StateDone, StateBlocked, StateExhausted, StateStopped, StateDeleted:
		return true
	}
	return false
}

// IsTerminalTaskState 任务是否已到终态。
func IsTerminalTaskState(s string) bool {
	switch s {
	case TaskDone, TaskFailed, TaskTimeout:
		return true
	}
	return false
}

// Run 一次探索。
type Run struct {
	ID          int64
	Goal        string
	Description string
	// Seeds 用户给的入口 URL（可能有端口/路径）。与范围不是一回事：
	// 范围回答"允许打哪"，seeds 回答"从哪开始"。
	Seeds     []string
	Status    string
	RoundNo   int64
	Version   int64
	// CampaignID / CampaignRole：这个 run 属于哪个战役、负责哪一块（0/空 = 独立任务）。
	CampaignID   int64
	CampaignRole string
	CreatedAt time.Time
	UpdatedAt time.Time
	Task      *Task
}

// Task run 的业务状态（run_tasks 表）。
type Task struct {
	RunID            int64
	Name             string
	Status           string
	Paused           bool
	Queued           bool
	QueuedAt         int64
	TimeoutSeconds   int
	FirstRunAt       int64
	DeadlineAt       int64
	HeartbeatSeconds int
	WorkerCount      int
	CompletedAt      int64
	// GoalsAt 是第 0 轮目标拆解完成时刻（unix 秒；0 = 还没拆过）。
	// 拆解失败不写这个标记，下一轮会重试，此时 seed 目标兜底。
	GoalsAt int64
}

// Node 图节点。Payload 保留原始 JSON，避免无谓的 map 反复编解码。
type Node struct {
	ID             int64
	RunID          int64
	Kind           string
	State          string
	Payload        json.RawMessage
	Priority       int
	Owner          string
	Origin         string
	BlockedReason  string
	ContentVersion int
	ColdSinceRound *int64
	// Rev 是最后一次写入分配的版本号（0 = M3.5 之前的历史行）。
	// 差分态势靠它判断"自上次以来这一行变过没有"。
	Rev         int64
	CompletedAt *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// DecodePayload 把 payload 解到 v。
func (n *Node) DecodePayload(v any) error {
	if len(n.Payload) == 0 {
		n.Payload = []byte(`{}`)
	}
	return json.Unmarshal(n.Payload, v)
}

// PayloadField 取 payload 里的一个字符串字段（缺失返回空串）。
func (n *Node) PayloadField(key string) string {
	var m map[string]any
	if err := n.DecodePayload(&m); err != nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

// Summary 是模型在列表里看到的一行摘要；缺 summary 时按 kind 回退到合适的字段。
func (n *Node) Summary() string {
	if s := n.PayloadField("summary"); s != "" {
		return s
	}
	if s := n.PayloadField("text"); s != "" {
		return s
	}
	return n.PayloadField("name")
}

// Edge 一条边。
type Edge struct {
	RunID int64
	SrcID int64
	Rel   string
	DstID int64
	// Rev 同 Node.Rev：这条边最后一次写入时的版本号。
	Rev int64
}

// Activity 执行过程的一步（只追加）。
type Activity struct {
	ID               int64
	RunID            int64
	NodeID           *int64
	Worker           string
	Kind             string
	Tool             string
	ToolUseID        string
	IsError          bool
	Summary          string
	Detail           string
	Metadata         map[string]any
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	Segment          *int
	CreatedAt        time.Time

	// P2-5：trace / span。span 行（Kind=ActSpan）带这五个字段，
	// 普通活动行留空 —— 于是"时间线"和"过程记录"是同一张表、同一份写入路径。
	TraceID      string
	SpanID       string
	ParentSpanID string
	SpanName     string
	DurationMs   int
}

// Activity kinds（与 ARTEX 对齐的一小部分，后续按需扩展）。
const (
	ActThinking   = "thinking"
	ActText       = "text"
	ActToolUse    = "tool_use"
	ActToolResult = "tool_result"
	ActUsage      = "usage"
	ActResult     = "result"
	ActRound      = "round"
	ActSystem     = "system"
	// ActSpan 是一段被计时的执行（planner 轮次 / 意图执行 / LLM 调用 / 工具调用）。
	// 它出现在过程流里，但前端应当按"时间线"渲染，而不是当成一张工具卡片。
	ActSpan = "span"
)

// Entity 领域实体（跨 run 共享）。
type Entity struct {
	ID          int64
	Kind        string
	Domain      string
	RootDomain  string
	RecordType  string
	IP          string
	Port        int
	URL         string
	Method      string
	ServiceType string
	ServiceName string
	StatusCode  int
	Title       string
	RunIDs      []int64
}

// Scope 授权范围的一条。
type Scope struct {
	Kind   string
	Domain string
	Net    string // CIDR 文本，例如 10.0.0.0/8
	OrgID  int64
	Value  string
	Source string
	Reason string
}

// Constraint 操作约束。
type Constraint struct {
	ID   int64
	Kind string // allow | deny
	Text string
}

// TokenUsage 是 token 聚合口径（只汇总 kind='result' 的权威行）。
type TokenUsage struct {
	Input      int
	Output     int
	CacheRead  int
	CacheWrite int
}

// Attachment 是任务附件（只读投喂给 agent 的材料）。
//
// 它**不是**文件系统权限：agent 只能按 name/id 读这一段内容，路径由服务端决定，
// 内容按不可信输入处理（I18）。
type Attachment struct {
	ID        int64
	RunID     int64
	Name      string
	Kind      string // text | binary | archive | image
	Mime      string
	Size      int64
	SHA256    string
	Path      string // 相对 DataDir
	Summary   string // 前 200 字（注入态势用）
	CreatedAt time.Time
}

// DeriveStatus 是全站唯一的"任务显示状态"推导函数。
//
// 依据 升级计划 P2-2：列表、详情、编排工具必须调同一个函数，
// 否则会出现"列表显示运行中、详情显示空闲"。
func DeriveStatus(t *Task, hasActiveIntent, hasActiveLLMCall bool) string {
	if t == nil {
		return TaskCreated
	}
	if IsTerminalTaskState(t.Status) {
		return t.Status
	}
	switch {
	case t.Paused:
		return TaskPaused
	case t.Queued:
		return "queued"
	case hasActiveLLMCall:
		return TaskRunning
	case hasActiveIntent:
		return TaskRunning
	default:
		return "idle"
	}
}

// TrimSummary 截断摘要到 n 个 rune，并保证返回值是合法 UTF-8。
func TrimSummary(s string, n int) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	s = strings.ReplaceAll(s, "\x00", "")
	if n <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
