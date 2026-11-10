package agent

import (
	"archive/zip"
	"bytes"
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

// 造一个内存 zip：按传入顺序写条目（name → 内容）。
func zipOf(t *testing.T, files ...struct {
	Name string
	Body []byte
}) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range files {
		w, err := zw.Create(f.Name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(f.Body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// CTF1 的形状：zip → tempdir/WEB附件/fj.zip → ROOT.war → WEB-INF（文本 + .class）。
func buildCTF1LikeZip(t *testing.T) []byte {
	t.Helper()
	webXML := []byte(`<web-app><servlet><servlet-name>mvc-dispatcher</servlet-name></servlet></web-app>`)
	classBytes := append([]byte{0xca, 0xfe, 0xba, 0xbe, 0x00, 0x00, 0x00, 0x34, 0x00},
		[]byte("java/lang/String\x00/api/test\x00forbidden\x00getRequestURI\x00")...)
	war := zipOf(t,
		struct {
			Name string
			Body []byte
		}{"WEB-INF/web.xml", webXML},
		struct {
			Name string
			Body []byte
		}{"WEB-INF/classes/com/dbappsecurity/common/controller/ApiController.class", classBytes},
		struct {
			Name string
			Body []byte
		}{"Dockerfile", []byte("FROM jetty:9.4.49-jdk8\nCOPY flag /flag\n")},
	)
	inner := zipOf(t,
		struct {
			Name string
			Body []byte
		}{"Dockerfile", []byte("FROM jetty:9.4.49-jdk8\nCOPY ROOT.war /var/lib/jetty/webapps/root.war\n")},
		struct {
			Name string
			Body []byte
		}{"ROOT.war", war},
	)
	return zipOf(t,
		struct {
			Name string
			Body []byte
		}{"readme.txt", []byte("题目说明：登录后见 flag\n")},
		struct {
			Name string
			Body []byte
		}{"tempdir/WEB附件/fj.zip", inner},
	)
}

// M14：归档工具是"内存里看"，不落盘、有上限、能穿透嵌套。
func TestArchiveTools(t *testing.T) {
	st := testkit.Store(t)
	ctx := context.Background()
	run, _ := testkit.Run(t, st, nil, nil)
	dataDir := t.TempDir()

	content := buildCTF1LikeZip(t)
	dir := filepath.Join(dataDir, "runs", fmt.Sprint(run.ID), "attachments")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "att.zip"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	att, err := st.AddAttachment(ctx, &model.Attachment{
		RunID: run.ID, Name: "Web-easy_api的附件.zip", Kind: "archive",
		Mime: "application/zip", Size: int64(len(content)), SHA256: "sha-ctf1",
		Path:    filepath.Join("runs", fmt.Sprint(run.ID), "attachments", "att.zip"),
		Summary: "二进制，魔数：50 4b 03 04",
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = att
	rc := &Context{Store: st, RunID: run.ID, DataDir: dataDir, MaxOutput: 30000}

	list := func(raw string) (entries []map[string]any, text string) {
		t.Helper()
		res, err := NewArchiveListTool().Handle(ctx, rc, json.RawMessage(raw))
		if err != nil {
			t.Fatalf("archive_list 返回 Go error：%v", err)
		}
		if res.IsError {
			t.Fatalf("archive_list 失败：%s", res.Text)
		}
		var out struct {
			Entries []map[string]any `json:"entries"`
		}
		if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
			t.Fatalf("list 不是 JSON：%v（%s）", err, res.Text)
		}
		return out.Entries, res.Text
	}

	// 1) 根：能看到内层 fj.zip 与说明文件。
	entries, _ := list(`{"name":"Web-easy_api的附件.zip"}`)
	if len(entries) != 2 {
		t.Fatalf("根条目数 = %d，期望 2：%+v", len(entries), entries)
	}
	foundNested := false
	for _, e := range entries {
		if e["name"] == "tempdir/WEB附件/fj.zip" && e["archive"] == true {
			foundNested = true
		}
	}
	if !foundNested {
		t.Fatalf("没识别出内层归档：%+v", entries)
	}

	// 2) 穿透一层：看到 ROOT.war 与 Dockerfile。
	entries, _ = list(`{"name":"Web-easy_api的附件.zip","path":"tempdir/WEB附件/fj.zip"}`)
	if len(entries) != 2 {
		t.Fatalf("内层条目数 = %d，期望 2：%+v", len(entries), entries)
	}

	// 3) 再穿透一层 + prefix：只看 WEB-INF。
	entries, _ = list(`{"name":"Web-easy_api的附件.zip","path":"tempdir/WEB附件/fj.zip/ROOT.war","prefix":"WEB-INF/"}`)
	if len(entries) != 2 {
		t.Fatalf("WEB-INF 条目数 = %d，期望 2：%+v", len(entries), entries)
	}

	read := func(raw string) (map[string]any, string) {
		t.Helper()
		res, err := NewArchiveReadTool().Handle(ctx, rc, json.RawMessage(raw))
		if err != nil {
			t.Fatalf("archive_read 返回 Go error：%v", err)
		}
		if res.IsError {
			t.Fatalf("archive_read 失败：%s", res.Text)
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
			t.Fatalf("read 不是 JSON：%v（%s）", err, res.Text)
		}
		return out, res.Text
	}

	// 4) 读文本条目（穿透两层）。
	out, _ := read(`{"name":"Web-easy_api的附件.zip","path":"tempdir/WEB附件/fj.zip/ROOT.war/WEB-INF/web.xml"}`)
	if !strings.Contains(fmt.Sprint(out["content"]), "mvc-dispatcher") {
		t.Fatalf("没读到 web.xml 内容：%+v", out)
	}
	if out["mode"] != "text" {
		t.Fatalf("web.xml 应走 text，实际 %v", out["mode"])
	}
	// 4b) 分段读：文件比窗口大时必须返回前 N 字节 + 续读指针，而不是报错。
	out, _ = read(`{"name":"Web-easy_api的附件.zip","path":"tempdir/WEB附件/fj.zip/ROOT.war/WEB-INF/web.xml","limit":10}`)
	if got := len([]rune(fmt.Sprint(out["content"]))); got == 0 || got > 10 {
		t.Fatalf("分段读长度不对：%d", got)
	}
	if out["truncated"] != true || out["next_offset"] == nil {
		t.Fatalf("应当标记截断并给 next_offset：%+v", out)
	}

	// 5) .class 用 strings：黑名单与路由都在常量池里。
	out, _ = read(`{"name":"Web-easy_api的附件.zip","path":"tempdir/WEB附件/fj.zip/ROOT.war/WEB-INF/classes/com/dbappsecurity/common/controller/ApiController.class","mode":"strings"}`)
	strs := fmt.Sprint(out["strings"])
	for _, want := range []string{"/api/test", "forbidden", "getRequestURI"} {
		if !strings.Contains(strs, want) {
			t.Fatalf("strings 里没有 %q：%v", want, out["strings"])
		}
	}

	// 6) 忘了写 mode 也不会对着乱码瞎推：二进制自动降级 strings。
	out, _ = read(`{"name":"Web-easy_api的附件.zip","path":"tempdir/WEB附件/fj.zip/ROOT.war/WEB-INF/classes/com/dbappsecurity/common/controller/ApiController.class"}`)
	if out["mode"] != "strings(auto)" {
		t.Fatalf("二进制应自动降级 strings，实际 %v", out["mode"])
	}

	// 7) 错误路径都是"可读错误"，不是 Go error。
	bad := []string{
		`{"name":"Web-easy_api的附件.zip","path":"nope.txt"}`,
		`{"name":"Web-easy_api的附件.zip","path":"tempdir/WEB附件/fj.zip/Dockerfile/WEB-INF/web.xml"}`,
		`{"name":"Web-easy_api的附件.zip","path":"a/b/c/d/e/f.zip"}`,
		`{"name":"Web-easy_api的附件.zip","path":""}`,
	}
	for _, raw := range bad {
		res, err := NewArchiveReadTool().Handle(ctx, rc, json.RawMessage(raw))
		if err != nil {
			t.Fatalf("%s 不该返回 Go error：%v", raw, err)
		}
		if !res.IsError {
			t.Fatalf("%s 应当报错：%s", raw, res.Text)
		}
	}

	// 8) 非归档（普通文本）用 archive_list 要给可操作的提示。
	res, _ := NewArchiveListTool().Handle(ctx, rc, json.RawMessage(`{"name":"nope.zip"}`))
	if !res.IsError || !strings.Contains(res.Text, "附件不存在") {
		t.Fatalf("不存在的附件应当报错：%s", res.Text)
	}
}

// 压缩炸弹 / 超大条目：必须在读取前拦住（不能靠"读完再判断"）。
func TestArchiveReadRefusesHugeEntry(t *testing.T) {
	st := testkit.Store(t)
	ctx := context.Background()
	run, _ := testkit.Run(t, st, nil, nil)
	dataDir := t.TempDir()

	huge := bytes.Repeat([]byte("A"), int(maxArchiveEntry)+1024)
	content := zipOf(t, struct {
		Name string
		Body []byte
	}{"big.txt", huge})

	dir := filepath.Join(dataDir, "runs", fmt.Sprint(run.ID), "attachments")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bomb.zip"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddAttachment(ctx, &model.Attachment{
		RunID: run.ID, Name: "bomb.zip", Kind: "archive", Size: int64(len(content)),
		SHA256: "sha-bomb", Path: filepath.Join("runs", fmt.Sprint(run.ID), "attachments", "bomb.zip"),
	}); err != nil {
		t.Fatal(err)
	}

	rc := &Context{Store: st, RunID: run.ID, DataDir: dataDir}
	res, err := NewArchiveReadTool().Handle(ctx, rc, json.RawMessage(`{"name":"bomb.zip","path":"big.txt"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "上限") {
		t.Fatalf("超大条目应当被拒绝：%s", res.Text)
	}
}

// read_attachment 的 strings 模式：单独一个二进制附件也能按关键字找。
func TestReadAttachmentStringsMode(t *testing.T) {
	st := testkit.Store(t)
	ctx := context.Background()
	run, _ := testkit.Run(t, st, nil, nil)
	dataDir := t.TempDir()

	// 一个"像 ELF"的二进制，里面藏着两条字符串。
	body := append([]byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0},
		[]byte("QY{flag_in_binary}\x00/usr/local/etc/app.conf\x00")...)
	dir := filepath.Join(dataDir, "runs", fmt.Sprint(run.ID), "attachments")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.bin"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddAttachment(ctx, &model.Attachment{
		RunID: run.ID, Name: "app.bin", Kind: "binary", Size: int64(len(body)),
		SHA256: "sha-bin", Path: filepath.Join("runs", fmt.Sprint(run.ID), "attachments", "app.bin"),
	}); err != nil {
		t.Fatal(err)
	}

	rc := &Context{Store: st, RunID: run.ID, DataDir: dataDir}
	res, err := NewReadAttachmentTool().Handle(ctx, rc, json.RawMessage(`{"name":"app.bin","mode":"strings"}`))
	if err != nil || res.IsError {
		t.Fatalf("strings 失败：%+v %v", res, err)
	}
	if !strings.Contains(res.Text, "QY{flag_in_binary}") || !strings.Contains(res.Text, "/usr/local/etc/app.conf") {
		t.Fatalf("strings 没提到关键字：%.400s", res.Text)
	}
	if !strings.Contains(res.Text, "不可信输入") {
		t.Fatalf("必须保留不可信输入提示：%.200s", res.Text)
	}
}
