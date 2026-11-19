package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/tim5wang/gmem/ops"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// migration v9（F-7 收口）：op_runs.skip_count + op_runs.last_skipped_at
//
// 三个口径与 v6/v7/v8 同构：
//  1. **追加而非改写**：v1~v8 的身份（名称/语句数/DDL 指纹/Apply 形态）逐字节不变；
//  2. **行为正确**：两列建出来、默认 0、升级不丢既有行、重复迁移幂等；
//  3. **不增行**：这两列存在的唯一目的是让 duplicate 有痕迹，而受保护用例钉死了
//     duplicate 之后 op_runs / audit_log 的行数都不得变（只能改列）。
// ===========================================================================

// TestMigrateV9AppendsAfterV8WithoutRewriting 把"必须是追加"钉死在 v9 上。
func TestMigrateV9AppendsAfterV8WithoutRewriting(t *testing.T) {
	ms := migrations()

	byVersion := map[int]migration{}
	for _, m := range ms {
		if _, dup := byVersion[m.Version]; dup {
			t.Fatalf("迁移版本 %d 重复出现", m.Version)
		}
		byVersion[m.Version] = m
	}

	// v1~v5 由指纹表钉死（DDL 逐字节）。
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

	// v6 / v7 / v8 是 Apply/DDL 型：身份由 版本+名称+形态 锁定。
	wantShape := []struct {
		version int
		name    string
		apply   bool
		stmts   bool // true = 必须有 DDL 语句
	}{
		{6, "p1_concept_id_soft_ref", true, false},
		{7, "p3_lifecycle_ops_audit", false, true},
		{8, "p3_concept_created_at_scope", true, false},
	}
	for _, w := range wantShape {
		got, ok := byVersion[w.version]
		if !ok {
			t.Errorf("v%d (%s) 消失了", w.version, w.name)
			continue
		}
		if got.Name != w.name {
			t.Errorf("v%d 名称=%q, want %q（历史迁移不得改名）", w.version, got.Name, w.name)
		}
		if (got.Apply != nil) != w.apply {
			t.Errorf("v%d Apply 形态被改：apply=%v, want %v", w.version, got.Apply != nil, w.apply)
		}
		if w.stmts && len(got.Statements) == 0 {
			t.Errorf("v%d 的 DDL 语句被清空", w.version)
		}
	}

	// v9 必须是追加的最后一条，且是 Apply 型（ALTER TABLE 只能在 Apply 里带列判断）。
	v9, ok := byVersion[9]
	if !ok {
		t.Fatal("缺少 v9（op_runs.skip_count/last_skipped_at 必须新增迁移）")
	}
	if v9.Name != "p3_op_runs_skip_counters" {
		t.Errorf("v9 名称=%q, want p3_op_runs_skip_counters", v9.Name)
	}
	if v9.Apply == nil {
		t.Error("v9 必须是 Apply 型（需要列存在性判断以保证重跑幂等）")
	}
	if len(v9.Statements) != 0 {
		t.Errorf("v9 的 DDL 语句数=%d，ALTER TABLE 必须走 Apply（列判断在函数体内）", len(v9.Statements))
	}
	if last := ms[len(ms)-1].Version; last != 9 {
		t.Errorf("最新迁移版本=%d, want 9（v9 必须是最后一条）", last)
	}
	for i := 1; i < len(ms); i++ {
		if ms[i].Version <= ms[i-1].Version {
			t.Errorf("迁移版本未严格递增：v%d 在 v%d 之后", ms[i].Version, ms[i-1].Version)
		}
	}
}

