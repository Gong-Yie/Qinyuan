package store

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
)

// GraphNode 是链路图的节点（含 payload，前端需要按 kind 取摘要/状态）。
type GraphNode struct {
	ID       int64           `json:"id"`
	Kind     string          `json:"kind"`
	State    string          `json:"state"`
	Priority int             `json:"priority"`
	Summary  string          `json:"summary"`
	Payload  json.RawMessage `json:"payload,omitempty"`
}

// GraphEdge 是链路图的边（src → dst 表示"从 src 指向 dst"）。
type GraphEdge struct {
	Src int64  `json:"src"`
	Dst int64  `json:"dst"`
	Rel string `json:"rel"`
}

// Graph 取一个 run 的完整链路图。
//
// 一次取全（几十到几百节点）：布局在前端算，后端不做遍历 —— 前端要做
// 断环/分层/重心法排序，它需要完整的边集。
func (s *Store) Graph(ctx context.Context, runID int64) ([]GraphNode, []GraphEdge, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, kind, state, priority,
		       COALESCE(
		         NULLIF(left(payload->>'summary', 160), ''),
		         NULLIF(left(payload->>'text', 160), ''),
		         NULLIF(left(payload->>'title', 160), ''),
		         NULLIF(left(payload->>'body', 160), ''),
		         ''
		       ),
		       payload
		  FROM nodes WHERE run_id=$1 ORDER BY id ASC`, runID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var nodes []GraphNode
	for rows.Next() {
		var n GraphNode
		if err := rows.Scan(&n.ID, &n.Kind, &n.State, &n.Priority, &n.Summary, &n.Payload); err != nil {
			return nil, nil, err
		}
		nodes = append(nodes, n)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	erows, err := s.pool.Query(ctx,
		`SELECT src_id, dst_id, rel FROM edges WHERE run_id=$1 ORDER BY src_id, dst_id`, runID)
	if err != nil {
		return nil, nil, err
	}
	defer erows.Close()
	var edges []GraphEdge
	for erows.Next() {
		var e GraphEdge
		if err := erows.Scan(&e.Src, &e.Dst, &e.Rel); err != nil {
			return nil, nil, err
		}
		edges = append(edges, e)
	}
	return nodes, edges, erows.Err()
}

// CovNode 是资产覆盖图的节点。Key 是前端用来连边的稳定标识（a:<id>）。
type CovNode struct {
	Key      string `json:"key"`
	EntityID int64  `json:"entity_id,omitempty"`
	Kind     string `json:"kind"`
	Label    string `json:"label"`
	Domain   string `json:"domain,omitempty"`
	IP       string `json:"ip,omitempty"`
	URL      string `json:"url,omitempty"`
	Port     int    `json:"port,omitempty"`
	Title    string `json:"page_title,omitempty"`
	Status   int    `json:"status_code,omitempty"`
	Tested   bool   `json:"tested"`
	InScope  bool   `json:"in_scope"`
}

// CovEdge 是覆盖图的边：src(子) → dst(父)。
type CovEdge struct {
	Src string `json:"src"`
	Dst string `json:"dst"`
}

// CoverageGraph 返回覆盖图：范围内实体 + 已测标记 + 父子关系。
//
// 父子关系没有存 parent_id（与 ARTEX 一样，靠列冗余派生）：
// endpoint → service（同 host:port），service/endpoint → 子域或 IP，子域 → 根域。
// 只连"父子都在范围内"的边，避免把无关资产拉进图里。
func (s *Store) CoverageGraph(ctx context.Context, runID int64) ([]CovNode, []CovEdge, error) {
	rows, err := s.pool.Query(ctx, `
		WITH target AS (
		  SELECT DISTINCT e.*
		    FROM entities e
		    JOIN run_scope sc ON sc.run_id = $1 AND (
		         (sc.kind = 'root_domain' AND e.root_domain = sc.domain) OR
		         (sc.kind = 'subdomain'   AND e.domain = sc.domain) OR
		         (sc.kind = 'ip'          AND e.ip <> '' AND e.ip = sc.value) OR
		         (sc.kind = 'cidr'        AND sc.net >>= try_inet(e.ip)) OR
		         (sc.kind = 'org'         AND e.owner_org_id = sc.org_id)
		    )
		), related AS (
		  -- 与范围内实体同主机/同根域的资产：它们不在授权范围里（in_scope=false），
		  -- 但"打到哪了"必须看得见，否则覆盖图只有一个孤零零的 IP 节点。
		  SELECT DISTINCT e.*
		    FROM entities e
		    JOIN target t ON (
		         (e.ip <> '' AND e.ip = t.ip) OR
		         (e.domain <> '' AND t.domain <> '' AND e.domain = t.domain) OR
		         (e.root_domain <> '' AND t.root_domain <> '' AND e.root_domain = t.root_domain) OR
		         (e.url <> '' AND t.ip <> '' AND
		            split_part(split_part(split_part(e.url,'://',2),'/',1),':',1) = t.ip) OR
		         (e.url <> '' AND t.domain <> '' AND
		            split_part(split_part(split_part(e.url,'://',2),'/',1),':',1) = t.domain)
		    )
		), tested AS (
		  SELECT DISTINCT a.entity_id
		    FROM anchors a JOIN nodes n ON n.id = a.node_id
		   WHERE n.run_id = $1 AND n.kind='fact' AND n.state <> 'origin'
		)
		SELECT id, kind, COALESCE(domain,''), COALESCE(ip,''), COALESCE(url,''),
		       COALESCE(port,0), COALESCE(page_title,''), COALESCE(status_code,0),
		       id IN (SELECT entity_id FROM tested),
		       COALESCE(root_domain,''), COALESCE(service_type,''),
		       id IN (SELECT id FROM target)
		  FROM (SELECT * FROM target UNION SELECT * FROM related) u
		 ORDER BY id`, runID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	type ent struct {
		node                    CovNode
		rootDomain, serviceType string
	}
	var ents []ent
	byDomain := map[string]string{}
	byIP := map[string]string{}
	for rows.Next() {
		var e ent
		var root, svc string
		if err := rows.Scan(&e.node.EntityID, &e.node.Kind, &e.node.Domain, &e.node.IP, &e.node.URL,
			&e.node.Port, &e.node.Title, &e.node.Status, &e.node.Tested, &root, &svc, &e.node.InScope); err != nil {
			return nil, nil, err
		}
		e.rootDomain, e.serviceType = root, svc
		e.node.Key = "a:" + itoa64(e.node.EntityID)
		e.node.Label = covLabel(e.node)
		ents = append(ents, e)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	// 建索引：域名 → key，IP → key，host:port → key（用于 endpoint→service）。
	for _, e := range ents {
		switch e.node.Kind {
		case "root_domain":
			byDomain[e.node.Domain] = e.node.Key
		case "subdomain":
			byDomain[e.node.Domain] = e.node.Key
		case "ip":
			byIP[e.node.IP] = e.node.Key
		}
	}
	hostPort := map[string]string{}
	for _, e := range ents {
		if e.node.Kind == "service" && e.serviceType == "http" && e.node.URL != "" {
			if auth := urlAuthority(e.node.URL); auth != "" {
				hostPort[auth] = e.node.Key
			}
		}
	}

	var nodes []CovNode
	var edges []CovEdge
	for _, e := range ents {
		nodes = append(nodes, e.node)
		parent := ""
		switch e.node.Kind {
		case "subdomain":
			parent = byDomain[e.rootDomain]
		case "service", "endpoint":
			if auth := urlAuthority(e.node.URL); auth != "" {
				parent = hostPort[auth]
			}
			if parent == "" {
				// 按 URL 主机挂：host 是 IP 挂 ip 节点，否则挂域名节点；
				// 再退回实体自身的 domain/ip 列（有些实体没有 url）。
				if host := urlHost(e.node.URL); host != "" {
					parent = byDomain[host]
					if parent == "" {
						parent = byIP[host]
					}
				}
			}
			if parent == "" && e.node.Domain != "" {
				parent = byDomain[e.node.Domain]
			}
			if parent == "" && e.node.IP != "" {
				parent = byIP[e.node.IP]
			}
		}
		if parent != "" && parent != e.node.Key {
			edges = append(edges, CovEdge{Src: e.node.Key, Dst: parent})
		}
	}

	if nodes == nil {
		nodes = []CovNode{}
	}
	if edges == nil {
		edges = []CovEdge{}
	}
	return nodes, edges, nil
}

func covLabel(n CovNode) string {
	if n.URL != "" {
		return n.URL
	}
	if n.Domain != "" {
		return n.Domain
	}
	if n.IP != "" && n.Port > 0 {
		return n.IP + ":" + itoa64(int64(n.Port))
	}
	if n.IP != "" {
		return n.IP
	}
	return n.Kind
}

// urlAuthority 取 scheme://host[:port]（去掉 path/query），用于 endpoint→service 的父子推断。
func urlAuthority(raw string) string {
	rest := ""
	switch {
	case strings.HasPrefix(raw, "https://"):
		rest = raw[len("https://"):]
	case strings.HasPrefix(raw, "http://"):
		rest = raw[len("http://"):]
	default:
		return ""
	}
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

func itoa64(v int64) string { return strconv.FormatInt(v, 10) }

// urlHost 取 URL 的 host（不含端口），用于把 service/endpoint 挂到 ip 或域名节点下。
func urlHost(raw string) string {
	auth := urlAuthority(raw)
	if auth == "" {
		return ""
	}
	if i := strings.LastIndex(auth, ":"); i > 0 && !strings.Contains(auth[i:], "]") {
		return auth[:i]
	}
	return auth
}
