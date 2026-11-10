package agent

import (
	"encoding/json"
	"time"
)

// intentBudget 是 planner 在 add_intent 里给一条方向定的预算（payload.budget）。
type intentBudget struct {
	MaxTurns   int `json:"max_turns"`
	MaxSeconds int `json:"max_seconds"`
}

// resolveBudget 把"任务级配置"与"意图级预算"夹到一起。
//
// 语义是**上限收紧**：配置是天花板，意图预算只能更小，不能放大。
// 越界值按上下限夹住而不报错 —— 预算是优化项，不该让一次执行直接失败。
// 返回的 applied 表示意图确实带了预算（用于发一条可审计的 activity）。
func resolveBudget(cfgTurns int, cfgDur time.Duration, payload []byte) (int, time.Duration, bool) {
	if cfgTurns <= 0 {
		cfgTurns = 12
	}
	if cfgDur <= 0 {
		cfgDur = 10 * time.Minute
	}

	var p struct {
		Budget *intentBudget `json:"budget"`
	}
	if len(payload) > 0 {
		_ = json.Unmarshal(payload, &p)
	}
	if p.Budget == nil || (p.Budget.MaxTurns <= 0 && p.Budget.MaxSeconds <= 0) {
		return cfgTurns, cfgDur, false
	}

	turns, dur := cfgTurns, cfgDur
	if n := p.Budget.MaxTurns; n > 0 {
		if n < 1 {
			n = 1
		}
		if n > 50 {
			n = 50
		}
		if n < turns {
			turns = n
		}
	}
	if s := p.Budget.MaxSeconds; s > 0 {
		if s < 30 {
			s = 30
		}
		if s > 3600 {
			s = 3600
		}
		if d := time.Duration(s) * time.Second; d < dur {
			dur = d
		}
	}
	return turns, dur, true
}
