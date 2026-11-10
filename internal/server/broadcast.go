package server

import (
	"sync"

	"qinyuan/internal/model"
)

// Bus 是进程内的按 run 广播（SSE 的数据源）。
//
// 三条纪律（实现规格/05 §4、学习.md 7.3）：
//  1. **可丢**：每个订阅者一个带缓冲的 channel，满了直接丢 —— 慢客户端不能拖住引擎；
//  2. **不做数据源**：断线补齐走 DB（ActivitiesAfter），广播只负责"快"；
//  3. **不 close**：退订只从表里摘掉，避免 Publish 与 close 竞争导致 panic。
type Bus struct {
	mu     sync.RWMutex
	subs   map[int64]map[int]chan *model.Activity
	seq    int
	buffer int
}

// NewBus 构造广播器。
func NewBus() *Bus {
	return &Bus{subs: make(map[int64]map[int]chan *model.Activity), buffer: 128}
}

// Subscribe 订阅某个 run 的活动流；返回的取消函数幂等，且不会 close channel。
func (b *Bus) Subscribe(runID int64) (<-chan *model.Activity, func()) {
	ch := make(chan *model.Activity, b.buffer)
	b.mu.Lock()
	id := b.seq
	b.seq++
	if b.subs[runID] == nil {
		b.subs[runID] = make(map[int]chan *model.Activity)
	}
	b.subs[runID][id] = ch
	b.mu.Unlock()

	cancel := func() {
		b.mu.Lock()
		if m := b.subs[runID]; m != nil {
			delete(m, id)
			if len(m) == 0 {
				delete(b.subs, runID)
			}
		}
		b.mu.Unlock()
	}
	return ch, cancel
}

// Publish 非阻塞广播：没有订阅者或缓冲已满时直接丢弃。
func (b *Bus) Publish(a *model.Activity) {
	if a == nil {
		return
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.subs[a.RunID] {
		select {
		case ch <- a:
		default: // 丢：客户端会用 since 从 DB 补齐
		}
	}
}

// Subscribers 只用于测试与诊断。
func (b *Bus) Subscribers(runID int64) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs[runID])
}
