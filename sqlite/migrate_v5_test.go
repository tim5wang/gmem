package sqlite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tim5wang/gmem/store"
)

// migrationStmtHash 对迁移的 DDL 语句做逐字节指纹。
//
// 用 "\n---\n" 连接而不是直接拼字符串：语句内部的换行与分隔符都参与指纹，
// 任何空白改动（缩进、换行）都会让指纹变化——这正是“逐字节不得改”的要求。
func migrationStmtHash(m migration) string {
	h := sha256.Sum256([]byte(strings.Join(m.Statements, "\n---\n")))
	return hex.EncodeToString(h[:])
}

// ===========================================================================
// D13 缺口 1 的 schema 侧：v5 必须是**追加**迁移，且把已有 FTS 行回填为 version=1
//
// 与 TestMigrateV4DoesNotRewritePublishedVersions 同构：
// 已发布的 v1/v2/v3 逐字节钉死（DDL 指纹），v4/v5 用 Apply 函数（无 DDL 语句），
// 因此按 版本+名称+是否有 DDL 声明 钉死它们的身份。
// ===========================================================================

// publishedMigration 描述一个**已发布**迁移的身份。
//
// stmtHash 只对纯 DDL 迁移有意义（v1/v2/v3）；Apply 型迁移为 ""，
// 它们的实体是函数体，靠版本号 + 名称 + 追加顺序锁定。
type publishedMigration struct {
	version  int
	name     string
	stmts    int
	stmtHash string
}

// 已发布迁移的指纹。改动任何一个字节都会让 TestMigrateV5DoesNotRewritePublishedVersions 报错。
//
// 为什么要指纹而不是只看名称：v2 的 DDL 是 memories_fts 的定义，
// "顺手给 v2 加个 version 列"会让已升级的库（跑过 v2）与新库（跑 v2 的新 DDL）schema 漂移，
// 而 migrate 不会重跑历史版本——这类事故几乎无法在生产中发现。
var publishedMigrations = []publishedMigration{
	{1, "p0_core_schema", 19, "f219bd4896fe2169c8de8509e9070fe734e6d966aa1e9e4cfc88a0a14edaa73e"},
	{2, "p0_fts5_lexical", 1, "467a419c82e4f46bf2dc899c1d7934a1607146c73ff3fe2ce6b1a1346d3ffd7f"},
	{3, "p0_vector_blob", 2, "5e70c1851d2af47d206e082d214e30cd5baf39b1a43c6f28b0fabe90e8c0e5e1"},
	{4, "p0_versioned_bitemporal", 0, ""},
	{5, "p0_versioned_fts", 0, ""},
}

func TestMigrateV5DoesNotRewritePublishedVersions(t *testing.T) {
	ms := migrations()

	for _, want := range publishedMigrations {
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
		if len(got.Statements) != want.stmts {
			t.Errorf("已发布迁移 v%d 的 DDL 语句数 %d → %d（历史迁移不得改）",
				want.version, want.stmts, len(got.Statements))
		}
		if want.stmtHash != "" {
			if h := migrationStmtHash(*got); h != want.stmtHash {
				t.Errorf("已发布迁移 v%d 的 DDL 被改写（指纹 %s → %s）——"+
					"schema 变更必须新增迁移，不得就地修改历史条目",
					want.version, want.stmtHash, h)
			}
		}
	}

	// 新变更必须是追加（最大版本 > 4），而不是就地改写。
	last := ms[len(ms)-1].Version
	if last <= 4 {
		t.Errorf("最新迁移版本 = %d，schema 变更没有以新增迁移追加", last)
	}
	// 版本号必须严格递增且从 1 开始连续（断号会让"按序应用"的语义被怀疑）。
	for i, m := range ms {
		if m.Version != i+1 {
			t.Errorf("迁移清单第 %d 项 Version = %d, want %d（版本号必须连续递增）", i, m.Version, i+1)
		}
	}
}

