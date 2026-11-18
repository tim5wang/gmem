package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// migration v6（D15）：移除 concept_id 外键、保留 source_episode 外键、补 concepts.origin
//
// 两个口径必须同时成立：
//  1. **追加而非改写**：v1~v5 的身份（名称/语句数/DDL 指纹）逐字节不变（见下方守卫）；
//  2. **行为正确**：concept_id 不再拒写，而血缘（source_episode）与父子表
//     （edges→nodes、aliases→nodes）的引用完整性仍在。
// ===========================================================================

// TestMigrateV6PreservesV1ToV5Identity 把 v1~v5 的身份钉死，并断言 v6 是追加的。
//
// 复用 publishedMigrations（v1~v5 的指纹表）：任何就地改写都会让这里报错。
// 这是 "v1~v5 逐字节不得改" 的直接守卫。
func TestMigrateV6PreservesV1ToV5Identity(t *testing.T) {
	ms := migrations()

	byVersion := map[int]migration{}
	for _, m := range ms {
		byVersion[m.Version] = m
	}
	for _, want := range publishedMigrations {
		got, ok := byVersion[want.version]
		if !ok {
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
			if h := migrationStmtHash(got); h != want.stmtHash {
				t.Errorf("已发布迁移 v%d 的 DDL 被改写（指纹 %s → %s）——"+
					"schema 变更必须新增迁移", want.version, want.stmtHash, h)
			}
		}
	}

	// v6 必须是**已发布条目**且是 Apply 型（表重建无法用纯 DDL 表达）。
	//
	// 断言口径的调整（P3 追加 v7 时）：原先这里写死「迁移条数 == 6」与「v6 是最后一条」，
	// 那等价于禁止后续任何 schema 变更——而 TD §8 的 audit_log 必须落在新迁移里。
	// 守卫的本意是「历史条目逐字节不得改」，因此改为：v1~v6 身份由指纹表钉死（上面的循环），
	// 条数只要求 >= 6 且 v6 仍在清单里；"新变更必须追加"由 migrate_v7_test.go 单独钉死。
	if len(ms) < 6 {
		t.Fatalf("迁移条数=%d, want >= 6（v6 必须存在，不得合并历史）", len(ms))
	}
	v6 := byVersion[6]
	if v6.Name != "p1_concept_id_soft_ref" {
		t.Errorf("v6 名称=%q, want p1_concept_id_soft_ref", v6.Name)
	}
	if v6.Apply == nil {
		t.Errorf("v6 必须用 Apply（需要重建表移除外键），实际是纯 DDL")
	}
	if len(v6.Statements) != 0 {
		t.Errorf("v6 的 DDL 语句数=%d，表重建必须走 Apply（幂等判断在函数体内）", len(v6.Statements))
	}
}

// TestMigrateV6DropsConceptFKKeepsLineage 断言外键矩阵：
//
//	memories.concept_id  无外键（D15：观察性引用）
//	nodes.concept_id     无外键（同上）
//	memories.source_episode  保留（血缘约束有意义）
//	nodes.source_episode     保留
//	edges.from_id/to_id      保留（父子表完整性）
//	aliases.node_id          保留
func TestMigrateV6DropsConceptFKKeepsLineage(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	for _, tc := range []struct {
		table, column string
		want          bool
	}{
		{"memories", "concept_id", false},
		{"nodes", "concept_id", false},
		{"memories", "source_episode", true},
		{"nodes", "source_episode", true},
		{"edges", "from_id", true},
		{"edges", "to_id", true},
		{"aliases", "node_id", true},
	} {
		got, err := foreignKeyExists(ctx, d.db, tc.table, tc.column)
		if err != nil {
			t.Fatalf("fk list %s.%s: %v", tc.table, tc.column, err)
		}
		if got != tc.want {
			t.Errorf("%s.%s 有外键=%v, want %v", tc.table, tc.column, got, tc.want)
		}
	}

	// concepts.origin 必须存在（D15：记录隐式来源）。
	has, err := tableHasColumn(ctx, d.db, "concepts", "origin")
	if err != nil {
		t.Fatalf("table_info concepts.origin: %v", err)
	}
	if !has {
		t.Fatalf("concepts 缺 origin 列（D15 要求记录 implicit 来源）")
	}
}

