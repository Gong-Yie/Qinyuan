// Package store 是唯一的数据访问层：pgx + PostgreSQL。
//
// 硬规则（来自实现规格/README 的三条铁律）：
//  1. 所有状态迁移都是 CAS（带 expected），禁止读改写；
//  2. 去重交给数据库的部分唯一索引，应用层只做归一化；
//  3. activity 只追加，不修改。
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// 领域错误。上层用 errors.Is 判断，不要靠字符串匹配。
var (
	ErrNotFound         = errors.New("store: not found")
	ErrStateConflict    = errors.New("store: state conflict")
	ErrParentNotAllowed = errors.New("store: parent must be fact or finding")
	ErrCrossRun         = errors.New("store: node belongs to another run")
)

// Store 持有连接池。
type Store struct {
	pool *pgxpool.Pool
	dsn  string
}

// Open 建连接池；不会自动迁移（调用方显式 Migrate）。
func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("解析 DSN 失败：%w", err)
	}
	cfg.MaxConns = 10
	cfg.MaxConnLifetime = time.Hour
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("连接池创建失败：%w", err)
	}
	s := &Store{pool: pool, dsn: dsn}
	if err := s.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

// Close 关闭连接池。
func (s *Store) Close() {
	if s != nil && s.pool != nil {
		s.pool.Close()
	}
}

// Ping 检查库是否可用。
func (s *Store) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := s.pool.Ping(ctx); err != nil {
		return fmt.Errorf("数据库不可用（%s）：%w", redactDSN(s.dsn), err)
	}
	return nil
}

// Pool 暴露连接池给测试与迁移使用（业务代码不要用）。
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// querier 让同一份 SQL 既能在事务里跑也能在池上跑。
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// 只摘掉密码，别把整个 DSN 吞掉（排障时要看 host/port/db）。
func redactDSN(dsn string) string {
	at := -1
	for i := len(dsn) - 1; i >= 0; i-- {
		if dsn[i] == '@' {
			at = i
			break
		}
	}
	if at < 0 {
		return dsn
	}
	schemeEnd := 0
	for i := 0; i+2 < len(dsn); i++ {
		if dsn[i] == ':' && dsn[i+1] == '/' && dsn[i+2] == '/' {
			schemeEnd = i + 3
			break
		}
	}
	if schemeEnd == 0 || schemeEnd > at {
		return dsn
	}
	cred := dsn[schemeEnd:at]
	for i := 0; i < len(cred); i++ {
		if cred[i] == ':' {
			cred = cred[:i] + ":***"
			break
		}
	}
	return dsn[:schemeEnd] + cred + dsn[at:]
}
