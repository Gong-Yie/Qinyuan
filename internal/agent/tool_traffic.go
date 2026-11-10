package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"qinyuan/internal/store"
)

// 流量工具：先查已录流量，再决定要不要重复请求。
//
// 这是"别重复 curl 同一 URL"从口号变成可执行动作的前提：worker 能看见
// **自己/队友** 已经打过什么（带 intent 归属），而不是靠 prompt 自觉。

// NewTrafficSearchTool 按 host/method/status/正文关键词检索已录流量。
func NewTrafficSearchTool() *Tool {
	return &Tool{
		Name:     "traffic_search",
		ReadOnly: true,
		Description: `检索本任务已录的流量（按 id 倒序）。发新请求之前先用它看一眼：同一个 URL/参数
是否已经打过、返回过什么。host 支持子串（如 8080 或 /login）；q 是正文子串（响应或请求体）。
只返回摘要（方法/URL/状态/长度/时间/正文前 400 字），要看原文用 traffic_get。`,
		Schema: toolSpecJSON(`{
  "type": "object",
  "properties": {
    "host": {"type": "string", "description": "URL 子串（主机、端口或路径片段）"},
    "method": {"type": "string", "description": "HTTP 方法（可选）"},
    "status": {"type": "integer", "description": "状态码（可选）"},
    "q": {"type": "string", "description": "正文子串（响应体或请求体）"},
    "limit": {"type": "integer", "minimum": 1, "maximum": 20, "description": "返回条数，默认 5"}
  }
}`),
		Handle: func(ctx context.Context, rc *Context, raw json.RawMessage) (Result, error) {
			var in struct {
				Host   string `json:"host"`
				Method string `json:"method"`
				Status int    `json:"status"`
				Q      string `json:"q"`
				Limit  int    `json:"limit"`
			}
			if err := json.Unmarshal(raw, &in); err != nil {
				return Errorf("参数解析失败：%v", err), nil
			}
			rows, err := rc.Store.SearchTraffic(ctx, rc.RunID, store.TrafficQuery{
				Host: in.Host, Method: in.Method, Status: in.Status, Q: in.Q, Limit: in.Limit,
			})
			if err != nil {
				return Errorf("检索失败：%v", err), nil
			}
			items := make([]map[string]any, 0, len(rows))
			for _, e := range rows {
				items = append(items, map[string]any{
					"id": e.ID, "method": e.Method, "url": e.URL, "status": e.Status,
					"resp_len": e.RespLen, "content_type": e.ContentType,
					"duration_ms": e.DurationMS, "ts": e.TS,
					"intent_id": e.IntentID, "binary": e.IsBinary, "error": e.Error,
					"preview": e.Preview,
				})
			}
			return JSONResult(map[string]any{"items": items, "count": len(items)}), nil
		},
	}
}

// NewTrafficGetTool 取一条交换的原文（正文按 offset/length 分页，超阈值提示落盘路径）。
func NewTrafficGetTool() *Tool {
	return &Tool{
		Name:     "traffic_get",
		ReadOnly: true,
		Description: `按 id 取一条已录交换的完整记录：请求（方法/URL/头/体）+ 响应（状态/头/体）。
正文默认回 8KB；被截断时会给落盘路径（Read 工具可读全文）与 offset 提示。`,
		Schema: toolSpecJSON(`{
  "type": "object",
  "properties": {
    "id": {"type": "integer", "description": "traffic_search 返回的 id"},
    "offset": {"type": "integer", "minimum": 0, "description": "正文起始字节（默认 0）"},
    "length": {"type": "integer", "minimum": 1, "maximum": 32000, "description": "读多少字节（默认 8192）"}
  },
  "required": ["id"]
}`),
		Handle: func(ctx context.Context, rc *Context, raw json.RawMessage) (Result, error) {
			var in struct {
				ID     int64 `json:"id"`
				Offset int   `json:"offset"`
				Length int   `json:"length"`
			}
			if err := json.Unmarshal(raw, &in); err != nil {
				return Errorf("参数解析失败：%v", err), nil
			}
			if in.ID <= 0 {
				return Errorf("id 必填"), nil
			}
			if in.Length <= 0 {
				in.Length = 8192
			}
			e, err := rc.Store.TrafficDetail(ctx, rc.RunID, in.ID)
			if err != nil {
				return Errorf("流量 #%d 不存在于本任务", in.ID), nil
			}
			out := map[string]any{
				"id": e.ID, "intent_id": e.IntentID, "method": e.Method, "url": e.URL,
				"status": e.Status, "content_type": e.ContentType,
				"req_len": e.ReqLen, "resp_len": e.RespLen, "duration_ms": e.DurationMS,
				"req_headers": e.ReqHeaders, "resp_headers": e.RespHeaders,
				"req_sha": e.ReqSHA, "resp_sha": e.RespSHA,
				"binary": e.IsBinary, "truncated": e.Truncated, "error": e.Error,
				"req_body": e.ReqBody, "resp_body": e.RespBody,
			}
			// 正文落盘时给分页能力：offset/length 直接从文件读，避免把全文灌进上下文。
			if rel := pickPath(e.RespPath); rel != "" || pickPath(e.ReqPath) != "" {
				if e.RespPath != "" {
					if chunk, total, perr := readBodyChunk(rc, e.RespPath, in.Offset, in.Length); perr == nil {
						out["resp_body_file"] = e.RespPath
						out["resp_body_total"] = total
						out["resp_body_at_offset"] = chunk
						if in.Offset+len(chunk) < total {
							out["next_offset"] = in.Offset + len(chunk)
						}
					}
				}
				if e.ReqPath != "" {
					if chunk, total, perr := readBodyChunk(rc, e.ReqPath, in.Offset, in.Length); perr == nil {
						out["req_body_file"] = e.ReqPath
						out["req_body_total"] = total
						out["req_body_at_offset"] = chunk
					}
				}
			}
			return JSONResult(out), nil
		},
	}
}

func pickPath(p string) string { return strings.TrimSpace(p) }

// readBodyChunk 按偏移读落盘正文；返回片段、总字节数与错误。
func readBodyChunk(rc *Context, rel string, offset, length int) (string, int, error) {
	abs := filepath.Join(rc.DataDir, filepath.FromSlash(rel))
	f, err := os.Open(abs)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	total := int(st.Size())
	if offset >= total {
		return "", total, nil
	}
	if length <= 0 {
		length = 8192
	}
	if offset+length > total {
		length = total - offset
	}
	buf := make([]byte, length)
	n, err := f.ReadAt(buf, int64(offset))
	if n == 0 && err != nil {
		return "", total, err
	}
	return fmt.Sprintf("%s", buf[:n]), total, nil
}
