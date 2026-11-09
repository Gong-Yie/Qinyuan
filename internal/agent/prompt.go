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
