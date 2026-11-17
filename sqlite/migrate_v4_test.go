package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/tim5wang/gmem/store"
)

// openAtVersion 打开一个**停在指定迁移版本**的库，用于验证升级路径。
//
// 手法：先建 schema_migrations，再只跑 <= version 的迁移，
// 得到与"旧发布版本"完全一致的 schema（因此可以写入旧形态数据）。
func openAtVersion(t *testing.T, path string, version int) *DB {
	t.Helper()
	ctx := context.Background()

	sqlDB, err := sql.Open("sqlite", buildDSN(path, 5e9))
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	if err := sqlDB.PingContext(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}

	c := &conn{db: sqlDB}
	d := &DB{
		db:        sqlDB,
		c:         c,
		storeImpl: &storeImpl{c: c},
		lexical:   &lexicalIndex{c: c},
		vector:    &vectorIndex{c: c},
		graph:     &graphStore{c: c},
	}
	if _, err := sqlDB.ExecContext(ctx, migrationsTableDDL); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}
	for _, m := range migrations() {
		if m.Version > version {
			continue
		}
		if err := d.applyMigration(ctx, m); err != nil {
			t.Fatalf("apply migration %d: %v", m.Version, err)
		}
	}

	got, err := d.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if got != version {
		t.Fatalf("建库版本 = %d, want %d", got, version)
	}
	return d
}

// TestMigrateV3ToV4PreservesData 验证 v4 是**追加**的迁移，且升级不丢数据。
//
// 这是"不得改写已发布的 v1/v2/v3"这条要求的实际检验：
// 用旧 schema 建库并写入数据，再跑完整 Migrate，断言
//   - memories / edges 里的旧数据全部存活，并被赋予 version=1；
//   - nodes 补上 source_episode 列且既有行不丢；
//   - 旧的四时间戳值原样保留。
func TestMigrateV3ToV4PreservesData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v3-to-v4.db")
	ctx := context.Background()

	old := openAtVersion(t, path, 3)

	// 旧形态写入：memories 无 version 列、nodes 无 source_episode 列。
	if _, err := old.db.ExecContext(ctx, `INSERT INTO episodes (id, scope, content, at)
		VALUES ('ep-old', 'user', '旧语料', 100)`); err != nil {
		t.Fatalf("insert episode: %v", err)
	}
	if _, err := old.db.ExecContext(ctx, `INSERT INTO memories
		(id, scope, kind, content, t_valid, t_invalid, t_created, t_expired, source_episode, importance)
		VALUES ('m-old', 'user', 'semantic', '旧记忆', 100, 500, 100, NULL, 'ep-old', 0.9)`); err != nil {
		t.Fatalf("insert old memory: %v", err)
	}
	if _, err := old.db.ExecContext(ctx, `INSERT INTO nodes (id, scope, canonical_name, props_json)
		VALUES ('n-old', 'user', '旧节点', '{}')`); err != nil {
		t.Fatalf("insert old node: %v", err)
	}
	if _, err := old.db.ExecContext(ctx, `INSERT INTO edges
		(id, from_id, to_id, relation, weight, t_valid, t_invalid, t_created, t_expired)
		VALUES ('e-old', 'n-old', 'n-old', 'self', 1, 100, 500, 100, NULL)`); err != nil {
		t.Fatalf("insert old edge: %v", err)
	}

	// 升级。
	if err := old.Migrate(ctx); err != nil {
		t.Fatalf("Migrate v3→v4: %v", err)
	}
	want := migrations()[len(migrations())-1].Version
	if v, err := old.SchemaVersion(ctx); err != nil || v != want {
		t.Fatalf("升级后 SchemaVersion = %d, err=%v, want %d", v, err, want)
	}

	// memories：数据存活 + version=1 + 时间戳原样。
	m, err := old.Store().GetMemory(ctx, "m-old")
	if err != nil {
		t.Fatalf("升级后旧 memory 丢失: %v", err)
	}
	if m.Version != 1 {
		t.Errorf("升级后 Version = %d, want 1（既有行应落成第一版）", m.Version)
	}
	if m.Content != "旧记忆" || m.TValid != 100 || m.TInvalid != 500 ||
		m.TCreated != 100 || m.SourceEpisode != "ep-old" || m.Importance != 0.9 {
		t.Errorf("升级后字段被改写: %+v", m)
	}

	// nodes：source_episode 列存在，旧行不丢。
	if cols := columnNames(t, old, "nodes"); !cols["source_episode"] {
		t.Errorf("升级后 nodes 缺 source_episode 列: %v", cols)
	}
	if _, err := old.Graph().GetNode(ctx, "n-old"); err != nil {
		t.Errorf("升级后旧 node 丢失: %v", err)
	}

	// edges：数据存活 + version=1 + 四时间戳原样。
	var eVersion int
	var tValid, tInvalid, tCreated int64
	var tExpired sql.NullInt64
	if err := old.db.QueryRowContext(ctx,
		`SELECT version, t_valid, t_invalid, t_created, t_expired FROM edges WHERE id = 'e-old'`).
		Scan(&eVersion, &tValid, &tInvalid, &tCreated, &tExpired); err != nil {
		t.Fatalf("升级后旧 edge 丢失: %v", err)
	}
	if eVersion != 1 || tValid != 100 || tInvalid != 500 || tCreated != 100 || tExpired.Valid {
		t.Errorf("升级后 edge = {v%d t_valid=%d t_invalid=%d t_created=%d t_expired=%v}",
			eVersion, tValid, tInvalid, tCreated, tExpired)
	}

	// 升级后新写入走版本化路径（旧行仍是 v1，新行成为 v2）。
	if err := old.Store().PutMemory(ctx, store.Memory{
		ID: "m-old", Scope: store.ScopeUser, Kind: store.KindSemantic,
		Content: "新记忆", TValid: 100, TCreated: 600, SourceEpisode: "ep-old",
	}); err != nil {
		t.Fatalf("升级后 PutMemory: %v", err)
	}
	cur, err := old.Store().GetMemory(ctx, "m-old")
	if err != nil {
		t.Fatalf("GetMemory: %v", err)
	}
	if cur.Version != 2 || cur.Content != "新记忆" {
		t.Errorf("升级后写入 = {v%d %q}, want {v2 新记忆}", cur.Version, cur.Content)
	}

	// 重复 Migrate 幂等：不得把 v4 的拷贝步骤再跑一次（否则版本行会翻倍）。
	before := countRows(t, old.db, `SELECT COUNT(1) FROM memories`)
	if err := old.Migrate(ctx); err != nil {
		t.Fatalf("重复 Migrate: %v", err)
	}
	if after := countRows(t, old.db, `SELECT COUNT(1) FROM memories`); after != before {
		t.Errorf("重复 Migrate 后 memories 行数 %d → %d（重复拷贝）", before, after)
	}

	// 元数据丢失后重跑整个清单也不得炸库或重复拷贝（Apply 的幂等判断）。
	if _, err := old.db.ExecContext(ctx, `DELETE FROM schema_migrations`); err != nil {
		t.Fatalf("清空 schema_migrations: %v", err)
	}
	before = countRows(t, old.db, `SELECT COUNT(1) FROM memories`)
	if err := old.Migrate(ctx); err != nil {
		t.Fatalf("元数据丢失后 Migrate: %v", err)
	}
	if after := countRows(t, old.db, `SELECT COUNT(1) FROM memories`); after != before {
		t.Errorf("元数据丢失重迁移后 memories 行数 %d → %d", before, after)
	}
	if _, err := old.Store().GetMemory(ctx, "m-old"); err != nil {
		t.Errorf("元数据丢失重迁移后数据丢失: %v", err)
	}
}

