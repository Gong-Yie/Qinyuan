package store

import "context"

// FindingRow 是跨 run 的发现列表项（发现页用）。
type FindingRow struct {
	ID       int64  `json:"id"`
	RunID    int64  `json:"run_id"`
	Goal     string `json:"run_goal"`
	State    string `json:"state"`
	Name     string `json:"name"`
	Severity string `json:"severity,omitempty"`
	Summary  string `json:"summary"`
	Entity   string `json:"entity,omitempty"`
	Created  int64  `json:"created_at"`
}

// ListFindings 跨 run 列发现（按 id 倒序）。
//
// findings 是 nodes(kind='finding') 的视图语义：这里直接读节点，不读视图，
// 因为还要带上 run 的目标（列表里"这条发现属于哪个任务"必须一眼可见）。
func (s *Store) ListFindings(ctx context.Context, limit int) ([]FindingRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT n.id, n.run_id, r.goal, n.state,
		       COALESCE(n.payload->>'name', n.payload->>'summary', ''),
		       COALESCE(n.payload->>'severity', ''),
		       COALESCE(n.payload->>'summary', n.payload->>'name', ''),
		       COALESCE(n.payload->>'target', n.payload->>'entity', ''),
		       EXTRACT(EPOCH FROM n.created_at)::bigint
		  FROM nodes n
		  JOIN runs r ON r.id = n.run_id
		 WHERE n.kind = 'finding'
		 ORDER BY n.id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []FindingRow
	for rows.Next() {
		var f FindingRow
		if err := rows.Scan(&f.ID, &f.RunID, &f.Goal, &f.State, &f.Name, &f.Severity, &f.Summary, &f.Entity, &f.Created); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// EntityRow 是资产页的列表项。
type EntityRow struct {
	ID          int64   `json:"id"`
	Kind        string  `json:"kind"`
	Domain      string  `json:"domain,omitempty"`
	IP          string  `json:"ip,omitempty"`
	URL         string  `json:"url,omitempty"`
	Port        int     `json:"port,omitempty"`
	ServiceName string  `json:"service_name,omitempty"`
	Title       string  `json:"page_title,omitempty"`
	Status      int     `json:"status_code,omitempty"`
	RunIDs      []int64 `json:"run_ids,omitempty"`
	LastSeen    int64   `json:"last_seen"`
}

// ListEntities 列实体（跨 run 共享的资产图），按最近可见倒序。
func (s *Store) ListEntities(ctx context.Context, limit int) ([]EntityRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, kind, COALESCE(domain,''), COALESCE(ip,''), COALESCE(url,''),
		       COALESCE(port,0), COALESCE(service_name,''), COALESCE(page_title,''),
		       COALESCE(status_code,0), COALESCE(run_ids, '{}'),
		       EXTRACT(EPOCH FROM last_seen)::bigint
		  FROM entities ORDER BY last_seen DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []EntityRow
	for rows.Next() {
		var e EntityRow
		if err := rows.Scan(&e.ID, &e.Kind, &e.Domain, &e.IP, &e.URL, &e.Port,
			&e.ServiceName, &e.Title, &e.Status, &e.RunIDs, &e.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
