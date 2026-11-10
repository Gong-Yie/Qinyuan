package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"qinyuan/internal/store"
	"qinyuan/internal/testkit"
)

// M12 上传/列表/下载/删除的端到端（走真实 Handler）。
func TestAttachmentUploadListDownloadDelete(t *testing.T) {
	st := testkit.Store(t)
	srv := New(st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	run, err := st.CreateRun(t.Context(), store.CreateRunInput{Goal: "附件测试", WorkerCount: 1})
	if err != nil {
		t.Fatal(err)
	}

	// 两个文件：一个文本（含中文）、一个二进制；文件名带路径试图穿越。
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, f := range []struct {
		name string
		data []byte
	}{
		{"../../notes.txt", []byte("第一行：题目说明\nflag 在这里\n")},
		{"chall.bin", []byte{0x7f, 0x45, 0x4c, 0x46, 0x00}},
	} {
		h := make(map[string][]string)
		h["Content-Disposition"] = []string{`form-data; name="file"; filename="` + f.name + `"`}
		w, err := mw.CreatePart(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(f.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(ts.URL+"/api/runs/"+itoa64(run.ID)+"/attachments",
		mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 201 {
		t.Fatalf("上传应 201，实际 %d：%s", resp.StatusCode, raw)
	}
	var up struct {
		Items []map[string]any `json:"items"`
		Count int              `json:"count"`
	}
	if err := json.Unmarshal(raw, &up); err != nil {
		t.Fatal(err)
	}
	if up.Count != 2 {
		t.Fatalf("应上传 2 个，实际 %d：%s", up.Count, raw)
	}
	// 文件名被清洗：不含路径分隔符。
	for _, it := range up.Items {
		if n, _ := it["name"].(string); strings.ContainsAny(n, `/\`) {
			t.Fatalf("文件名应去掉路径：%q", n)
		}
	}

	// 列表：类型识别 + 摘要 + 下载地址。
	resp2, err := http.Get(ts.URL + "/api/runs/" + itoa64(run.ID) + "/attachments")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var list struct {
		Items []map[string]any `json:"items"`
	}
	_ = json.NewDecoder(resp2.Body).Decode(&list)
	if len(list.Items) != 2 {
		t.Fatalf("列表应 2 条：%+v", list.Items)
	}
	kinds := map[string]string{}
	var textID int64
	for _, it := range list.Items {
		name, _ := it["name"].(string)
		kind, _ := it["kind"].(string)
		kinds[name] = kind
		if name == "notes.txt" {
			if idNum, ok := it["id"].(float64); ok {
				textID = int64(idNum)
			}
		}
	}
	if kinds["notes.txt"] != "text" || kinds["chall.bin"] != "binary" {
		t.Fatalf("类型识别不对：%+v", kinds)
	}

	// 下载：字节完全一致。
	dl, err := http.Get(ts.URL + "/api/runs/" + itoa64(run.ID) + "/attachments/" + itoa64(textID))
	if err != nil {
		t.Fatal(err)
	}
	defer dl.Body.Close()
	got, _ := io.ReadAll(dl.Body)
	if string(got) != "第一行：题目说明\nflag 在这里\n" {
		t.Fatalf("下载内容不一致：%q", got)
	}
	if cd := dl.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Fatalf("应带下载头：%q", cd)
	}

	// 删除 → 列表变 1。
	req, _ := http.NewRequest(http.MethodDelete,
		ts.URL+"/api/runs/"+itoa64(run.ID)+"/attachments/"+itoa64(textID), nil)
	dresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer dresp.Body.Close()
	if dresp.StatusCode != 200 {
		t.Fatalf("删除应 200，实际 %d", dresp.StatusCode)
	}
	left, _ := st.Attachments(t.Context(), run.ID)
	if len(left) != 1 {
		t.Fatalf("删后应剩 1 条：%+v", left)
	}
}

// 超过单文件上限 → 400（而不是悄悄截断）。
func TestAttachmentUploadTooLarge(t *testing.T) {
	st := testkit.Store(t)
	srv := New(st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv.MaxUploadMB = 1
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	run, err := st.CreateRun(t.Context(), store.CreateRunInput{Goal: "上限测试", WorkerCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	w, _ := mw.CreateFormFile("file", "big.bin")
	_, _ = w.Write(bytes.Repeat([]byte("A"), 2<<20)) // 2MB > 1MB
	_ = mw.Close()

	resp, err := http.Post(ts.URL+"/api/runs/"+itoa64(run.ID)+"/attachments",
		mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("超限应 400，实际 %d：%s", resp.StatusCode, body)
	}
}
