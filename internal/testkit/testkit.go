// Package testkit 提供测试用的隔离库与夹具。
//
// 它只被 *_test.go 引用（放在普通包是为了让多个测试包共享同一份实现）。
package testkit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

	"qinyuan/internal/model"
	"qinyuan/internal/store"
)

// DSN 返回测试库连接串（默认本地便携实例）。
func DSN() string {
	if v := os.Getenv("QINYUAN_TEST_DSN"); v != "" {
		return v
	}
	return "postgres://postgres@127.0.0.1:55432/qinyuan_test?sslmode=disable"
}

// Store 每个测试一个独立 schema，跑真实迁移，结束 drop。
func Store(t *testing.T) *store.Store {
	t.Helper()
	dsn := DSN()
	ctx := context.Background()

	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("测试库不可用（%v）；请先起 scripts/devdb.ps1 serve", err)
	}
	defer admin.Close(ctx)

	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	schema := "tk_" + hex.EncodeToString(b)
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatalf("建 schema 失败：%v", err)
	}

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()

	s, err := store.Open(ctx, u.String())
	if err != nil {
		t.Fatalf("打开测试库失败：%v", err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("测试库迁移失败：%v", err)
	}
	t.Cleanup(func() {
		s.Close()
		cleanup, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			return
		}
		defer cleanup.Close(context.Background())
		_, _ = cleanup.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	})
	return s
}

// Run 建一个带范围与种子实体的测试 run，返回 run 与第一个实体 id。
func Run(t *testing.T, s *store.Store, scopes []model.Scope, ents []store.EntityInput) (*model.Run, int64) {
	t.Helper()
	run, err := s.CreateRun(context.Background(), store.CreateRunInput{
		Goal: "测试目标", WorkerCount: 1, Scopes: scopes, SeedEntities: ents,
	})
	if err != nil {
		t.Fatalf("建 run 失败：%v", err)
	}
	var assetID int64
	if len(ents) > 0 {
		list, err := s.ListEntitiesForRun(context.Background(), run.ID, 5)
		if err != nil || len(list) == 0 {
			t.Fatalf("种子实体缺失：%v", err)
		}
		assetID = list[0].ID
	}
	return run, assetID
}
