package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

	"qinyuan/internal/model"
)

// newTestStore 每个测试一个独立 schema（跑真实迁移，跑完 drop）。
//
// 如果本机没有测试库，测试会被 skip —— 但 CI 上必须设 QINYUAN_TEST_DSN。
func newTestStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("QINYUAN_TEST_DSN")
	if dsn == "" {
		dsn = "postgres://postgres@127.0.0.1:55432/qinyuan_test?sslmode=disable"
	}
	ctx := context.Background()

	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("测试库不可用（%v）；设置 QINYUAN_TEST_DSN 或先起 scripts/devdb.ps1", err)
	}
	defer admin.Close(ctx)

	schema := "t_" + randHex(6)
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatalf("建 schema 失败：%v", err)
	}

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("DSN 解析失败：%v", err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()

	s, err := Open(ctx, u.String())
	if err != nil {
		t.Fatalf("打开测试库失败：%v", err)
	}
	if err := s.Migrate(ctx); err != nil {
		s.Close()
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

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func newTestRun(t *testing.T, s *Store) *model.Run {
	t.Helper()
	run, err := s.CreateRun(context.Background(), CreateRunInput{
		Goal:        "测试目标",
		WorkerCount: 1,
	})
	if err != nil {
		t.Fatalf("建测试 run 失败：%v", err)
	}
	return run
}
