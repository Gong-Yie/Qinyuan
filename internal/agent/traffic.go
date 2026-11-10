package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"qinyuan/internal/store"
)

// 流量录制：在**唯一出网点**（http_request）录，不做 MITM。
//
// 为什么不做代理（docs/13）：worker 的工具面里没有任意命令执行，所有目标流量都由
// 本进程发出。在工具里录能拿到 intent / tool_use 归属，也不需要 CA 与代理端口。

// trafficInlineMax 是内联进 DB 的正文上限（可被检索）；更长的正文落内容寻址文件。
const trafficInlineMax = 8 << 10

// trafficRecord 是一次交换的写入参数（工具层填好交给 store）。
type trafficRecord struct {
	Method      string
	URL         string
	Status      int
	ContentType string
	ReqHeaders  map[string]string
	RespHeaders map[string]string
	ReqBody     []byte
	RespBody    []byte
	DurationMS  int
	Err         string
}

// recordTraffic 写一条交换；失败只返回 0（调用方不该因为"记不上账"而失败）。
func recordTraffic(ctx context.Context, rc *Context, rec trafficRecord) int64 {
	if rc == nil || rc.Store == nil {
		return 0
	}
	reqSHA, reqPath := persistTrafficBody(rc, "req", rec.ReqBody)
	respSHA, respPath := persistTrafficBody(rc, "resp", rec.RespBody)
	binary := looksBinary(rec.ContentType, rec.RespBody)

	in := store.TrafficInput{
		RunID: rc.RunID, IntentID: rc.IntentID, ToolUseID: rc.ToolUseID,
		Method: rec.Method, URL: rec.URL, Status: rec.Status, ContentType: rec.ContentType,
		ReqLen: len(rec.ReqBody), RespLen: len(rec.RespBody), DurationMS: rec.DurationMS,
		ReqHeaders: rec.ReqHeaders, RespHeaders: rec.RespHeaders,
		ReqBody: previewText(rec.ReqBody, binary), RespBody: previewText(rec.RespBody, binary),
		ReqSHA: reqSHA, RespSHA: respSHA, ReqPath: reqPath, RespPath: respPath,
		IsBinary: binary, Truncated: len(rec.RespBody) > trafficInlineMax, Error: rec.Err,
	}
	id, err := rc.Store.RecordTraffic(ctx, in)
	if err != nil {
		// 录不上不影响本次调用：证据链可以在 report_finding 里显式说明缺失。
		return 0
	}
	return id
}

// persistTrafficBody 内容寻址落盘（超阈值才写），返回 sha 与相对 dataDir 的路径。
func persistTrafficBody(rc *Context, kind string, body []byte) (sha, rel string) {
	if len(body) == 0 {
		return "", ""
	}
	sum := sha256.Sum256(body)
	sha = hex.EncodeToString(sum[:])
	if len(body) <= trafficInlineMax {
		return sha, ""
	}
	rel = filepath.ToSlash(filepath.Join("runs", itoaInt(rc.RunID), "traffic", sha[:2], sha))
	abs := filepath.Join(rc.DataDir, filepath.FromSlash(rel))
	if _, err := os.Stat(abs); err == nil {
		return sha, rel // 内容寻址：同样内容已经写过
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return sha, ""
	}
	if err := os.WriteFile(abs, body, 0o644); err != nil {
		return sha, ""
	}
	return sha, rel
}

// previewText 把正文变成可检索的预览（二进制只留一句描述）。
func previewText(body []byte, binary bool) string {
	if len(body) == 0 {
		return ""
	}
	if binary {
		return fmt.Sprintf("[binary %d bytes]", len(body))
	}
	if len(body) > trafficInlineMax {
		body = body[:trafficInlineMax]
	}
	if utf8.Valid(body) {
		return string(body)
	}
	// 非法 UTF-8：退化成替换字符，保证 DB 写入不炸。
	return strings.ToValidUTF8(string(body), "\uFFFD")
}

func looksBinary(contentType string, body []byte) bool {
	ct := strings.ToLower(contentType)
	for _, p := range []string{"image/", "video/", "audio/", "application/octet-stream",
		"application/zip", "application/pdf", "application/x-protobuf", "font/"} {
		if strings.HasPrefix(ct, p) {
			return true
		}
	}
	probe := body
	if len(probe) > 512 {
		probe = probe[:512]
	}
	for _, b := range probe {
		if b == 0 {
			return true
		}
	}
	return false
}

func itoaInt(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

var _ = time.Now