// TestMigrateV9AddsOpRunsSkipColumns 断言两列存在、默认 0、且列顺序追加在末尾。
func TestMigrateV9AddsOpRunsSkipColumns(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	cols := columnNames(t, d, "op_runs")
	for _, want := range []string{"skip_count", "last_skipped_at"} {
		if !cols[want] {
			t.Errorf("op_runs 缺列 %q（v9 未生效）", want)
		}
	}

	// 新列必须是 NOT NULL DEFAULT 0：计数从 0 开始，且旧行不需要回填。
	if _, err := d.db.ExecContext(ctx, `
		INSERT INTO op_runs (key, op, scope, trigger_kind, status)
		VALUES ('k-default', 'decay', 'user', 'idle', 'applied')`); err != nil {
		t.Fatalf("插入 op_runs: %v", err)
	}
	var skipCount, lastSkippedAt int
	if err := d.db.QueryRowContext(ctx,
		`SELECT skip_count, last_skipped_at FROM op_runs WHERE key = 'k-default'`).
		Scan(&skipCount, &lastSkippedAt); err != nil {
		t.Fatalf("读新列: %v", err)
	}
	if skipCount != 0 || lastSkippedAt != 0 {
		t.Errorf("新行默认值 = (%d, %d), want (0, 0)", skipCount, lastSkippedAt)
	}

	// 列顺序：runCols 的 Scan 顺序必须与物理列一致（否则会错位到运行时才炸）。
	rows, err := d.db.QueryContext(ctx, `SELECT `+runCols+` FROM op_runs LIMIT 0`)
	if err != nil {
		t.Fatalf("select runCols: %v", err)
	}
	defer rows.Close()
	got, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns: %v", err)
	}
	want := []string{
		"key", "op", "scope", "trigger_kind", "actor", "reason", "status",
		"started_at", "finished_at", "note", "skip_count", "last_skipped_at",
	}
	if len(got) != len(want) {
		t.Fatalf("runCols 列数=%d, want %d（%v）", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 列=%q, want %q（scanRun 的 Scan 顺序会错位）", i, got[i], want[i])
		}
	}
}

