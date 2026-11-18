package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// migration v8（P3 修复）：concepts.created_at + concepts.scope
//
// 两个口径与 v6/v7 同构：
//  1. **追加而非改写**：v1~v7 的身份（名称/语句数/DDL 指纹/Apply 形态）逐字节不变；
//  2. **行为正确**：两列建出来、回填不把旧数据置 0（否则 TTL 会把它们当"很久以前"）、
//     重复迁移幂等。
//
// 为什么要这两列：
//   - created_at 是 DraftTTL 的判定基准（F-1）。缺失时按 last_used_at=0 处理会让
//     "刚创建的孤儿 draft"在第一次 Prune 就被删（数据丢失级）；
//   - scope 是 Prune 的隔离边界（F-4）。没有它，agent scope 的清理会删掉 user scope
//     的草稿。
// ===========================================================================

// TestMigrateV8AppendsAfterV7WithoutRewriting 把"必须是追加"钉死在 v8 上。
func TestMigrateV8AppendsAfterV7WithoutRewriting(t *testing.T) {
	ms := migrations()

	byVersion := map[int]migration{}
	for _, m := range ms {
		if _, dup := byVersion[m.Version]; dup {
			t.Fatalf("迁移版本 %d 重复出现", m.Version)
		}
		byVersion[m.Version] = m
	}

	// v1~v5 由指纹表钉死。
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
			t.Errorf("已发布迁移 v%d 的 DDL 语句数 %d → %d", want.version, want.stmts, len(got.Statements))
		}
		if want.stmtHash != "" && migrationStmtHash(got) != want.stmtHash {
			t.Errorf("已发布迁移 v%d 的 DDL 被改写——schema 变更必须新增迁移", want.version)
		}
	}

	// v6 / v7 是 Apply/DDL 型：身份由 版本+名称 锁定，形态不得变。
	v6, ok := byVersion[6]
	if !ok {
		t.Fatal("v6 (p1_concept_id_soft_ref) 消失了")
	}
	if v6.Name != "p1_concept_id_soft_ref" || v6.Apply == nil || len(v6.Statements) != 0 {
		t.Errorf("v6 身份被改：name=%q apply=%v stmts=%d", v6.Name, v6.Apply != nil, len(v6.Statements))
	}
	v7, ok := byVersion[7]
	if !ok {
		t.Fatal("v7 (p3_lifecycle_ops_audit) 消失了")
	}
	if v7.Name != "p3_lifecycle_ops_audit" || v7.Apply != nil || len(v7.Statements) == 0 {
		t.Errorf("v7 身份被改：name=%q apply=%v stmts=%d", v7.Name, v7.Apply != nil, len(v7.Statements))
	}

	// v8 必须是追加的最后一条，且是 Apply 型（ALTER TABLE 只能在 Apply 里带列判断）。
	v8, ok := byVersion[8]
	if !ok {
		t.Fatal("缺少 v8（concepts.created_at/scope 必须新增迁移）")
	}
	if v8.Name != "p3_concept_created_at_scope" {
		t.Errorf("v8 名称=%q, want p3_concept_created_at_scope", v8.Name)
	}
	if v8.Apply == nil {
		t.Error("v8 必须是 Apply 型（需要列存在性判断以保证重跑幂等）")
	}
	if last := ms[len(ms)-1].Version; last < 8 {
		t.Errorf("最新迁移版本=%d, want >= 8", last)
	}
	for i := 1; i < len(ms); i++ {
		if ms[i].Version <= ms[i-1].Version {
			t.Errorf("迁移版本未严格递增：v%d 在 v%d 之后", ms[i].Version, ms[i-1].Version)
		}
	}
}

