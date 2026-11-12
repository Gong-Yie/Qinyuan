package model

import "strings"

// 审批模式（M21）。
//
//   - manual：命中 ask 就挂起，等人（默认，保持现有安全边界）
//   - auto：命中 ask 自动放行；deny 仍然绝对优先（不因为自动化而放宽拒绝）
//   - off：整个审批门跳过（连 deny 也不看）；只应由 operator 在受控任务上使用
//
// 空字符串表示"跟随全局默认"：run 级留空时用 QINYUAN_APPROVAL_MODE 的值。
const (
	ApprovalModeInherit = ""
	ApprovalModeManual  = "manual"
	ApprovalModeAuto    = "auto"
	ApprovalModeOff     = "off"
)

// NormalizeApprovalMode 把用户输入（含 inherit/空）归一化；第二个返回值表示是否合法。
func NormalizeApprovalMode(s string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "inherit":
		return ApprovalModeInherit, true
	case ApprovalModeManual:
		return ApprovalModeManual, true
	case ApprovalModeAuto:
		return ApprovalModeAuto, true
	case ApprovalModeOff:
		return ApprovalModeOff, true
	default:
		return "", false
	}
}

// EffectiveApprovalMode 把 run 级模式与全局默认合成最终模式。
// 非法/空值一律回落到 manual，绝不因为脏数据把门关掉。
func EffectiveApprovalMode(runMode, defaultMode string) string {
	if m, ok := NormalizeApprovalMode(runMode); ok && m != ApprovalModeInherit {
		return m
	}
	if m, ok := NormalizeApprovalMode(defaultMode); ok && m != ApprovalModeInherit {
		return m
	}
	return ApprovalModeManual
}
