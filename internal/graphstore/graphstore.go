// Package graphstore 是《升级计划》P2-4 的可插拔图访问接口。
//
// 只抽 agent 真正用到的那一小块（11 个方法），不把整个 store 搬过来：
//   - 读：Run / Scopes / Edges / NodesByKind / CountByKind / CountIntentState /
//     HasActiveIntent / ListEntitiesForRun / PlanGraph / Coverage / CampaignViewForRun
//   - 写：本接口暂时只读。写路径（AddIntent/AddFact/record_finding）仍走具体 store，
//     因为它们与事务、版本戳（rev）、outbox 强耦合，抽象早了就会错（P2-4 的风险栏）。
//
// 收益（已经兑现的部分）：
//   - planner 的态势组装（BuildOverview）不再绑定 PostgreSQL；
//   - memstore 让"计划链 ready 判定 / 计数 / 渲染"能在纯内存里单测，不必起库。
//
// 刻意保留的依赖：Coverage / CampaignView / PlanGraph 这些 **DTO 类型** 仍来自 store。
// 把它们搬走是一次更大的重构（要连 model 一起分层），P2-4 的验收只要求"agent 不再
// 直接依赖具体存储实现"，不要求顺手完成整个分层。
package graphstore

import (
	"context"

	"qinyuan/internal/model"
	"qinyuan/internal/store"
)

// Graph 是构建"态势"所需的最小图访问面。
type Graph interface {
	Run(ctx context.Context, runID int64) (*model.Run, error)
	Scopes(ctx context.Context, runID int64) ([]model.Scope, error)
	Edges(ctx context.Context, runID int64) ([]model.Edge, error)
	NodesByKind(ctx context.Context, runID int64, kind string, limit int) ([]*model.Node, error)
	CountByKind(ctx context.Context, runID int64, kind string) (int, error)
	CountIntentState(ctx context.Context, runID int64, state string) (int, error)
	HasActiveIntent(ctx context.Context, runID int64) (bool, error)
	ListEntitiesForRun(ctx context.Context, runID int64, limit int) ([]*model.Entity, error)
	PlanGraph(ctx context.Context, runID int64) (*store.PlanGraph, error)
	Coverage(ctx context.Context, runID int64) (*store.Coverage, error)
	CampaignViewForRun(ctx context.Context, runID int64) (*store.CampaignView, error)
}

// 编译期断言：真实存储必须满足接口（改签名会在这里炸，而不是在调用点）。
var _ Graph = (*store.Store)(nil)