// TestMigrateV9UpgradeKeepsExistingRuns 断言 v8 → v9 升级不丢既有台账行、计数归 0。
//
// 为什么从**真正的 v8 库**升级：在 v9 库上把列清零后再重跑会走"列已存在"的分支，
// 测不到升级时刻的真实行为（与 v8 的 TestMigrateV8BackfillsSafely 同一口径）。
func TestMigrateV9UpgradeKeepsExistingRuns(t *testing.T) {
	ctx := context.Background()
	old := openAtVersion(t, filepath.Join(t.TempDir(), "v8-to-v9.db"), 8)

	// v8 形态写入：op_runs 还没有 skip_count / last_skipped_at。
	if _, err := old.db.ExecContext(ctx, `
		INSERT INTO op_runs (key, op, scope, trigger_kind, actor, reason, status, started_at, note)
		VALUES ('k-old', 'decay', 'user', 'idle', 'a', 'r', 'applied', 1234, 'steps=1 applied=1')`); err != nil {
		t.Fatalf("v8 插入 op_runs: %v", err)
	}

	if err := old.Migrate(ctx); err != nil {
		t.Fatalf("v8 → v9 Migrate: %v", err)
	}
	if v, err := old.SchemaVersion(ctx); err != nil || v != 9 {
		t.Fatalf("升级后 SchemaVersion = %d, err = %v, want 9", v, err)
	}

	var (
		status        string
		startedAt     int64
		note          string
		skipCount     int
		lastSkippedAt int64
	)
	if err := old.db.QueryRowContext(ctx, `
		SELECT status, started_at, note, skip_count, last_skipped_at
		FROM op_runs WHERE key = 'k-old'`).
		Scan(&status, &startedAt, &note, &skipCount, &lastSkippedAt); err != nil {
		t.Fatalf("升级后读 op_runs: %v", err)
	}
	if status != "applied" || startedAt != 1234 || note != "steps=1 applied=1" {
		t.Errorf("升级改写了既有行：status=%q started_at=%d note=%q", status, startedAt, note)
	}
	if skipCount != 0 || lastSkippedAt != 0 {
		t.Errorf("升级后 skip_count/last_skipped_at = (%d, %d), want (0, 0)（默认值，不回填假痕迹）",
			skipCount, lastSkippedAt)
	}
	var rows int
	if err := old.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM op_runs`).Scan(&rows); err != nil {
		t.Fatalf("数 op_runs: %v", err)
	}
	if rows != 1 {
		t.Errorf("升级后 op_runs 行数=%d, want 1（迁移不得增删行）", rows)
	}

	// 幂等：再跑一次不得改变任何东西。
	if err := old.Migrate(ctx); err != nil {
		t.Fatalf("重复 Migrate: %v", err)
	}
	var again int
	if err := old.db.QueryRowContext(ctx,
		`SELECT skip_count FROM op_runs WHERE key = 'k-old'`).Scan(&again); err != nil {
		t.Fatalf("重跑后读 op_runs: %v", err)
	}
	if again != 0 {
		t.Errorf("重复迁移改写了 skip_count：%d", again)
	}
}

// TestMarkRunSkippedUpdatesInPlace 断言 MarkRunSkipped 只做 UPDATE：不增行、不动其他列。
func TestMarkRunSkippedUpdatesInPlace(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	be := d.OpsBackend()

	rec := ops.RunRecord{
		Key: "k-skip", Op: ops.OpDecay, Scope: store.ScopeUser, Trigger: ops.TriggerIdle,
		Actor: "a", Reason: "r", Status: ops.RunApplied,
		StartedAt: 1000, FinishedAt: 1100, Note: "steps=0 applied=0",
	}
	if err := be.InsertRun(ctx, rec); err != nil {
		t.Fatalf("InsertRun: %v", err)
	}
	rowsBefore := countRows(t, d.db, `SELECT COUNT(1) FROM op_runs`)
	auditBefore := countRows(t, d.db, `SELECT COUNT(1) FROM audit_log`)

	for i, at := range []int64{2000, 3000} {
		if err := be.MarkRunSkipped(ctx, "k-skip", at); err != nil {
			t.Fatalf("MarkRunSkipped #%d: %v", i+1, err)
		}
		if got := countRows(t, d.db, `SELECT COUNT(1) FROM op_runs`); got != rowsBefore {
			t.Fatalf("第 %d 次跳过之后 op_runs 行数=%d, want %d（MarkRunSkipped 必须只做 UPDATE）",
				i+1, got, rowsBefore)
		}
		if got := countRows(t, d.db, `SELECT COUNT(1) FROM audit_log`); got != auditBefore {
			t.Fatalf("第 %d 次跳过之后 audit_log 行数=%d, want %d（不得插审计行）",
				i+1, got, auditBefore)
		}
	}

	got, found, err := be.GetRun(ctx, "k-skip")
	if err != nil || !found {
		t.Fatalf("GetRun: found=%v err=%v", found, err)
	}
	if got.SkipCount != 2 {
		t.Errorf("SkipCount=%d, want 2（每次跳过累加一次）", got.SkipCount)
	}
	if got.LastSkippedAt != 3000 {
		t.Errorf("LastSkippedAt=%d, want 3000（取最后一次跳过时刻）", got.LastSkippedAt)
	}
	// 其余列一字不动：跳过不改变这次运行原有的结论。
	if got.Status != ops.RunApplied || got.StartedAt != 1000 || got.FinishedAt != 1100 ||
		got.Note != "steps=0 applied=0" || got.Actor != "a" || got.Reason != "r" {
		t.Errorf("MarkRunSkipped 改动了非计数列：%+v", got)
	}

	// 未知 key：必须报 ErrNotFound（而不是静默成功，让"台账里没有这次运行"找不到原因）。
	if err := be.MarkRunSkipped(ctx, "k-missing", 4000); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("未知 key 的 MarkRunSkipped 错误=%v, want store.ErrNotFound", err)
	}
	if got := countRows(t, d.db, `SELECT COUNT(1) FROM op_runs`); got != rowsBefore {
		t.Errorf("未知 key 之后 op_runs 行数=%d, want %d（不得补插行）", got, rowsBefore)
	}
}
