package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"qinyuan/internal/model"
)

// 本文件是 M12 的附件上传/下载口。
//
// 安全边界（写清楚）：
//   - 上传需要 operator（默认规则：写操作），单文件 ≤ MaxUploadMB；
//   - 落盘路径**由服务端生成**（内容寻址），文件名只做展示、不参与路径；
//   - agent 只能通过只读工具按 name/id 读，永远拿不到路径或任意读的能力。

// defaultMaxUploadMB 单文件上限（配置可覆盖）。
const defaultMaxUploadMB = 32

func (s *Server) maxUploadBytes() int64 {
	mb := s.MaxUploadMB
	if mb <= 0 {
		mb = defaultMaxUploadMB
	}
	return int64(mb) << 20
}

func (s *Server) handleUploadAttachment(w http.ResponseWriter, r *http.Request) {
	runID, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if _, err := s.Store.Run(r.Context(), runID); err != nil {
		writeError(w, http.StatusNotFound, "任务不存在")
		return
	}
	maxBytes := s.maxUploadBytes()
	// 多留 1MB 给 multipart 边界与其它小字段。
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes+1<<20)
	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "需要 multipart/form-data（字段名 file）")
		return
	}

	dir := filepath.Join(s.dataDir(), "runs", itoa64(runID), "attachments")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "建附件目录失败："+err.Error())
		return
	}

	results := []map[string]any{}
	errs := []string{}
	for {
		part, perr := mr.NextPart()
		if errors.Is(perr, io.EOF) {
			break
		}
		if perr != nil {
			writeError(w, http.StatusBadRequest, "读 multipart 失败："+perr.Error())
			return
		}
		if part.FormName() != "file" {
			_, _ = io.Copy(io.Discard, part)
			_ = part.Close()
			continue
		}
		raw, rerr := io.ReadAll(io.LimitReader(part, maxBytes+1))
		_ = part.Close()
		if rerr != nil {
			errs = append(errs, fmt.Sprintf("%s：读取出错 %v", part.FileName(), rerr))
			continue
		}
		if int64(len(raw)) > maxBytes {
			errs = append(errs, fmt.Sprintf("%s：超过单文件上限 %dMB", part.FileName(), maxBytes>>20))
			continue
		}
		name := sanitizeFilename(part.FileName())
		if name == "" {
			errs = append(errs, "跳过了一个没有文件名的 part")
			continue
		}
		sum := sha256.Sum256(raw)
		sha := hex.EncodeToString(sum[:])
		kind, mimeType := classifyAttachment(name, raw)
		ext := strings.ToLower(filepath.Ext(name))
		rel := filepath.Join("runs", itoa64(runID), "attachments", sha[:16]+ext)
		full := filepath.Join(s.dataDir(), rel)
		if _, statErr := os.Stat(full); statErr != nil {
			// 内容寻址：同内容只写一次（同任务内天然去重）。
			tmp := full + ".tmp"
			if werr := os.WriteFile(tmp, raw, 0o644); werr != nil {
				errs = append(errs, fmt.Sprintf("%s：落盘失败 %v", name, werr))
				continue
			}
			if rerr := os.Rename(tmp, full); rerr != nil {
				_ = os.Remove(tmp)
				errs = append(errs, fmt.Sprintf("%s：落盘失败 %v", name, rerr))
				continue
			}
		}
		att, aerr := s.Store.AddAttachment(r.Context(), &model.Attachment{
			RunID: runID, Name: name, Kind: kind, Mime: mimeType,
			Size: int64(len(raw)), SHA256: sha, Path: rel,
			Summary: summarizeAttachment(kind, raw),
		})
		if aerr != nil {
			errs = append(errs, fmt.Sprintf("%s：入库失败 %v", name, aerr))
			continue
		}
		results = append(results, attachmentDTO(att))
	}
	if len(results) == 0 {
		msg := "没有成功上传任何文件（字段名必须是 file）"
		if len(errs) > 0 {
			msg += "：" + strings.Join(errs, "；")
		}
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	sort.Slice(results, func(i, j int) bool {
		a, _ := results[i]["id"].(int64)
		b, _ := results[j]["id"].(int64)
		return a < b
	})
	writeJSON(w, http.StatusCreated, map[string]any{
		"items": results, "count": len(results), "errors": errs,
	})
}

func (s *Server) handleListAttachments(w http.ResponseWriter, r *http.Request) {
	runID, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	atts, err := s.Store.Attachments(r.Context(), runID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	items := make([]map[string]any, 0, len(atts))
	for _, a := range atts {
		items = append(items, attachmentDTO(a))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "count": len(items)})
}

// handleDownloadAttachment 原样下载（给人工核对用；agent 不走这个口）。
func (s *Server) handleDownloadAttachment(w http.ResponseWriter, r *http.Request) {
	runID, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	aid, ok := pathID(w, r, "aid")
	if !ok {
		return
	}
	att, err := s.Store.AttachmentByID(r.Context(), runID, aid)
	if err != nil {
		writeError(w, http.StatusNotFound, "附件不存在")
		return
	}
	full, perr := attachmentFilePath(s.dataDir(), att.Path)
	if perr != nil {
		writeError(w, http.StatusInternalServerError, "附件路径异常")
		return
	}
	f, oerr := os.Open(full)
	if oerr != nil {
		writeError(w, http.StatusGone, "附件文件已被清理")
		return
	}
	defer f.Close()
	ctype := att.Mime
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", contentDisposition(att.Name))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", att.Size))
	_, _ = io.Copy(w, f)
}