// TestMigrateV6KeepsForeignKeysEnforced 证明外键**仍在生效**（只是不再覆盖 concept_id）。
//
// 若 v6 重建表时把 PRAGMA foreign_keys 或子表外键一起丢了，"移除外键"就会变成
// "移除所有引用完整性"——那是比原缺陷更大的回退。
func TestMigrateV6KeepsForeignKeysEnforced(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	var fk int
	if err := d.db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatalf("PRAGMA foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Fatalf("foreign_keys=%d, want 1", fk)
	}

	// 悬空 source_episode 必须被拒。
	if _, err := d.db.ExecContext(ctx,
		`INSERT INTO memories (id, version, scope, kind, content, source_episode)
		 VALUES ('m-bad', 1, 'user', 'semantic', 'x', 'ep-missing')`); err == nil {
		t.Error("悬空 source_episode 被接受：血缘外键丢失")
	}
	// 悬空边端点必须被拒。
	if _, err := d.db.ExecContext(ctx,
		`INSERT INTO edges (id, version, from_id, to_id, relation)
		 VALUES ('e-bad', 1, 'n-missing', 'n-also-missing', 'uses')`); err == nil {
		t.Error("悬空边端点被接受：edges 外键丢失")
	}
	// 悬空 concept_id 必须被**接受**（D15）。
	if _, err := d.db.ExecContext(ctx,
		`INSERT INTO nodes (id, scope, concept_id, canonical_name)
		 VALUES ('n-ok', 'user', 'c-never-declared', 'x')`); err != nil {
		t.Errorf("悬空 concept_id 被拒绝（D15 要求放开）：%v", err)
	}
}

// TestMigrateV5ToV6PreservesData 验证升级路径不丢数据。
//
// 关键在于 v6 要重建 nodes 及其子表 edges/aliases：顺序错了会让事务在提交时报
// FOREIGN KEY constraint failed（父表被引用时直接 DROP）。本用例用真实升级路径钉住它。
func TestMigrateV5ToV6PreservesData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v5-to-v6.db")
	ctx := context.Background()

	old := openAtVersion(t, path, 5)

	// v5 形态：外键仍在，因此必须先建 concept/episode 再写子表。
	if _, err := old.db.ExecContext(ctx,
		`INSERT INTO episodes (id, scope, content, at) VALUES ('ep-1', 'user', '旧语料', 100)`); err != nil {
		t.Fatalf("insert episode: %v", err)
	}
	if _, err := old.db.ExecContext(ctx,
		`INSERT INTO concepts (id, name, status, definition) VALUES ('c-1', '旧概念', 'certified', '旧定义')`); err != nil {
		t.Fatalf("insert concept: %v", err)
	}
	if _, err := old.db.ExecContext(ctx, `
		INSERT INTO memories (id, version, scope, concept_id, kind, content, t_valid, t_created, source_episode)
		VALUES ('m-old', 1, 'user', 'c-1', 'semantic', '旧记忆正文', 100, 100, 'ep-1')`); err != nil {
		t.Fatalf("insert memory: %v", err)
	}
	if _, err := old.db.ExecContext(ctx, `
		INSERT INTO nodes (id, scope, concept_id, canonical_name, source_episode)
		VALUES ('n-a', 'user', 'c-1', 'A', 'ep-1'), ('n-b', 'user', 'c-1', 'B', NULL)`); err != nil {
		t.Fatalf("insert nodes: %v", err)
	}
	if _, err := old.db.ExecContext(ctx, `
		INSERT INTO edges (id, version, from_id, to_id, relation, weight, t_valid, t_created)
		VALUES ('e-1', 1, 'n-a', 'n-b', 'uses', 1, 100, 100)`); err != nil {
		t.Fatalf("insert edge: %v", err)
	}
	if _, err := old.db.ExecContext(ctx,
		`INSERT INTO aliases (surface, node_id, source, confidence) VALUES ('A', 'n-a', 'dict', 1)`); err != nil {
		t.Fatalf("insert alias: %v", err)
	}

	if err := old.Migrate(ctx); err != nil {
		t.Fatalf("Migrate v5→v6: %v", err)
	}
	// v5→v6 升级后版本必须**至少**是 6（可能已顺带应用后续迁移，例如 P3 的 v7）。
	if v, err := old.SchemaVersion(ctx); err != nil || v < 6 {
		t.Fatalf("升级后 SchemaVersion=%d err=%v, want >= 6", v, err)
	}

	// 数据全部存活，且外键列的值原样保留。
	var conceptID, episode string
	if err := old.db.QueryRowContext(ctx,
		`SELECT concept_id, source_episode FROM memories WHERE id = 'm-old'`).
		Scan(&conceptID, &episode); err != nil {
		t.Fatalf("升级后 memories 行丢失: %v", err)
	}
	if conceptID != "c-1" || episode != "ep-1" {
		t.Errorf("memories 外键列被改：concept_id=%q source_episode=%q", conceptID, episode)
	}
	for _, q := range []struct {
		name string
		sql  string
		want int
	}{
		{"nodes", `SELECT COUNT(1) FROM nodes`, 2},
		{"edges", `SELECT COUNT(1) FROM edges`, 1},
		{"aliases", `SELECT COUNT(1) FROM aliases`, 1},
		{"concepts", `SELECT COUNT(1) FROM concepts`, 1},
	} {
		if n := countRows(t, old.db, q.sql); n != q.want {
			t.Errorf("升级后 %s 行数=%d, want %d", q.name, n, q.want)
		}
	}

	// 升级后 concept_id 可悬空，source_episode 不可。
	if _, err := old.db.ExecContext(ctx,
		`INSERT INTO memories (id, version, scope, concept_id, kind, content)
		 VALUES ('m-new', 1, 'user', 'c-undeclared', 'semantic', 'x')`); err != nil {
		t.Errorf("升级后 concept_id 仍被外键拒绝：%v", err)
	}
	if _, err := old.db.ExecContext(ctx,
		`INSERT INTO memories (id, version, scope, kind, content, source_episode)
		 VALUES ('m-new2', 1, 'user', 'semantic', 'x', 'ep-missing')`); err == nil {
		t.Error("升级后 source_episode 外键丢失")
	}

	// 幂等：重复 Migrate 不得再搬一次数据。
	if err := old.Migrate(ctx); err != nil {
		t.Fatalf("重复 Migrate: %v", err)
	}
	if n := countRows(t, old.db, `SELECT COUNT(1) FROM nodes`); n != 2 {
		t.Errorf("重复 Migrate 后 nodes=%d, want 2（重复拷贝）", n)
	}
}

