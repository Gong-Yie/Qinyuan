package store

import (
	"context"
	"testing"
)

// P2：expected_evidence 的匹配规则。
//
// 校准数据来自 3 次真实 run（run #18/#19/#20）的 15 对 (step, fact)：旧规则
// （整串 2-gram 覆盖率 ≥60%）在 10 个真实正例上 0 命中；新规则 12/15，且 4 个反例
// 全部不误报。这里锁住"会命中"和"不该命中"两类行为。
func TestExpectedEvidenceMatchesRealPhrasing(t *testing.T) {
	cases := []struct {
		name     string
		text     string
		expected string
		want     bool
	}{
		{
			name:     "整串包含（旧路径仍然最快）",
			text:     "已经拿到证据1",
			expected: "证据1",
			want:     true,
		},
		{
			name:     "任务口吻整句：命中的是里面那个具体片段",
			text:     "服务指纹：http://127.0.0.1:8080/ 为 nginx/1.18.0 (Ubuntu) 反向代理的 PHP/7.4.3 应用，页面自述版本 v1.4.2",
			expected: "拿到 Server 响应头或框架版本标识等服务指纹",
			want:     true,
		},
		{
			name:     "登录入口：3 字签名命中两个",
			text:     "确认登录入口为 http://127.0.0.1:8080/login（GET/POST 200，标题「登录 - Acme」），首页表单 action=/login",
			expected: "登录入口路径（登录页面或登录接口路径）",
			want:     true,
		},
		{
			name:     "ASCII 整词签名（server）",
			text:     "GET / -> 200, Server: nginx/1.18.0 (Ubuntu), X-Powered-By: PHP/7.4.3",
			expected: "Server 响应头与框架/版本指纹的原始响应证据",
			want:     true,
		},
		{
			name:     "只有 1 个 3 字签名，不够（宁漏不误）",
			text:     "这次探测只覆盖了端口层面的连通性",
			expected: "确认框架/中间件名称及其版本号",
			want:     false,
		},
		{
			name:     "反例：端口扫描不能证明指纹步",
			text:     "端口探测：80/443/8000/8080 均 connection refused，5000 超时",
			expected: "拿到 Server 响应头或框架版本标识等服务指纹",
			want:     false,
		},
		{
			name:     "反例：登录页事实不能证明版本步",
			text:     "真实登录页只有 /login（title「登录 - Acme」，148B，POST 表单 user+pass）",
			expected: "确认框架/中间件名称及其版本号（如 favicon 指纹、静态资源 hash、Cookie 名）",
			want:     false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := expectedMatches(c.text, c.expected); got != c.want {
				t.Fatalf("expectedMatches = %v，期望 %v", got, c.want)
			}
		})
	}
}

// 真实 run 的端到端：事实写回后步骤自动完成（不调 mark_step_done）。
func TestSatisfyStepByRuleOnRealPhrasing(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	run := newTestRun(t, s)

	stepID, err := s.AddStep(ctx, AddStepInput{
		RunID: run.ID, Text: "抓根路径响应头",
		ExpectedEvidence: "拿到 Server 响应头或框架版本标识等服务指纹",
	})
	if err != nil {
		t.Fatal(err)
	}
	const summary = "服务指纹：127.0.0.1:8080 为 nginx/1.18.0 反代的 PHP/7.4.3 应用"
	factID, err := s.AddFact(ctx, AddFactInput{
		RunID: run.ID, Summary: summary,
		Evidence: "GET / -> 200, Server: nginx/1.18.0 (Ubuntu)", Confidence: "observed",
	})
	if err != nil {
		t.Fatal(err)
	}
	satisfied, err := s.SatisfyStepsByFact(ctx, run.ID, factID, summary)
	if err != nil {
		t.Fatal(err)
	}
	if len(satisfied) != 1 || satisfied[0] != stepID {
		t.Fatalf("规则应命中步骤 #%d，实际 %v", stepID, satisfied)
	}
}
