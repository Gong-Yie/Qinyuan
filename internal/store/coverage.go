package store

import (
	"context"

	"qinyuan/internal/model"
)

// 类型已迁到 model（P2-4 S3）：别名保留旧名字，调用点零改动。
// 口径与语义见 model.Coverage 的注释（分母/分子/上卷都在那里）。
type KindCoverage = model.KindCoverage
type Coverage = model.Coverage

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
