package agent

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"qinyuan/internal/model"
)

// 本文件是 M14 的归档工具：**在内存里看归档**，不落盘、不解压到磁盘。
//
// 为什么需要它（docs/19 的 CTF1）：题目材料是 zip → fj.zip → ROOT.war，
// 没有 shell 的 worker 只能把 3.2MB 当 hexdump 读 —— 既读不完，也还原不出结构。
// 这里补的不是"解压"，而是三个只读动作：
//
//	archive_list  —— 列条目（名字/大小/压缩后大小/方法），可以穿透嵌套归档；
//	archive_read  —— 读某个条目的文本，或对二进制做 strings（.class 常量池靠它）；
//	read_attachment(mode=strings) —— 对非归档的二进制附件做同样的提取。
//
// 安全边界（与 docs/18 一致，别放松）：
//   - 只接受附件 id/name，不接受文件路径 → 没有任意读入口；
//   - 绝不写盘 → zip-slip 无从谈起；
//   - 单层解压 ≤ 64MB、单条目 ≤ 16MB、嵌套 ≤ 3 层、清单 ≤ 500 条；
//   - 条目名只做回显与精确匹配，不参与任何文件系统操作。

const (
	maxArchiveLayer   = 64 << 20 // 单层归档解压到内存的上限（含嵌套层）
	maxArchiveEntry   = 16 << 20 // 单个条目读取上限
	maxArchiveDepth   = 3        // 允许穿透的嵌套层数
	maxArchiveList    = 500      // 一次列出的条目上限
	archiveReadDef    = 16 << 10 // 默认读多少字节
	archiveReadMax    = 64 << 10 // 单次最多读多少字节
	maxStringsScan    = 4 << 20  // strings 最多扫多少字节
	maxStringsItems   = 400      // strings 最多返回多少条
	maxStringRun      = 200      // 单条字符串最长保留多少字符
	archiveListNote   = "path 可以指向嵌套归档（如 tempdir/WEB附件/fj.zip）；条目名原样回显，读它用 archive_read"
	attachmentMissing = "附件不存在（跨任务的附件一律不可见；用 list_attachments 核对名字）"
)

// NewArchiveListTool 列出附件（或其中某个嵌套归档）的条目。
func NewArchiveListTool() *Tool {
	return &Tool{
		Name: "archive_list",
		Description: `列出 zip / war / jar / apk 等归档的条目（只读，不落盘，不解压到磁盘）。
path 指向一个**嵌套归档**时可以继续往下看（例：先列附件根，看到 tempdir/WEB附件/fj.zip，
再传 path="tempdir/WEB附件/fj.zip" 看里面的 ROOT.war 与 Dockerfile）。
prefix 只看某个前缀（例："WEB-INF/"）。返回名字/大小/压缩后大小/压缩方法/是否还是归档。
读条目内容用 archive_read（文本给正文，.class 这类二进制用 mode=strings）。`,
		ReadOnly: true,
		Schema: toolSpecJSON(`{
  "type": "object",
  "properties": {
    "id": {"type": "integer", "description": "附件 id（与 name 二选一）"},
    "name": {"type": "string", "description": "附件名（与 id 二选一）"},
    "path": {"type": "string", "description": "嵌套归档的路径（空 = 附件本身），如 tempdir/WEB附件/fj.zip"},
    "prefix": {"type": "string", "description": "只列这个前缀下的条目，如 WEB-INF/"}
  }
}`),
		Handle: handleArchiveList,
	}
}

// NewArchiveReadTool 读归档条目的文本，或对二进制条目做 strings。
func NewArchiveReadTool() *Tool {
	return &Tool{
		Name: "archive_read",
		Description: `读归档里某个条目的内容（只读，不落盘）。
path 是**条目路径**，可以包含嵌套归档（例：tempdir/WEB附件/fj.zip/ROOT.war/WEB-INF/web.xml）。
mode：
  - text（默认）：按文本返回；二进制/压缩内容会自动转 strings 并标注；
  - strings：提取可打印 ASCII 片段（≥4 字符，去重，带顺序）—— .class 的常量池、
    jar/二进制里的路由与关键字都靠它；
  - hex：小范围 hexdump（只在确有必要时用）。
文本支持 offset/limit 分段（默认 16KB，最大 64KB）；strings 默认扫 512KB（最大 4MB）。`,
		ReadOnly: true,
		Schema: toolSpecJSON(`{
  "type": "object",
  "properties": {
    "id": {"type": "integer", "description": "附件 id（与 name 二选一）"},
    "name": {"type": "string", "description": "附件名（与 id 二选一）"},
    "path": {"type": "string", "description": "条目路径（可含嵌套归档），如 WEB-INF/web.xml"},
    "mode": {"type": "string", "enum": ["text", "strings", "hex"], "description": "默认 text"},
    "offset": {"type": "integer", "description": "text/hex 从第几字节开始，默认 0"},
    "limit": {"type": "integer", "description": "本次读多少字节；text/hex 默认 16384（≤65536），strings 默认 524288（≤4194304）"}
  }
}`),
		Handle: handleArchiveRead,
	}
}

