package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"qinyuan/internal/model"
)

// AddCallback 登记一条回连白名单（operator 操作）。
//
// 校验在这里而不是只靠 API：TTL 上限 4 小时、端口格式、proto 枚举。
// 越界一律拒绝——白名单是"开口"，宁可多报错，也不要静默放宽。
func (s *Store) AddCallback(ctx context.Context, runID int64, host, proto, ports, note, createdBy string, ttl time.Duration) (*model.Callback, error) {
	runIDHost := strings.TrimSpace(host)
	if runIDHost == "" {
		return nil, errors.New("host 必填")
	}
	proto = strings.ToLower(strings.TrimSpace(proto))
	if proto == "" {
		proto = "tcp"
	}
	if proto != "tcp" && proto != "udp" {
		return nil, errors.New("proto 只支持 tcp / udp")
	}
	if err := validatePorts(ports); err != nil {
		return nil, err
	}
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	if ttl > 4*time.Hour {
		ttl = 4 * time.Hour
	}
	if strings.TrimSpace(createdBy) == "" {
		createdBy = "operator"
	}

	var cb model.Callback
	err := s.pool.QueryRow(ctx, `
		INSERT INTO run_callbacks (run_id, host, proto, ports, note, created_by, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6, now() + $7::interval)
		RETURNING id, run_id, host, proto, ports, note, created_by, created_at, expires_at, revoked_at`,
		runID, runIDHost, proto, strings.TrimSpace(ports), note, createdBy,
		fmt.Sprintf("%d seconds", int(ttl.Seconds()))).
		Scan(&cb.ID, &cb.RunID, &cb.Host, &cb.Proto, &cb.Ports, &cb.Note,
			&cb.CreatedBy, &cb.CreatedAt, &cb.ExpiresAt, &cb.RevokedAt)
	if err != nil {
		return nil, err
	}
	return &cb, nil
}

// Callbacks 列出本任务的全部白名单（含已过期/已吊销，供审计）。
func (s *Store) Callbacks(ctx context.Context, runID int64) ([]*model.Callback, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, run_id, host, proto, ports, note, created_by, created_at, expires_at, revoked_at
		  FROM run_callbacks WHERE run_id=$1 ORDER BY id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Callback
	for rows.Next() {
		var cb model.Callback
		if err := rows.Scan(&cb.ID, &cb.RunID, &cb.Host, &cb.Proto, &cb.Ports, &cb.Note,
			&cb.CreatedBy, &cb.CreatedAt, &cb.ExpiresAt, &cb.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, &cb)
	}
	return out, rows.Err()
}

// ActiveCallbacks 只返回"当前生效"的白名单（未吊销、未过期）。
func (s *Store) ActiveCallbacks(ctx context.Context, runID int64) ([]*model.Callback, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, run_id, host, proto, ports, note, created_by, created_at, expires_at, revoked_at
		  FROM run_callbacks
		 WHERE run_id=$1 AND revoked_at IS NULL AND expires_at > now()
		 ORDER BY id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Callback
	for rows.Next() {
		var cb model.Callback
		if err := rows.Scan(&cb.ID, &cb.RunID, &cb.Host, &cb.Proto, &cb.Ports, &cb.Note,
			&cb.CreatedBy, &cb.CreatedAt, &cb.ExpiresAt, &cb.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, &cb)
	}
	return out, rows.Err()
}

// RevokeCallback 吊销一条白名单（幂等：已吊销返回 false）。
func (s *Store) RevokeCallback(ctx context.Context, runID, id int64) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE run_callbacks SET revoked_at=now()
		 WHERE run_id=$1 AND id=$2 AND revoked_at IS NULL`, runID, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// CallbackByID 读一条（跨 run 一律当作不存在）。
func (s *Store) CallbackByID(ctx context.Context, runID, id int64) (*model.Callback, error) {
	var cb model.Callback
	err := s.pool.QueryRow(ctx, `
		SELECT id, run_id, host, proto, ports, note, created_by, created_at, expires_at, revoked_at
		  FROM run_callbacks WHERE run_id=$1 AND id=$2`, runID, id).
		Scan(&cb.ID, &cb.RunID, &cb.Host, &cb.Proto, &cb.Ports, &cb.Note,
			&cb.CreatedBy, &cb.CreatedAt, &cb.ExpiresAt, &cb.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &cb, nil
}

// validatePorts 校验 "4444" / "40000-40100"。
func validatePorts(ports string) error {
	ports = strings.TrimSpace(ports)
	parts := strings.SplitN(ports, "-", 2)
	parse := func(s string) (int, error) {
		// 必须整段都是数字：Sscanf("%d") 会接受 "4444abc"（只读前缀），
		// 于是 API 创建成功、sandbox.ParsePorts 却拒绝，白名单静默不生效。
		return strconv.Atoi(strings.TrimSpace(s))
	}
	low, err := parse(parts[0])
	if err != nil || low < 1 || low > 65535 {
		return errors.New("ports 不合法：应为 4444 或 40000-40100")
	}
	if len(parts) == 2 {
		high, err := parse(parts[1])
		if err != nil || high < low || high > 65535 {
			return errors.New("ports 不合法：区间上界必须 ≥ 下界且 ≤ 65535")
		}
	}
	return nil
}
