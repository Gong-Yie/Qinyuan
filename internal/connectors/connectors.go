// Package connectors 是《升级计划》P3-3 的双向集成层：把 outbox 里的事件推给外部系统，
// 并把外部状态拉回来写回 finding。
//
// 三条纪律：
//  1. **幂等靠台账**（store.connector_deliveries）：outbox 保"至少一次"，
//     外部系统要"恰好一次"，重复推同一个事件不能建两个单；
//  2. **失败向上抛**：由 outbox 退避重试/进死信，连接器自己不重试（否则两层重试会放大）；
//  3. **回写只改 payload 的一个字段**（ticket_status/ticket_id），不新增副本 ——
//     findings 的单一真相源是 nodes(kind='finding')。
package connectors

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"qinyuan/internal/store"
)

// Event 就是 outbox 事件（不另造类型：投递的语义由 P2-3 定义）。
type Event = store.OutboxEvent

// Sink 是单向"推"的连接器。
type Sink interface {
	Name() string
	Push(ctx context.Context, ev Event) (externalID string, err error)
}

// Update 是外部系统的状态变更。
type Update struct {
	ExternalID string
	Status     string
	Note       string
}

// Puller 是可选能力：能拉状态变更的连接器实现它。
type Puller interface {
	Pull(ctx context.Context) ([]Update, error)
}

// EventKey 是事件在台账里的幂等键。
//
// finding 事件用 "finding:<id>"（稳定、可反查）；其余用 topic + payload 哈希。
func EventKey(ev Event) string {
	var p struct {
		FindingID int64 `json:"finding_id"`
	}
	if json.Unmarshal(ev.Payload, &p) == nil && p.FindingID > 0 {
		return fmt.Sprintf("finding:%d", p.FindingID)
	}
	sum := sha256.Sum256(ev.Payload)
	return ev.Topic + ":" + hex.EncodeToString(sum[:8])
}

// Fanout 把一条事件扇出给所有连接器，并用台账去重。
type Fanout struct {
	Store *store.Store
	Sinks []Sink
	Log   *slog.Logger
}

// Handle 是 outbox 的 handler（topic=finding.recorded 等）。
func (f *Fanout) Handle(ctx context.Context, ev Event) error {
	if f == nil || f.Store == nil {
		return nil
	}
	key := EventKey(ev)
	for _, sink := range f.Sinks {
		d, done, err := f.Store.Delivery(ctx, sink.Name(), key)
		if err != nil {
			return err
		}
		// 只有"推成功过"才算已投递；failed 行留着是为了排障，不能挡住重试。
		if done && d != nil && d.Status != "failed" {
			continue // 已经推过：跳过（这就是"恰好一次"）
		}
		extID, err := sink.Push(ctx, ev)
		if err != nil {
			_ = f.Store.PutDelivery(ctx, sink.Name(), key, "", "failed", err.Error())
			// 让 outbox 重试：已经成功的 sink 下次会被台账跳过。
			return fmt.Errorf("%s 推送失败：%w", sink.Name(), err)
		}
		if err := f.Store.PutDelivery(ctx, sink.Name(), key, extID, "pushed", ""); err != nil {
			return err
		}
		if f.Log != nil {
			f.Log.Info("[connector] 已推送", "connector", sink.Name(), "event", key, "external_id", extID)
		}
	}
	return nil
}

// SyncTickets 拉外部状态并写回 finding（"外部把单改成 fixed，平台跟着变"）。
//
// 只处理实现了 Puller 的连接器；未登记的 external_id 直接跳过（不是错误）。
func (f *Fanout) SyncTickets(ctx context.Context, runID int64) (int, error) {
	applied := 0
	for _, sink := range f.Sinks {
		p, ok := sink.(Puller)
		if !ok {
			continue
		}
		updates, err := p.Pull(ctx)
		if err != nil {
			return applied, fmt.Errorf("%s 拉取失败：%w", sink.Name(), err)
		}
		for _, u := range updates {
			d, found, err := f.Store.DeliveryByExternalID(ctx, sink.Name(), u.ExternalID)
			if err != nil {
				return applied, err
			}
			if !found || !strings.HasPrefix(d.EventKey, "finding:") {
				continue
			}
			var fid int64
			if _, err := fmt.Sscanf(d.EventKey, "finding:%d", &fid); err != nil || fid <= 0 {
				continue
			}
			ok, err := f.Store.SetFindingTicketStatus(ctx, runID, fid, u.Status, u.ExternalID, u.Note)
			if err != nil {
				return applied, err
			}
			if ok {
				applied++
				_ = f.Store.PutDelivery(ctx, sink.Name(), d.EventKey, u.ExternalID, "synced:"+u.Status, "")
			}
		}
	}
	return applied, nil
}