func handleArchiveList(ctx context.Context, rc *Context, raw json.RawMessage) (Result, error) {
	var in struct {
		ID     int64  `json:"id"`
		Name   string `json:"name"`
		Path   string `json:"path"`
		Prefix string `json:"prefix"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return Errorf("参数解析失败：%v", err), nil
	}
	att, err := lookupAttachment(ctx, rc, in.ID, in.Name)
	if err != nil {
		return Errorf("%s", attachmentMissing), nil
	}
	zr, label, err := openArchiveLayer(rc.DataDir, att, in.Path)
	if err != nil {
		return Errorf("%v", err), nil
	}

	prefix := strings.TrimPrefix(strings.TrimSpace(in.Prefix), "./")
	items := make([]map[string]any, 0, 64)
	scanned := 0
	truncated := false
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if prefix != "" && !strings.HasPrefix(f.Name, prefix) {
			continue
		}
		scanned++
		if len(items) >= maxArchiveList {
			truncated = true
			continue
		}
		items = append(items, map[string]any{
			"name":       f.Name,
			"size":       int64(f.UncompressedSize64),
			"compressed": int64(f.CompressedSize64),
			"method":     methodName(f.Method),
			"archive":    looksArchive(f.Name),
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i]["name"].(string) < items[j]["name"].(string) })

	out := map[string]any{
		"archive":     label,
		"entries":     items,
		"returned":    len(items),
		"matched":     scanned,
		"truncated":   truncated,
		"note":        archiveListNote,
		"untrusted":   true,
		"max_entries": maxArchiveList,
	}
	if truncated {
		out["hint"] = fmt.Sprintf("条目超过 %d 条，已截断：用 prefix 缩小范围", maxArchiveList)
	}
	return JSONResult(out), nil
}

func handleArchiveRead(ctx context.Context, rc *Context, raw json.RawMessage) (Result, error) {
	var in struct {
		ID     int64  `json:"id"`
		Name   string `json:"name"`
		Path   string `json:"path"`
		Mode   string `json:"mode"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return Errorf("参数解析失败：%v", err), nil
	}
	if strings.TrimSpace(in.Path) == "" {
		return Errorf("path 必填：条目路径，先用 archive_list 看清单"), nil
	}
	if in.Offset < 0 {
		in.Offset = 0
	}
	mode := strings.ToLower(strings.TrimSpace(in.Mode))
	if mode == "" {
		mode = "text"
	}
	switch mode {
	case "text", "strings", "hex":
	default:
		return Errorf("mode 只支持 text / strings / hex"), nil
	}

	att, err := lookupAttachment(ctx, rc, in.ID, in.Name)
	if err != nil {
		return Errorf("%s", attachmentMissing), nil
	}
	entry, label, err := openArchiveEntry(rc.DataDir, att, in.Path)
	if err != nil {
		return Errorf("%v", err), nil
	}
	size := int64(entry.UncompressedSize64)
	if size > maxArchiveEntry {
		return Errorf("条目 %q 解压后 %d 字节，超过上限 %d（疑似压缩炸弹）", label, size, maxArchiveEntry), nil
	}

	// strings 模式：窗口更大，从 offset 开始扫。
	if mode == "strings" {
		if in.Limit <= 0 {
			in.Limit = 512 << 10
		}
		if in.Limit > maxStringsScan {
			in.Limit = maxStringsScan
		}
		buf, err := readZipEntry(entry, int64(in.Offset)+int64(in.Limit))
		if err != nil {
			return Errorf("读条目失败：%v", err), nil
		}
		if int64(in.Offset) >= int64(len(buf)) {
			buf = nil
		} else {
			buf = buf[in.Offset:]
		}
		strs := extractStrings(buf, maxStringsItems)
		return JSONResult(map[string]any{
			"entry":     label,
			"size":      size,
			"mode":      "strings",
			"strings":   strs,
			"returned":  len(strs),
			"truncated": len(strs) >= maxStringsItems,
			"untrusted": true,
			"note":      "可打印 ASCII 片段（≥4 字符，已去重）；.class 的常量池通常就在这里",
		}), nil
	}

	// text / hex：按字节窗口读。
	if in.Limit <= 0 {
		in.Limit = archiveReadDef
	}
	if in.Limit > archiveReadMax {
		in.Limit = archiveReadMax
	}
	window := int64(in.Offset) + int64(in.Limit)
	buf, err := readZipEntry(entry, window)
	if err != nil {
		return Errorf("读条目失败：%v", err), nil
	}
	if int64(in.Offset) >= int64(len(buf)) {
		buf = nil
	} else {
		buf = buf[in.Offset:]
	}

	if mode == "hex" {
		return JSONResult(map[string]any{
			"entry":     label,
			"size":      size,
			"mode":      "hex",
			"offset":    in.Offset,
			"hexdump":   hex.Dump(buf),
			"truncated": int64(in.Offset)+int64(len(buf)) < size,
			"untrusted": true,
		}), nil
	}

	// text：二进制就自动降级成 strings，并明确标注（别让模型对着乱码瞎推理）。
	if looksBinaryBytes(buf) {
		strs := extractStrings(buf, maxStringsItems)
		return JSONResult(map[string]any{
			"entry":     label,
			"size":      size,
			"mode":      "strings(auto)",
			"strings":   strs,
			"returned":  len(strs),
			"truncated": int64(in.Offset)+int64(len(buf)) < size,
			"untrusted": true,
			"note":      "这段内容是二进制，已自动改成 strings；要原字节用 mode=hex",
		}), nil
	}
	text := strings.ToValidUTF8(string(buf), "\uFFFD")
	out := map[string]any{
		"entry":       label,
		"size":        size,
		"mode":        "text",
		"offset":      in.Offset,
		"content":     text,
		"truncated":   int64(in.Offset)+int64(len(buf)) < size,
		"untrusted":   true,
		"next_offset": in.Offset + len(buf),
	}
	if int64(in.Offset)+int64(len(buf)) >= size {
		delete(out, "next_offset")
	}
	return JSONResult(out), nil
}

