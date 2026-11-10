package agent

import (
	"context"
	"strings"

	"qinyuan/internal/llm"
)

// 本文件是"上下文预算"的第三层：压缩（学习.md 4.1 / 实现规格 04）。
//
// 三层预算各管一段，不能混：
//
//	回合/时长  → MaxTurns / MaxDuration（撞到进收尾轮，见 settlement.go）
//	上下文窗口 → 这里：micro-compact（清旧工具结果）+ 摘要（把中段对话压成一段）
//	单条工具输出 → MaxOutput（超过落盘，只回 head + 指针，见 tools.go）
//
// 实现上刻意**不引入 tokenizer**：估算用"CJK 一字一 token、其它 4 字符一 token"，
// 误差对"什么时候该压"足够（阈值留了余量），换来的是零依赖、可单测。

// CompactionConfig 是上下文压缩的配置。
type CompactionConfig struct {
	// WindowTokens 上下文窗口（token）。<=0 表示关闭压缩。
	WindowTokens int
	// Threshold 用到窗口的百分之多少开始压（默认 0.6）。
	Threshold float64
	// KeepRounds 保留最近几"轮"（一轮 = assistant 消息 + 它之后的 tool 结果）。
	KeepRounds int
	// SummarizeMaxTokens 摘要输出上限（默认 1200）。
	SummarizeMaxTokens int
}

func (c CompactionConfig) normalized() CompactionConfig {
	if c.WindowTokens <= 0 {
		return c // 关闭
	}
	if c.Threshold <= 0 || c.Threshold >= 1 {
		c.Threshold = 0.6
	}
	if c.KeepRounds <= 0 {
		c.KeepRounds = 6
	}
	if c.SummarizeMaxTokens <= 0 {
		c.SummarizeMaxTokens = 1200
	}
	return c
}

func (c CompactionConfig) enabled() bool { return c.WindowTokens > 0 }

// limit 是本轮允许占用的 token 数（窗口 × 阈值）。
func (c CompactionConfig) limit() int {
	c = c.normalized()
	if c.WindowTokens <= 0 {
		return 0
	}
	return int(float64(c.WindowTokens) * c.Threshold)
}

// 压缩用的标记。测试与前端都靠它识别"这里被压过"，所以不要随手改字面量。
const (
	microCompactMarker = "…（旧工具结果已清理，需要时用 node_detail / 重新执行命令取回）"
	summaryHeader      = "\n\n【历史摘要（更早的对话已压缩；需要细节请重新执行工具）】\n"
)

// EstimateTokens 是 token 数的粗略估计。
//
// CJK 按 1 字 1 token（中文 BPE 通常 1~2 字/token，这里偏保守）；
// 其它按 4 字符 1 token。只用于"该不该压"，不用于记账。
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	cjk, other := 0, 0
	for _, r := range s {
		switch {
		case r >= 0x2E80: // CJK / 全角标点 / 日韩
			cjk++
		default:
			other++
		}
	}
	return cjk + (other+3)/4
}

// estimateMessages 把 system + 全部消息折算成 token 估计。
func estimateMessages(system string, msgs []llm.Message) int {
	total := EstimateTokens(system)
	for _, m := range msgs {
		total += 4 // 每条消息的固定开销
		total += EstimateTokens(m.Text)
		for _, tc := range m.ToolCalls {
			total += EstimateTokens(string(tc.Input)) + EstimateTokens(tc.Name)
		}
	}
	return total
}

// splitRounds 把消息切成"首条 user 消息 + 若干轮"。
//
// 一轮 = 一条 assistant（可能带 tool_calls）+ 它后面紧跟着的 tool 结果。
// 这样切是为了**不破坏 tool_call ↔ tool_result 的配对**：OpenAI 兼容端点会拒绝
// 孤立的 tool 消息，按轮切永远成对。
func splitRounds(msgs []llm.Message) (head llm.Message, rounds [][]llm.Message) {
	if len(msgs) == 0 {
		return llm.Message{}, nil
	}
	head = msgs[0]
	for _, m := range msgs[1:] {
		if len(rounds) == 0 || m.Role == llm.RoleAssistant || m.Role == llm.RoleUser {
			rounds = append(rounds, []llm.Message{m})
			continue
		}
		rounds[len(rounds)-1] = append(rounds[len(rounds)-1], m)
	}
	return head, rounds
}

func flattenRounds(rounds [][]llm.Message) []llm.Message {
	out := make([]llm.Message, 0, len(rounds))
	for _, r := range rounds {
		out = append(out, r...)
	}
	return out
}