func (s *Server) handleDeleteAttachment(w http.ResponseWriter, r *http.Request) {
	runID, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	aid, ok := pathID(w, r, "aid")
	if !ok {
		return
	}
	path, deleted, err := s.Store.DeleteAttachment(r.Context(), runID, aid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !deleted {
		writeError(w, http.StatusNotFound, "附件不存在")
		return
	}
	// path 非空 = 没有别的展示名引用这个内容，可以真删文件。
	if path != "" {
		if full, perr := attachmentFilePath(s.dataDir(), path); perr == nil {
			_ = os.Remove(full)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": aid})
}

func attachmentDTO(a *model.Attachment) map[string]any {
	return map[string]any{
		"id": a.ID, "name": a.Name, "kind": a.Kind, "mime": a.Mime,
		"size": a.Size, "sha256": a.SHA256,
		"summary": model.TrimSummary(a.Summary, 200),
		"created_at": a.CreatedAt.Unix(),
		"download":   fmt.Sprintf("/api/runs/%d/attachments/%d", a.RunID, a.ID),
	}
}

// attachmentFilePath 与 agent 侧同一套纵深防御：库里的相对路径不能逃出 DataDir。
func attachmentFilePath(dataDir, rel string) (string, error) {
	if strings.TrimSpace(rel) == "" {
		return "", fmt.Errorf("路径为空")
	}
	base, err := filepath.Abs(dataDir)
	if err != nil {
		return "", err
	}
	full, err := filepath.Abs(filepath.Join(base, rel))
	if err != nil {
		return "", err
	}
	if full != base && !strings.HasPrefix(full, base+string(os.PathSeparator)) {
		return "", fmt.Errorf("路径逃出数据目录")
	}
	return full, nil
}

// sanitizeFilename 只保留"展示用"的名字：去路径、去控制字符、限长。
//
// 名字永远不参与落盘路径（落盘用 sha），所以这里的目标是"别让名字搞坏 prompt / 头"。
func sanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == unicode.ReplacementChar:
			continue
		case unicode.IsControl(r):
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	// 限长：按 rune 截断，别把 UTF-8 切坏。
	if r := []rune(out); len(r) > 120 {
		ext := filepath.Ext(out)
		keep := 120 - len([]rune(ext))
		if keep < 8 {
			keep = 8
		}
		out = string(r[:keep]) + ext
	}
	return out
}

var kindByExt = map[string]string{
	".zip": "archive", ".tar": "archive", ".gz": "archive", ".tgz": "archive",
	".7z": "archive", ".rar": "archive", ".bz2": "archive", ".xz": "archive",
	".png": "image", ".jpg": "image", ".jpeg": "image", ".gif": "image",
	".webp": "image", ".bmp": "image", ".ico": "image",
}

// classifyAttachment 判断类型：先看扩展名，再看内容是不是合法 UTF-8 文本。
func classifyAttachment(name string, raw []byte) (kind, mimeType string) {
	ext := strings.ToLower(filepath.Ext(name))
	if k, ok := kindByExt[ext]; ok {
		if t := mime.TypeByExtension(ext); t != "" {
			return k, t
		}
		return k, "application/octet-stream"
	}
	head := raw
	if len(head) > 8192 {
		head = head[:8192]
	}
	if looksText(head) {
		if t := mime.TypeByExtension(ext); strings.HasPrefix(t, "text/") || t == "application/json" {
			return "text", t
		}
		return "text", "text/plain; charset=utf-8"
	}
	return "binary", "application/octet-stream"
}

// looksText：无 NUL 且（整体合法 UTF-8，或截断在半个字符上时也接受）。
func looksText(head []byte) bool {
	for _, b := range head {
		if b == 0 {
			return false
		}
	}
	if utf8.Valid(head) {
		return true
	}
	if len(head) >= 3 {
		for cut := 1; cut <= 3 && cut < len(head); cut++ {
			if utf8.Valid(head[:len(head)-cut]) {
				return true
			}
		}
	}
	return false
}

// summarizeAttachment 生成注入态势的"前 200 字摘要"。
func summarizeAttachment(kind string, raw []byte) string {
	if kind != "text" {
		n := len(raw)
		if n > 8 {
			n = 8
		}
		parts := make([]string, 0, n)
		for _, b := range raw[:n] {
			parts = append(parts, fmt.Sprintf("%02x", b))
		}
		return "二进制，魔数：" + strings.Join(parts, " ")
	}
	s := utf8Text(raw)
	s = strings.Join(strings.Fields(s), " ") // 折成一行：注入 prompt 时更稳定
	return model.TrimSummary(s, 200)
}

func utf8Text(raw []byte) string {
	if len(raw) > 4096 {
		raw = raw[:4096]
	}
	out := strings.ToValidUTF8(string(raw), "\uFFFD")
	return strings.ReplaceAll(out, "\x00", "")
}

// contentDisposition 生成兼容中文文件名的下载头（ASCII 回退 + RFC 5987）。
func contentDisposition(name string) string {
	ascii := strings.Map(func(r rune) rune {
		if r < 32 || r > 126 || r == '"' {
			return '_'
		}
		return r
	}, name)
	if ascii == "" {
		ascii = "attachment.bin"
	}
	return fmt.Sprintf("attachment; filename=%q; filename*=UTF-8''%s", ascii, url.PathEscape(name))
}