// openArchiveLayer 打开"附件 → 若干层嵌套归档之后"的那个归档。
func openArchiveLayer(dataDir string, att *model.Attachment, inner string) (*zip.Reader, string, error) {
	zr, label, entry, err := walkArchive(dataDir, att, inner, true)
	if err != nil {
		return nil, "", err
	}
	_ = entry
	return zr, label, nil
}

// openArchiveEntry 把 "a/b.zip/c/d.txt" 解析成最后一层的条目。
func openArchiveEntry(dataDir string, att *model.Attachment, p string) (*zip.File, string, error) {
	_, label, entry, err := walkArchive(dataDir, att, p, false)
	if err != nil {
		return nil, "", err
	}
	if entry == nil {
		if strings.TrimSpace(p) == "" {
			return nil, "", fmt.Errorf("path 不能为空：先用 archive_list 看清单")
		}
		return nil, "", fmt.Errorf("%s 里没有条目 %q（先用 archive_list 看清单）", label, strings.TrimSpace(p))
	}
	return entry, label + "/" + entry.Name, nil
}

// walkArchive 沿 path 下钻。条目名本身可能带 "/"（如 tempdir/WEB附件/fj.zip），
// 所以不能 split("/") 逐段找，而要"最长前缀匹配"：
//   - 整段正好等于某个条目 → openFinal=false 时就是它（entry 非空）；
//     openFinal=true 时要求它本身是归档、打开后返回那一层；
//   - 否则找最长的条目名 n 使 path 以 "n/" 开头 → 它必须是归档，打开后继续；
//   - 都找不到 → 报"没有条目"。
func walkArchive(dataDir string, att *model.Attachment, p string, openFinal bool) (*zip.Reader, string, *zip.File, error) {
	raw, label, err := openAttachmentBytes(dataDir, att)
	if err != nil {
		return nil, "", nil, err
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, "", nil, fmt.Errorf("%s 不是可解析的 zip/war/jar：%v（若是普通文件，用 read_attachment 读）", att.Name, err)
	}
	remain := strings.Trim(strings.ReplaceAll(p, "\\", "/"), "/")
	if remain == "" || remain == "." {
		return zr, label, nil, nil
	}

	for depth := 0; ; depth++ {
		if f := exactEntry(zr, remain); f != nil {
			if f.FileInfo().IsDir() {
				return nil, "", nil, fmt.Errorf("%q 是目录；用 archive_list 看它下面有什么", remain)
			}
			if !openFinal {
				return zr, label, f, nil
			}
			if !looksArchive(f.Name) {
				return nil, "", nil, fmt.Errorf("%q 不是归档（%s）；要读它的内容用 archive_read", f.Name, methodName(f.Method))
			}
			if depth+1 > maxArchiveDepth {
				return nil, "", nil, fmt.Errorf("嵌套层数超过上限 %d", maxArchiveDepth)
			}
			nzr, err := openNestedZip(f, label)
			if err != nil {
				return nil, "", nil, err
			}
			return nzr, label + "/" + f.Name, nil, nil
		}
		next, rest := longestPrefixEntry(zr, remain)
		if next == nil {
			return nil, "", nil, fmt.Errorf("%s 里没有条目 %q（先用 archive_list 看清单）", label, remain)
		}
		if !looksArchive(next.Name) {
			return nil, "", nil, fmt.Errorf("路径里的 %q 不是归档，不能再往下走；要读它的内容请把 path 停在它这里", next.Name)
		}
		if depth+1 > maxArchiveDepth {
			return nil, "", nil, fmt.Errorf("嵌套层数超过上限 %d", maxArchiveDepth)
		}
		nzr, err := openNestedZip(next, label)
		if err != nil {
			return nil, "", nil, err
		}
		label = label + "/" + next.Name
		zr = nzr
		remain = rest
	}
}

