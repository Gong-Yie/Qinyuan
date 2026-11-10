# M4c · 流式输出与 committed 边界

> 依据：`实现规格/05 §1`（五层重试里的 L1–L3）、`学习.md 6.1`。
> 状态：**已落地并通过自动化验收**（`go test ./...` + `-race` 全绿）。前两阶段见 [`04-M4a`](04-M4a-撞预算不白跑.md) / [`05-M4b`](05-M4b-删除与准入.md)。

---

## 1. 为什么要有 committed 这个概念

非流式调用只有两种结果：整块成功、或者失败。流式不一样：**内容会一段一段交付**。
一旦你已经把"你好"给了用户、或者工具已经开始执行这个 `tool_call`，再"换一个 provider 重放一次"
就会：

- 用户看到两遍同样的内容；
- 同一次写库/写资产被执行两次（副作用重复）。

所以 streaming 的工程核心不是"逐字显示"，而是**一条不可回退的线**：

```
未 committed：可以重试、可以换人、可以丢弃重来（此时什么都没交付）
已 committed：只能继续或放弃；绝不重放
```

---

## 2. 接口

`internal/llm/stream.go`：

```go
type StreamEvent struct {
    Kind      string // text | thinking | done
    Text      string
    Committed bool   // 从这条事件起内容已交付
}

type Streamer interface {
    Stream(ctx, *Request, onEvent func(StreamEvent) error) (*Response, error)
}
```

- **可选能力**：不实现 `Streamer` 的 provider 继续走 `Complete`，上层行为不变；
- **谁负责重试**：provider 自己（`streamOnce` 内部看到 `committed` 就不再循环重试）；
- **失败时给什么**：已 committed 的失败返回 **部分结果 + `ErrStreamInterrupted`** ——
  调用方据此把它当"这次 run 的部分结果"，而不是把整条意图重放。

---

## 3. wire 解析（OpenAI Chat Completions SSE）

```
data: {"choices":[{"delta":{"content":"你好"}}]}
data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"http_request","arguments":"{\"url\":"}}]}}]}
data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"http://x/\"}"}}]}}]}
data: {"choices":[],"usage":{"prompt_tokens":10,...}}
data: [DONE]
```

四个必须处理对的点：

| 点 | 为什么 |
|---|---|
| `delta.content` 是**增量** | 拼接，不能覆盖 |
| `delta.tool_calls[].arguments` 是**字符串分片** | 按 `index` 追加；整块覆盖会得到半个 JSON |
| `usage` 只在 `stream_options.include_usage=true` 时出现 | 请求里显式带上；缺了就记 0（不影响正确性） |
| 单行坏 JSON / 注释行 | 跳过，不打断整个流（网关偶尔插脏数据） |

`reasoning_content`（DeepSeek 的思考增量）也一并合并进 `Response.Thinking`。

---

## 4. 重试矩阵（这一节是重点）

| 情况 | 能否重试 | 说明 |
|---|---|---|
| 非 200（401/403/404） | 否 | 换人也没用 |
| 429 + 余额关键词 | 否 | 立刻失败（上层换 profile 的语义在 M5 做） |
| 429 普通限流 / 5xx / 网络错误，**未 committed** | 是 | 指数退避，最多 `MaxRetries` |
| 流里**一个事件都没有** | 是 | L2 空响应重试 |
| **已 committed 后断流** | 否 | 返回 `ErrStreamInterrupted` + 部分结果 |
| 上游 `error` 字段 | 否（已 committed）/ 是（未 committed） | 走 `classifyMessage` |

---

## 5. worker 侧：一次调用 + TTFT

`agent/stream.go:completeTurn` 是唯一入口：

```go
if s, ok := provider.(llm.Streamer); ok {
    return s.Stream(ctx, req, onEvent)   // onEvent 记"首字延迟"
}
return provider.Complete(ctx, req)       // 非流式 provider：行为不变
```

worker 拿到两样东西：

1. **TTFT**：第一条 `Committed` 事件的时间差，写成一条 system activity + result 的 `metadata.ttft_ms`
   （排障时最有用："卡了 40 秒"是连不上，还是模型在憋）；
2. **中断时的部分结果**：已 committed 后断流 → `lastText` 保留，活动里写明
   「流式输出中途断开（已提交，不重放；保留已收到的部分结果）」，意图落 `blocked`。

planner / goals 仍然走整块调用：它们的回合短（6/8 轮），流式收益低，先不扩散改动面。

---

## 6. 验收（自动化）

| 用例 | 断言 |
|---|---|
| `llm.TestOpenAIStreamMergesDeltas` | 正文/thinking 拼接；`tool_calls` 按 index 合并、arguments 分片拼成合法 JSON；usage 取到；首条增量即 `Committed` |
| `llm.TestOpenAIStreamInterruptedAfterCommitDoesNotRetry` | 断流后返回 `ErrStreamInterrupted` + 部分文本；**请求数 = 1**（没重试） |
| `llm.TestOpenAIStreamRetriesBeforeCommit` | 第一次空流（未 committed）→ 自动重试，第二次成功；请求数 = 2 |
| `llm.TestOpenAIStreamQuotaNoRetry` | 429 余额 → `ErrQuota`，不重试；错误里不含凭据 |
| `agent.TestCompleteTurnUsesStreamerAndReportsTTFT` | 支持流式时走 Stream；TTFT 回调恰好一次且在 delay 之后 |
| `agent.TestCompleteTurnFallsBackToComplete` | 非流式 provider 退回 Complete，不报 TTFT |

---

## 7. 本轮没做

1. **增量 activity 行**：现在一轮只写一条合并后的 text/thinking（M5 的 SSE 前端需要逐字时再拆）；
2. **按角色换 provider 的 failover**（P1-4）：`llm_profiles` 那一套还没落在钦原；
3. **流式下的 settlement 交互**：收尾轮复用同一个 provider，如果收尾轮也被中断，按同样规则处理（未单独测试）；
4. **Anthropic / openai-responses 的流式**：只做了 `chat/completions` 这一种 wire。

---

## 8. 坑

1. **`arguments` 是分片**（§3）：整块覆盖会得到半个 JSON，工具层只会报"参数解析失败"，很难查。
2. **`stream_options.include_usage` 必须显式要**：否则流式响应里没有 token 账，记账全是 0。
3. **工具调用的第一片也算 committed**：之后不能换人，否则同一次调用会被执行两次。
4. **不要在 committed 之后重试**：这是本阶段唯一真正的"正确性"约束，其他都是体验。
5. **单行坏 JSON 不要中断**：网关插一行心跳/脏数据是常态，跳过即可。
