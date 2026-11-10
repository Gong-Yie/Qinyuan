package agent

import "time"

// 本文件是"撞预算时的收尾轮"（settlement，学习.md 4.2 / 实现规格 02 §11）。
//
// 一次执行的预算是硬上限：撞到 MaxTurns/MaxDuration 就必须停。但"停"不等于
// "白跑"——模型手上往往已经拿到了结论，只是没来得及写回。收尾轮做的就是：
//
//	隐藏"继续探测"的工具（http_request） → 注入收尾指令 → 给几轮把结论写回
//
// 关键取舍：**收尾只写回，不改判定**。最终 reason 仍然是 max_turns/timeout，
// 引擎照旧把意图落 exhausted —— 让规划者知道"这是被掐断的，可以续派"。

// SettlementConfig 是收尾轮的配置。
type SettlementConfig struct {
	// MaxTurns 收尾轮最多几回合（默认 3）。
	MaxTurns int
	// Timeout 收尾轮的总时长（默认 2 分钟）：它用的是独立的预算，
	// 不能沿用已经耗尽的那个（撞时长上限时 execCtx 已经过期）。
	Timeout time.Duration
	// Hidden 收尾轮隐藏的工具（默认 http_request）。
	Hidden []string
	// Prompt 收尾指令（默认 DefaultSettlementPrompt）。
	Prompt string
}

// DefaultSettlementPrompt 是收尾轮的指令正文。
//
// 写法上刻意避开"总结一下"这种模糊说法：只让它做三件**可核验**的事。
const DefaultSettlementPrompt = `【收尾轮】你本轮的执行预算已经用完，系统不会再为这条意图开新的探测回合。
不要再发起任何目标交互（http_request 已隐藏，调用会被拒绝）。现在只做三件事：
1. 把已经拿到、但还没写回图里的结论逐条写回：record_fact / report_finding / insert_assets；
2. 写回时保留 evidence 与 confidence，不要补写没见过的东西（拿不准就标 inferred）；
3. 最后用一句话说明：你做到了哪一步、还差什么，然后停止调用工具。`

// DefaultSettlementHidden 是收尾轮默认隐藏的工具。
//
// 只隐藏"往目标打请求"这一类：写回工具必须留着，否则收尾轮就没意义了。
var DefaultSettlementHidden = []string{"http_request"}

func (s SettlementConfig) normalized() SettlementConfig {
	if s.MaxTurns <= 0 {
		s.MaxTurns = 3
	}
	if s.Timeout <= 0 {
		s.Timeout = 2 * time.Minute
	}
	if len(s.Hidden) == 0 {
		s.Hidden = append([]string(nil), DefaultSettlementHidden...)
	}
	if s.Prompt == "" {
		s.Prompt = DefaultSettlementPrompt
	}
	return s
}
