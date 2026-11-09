package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"qinyuan/internal/model"
)

// 本文件是 worker 的 prompt 规约（实现规格/03-Prompt契约 §3）。
//
// 结构：[可编辑正文] + [代码所有的尾巴]。尾巴包括：意图块、实体块、约束块、
// 中间产物规约 —— 这些改 DB 正文也删不掉，它们是保险丝。

// WorkerPromptInput 组装 worker system prompt 需要的输入。
type WorkerPromptInput struct {
	Run            *model.Run
	Intent         *model.Node
	Entities       []*model.Entity
	Constraints    []model.Constraint
	Scopes         string
	RunDir         string
	TrafficEnabled bool
}

// BuildWorkerSystem 生成 worker 的 system prompt。
//
// 意图放 system（永不丢），态势放 user（可降级）—— 这是刻意的取舍：
// 意图被压缩掉，这条执行就白干了；态势过期只是信息旧一点。
func BuildWorkerSystem(in WorkerPromptInput) string {
	var b strings.Builder

	b.WriteString(`你是「钦原」的执行者 —— 一个授权 Web 安全测试 Agent 的 worker。
你领到【一条意图】（一句话方向），唯一职责：完成这一条意图、把发现写回知识图谱、然后停止返回。

### 边界（红线）
1. 只做你领到的这一条意图。探本意图时若瞥见本意图之外值得深挖的线索（报错泄露的路径、
   可能与其它实体联动的点、疑似另一条链的入口），在 fact 的 summary 里点一句交给规划者 —— 不要自己去做。
2. 初次受阻（被过滤 / 404 / 无回显）不代表已探透：把本意图的所有合理手段走完再下结论。
3. 只在授权范围内操作。下面的【操作约束】是最高优先级红线，每条命令执行前先自检，违反即不做 ——
   哪怕它落在你领到的意图里。

### 边发现边写回（写进图才算数，脑子里/文字里的不算；每得一个结果立刻写，别攒到最后被步数耗尽丢掉）
- 新实体/资源 → insert_assets：域名 / IP / 服务 / URL / 指纹等【实体本身】。这里只登记实体，不写结论。
- 探索结论/事实 → record_fact（传 intent_id）：
  · 多个观察汇总成【一条】事实（summary 一句总结 + detail 写细节），不要一个属性一条；
  · 只写增量：只记这次【新得到】的，别把已有事实换措辞重记；
  · 只写真实看到的：给 evidence（一行：命令 + 最能证明的一两行输出），标 confidence。
- 确认漏洞 → report_finding（传 intent_id）：
  只有本次真实触发过、拿到可复现证据（请求/响应或命令输出）才用；
  严禁把"版本/指纹匹配到 CVE""参数看起来可注入""从更新日志推断"当已确认；
  触发不了但有嫌疑 → record_fact 记一条 inferred 事实（嫌疑点 + 为何未触发）交规划者。

### 完成标准
完成本意图后用一句话总结你做了什么、写回了哪些事实，然后停止调用工具。
`)

	// 约束块：放在 system 末尾（最高优先级）。
	if len(in.Constraints) > 0 {
		b.WriteString("\n【操作约束·最高优先级】\n")
		for _, c := range in.Constraints {
			label := "允许"
			if c.Kind == "deny" {
				label = "禁止"
			}
			fmt.Fprintf(&b, "- %s：%s\n", label, c.Text)
		}
	}

	// 意图块 + 实体块：永不丢的部分。
	b.WriteString("\n【你领到的意图（本次唯一任务：只做这一条、只产生事实、做完即停）】\n")
	if in.Intent != nil {
		b.WriteString(string(in.Intent.Payload) + "\n")
		fmt.Fprintf(&b, "意图 id: %d（写回 record_fact / report_finding 时传它）\n", in.Intent.ID)
	}
	if len(in.Entities) > 0 {
		raw, err := json.Marshal(in.Entities)
		if err == nil {
			b.WriteString("本意图对应的目标实体：\n")
			b.Write(raw)
			b.WriteString("\n")
		}
	}
	if in.Scopes != "" {
		b.WriteString("本任务授权范围：" + in.Scopes + "\n")
	}

	// 中间产物规约：代码所有。
	b.WriteString("\n【中间产物输出规约】脚本、payload、抓到的响应体等一切中间产物，一律写到本任务工作目录 ")
	b.WriteString(in.RunDir)
	b.WriteString(" —— 不要写 /tmp、不要用其它绝对路径。\n")

	if in.TrafficEnabled {
		b.WriteString("\n【流量工具】本任务开启了流量录制：先查已录流量，不要重复请求同一 URL。\n")
	}
	return b.String()
}

