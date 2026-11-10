package agent

import (
	"context"
	"encoding/json"

	"qinyuan/internal/model"
)

// NewNodeDetailTool 是唯一能拿到 evidence / detail 全文的口子。
func NewNodeDetailTool() *Tool {
	return &Tool{
		Name:        "node_detail",
		Description: `取一个节点的完整内容（payload 全文 + 状态 + 血缘 + 锚定实体）。列表里看到的只是摘要；摘要可疑时用它核对原文。`,
		ReadOnly:    true,
		Schema: toolSpecJSON(`{
  "type": "object",
  "properties": {"id": {"type": "integer", "description": "节点 id"}},
  "required": ["id"]
}`),
		Handle: handleNodeDetail,
	}
}

func handleNodeDetail(ctx context.Context, rc *Context, raw json.RawMessage) (Result, error) {
	var in struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return Errorf("参数解析失败：%v", err), nil
	}
	n, err := rc.Store.Node(ctx, rc.RunID, in.ID)
	if err != nil {
		return Errorf("节点 #%d 不存在于本任务（跨任务节点一律不可见）", in.ID), nil
	}
	ancestors, descendants := lineage(ctx, rc, n.ID)
	ents, err := rc.Store.EntitiesByNode(ctx, n.ID)
	if err != nil {
		return Errorf("读锚定实体失败：%v", err), nil
	}
	return JSONResult(map[string]any{
		"id":         n.ID,
		"kind":       n.Kind,
		"state":      n.State,
		"payload":    json.RawMessage(n.Payload),
		"priority":   n.Priority,
		"origin":     n.Origin,
		"created_at": n.CreatedAt.Unix(),
		"parents":    ancestors,
		"children":   descendants,
		"entities":   ents,
	}), nil
}

func lineage(ctx context.Context, rc *Context, id int64) (parents, children []int64) {
	edges, err := rc.Store.Edges(ctx, rc.RunID)
	if err != nil {
		return nil, nil
	}
	for _, e := range edges {
		if e.Rel == model.RelCovers {
			continue
		}
		if e.DstID == id {
			parents = append(parents, e.SrcID)
		}
		if e.SrcID == id {
			children = append(children, e.DstID)
		}
	}
	return parents, children
}

// NewOverviewTool 让 main agent 与调试路径也能拿到同一份态势。
func NewOverviewTool() *Tool {
	return &Tool{
		Name:        "overview",
		Description: `取本任务当前态势（goal/意图/事实/发现/实体计数）。planner 每轮的提示里已经预取了它，一般不需要再调。`,
		ReadOnly:    true,
		Schema:      toolSpecJSON(`{"type":"object","properties":{}}`),
		Handle: func(ctx context.Context, rc *Context, _ json.RawMessage) (Result, error) {
			ov, err := BuildOverview(ctx, rc.Store, rc.RunID, true)
			if err != nil {
				return Errorf("读态势失败：%v", err), nil
			}
			return JSONResult(ov), nil
		},
	}
}

// NewRefreshOverviewTool 是 P1-2 的逃生门：当模型觉得"增量视图不够、判断不踏实"时，
// 可以要一份全量态势。它会同时把下一轮也标成全量（模型显然在重新校准认知）。
func NewRefreshOverviewTool() *Tool {
	return &Tool{
		Name: "refresh_overview",
		Description: `要一份**全量**态势（比平时收到的增量视图完整得多，也更贵）。
只在"增量视图不足以判断、或怀疑自己漏掉了更早的重要事实"时用；拿到后请立刻据此决策，
不要为了省思考而反复刷新。刷新后下一轮也会自动给全量。`,
		ReadOnly: true,
		Schema:   toolSpecJSON(`{"type":"object","properties":{}}`),
		Handle: func(ctx context.Context, rc *Context, _ json.RawMessage) (Result, error) {
			ov, err := BuildOverview(ctx, rc.Store, rc.RunID, true)
			if err != nil {
				return Errorf("读全量态势失败：%v", err), nil
			}
			if rc.RefreshRequested != nil {
				*rc.RefreshRequested = true
			}
			return JSONResult(ov), nil
		},
	}
}

// NewListFindingsTool 让 worker 也能看全任务已确认的漏洞（避免重复上报）。
func NewListFindingsTool() *Tool {
	return &Tool{
		Name:        "list_findings",
		Description: `列出本任务已确认的漏洞（id/标题/严重度/摘要）。上报前先看一眼，避免重复上报同一个问题。`,
		ReadOnly:    true,
		Schema: toolSpecJSON(`{
  "type": "object",
  "properties": {"limit": {"type": "integer", "description": "默认 20"}}
}`),
		Handle: func(ctx context.Context, rc *Context, raw json.RawMessage) (Result, error) {
			var in struct {
				Limit int `json:"limit"`
			}
			_ = json.Unmarshal(raw, &in)
			if in.Limit <= 0 || in.Limit > 100 {
				in.Limit = 20
			}
			nodes, err := rc.Store.NodesByKind(ctx, rc.RunID, model.KindFinding, in.Limit)
			if err != nil {
				return Errorf("读漏洞列表失败：%v", err), nil
			}
			items := make([]map[string]any, 0, len(nodes))
			for _, n := range nodes {
				items = append(items, map[string]any{
					"id":       n.ID,
					"name":     firstNonEmptyStr(n.PayloadField("name"), n.Summary()),
					"severity": n.PayloadField("severity"),
					"summary":  model.TrimSummary(n.Summary(), 160),
					"state":    n.State,
				})
			}
			total, _ := rc.Store.CountByKind(ctx, rc.RunID, model.KindFinding)
			return JSONResult(map[string]any{"items": items, "total": total}), nil
		},
	}
}
