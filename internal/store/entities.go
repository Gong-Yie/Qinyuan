package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"qinyuan/internal/model"
)

// EntityInput 是"已归一化"的实体（归一化在 internal/domain 里做）。
type EntityInput struct {
	Kind          string
	Domain        string
	RootDomain    string
	RecordType    string
	IP            string
	Segment       string
	Port          int
	URL           string
	Method        string
	ServiceType   string
	ServiceName   string
	StatusCode    int
	ContentLength int64
	PageTitle     string
	Technologies  []string
}

// UpsertEntity 幂等写入实体（去重交给部分唯一索引）。
func (s *Store) UpsertEntity(ctx context.Context, runID int64, in EntityInput) (int64, bool, error) {
	return upsertEntity(ctx, s.pool, runID, in)
}

func upsertEntity(ctx context.Context, q querier, runID int64, in EntityInput) (int64, bool, error) {
	if runID <= 0 {
		return 0, false, errors.New("run_id 必填（实体要记录是哪个 run 见到它的）")
	}
	switch in.Kind {
	case "root_domain":
		if in.Domain == "" {
			return 0, false, errors.New("root_domain 需要 domain")
		}
		return scanUpsert(q.QueryRow(ctx, `
			INSERT INTO entities (kind, domain, root_domain, run_ids)
			VALUES ('root_domain', $1, $2, ARRAY[$3]::bigint[])
			ON CONFLICT (domain) WHERE kind='root_domain'
			DO UPDATE SET last_seen = now(),
				run_ids = (SELECT ARRAY(SELECT DISTINCT unnest(entities.run_ids || EXCLUDED.run_ids)))
			RETURNING id, (xmax = 0)`,
			in.Domain, firstNonEmpty(in.RootDomain, in.Domain), runID))

	case "ip":
		if in.IP == "" {
			return 0, false, errors.New("ip 需要 ip 字面量")
		}
		return scanUpsert(q.QueryRow(ctx, `
			INSERT INTO entities (kind, ip, segment, run_ids)
			VALUES ('ip', $1, NULLIF($2,'')::cidr, ARRAY[$3]::bigint[])
			ON CONFLICT (ip) WHERE kind='ip'
			DO UPDATE SET last_seen = now(),
				segment = COALESCE(EXCLUDED.segment, entities.segment),
				run_ids = (SELECT ARRAY(SELECT DISTINCT unnest(entities.run_ids || EXCLUDED.run_ids)))
			RETURNING id, (xmax = 0)`,
			in.IP, in.Segment, runID))

	case "subdomain":
		if in.Domain == "" {
			return 0, false, errors.New("subdomain 需要 domain")
		}
		return scanUpsert(q.QueryRow(ctx, `
			INSERT INTO entities (kind, domain, root_domain, record_type, ip, segment, run_ids)
			VALUES ('subdomain', $1, $2, NULLIF($3,''), NULLIF($4,''), NULLIF($5,'')::cidr, ARRAY[$6]::bigint[])
			ON CONFLICT (domain, (COALESCE(record_type, ''))) WHERE kind='subdomain'
			DO UPDATE SET last_seen = now(),
				root_domain = COALESCE(EXCLUDED.root_domain, entities.root_domain),
				ip = COALESCE(EXCLUDED.ip, entities.ip),
				segment = COALESCE(EXCLUDED.segment, entities.segment),
				run_ids = (SELECT ARRAY(SELECT DISTINCT unnest(entities.run_ids || EXCLUDED.run_ids)))
			RETURNING id, (xmax = 0)`,
			in.Domain, firstNonEmpty(in.RootDomain, in.Domain), in.RecordType, in.IP, in.Segment, runID))

	case "service":
		if in.ServiceType == "other" {
			return scanUpsert(q.QueryRow(ctx, `
				INSERT INTO entities (kind, domain, root_domain, ip, port, url, service_type, service_name, run_ids)
				VALUES ('service', NULLIF($1,''), NULLIF($2,''), NULLIF($3,''), NULLIF($4,0), NULLIF($5,''), 'other', NULLIF($6,''), ARRAY[$7]::bigint[])
				ON CONFLICT ((COALESCE(domain, '')), (COALESCE(ip, '')), port, service_name)
					WHERE kind='service' AND service_type='other'
				DO UPDATE SET last_seen = now(),
					url = COALESCE(EXCLUDED.url, entities.url),
					run_ids = (SELECT ARRAY(SELECT DISTINCT unnest(entities.run_ids || EXCLUDED.run_ids)))
				RETURNING id, (xmax = 0)`,
				in.Domain, in.RootDomain, in.IP, in.Port, in.URL, in.ServiceName, runID))
		}
		if in.URL == "" {
			return 0, false, errors.New("http service 需要 url")
		}
		return scanUpsert(q.QueryRow(ctx, `
			INSERT INTO entities (kind, domain, root_domain, ip, port, url, service_type, service_name,
				status_code, content_length, page_title, technologies, run_ids)
			VALUES ('service', NULLIF($1,''), NULLIF($2,''), NULLIF($3,''), NULLIF($4,0), $5, 'http', NULLIF($6,''),
				NULLIF($7,0), NULLIF($8,0), NULLIF($9,''), COALESCE($10::text[],'{}'), ARRAY[$11]::bigint[])
			ON CONFLICT (url) WHERE kind='service' AND service_type='http'
			DO UPDATE SET last_seen = now(),
				status_code = COALESCE(EXCLUDED.status_code, entities.status_code),
				content_length = COALESCE(EXCLUDED.content_length, entities.content_length),
				page_title = COALESCE(EXCLUDED.page_title, entities.page_title),
				technologies = (SELECT ARRAY(SELECT DISTINCT unnest(entities.technologies || EXCLUDED.technologies))),
				run_ids = (SELECT ARRAY(SELECT DISTINCT unnest(entities.run_ids || EXCLUDED.run_ids)))
			RETURNING id, (xmax = 0)`,
			in.Domain, in.RootDomain, in.IP, in.Port, in.URL, in.ServiceName,
			in.StatusCode, in.ContentLength, in.PageTitle, in.Technologies, runID))

	case "endpoint":
		if in.URL == "" || in.Method == "" {
			return 0, false, errors.New("endpoint 需要 url 与 method")
		}
		return scanUpsert(q.QueryRow(ctx, `
			INSERT INTO entities (kind, url, method, domain, root_domain, status_code, content_length, page_title, run_ids)
			VALUES ('endpoint', $1, $2, NULLIF($3,''), NULLIF($4,''), NULLIF($5,0), NULLIF($6,0), NULLIF($7,''), ARRAY[$8]::bigint[])
			ON CONFLICT (url, method) WHERE kind='endpoint'
			DO UPDATE SET last_seen = now(),
				status_code = COALESCE(EXCLUDED.status_code, entities.status_code),
				content_length = COALESCE(EXCLUDED.content_length, entities.content_length),
				page_title = COALESCE(EXCLUDED.page_title, entities.page_title),
				run_ids = (SELECT ARRAY(SELECT DISTINCT unnest(entities.run_ids || EXCLUDED.run_ids)))
			RETURNING id, (xmax = 0)`,
			in.URL, in.Method, in.Domain, in.RootDomain, in.StatusCode, in.ContentLength, in.PageTitle, runID))

	default:
		return 0, false, fmt.Errorf("未知实体类型 %q（允许 root_domain/ip/subdomain/service/endpoint）", in.Kind)
	}
}

