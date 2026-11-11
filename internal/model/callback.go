package model

import "time"

// Callback 是操作者显式登记的回连白名单（M17 执行沙箱用）。
//
// run_scope 回答"我们允许打谁"；Callback 回答"谁允许连回沙箱"。
// 两者方向相反、地址不同（操作者侧）、端口动态，所以是独立的一张表。
//
// 只有 operator 角色能创建/吊销；worker/planner 没有对应工具（防提示注入）。
type Callback struct {
	ID        int64
	RunID     int64
	Host      string
	Proto     string // tcp | udp
	Ports     string // "4444" 或 "40000-40100"
	Note      string
	CreatedBy string
	CreatedAt time.Time
	ExpiresAt time.Time
	RevokedAt *time.Time
}

// Active 判断一条白名单当前是否生效（未吊销且未过期）。
func (c *Callback) Active(now time.Time) bool {
	return c != nil && c.RevokedAt == nil && c.ExpiresAt.After(now)
}