// TestMigrateV4ToV5BackfillsVersionedFTS 验证 v5 是**回填**而非丢弃已有索引。
//
// 手法：用 v4 schema 建库 → 走生产路径写入一条记忆并索引 → 升级到 v5 →
// 断言旧索引行仍在、version 被回填为 1、且检索照样命中。
func TestMigrateV4ToV5BackfillsVersionedFTS(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v4-to-v5.db")
	ctx := context.Background()

	old := openAtVersion(t, path, 4)

	// v4 形态：memories 已有 version 列，但 memories_fts 没有 version 列。
	if err := old.Store().PutMemory(ctx, store.Memory{
		ID: "m-v5", Scope: store.ScopeUser, Kind: store.KindSemantic,
		Content: "用户身份认证失败", TValid: 100, TCreated: 100,
	}); err != nil {
		t.Fatalf("PutMemory: %v", err)
	}
	// v4 的 Index 实现按 id 覆盖、无 version 列；这里直接写旧形态的行，
	// 模拟"升级前已存在的派生索引"。
	if _, err := old.db.ExecContext(ctx,
		`INSERT INTO memories_fts (id, scope, body, bigrams) VALUES (?, ?, ?, ?)`,
		"m-v5", string(store.ScopeUser), "用户身份认证失败", "用户 户身 身份 份认 认证 证失 失败"); err != nil {
		t.Fatalf("insert legacy fts row: %v", err)
	}
	if n := countRows(t, old.db, `SELECT COUNT(1) FROM memories_fts`); n != 1 {
		t.Fatalf("升级前 memories_fts 行数 = %d, want 1", n)
	}

	// 升级到 v5。
	if err := old.Migrate(ctx); err != nil {
		t.Fatalf("Migrate v4→v5: %v", err)
	}
	want := migrations()[len(migrations())-1].Version
	if v, err := old.SchemaVersion(ctx); err != nil || v != want {
		t.Fatalf("升级后 SchemaVersion = %d, err=%v, want %d", v, err, want)
	}

	// 关键：旧行被回填 version=1，而不是被丢掉。
	var version int
	if err := old.db.QueryRowContext(ctx,
		`SELECT version FROM memories_fts WHERE id = 'm-v5'`).Scan(&version); err != nil {
		t.Fatalf("升级后旧 FTS 行丢失: %v", err)
	}
	if version != 1 {
		t.Errorf("回填后 version = %d, want 1（既有索引行应落成第一版）", version)
	}

	// 回填的行必须仍可检索（重建不得破坏 bigram 列）。
	hits, err := old.Lexical().Search(ctx, store.LexicalQuery{
		Query: "认证", Scope: store.ScopeUser, Limit: 10,
	})
	if err != nil {
		t.Fatalf("升级后 Search: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != "m-v5" || hits[0].Version != 1 {
		t.Errorf("升级后检索 = %v, want [{m-v5 v1}]", hits)
	}

	// 幂等：重复 Migrate 不得再搬一次（行数不变）。
	if err := old.Migrate(ctx); err != nil {
		t.Fatalf("重复 Migrate: %v", err)
	}
	if n := countRows(t, old.db, `SELECT COUNT(1) FROM memories_fts`); n != 1 {
		t.Errorf("重复 Migrate 后 memories_fts 行数 = %d, want 1（重复拷贝）", n)
	}
}

// TestMigrateV5IdempotentAfterMetadataLoss 验证 schema_migrations 丢失后重跑 v5 不炸库。
//
// v5 的 Apply 先判断 version 列是否存在，因此即使整个清单被重跑也不会二次重建。
func TestMigrateV5IdempotentAfterMetadataLoss(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	if err := d.Lexical().Index(ctx, []store.Document{
		{ID: "m-loss", Version: 2, Scope: store.ScopeUser, Text: "迁移幂等性验证"},
	}); err != nil {
		t.Fatalf("Index: %v", err)
	}
	before := countRows(t, d.db, `SELECT COUNT(1) FROM memories_fts`)

	if _, err := d.db.ExecContext(ctx, `DELETE FROM schema_migrations`); err != nil {
		t.Fatalf("清空 schema_migrations: %v", err)
	}
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("元数据丢失后 Migrate: %v", err)
	}

	if after := countRows(t, d.db, `SELECT COUNT(1) FROM memories_fts`); after != before {
		t.Errorf("元数据丢失重迁移后 memories_fts 行数 %d → %d（索引被重置）", before, after)
	}
	// 已写入的 v2 行必须原样保留（不能被"回填成 v1"）。
	var version int
	if err := d.db.QueryRowContext(ctx,
		`SELECT version FROM memories_fts WHERE id = 'm-loss'`).Scan(&version); err != nil {
		t.Fatalf("重迁移后索引行丢失: %v", err)
	}
	if version != 2 {
		t.Errorf("重迁移后 version = %d, want 2（已有版本不得被回填覆盖）", version)
	}
}
