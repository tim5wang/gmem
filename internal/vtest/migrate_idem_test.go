package vtest

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/tim5wang/gmem/sqlite"
	"github.com/tim5wang/gmem/store"
)

// ---------------------------------------------------------------------------
// 攻击面 3：迁移幂等
//
// "幂等"在这里要分三层验证，dev 的测试只覆盖了第一层：
//  1. 同一 handle 连续 Migrate：不报错、版本不变（dev 已覆盖）；
//  2. **不双写**：业务数据行数与索引行数不因重复迁移而增长；
//  3. 元数据丢失后仍能收敛（DDL 全是 IF NOT EXISTS 的兜底路径）；
//  4. 多 handle 并发迁移同一文件库（进程级竞争）。
// ---------------------------------------------------------------------------

// TestMigrateIdempotentNoDoubleWrite 验证重复迁移既不报错也不改数据。
//
// "双写"最可能的形态：迁移里若写成了 INSERT 而非 IF NOT EXISTS / INSERT OR IGNORE，
// 重复执行会插出重复行或直接报主键冲突。这里同时验证两种失败。
func TestMigrateIdempotentNoDoubleWrite(t *testing.T) {
	d, raw := openFile(t)
	ctx := context.Background()

	// 先写入业务数据，再重复迁移——避免"空库迁移总是成功"的假通过。
	if err := d.Lexical().Index(ctx, []store.Document{
		{ID: "d1", Scope: store.ScopeUser, Text: "用户身份认证失败"},
		{ID: "d2", Scope: store.ScopeUser, Text: "数据库迁移幂等性验证"},
	}); err != nil {
		t.Fatalf("Index: %v", err)
	}
	if err := d.Store().PutMemory(ctx, store.Memory{
		ID: "m1", Scope: store.ScopeUser, Kind: store.KindSemantic,
		Content: "身份认证失败", TValid: 100, TCreated: 100,
	}); err != nil {
		t.Fatalf("PutMemory: %v", err)
	}

	beforeFTS := countInt(t, raw, `SELECT COUNT(1) FROM memories_fts`)
	beforeMem := countInt(t, raw, `SELECT COUNT(1) FROM memories`)
	beforeMigrations := countInt(t, raw, `SELECT COUNT(1) FROM schema_migrations`)
	verBefore, err := d.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}

	for i := 0; i < 3; i++ {
		if err := d.Migrate(ctx); err != nil {
			t.Fatalf("第 %d 次重复 Migrate 报错（幂等性破坏）: %v", i+1, err)
		}
	}

	verAfter, err := d.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if verAfter != verBefore {
		t.Errorf("重复迁移改变了 schema 版本：%d -> %d", verBefore, verAfter)
	}

	if got := countInt(t, raw, `SELECT COUNT(1) FROM schema_migrations`); got != beforeMigrations {
		t.Errorf("schema_migrations 行数 %d -> %d（双写）", beforeMigrations, got)
	}
	if got := countInt(t, raw, `SELECT COUNT(1) FROM memories_fts`); got != beforeFTS {
		t.Errorf("memories_fts 行数 %d -> %d（双写）", beforeFTS, got)
	}
	if got := countInt(t, raw, `SELECT COUNT(1) FROM memories`); got != beforeMem {
		t.Errorf("memories 行数 %d -> %d（双写）", beforeMem, got)
	}

	// 数据仍然可检索（迁移不得清表或重建丢数据）。
	hits, err := d.Lexical().Search(ctx, "认证", store.ScopeUser, 10)
	if err != nil {
		t.Fatalf("Search after migrate: %v", err)
	}
	if !contains(hitIDs(hits), "d1") {
		t.Errorf("重复迁移后检索失效，hits=%v", hitIDs(hits))
	}
}

