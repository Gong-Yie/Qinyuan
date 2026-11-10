// 演示用的"假 OpenAI 兼容网关"：只实现 chat/completions，支持流式与非流式。
//
// 它不是产品代码，存在的目的只有一个：让 serve 走**真实的 OpenAI 适配器**
// （包括 M4c 的 SSE 解析与 committed 逻辑），从而看到真效果。
//
// 行为（按请求里有哪些工具判断会话角色）：
//
//	有 set_goals  → 第一次给 set_goals 工具调用（拆出一条目标），之后给一句文本
//	有 record_fact → 第 1 次给 record_fact（事实 A），第 2 次给 record_fact（事实 B），之后给文本
//	其它（planner）→ 给一句文本
//
// 跑法：go run scripts/demo/fakegw.go -addr 127.0.0.1:18080
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"time"
)

type msg struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []any           `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
	Name       string          `json:"name"`
}

type reqBody struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []msg  `json:"messages"`
	Tools    []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
}

func textOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

func (b reqBody) hasTool(name string) bool {
	for _, t := range b.Tools {
		if t.Function.Name == name {
			return true
		}
	}
	return false
}

func (b reqBody) countTool(name string) int {
	n := 0
	for _, m := range b.Messages {
		if m.Role == "tool" && m.Name == name {
			n++
		}
	}
	return n
}

// toolCall 是本次要返回的一个工具调用。
type toolCall struct {
	ID   string
	Name string
	Args string
}

type reply struct {
	Text  string
	Calls []toolCall
	Why   string
}

var intentIDRe = regexp.MustCompile(`意图 id:\s*(\d+)`)

func decide(b reqBody) reply {
	// 从 system/user 里找"意图 id: N"，让 record_fact 能挂到真实意图上。
	intentID := int64(0)
	for _, m := range b.Messages {
		if m.Role != "system" && m.Role != "user" {
			continue
		}
		if id := intentIDRe.FindStringSubmatch(textOf(m.Content)); len(id) == 2 {
			if v, err := strconv.ParseInt(id[1], 10, 64); err == nil {
				intentID = v
			}
			break
		}
	}

	switch {
	case b.hasTool("set_goals"):
		if b.countTool("set_goals") > 0 {
			return reply{Text: "拆解完成：1 条目标、0 条约束。", Why: "goals.done"}
		}
		args, _ := json.Marshal(map[string]any{
			"goals": []map[string]any{{"text": "确认演示站点的登录入口存在且可访问"}},
		})
		return reply{
			Calls: []toolCall{{ID: "goal_1", Name: "set_goals", Args: string(args)}},
			Why:   "goals.set",
		}

	case b.hasTool("record_fact"):
		n := b.countTool("record_fact")
		fact := func(summary, evidence string) reply {
			args, _ := json.Marshal(map[string]any{
				"facts": []map[string]any{{
					"summary": summary, "evidence": evidence, "confidence": "observed",
				}},
				"intent_id": intentID,
			})
			return reply{
				Calls: []toolCall{{ID: fmt.Sprintf("fact_%d", n), Name: "record_fact", Args: string(args)}},
				Why:   fmt.Sprintf("worker.fact%d", n+1),
			}
		}
		switch n {
		case 0:
			return fact("首页返回 200，Server: demo-gw，存在登录表单",
				"curl -sI http://demo.local/ → HTTP/1.1 200")
		case 1:
			// 这一条通常来自"收尾轮"（MaxTurns=1 时主循环只够写第一条）。
			return fact("登录表单提交到 /login，未见 CSRF token",
				"GET /login → <form action=/login> 无 csrf 隐藏域")
		default:
			return reply{Text: "已完成：写回两条事实（一条主循环、一条收尾轮）。", Why: "worker.done"}
		}

	default:
		return reply{Text: "等 worker 产出（planner 本轮不派新方向）。", Why: "planner.text"}
	}
}

func usageJSON(input, output int) map[string]any {
	return map[string]any{
		"prompt_tokens":     input,
		"completion_tokens": output,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": 0,
		},
	}
}

func writeNonStream(w http.ResponseWriter, b reqBody, r reply) {
	message := map[string]any{"role": "assistant", "content": r.Text}
	finish := "stop"
	if len(r.Calls) > 0 {
		finish = "tool_calls"
		calls := make([]map[string]any, 0, len(r.Calls))
		for _, c := range r.Calls {
			calls = append(calls, map[string]any{
				"id": c.ID, "type": "function",
				"function": map[string]any{"name": c.Name, "arguments": c.Args},
			})
		}
		message["tool_calls"] = calls
		message["content"] = nil
	}
	body := map[string]any{
		"choices": []map[string]any{{
			"message": message, "finish_reason": finish,
		}},
		"usage": usageJSON(120, 30),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func writeStream(w http.ResponseWriter, r reply) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	// 先停一下再吐第一个分片：模拟"模型在想"，让 TTFT 有可读的量级。
	time.Sleep(150 * time.Millisecond)
	send := func(v any) {
		raw, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", raw)
		if fl != nil {
			fl.Flush()
		}
		time.Sleep(15 * time.Millisecond) // 让"首字延迟"看得见
	}

	if len(r.Calls) > 0 {
		// 工具调用：按 index 给"开头 + 参数"两片，正好验证合并逻辑。
		c := r.Calls[0]
		send(map[string]any{"choices": []map[string]any{{
			"delta": map[string]any{"tool_calls": []map[string]any{{
				"index": 0, "id": c.ID, "type": "function",
				"function": map[string]any{"name": c.Name},
			}}},
		}}})
		half := len(c.Args) / 2
		send(map[string]any{"choices": []map[string]any{{
			"delta": map[string]any{"tool_calls": []map[string]any{{
				"index": 0, "function": map[string]any{"arguments": c.Args[:half]},
			}}},
		}}})
		send(map[string]any{"choices": []map[string]any{{
			"delta": map[string]any{"tool_calls": []map[string]any{{
				"index": 0, "function": map[string]any{"arguments": c.Args[half:]},
			}}},
		}}})
		send(map[string]any{"choices": []map[string]any{{"delta": map[string]any{}, "finish_reason": "tool_calls"}}})
	} else {
		for _, part := range splitRunes(r.Text, 12) {
			send(map[string]any{"choices": []map[string]any{{"delta": map[string]any{"content": part}}}})
		}
		send(map[string]any{"choices": []map[string]any{{"delta": map[string]any{}, "finish_reason": "stop"}}})
	}
	send(map[string]any{"choices": []any{}, "usage": usageJSON(120, 30)})
	fmt.Fprint(w, "data: [DONE]\n\n")
	if fl != nil {
		fl.Flush()
	}
}

func splitRunes(s string, n int) []string {
	r := []rune(s)
	var out []string
	for i := 0; i < len(r); i += n {
		end := i + n
		if end > len(r) {
			end = len(r)
		}
		out = append(out, string(r[i:end]))
	}
	return out
}

func main() {
	addr := flag.String("addr", "127.0.0.1:18080", "监听地址")
	flag.Parse()

	http.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var b reqBody
		if err := json.NewDecoder(req.Body).Decode(&b); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		r := decide(b)
		log.Printf("chat/completions stream=%v why=%s tools=%d", b.Stream, r.Why, len(b.Tools))
		if b.Stream {
			writeStream(w, r)
			return
		}
		writeNonStream(w, b, r)
	})
	http.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	log.Printf("fake gateway on http://%s/v1 （模型随便填，例：demo）", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}