func exactEntry(zr *zip.Reader, name string) *zip.File {
	for _, f := range zr.File {
		if f.Name == name || strings.TrimSuffix(f.Name, "/") == name {
			return f
		}
	}
	return nil
}

// longestPrefixEntry 找最长的条目名 n，使 remain 以 "n/" 开头；返回该条目与剩余路径。
func longestPrefixEntry(zr *zip.Reader, remain string) (*zip.File, string) {
	var best *zip.File
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		name := strings.TrimSuffix(f.Name, "/")
		if !strings.HasPrefix(remain, name+"/") {
			continue
		}
		if best == nil || len(name) > len(strings.TrimSuffix(best.Name, "/")) {
			best = f
		}
	}
	if best == nil {
		return nil, ""
	}
	name := strings.TrimSuffix(best.Name, "/")
	return best, strings.Trim(strings.TrimPrefix(remain, name), "/")
}

func openAttachmentBytes(dataDir string, att *model.Attachment) ([]byte, string, error) {
	p, err := attachmentPath(dataDir, att.Path)
	if err != nil {
		return nil, "", fmt.Errorf("附件路径异常（已拒绝读取）：%v", err)
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, "", fmt.Errorf("附件文件打不开（可能已被清理）：%v", err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxArchiveLayer+1))
	if err != nil {
		return nil, "", fmt.Errorf("读附件失败：%v", err)
	}
	if int64(len(raw)) > maxArchiveLayer {
		return nil, "", fmt.Errorf("附件超过 %d 字节上限，不支持在内存里展开", maxArchiveLayer)
	}
	return raw, att.Name, nil
}

func openNestedZip(f *zip.File, label string) (*zip.Reader, error) {
	if f.UncompressedSize64 > maxArchiveLayer {
		return nil, fmt.Errorf("%s/%s 解压后 %d 字节，超过单层上限 %d", label, f.Name, f.UncompressedSize64, maxArchiveLayer)
	}
	b, err := readZipEntry(f, maxArchiveLayer)
	if err != nil {
		return nil, err
	}
	if int64(len(b)) != int64(f.UncompressedSize64) {
		return nil, fmt.Errorf("%s 解压不完整（%d/%d 字节，超过单层上限或数据损坏）",
			f.Name, len(b), f.UncompressedSize64)
	}
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return nil, fmt.Errorf("%s 不是可解析的归档：%v", f.Name, err)
	}
	return zr, nil
}

func readZipEntry(f *zip.File, limit int64) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	// 只读前 limit 字节：调用方要"分段读"，不该因为文件比窗口大就报错。
	// 压缩炸弹由调用方在**读之前**看 UncompressedSize64 拦住。
	return io.ReadAll(io.LimitReader(rc, limit))
}

func looksArchive(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".zip", ".war", ".jar", ".ear", ".apk", ".aar":
		return true
	}
	return false
}

func methodName(m uint16) string {
	switch m {
	case zip.Store:
		return "store"
	case zip.Deflate:
		return "deflate"
	default:
		return fmt.Sprintf("method-%d", m)
	}
}

// extractStrings 取可打印 ASCII 片段（≥4 字符），按出现顺序去重，单条截断到 200 字符。
//
// 为什么是 ASCII：.class 的常量池、jar 里的路由与类名、ELF 的路径基本都是 ASCII；
// 中文常量会以 UTF-8 字节出现，被切成多个片段，不影响"找关键字"这个用途。
func extractStrings(b []byte, limit int) []string {
	out := make([]string, 0, 64)
	seen := map[string]bool{}
	start := -1
	flush := func(end int) {
		if start < 0 {
			return
		}
		s := string(b[start:end])
		start = -1
		if len(s) < 4 {
			return
		}
		if len(s) > maxStringRun {
			s = s[:maxStringRun]
		}
		if seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for i, c := range b {
		if c >= 0x20 && c <= 0x7e {
			if start < 0 {
				start = i
			}
			continue
		}
		flush(i)
	}
	flush(len(b))
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// looksBinary 用"含 NUL 或坏 UTF-8 比例过高"判定二进制。
func looksBinaryBytes(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	if bytes.IndexByte(b, 0) >= 0 {
		return true
	}
	return !utf8.Valid(b)
}
