package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// 诊断面（gmem doctor --self-test 的数据来源）
//
// 这里的方法全部**只读**：doctor 是体检，不是修复。任何"顺手建表/改 PRAGMA"
// 的行为都会让体检本身改变被体检对象，那样报告就不再有意义。
//
// 它们放在后端包而不是上层：PRAGMA 与表名是 SQLite 的实现细节，接口层
// 一旦直接读 pragma，D6 双后端就作废了（上层的判据必须是"后端自报状态"）。
// ===========================================================================

// ScopeCounts 是一个 scope 的行数快照。
//
// 为什么按 scope 分开数：范围隔离是本项目的硬约束（§2），"user 有数据、
// agent 空"与"两个 scope 都有数据"在排查召回问题时是完全不同的结论。
type ScopeCounts struct {
	Episodes int64
	Memories int64
	Nodes    int64
	Concepts int64
}

// Health 是库级自检快照。
type Health struct {
	// SchemaVersion 是 schema_migrations 的最大版本（空库 = 0）。
	SchemaVersion int
	// FTS5Available 表示 memories_fts 可用（FTS5 模块缺失时迁移就已经失败了，
	// 这里是**实测**一次查询，而不是"迁移过了所以应该有"）。
	FTS5Available bool
	FTS5Error     string
	// JournalMode 是 PRAGMA journal_mode 的实测值（memory 库会返回 "memory"）。
	JournalMode string
	// BusyTimeoutMs 是 PRAGMA busy_timeout 的实测值（毫秒）。
	BusyTimeoutMs int
	ForeignKeys   bool
	// RowCounts 是各表的行数（表名 → 行数），只含实际存在的表。
	RowCounts map[string]int64
	// ScopeCounts 是四个 scope 的按表行数。
	ScopeCounts map[store.Scope]ScopeCounts
	// TotalRows 是 RowCounts 的合计，用于"这个库是不是空的"这一个判断。
	TotalRows int64
}

// scopeTables 是带 scope 列的四张表（edges/aliases 没有 scope 列，单独统计）。
var scopeTables = []string{"episodes", "memories", "nodes", "concepts"}

// Health 采集一次库级自检快照。
func (d *DB) Health(ctx context.Context) (Health, error) {
	h := Health{RowCounts: map[string]int64{}, ScopeCounts: map[store.Scope]ScopeCounts{}}

	v, err := d.SchemaVersion(ctx)
	if err != nil {
		return h, err
	}
	h.SchemaVersion = v

	// FTS5：对虚表做一次真实查询。表不存在或模块缺失都会在这里暴露。
	if _, err := d.db.ExecContext(ctx, `SELECT count(*) FROM memories_fts`); err != nil {
		h.FTS5Error = err.Error()
	} else {
		h.FTS5Available = true
	}

	// PRAGMA 实测（DSN 里设了，但"设了"不等于"生效"）。
	var journal string
	if err := d.db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journal); err != nil {
		return h, fmt.Errorf("sqlite: read journal_mode: %w", err)
	}
	h.JournalMode = journal

	var busy int
	if err := d.db.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busy); err != nil {
		return h, fmt.Errorf("sqlite: read busy_timeout: %w", err)
	}
	h.BusyTimeoutMs = busy

	var fk int
	if err := d.db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil {
		return h, fmt.Errorf("sqlite: read foreign_keys: %w", err)
	}
	h.ForeignKeys = fk == 1

	tables, err := d.Tables(ctx)
	if err != nil {
		return h, err
	}
	for _, t := range tables {
		var n int64
		// 表名来自 sqlite_master（不是外部输入），但仍旧只允许白名单形态，
		// 避免把任意标识符拼进 SQL。
		if !safeIdent(t) {
			continue
		}
		if err := d.db.QueryRowContext(ctx, `SELECT count(*) FROM `+t).Scan(&n); err != nil {
			continue // 虚表/视图计数失败不影响其它项
		}
		h.RowCounts[t] = n
		h.TotalRows += n
	}
	// memories_fts 是虚表，计数成功时也从 TotalRows 里排除：
	// 它是派生索引，不构成"库里有用户数据"。
	delete(h.RowCounts, "memories_fts")
	delete(h.RowCounts, "memories_fts_v5")

	for _, sc := range []store.Scope{
		store.ScopeUser, store.ScopeAgent, store.ScopeProject, store.ScopeGlobal,
	} {
		var sc0 ScopeCounts
		for _, t := range scopeTables {
			var n int64
			if err := d.db.QueryRowContext(ctx,
				`SELECT count(*) FROM `+t+` WHERE scope = ?`, string(sc)).Scan(&n); err != nil {
				continue
			}
			switch t {
			case "episodes":
				sc0.Episodes = n
			case "memories":
				sc0.Memories = n
			case "nodes":
				sc0.Nodes = n
			case "concepts":
				sc0.Concepts = n
			}
		}
		h.ScopeCounts[sc] = sc0
	}
	return h, nil
}