// TestMigrateV6IdempotentAfterMetadataLoss：schema_migrations 丢失后重跑 v6 不炸库。
//
// v6 的 Apply 先查 concept_id 上是否还有外键，没有就只补 origin 列，绝不第二次拷贝。
func TestMigrateV6IdempotentAfterMetadataLoss(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	// 走生产路径写入一条带隐式概念的记忆。
	if err := d.Store().PutMemory(ctx, store.Memory{
		ID: "m-keep", Scope: store.ScopeUser, Kind: store.KindSemantic,
		ConceptID: "c-implicit", Content: "迁移幂等", TValid: 100, TCreated: 100,
	}); err != nil {
		t.Fatalf("PutMemory: %v", err)
	}
	before := countRows(t, d.db, `SELECT COUNT(1) FROM concepts`)

	// 模拟迁移元数据丢失：整个清单会被重跑。
	if _, err := d.db.ExecContext(ctx, `DELETE FROM schema_migrations`); err != nil {
		t.Fatalf("clear schema_migrations: %v", err)
	}
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("重跑 Migrate: %v", err)
	}
	if v, err := d.SchemaVersion(ctx); err != nil || v < 6 {
		t.Fatalf("SchemaVersion=%d err=%v, want >= 6", v, err)
	}
	if after := countRows(t, d.db, `SELECT COUNT(1) FROM concepts`); after != before {
		t.Errorf("重跑后 concepts 行数 %d → %d（重复拷贝）", before, after)
	}
	if n := countRows(t, d.db, `SELECT COUNT(1) FROM memories`); n != 1 {
		t.Errorf("重跑后 memories 行数=%d, want 1", n)
	}
	got, err := d.Store().GetMemory(ctx, "m-keep")
	if err != nil {
		t.Fatalf("重跑后记忆丢失: %v", err)
	}
	if got.ConceptID != "c-implicit" {
		t.Errorf("重跑后 concept_id=%q", got.ConceptID)
	}
}