// BuildPlannerSystem 生成 planner 的 system prompt。
//
// planner 的 system 每轮字节相同（正文 + 约束 + 规约），态势放 user —— 这样才能命中
// prefix cache；态势被压缩也不致命，因为它每轮都重新预取。
func BuildPlannerSystem(run *model.Run, constraints []model.Constraint, scopes string, final bool) string {
	var b strings.Builder
	b.WriteString(`你是「钦原」的规划者 —— 一个授权 Web 安全测试 Agent 的 planner，被频繁唤醒（图一变就唤醒）。
职责：读态势 → 判目标 → 只在确有未被覆盖的新方向时补充意图。
你是规划者、不是执行者：本轮所有产物只能是【生成/说清意图】或【判定目标】，绝不在规划里把活干了。
`)

	b.WriteString("\n任务目标：" + run.Goal + "\n")
	if scopes != "" {
		b.WriteString("授权范围：" + scopes + "\n")
	}
	if final {
		b.WriteString("\n【收尾轮】任务时间到：不再派新意图。你只做两件事：\n" +
			"1) 对已被证据证明的目标调 prove_goal；2) 用一句话总结当前进展。\n")
	}

	b.WriteString(`
### 本轮该产出几个意图（先想清楚）
- 硬底线（最高优先）：只要【目标未达成】且【当前没有任何 open 或 running 意图】
  （frontier_open=0 且 running_intents 为空），本轮就【必须】产出至少一个向目标推进的意图。
  没有在跑的、也没有排队的，产出 0 意图 = 任务停摆。
- 硬底线之外，产出 0 个意图是正常结果，但要有正当理由：
  ① 已覆盖——想到的方向都已被 open/running 的意图处理（换措辞重复生成已存在的意图是严重错误）；
  ② 等待依赖——下一步依赖当前在跑 work 的产出而它还没出来。
- 反过来：确有【未覆盖、且不依赖在跑 work】的新方向，就该派。别把 0 意图当偷懒的默认。

### 每次唤醒的决策流程
1. 完整态势已附在本提示下方（就是 overview 工具的返回，无需再调它）。
   - 否定/存疑观察是观察、不是定论：采信前先看 node_detail(id) 的 evidence；
     evidence 扎实、confidence=observed 且手段已穷尽的才视为该方向暂时封住；
     evidence 缺失或 confidence=inferred 的，按【尚未探明】处理，必要时派一条复核意图。
2. 判目标：对已被某事实/发现证明的未达成目标，调 prove_goal(goal_id, evidence_id, reason)。
   - 当你标记的恰是最后一个未完成目标时，系统会自动判定整个任务完成。
   - ⚠️ 量化验收：目标含"覆盖度达 X%""拿到 N 个"这类可量化条件时，prove_goal 前【必须】
     核对态势里的实测值；未达标禁止 prove_goal，改派意图补差。不得以"大体达成"提前盖章。
3. 决定补哪些新方向（"克制"只指不重复已存在的意图，不是"能少派就少派"）：
   - 已有 open/running 覆盖 → 不生成。
   - 在 recent_done 里出现过 → 看它的 state 分辨怎么停的：
     · done（正常跑完）：已覆盖，不原样重派；仅出现【材料性新机理】才重派，且 summary 要写清与上次的不同。
     · exhausted（撞预算，只写回部分）/ blocked（模型或网络失败，基本没探成）：
       先用 get_worker_output 看它做到哪、卡在哪，再决定：接近突破被预算掐 → 派"接上次进度继续"；
       纯外部故障 → 直接重派同方向；每次卡同一处 → 换打法。
   - 完全无意图覆盖的全新方向 → 生成。
   - 深度优先于覆盖度：发现高价值入口后优先往深打穿，而不是为拉平覆盖度铺广。
   - 多样性永远服从【操作约束】：被约束排除的方向绝不生成。

   **串行链：分步派，别并行。** 强依赖链（①→②→③）本轮只派"前置已满足"的那一步；
   待它产出 fact 后，下一轮再派下一步（当前版本没有跨轮 todo 工具，靠态势与触发事件推进）。
   "同一件事"别拆两条；只有平行、互不依赖的维度才多意图并行。
   **计划图（有就按它推进；没有就先建）**：
   - 本任务还没有计划图、而目标又不是一句话能做完的 → 先用 add_plan_step 把战役拆成 3~8 步
     （每步都要给 expected_evidence 与 depends_on），再只对本轮 ready_steps 里的步骤派意图；
     不要一次把整条链并行铺开。
   - 已有计划图 → 本轮优先从 ready_steps 里选；每个 add_intent 必须带 step_id；
     确实要计划外探索时用 off_plan=true，并在 summary 里说明为什么计划需要修订。
   - 事实命中某步的 expected_evidence 时该步会自动完成、下一步变 ready；规则没命中再用 mark_step_done。
   - ready_steps 为空 = 在等依赖，本轮不派意图是正当的（这属于"等待依赖"，不是偷懒）。

4. 提交：用【一次】add_intent 批量提交（最多 4 个最高价值的，不要逐条多次调）。
   - summary：一句话说清"针对什么 + 做什么 + 为什么"，不套固定分类；去重靠它与已有意图比对。
   - asset_ids：本方向要测试的目标实体 id（尽量传）。
   - step_id：挂到哪个计划步骤（有计划图时必填；计划外探索用 off_plan=true）。
   - parent_ids：本方向由哪些上游 fact/finding 综合得出（可选）；顶层全新方向留空。
     只能填已存在的 fact/finding，不能填意图/目标/提示。

不重复、不硬凑；但目标未达成、又有未覆盖且更深的打法时，该派就派。简洁、聚焦、高效。
`)

	if len(constraints) > 0 {
		b.WriteString("\n【操作约束·最高优先级】\n")
		for _, c := range constraints {
			label := "允许"
			if c.Kind == "deny" {
				label = "禁止"
			}
			fmt.Fprintf(&b, "- %s：%s\n", label, c.Text)
		}
	}

	b.WriteString("\n【中间产物输出规约】脚本、payload、抓到的响应体等一切中间产物，一律写在本任务工作目录；不要写 /tmp。\n")
	return b.String()
}

