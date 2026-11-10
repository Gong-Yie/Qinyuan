package agent

import (
	"context"
	"time"

	"qinyuan/internal/llm"
)

// 本文件是"一次模型调用"的统一入口（M4c）。
//
// 为什么需要它：provider 可能支持流式（llm.Streamer），也可能只支持整块（Complete）。
// 上层只关心"这一轮拿到什么"，但流式还能顺带给出**首字延迟（TTFT）** ——
// 那是排障时最有用的一个数（"卡了 40 秒"到底是连不上，还是模型在憋）。
//
// committed 语义由 provider 负责：它自己知道能不能重试。这里的 onFirstToken
// 只做观测，不参与控制。

// completeTurn 调一次模型。onFirstToken 在"第一次交付内容"时回调一次（可能不回调）。
func completeTurn(ctx context.Context, provider llm.Provider, req *llm.Request, onFirstToken func(time.Duration)) (*llm.Response, error) {
	start := time.Now()
	reported := false
	if s, ok := provider.(llm.Streamer); ok {
		return s.Stream(ctx, req, func(ev llm.StreamEvent) error {
			if !reported && ev.Committed {
				reported = true
				if onFirstToken != nil {
					onFirstToken(time.Since(start))
				}
			}
			return nil
		})
	}
	return provider.Complete(ctx, req)
}
