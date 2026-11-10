package connectors

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"qinyuan/internal/report"
	"qinyuan/internal/store"
)

// 本文件是 P3-3 的第三条链路：CI。
//
// 两个方向：
//   - 导出：把每个已确认（verified）的 finding 变成一段可执行测试（shell 脚本）；
//   - 回写：回归跑完把结果写回 finding_retests（于是 verify_status 跟着变，
//     报告里"已确认/待复核"的分区自动更新）。

// ExportCI 把一个 run 里**已确认**的发现导出成可直接跑的脚本集合。
//
// 只导出 verified：没经过复测的 PoC 写进 CI 只会让流水线红一片（还可能误伤线上）。
// 返回写出的文件路径。
func ExportCI(ctx context.Context, st *store.Store, runID int64, dir string) ([]string, error) {
	rep, err := (&report.Builder{Store: st}).Build(ctx, runID, report.Options{Redact: false})
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	var out []string
	// 一条目标就够：CI 用一个入口脚本按顺序跑，失败即停（回归语义）。
	main := strings.Builder{}
	main.WriteString("#!/bin/sh\n")
	main.WriteString("# 钦原 CI 回归入口 · run #" + fmt.Sprint(runID) + "\n")
	main.WriteString("# 每个脚本失败即表示该发现已修复（或断言失效），退出码非 0\n")
	main.WriteString("set -e\n\n")
	main.WriteString("cd \"$(dirname \"$0\")\"\n\n")

	for _, f := range rep.Confirmed {
		if len(f.PoCs) == 0 {
			continue
		}
		name := fmt.Sprintf("finding-%d.sh", f.ID)
		body := "#!/bin/sh\n" +
			"# 发现 #" + fmt.Sprint(f.ID) + " · " + f.Name + "（" + f.Severity + "）\n" +
			"# 类别：" + f.Vuln + "\n" +
			"# 断言：状态码/响应特征应仍能复现；若不再复现 → 该发现已被修复\n" +
			"set -e\n\n" +
			strings.Join(f.PoCs, "\n\n") + "\n"
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			return out, err
		}
		out = append(out, path)
		main.WriteString("./" + name + "\n")
	}
	mainPath := filepath.Join(dir, "run-all.sh")
	if err := os.WriteFile(mainPath, []byte(main.String()), 0o755); err != nil {
		return out, err
	}
	out = append(out, mainPath)
	return out, nil
}

// ImportCIResult 把一条 CI 回归结果写回复测历史。
//
// status: verified（仍可复现 = 漏洞还在）/ failed（不再复现 = 已修复）。
// 其余一律 unverifiable（让报告把它归到"需人工确认"，而不是假装测过）。
func ImportCIResult(ctx context.Context, st *store.Store, runID, findingID int64, status, note string) (*store.Retest, error) {
	switch status {
	case "verified", "failed":
	case "unverifiable", "":
		status = "unverifiable"
	default:
		return nil, fmt.Errorf("status 只能是 verified/failed/unverifiable")
	}
	r := store.Retest{
		RunID: runID, FindingID: findingID, Status: status,
		Expected:  "CI 回归：脚本应能复现该发现",
		Observed:  note,
		Error:     "",
		DurationMS: 0,
		CreatedAt: time.Now().Unix(),
	}
	id, err := st.AddRetest(ctx, r)
	if err != nil {
		return nil, err
	}
	r.ID = id
	return &r, nil
}