// TestMigrateV4DoesNotRewritePublishedVersions 确认 v1/v2/v3 的迁移条目未被改动。
//
// 用版本号 + 名称双重锁定：这三条一旦被"顺手优化"（改 DDL、改名字），
// 已经升级过的库与新库会产生 schema 漂移，而 migrate 不会重跑历史版本，
// 这类事故极难发现。本用例把它们的身份钉死。
func TestMigrateV4DoesNotRewritePublishedVersions(t *testing.T) {
	published := []struct {
		version int
		name    string
	}{
		{1, "p0_core_schema"},
		{2, "p0_fts5_lexical"},
		{3, "p0_vector_blob"},
	}
	ms := migrations()
	for _, want := range published {
		var got *migration
		for i := range ms {
			if ms[i].Version == want.version {
				got = &ms[i]
				break
			}
		}
		if got == nil {
			t.Errorf("已发布迁移 v%d (%s) 消失了", want.version, want.name)
			continue
		}
		if got.Name != want.name {
			t.Errorf("已发布迁移 v%d 名称被改：%q → %q", want.version, want.name, got.Name)
		}
	}

	// 新变更必须是追加（最大版本 > 3），而不是就地改写。
	last := ms[len(ms)-1].Version
	if last <= 3 {
		t.Errorf("最新迁移版本 = %d，schema 变更没有以新增迁移追加", last)
	}
}

func countRows(t *testing.T, db *sql.DB, query string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), query).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return n
}

// TestInvalidateMissingReturnsNotFound 固定 InvalidateMemory 的错误语义。
func TestInvalidateMissingReturnsNotFound(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	if err := d.Store().InvalidateMemory(ctx, "no-such", 1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("invalidate missing = %v, want ErrNotFound", err)
	}
}
