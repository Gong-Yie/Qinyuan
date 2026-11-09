package store

import (
	"context"
	_ "embed"
	"fmt"

	"github.com/jackc/pgx/v5"
)

//go:embed schema.sql
var schemaSQL string

// migrationLockKey 是 pg_advisory_lock 的键：多实例同时启动时只允许一个跑 DDL。
const migrationLockKey int64 = 0x71_69_6e_79_75_61_6e // "qinyuan" 的十六进制近似

// Migrate 幂等迁移：每次启动全量执行 schema.sql。
//
// 为什么可以全量执行：schema.sql 里每一句都写成 IF NOT EXISTS / CREATE OR REPLACE /
// DO $$ 检测后重建。代价是第一次读 schema 的人要习惯这种风格。
func (s *Store) Migrate(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("获取迁移连接失败：%w", err)
	}
	defer conn.Release()

	// advisory lock 必须钉在同一条连接上（连接池会把语句路由到别的连接）。
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return fmt.Errorf("获取迁移锁失败：%w", err)
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrationLockKey)
	}()

	// 用简单的多语句协议执行：schema.sql 自身是一个隐式事务，中途失败即整体回滚。
	// pgx 的扩展协议不允许一次多语句，所以这里走 PgConn().Exec。
	if _, err := conn.Conn().PgConn().Exec(ctx, schemaSQL).ReadAll(); err != nil {
		return fmt.Errorf("执行 schema 失败：%w", err)
	}
	return nil
}

// migrateInSchema 在指定 schema 内跑迁移（测试隔离用）。
func migrateInSchema(ctx context.Context, conn *pgx.Conn, schema string) error {
	if _, err := conn.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS `+pgx.Identifier{schema}.Sanitize()); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, `SET search_path TO `+pgx.Identifier{schema}.Sanitize()); err != nil {
		return err
	}
	if _, err := conn.PgConn().Exec(ctx, schemaSQL).ReadAll(); err != nil {
		return err
	}
	return nil
}
