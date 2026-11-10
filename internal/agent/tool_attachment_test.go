package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"qinyuan/internal/model"
	"qinyuan/internal/testkit"
)

// M12：附件工具是"只读 + 按名读"，路径永远由服务端决定。
func TestAttachmentTools(t *testing.T) {
	st := testkit.Store(t)
	ctx := context.Background()
	run, _ := testkit.Run(t, st, nil, nil)
	dataDir := t.TempDir()

	writeAtt := func(name, kind string, content []byte) *model.Attachment {
		t.Helper()
		dir := filepath.Join(dataDir, "runs", fmt.Sprint(run.ID), "attachments")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		full := filepath.Join(dir, name)
		if err := os.WriteFile(full, content, 0o644); err != nil {
			t.Fatal(err)
		}
		rel := filepath.Join("runs", fmt.Sprint(run.ID), "attachments", name)
		att, err := st.AddAttachment(ctx, &model.Attachment{
			RunID: run.ID, Name: name, Kind: kind, Mime: "text/plain",
			Size: int64(len(content)), SHA256: "sha-" + name, Path: rel, Summary: "摘要-" + name,
		})
		if err != nil {
			t.Fatal(err)
		}
		return att
	}

	text := []byte("第一行：题目说明\nsecond line\n" + strings.Repeat("x", 300))
	writeAtt("notes.txt", "text", text)
	writeAtt("chall.bin", "binary", []byte{0x7f, 0x45, 0x4c, 0x46, 0x02, 0x01})

	rc := &Context{Store: st, RunID: run.ID, DataDir: dataDir, MaxOutput: 30000}

	// list_attachments
	res, err := NewListAttachmentsTool().Handle(ctx, rc, json.RawMessage(`{}`))
	if err != nil || res.IsError {
		t.Fatalf("list 失败：%+v %v", res, err)
	}
	var listed struct {
		Items []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
			Kind string `json:"kind"`
		} `json:"items"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal([]byte(res.Text), &listed); err != nil {
		t.Fatalf("list 返回不是 JSON：%v", err)
	}
	if listed.Total != 2 || len(listed.Items) != 2 {
		t.Fatalf("列表数量不对：%+v", listed)
	}

	// read by name
	res, err = NewReadAttachmentTool().Handle(ctx, rc, json.RawMessage(`{"name":"notes.txt"}`))
	if err != nil || res.IsError {
		t.Fatalf("read 失败：%+v %v", res, err)
	}
	if !strings.Contains(res.Text, "第一行：题目说明") || !strings.Contains(res.Text, "不可信输入") {
		t.Fatalf("读文本结果不对：%.200s", res.Text)
	}

	// offset 续读：读最后 10 字节 → 不该再有"继续读"的尾巴
	res, _ = NewReadAttachmentTool().Handle(ctx, rc, json.RawMessage(
		fmt.Sprintf(`{"name":"notes.txt","offset":%d,"limit":10}`, len(text)-10)))
	if res.IsError {
		t.Fatalf("分段读失败：%s", res.Text)
	}
	if strings.Contains(res.Text, "继续读用 offset") {
		t.Fatalf("读到末尾不该再提示续读：%s", res.Text)
	}

	// 二进制 → hexdump
	res, _ = NewReadAttachmentTool().Handle(ctx, rc, json.RawMessage(`{"name":"chall.bin"}`))
	if res.IsError || !strings.Contains(res.Text, "hexdump") || !strings.Contains(res.Text, "7f 45 4c 46") {
		t.Fatalf("二进制应给 hexdump：%.300s", res.Text)
	}

	// 不存在的名字 / 不存在的 id / 空参数 → 都是可读错误
	for _, raw := range []string{`{"name":"nope.txt"}`, `{"id":999999}`, `{}`} {
		res, err := NewReadAttachmentTool().Handle(ctx, rc, json.RawMessage(raw))
		if err != nil {
			t.Fatalf("不该返回 Go error：%v", err)
		}
		if !res.IsError {
			t.Fatalf("%s 应当报错：%s", raw, res.Text)
		}
	}

	// 纵深防御：库里若被写入逃出数据目录的路径，也必须拒绝。
	bad, err := st.AddAttachment(ctx, &model.Attachment{
		RunID: run.ID, Name: "escape.txt", Kind: "text", Size: 3,
		SHA256: "x", Path: "../../etc/passwd",
	})
	if err != nil {
		t.Fatal(err)
	}
	res, _ = NewReadAttachmentTool().Handle(ctx, rc, json.RawMessage(fmt.Sprintf(`{"id":%d}`, bad.ID)))
	if !res.IsError || !strings.Contains(res.Text, "路径异常") {
		t.Fatalf("逃逸路径必须拒绝：%s", res.Text)
	}
}
