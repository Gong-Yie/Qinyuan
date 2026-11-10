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
// 语义（实现规格/01 §6.4）：`tested` **只认 fact 的锚点** ——
// finding 是结论不是覆盖，intent 更不算；origin fact 是任务起点，也不算"测过"。
type Coverage struct {
	Total  int            `json:"total"`
	Tested int            `json:"tested"`
	Pct    float64        `json:"pct"`
	ByKind []KindCoverage `json:"by_kind"`
}

// Coverage 计算某个 run 的覆盖度。
func (s *Store) Coverage(ctx context.Context, runID int64) (*Coverage, error) {
	rows, err := s.pool.Query(ctx, `
		WITH target AS (
		  SELECT DISTINCT e.id, e.kind
		    FROM entities e
		    JOIN run_scope sc ON sc.run_id = $1 AND (
		         (sc.kind = 'root_domain' AND e.root_domain = sc.domain) OR
		         (sc.kind = 'subdomain'   AND e.domain = sc.domain) OR
		         -- IP 目标：runplan 为 "ip 字面量" 写的是 value（不是 net），
		         -- 早先只按 sc.net 匹配，导致 IP 任务的覆盖度永远是 0/0。
		         (sc.kind = 'ip'   AND e.ip <> '' AND e.ip = sc.value) OR
		         (sc.kind = 'cidr' AND sc.net >>= try_inet(e.ip)) OR
		         (sc.kind = 'org' AND e.owner_org_id = sc.org_id)
		    )
		), tested AS (
		  SELECT DISTINCT a.entity_id
		    FROM anchors a
		    JOIN nodes n ON n.id = a.node_id
		   WHERE n.run_id = $1 AND n.kind = 'fact' AND n.state <> 'origin'
		)
		SELECT t.kind, count(*) AS total,
		       count(*) FILTER (WHERE t.id IN (SELECT entity_id FROM tested)) AS tested
		  FROM target t GROUP BY t.kind ORDER BY t.kind`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cov := &Coverage{}
	for rows.Next() {
		var kc KindCoverage
		if err := rows.Scan(&kc.Kind, &kc.Total, &kc.Tested); err != nil {
			return nil, err
		}
		cov.Total += kc.Total
		cov.Tested += kc.Tested
		cov.ByKind = append(cov.ByKind, kc)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if cov.Total > 0 {
		cov.Pct = float64(cov.Tested) / float64(cov.Total) * 100
	}
	return cov, nil
}
