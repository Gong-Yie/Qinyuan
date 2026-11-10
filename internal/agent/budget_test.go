package agent

import (
	"testing"
	"time"
)

// P4：意图预算是"上限收紧"，不能放大任务级配置，也不能把执行搞挂。
func TestResolveBudget(t *testing.T) {
	const cfgTurns = 12
	cfgDur := 10 * time.Minute

	cases := []struct {
		name      string
		payload   string
		wantTurns int
		wantDur   time.Duration
		applied   bool
	}{
		{"没有预算：用配置", `{"summary":"x"}`, cfgTurns, cfgDur, false},
		{"空 payload", ``, cfgTurns, cfgDur, false},
		{"收紧回合", `{"budget":{"max_turns":3}}`, 3, cfgDur, true},
		{"收紧时长", `{"budget":{"max_seconds":120}}`, cfgTurns, 2 * time.Minute, true},
		{"同时收紧", `{"budget":{"max_turns":5,"max_seconds":90}}`, 5, 90 * time.Second, true},
		{"比配置大：不放大", `{"budget":{"max_turns":50,"max_seconds":3600}}`, cfgTurns, cfgDur, true},
		{"越界小值被夹住", `{"budget":{"max_turns":-3,"max_seconds":1}}`, cfgTurns, 30 * time.Second, true},
		{"坏 JSON 不 panic", `{"budget":`, cfgTurns, cfgDur, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			turns, dur, applied := resolveBudget(cfgTurns, cfgDur, []byte(c.payload))
			if turns != c.wantTurns || dur != c.wantDur || applied != c.applied {
				t.Fatalf("resolveBudget = (%d, %s, %v)，期望 (%d, %s, %v)",
					turns, dur, applied, c.wantTurns, c.wantDur, c.applied)
			}
		})
	}
}

// 配置为 0（未设置）时要有默认值，否则会被预算"收紧成 0 回合"。
func TestResolveBudgetDefaultsWhenConfigEmpty(t *testing.T) {
	turns, dur, _ := resolveBudget(0, 0, []byte(`{"budget":{"max_turns":3}}`))
	if turns != 3 || dur != 10*time.Minute {
		t.Fatalf("resolveBudget = (%d, %s)，期望 (3, 10m)", turns, dur)
	}
}
