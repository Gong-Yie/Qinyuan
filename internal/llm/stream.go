package llm

import (
	"context"
	"errors"
)

// 本文件是"流式输出"的接口层（M4c）。
//
// 为什么要有 committed 这个概念：
// 一次模型调用在"第一次把内容交付给调用方"之前是可以随便重试/换人的；
// 一旦交付（用户已经看到字、或者工具已经开始调），**再重放就会重复输出、重复副作用**。
// 所以 provider 自己要知道"现在还能不能重试"，而不是让上层猜。
//
// 约定：
//   - Stream 只在**未 committed** 时自己重试；
//   - 一旦调用过 onEvent（哪怕只有一个字符），就不能换人重放；
//   - 已经 committed 之后失败，返回**部分结果 + error**，由上层决定怎么收场。

// StreamEvent 是一条增量事件。
type StreamEvent struct {
	// Kind：text | thinking | committed | done
	Kind string
	Text string
	// Committed=true 表示"从这条事件起，内容已经交付给调用方，不可重放"。
	Committed bool
}

// 增量事件的 kind（字符串而不是枚举，方便日志直读）。
const (
	StreamText     = "text"
	StreamThinking = "thinking"
	StreamDone     = "done"
)

// Streamer 是可选能力：支持增量输出的 provider 实现它。
//
// 上层用它拿到"首字时间（TTFT）"和"中断时的部分结果"；不实现它的 provider
// 继续走 Complete，行为不变。
type Streamer interface {
	Stream(ctx context.Context, req *Request, onEvent func(StreamEvent) error) (*Response, error)
}

// ErrStreamInterrupted 表示流在中途断了（已提交 → 不可重放）。
var ErrStreamInterrupted = errors.New("llm: stream interrupted after commit")