// TestMigrateV8AddsConceptColumns 断言两列存在，且回填不会把旧概念置成"很久以前"。
func TestMigrateV8AddsConceptColumns(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	cols := columnNames(t, d, "concepts")
	for _, want := range []string{"created_at", "scope"} {
		if !cols[want] {
			t.Errorf("concepts 缺列 %q（v8 未生效）", want)
		}
	}

	// 索引必须建出来：Prune 的 draft 分支按 (scope, status) 过滤。
	var n int
	if err := d.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM sqlite_master WHERE type='index' AND name='idx_concepts_scope_status'`).
		Scan(&n); err != nil {
		t.Fatalf("查索引: %v", err)
	}
	if n != 1 {
		t.Errorf("缺少索引 idx_concepts_scope_status（Prune 的 scope 过滤会退化成全表扫）")
	}
}

// TestMigrateV8BackfillsSafely 断言 v7 → v8 升级不会把已有概念变成"立即过期"。
//
// 为什么要从**真正的 v7 库**升级（而不是在 v8 库上把列清零）：
// 手工清零后再重跑 v8，列已存在，列判断会跳过回填，测到的是"不存在的那条路径"。
// 只有停在 v7（concepts 还没有这两列）才能验证升级时刻的回填行为。
//
// 断言：
//   - created_at 必须 > 0（=0 会被 TTL 判成"很久以前"，正是 F-1 的数据丢失形状）；
//   - scope 从引用它的记忆继承（没有引用则留空，只由 global scope 的 Prune 处理）。
func TestMigrateV8BackfillsSafely(t *testing.T) {
	ctx := context.Background()
	old := openAtVersion(t, filepath.Join(t.TempDir(), "v7-to-v8.db"), 7)

	// v7 形态写入：concepts 还没有 created_at / scope 列。
	if _, err := old.db.ExecContext(ctx,
		`INSERT INTO concepts (id, name, parent_id, status, definition, origin)
		 VALUES ('c-old', 'c-old', NULL, 'draft', '', 'implicit')`); err != nil {
		t.Fatalf("insert concept: %v", err)
	}
	if _, err := old.db.ExecContext(ctx,
		`INSERT INTO memories (id, version, scope, concept_id, kind, content, t_valid, t_created)
		 VALUES ('m-old', 1, 'user', 'c-old', 'semantic', '历史记忆正文（足够长）', 100, 100)`); err != nil {
		t.Fatalf("insert memory: %v", err)
	}

	// 升级到最新（含 v8）。
	if err := old.Migrate(ctx); err != nil {
		t.Fatalf("Migrate v7→v8: %v", err)
	}
	d := old

	var createdAt int64
	var scope string
	if err := d.db.QueryRowContext(ctx,
		`SELECT created_at, scope FROM concepts WHERE id = 'c-old'`).
		Scan(&createdAt, &scope); err != nil {
		t.Fatalf("读 concepts: %v", err)
	}
	if createdAt <= 0 {
		t.Errorf("created_at=%d：迁移把旧概念置成 0，DraftTTL 会立即把它判为过期（F-1 的数据丢失形状）", createdAt)
	}
	if scope != string(store.ScopeUser) {
		t.Errorf("scope=%q, want %q（应从引用它的记忆继承边界，否则无法按 scope 隔离）", scope, store.ScopeUser)
	}

	// 幂等：再跑一次不得改变结论（也不得把 created_at 往后推）。
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("重复 Migrate: %v", err)
	}
	var again int64
	if err := d.db.QueryRowContext(ctx,
		`SELECT created_at FROM concepts WHERE id = 'c-old'`).Scan(&again); err != nil {
		t.Fatalf("重跑后读 concepts: %v", err)
	}
	if again != createdAt {
		t.Errorf("重复迁移改写了 created_at：%d → %d", createdAt, again)
	}
}

// TestMigrateV8KeepsConceptColumnOrder 钉死 conceptCols 与表列一致。
//
// 为什么单独一条：scanConcept 按固定顺序 Scan，列顺序漂移会在运行时才炸
// （"expected N destination arguments"），而那时往往已经在生产里。
func TestMigrateV8KeepsConceptColumnOrder(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	rows, err := d.db.QueryContext(ctx, `SELECT `+conceptCols+` FROM concepts LIMIT 0`)
	if err != nil {
		t.Fatalf("select conceptCols: %v", err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns: %v", err)
	}
	want := []string{"id", "name", "parent_id", "status", "definition", "origin", "created_at", "scope"}
	if len(cols) != len(want) {
		t.Fatalf("conceptCols 列数=%d, want %d（%v）", len(cols), len(want), cols)
	}
	for i := range want {
		if cols[i] != want[i] {
			t.Errorf("第 %d 列=%q, want %q（scanConcept 的 Scan 顺序会错位）", i, cols[i], want[i])
		}
	}
	if !strings.Contains(conceptCols, "created_at") || !strings.Contains(conceptCols, "scope") {
		t.Errorf("conceptCols 缺 v8 新增列：%q", conceptCols)
	}
}
