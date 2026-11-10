package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"qinyuan/internal/domain"
	"qinyuan/internal/store"
)

// 复测：用 finding 绑定的那条交换重放一次，按断言给三档结论。
//
// 为什么不是"看起来还在"：状态码 + 响应指纹（sha / 长度）都要落 expected/observed，
// 否则报告里的 verified 无法复核 —— 这段代码的价值就在于"可被推翻"。

const retestMaxBody = 4 << 20

// RetestFinding 复测一条 finding 的所有绑定证据，逐条落 finding_retests，返回写入的行。
func RetestFinding(ctx context.Context, st *store.Store, runID, findingID int64, allowLoopback bool) ([]store.Retest, error) {
	bindings, err := st.FindingTrafficList(ctx, runID, findingID)
	if err != nil {
		return nil, err
	}
	if len(bindings) == 0 {
		r := store.Retest{
			RunID: runID, FindingID: findingID, Status: store.RetestUnverifiable,
			Expected: "至少一条绑定的请求/响应证据",
			Observed: "没有绑定任何流量（report_finding 时没传 traffic_ids）",
		}
		if id, err := st.AddRetest(ctx, r); err != nil {
			return nil, err
		} else {
			r.ID = id
			r.CreatedAt = time.Now().Unix()
		}
		return []store.Retest{r}, nil
	}

	scopes, err := st.Scopes(ctx, runID)
	if err != nil {
		return nil, err
	}
	scopeSet := domain.NewScopeSet(scopes)

	var out []store.Retest
	for _, b := range bindings {
		exID, _ := b["exchange_id"].(int64)
		row := RetestOne(ctx, st, runID, findingID, exID, scopeSet, allowLoopback)
		id, err := st.AddRetest(ctx, row)
		if err != nil {
			return out, err
		}
		row.ID = id
		row.CreatedAt = time.Now().Unix() // 内存对象回给调用方时补时间（DB 侧由 now() 填）
		out = append(out, row)
	}
	return out, nil
}

// RetestOne 复测一条交换（写成独立函数便于单测）。
func RetestOne(ctx context.Context, st *store.Store, runID, findingID, exchangeID int64,
	scopeSet *domain.ScopeSet, allowLoopback bool) store.Retest {

	row := store.Retest{RunID: runID, FindingID: findingID, ExchangeID: exchangeID}
	if exchangeID == 0 {
		row.Status = store.RetestUnverifiable
		row.Expected = "原始交换可重放"
		row.Observed = "流量已被清理，只剩证据快照（预览 + sha）"
		return row
	}
	e, err := st.TrafficDetail(ctx, runID, exchangeID)
	if err != nil {
		row.Status = store.RetestUnverifiable
		row.Expected = "原始交换可重放"
		row.Observed = "流量记录不存在（可能已随任务清理）"
		row.Error = err.Error()
		return row
	}
	row.Expected = fmt.Sprintf("%s %s → %d, sha=%s, len=%d", e.Method, e.URL, e.Status, short(e.RespSHA), e.RespLen)

	if scopeSet == nil || scopeSet.Empty() {
		row.Status = store.RetestUnverifiable
		row.Observed = "本任务当前没有授权范围，拒绝重放"
		return row
	}
	if ok, why := scopeSet.AllowsURL(e.URL); !ok {
		row.Status = store.RetestUnverifiable
		row.Observed = "超出当前授权范围，拒绝重放：" + why
		return row
	}
	if e.IsBinary {
		row.Status = store.RetestUnverifiable
		row.Observed = "响应是二进制，无法用指纹比对判定"
		return row
	}

	reqBody, err := replayBody(st, e)
	if err != nil {
		row.Status = store.RetestUnverifiable
		row.Observed = "读取原始请求体失败：" + err.Error()
		row.Error = err.Error()
		return row
	}

	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var bodyReader io.Reader
	if len(reqBody) > 0 {
		bodyReader = strings.NewReader(string(reqBody))
	}
	req, err := http.NewRequestWithContext(reqCtx, strings.ToUpper(e.Method), e.URL, bodyReader)
	if err != nil {
		row.Status = store.RetestUnverifiable
		row.Observed = "构造请求失败：" + err.Error()
		row.Error = err.Error()
		return row
	}
	for k, v := range e.ReqHeaders {
		// 跳过来回会变的头，避免复测被这些噪声干扰。
		switch strings.ToLower(k) {
		case "host", "content-length", "connection", "accept-encoding":
			continue
		}
		req.Header.Set(k, v)
	}

	start := time.Now()
	resp, err := httpClientFor(allowLoopback).Do(req)
	row.DurationMS = int(time.Since(start).Milliseconds())
	if err != nil {
		row.Status = store.RetestUnverifiable
		row.Observed = "重放失败（网络/目标不可达）：" + err.Error()
		row.Error = err.Error()
		return row
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, retestMaxBody+1))
	if err != nil {
		row.Status = store.RetestUnverifiable
		row.Observed = "读响应失败：" + err.Error()
		row.Error = err.Error()
		return row
	}
	sum := sha256.Sum256(body)
	sha := hex.EncodeToString(sum[:])

	switch {
	case resp.StatusCode != e.Status:
		row.Status = store.RetestFailed
		row.Observed = fmt.Sprintf("状态码变了：%d → %d（长度 %d → %d）",
			e.Status, resp.StatusCode, e.RespLen, len(body))
	case e.RespSHA != "" && sha == e.RespSHA:
		row.Status = store.RetestVerified
		row.Observed = fmt.Sprintf("%d，响应 sha 完全一致（%s）", resp.StatusCode, short(sha))
	case e.RespLen > 0 && withinRatio(len(body), e.RespLen, 0.10):
		row.Status = store.RetestVerified
		row.Observed = fmt.Sprintf("%d，长度 %d ≈ 原始 %d（±10%%），sha 不同（%s）",
			resp.StatusCode, len(body), e.RespLen, short(sha))
	default:
		row.Status = store.RetestFailed
		row.Observed = fmt.Sprintf("%d，响应明显不同：长度 %d vs 原始 %d，sha=%s",
			resp.StatusCode, len(body), e.RespLen, short(sha))
	}
	return row
}

// replayBody 优先读落盘的原始请求体（DB 里只存 8KB 预览），再退回预览。
func replayBody(st *store.Store, e *store.TrafficExchange) ([]byte, error) {
	if e.ReqPath == "" {
		return []byte(e.ReqBody), nil
	}
	// 路径是相对 dataDir 的；这里需要调用方的 DataDir —— 由 RetestOne 的调用方保证
	// 通过包级配置注入（见 SetTrafficDataDir）。没有配置时退回预览。
	dataDir := trafficDataDir()
	if dataDir == "" {
		return []byte(e.ReqBody), nil
	}
	abs := filepath.Join(dataDir, filepath.FromSlash(e.ReqPath))
	b, err := os.ReadFile(abs)
	if err != nil {
		return []byte(e.ReqBody), nil
	}
	return b, nil
}

var trafficDir string

// SetTrafficDataDir 让复测能读到落盘的原始请求体；由 cmd/engine 装配时调用。
func SetTrafficDataDir(dir string) { trafficDir = dir }

func trafficDataDir() string { return trafficDir }

func withinRatio(v, base int, ratio float64) bool {
	if base <= 0 {
		return false
	}
	diff := float64(v-base) / float64(base)
	if diff < 0 {
		diff = -diff
	}
	return diff <= ratio
}

func short(sha string) string {
	if len(sha) <= 12 {
		return sha
	}
	return sha[:12]
}
