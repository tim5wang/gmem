package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// ===========================================================================
// migration v7（P3）：生命周期状态 + op 台账 + 审计 + 重定向 + 概念使用统计
//
// 两个口径必须同时成立：
//  1. **追加而非改写**：v1~v6 的身份（名称/语句数/DDL 指纹）逐字节不变；
//  2. **行为正确**：新表建出来、CHECK 约束生效、重复迁移幂等。
// ===========================================================================

// TestMigrateV7AppendsAfterV6WithoutRewriting 把"必须是追加"钉死在 v7 上。
//
// v6 的守卫（TestMigrateV6PreservesV1ToV5Identity）负责 v1~v5 的指纹；
// 这里负责 v6 的身份 + v7 的追加性，两者合起来覆盖 v1~v7。
func TestMigrateV7AppendsAfterV6WithoutRewriting(t *testing.T) {
	ms := migrations()

	byVersion := map[int]migration{}
	for _, m := range ms {
		if _, dup := byVersion[m.Version]; dup {
			t.Fatalf("迁移版本 %d 重复出现", m.Version)
		}
		byVersion[m.Version] = m
	}

	// v1~v5 由指纹表钉死（复用 publishedMigrations）。
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
		if want.stmtHash != "" {
			if h := migrationStmtHash(got); h != want.stmtHash {
				t.Errorf("已发布迁移 v%d 的 DDL 被改写（指纹 %s → %s）——schema 变更必须新增迁移",
					want.version, want.stmtHash, h)
			}
		}
	}

	// v6 是 Apply 型：它的实体是函数体，靠 版本+名称+无 DDL 声明 锁定。
	v6, ok := byVersion[6]
	if !ok {
		t.Fatal("v6 (p1_concept_id_soft_ref) 消失了")
	}
	if v6.Name != "p1_concept_id_soft_ref" {
		t.Errorf("v6 名称=%q, want p1_concept_id_soft_ref", v6.Name)
	}
	if v6.Apply == nil || len(v6.Statements) != 0 {
		t.Errorf("v6 必须是 Apply 型且无 DDL 语句（Apply=%v stmts=%d）", v6.Apply != nil, len(v6.Statements))
	}

	// v7 必须是追加的最后一条，且是纯 DDL（v7 只建新表，不重建历史表）。
	v7, ok := byVersion[7]
	if !ok {
		t.Fatal("缺少 v7（P3 的 schema 变更必须新增迁移）")
	}
	if v7.Name != "p3_lifecycle_ops_audit" {
		t.Errorf("v7 名称=%q, want p3_lifecycle_ops_audit", v7.Name)
	}
	if v7.Apply != nil {
		t.Errorf("v7 应当用纯 DDL（只建新表，不重建历史表）")
	}
	if len(v7.Statements) == 0 {
		t.Error("v7 没有任何 DDL 语句")
	}
	if last := ms[len(ms)-1].Version; last != 7 {
		t.Errorf("最新迁移版本=%d, want 7（新变更必须追加在末尾）", last)
	}

	// 每条 DDL 都必须 IF NOT EXISTS：schema_migrations 丢失后整个清单会被重跑。
	for i, stmt := range v7.Statements {
		if !strings.Contains(stmt, "IF NOT EXISTS") {
			t.Errorf("v7 第 %d 条 DDL 缺少 IF NOT EXISTS（重跑会炸库）：\n%s", i+1, stmt)
		}
	}
}

