package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/tim5wang/gmem/ops"
	"github.com/tim5wang/gmem/store"
)

// OpsBackend 是 ops 包所需的存储端口实现（TD §7 §8：生命周期状态、幂等台账、
// audit_log、Merge 重定向、概念使用统计、Prune 物理删除）。
//
// # 为什么不把这些方法加进 store.Store
//
// store 是 P0 已发布的**公开契约**，加方法会破坏所有既有实现（internal/vtest 的
// 内存后端就会编译失败），这与 P1 用 store.OntologyReader 表达"可选扩展"是同一条
// 纪律。ops.Backend 是新端口，只在需要它的上层出现。
//
// # 事务
//
// 所有方法都通过 c.querier(ctx) 解析执行句柄：在 Store.Tx 回调内调用（ops.Runner
// 就是这么用的）时落入同一事务，因此审计与业务变更同生共死——回滚不会留下
// "审计说有变更、数据却没变"。在事务外调用会拿到 ErrTxConflict（快速失败，不排队）。
type OpsBackend struct{ c *conn }

var _ ops.Backend = (*OpsBackend)(nil)

// OpsBackend 返回 op 层的存储端口。
func (d *DB) OpsBackend() *OpsBackend { return &OpsBackend{c: d.c} }

// ---------------------------------------------------------------------------
// 生命周期（memory_state）
// ---------------------------------------------------------------------------

const stateCols = `memory_id, scope, status, importance_base, importance_recency,
	importance_access_boost, importance_grade, importance, status_changed_at,
	last_recalled_at, created_at, updated_at`

func scanState(sc interface{ Scan(...any) error }) (ops.State, error) {
	var st ops.State
	var scope, status string
	if err := sc.Scan(&st.MemoryID, &scope, &status, &st.Base, &st.Recency,
		&st.AccessBoost, &st.Grade, &st.Importance, &st.StatusChangedAt,
		&st.LastRecalledAt, &st.CreatedAt, &st.UpdatedAt); err != nil {
		return ops.State{}, err
	}
	st.Scope = store.Scope(scope)
	st.Status = ops.MemoryStatus(status)
	return st, nil
}