func scanUpsert(row pgx.Row) (int64, bool, error) {
	var id int64
	var inserted bool
	if err := row.Scan(&id, &inserted); err != nil {
		return 0, false, err
	}
	return id, inserted, nil
}

// Anchor 把节点锚到实体（重复忽略）。
func (s *Store) Anchor(ctx context.Context, nodeID, entityID int64) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO anchors (node_id, entity_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, nodeID, entityID)
	return err
}

// Entity 读一个实体。
func (s *Store) Entity(ctx context.Context, id int64) (*model.Entity, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, kind, COALESCE(domain,''), COALESCE(root_domain,''), COALESCE(record_type,''),
		       COALESCE(ip,''), COALESCE(port,0), COALESCE(url,''), COALESCE(method,''),
		       COALESCE(service_type,''), COALESCE(service_name,''), COALESCE(status_code,0),
		       COALESCE(page_title,''), run_ids
		  FROM entities WHERE id=$1`, id)
	var e model.Entity
	err := row.Scan(&e.ID, &e.Kind, &e.Domain, &e.RootDomain, &e.RecordType,
		&e.IP, &e.Port, &e.URL, &e.Method, &e.ServiceType, &e.ServiceName, &e.StatusCode, &e.Title, &e.RunIDs)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &e, err
}

// ListEntitiesForRun 取某个 run 见过的实体。
func (s *Store) ListEntitiesForRun(ctx context.Context, runID int64, limit int) ([]*model.Entity, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, kind, COALESCE(domain,''), COALESCE(root_domain,''), COALESCE(record_type,''),
		       COALESCE(ip,''), COALESCE(port,0), COALESCE(url,''), COALESCE(method,''),
		       COALESCE(service_type,''), COALESCE(service_name,''), COALESCE(status_code,0),
		       COALESCE(page_title,''), run_ids
		  FROM entities WHERE $1 = ANY(run_ids) ORDER BY id DESC LIMIT $2`, runID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Entity
	for rows.Next() {
		var e model.Entity
		if err := rows.Scan(&e.ID, &e.Kind, &e.Domain, &e.RootDomain, &e.RecordType,
			&e.IP, &e.Port, &e.URL, &e.Method, &e.ServiceType, &e.ServiceName, &e.StatusCode, &e.Title, &e.RunIDs); err != nil {
			return nil, err
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

// EntitiesByNode 取一个节点锚定的实体。
func (s *Store) EntitiesByNode(ctx context.Context, nodeID int64) ([]*model.Entity, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT e.id, e.kind, COALESCE(e.domain,''), COALESCE(e.root_domain,''), COALESCE(e.record_type,''),
		       COALESCE(e.ip,''), COALESCE(e.port,0), COALESCE(e.url,''), COALESCE(e.method,''),
		       COALESCE(e.service_type,''), COALESCE(e.service_name,''), COALESCE(e.status_code,0),
		       COALESCE(e.page_title,''), e.run_ids
		  FROM entities e JOIN anchors a ON a.entity_id = e.id
		 WHERE a.node_id = $1 ORDER BY e.id`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Entity
	for rows.Next() {
		var e model.Entity
		if err := rows.Scan(&e.ID, &e.Kind, &e.Domain, &e.RootDomain, &e.RecordType,
			&e.IP, &e.Port, &e.URL, &e.Method, &e.ServiceType, &e.ServiceName, &e.StatusCode, &e.Title, &e.RunIDs); err != nil {
			return nil, err
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
