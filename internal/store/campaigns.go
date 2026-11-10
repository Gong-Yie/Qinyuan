package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"qinyuan/internal/model"
)

// 本文件是《升级计划》P3-1 的数据层：战役（campaign）。
//
// 战役的价值只有三个聚合，别的都是装饰：
//  1) 总覆盖度（战役范围里被任一子 run 的 fact 锚定过的实体比例）；
//  2) 总进度（所有子 run 的目标状态聚合）；
//  3) 重复投入（同一实体被 ≥2 个子 run 测过 → 该拆分/该合并的信号）。

// Campaign 是一次战役。
type Campaign struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Goal      string    `json:"goal"`
	OrgIDs    []int64   `json:"org_ids"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"-"`
	CreatedAtSec int64  `json:"created_at"`
	Runs      int       `json:"runs"`
}

// CampaignScope 是战役级范围（子 run 继承，只许减不许加）。
type CampaignScope struct {
	Kind   string `json:"kind"`
	Domain string `json:"domain,omitempty"`
	Net    string `json:"net,omitempty"`
	OrgID  int64  `json:"org_id,omitempty"`
	Value  string `json:"value,omitempty"`
}

// CampaignRun 是战役里的一个子 run 的一行。
type CampaignRun struct {
	RunID    int64   `json:"run_id"`
	Goal     string  `json:"goal"`
	Role     string  `json:"role,omitempty"`
	Status   string  `json:"status"`
	Paused   bool    `json:"paused"`
	Facts    int     `json:"facts"`
	Findings int     `json:"findings"`
	Coverage float64 `json:"coverage_pct"`
}

// CampaignDupe 是被多个子 run 测过的实体（重复投入）。
type CampaignDupe struct {
	EntityID int64   `json:"entity_id"`
	Kind     string  `json:"kind"`
	Label    string  `json:"label"`
	Runs     []int64 `json:"runs"`
	FactIDs  []int64 `json:"fact_ids"`
}

// 视图类型已迁到 model（P2-4 S3）：别名保留旧名字，调用点零改动。
type CampaignDedupHit = model.CampaignDedupHit
type CampaignView = model.CampaignView

// CampaignSummary 是战役视图的完整聚合。
type CampaignSummary struct {
	Campaign
	Scope      []CampaignScope `json:"scope"`
	Runs       []CampaignRun   `json:"run_list"`
	Coverage   struct {
		Total  int     `json:"total"`
		Tested int     `json:"tested"`
		Pct    float64 `json:"pct"`
	} `json:"coverage"`
	Goals struct {
		Total int `json:"total"`
		Met   int `json:"met"`
	} `json:"goals"`
	Duplicates []CampaignDupe `json:"duplicates"`
}

// CreateCampaign 建战役。
func (s *Store) CreateCampaign(ctx context.Context, name, goal string, orgIDs []int64) (*Campaign, error) {
	if name == "" || goal == "" {
		return nil, errors.New("name 与 goal 必填")
	}
	if orgIDs == nil {
		orgIDs = []int64{}
	}
	var c Campaign
	var created time.Time
	err := s.pool.QueryRow(ctx, `
		INSERT INTO campaigns (name, goal, org_ids) VALUES ($1,$2,$3)
		RETURNING id, name, goal, org_ids, status, created_at`,
		name, goal, orgIDs).
		Scan(&c.ID, &c.Name, &c.Goal, &c.OrgIDs, &c.Status, &created)
	if err != nil {
		return nil, fmt.Errorf("建战役失败：%w", err)
	}
	c.CreatedAt = created
	c.CreatedAtSec = created.Unix()
	return &c, nil
}

