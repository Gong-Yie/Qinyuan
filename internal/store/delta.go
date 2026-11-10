package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"qinyuan/internal/model"
)

// 本文件是《升级计划》P1-2（差分态势）的数据层。
//
// 做法：**不建事件表**，给 nodes/edges 各加一个 rev 列，写入时打上"当时的版本号"。
// 于是"自版本 V 以来的变化"就是 `rev > V` 的行集合：
//
//	runs.version 单调分配（每次写 bump 一次） → 版本即顺序
//	行上的 rev    = 它最后一次被写的版本     → 行即最终态
//
// 好处是不会出现"事件流与图不一致"（事件本身就是行），也不需要重放：
// 客户端把增量行按 id 覆盖到自己的快照上，结果与全量读取**逐字段相等**（有属性测试钉住）。
//
// 唯一需要纪律的地方：**所有写路径都必须打 rev**，否则那条变化会从增量里消失。
// 覆盖清单见 docs/03-M3.5-目标拆解与差分态势.md §3。

// Delta 是一次增量读取的结果（窗口是 (since, To]）。
type Delta struct {
	From int64
	To   int64

	Nodes []*model.Node
	Edges []model.Edge

	// Truncated=true 表示窗口内变化太多（超过调用方给的上限），
	// Nodes/Edges 为空 —— 调用方应当退回全量快照。
	Truncated bool
	Total     int
}

// Empty 这个窗口没有任何变化。
func (d *Delta) Empty() bool {
	return d == nil || (len(d.Nodes) == 0 && len(d.Edges) == 0)
}

// bumpVersion 分配下一个版本号（runs.version 单调递增）并返回它。
//
// 分配必须与业务写入在**同一个事务**里：否则会出现"版本涨了但行没写"或
// "行写了但版本没涨"，两种都会让增量丢事件。
func bumpVersion(ctx context.Context, q querier, runID int64) (int64, error) {
	var rev int64
	err := q.QueryRow(ctx,
		`UPDATE runs SET version = version + 1 WHERE id=$1 RETURNING version`, runID).Scan(&rev)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: run #%d", ErrNotFound, runID)
	}
	if err != nil {
		return 0, fmt.Errorf("分配版本号失败：%w", err)
	}
	return rev, nil
}

// stampNodes 把若干节点标记为"在 rev 这个版本被写过"。
func stampNodes(ctx context.Context, q querier, runID, rev int64, ids ...int64) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := q.Exec(ctx,
		`UPDATE nodes SET rev=$3 WHERE run_id=$1 AND id = ANY($2)`, runID, ids, rev); err != nil {
		return fmt.Errorf("打版本戳失败：%w", err)
	}
	return nil
}

// stampEdge 给一条边打版本戳（边的主键是 (run_id, src, rel, dst)）。
func stampEdge(ctx context.Context, q querier, runID, rev, src int64, rel string, dst int64) error {
	if _, err := q.Exec(ctx, `
		UPDATE edges SET rev=$5
		 WHERE run_id=$1 AND src_id=$2 AND rel=$3 AND dst_id=$4`,
		runID, src, rel, dst, rev); err != nil {
		return fmt.Errorf("给边打版本戳失败：%w", err)
	}
	return nil
}

// DeltaSince 读 (since, to] 窗口内的变化。to 是读取那一刻的 runs.version，
// 于是"读到一半又有新写入"不会污染本窗口（下一次增量会补上）。
//
// limit 是节点 + 边的总条数上限：超过就返回 Truncated（调用方退全量），
// 避免模型上下文被一次刷屏式变更灌爆。
func (s *Store) DeltaSince(ctx context.Context, runID, since int64, limit int) (*Delta, error) {
	if limit <= 0 {
		limit = 200
	}
	if since < 0 {
		since = 0
	}

	var to int64
	err := s.pool.QueryRow(ctx, `SELECT version FROM runs WHERE id=$1`, runID).Scan(&to)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	d := &Delta{From: since, To: to}
	if to <= since {
		return d, nil
	}

	var total int
	if err := s.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM nodes WHERE run_id=$1 AND rev > $2 AND rev <= $3)
		     + (SELECT count(*) FROM edges WHERE run_id=$1 AND rev > $2 AND rev <= $3)`,
		runID, since, to).Scan(&total); err != nil {
		return nil, err
	}
	d.Total = total
	if total > limit {
		d.Truncated = true
		return d, nil
	}

	rows, err := s.pool.Query(ctx, `
		SELECT `+nodeColumns+` FROM nodes
		 WHERE run_id=$1 AND rev > $2 AND rev <= $3
		 ORDER BY rev, id`, runID, since, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		d.Nodes = append(d.Nodes, n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	erows, err := s.pool.Query(ctx, `
		SELECT run_id, src_id, rel, dst_id, rev FROM edges
		 WHERE run_id=$1 AND rev > $2 AND rev <= $3
		 ORDER BY rev, src_id, dst_id`, runID, since, to)
	if err != nil {
		return nil, err
	}
	defer erows.Close()
	for erows.Next() {
		var e model.Edge
		if err := erows.Scan(&e.RunID, &e.SrcID, &e.Rel, &e.DstID, &e.Rev); err != nil {
			return nil, err
		}
		d.Edges = append(d.Edges, e)
	}
	return d, erows.Err()
}