// TestMigrateV7CreatesLifecycleTables 断言 v7 建出的表与关键列都在。
func TestMigrateV7CreatesLifecycleTables(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	tables := map[string]bool{}
	got, err := d.Tables(ctx)
	if err != nil {
		t.Fatalf("Tables: %v", err)
	}
	for _, n := range got {
		tables[n] = true
	}
	for _, want := range []string{
		"memory_state", "op_runs", "audit_log", "memory_redirects",
		"concept_usage", "op_watermarks",
	} {
		if !tables[want] {
			t.Errorf("v7 缺少表 %q（现有 %v）", want, got)
		}
	}

	// importance 必须**分项存储**：四项各一列（TD §7 纪律 2）。
	cols := columnNames(t, d, "memory_state")
	for _, want := range []string{
		"memory_id", "scope", "status",
		"importance_base", "importance_recency", "importance_access_boost", "importance_grade",
		"importance", "status_changed_at", "last_recalled_at", "created_at", "updated_at",
	} {
		if !cols[want] {
			t.Errorf("memory_state 缺列 %q", want)
		}
	}

	// 审计必须能回答"谁/何时/因何/什么变更"：这四组列缺一不可。
	acols := columnNames(t, d, "audit_log")
	for _, want := range []string{
		"seq", "op_id", "op", "scope", "trigger_kind", "actor", "reason",
		"step_id", "step_kind", "subject", "before_json", "after_json", "changed", "at",
	} {
		if !acols[want] {
			t.Errorf("audit_log 缺列 %q", want)
		}
	}

	// op_runs 的主键必须是幂等 key 本身（并发重入靠主键冲突而不是竞态窗口）。
	if err := d.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM pragma_table_info('op_runs') WHERE name='key' AND pk=1`).Scan(new(int)); err != nil {
		t.Fatalf("检查 op_runs 主键: %v", err)
	}
}

// TestMigrateV7CheckConstraints 断言状态与触发源的取值域由数据库兜底。
func TestMigrateV7CheckConstraints(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	if _, err := d.db.ExecContext(ctx, `
		INSERT INTO memory_state (memory_id, scope, status) VALUES ('m1', 'user', 'bogus')`); err == nil {
		t.Error("memory_state.status 接受了非法值（CHECK 缺失或未生效）")
	}
	for _, st := range []string{
		"candidate", "rejected", "active", "invalidated", "stale", "archived",
	} {
		if _, err := d.db.ExecContext(ctx, `
			INSERT INTO memory_state (memory_id, scope, status) VALUES (?, 'user', ?)`,
			"m_"+st, st); err != nil {
			t.Errorf("memory_state.status 拒绝了合法值 %q: %v", st, err)
		}
	}
	if _, err := d.db.ExecContext(ctx, `
		INSERT INTO op_runs (key, op, scope, trigger_kind, status) VALUES ('k1','decay','user','idle','bogus')`); err == nil {
		t.Error("op_runs.status 接受了非法值")
	}
	// 同一幂等 key 第二次插入必须冲突（幂等闸靠主键，不靠查再插）。
	if _, err := d.db.ExecContext(ctx, `
		INSERT INTO op_runs (key, op, scope, trigger_kind, status) VALUES ('k2','decay','user','idle','running')`); err != nil {
		t.Fatalf("插入 op_runs: %v", err)
	}
	if _, err := d.db.ExecContext(ctx, `
		INSERT INTO op_runs (key, op, scope, trigger_kind, status) VALUES ('k2','decay','user','idle','running')`); err == nil {
		t.Error("op_runs 允许重复 key（幂等闸失效）")
	}
}

// TestMigrateV7IdempotentAfterMetadataLoss 断言迁移元数据丢失后重跑安全。
func TestMigrateV7IdempotentAfterMetadataLoss(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v7-idem.db")
	ctx := context.Background()
	d, err := Open(ctx, Config{Path: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = d.Close() }()

	if _, err := d.db.ExecContext(ctx, `
		INSERT INTO memory_state (memory_id, scope, status, importance_base)
		VALUES ('m-keep', 'user', 'active', 0.7)`); err != nil {
		t.Fatalf("insert memory_state: %v", err)
	}
	before := countRows(t, d.db, `SELECT COUNT(1) FROM memory_state`)

	// 模拟迁移元数据丢失：整个清单会被重跑。
	if _, err := d.db.ExecContext(ctx, `DELETE FROM schema_migrations`); err != nil {
		t.Fatalf("clear schema_migrations: %v", err)
	}
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("重跑 Migrate: %v", err)
	}
	if v, err := d.SchemaVersion(ctx); err != nil || v < 7 {
		t.Fatalf("SchemaVersion=%d err=%v, want >= 7", v, err)
	}
	if after := countRows(t, d.db, `SELECT COUNT(1) FROM memory_state`); after != before {
		t.Errorf("重跑后 memory_state 行数 %d → %d（重复写入）", before, after)
	}
	if got := countRows(t, d.db, `SELECT COUNT(1) FROM memory_state WHERE memory_id='m-keep'`); got != 1 {
		t.Errorf("重跑后既有状态行丢失（rows=%d）", got)
	}
}

// TestMigrateV7PreservesV6Behaviour 断言 v7 **不改动**历史表的行为：
// concepts.origin 仍在、concept_id 仍无外键、source_episode 外键仍生效。
//
// 这条用例防的是"顺手给 v7 加个 ALTER TABLE"——那会让已升级的库与新库 schema 漂移，
// 而 migrate 不会重跑历史版本，这类事故几乎无法在生产中发现。
func TestMigrateV7PreservesV6Behaviour(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	has, err := tableHasColumn(ctx, d.db, "concepts", "origin")
	if err != nil {
		t.Fatalf("table_info concepts: %v", err)
	}
	if !has {
		t.Error("v7 之后 concepts.origin 消失了（D15 的审计线索被破坏）")
	}

	for _, tc := range []struct {
		table, column string
		want          bool
	}{
		{"memories", "concept_id", false},
		{"nodes", "concept_id", false},
		{"memories", "source_episode", true},
		{"edges", "from_id", true},
		{"aliases", "node_id", true},
	} {
		got, err := foreignKeyExists(ctx, d.db, tc.table, tc.column)
		if err != nil {
			t.Fatalf("fk %s.%s: %v", tc.table, tc.column, err)
		}
		if got != tc.want {
			t.Errorf("v7 之后 %s.%s 外键=%v, want %v", tc.table, tc.column, got, tc.want)
		}
	}

	// memories 的列集合也不得被 v7 改动（P0/P1 的读取路径依赖它）。
	cols := columnNames(t, d, "memories")
	for _, want := range []string{
		"id", "version", "scope", "concept_id", "kind", "content",
		"t_valid", "t_invalid", "t_created", "t_expired",
		"source_episode", "importance", "recall_count",
	} {
		if !cols[want] {
			t.Errorf("v7 之后 memories 缺列 %q", want)
		}
	}
}