// compactMessages 是压缩的唯一入口。
//
// 顺序（从便宜到贵）：
//  1. 没超阈值 → 原样返回；
//  2. micro-compact：把老轮次里的工具结果换成一句标记（非破坏，可重新执行取回）；
//  3. 还超 → 摘要：老轮次交给模型压成一段，保留首条 user 消息与最近 KeepRounds 轮；
//  4. 摘要后仍超 → 硬阀门：只留最近 2 轮，并把剩余工具结果截断。
func compactMessages(ctx context.Context, provider llm.Provider, cfg CompactionConfig, system string, msgs []llm.Message) ([]llm.Message, bool, error) {
	cfg = cfg.normalized()
	if !cfg.enabled() || len(msgs) < 3 {
		return msgs, false, nil
	}
	if estimateMessages(system, msgs) <= cfg.limit() {
		return msgs, false, nil
	}

	head, rounds := splitRounds(msgs)
	if len(rounds) <= cfg.KeepRounds {
		// 轮次本来就少：只做 micro-compact，不动结构。
		compacted, changed := microCompact(rounds, 0)
		return rebuild(head, compacted), changed, nil
	}

	// ① micro-compact：只清"保留窗口之外"的老工具结果。
	rounds, changed := microCompact(rounds, cfg.KeepRounds)
	if changed {
		if estimateMessages(system, rebuild(head, rounds)) <= cfg.limit() {
			return rebuild(head, rounds), true, nil
		}
	}

	// ② 摘要中段：保留最近 KeepRounds 轮，其余交给模型压成一段。
	keep := cfg.KeepRounds
	if keep >= len(rounds) {
		keep = len(rounds) - 1
	}
	old, recent := rounds[:len(rounds)-keep], rounds[len(rounds)-keep:]
	summary, err := summarizeRounds(ctx, provider, cfg, old)
	if err != nil {
		// 摘要失败不能让这一轮直接失败：退回 micro-compact 的结果（至少比不压好）。
		return rebuild(head, rounds), true, nil
	}
	head.Text = head.Text + summaryHeader + summary
	out := rebuild(head, recent)

	// ③ 硬阀门：摘要后仍超窗（例如最近几轮本身巨大），只能截断最近轮里的工具结果。
	if estimateMessages(system, out) > cfg.limit() {
		trimmed := make([]llm.Message, 0, len(out))
		for i, m := range out {
			if i > 0 && m.Role == llm.RoleTool && len([]rune(m.Text)) > 2000 {
				m.Text = string([]rune(m.Text)[:2000]) + "\n" + microCompactMarker
			}
			trimmed = append(trimmed, m)
		}
		out = trimmed
	}
	return out, true, nil
}

func rebuild(head llm.Message, rounds [][]llm.Message) []llm.Message {
	out := make([]llm.Message, 0, 1+len(rounds)*2)
	out = append(out, head)
	return append(out, flattenRounds(rounds)...)
}

// microCompact 把"保留窗口之前"的工具结果清成标记（保留头部，便于模型回忆）。
func microCompact(rounds [][]llm.Message, keepLast int) ([][]llm.Message, bool) {
	cut := len(rounds) - keepLast
	if cut < 0 {
		cut = 0
	}
	changed := false
	for i := 0; i < cut; i++ {
		for j, m := range rounds[i] {
			if m.Role != llm.RoleTool || strings.Contains(m.Text, microCompactMarker) {
				continue
			}
			rounds[i][j].Text = headRunes(m.Text, 200) + "\n" + microCompactMarker
			changed = true
		}
	}
	return rounds, changed
}

// summarizeRounds 把老轮次压成一段摘要（一次模型调用）。
func summarizeRounds(ctx context.Context, provider llm.Provider, cfg CompactionConfig, rounds [][]llm.Message) (string, error) {
	var b strings.Builder
	for _, r := range rounds {
		for _, m := range r {
			switch m.Role {
			case llm.RoleAssistant:
				if m.Text != "" {
					b.WriteString("助手：" + headRunes(m.Text, 500) + "\n")
				}
				for _, tc := range m.ToolCalls {
					b.WriteString("调用：" + tc.Name + " " + headRunes(string(tc.Input), 300) + "\n")
				}
			case llm.RoleTool:
				b.WriteString("结果：" + headRunes(m.Text, 500) + "\n")
			default:
				b.WriteString("用户：" + headRunes(m.Text, 500) + "\n")
			}
		}
	}
	if b.Len() == 0 {
		return "（无可摘要内容）", nil
	}

	resp, err := provider.Complete(ctx, &llm.Request{
		SessionKey: "compaction",
		System: `你是「钦原」的上下文压缩器。把下面这段执行过程压成一份**可继续工作**的摘要。
要求：
1. 只保留"已确认的事实、拿到过的证据、做过的尝试与结果、还差什么"；
2. 保留节点 id / 实体名 / 命令关键参数这类能被后续核对的信息；
3. 不要罗列过程、不要复述工具输出原文；总长不超过 300 字；
4. 如果某条结论的 evidence 不完整，明确标注"证据不足"。`,
		Messages: []llm.Message{{Role: llm.RoleUser, Text: b.String()}},
	})
	if err != nil {
		return "", err
	}
	summary := strings.TrimSpace(resp.Text)
	if summary == "" {
		summary = "（压缩器没有产出内容）"
	}
	return headRunes(summary, cfg.SummarizeMaxTokens*2), nil // 2 字符/token 的保守上限
}

func headRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
