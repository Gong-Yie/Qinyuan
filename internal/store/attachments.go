package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"qinyuan/internal/model"
)

// 本文件是 M12 的数据层：任务附件（只读投喂）。
//
// 三条纪律：
//  1. **同一任务内同名覆盖**（UNIQUE(run_id,name)）：重传同名文件是"更新"，不是加一份；
//  2. 路径由服务端生成（`runs/<id>/attachments/<sha>`），模型永远给不了路径；
//  3. 内容寻址 → 同一任务内同内容只落一份盘（去重），但每行仍是独立的展示名。

// AddAttachment 写一条附件元数据（同名覆盖）。
func (s *Store) AddAttachment(ctx context.Context, a *model.Attachment) (*model.Attachment, error) {
	if a.RunID <= 0 || a.Name == "" {
		return nil, errors.New("run_id 与 name 必填")
	}
	if a.Kind == "" {
		a.Kind = "text"
	}
	var out model.Attachment
	var created time.Time
	err := s.pool.QueryRow(ctx, `
		INSERT INTO attachments (run_id, name, kind, mime, size, sha256, path, summary)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (run_id, name) DO UPDATE
		   SET kind=EXCLUDED.kind, mime=EXCLUDED.mime, size=EXCLUDED.size,
		       sha256=EXCLUDED.sha256, path=EXCLUDED.path, summary=EXCLUDED.summary,
		       created_at=now()
		RETURNING id, run_id, name, kind, mime, size, sha256, path, summary, created_at`,
		a.RunID, a.Name, a.Kind, a.Mime, a.Size, a.SHA256, a.Path, a.Summary).
		Scan(&out.ID, &out.RunID, &out.Name, &out.Kind, &out.Mime, &out.Size,
			&out.SHA256, &out.Path, &out.Summary, &created)
	if err != nil {
		return nil, fmt.Errorf("写附件失败：%w", err)
	}
	out.CreatedAt = created
	return &out, nil
}

// Attachments 列一个任务的全部附件（按 id 正序）。
func (s *Store) Attachments(ctx context.Context, runID int64) ([]*model.Attachment, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, run_id, name, kind, mime, size, sha256, path, summary, created_at
		  FROM attachments WHERE run_id=$1 ORDER BY id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.Attachment{}
	for rows.Next() {
		var a model.Attachment
		if err := rows.Scan(&a.ID, &a.RunID, &a.Name, &a.Kind, &a.Mime, &a.Size,
			&a.SHA256, &a.Path, &a.Summary, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &a)
	}
	return out, rows.Err()
}

// AttachmentByID 读一条（限定 run：跨任务一律"不存在"）。
func (s *Store) AttachmentByID(ctx context.Context, runID, id int64) (*model.Attachment, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, run_id, name, kind, mime, size, sha256, path, summary, created_at
		  FROM attachments WHERE run_id=$1 AND id=$2`, runID, id)
	return scanAttachment(row)
}

// AttachmentByName 按展示名读（agent 的工具入口）。
func (s *Store) AttachmentByName(ctx context.Context, runID int64, name string) (*model.Attachment, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, run_id, name, kind, mime, size, sha256, path, summary, created_at
		  FROM attachments WHERE run_id=$1 AND name=$2`, runID, name)
	return scanAttachment(row)
}

func scanAttachment(row pgx.Row) (*model.Attachment, error) {
	var a model.Attachment
	err := row.Scan(&a.ID, &a.RunID, &a.Name, &a.Kind, &a.Mime, &a.Size,
		&a.SHA256, &a.Path, &a.Summary, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// DeleteAttachment 删一条元数据，返回被删的 path（调用方决定要不要删文件）。
func (s *Store) DeleteAttachment(ctx context.Context, runID, id int64) (string, bool, error) {
	var path string
	err := s.pool.QueryRow(ctx,
		`DELETE FROM attachments WHERE run_id=$1 AND id=$2 RETURNING path`, runID, id).Scan(&path)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	// 同一任务里可能还有别的展示名指向同一个内容文件：那就不能删文件。
	var refs int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM attachments WHERE run_id=$1 AND path=$2`, runID, path).Scan(&refs); err != nil {
		return path, true, err
	}
	if refs > 0 {
		return "", true, nil
	}
	return path, true, nil
}
