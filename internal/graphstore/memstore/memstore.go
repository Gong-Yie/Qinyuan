// Package memstore 是 graphstore.Graph 的内存实现（P2-4 的收益兑现）。
//
// 用途只有一个：让 planner 的决策输入（态势组装、计划 ready 判定、计数）能在
// **不起 PostgreSQL** 的情况下单测。它不是生产存储，也不打算实现 CAS/事务 ——
// 写路径仍然只属于 store（那是正确性的地方）。
package memstore

import (
	"context"
	"fmt"
	"sync"

	"qinyuan/internal/model"
)

// Store 是一份内存图。
type Store struct {
	mu sync.Mutex

	runs     map[int64]*model.Run
	scopes   map[int64][]model.Scope
	nodes    map[int64][]*model.Node
	edges    map[int64][]model.Edge
	entities map[int64][]*model.Entity
	atts     map[int64][]*model.Attachment
	plan     map[int64]*model.PlanGraph
	coverage map[int64]*model.Coverage
	campaign map[int64]*model.CampaignView

	nextID int64
}

// New 建一个空的内存存储。
func New() *Store {
	return &Store{
		runs: map[int64]*model.Run{}, scopes: map[int64][]model.Scope{},
		nodes: map[int64][]*model.Node{}, edges: map[int64][]model.Edge{},
		entities: map[int64][]*model.Entity{}, plan: map[int64]*model.PlanGraph{},
		atts:     map[int64][]*model.Attachment{},
		coverage: map[int64]*model.Coverage{}, campaign: map[int64]*model.CampaignView{},
		nextID: 100,
	}
}

// AddRun 预置一个 run 与它的范围。
func (s *Store) AddRun(r *model.Run, scopes []model.Scope) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs[r.ID] = r
	s.scopes[r.ID] = append([]model.Scope(nil), scopes...)
}

// AddNode 加一个节点（返回分配到的 id）。
func (s *Store) AddNode(runID int64, n *model.Node) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	n.ID = s.nextID
	n.RunID = runID
	s.nodes[runID] = append(s.nodes[runID], n)
	return n.ID
}

// AddEdge 加一条边。
func (s *Store) AddEdge(runID int64, e model.Edge) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e.RunID = runID
	s.edges[runID] = append(s.edges[runID], e)
}

// AddEntity 预置一个实体。
func (s *Store) AddEntity(runID int64, e *model.Entity) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entities[runID] = append(s.entities[runID], e)
}

// AddAttachment 预置一个附件。
func (s *Store) AddAttachment(runID int64, a *model.Attachment) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	a.ID = s.nextID
	a.RunID = runID
	s.atts[runID] = append(s.atts[runID], a)
}

// Attachments 列附件。
func (s *Store) Attachments(_ context.Context, runID int64) ([]*model.Attachment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*model.Attachment(nil), s.atts[runID]...), nil
}

// SetPlan 预置计划图。
func (s *Store) SetPlan(runID int64, pg *model.PlanGraph) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.plan[runID] = pg
}

// SetCoverage 预置覆盖度。
func (s *Store) SetCoverage(runID int64, c *model.Coverage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.coverage[runID] = c
}

// SetCampaignView 预置战役视图。
func (s *Store) SetCampaignView(runID int64, v *model.CampaignView) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.campaign[runID] = v
}

// Run 读 run。
func (s *Store) Run(_ context.Context, runID int64) (*model.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[runID]
	if !ok {
		return nil, fmt.Errorf("memstore: run %d 不存在", runID)
	}
	return r, nil
}

// Scopes 读范围。
func (s *Store) Scopes(_ context.Context, runID int64) ([]model.Scope, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.Scope(nil), s.scopes[runID]...), nil
}

// Edges 读边。
func (s *Store) Edges(_ context.Context, runID int64) ([]model.Edge, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.Edge(nil), s.edges[runID]...), nil
}

// NodesByKind 按 kind 取（id 倒序，与真实 store 一致）。
func (s *Store) NodesByKind(_ context.Context, runID int64, kind string, limit int) ([]*model.Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 {
		limit = 50
	}
	var all []*model.Node
	for i := len(s.nodes[runID]) - 1; i >= 0; i-- {
		if s.nodes[runID][i].Kind == kind {
			all = append(all, s.nodes[runID][i])
			if len(all) >= limit {
				break
			}
		}
	}
	return all, nil
}

// CountByKind 计数。
func (s *Store) CountByKind(_ context.Context, runID int64, kind string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, x := range s.nodes[runID] {
		if x.Kind == kind {
			n++
		}
	}
	return n, nil
}

// CountIntentState 计数某状态的意图。
func (s *Store) CountIntentState(_ context.Context, runID int64, state string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, x := range s.nodes[runID] {
		if x.Kind == model.KindIntent && x.State == state {
			n++
		}
	}
	return n, nil
}

// HasActiveIntent 是否还有 open/running 意图。
func (s *Store) HasActiveIntent(_ context.Context, runID int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range s.nodes[runID] {
		if x.Kind == model.KindIntent && (x.State == model.StateOpen || x.State == model.StateRunning) {
			return true, nil
		}
	}
	return false, nil
}

// ListEntitiesForRun 列实体。
func (s *Store) ListEntitiesForRun(_ context.Context, runID int64, limit int) ([]*model.Entity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.entities[runID]
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return append([]*model.Entity(nil), out...), nil
}

// PlanGraph 读计划图（没有就是空视图，与真实 store 一致）。
func (s *Store) PlanGraph(_ context.Context, runID int64) (*model.PlanGraph, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if pg, ok := s.plan[runID]; ok {
		return pg, nil
	}
	return &model.PlanGraph{}, nil
}

// Coverage 读覆盖度（没有就是 0/0）。
func (s *Store) Coverage(_ context.Context, runID int64) (*model.Coverage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.coverage[runID]; ok {
		return c, nil
	}
	return &model.Coverage{}, nil
}

// CampaignViewForRun 读战役视图（默认 nil = 独立任务）。
func (s *Store) CampaignViewForRun(_ context.Context, runID int64) (*model.CampaignView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.campaign[runID], nil
}
