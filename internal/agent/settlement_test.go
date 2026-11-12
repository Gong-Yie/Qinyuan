package agent

import "testing"

// 收尾轮必须同时隐藏 http_request 和 exec：
// exec 可以用 network=target/callback 继续探测，绕开“只写回”的收尾语义。
func TestDefaultSettlementHidesExec(t *testing.T) {
	found := map[string]bool{}
	for _, name := range DefaultSettlementHidden {
		found[name] = true
	}
	for _, want := range []string{"http_request", "exec"} {
		if !found[want] {
			t.Fatalf("收尾轮应隐藏 %q，实际：%v", want, DefaultSettlementHidden)
		}
	}
}