// PutState 覆盖写状态（Decay / 状态迁移用）。
func (b *OpsBackend) PutState(ctx context.Context, s ops.State) error {
	if s.MemoryID == "" {
		return fmt.Errorf("sqlite: put state: empty memory id")
	}
	if !s.Status.Valid() {
		return fmt.Errorf("sqlite: put state: invalid status %q", s.Status)
	}
	q, err := b.c.querier(ctx)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, `
		INSERT INTO memory_state (`+stateCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(memory_id) DO UPDATE SET
			scope=excluded.scope, status=excluded.status,
			importance_base=excluded.importance_base,
			importance_recency=excluded.importance_recency,
			importance_access_boost=excluded.importance_access_boost,
			importance_grade=excluded.importance_grade,
			importance=excluded.importance,
			status_changed_at=excluded.status_changed_at,
			last_recalled_at=excluded.last_recalled_at,
			created_at=excluded.created_at, updated_at=excluded.updated_at`,
		s.MemoryID, string(s.Scope), string(s.Status), s.Base, s.Recency,
		s.AccessBoost, s.Grade, s.Importance, s.StatusChangedAt,
		s.LastRecalledAt, s.CreatedAt, s.UpdatedAt)
	if err != nil {
		return fmt.Errorf("sqlite: put state %q: %w", s.MemoryID, err)
	}
	return nil
}

// EnsureState 只在不存在时插入（新写入的记忆补一条默认状态）。
//
// 用 INSERT OR IGNORE 而不是"先查再插"：后者在并发下有竞态窗口，
// 而这里的目标语义恰好就是"谁先写入谁算"。
func (b *OpsBackend) EnsureState(ctx context.Context, s ops.State) error {
	if s.MemoryID == "" {
		return fmt.Errorf("sqlite: ensure state: empty memory id")
	}
	q, err := b.c.querier(ctx)
	if err != nil {
		return err
	}
	if _, err := q.ExecContext(ctx, `
		INSERT OR IGNORE INTO memory_state (`+stateCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.MemoryID, string(s.Scope), string(s.Status), s.Base, s.Recency,
		s.AccessBoost, s.Grade, s.Importance, s.StatusChangedAt,
		s.LastRecalledAt, s.CreatedAt, s.UpdatedAt); err != nil {
		return fmt.Errorf("sqlite: ensure state %q: %w", s.MemoryID, err)
	}
	return nil
}

// GetState 读一条状态；不存在返回 (零值, false, nil)。
func (b *OpsBackend) GetState(ctx context.Context, memoryID string) (ops.State, bool, error) {
	q, err := b.c.querier(ctx)
	if err != nil {
		return ops.State{}, false, err
	}
	st, err := scanState(q.QueryRowContext(ctx,
		`SELECT `+stateCols+` FROM memory_state WHERE memory_id = ?`, memoryID))
	if errors.Is(err, sql.ErrNoRows) {
		return ops.State{}, false, nil
	}
	if err != nil {
		return ops.State{}, false, fmt.Errorf("sqlite: get state %q: %w", memoryID, err)
	}
	return st, true, nil
}

// ListStates 按 scope + 状态过滤（Prune / Consolidate 用）。
func (b *OpsBackend) ListStates(ctx context.Context, scope store.Scope, statuses []ops.MemoryStatus, limit int) ([]ops.State, error) {
	q, err := b.c.querier(ctx)
	if err != nil {
		return nil, err
	}
	sqlText := `SELECT ` + stateCols + ` FROM memory_state WHERE scope = ?`
	args := []any{string(scope)}
	if len(statuses) > 0 {
		ph := make([]string, len(statuses))
		for i, s := range statuses {
			ph[i] = "?"
			args = append(args, string(s))
		}
		sqlText += ` AND status IN (` + strings.Join(ph, ",") + `)`
	}
	sqlText += ` ORDER BY status_changed_at ASC, memory_id ASC`
	if limit > 0 {
		sqlText += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := q.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list states: %w", err)
	}
	defer rows.Close()
	var out []ops.State
	for rows.Next() {
		st, err := scanState(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: scan state: %w", err)
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// DeleteStates 删除状态行（Prune 后不得留下孤儿状态）。
func (b *OpsBackend) DeleteStates(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	q, err := b.c.querier(ctx)
	if err != nil {
		return err
	}
	ph := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		ph[i] = "?"
		args[i] = id
	}
	if _, err := q.ExecContext(ctx,
		`DELETE FROM memory_state WHERE memory_id IN (`+strings.Join(ph, ",")+`)`, args...); err != nil {
		return fmt.Errorf("sqlite: delete states: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 幂等台账（op_runs）
// ---------------------------------------------------------------------------

const runCols = `key, op, scope, trigger_kind, actor, reason, status, started_at, finished_at, note`

// InsertRun 插入一次运行登记。key 已存在时返回 ops.ErrDuplicateRun。
//
// 用**插入**而不是 upsert：幂等闸靠主键冲突，而不是靠"先查再插"的竞态窗口。
func (b *OpsBackend) InsertRun(ctx context.Context, r ops.RunRecord) error {
	q, err := b.c.querier(ctx)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, `
		INSERT INTO op_runs (`+runCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Key, string(r.Op), string(r.Scope), string(r.Trigger), r.Actor, r.Reason,
		string(r.Status), r.StartedAt, r.FinishedAt, r.Note)
	if err != nil {
		if isUniqueViolation(err) {
			return ops.ErrDuplicateRun
		}
		return fmt.Errorf("sqlite: insert run %q: %w", r.Key, err)
	}
	return nil
}

// UpdateRun 覆盖运行结论（状态、结束时间、备注）。
func (b *OpsBackend) UpdateRun(ctx context.Context, r ops.RunRecord) error {
	q, err := b.c.querier(ctx)
	if err != nil {
		return err
	}
	res, err := q.ExecContext(ctx, `
		UPDATE op_runs SET op=?, scope=?, trigger_kind=?, actor=?, reason=?, status=?,
			started_at=?, finished_at=?, note=? WHERE key = ?`,
		string(r.Op), string(r.Scope), string(r.Trigger), r.Actor, r.Reason,
		string(r.Status), r.StartedAt, r.FinishedAt, r.Note, r.Key)
	if err != nil {
		return fmt.Errorf("sqlite: update run %q: %w", r.Key, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("sqlite: update run %q: %w", r.Key, store.ErrNotFound)
	}
	return nil
}

// GetRun 读一次运行登记。
func (b *OpsBackend) GetRun(ctx context.Context, key string) (ops.RunRecord, bool, error) {
	q, err := b.c.querier(ctx)
	if err != nil {
		return ops.RunRecord{}, false, err
	}
	r, err := scanRun(q.QueryRowContext(ctx, `SELECT `+runCols+` FROM op_runs WHERE key = ?`, key))
	if errors.Is(err, sql.ErrNoRows) {
		return ops.RunRecord{}, false, nil
	}
	if err != nil {
		return ops.RunRecord{}, false, fmt.Errorf("sqlite: get run %q: %w", key, err)
	}
	return r, true, nil
}

func scanRun(sc interface{ Scan(...any) error }) (ops.RunRecord, error) {
	var r ops.RunRecord
	var op, scope, trig, status string
	if err := sc.Scan(&r.Key, &op, &scope, &trig, &r.Actor, &r.Reason, &status,
		&r.StartedAt, &r.FinishedAt, &r.Note); err != nil {
		return ops.RunRecord{}, err
	}
	r.Op = ops.OpName(op)
	r.Scope = store.Scope(scope)
	r.Trigger = ops.TriggerKind(trig)
	r.Status = ops.RunStatus(status)
	return r, nil
}

// ListRuns 按 scope/op 列出最近运行（审计与 doctor 用）。
func (b *OpsBackend) ListRuns(ctx context.Context, q ops.RunQuery) ([]ops.RunRecord, error) {
	dq, err := b.c.querier(ctx)
	if err != nil {
		return nil, err
	}
	sqlText := `SELECT ` + runCols + ` FROM op_runs`
	var where []string
	var args []any
	if q.Scope != "" {
		where = append(where, "scope = ?")
		args = append(args, string(q.Scope))
	}
	if q.Op != "" {
		where = append(where, "op = ?")
		args = append(args, string(q.Op))
	}
	if len(where) > 0 {
		sqlText += " WHERE " + strings.Join(where, " AND ")
	}
	sqlText += ` ORDER BY started_at DESC, key ASC`
	if q.Limit > 0 {
		sqlText += ` LIMIT ?`
		args = append(args, q.Limit)
	}
	rows, err := dq.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list runs: %w", err)
	}
	defer rows.Close()
	var out []ops.RunRecord
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: scan run: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// 审计（audit_log）
// ---------------------------------------------------------------------------

const auditCols = `seq, op_id, op, scope, trigger_kind, actor, reason, step_id, step_kind,
	subject, before_json, after_json, changed, at`

// AppendAudit 追加一条审计，返回自增 seq。
func (b *OpsBackend) AppendAudit(ctx context.Context, e ops.AuditEntry) (int64, error) {
	q, err := b.c.querier(ctx)
	if err != nil {
		return 0, err
	}
	res, err := q.ExecContext(ctx, `
		INSERT INTO audit_log (op_id, op, scope, trigger_kind, actor, reason, step_id,
			step_kind, subject, before_json, after_json, changed, at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.OpID, string(e.Op), string(e.Scope), string(e.Trigger), e.Actor, e.Reason,
		e.StepID, e.StepKind, e.Subject, e.BeforeJSON, e.AfterJSON, boolToInt(e.Changed), e.At)
	if err != nil {
		return 0, fmt.Errorf("sqlite: append audit: %w", err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return 0, nil // 某些驱动不返回 LastInsertId；不影响主结论
	}
	return seq, nil
}

// ListAudit 按条件列出审计（按 seq 升序，便于重放）。
func (b *OpsBackend) ListAudit(ctx context.Context, aq ops.AuditQuery) ([]ops.AuditEntry, error) {
	q, err := b.c.querier(ctx)
	if err != nil {
		return nil, err
	}
	sqlText := `SELECT ` + auditCols + ` FROM audit_log`
	var where []string
	var args []any
	if aq.OpID != "" {
		where = append(where, "op_id = ?")
		args = append(args, aq.OpID)
	}
	if aq.Scope != "" {
		where = append(where, "scope = ?")
		args = append(args, string(aq.Scope))
	}
	if aq.Subject != "" {
		where = append(where, "subject = ?")
		args = append(args, aq.Subject)
	}
	if aq.Op != "" {
		where = append(where, "op = ?")
		args = append(args, string(aq.Op))
	}
	if aq.SinceSeq > 0 {
		where = append(where, "seq > ?")
		args = append(args, aq.SinceSeq)
	}
	if len(where) > 0 {
		sqlText += " WHERE " + strings.Join(where, " AND ")
	}
	sqlText += ` ORDER BY seq ASC`
	if aq.Limit > 0 {
		sqlText += ` LIMIT ?`
		args = append(args, aq.Limit)
	}
	rows, err := q.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list audit: %w", err)
	}
	defer rows.Close()
	var out []ops.AuditEntry
	for rows.Next() {
		var e ops.AuditEntry
		var op, scope, trig string
		var changed int
		if err := rows.Scan(&e.Seq, &e.OpID, &op, &scope, &trig, &e.Actor, &e.Reason,
			&e.StepID, &e.StepKind, &e.Subject, &e.BeforeJSON, &e.AfterJSON, &changed, &e.At); err != nil {
			return nil, fmt.Errorf("sqlite: scan audit: %w", err)
		}
		e.Op = ops.OpName(op)
		e.Scope = store.Scope(scope)
		e.Trigger = ops.TriggerKind(trig)
		e.Changed = changed != 0
		out = append(out, e)
	}
	return out, rows.Err()
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "UNIQUE constraint failed") || strings.Contains(s, "constraint failed: UNIQUE")
}

// ---------------------------------------------------------------------------
// Merge 重定向
// ---------------------------------------------------------------------------

// PutRedirect 记录旧 ID → 新 ID。
func (b *OpsBackend) PutRedirect(ctx context.Context, r ops.Redirect) error {
	if r.OldID == "" || r.NewID == "" {
		return fmt.Errorf("sqlite: put redirect: empty id")
	}
	q, err := b.c.querier(ctx)
	if err != nil {
		return err
	}
	if _, err := q.ExecContext(ctx, `
		INSERT INTO memory_redirects (old_id, new_id, scope, reason, at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(old_id) DO UPDATE SET
			new_id=excluded.new_id, scope=excluded.scope,
			reason=excluded.reason, at=excluded.at`,
		r.OldID, r.NewID, string(r.Scope), r.Reason, r.At); err != nil {
		return fmt.Errorf("sqlite: put redirect %q → %q: %w", r.OldID, r.NewID, err)
	}
	return nil
}

// ResolveRedirect 跟随链（A→B→C 解析到 C）。
//
// 有环保护：merge 是人为操作，链上出现环说明调用方写错了，此时返回最后一个节点
// 而不是死循环。
func (b *OpsBackend) ResolveRedirect(ctx context.Context, id string) (string, error) {
	q, err := b.c.querier(ctx)
	if err != nil {
		return "", err
	}
	cur := id
	seen := map[string]struct{}{cur: {}}
	for i := 0; i < 64; i++ {
		var next string
		err := q.QueryRowContext(ctx, `SELECT new_id FROM memory_redirects WHERE old_id = ?`, cur).Scan(&next)
		if errors.Is(err, sql.ErrNoRows) {
			return cur, nil
		}
		if err != nil {
			return "", fmt.Errorf("sqlite: resolve redirect %q: %w", cur, err)
		}
		if _, dup := seen[next]; dup {
			return cur, nil
		}
		seen[next] = struct{}{}
		cur = next
	}
	return cur, nil
}

// ReassignAlias 把 surface 的映射从 fromNode 改到 toNode。
//
// 先插入新行再删旧行：aliases 的主键是 (surface, node_id)，直接 UPDATE 会撞主键。
// 也必须删旧行——只加新行的话，ListAliases 仍会返回旧节点，合并等于没做。
func (b *OpsBackend) ReassignAlias(ctx context.Context, surface, fromNode, toNode string) error {
	q, err := b.c.querier(ctx)
	if err != nil {
		return err
	}
	var source string
	var confidence float64
	err = q.QueryRowContext(ctx,
		`SELECT source, confidence FROM aliases WHERE surface = ? AND node_id = ?`,
		surface, fromNode).Scan(&source, &confidence)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // 已经不在旧节点上了：幂等
	}
	if err != nil {
		return fmt.Errorf("sqlite: read alias %q: %w", surface, err)
	}
	if _, err := q.ExecContext(ctx, `
		INSERT INTO aliases (surface, node_id, source, confidence) VALUES (?, ?, ?, ?)
		ON CONFLICT(surface, node_id) DO UPDATE SET
			source=excluded.source, confidence=MAX(aliases.confidence, excluded.confidence)`,
		surface, toNode, source, confidence); err != nil {
		return fmt.Errorf("sqlite: move alias %q → %q: %w", surface, toNode, err)
	}
	if _, err := q.ExecContext(ctx,
		`DELETE FROM aliases WHERE surface = ? AND node_id = ?`, surface, fromNode); err != nil {
		return fmt.Errorf("sqlite: delete old alias %q → %q: %w", surface, fromNode, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 进度水位
// ---------------------------------------------------------------------------

// GetWatermark 读水印。
func (b *OpsBackend) GetWatermark(ctx context.Context, scope store.Scope, op ops.OpName) (int64, bool, error) {
	q, err := b.c.querier(ctx)
	if err != nil {
		return 0, false, err
	}
	var at int64
	err = q.QueryRowContext(ctx,
		`SELECT at FROM op_watermarks WHERE scope = ? AND op = ?`, string(scope), string(op)).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("sqlite: get watermark %s/%s: %w", scope, op, err)
	}
	return at, true, nil
}

// PutWatermark 写水印。
func (b *OpsBackend) PutWatermark(ctx context.Context, scope store.Scope, op ops.OpName, at int64) error {
	q, err := b.c.querier(ctx)
	if err != nil {
		return err
	}
	if _, err := q.ExecContext(ctx, `
		INSERT INTO op_watermarks (scope, op, at) VALUES (?, ?, ?)
		ON CONFLICT(scope, op) DO UPDATE SET at=excluded.at`,
		string(scope), string(op), at); err != nil {
		return fmt.Errorf("sqlite: put watermark %s/%s: %w", scope, op, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 概念使用统计与清理（D15）
// ---------------------------------------------------------------------------

// TouchConceptUsage 记一次使用（use_count++ 并刷新 last_used_at）。
func (b *OpsBackend) TouchConceptUsage(ctx context.Context, conceptID string, at int64) error {
	if conceptID == "" {
		return nil
	}
	q, err := b.c.querier(ctx)
	if err != nil {
		return err
	}
	if _, err := q.ExecContext(ctx, `
		INSERT INTO concept_usage (concept_id, use_count, last_used_at) VALUES (?, 1, ?)
		ON CONFLICT(concept_id) DO UPDATE SET
			use_count = concept_usage.use_count + 1,
			last_used_at = MAX(concept_usage.last_used_at, excluded.last_used_at)`,
		conceptID, at); err != nil {
		return fmt.Errorf("sqlite: touch concept usage %q: %w", conceptID, err)
	}
	return nil
}

// GetConceptStat 读概念统计。
func (b *OpsBackend) GetConceptStat(ctx context.Context, conceptID string) (ops.ConceptStat, bool, error) {
	q, err := b.c.querier(ctx)
	if err != nil {
		return ops.ConceptStat{}, false, err
	}
	var st ops.ConceptStat
	st.ConceptID = conceptID
	err = q.QueryRowContext(ctx,
		`SELECT use_count, last_used_at FROM concept_usage WHERE concept_id = ?`, conceptID).
		Scan(&st.UseCount, &st.LastUsedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return st, false, nil
	}
	if err != nil {
		return ops.ConceptStat{}, false, fmt.Errorf("sqlite: get concept stat %q: %w", conceptID, err)
	}
	return st, true, nil
}

// CountConceptRefs 统计引用该概念的 memories（全部版本）与 nodes 条数。
//
// 统计**全部版本**而不是当前版本：物理删概念后，历史版本上的 concept_id 也会变成
// 悬空引用，而 as_of 回溯正是要看历史版本。
func (b *OpsBackend) CountConceptRefs(ctx context.Context, conceptID string) (int, error) {
	q, err := b.c.querier(ctx)
	if err != nil {
		return 0, err
	}
	var n int
	if err := q.QueryRowContext(ctx,
		`SELECT (SELECT COUNT(1) FROM memories WHERE concept_id = ?)
		      + (SELECT COUNT(1) FROM nodes WHERE concept_id = ?)`,
		conceptID, conceptID).Scan(&n); err != nil {
		return 0, fmt.Errorf("sqlite: count concept refs %q: %w", conceptID, err)
	}
	return n, nil
}

// ListStaleDrafts 返回 scope 内“活得太久”且无引用的 draft 概念。
//
// # 时间基准：有使用记录信使用时间，否则信创建时间（F-1）
//
// 旧实现把缺失的统计行登记成 last_used_at=0，而 cutoff=now-TTL 恒为正，
// 于是 0 <= cutoff 恒真：**刚创建的稿子第一次 Prune 就被删**。
// 那不是“清过期草稿”，而是“清所有无引用的新稿子”——D15 的反面，且不可恢复。
//
// 现在：有过真实使用（last_used_at > 0）就按使用时间，否则按创建时间
// （v8 的 concepts.created_at，由 PutConcept 兼底保证非 0）。
// 因此“刚建且从未使用”的稿子至少能活一个完整 TTL。
//
// # 为什么不是 MAX(last_used_at, created_at)
//
// created_at 来自**墙钟**（PutConcept 里的 time.Now），而 cutoff 来自调用方的时钟；
// 两者来源不同（测试里甚至是不同维度的时间），取大值会选到“未来”时间，
// 把本该过期的旧稿子永远留在库里（清理失效）。以“最近一次真实使用”优先才符合 D15。
//
// 为什么不再“首次见到即登记”（旧实现的副作用）：
// 那条 INSERT 正是缺陷的源头，而且让一个**读路径**在生产事务里写行。
// 判定不再需要它（LEFT JOIN 已能表达“从未使用”）；使用统计仍由
// TouchConceptUsage（Extract 路径）负责。
//
// # 为什么必须带 scope（F-4）
//
// Runner 把 Scope 当必填隔离边界，而 concepts 历史上没有 scope 列，于是
// agent scope 的 Prune 会删掉 user scope 的草稿。归属性由 v8 的 concepts.scope 携带，
// 创建时钉死（见 ensureConcept）。
//
// scope=” 的历史稿子（v8 迁移无法从引用回溯出边界的）不做归属过滤：它们没有任何引用，
// 也就没有任何 scope 能“拥有”它们；调用方（Prune）会把这种情况记进 Degraded，不静默。
func (b *OpsBackend) ListStaleDrafts(ctx context.Context, scope store.Scope, cutoff int64, limit int) ([]store.Concept, error) {
	q, err := b.c.querier(ctx)
	if err != nil {
		return nil, err
	}
	sqlText := `SELECT ` + conceptCols + `
		FROM concepts c LEFT JOIN concept_usage u ON u.concept_id = c.id
		WHERE c.status = ?
		  AND (c.scope = ? OR c.scope = '')
		  -- 时间基准：有使用记录就信它，否则用创建时间。
		  -- 不能用 MAX(last_used_at, created_at)：created_at 来自墙钟（PutConcept 兜底用的
		  -- time.Now），而 cutoff 来自调用方的时钟；两者混用时 MAX 会取到“未来”时间，
		  -- 把本该过期的旧稿子永远留在库里（清理失效）。以“最近一次真实使用”优先才符合 D15。
		  AND CASE WHEN COALESCE(u.last_used_at, 0) > 0
		           THEN COALESCE(u.last_used_at, 0) ELSE COALESCE(c.created_at, 0) END <= ?
		ORDER BY CASE WHEN COALESCE(u.last_used_at, 0) > 0
		              THEN COALESCE(u.last_used_at, 0) ELSE COALESCE(c.created_at, 0) END ASC,
		         c.id ASC`
	args := []any{string(store.ConceptDraft), string(scope), cutoff}
	if limit > 0 {
		sqlText += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := q.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list stale drafts: %w", err)
	}
	defer rows.Close()
	var out []store.Concept
	for rows.Next() {
		c, err := scanConcept(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: scan draft: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteConcept 物理删除概念（Prune 专用；调用方须先确认无引用）。
func (b *OpsBackend) DeleteConcept(ctx context.Context, conceptID string) error {
	q, err := b.c.querier(ctx)
	if err != nil {
		return err
	}
	if _, err := q.ExecContext(ctx, `DELETE FROM concept_usage WHERE concept_id = ?`, conceptID); err != nil {
		return fmt.Errorf("sqlite: delete concept usage %q: %w", conceptID, err)
	}
	if _, err := q.ExecContext(ctx, `DELETE FROM concepts WHERE id = ?`, conceptID); err != nil {
		return fmt.Errorf("sqlite: delete concept %q: %w", conceptID, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Prune 的物理删除
// ---------------------------------------------------------------------------

// DeleteMemoriesPermanently 删除该 id 的**全部版本**，返回删除行数。
//
// 它是 Prune 的唯一物理删除路径：FTS/向量由 ops.pruneMemory 先按 (id, version)
// 精确清理，再走到这里删主表。
func (b *OpsBackend) DeleteMemoriesPermanently(ctx context.Context, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	q, err := b.c.querier(ctx)
	if err != nil {
		return 0, err
	}
	ph := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		ph[i] = "?"
		args[i] = id
	}
	res, err := q.ExecContext(ctx,
		`DELETE FROM memories WHERE id IN (`+strings.Join(ph, ",")+`)`, args...)
	if err != nil {
		return 0, fmt.Errorf("sqlite: delete memories: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return int(n), nil
}

// MemoryVersions 返回该 id 的全部版本号（Prune 要按 (id, version) 精确删 FTS）。
func (b *OpsBackend) MemoryVersions(ctx context.Context, id string) ([]int, error) {
	q, err := b.c.querier(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx,
		`SELECT version FROM memories WHERE id = ? ORDER BY version ASC`, id)
	if err != nil {
		return nil, fmt.Errorf("sqlite: memory versions %q: %w", id, err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("sqlite: scan version: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// 扫描型 op 的读取面
// ---------------------------------------------------------------------------

// ListEpisodesPendingExtraction 返回尚无派生记忆的 episode（Extract 的输入）。
func (b *OpsBackend) ListEpisodesPendingExtraction(ctx context.Context, scope store.Scope, limit int) ([]store.Episode, error) {
	q, err := b.c.querier(ctx)
	if err != nil {
		return nil, err
	}
	sqlText := `SELECT e.id, e.scope, e.namespace, e.role, e.content, e.content_hash, e.at
		FROM episodes e
		WHERE e.scope = ?
		  AND NOT EXISTS (SELECT 1 FROM memories m WHERE m.source_episode = e.id)
		ORDER BY e.at ASC, e.id ASC`
	args := []any{string(scope)}
	if limit > 0 {
		sqlText += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := q.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list episodes pending: %w", err)
	}
	defer rows.Close()
	var out []store.Episode
	for rows.Next() {
		var ep store.Episode
		var sc string
		if err := rows.Scan(&ep.ID, &sc, &ep.Namespace, &ep.Role, &ep.Content, &ep.ContentHash, &ep.At); err != nil {
			return nil, fmt.Errorf("sqlite: scan episode: %w", err)
		}
		ep.Scope = store.Scope(sc)
		out = append(out, ep)
	}
	return out, rows.Err()
}

// ListNodes 返回该 scope 的节点。
func (b *OpsBackend) ListNodes(ctx context.Context, scope store.Scope, limit int) ([]store.Node, error) {
	q, err := b.c.querier(ctx)
	if err != nil {
		return nil, err
	}
	sqlText := `SELECT id, scope, concept_id, canonical_name, source_episode, props_json
		FROM nodes WHERE scope = ? ORDER BY id ASC`
	args := []any{string(scope)}
	if limit > 0 {
		sqlText += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := q.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list nodes: %w", err)
	}
	defer rows.Close()
	var out []store.Node
	for rows.Next() {
		var n store.Node
		var sc string
		var conceptID, sourceEpisode sql.NullString
		if err := rows.Scan(&n.ID, &sc, &conceptID, &n.CanonicalName, &sourceEpisode, &n.PropsJSON); err != nil {
			return nil, fmt.Errorf("sqlite: scan node: %w", err)
		}
		n.Scope = store.Scope(sc)
		n.ConceptID = fromNullableText(conceptID)
		n.SourceEpisode = fromNullableText(sourceEpisode)
		out = append(out, n)
	}
	return out, rows.Err()
}

// ListEdges 返回该 scope 的**当前版本**边（edges 没有 scope 列，靠节点归属过滤）。
func (b *OpsBackend) ListEdges(ctx context.Context, scope store.Scope, limit int) ([]store.Edge, error) {
	q, err := b.c.querier(ctx)
	if err != nil {
		return nil, err
	}
	sqlText := `SELECT e.id, e.version, e.from_id, e.to_id, e.relation, e.weight,
			e.t_valid, e.t_invalid, e.t_created, e.t_expired
		FROM edges e
		JOIN nodes fn ON fn.id = e.from_id
		JOIN nodes tn ON tn.id = e.to_id
		WHERE e.t_expired IS NULL AND fn.scope = ? AND tn.scope = ?
		ORDER BY e.id ASC`
	args := []any{string(scope), string(scope)}
	if limit > 0 {
		sqlText += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := q.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list edges: %w", err)
	}
	defer rows.Close()
	var out []store.Edge
	for rows.Next() {
		var e store.Edge
		var rel string
		var tInvalid, tExpired sql.NullInt64
		if err := rows.Scan(&e.ID, &e.Version, &e.FromID, &e.ToID, &rel, &e.Weight,
			&e.TValid, &tInvalid, &e.TCreated, &tExpired); err != nil {
			return nil, fmt.Errorf("sqlite: scan edge: %w", err)
		}
		e.Relation = store.Relation(rel)
		e.TInvalid = fromNullableInt(tInvalid)
		e.TExpired = fromNullableInt(tExpired)
		out = append(out, e)
	}
	return out, rows.Err()
}

// CountUnconsolidatedMemories 返回上次 Consolidate 之后新增的记忆条数（阈值触发用）。
//
// 没有水印时按"全部当前版本"计：这正是"未整理条目"的字面含义（一条都没整理过）。
func (b *OpsBackend) CountUnconsolidatedMemories(ctx context.Context, scope store.Scope) (int, error) {
	q, err := b.c.querier(ctx)
	if err != nil {
		return 0, err
	}
	at, _, err := b.GetWatermark(ctx, scope, ops.OpConsolidate)
	if err != nil {
		return 0, err
	}
	var n int
	if err := q.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM memories
		 WHERE scope = ? AND t_expired IS NULL AND (? = 0 OR t_created * 1000 > ?)`,
		string(scope), at, at).Scan(&n); err != nil {
		return 0, fmt.Errorf("sqlite: count unconsolidated: %w", err)
	}
	return n, nil
}