// ---------- 内置连接器 ----------

// Webhook 是通用 HTTP 连接器（工单系统 / 资产回写的通用形态）。
//
// 推送体：{event, event_key, payload}；响应体里若有 {"id": "..."} 就用它当外部 id，
// 否则用 event_key（外部系统至少可以用幂等键去重）。
type Webhook struct {
	Label   string
	URL     string
	Token   string
	Client  *http.Client
	Timeout time.Duration
}

// Name 连接器名（台账里的键，改名等于换一个连接器）。
func (w *Webhook) Name() string { return w.Label }

func (w *Webhook) client() *http.Client {
	if w.Client != nil {
		return w.Client
	}
	timeout := w.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &http.Client{Timeout: timeout}
}

// Push 推一条事件。
func (w *Webhook) Push(ctx context.Context, ev Event) (string, error) {
	if w.URL == "" {
		return "", fmt.Errorf("webhook %s 没配 URL", w.Label)
	}
	key := EventKey(ev)
	body, err := json.Marshal(map[string]any{
		"event": ev.Topic, "event_key": key, "payload": json.RawMessage(ev.Payload),
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	// 幂等键同时给 header 与 body：不同系统的约定不一样，两边都给最稳。
	req.Header.Set("Idempotency-Key", key)
	if w.Token != "" {
		req.Header.Set("Authorization", "Bearer "+w.Token)
	}
	resp, err := w.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("HTTP %d：%s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &out) == nil && out.ID != "" {
		return out.ID, nil
	}
	return key, nil
}

// Pull 拉状态：GET {URL}/status?since= 返回 [{"id","status","note"}]。
//
// 约定很土但够用：任何"工单系统 → 平台"的回写都需要一个能列出变更的端点。
func (w *Webhook) Pull(ctx context.Context) ([]Update, error) {
	if w.URL == "" {
		return nil, nil
	}
	url := strings.TrimSuffix(w.URL, "/") + "/status"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if w.Token != "" {
		req.Header.Set("Authorization", "Bearer "+w.Token)
	}
	resp, err := w.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil // 对面没实现状态端点：不是错误
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d：%s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var items []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Note   string `json:"note"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("状态响应不是合法 JSON：%w", err)
	}
	out := make([]Update, 0, len(items))
	for _, it := range items {
		out = append(out, Update{ExternalID: it.ID, Status: it.Status, Note: it.Note})
	}
	return out, nil
}

// Memory 是测试/演练用的连接器：记录推了什么、可以注入失败。
type Memory struct {
	Label   string
	FailN   int // 前 N 次 Push 失败（测重试）
	mu      sync.Mutex
	calls   int
	pushed  map[string]string // eventKey -> externalID
	updates []Update
}

// Name 连接器名。
func (m *Memory) Name() string { return m.Label }

// Push 记录一次推送（按 event_key 幂等：同 key 返回同一 external id）。
func (m *Memory) Push(_ context.Context, ev Event) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.FailN > 0 {
		m.FailN--
		return "", fmt.Errorf("注入失败（剩余 %d 次）", m.FailN)
	}
	if m.pushed == nil {
		m.pushed = map[string]string{}
	}
	key := EventKey(ev)
	if id, ok := m.pushed[key]; ok {
		return id, nil
	}
	id := fmt.Sprintf("%s-%d", m.Label, len(m.pushed)+1)
	m.pushed[key] = id
	return id, nil
}

// SetUpdates 预置下一次 Pull 的结果。
func (m *Memory) SetUpdates(us []Update) {
	m.mu.Lock()
	m.updates = us
	m.mu.Unlock()
}

// Pull 返回预置的状态变更。
func (m *Memory) Pull(context.Context) ([]Update, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Update(nil), m.updates...), nil
}

// Calls 推送次数（含失败）。
func (m *Memory) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// Pushed 已成功推送的 event_key → externalID。
func (m *Memory) Pushed() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]string{}
	for k, v := range m.pushed {
		out[k] = v
	}
	return out
}
