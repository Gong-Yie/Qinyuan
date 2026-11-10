package store

import (
	"context"
)

// KindCoverage 按实体类型分组的覆盖度。
type KindCoverage struct {
	Kind   string `json:"kind"`
	Total  int    `json:"total"`
	Tested int    `json:"tested"`
}

// Coverage 是范围覆盖度。
//
// 语义（实现规格/01 §6.4，M5b 补了两条）：
//   - 分母：`run_scope` 命中的实体；
//   - 分子：被 `fact` 锚定的实体，且**沿父链上卷** —— 事实锚在 /login 这个 endpoint 上时，
//     它所属的 service 与 ip 也算"碰过"；否则覆盖率会把"打了登录页"读成"这台主机没测过"；
//   - `finding` 是结论不是覆盖，`intent` 不算，`origin fact` 是任务起点也不算。
//
// 直接复用 CoverageGraph：覆盖图与覆盖度必须是同一份口径，否则同一页面上两个数字会打架。
type Coverage struct {
	Total  int            `json:"total"`
	Tested int            `json:"tested"`
	Pct    float64        `json:"pct"`
	ByKind []KindCoverage `json:"by_kind"`
}

// Coverage 计算某个 run 的覆盖度。
func (s *Store) Coverage(ctx context.Context, runID int64) (*Coverage, error) {
	nodes, _, err := s.CoverageGraph(ctx, runID)
	if err != nil {
		return nil, err
	}
	cov := &Coverage{}
	byKind := map[string]*KindCoverage{}
	order := []string{}
	for _, n := range nodes {
		if !n.InScope {
			continue
		}
		kc, ok := byKind[n.Kind]
		if !ok {
			kc = &KindCoverage{Kind: n.Kind}
			byKind[n.Kind] = kc
			order = append(order, n.Kind)
		}
		kc.Total++
		if n.Tested {
			kc.Tested++
		}
		cov.Total++
		if n.Tested {
			cov.Tested++
		}
	}
	for _, k := range order {
		cov.ByKind = append(cov.ByKind, *byKind[k])
	}
	if cov.Total > 0 {
		cov.Pct = float64(cov.Tested) / float64(cov.Total) * 100
	}
	return cov, nil
}