// Campaign 读一个战役（含 run 数）。
func (s *Store) Campaign(ctx context.Context, id int64) (*Campaign, error) {
	var c Campaign
	var created time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT c.id, c.name, c.goal, c.org_ids, c.status, c.created_at,
		       (SELECT count(*) FROM runs r WHERE r.campaign_id = c.id)
		  FROM campaigns c WHERE c.id=$1`, id).
		Scan(&c.ID, &c.Name, &c.Goal, &c.OrgIDs, &c.Status, &created, &c.Runs)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	c.CreatedAt = created
	c.CreatedAtSec = created.Unix()
	return &c, nil
}

// ListCampaigns 列出战役（新的在前）。
func (s *Store) ListCampaigns(ctx context.Context, limit int) ([]*Campaign, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT c.id, c.name, c.goal, c.org_ids, c.status, c.created_at,
		       (SELECT count(*) FROM runs r WHERE r.campaign_id = c.id)
		  FROM campaigns c ORDER BY c.id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Campaign{}
	for rows.Next() {
		var c Campaign
		var created time.Time
		if err := rows.Scan(&c.ID, &c.Name, &c.Goal, &c.OrgIDs, &c.Status, &created, &c.Runs); err != nil {
			return nil, err
		}
		c.CreatedAt = created
		c.CreatedAtSec = created.Unix()
		out = append(out, &c)
	}
	return out, err
}

// SetCampaignStatus 改状态（暂停/恢复/完成/归档）。
func (s *Store) SetCampaignStatus(ctx context.Context, id int64, status string) (bool, error) {
	switch status {
	case "active", "paused", "done", "archived":
	default:
		return false, fmt.Errorf("status 只能是 active/paused/done/archived")
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE campaigns SET status=$2, updated_at=now() WHERE id=$1`, id, status)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// AddCampaignScope 加一条战役范围。
func (s *Store) AddCampaignScope(ctx context.Context, campaignID int64, sc CampaignScope) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO campaign_scope (campaign_id, kind, domain, net, org_id, value)
		VALUES ($1,$2,NULLIF($3,''),NULLIF($4,'')::cidr,NULLIF($5,0),NULLIF($6,''))
		ON CONFLICT DO NOTHING`,
		campaignID, sc.Kind, sc.Domain, sc.Net, sc.OrgID, sc.Value)
	return err
}

// CampaignScopes 读战役范围。
func (s *Store) CampaignScopes(ctx context.Context, campaignID int64) ([]CampaignScope, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT kind, COALESCE(domain,''), COALESCE(net::text,''), COALESCE(org_id,0), COALESCE(value,'')
		  FROM campaign_scope WHERE campaign_id=$1 ORDER BY id`, campaignID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CampaignScope{}
	for rows.Next() {
		var sc CampaignScope
		if err := rows.Scan(&sc.Kind, &sc.Domain, &sc.Net, &sc.OrgID, &sc.Value); err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// AttachRun 把 run 挂到战役下（幂等；campaign_id=0 表示解绑）。
//
// 刻意**不级联**子 run 的范围：战役范围只作为继承来源，run_scope 仍是每个 run 的授权真相。
func (s *Store) AttachRun(ctx context.Context, runID, campaignID int64, role string) error {
	if campaignID > 0 {
		if _, err := s.Campaign(ctx, campaignID); err != nil {
			return err
		}
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE runs SET campaign_id=NULLIF($2,0), campaign_role=$3, updated_at=now()
		 WHERE id=$1`, runID, campaignID, role)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// CampaignRuns 列子 run（带事实/发现计数与单 run 覆盖度）。
func (s *Store) CampaignRuns(ctx context.Context, campaignID int64) ([]CampaignRun, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT r.id, r.goal, r.campaign_role, COALESCE(t.status,'created'), COALESCE(t.paused,false),
		       (SELECT count(*) FROM nodes n WHERE n.run_id=r.id AND n.kind='fact' AND n.state<>'origin'),
		       (SELECT count(*) FROM nodes n WHERE n.run_id=r.id AND n.kind='finding')
		  FROM runs r LEFT JOIN run_tasks t ON t.run_id=r.id
		 WHERE r.campaign_id=$1 ORDER BY r.id`, campaignID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CampaignRun{}
	for rows.Next() {
		var cr CampaignRun
		if err := rows.Scan(&cr.RunID, &cr.Goal, &cr.Role, &cr.Status, &cr.Paused, &cr.Facts, &cr.Findings); err != nil {
			return nil, err
		}
		if cov, err := s.Coverage(ctx, cr.RunID); err == nil && cov != nil {
			cr.Coverage = cov.Pct
		}
		out = append(out, cr)
	}
	return out, rows.Err()
}

// CampaignSummary 是战役视图的唯一聚合入口（2 秒内要出，见验收）。
func (s *Store) CampaignSummary(ctx context.Context, campaignID int64) (*CampaignSummary, error) {
	c, err := s.Campaign(ctx, campaignID)
	if err != nil {
		return nil, err
	}
	out := &CampaignSummary{Campaign: *c}
	if out.Scope, err = s.CampaignScopes(ctx, campaignID); err != nil {
		return nil, err
	}
	if out.Runs, err = s.CampaignRuns(ctx, campaignID); err != nil {
		return nil, err
	}
	total, tested, err := s.campaignCoverage(ctx, campaignID, out.Scope)
	if err != nil {
		return nil, err
	}
	out.Coverage.Total, out.Coverage.Tested = total, tested
	if total > 0 {
		out.Coverage.Pct = float64(tested) * 100 / float64(total)
	}
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE n.state='met')
		  FROM nodes n JOIN runs r ON r.id=n.run_id
		 WHERE r.campaign_id=$1 AND n.kind='goal'`, campaignID).
		Scan(&out.Goals.Total, &out.Goals.Met); err != nil {
		return nil, err
	}
	if out.Duplicates, err = s.CampaignDuplicates(ctx, campaignID, 50); err != nil {
		return nil, err
	}
	return out, nil
}

// campaignCoverage 算"战役范围里有多少实体、其中多少被任一子 run 的 fact 测过"。
//
// 逐条范围求和并去重（战役规模有限，清晰比一条巨型 SQL 重要）：
// 覆盖度只认 fact 锚点（与单任务同一口径：finding/intent 都不算测过）。
func (s *Store) campaignCoverage(ctx context.Context, campaignID int64, scopes []CampaignScope) (int, int, error) {
	if len(scopes) == 0 {
		return 0, 0, nil
	}
	seen := map[int64]bool{}
	for _, sc := range scopes {
		pred, arg := "", ""
		switch sc.Kind {
		case "root_domain":
			pred, arg = `(e.domain = $2 OR e.domain LIKE '%.' || $2)`, sc.Domain
		case "subdomain":
			pred, arg = `(e.domain = $2)`, sc.Domain
		case "ip":
			pred, arg = `(e.ip = $2 OR e.domain = $2)`, firstNonEmpty(sc.Value, sc.Domain)
		case "cidr":
			pred, arg = `(e.segment <<= $2::cidr)`, sc.Net
		default:
			continue // org/icp/keyword 目前没有实体列可匹配：不虚报分母
		}
		rows, err := s.pool.Query(ctx, `
			SELECT e.id, EXISTS(
				SELECT 1 FROM anchors a
				  JOIN nodes n ON n.id=a.node_id
				  JOIN runs r ON r.id=n.run_id
				 WHERE a.entity_id=e.id AND n.kind='fact' AND n.state<>'origin'
				   AND r.campaign_id=$1)
			  FROM entities e WHERE `+pred, campaignID, arg)
		if err != nil {
			return 0, 0, err
		}
		for rows.Next() {
			var id int64
			var tested bool
			if err := rows.Scan(&id, &tested); err != nil {
				rows.Close()
				return 0, 0, err
			}
			seen[id] = seen[id] || tested
		}
		rows.Close()
	}
	total, tested := 0, 0
	for _, t := range seen {
		total++
		if t {
			tested++
		}
	}
	return total, tested, nil
}

// CampaignDuplicates 找"同一实体被 ≥2 个子 run 测过"（重复投入）。
func (s *Store) CampaignDuplicates(ctx context.Context, campaignID int64, limit int) ([]CampaignDupe, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT a.entity_id,
		       array_agg(DISTINCT n.run_id) AS runs,
		       array_agg(DISTINCT n.id)     AS facts,
		       e.kind, COALESCE(e.domain, e.ip, e.url, '')
		  FROM anchors a
		  JOIN nodes n ON n.id=a.node_id
		  JOIN runs r  ON r.id=n.run_id
		  JOIN entities e ON e.id=a.entity_id
		 WHERE n.kind='fact' AND n.state<>'origin' AND r.campaign_id=$1
		 GROUP BY a.entity_id, e.kind, e.domain, e.ip, e.url
		HAVING count(DISTINCT n.run_id) >= 2
		 ORDER BY a.entity_id DESC LIMIT $2`, campaignID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CampaignDupe{}
	for rows.Next() {
		var d CampaignDupe
		if err := rows.Scan(&d.EntityID, &d.Runs, &d.FactIDs, &d.Kind, &d.Label); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// CampaignViewForRun 给 planner/worker 注入"同战役上下文"（每轮随态势给）。
//
// 返回 nil = 这个 run 不属于任何战役（零注入，不要给"无关战役"）。
func (s *Store) CampaignViewForRun(ctx context.Context, runID int64) (*CampaignView, error) {
	run, err := s.Run(ctx, runID)
	if err != nil {
		return nil, err
	}
	if run.CampaignID == 0 {
		return nil, nil
	}
	c, err := s.Campaign(ctx, run.CampaignID)
	if err != nil {
		return nil, err
	}
	v := &CampaignView{
		ID: c.ID, Name: c.Name, Status: c.Status, Role: run.CampaignRole, Runs: c.Runs,
	}
	scopes, err := s.CampaignScopes(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	if v.Total, v.Tested, err = s.campaignCoverage(ctx, c.ID, scopes); err != nil {
		return nil, err
	}
	if v.Total > 0 {
		v.CoveragePct = float64(v.Tested) * 100 / float64(v.Total)
	}
	dupes, err := s.CampaignDuplicates(ctx, c.ID, 500)
	if err != nil {
		return nil, err
	}
	v.Duplicates = len(dupes)
	v.TestedElsewhere, err = s.campaignHits(ctx, c.ID, runID, 10)
	if err != nil {
		return nil, err
	}
	if v.Duplicates > 0 {
		v.Note = "同战役里已有实体被多个任务重复测过，派方向前先看 tested_in_other_runs"
	}
	return v, nil
}

// campaignHits 取"别的子 run 已经写过 fact 的实体"（最多 limit 条）。
func (s *Store) campaignHits(ctx context.Context, campaignID, runID int64, limit int) ([]CampaignDedupHit, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT a.entity_id, n.run_id, n.id, COALESCE(n.payload->>'summary','')
		  FROM anchors a
		  JOIN nodes n ON n.id=a.node_id
		  JOIN runs r  ON r.id=n.run_id
		 WHERE r.campaign_id=$1 AND n.kind='fact' AND n.state<>'origin' AND n.run_id<>$2
		 ORDER BY n.id DESC LIMIT $3`, campaignID, runID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CampaignDedupHit{}
	for rows.Next() {
		var h CampaignDedupHit
		if err := rows.Scan(&h.EntityID, &h.RunID, &h.FactID, &h.Summary); err != nil {
			return nil, err
		}
		h.Summary = model.TrimSummary(h.Summary, 160)
		out = append(out, h)
	}
	return out, rows.Err()
}

