package agent

import (
	"context"
	"testing"
	"time"

	"qinyuan/internal/llm"
)

// 本文件钉住 M4c 的"一次模型调用"入口：有流式就用流式并测 TTFT，没有就退回整块。

type fakeStreamProvider struct {
	resp     llm.Response
	delay    time.Duration
	streamed bool
}

func (f *fakeStreamProvider) Complete(context.Context, *llm.Request) (*llm.Response, error) {
	return &f.resp, nil
}

func (f *fakeStreamProvider) Stream(ctx context.Context, _ *llm.Request, onEvent func(llm.StreamEvent) error) (*llm.Response, error) {
	f.streamed = true
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(f.delay):
	}
	if onEvent != nil {
		_ = onEvent(llm.StreamEvent{Kind: llm.StreamText, Text: "x", Committed: true})
	}
	return &f.resp, nil
}

func TestCompleteTurnUsesStreamerAndReportsTTFT(t *testing.T) {
	f := &fakeStreamProvider{resp: llm.Response{Text: "流式结果"}, delay: 30 * time.Millisecond}
	var ttft time.Duration
	var calls int

	resp, err := completeTurn(context.Background(), f, &llm.Request{}, func(d time.Duration) {
		calls++
		ttft = d
	})
	if err != nil {
		t.Fatalf("调用失败：%v", err)
	}
	if !f.streamed {
		t.Fatalf("provider 支持流式时必须走 Stream")
	}
	if resp.Text != "流式结果" {
		t.Fatalf("响应不对：%+v", resp)
	}
	if calls != 1 {
		t.Fatalf("TTFT 回调应当只来一次，实际 %d", calls)
	}
	if ttft < 20*time.Millisecond {
		t.Fatalf("TTFT 应当在 delay 之后（%v）", ttft)
	}
}

func TestCompleteTurnFallsBackToComplete(t *testing.T) {
	// 假 provider 只有 Complete（非流式）——TTFT 回调不该触发。
	prov := llm.NewFakeFactory(llm.Step{Text: "整块结果"}).For("test")
	called := false
	resp, err := completeTurn(context.Background(), prov, &llm.Request{}, func(time.Duration) { called = true })
	if err != nil {
		t.Fatalf("调用失败：%v", err)
	}
	if resp.Text != "整块结果" {
		t.Fatalf("响应不对：%+v", resp)
	}
	if called {
		t.Fatalf("非流式 provider 不该报 TTFT")
	}
}
