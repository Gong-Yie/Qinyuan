package sandbox

import (
	"bytes"
	"io"
	"os"
)

// limitedBuffer 是“头 + 落盘”的流式缓冲：
//   - 前 limit 字节留在内存里给模型（head）；
//   - 完整字节写进 path（超过 spillLimit 后停止写，并置 spillTruncated）；
//   - 无论是否落盘，都把管道读干，避免把 docker 的子进程堵死。
//
// 这是 ARTEX Capture 的钦原版本：截断必须带指针，不能只给一个 truncated=true。
type limitedBuffer struct {
	buf   bytes.Buffer
	limit int
	total int64

	path       string
	file       *os.File
	spillLimit int64
	written    int64
	spillTrunc bool
	spillErr   string
}

func newLimitedBuffer(limit int, path string, spillLimit int64) *limitedBuffer {
	b := &limitedBuffer{limit: limit, path: path, spillLimit: spillLimit}
	if b.spillLimit <= 0 {
		b.spillLimit = 50 << 20
	}
	if path != "" {
		f, err := os.Create(path)
		if err != nil {
			b.spillErr = err.Error()
			b.path = ""
		} else {
			b.file = f
		}
	}
	return b
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.total += int64(len(p))

	if b.limit > 0 && b.buf.Len() < b.limit {
		room := b.limit - b.buf.Len()
		if room > len(p) {
			room = len(p)
		}
		if room > 0 {
			b.buf.Write(p[:room])
		}
	}

	if b.file != nil {
		room := b.spillLimit - b.written
		if room > 0 {
			n := int64(len(p))
			if n > room {
				n = room
			}
			if _, err := b.file.Write(p[:n]); err != nil {
				b.spillErr = err.Error()
				_ = b.file.Close()
				b.file = nil
				b.path = ""
			} else {
				b.written += n
			}
		}
		if int64(len(p)) > room && room >= 0 {
			b.spillTrunc = true
		}
	}
	return len(p), nil
}

func (b *limitedBuffer) Close() {
	if b.file == nil {
		return
	}
	if err := b.file.Close(); err != nil && b.spillErr == "" {
		b.spillErr = err.Error()
	}
	b.file = nil
	if b.total == 0 && b.path != "" {
		_ = os.Remove(b.path)
		b.path = ""
	}
}

func (b *limitedBuffer) String() string { return b.buf.String() }

func (b *limitedBuffer) Total() int64 { return b.total }

func (b *limitedBuffer) Truncated() bool {
	return b.limit > 0 && b.total > int64(b.limit)
}

func (b *limitedBuffer) SpillTruncated() bool { return b.spillTrunc }

func (b *limitedBuffer) SpillError() string { return b.spillErr }

// Path 返回落盘路径；只有真的创建/保留文件时才非空。
func (b *limitedBuffer) Path() string { return b.path }

var _ io.Writer = (*limitedBuffer)(nil)