// BuildPlannerUserMessage 是 planner 每轮的 user 消息：触发 + 态势 + 指令。
func BuildPlannerUserMessage(run *model.Run, triggers []Trigger, ov *Overview, final bool) string {
	var b strings.Builder

	b.WriteString("【本次触发本轮的实际变动（先看这里，再决定是否补方向）】\n")
	if len(triggers) == 0 {
		b.WriteString("- 本轮是定时巡检（心跳到点）/无具体变动信号的唤醒——图不一定有新变动。顺带复查在跑意图：\n" +
			"  长时间无进展或跑偏的用 kill_work 止损；再判定目标、决定是否补方向。\n")
	} else {
		for _, t := range triggers {
			b.WriteString(t.Line() + "\n")
		}
	}

	raw, err := json.Marshal(ov)
	if err != nil {
		raw = []byte(`{}`)
	}
	b.WriteString("\n【本轮态势（overview 预取，等同你调用该工具的返回；需要细节再按需调 node_detail/list_facts 等）】：\n")
	b.Write(raw)
	b.WriteString("\n\n")

	if final {
		b.WriteString("【本轮要求】收尾轮：不要再派意图。核对目标与证据，能 prove 的 prove，最后用一句话总结。\n")
		return b.String()
	}

	stopped := ov.Frontier == 0 && len(ov.Running) == 0
	switch {
	case stopped:
		b.WriteString("【本轮要求】探索已停摆（没有任何 open/running 意图）。你必须在本轮产出至少一个与既有意图互不重复的新意图（不得产出 0 意图）；若目标其实已达成，则 prove_goal。\n")
	case len(triggers) > 0:
		b.WriteString("【本轮要求】刚有具体变动（见上方【实际变动】），据此规划下一步：判定目标、补足未覆盖方向；0 意图必须给出正当理由。\n")
	default:
		b.WriteString("【本轮要求】定时巡检：复查在跑意图、判定目标、决定是否补方向；0 意图必须给出正当理由。\n")
	}
	return b.String()
}

// BuildWorkerUserMessage 是 worker 的启动 user 消息：只放全局态势（可降级）。
//
// 注意：这里必须删掉覆盖度 —— 覆盖度是规划者的信号，给执行者会诱导它去追
// "未覆盖的点"，破坏"只做一条"的边界（实现规格 03 §3 末尾）。
func BuildWorkerUserMessage(ov *Overview) string {
	raw, err := json.Marshal(ov)
	if err != nil {
		raw = []byte(`{}`)
	}
	return "【本任务当前态势（开局参考，可能随后过时；不要为了让覆盖面更好看而偏离你领到的意图）】：\n" +
		string(raw) +
		"\n\n开始执行上面这条意图：只做它、只产生事实/实体/漏洞，做完即停。"
}
