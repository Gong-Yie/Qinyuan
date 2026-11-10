package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"

	"qinyuan/internal/domain"
	"qinyuan/internal/model"
	"qinyuan/internal/store"
)

// NewAddIntentTool 是 planner 的"唯一动作"：产出一条探索方向。
//
// 硬校验（不靠 prompt 自觉）：
//  1. parent_ids 只能是已存在的 fact / finding（store 里校验）；
//  2. asset_ids 必须过范围闸门；
//  3. 收尾轮（Final）一律拒绝 —— 收尾只判定目标，不扩战线。
func NewAddIntentTool() *Tool {
	return &Tool{
		Name: "add_intent",
		Description: `创建一条探索方向（intent），可以一次批量提交（最多 4 条，优先批量）。
summary 要说清"针对什么 + 做什么 + 为什么"，依赖去重靠它与已有意图比对。
parent_ids 只能填已存在的【事实/发现】（从它们综合出这条方向）；顶层全新方向留空。
asset_ids 填这条方向要处理的目标实体 id（尽量传）。
step_id：把这条方向挂到计划图的某一步（有计划图时必填，除非 off_plan=true）。
off_plan：显式声明"这是计划外探索"（会作为计划需要修订的信号）。
budget：给这条方向定预算（可选）。探索性/低把握 → 8 回合/300 秒；已确认可深入 → 20 回合/900 秒；
复核类 → 10 回合/600 秒。不填 = 默认。它只是上限，撞到会走收尾轮并把已得的写回。`,
		Schema: toolSpecJSON(`{
  "type": "object",
  "properties": {
    "intents": {
      "type": "array",
      "maxItems": 4,
      "items": {
        "type": "object",
        "properties": {
          "summary": {"type": "string", "description": "一句话方向：针对什么 + 做什么 + 为什么"},
          "asset_ids": {"type": "array", "items": {"type": "integer"}},
          "parent_ids": {"type": "array", "items": {"type": "integer"}},
          "priority": {"type": "integer", "minimum": 0, "maximum": 10},
          "step_id": {"type": "integer", "description": "挂到哪个计划步骤"},
          "off_plan": {"type": "boolean", "description": "计划外探索（有计划图时必须显式声明）"},
          "budget": {
            "type": "object",
            "description": "这条方向的预算上限（可选）",
            "properties": {
              "max_turns": {"type": "integer", "minimum": 1, "maximum": 50},
              "max_seconds": {"type": "integer", "minimum": 30, "maximum": 3600}
            }
          }
        },
        "required": ["summary"]
      }
    }
  },
  "required": ["intents"]
}`),
		Handle: handleAddIntent,
	}
}

type addIntentItem struct {
	Summary   string  `json:"summary"`
	AssetIDs  []int64 `json:"asset_ids"`
	ParentIDs []int64 `json:"parent_ids"`
	Priority  int     `json:"priority"`
	StepID    int64   `json:"step_id"`
	OffPlan   bool    `json:"off_plan"`
	Budget    struct {
		MaxTurns   int `json:"max_turns"`
		MaxSeconds int `json:"max_seconds"`
	} `json:"budget"`
}