// safeIdent 只接受字母数字与下划线（表名白名单形态）。
func safeIdent(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
		default:
			return false
		}
	}
	return true
}

// MemoryHistory 返回该 id 的**全部版本**（含已过期/已失效的旧版），按版本升序。
//
// # 为什么需要它（P5 的 gmem history）
//
// store.MemoryQuery 刻意不带"按 id 查"的过滤条件（那是 P0 已发布的契约），
// 而"版本链"恰恰只能按 id 取。用 ListMemories(IncludeExpired) 全表扫再过滤，
// 在万级库上会把整张表读进内存只为回答一条记忆的历史——这是典型的小功能拖垮
// 大路径。因此仿照 store.OntologyReader 的手法：在本后端上提供窄方法，
// 上层用可选接口断言，缺实现时降级（并在 Degraded 里说清楚）。
func (b *OpsBackend) MemoryHistory(ctx context.Context, id string) ([]store.Memory, error) {
	q, err := b.c.querier(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx,
		`SELECT `+memoryCols+` FROM memories WHERE id = ? ORDER BY version ASC`, id)
	if err != nil {
		return nil, fmt.Errorf("sqlite: memory history %q: %w", id, err)
	}
	defer rows.Close()
	var out []store.Memory
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: scan memory history: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// CurrentMemoryExists 报告该 id 是否有当前版本（t_expired 为空）。
func (b *OpsBackend) CurrentMemoryExists(ctx context.Context, id string) (bool, error) {
	q, err := b.c.querier(ctx)
	if err != nil {
		return false, err
	}
	var n int64
	if err := q.QueryRowContext(ctx,
		`SELECT count(*) FROM memories WHERE id = ? AND t_expired IS NULL`, id).Scan(&n); err != nil {
		return false, fmt.Errorf("sqlite: current memory %q: %w", id, err)
	}
	return n > 0, nil
}

// dataTables 是“用户数据”表（不含派生索引与运维台账）。
//
// 为什么把派生索引（memories_fts 等）排除在“非空”判定之外：
// 它们是主表的影子，主表为空时它们也必然为空；反过来，若哪天出现
// “主表空、索引有残留”的状态，那正是需要被修的数据损坏，而不是“库里有数据”。
var dataTables = []string{
	"episodes", "memories", "nodes", "edges", "concepts", "aliases",
}

// DataCounts 返回“用户数据”各表的行数（表名 → 行数）。
//
// 用途：`gmem demo` 的“非空库拒绝”闸门。刻意只数数据表而不数 op_runs/audit_log：
// 一个跑过维护 op 但没有任何记忆的库，对使用者来说仍然是空库。
func (d *DB) DataCounts(ctx context.Context) (map[string]int64, error) {
	out := make(map[string]int64, len(dataTables))
	for _, t := range dataTables {
		var n int64
		err := d.db.QueryRowContext(ctx, `SELECT count(*) FROM `+t).Scan(&n)
		if err != nil {
			// 表不存在（历史库/半迁移）不当作错误：它等价于 0 行。
			continue
		}
		out[t] = n
	}
	return out, nil
}

// LatestSchemaVersion 返回本二进制负责的**最新**迁移版本。
//
// doctor 用它判定“库比程序新/旧”。写成一个函数而不是常量：
// 它的唯一真相来源是 migrations() 清单，新增迁移时自动跟上，
// 不会出现“常量忘了改 → doctor 永远报 WARN”的隐性失效。
func LatestSchemaVersion() int {
	latest := 0
	for _, m := range migrations() {
		if m.Version > latest {
			latest = m.Version
		}
	}
	return latest
}

var _ = sql.ErrNoRows // 保持 database/sql 在仅用于错误分支时的显式依赖
