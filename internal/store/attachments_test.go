package store

import (
	"context"
	"testing"

	"qinyuan/internal/model"
)

// M12：附件元数据的基本语义（同名覆盖、按名/按 id 读、跨任务不可见、删除时判断是否还有引用）。
func TestAttachmentsBasic(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	run := newTestRun(t, s)
	other := newTestRun(t, s)

	a, err := s.AddAttachment(ctx, &model.Attachment{
		RunID: run.ID, Name: "chall.zip", Kind: "archive", Mime: "application/zip",
		Size: 1234, SHA256: "aa", Path: "runs/1/attachments/aa.zip", Summary: "题目压缩包",
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == 0 || a.CreatedAt.IsZero() {
		t.Fatalf("返回值缺字段：%+v", a)
	}

	// 同名再传 = 覆盖（同一行），不是加一份。
	a2, err := s.AddAttachment(ctx, &model.Attachment{
		RunID: run.ID, Name: "chall.zip", Kind: "archive", Mime: "application/zip",
		Size: 999, SHA256: "bb", Path: "runs/1/attachments/bb.zip", Summary: "更新后的包",
	})
	if err != nil {
		t.Fatal(err)
	}
	if a2.ID != a.ID {
		t.Fatalf("同名应覆盖同一行：old=%d new=%d", a.ID, a2.ID)
	}
	list, err := s.Attachments(ctx, run.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("列表应有 1 条：%v %+v", err, list)
	}
	if list[0].Summary != "更新后的包" || list[0].SHA256 != "bb" {
		t.Fatalf("覆盖没生效：%+v", list[0])
	}

	// 按名 / 按 id 都能读；跨任务一律不存在。
	if got, err := s.AttachmentByName(ctx, run.ID, "chall.zip"); err != nil || got.ID != a.ID {
		t.Fatalf("按名读取失败：%v %+v", err, got)
	}
	if got, err := s.AttachmentByID(ctx, run.ID, a.ID); err != nil || got.Name != "chall.zip" {
		t.Fatalf("按 id 读取失败：%v %+v", err, got)
	}
	if _, err := s.AttachmentByID(ctx, other.ID, a.ID); err == nil {
		t.Fatal("跨任务读附件必须报不存在")
	}

	// 两个展示名指向同一份内容时，删掉一个不能删文件。
	if _, err := s.AddAttachment(ctx, &model.Attachment{
		RunID: run.ID, Name: "copy.zip", Kind: "archive", Size: 999,
		SHA256: "bb", Path: "runs/1/attachments/bb.zip", Summary: "同一份内容",
	}); err != nil {
		t.Fatal(err)
	}
	if path, ok, err := s.DeleteAttachment(ctx, run.ID, a.ID); err != nil || !ok || path != "" {
		t.Fatalf("还有引用时不该给出待删路径：path=%q ok=%v err=%v", path, ok, err)
	}
	// 删最后一条 → 返回路径（调用方删文件）。
	var lastID int64
	for _, x := range mustAttachments(t, s, run.ID) {
		lastID = x.ID
	}
	if path, ok, err := s.DeleteAttachment(ctx, run.ID, lastID); err != nil || !ok || path == "" {
		t.Fatalf("最后一条应返回待删路径：path=%q ok=%v err=%v", path, ok, err)
	}
	if list, _ := s.Attachments(ctx, run.ID); len(list) != 0 {
		t.Fatalf("删完应为空：%+v", list)
	}
}

func mustAttachments(t *testing.T, s *Store, runID int64) []*model.Attachment {
	t.Helper()
	list, err := s.Attachments(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	return list
}