func handleAddIntent(ctx context.Context, rc *Context, raw json.RawMessage) (Result, error) {
	if rc.Final {
		return Errorf("本轮是收尾轮：只判定目标，不再派新意图。请用 prove_goal 判定已达成目标，或直接总结。"), nil
	}
	var in struct {
		Intents []addIntentItem `json:"intents"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return Errorf("参数解析失败：%v", err), nil
	}
	if len(in.Intents) == 0 {
		return Errorf("intents 不能为空；如果确实不该派，本轮就不要调用这个工具"), nil
	}
	if len(in.Intents) > 4 {
		return Errorf("一次最多 4 条（收到 %d）：只提交最高价值的几条", len(in.Intents)), nil
	}

	type result struct {
		Index   int    `json:"index"`
		ID      int64  `json:"id,omitempty"`
		Summary string `json:"summary,omitempty"`
		Error   string `json:"error,omitempty"`
	}
	results := make([]result, 0, len(in.Intents))
	for i, it := range in.Intents {
		if strings.TrimSpace(it.Summary) == "" {
			results = append(results, result{Index: i, Error: "summary 不能为空"})
			continue
		}
		if err := checkAssetsInScope(ctx, rc, it.AssetIDs); err != nil {
			results = append(results, result{Index: i, Summary: it.Summary, Error: err.Error()})
			continue
		}
		// 有计划图时，新方向必须挂在某个步骤上；确实要计划外探索，用 off_plan 显式声明。
		// 这是 P1-1 的 S3（强制）阶段，但保留逃生门，避免计划图僵化探索。
		if pg, err := rc.Store.PlanGraph(ctx, rc.RunID); err == nil && len(pg.Steps) > 0 &&
			it.StepID == 0 && !it.OffPlan {
			results = append(results, result{Index: i, Summary: it.Summary,
				Error: "本任务已有计划图：请把这条方向挂到某个步骤（step_id），或用 off_plan=true 显式说明这是计划外探索"})
			continue
		}
		id, err := rc.Store.AddIntent(ctx, store.AddIntentInput{
			RunID: rc.RunID, Summary: it.Summary, AssetIDs: it.AssetIDs,
			ParentIDs: it.ParentIDs, Priority: it.Priority, Origin: "planner",
			BudgetTurns: it.Budget.MaxTurns, BudgetSeconds: it.Budget.MaxSeconds,
		})
		if err != nil {
			results = append(results, result{Index: i, Summary: it.Summary, Error: err.Error()})
			continue
		}
		if it.StepID > 0 {
			if err := rc.Store.LinkStepIntent(ctx, rc.RunID, it.StepID, id); err != nil {
				results = append(results, result{Index: i, ID: id, Summary: it.Summary,
					Error: "意图已建，但挂到步骤失败：" + err.Error()})
				continue
			}
		}
		results = append(results, result{Index: i, ID: id, Summary: it.Summary})
	}
	return JSONResult(map[string]any{"results": results}), nil
}

// NewProveGoalTool 用证据证明目标达成；最后一个目标被证明时，引擎会把任务判 done。
func NewProveGoalTool() *Tool {
	return &Tool{
		Name: "prove_goal",
		Description: `用一条事实或发现证明某个目标已达成。只有拿到可复核的证据才调用。
若目标含"覆盖度达 X%""拿到 N 个"这类量化条件，调用前必须核对态势里的实测值，未达标禁止提前盖章。
证明最后一个未达成目标时，系统会自动把整个任务判为完成。`,
		Schema: toolSpecJSON(`{
  "type": "object",
  "properties": {
    "goal_id": {"type": "integer"},
    "evidence_id": {"type": "integer", "description": "事实或发现的节点 id"},
    "reason": {"type": "string", "description": "一句话说明证据如何证明目标"}
  },
  "required": ["goal_id", "evidence_id"]
}`),
		Handle: handleProveGoal,
	}
}

func handleProveGoal(ctx context.Context, rc *Context, raw json.RawMessage) (Result, error) {
	var in struct {
		GoalID     int64  `json:"goal_id"`
		EvidenceID int64  `json:"evidence_id"`
		Reason     string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return Errorf("参数解析失败：%v", err), nil
	}
	remaining, err := rc.Store.ProveGoal(ctx, rc.RunID, in.GoalID, in.EvidenceID, in.Reason)
	if err != nil {
		return Errorf("%v", err), nil
	}
	return JSONResult(map[string]any{
		"goal_id":              in.GoalID,
		"met":                  true,
		"remaining_open_goals": remaining,
		"task_completed":       remaining == 0,
	}), nil
}

// NewListFactsTool 给 planner 看事实清单（列表只给摘要，全文用 node_detail）。
func NewListFactsTool() *Tool {
	return &Tool{
		Name:        "list_facts",
		Description: `列出本任务的事实（id / 摘要 / 置信度 / 来自哪条意图）。只看摘要，全文用 node_detail。`,
		ReadOnly:    true,
		Schema: toolSpecJSON(`{
  "type": "object",
  "properties": {
    "q": {"type": "string", "description": "按摘要/详情关键字过滤"},
    "limit": {"type": "integer", "description": "默认 20，最大 100"}
  }
}`),
		Handle: func(ctx context.Context, rc *Context, raw json.RawMessage) (Result, error) {
			var in struct {
				Q     string `json:"q"`
				Limit int    `json:"limit"`
			}
			_ = json.Unmarshal(raw, &in)
			if in.Limit <= 0 || in.Limit > 100 {
				in.Limit = 20
			}
			nodes, err := rc.Store.NodesByKind(ctx, rc.RunID, model.KindFact, 200)
			if err != nil {
				return Errorf("读事实失败：%v", err), nil
			}
			edges, _ := rc.Store.Edges(ctx, rc.RunID)
			fromIntent := map[int64]int64{}
			for _, e := range edges {
				if e.Rel == model.RelYields {
					fromIntent[e.DstID] = e.SrcID
				}
			}
			items := make([]map[string]any, 0, len(nodes))
			for _, n := range nodes {
				if in.Q != "" && !strings.Contains(n.Summary(), in.Q) &&
					!strings.Contains(n.PayloadField("detail"), in.Q) {
					continue
				}
				items = append(items, map[string]any{
					"id": n.ID, "summary": model.TrimSummary(n.Summary(), 160),
					"confidence": n.PayloadField("confidence"), "state": n.State,
					"from_intent": fromIntent[n.ID],
				})
				if len(items) >= in.Limit {
					break
				}
			}
			total, _ := rc.Store.CountByKind(ctx, rc.RunID, model.KindFact)
			return JSONResult(map[string]any{"items": items, "total": total}), nil
		},
	}
}

// NewGetWorkerOutputTool 让 planner 判断"这条意图到底做出了什么"。
func NewGetWorkerOutputTool() *Tool {
	return &Tool{
		Name:        "get_worker_output",
		Description: `取某条意图最后一段输出（≤800 字）。用于判断它是正常跑完、还是被预算掐断。`,
		ReadOnly:    true,
		Schema: toolSpecJSON(`{
  "type": "object",
  "properties": {"intent_id": {"type": "integer"}},
  "required": ["intent_id"]
}`),
		Handle: func(ctx context.Context, rc *Context, raw json.RawMessage) (Result, error) {
			var in struct {
				IntentID int64 `json:"intent_id"`
			}
			if err := json.Unmarshal(raw, &in); err != nil {
				return Errorf("参数解析失败：%v", err), nil
			}
			out, err := rc.Store.LastIntentOutput(ctx, rc.RunID, in.IntentID)
			if err != nil {
				return Errorf("读输出失败：%v", err), nil
			}
			trimmed := model.TrimSummary(out, 800)
			return JSONResult(map[string]any{
				"intent_id": in.IntentID,
				"output":    trimmed,
				"truncated": len([]rune(out)) > 800,
			}), nil
		},
	}
}

// NewKillWorkTool 止损：取消一条在跑的意图。
func NewKillWorkTool() *Tool {
	return &Tool{
		Name: "kill_work",
		Description: `取消一条正在跑的意图。只在该方向已经明确错误（证据证明是死路、或明显跑偏）时用；
不要用它代替"等一等"。`,
		Schema: toolSpecJSON(`{
  "type": "object",
  "properties": {"intent_id": {"type": "integer"}, "reason": {"type": "string"}},
  "required": ["intent_id"]
}`),
		Handle: func(ctx context.Context, rc *Context, raw json.RawMessage) (Result, error) {
			var in struct {
				IntentID int64  `json:"intent_id"`
				Reason   string `json:"reason"`
			}
			if err := json.Unmarshal(raw, &in); err != nil {
				return Errorf("参数解析失败：%v", err), nil
			}
			if rc.KillWork == nil || !rc.KillWork(in.IntentID) {
				return Errorf("当前没有运行中的意图 #%d（可能已经结束）", in.IntentID), nil
			}
			return JSONResult(map[string]any{"intent_id": in.IntentID, "killed": true}), nil
		},
	}
}

// NewSteerWorkTool 让 planner 对正在跑的 work 做**实时纠偏**（不杀进程、不丢进展）。
//
// 语义：消息进队列，worker 在**下一次工具调用之前**取走，并拦下那次调用，
// 把消息当作工具结果交回模型。所以它是"下一步生效"，不是"立刻打断"。
func NewSteerWorkTool() *Tool {
	return &Tool{
		Name: "steer_work",
		Description: `对一条正在跑的意图做实时纠偏：消息会在它下一次工具调用**之前**送到，
并拦下那次调用（模型能看到并改道）。不杀进程、不丢已经做到的进展。
适用：方向对但打法不对（例如「别再试 union 了，改盲注」）、要它优先核实某个点、提醒约束。
不适用：方向整个错了 —— 那用 kill_work。`,
		Schema: toolSpecJSON(`{
  "type": "object",
  "properties": {
    "intent_id": {"type": "integer"},
    "message": {"type": "string", "description": "一句话纠偏指令，要具体"}
  },
  "required": ["intent_id", "message"]
}`),
		Handle: func(ctx context.Context, rc *Context, raw json.RawMessage) (Result, error) {
			var in struct {
				IntentID int64  `json:"intent_id"`
				Message  string `json:"message"`
			}
			if err := json.Unmarshal(raw, &in); err != nil {
				return Errorf("参数解析失败：%v", err), nil
			}
			if strings.TrimSpace(in.Message) == "" {
				return Errorf("message 不能为空：纠偏要说清「改成什么」，而不是「注意一下」"), nil
			}
			if rc.SteerWork == nil || !rc.SteerWork(in.IntentID, in.Message) {
				return Errorf("当前没有运行中的意图 #%d（可能已经结束）；结束了的意图请用 add_intent 重新派", in.IntentID), nil
			}
			return JSONResult(map[string]any{
				"intent_id": in.IntentID, "steered": true,
				"note": "消息会在该意图下一次工具调用前生效（并拦下那次调用）",
			}), nil
		},
	}
}

// NewPlannerRegistry 是 planner 的工具面：读态势 + 产意图 + 判目标 + 跨 work 回看。
// 注意它没有 http_request / insert_assets / record_fact —— 规划者不亲自干活。
func NewPlannerRegistry() *Registry {
	return NewRegistry(
		NewOverviewTool(),
		NewRefreshOverviewTool(),
		NewListFactsTool(),
		NewListFindingsTool(),
		NewNodeDetailTool(),
		NewGetWorkerOutputTool(),
		NewAddPlanStepTool(),
		NewListPlanTool(),
		NewMarkStepDoneTool(),
		NewAddIntentTool(),
		NewProveGoalTool(),
		NewKillWorkTool(),
		NewSteerWorkTool(),
	)
}

// NewWorkerRegistry 是 worker 的工具面：只做领到的那一条 + 三种写回。
// 它拿不到 add_intent / prove_goal / add_scope —— 边界靠工具面，不靠 prompt。
func NewWorkerRegistry() *Registry {
	return NewRegistry(
		NewHTTPTool(),
		NewTrafficSearchTool(),
		NewTrafficGetTool(),
		NewInsertAssetsTool(),
		NewRecordFactTool(),
		NewReportFindingTool(),
		NewNodeDetailTool(),
		NewListFindingsTool(),
	)
}

// checkAssetsInScope 在"建意图之前"过范围闸门：越界实体根本进不了图。
func checkAssetsInScope(ctx context.Context, rc *Context, assetIDs []int64) error {
	if len(assetIDs) == 0 {
		return nil
	}
	if rc.Scopes == nil || rc.Scopes.Empty() {
		return fmt.Errorf("本任务尚未登记授权范围，不能绑定目标实体")
	}
	for _, id := range assetIDs {
		e, err := rc.Store.Entity(ctx, id)
		if err != nil {
			return fmt.Errorf("asset_id #%d 不存在；请先用 insert_assets 登记", id)
		}
		host, ip := hostOfEntity(e)
		if ip.IsValid() && rc.Scopes.AllowsIP(ip) {
			continue
		}
		if host != "" && rc.Scopes.AllowsHost(host) {
			continue
		}
		return fmt.Errorf("asset_id #%d（%s）不在本任务授权范围内：%s", id, firstNonEmptyStr(host, ip.String()), rc.Scopes.Describe())
	}
	return nil
}

func hostOfEntity(e *model.Entity) (string, netip.Addr) {
	if e == nil {
		return "", netip.Addr{}
	}
	switch e.Kind {
	case "ip":
		if ip, err := domain.ParseIP(e.IP); err == nil {
			return e.IP, ip
		}
	case "root_domain", "subdomain":
		return e.Domain, netip.Addr{}
	case "service", "endpoint":
		if e.URL != "" {
			if h, err := domain.HostOfURL(e.URL); err == nil {
				if ip, err := domain.ParseIP(h); err == nil {
					return h, ip
				}
				return h, netip.Addr{}
			}
		}
		return e.Domain, netip.Addr{}
	}
	return e.Domain, netip.Addr{}
}