// TestMigrateSurvivesMetadataLoss 验证 schema_migrations 记录丢失后 Migrate 仍不炸库。
//
// 这是"DDL 全 IF NOT EXISTS"这一设计的实际检验：崩溃恢复、误删元数据、
// 或从旧备份恢复时都可能出现"表在、版本记录没了"的状态。
func TestMigrateSurvivesMetadataLoss(t *testing.T) {
	path := filepath.Join(t.TempDir(), "loss.db")
	ctx := context.Background()

	d, err := sqlite.Open(ctx, sqlite.Config{Path: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := d.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	if err := d.Lexical().Index(ctx, []store.Document{
		{ID: "d1", Scope: store.ScopeUser, Text: "用户身份认证失败"},
	}); err != nil {
		t.Fatalf("Index: %v", err)
	}

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	defer func() {
		if err := raw.Close(); err != nil {
			t.Errorf("raw close: %v", err)
		}
	}()

	if _, err := raw.ExecContext(ctx, `DELETE FROM schema_migrations`); err != nil {
		t.Fatalf("清空 schema_migrations: %v", err)
	}
	if v, err := d.SchemaVersion(ctx); err != nil || v != 0 {
		t.Fatalf("清空后 SchemaVersion = %d, err = %v, want 0", v, err)
	}

	// 关键断言：已有表的情况下重跑全部迁移，不得报错。
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("元数据丢失后 Migrate 失败（IF NOT EXISTS 兜底未生效）: %v", err)
	}

	// 业务数据必须活着。
	if n := countInt(t, raw, `SELECT COUNT(1) FROM memories_fts`); n != 1 {
		t.Errorf("重跑迁移后 memories_fts 行数 = %d, want 1（数据被清）", n)
	}
	hits, err := d.Lexical().Search(ctx, "认证", store.ScopeUser, 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !contains(hitIDs(hits), "d1") {
		t.Errorf("重跑迁移后检索失效，hits=%v", hitIDs(hits))
	}
	if v, err := d.SchemaVersion(ctx); err != nil || v == 0 {
		t.Errorf("重跑迁移后 SchemaVersion = %d, err = %v, want 收敛到最新版本", v, err)
	}
}

// TestMigrateConcurrentHandlesConverge 验证**多个独立 handle** 反复迁移同一文件库后，
// 版本记录唯一且收敛到最新——即跨连接/跨进程的幂等性。
//
// 为什么不直接并发：首轮写成"4 个 goroutine 同时 Open"时测出 8/20 的随机失败
// （SQLITE_BUSY: database is locked），根因是 sqlite.Open 在设置 busy_timeout
// **之前**就 Ping，详见 repro_findings_test.go 的确定性复现与报告「可后续 #1」。
// 这是一个**已知缺陷**，不适合作为本用例的断言对象——否则测试本身变成随机的。
// 因此这里改为串行 + 带重试地反复迁移（模拟多进程依次启动），
// 断言真正属于"迁移幂等"的契约：版本唯一、收敛、数据不双写。
//
// 缺陷本身的确定性复现见 TestReproOpenBusyTimeoutOrdering（需 GMEM_VERIFY_BLOCKER=1）。
func TestMigrateConcurrentHandlesConverge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "converge.db")
	ctx := context.Background()

	const rounds = 6
	wantVersion := 0
	for round := 0; round < rounds; round++ {
		d, err := openWithRetry(ctx, path)
		if err != nil {
			t.Fatalf("第 %d 轮 Open 失败: %v", round+1, err)
		}
		v, err := d.SchemaVersion(ctx)
		if err != nil {
			t.Fatalf("第 %d 轮 SchemaVersion: %v", round+1, err)
		}
		if round == 0 {
			wantVersion = v
		} else if v != wantVersion {
			t.Errorf("第 %d 轮 schema 版本 = %d, want 稳定在 %d", round+1, v, wantVersion)
		}
		if err := d.Migrate(ctx); err != nil {
			t.Fatalf("第 %d 轮 Migrate（已迁移过）报错: %v", round+1, err)
		}
		if err := d.Close(); err != nil {
			t.Fatalf("第 %d 轮 Close: %v", round+1, err)
		}
	}

	// 版本记录必须唯一（无并发/重复双写）。
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	defer func() { _ = raw.Close() }()

	rows, err := raw.QueryContext(ctx, `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("读 schema_migrations: %v", err)
	}
	defer func() { _ = rows.Close() }()

	seen := map[int]bool{}
	count := 0
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if seen[v] {
			t.Errorf("schema_migrations 重复版本 %d（双写）", v)
		}
		seen[v] = true
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	if count == 0 {
		t.Errorf("schema_migrations 为空")
	}
	if !seen[wantVersion] {
		t.Errorf("缺少最新版本记录 %d，得到 %v", wantVersion, seen)
	}
}

// openWithRetry 在遇到 SQLITE_BUSY 时短暂退避重试。
//
// 存在的理由本身就是缺陷证据：同一进程内依次 Open 本不该需要重试。
func openWithRetry(ctx context.Context, path string) (*sqlite.DB, error) {
	var lastErr error
	for attempt := 0; attempt < 20; attempt++ {
		d, err := sqlite.Open(ctx, sqlite.Config{Path: path})
		if err == nil {
			return d, nil
		}
		lastErr = err
		time.Sleep(time.Duration(attempt+1) * 10 * time.Millisecond)
	}
	return nil, lastErr
}

// TestMigrateLeavesNoPartialSchema 验证单条迁移失败时整版回滚，不留半截 schema。
//
// 手法：手工制造一个"迁移清单里不存在的版本记录"不足以触发；改为建一张
// 与迁移 DDL 冲突的表（同名 memories，缺列），让 v1 的 CREATE TABLE 之后
// 的 CREATE INDEX 失败，观察是否留下部分对象。
//
// 预期（设计承诺："单个迁移在一个事务里完成，失败整版回滚"）：
// 迁移返回错误，且 schema_migrations 里没有 v1 记录。
func TestMigrateLeavesNoPartialSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "partial.db")
	ctx := context.Background()

	// 先手工建一个"坏"库：memories 表结构不完整，使 idx_memories_* 建索引失败。
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `CREATE TABLE memories (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatalf("预置坏表: %v", err)
	}
	if _, err := raw.ExecContext(ctx, migrationsTableDDLForTest); err != nil {
		t.Fatalf("预置 schema_migrations: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("raw close: %v", err)
	}

	d, err := sqlite.Open(ctx, sqlite.Config{Path: path})
	if err == nil {
		// 没报错说明迁移容忍了坏 schema —— 记录下来而不是静默通过。
		defer func() { _ = d.Close() }()
		t.Logf("注意：坏 schema 下 Open 未报错，迁移对该冲突是容忍的（见报告「可后续」项）")
		return
	}

	// 报了错：必须确认没有留下半截记录。
	raw2, err2 := sql.Open("sqlite", path)
	if err2 != nil {
		t.Fatalf("raw reopen: %v", err2)
	}
	defer func() { _ = raw2.Close() }()

	var n int
	if err := raw2.QueryRowContext(ctx, `SELECT COUNT(1) FROM schema_migrations WHERE version = 1`).Scan(&n); err != nil {
		t.Fatalf("查 schema_migrations: %v", err)
	}
	if n != 0 {
		t.Errorf("迁移失败却写入了 schema_migrations 记录（事务未回滚）")
	}
}

// migrationsTableDDLForTest 复制生产 DDL 的形态（不改生产代码，只在测试侧重建同一张表）。
const migrationsTableDDLForTest = `CREATE TABLE IF NOT EXISTS schema_migrations (
	version    INTEGER PRIMARY KEY,
	name       TEXT    NOT NULL,
	applied_at INTEGER NOT NULL
)`
