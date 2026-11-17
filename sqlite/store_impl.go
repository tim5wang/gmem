package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/tim5wang/gmem/store"
)

type storeImpl struct{ c *conn }

var _ store.Store = (*storeImpl)(nil)

// Tx 在同一事务内执行 fn；fn 返回错误或 panic 时整体回滚。
//
// §11 的"同一事务边界"是这样实现的：本方法把 *sql.Tx 放进 ctx（withTx），
// storeImpl 自身不持有连接，所有 Port 都按 ctx 解析执行句柄，
// 因此回调里用**回调收到的 ctx** 调 Lexical/Vector/Graph 会落到同一事务。
//
// 用外层 ctx 调其它 Port 会快速得到 ErrTxConflict，而不是排队等连接（B-1）。
func (s *storeImpl) Tx(ctx context.Context, fn func(ctx context.Context, tx store.Store) error) error {
	return s.c.runInTx(ctx, func(tctx context.Context, _ querier) error {
		return fn(tctx, s)
	})
}

// nullableInt 把领域层"0 = 未失效"的约定映射到 SQL NULL。
func nullableInt(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func fromNullableInt(n sql.NullInt64) int64 {
	if !n.Valid {
		return 0
	}
	return n.Int64
}

func nullableText(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func fromNullableText(n sql.NullString) string {
	if !n.Valid {
		return ""
	}
	return n.String
}

// ---------------------------------------------------------------------------
// episodes
// ---------------------------------------------------------------------------

const episodeCols = `id, scope, namespace, role, content, content_hash, at`

func (s *storeImpl) PutEpisode(ctx context.Context, e store.Episode) error {
	if err := e.Validate(); err != nil {
		return err
	}
	q, err := s.c.querier(ctx)
	if err != nil {
		return err
	}
	// episodes 是不可变来源：重复写入同一 ID 直接覆盖，仍不产生历史删除。
	_, err = q.ExecContext(ctx, `
		INSERT INTO episodes (`+episodeCols+`) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			scope=excluded.scope, namespace=excluded.namespace, role=excluded.role,
			content=excluded.content, content_hash=excluded.content_hash, at=excluded.at`,
		e.ID, string(e.Scope), e.Namespace, e.Role, e.Content, e.ContentHash, e.At)
	if err != nil {
		return fmt.Errorf("sqlite: put episode %q: %w", e.ID, err)
	}
	return nil
}

func (s *storeImpl) GetEpisode(ctx context.Context, id string) (store.Episode, error) {
	q, err := s.c.querier(ctx)
	if err != nil {
		return store.Episode{}, err
	}
	var e store.Episode
	var scope string
	err = q.QueryRowContext(ctx,
		`SELECT `+episodeCols+` FROM episodes WHERE id = ?`, id).
		Scan(&e.ID, &scope, &e.Namespace, &e.Role, &e.Content, &e.ContentHash, &e.At)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Episode{}, store.ErrNotFound
	}
	if err != nil {
		return store.Episode{}, fmt.Errorf("sqlite: get episode %q: %w", id, err)
	}
	e.Scope = store.Scope(scope)
	return e, nil
}

// ---------------------------------------------------------------------------
// memories（双时态 + 系统时间版本化，D12）
// ---------------------------------------------------------------------------

const memoryCols = `id, version, scope, concept_id, kind, content, t_valid, t_invalid,
	t_created, t_expired, source_episode, importance, recall_count`

// scanMemory 把一行 memories 扫进领域结构。
func scanMemory(sc interface{ Scan(...any) error }) (store.Memory, error) {
	var m store.Memory
	var scope, kind string
	var conceptID, sourceEpisode sql.NullString
	var tInvalid, tExpired sql.NullInt64
	if err := sc.Scan(&m.ID, &m.Version, &scope, &conceptID, &kind, &m.Content, &m.TValid,
		&tInvalid, &m.TCreated, &tExpired, &sourceEpisode, &m.Importance, &m.RecallCount); err != nil {
		return store.Memory{}, err
	}
	m.Scope = store.Scope(scope)
	m.ConceptID = fromNullableText(conceptID)
	m.Kind = store.MemoryKind(kind)
	m.TInvalid = fromNullableInt(tInvalid)
	m.TExpired = fromNullableInt(tExpired)
	m.SourceEpisode = fromNullableText(sourceEpisode)
	return m, nil
}

// memorySame 判断一次写入与既有版本是否**完全等价**（等价即幂等重写，不产生新版本）。
func memorySame(a, b store.Memory) bool {
	return a.Scope == b.Scope && a.ConceptID == b.ConceptID && a.Kind == b.Kind &&
		a.Content == b.Content && a.TValid == b.TValid && a.TInvalid == b.TInvalid &&
		a.TCreated == b.TCreated && a.TExpired == b.TExpired &&
		a.SourceEpisode == b.SourceEpisode && a.Importance == b.Importance &&
		a.RecallCount == b.RecallCount
}

// insertMemory 写入指定版本。
//
// ON CONFLICT 分支刻意用 COALESCE 保住既有的 t_invalid（B-2）：
// 同一 (id, version) 被重复写入时，绝不允许把已失效事实改回"有效"。
func insertMemory(ctx context.Context, q querier, m store.Memory, version int) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO memories (`+memoryCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id, version) DO UPDATE SET
			scope=excluded.scope, concept_id=excluded.concept_id, kind=excluded.kind,
			content=excluded.content, t_valid=excluded.t_valid,
			t_invalid=COALESCE(excluded.t_invalid, memories.t_invalid),
			t_created=excluded.t_created,
			t_expired=COALESCE(excluded.t_expired, memories.t_expired),
			source_episode=excluded.source_episode, importance=excluded.importance,
			recall_count=excluded.recall_count`,
		m.ID, version, string(m.Scope), nullableText(m.ConceptID), string(m.Kind), m.Content,
		m.TValid, nullableInt(m.TInvalid), m.TCreated, nullableInt(m.TExpired),
		nullableText(m.SourceEpisode), m.Importance, m.RecallCount)
	if err != nil {
		return fmt.Errorf("sqlite: put memory %q v%d: %w", m.ID, version, err)
	}
	return nil
}

// PutMemory 写入一条记忆；同一 ID 的再次写入按 D12 产生**新版本**。
//
//   - 首次写入 → version 1；
//   - 与当前版本完全等价的重写 → 幂等空操作（不动 t_invalid / t_expired）；
//   - 内容或时间戳不同 → 插入 version+1，并把旧版本置 t_expired（系统时间轴的真实写入路径）；
//   - 旧版本若已 t_invalid（世界时间失效），新版本继承该 t_invalid，避免"已失效事实复活"（B-2）。
func (s *storeImpl) PutMemory(ctx context.Context, m store.Memory) error {
	if err := m.Validate(); err != nil {
		return err
	}
	q, err := s.c.querier(ctx)
	if err != nil {
		return err
	}

	latest, hasLatest, err := latestMemory(ctx, q, m.ID)
	if err != nil {
		return err
	}
	if !hasLatest {
		return insertMemory(ctx, q, m, 1)
	}

	cur, hasCur, err := currentMemory(ctx, q, m.ID)
	if err != nil {
		return err
	}
	if hasCur && memorySame(cur, m) {
		return nil // 幂等重写：不产生新版本，也不碰 t_invalid
	}

	// B-2：已失效（世界时间）的事实不得因再次写入而复活。
	if m.TInvalid == 0 && latest.TInvalid != 0 {
		m.TInvalid = latest.TInvalid
	}

	next := latest.Version + 1
	if hasCur {
		// B-5：t_expired 的真实写入路径 —— 旧版本在新版本生效时刻过期。
		expired := m.TCreated
		if expired <= cur.TCreated {
			expired = cur.TCreated + 1
		}
		if _, err := q.ExecContext(ctx,
			`UPDATE memories SET t_expired = ? WHERE id = ? AND version = ?`,
			expired, cur.ID, cur.Version); err != nil {
			return fmt.Errorf("sqlite: expire memory %q v%d: %w", cur.ID, cur.Version, err)
		}
	}
	return insertMemory(ctx, q, m, next)
}

// currentMemory 取当前版本（t_expired IS NULL）。has=false 表示没有当前版本。
func currentMemory(ctx context.Context, q querier, id string) (store.Memory, bool, error) {
	row := q.QueryRowContext(ctx,
		`SELECT `+memoryCols+` FROM memories
		 WHERE id = ? AND t_expired IS NULL ORDER BY version DESC LIMIT 1`, id)
	m, err := scanMemory(row)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Memory{}, false, nil
	}
	if err != nil {
		return store.Memory{}, false, fmt.Errorf("sqlite: get current memory %q: %w", id, err)
	}
	return m, true, nil
}

// latestMemory 取版本号最大的那一行（可能已被 t_expired）。
func latestMemory(ctx context.Context, q querier, id string) (store.Memory, bool, error) {
	row := q.QueryRowContext(ctx,
		`SELECT `+memoryCols+` FROM memories WHERE id = ? ORDER BY version DESC LIMIT 1`, id)
	m, err := scanMemory(row)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Memory{}, false, nil
	}
	if err != nil {
		return store.Memory{}, false, fmt.Errorf("sqlite: get latest memory %q: %w", id, err)
	}
	return m, true, nil
}

// GetMemory 返回该 ID 的**当前版本**（t_expired IS NULL）。
func (s *storeImpl) GetMemory(ctx context.Context, id string) (store.Memory, error) {
	q, err := s.c.querier(ctx)
	if err != nil {
		return store.Memory{}, err
	}
	m, ok, err := currentMemory(ctx, q, id)
	if err != nil {
		return store.Memory{}, err
	}
	if !ok {
		return store.Memory{}, store.ErrNotFound
	}
	return m, nil
}

// ListMemories 支持双时态回溯：
//
//   - AsOf 为 nil：默认只返回**当前有效**的事实（t_invalid IS NULL 且 t_expired IS NULL）；
//     IncludeInvalid / IncludeExpired 可放开这两个限制，用于 history 与审计。
//   - AsOf 非 nil：返回"在该时刻同时满足世界时间与系统时间"的事实，
//     即 t_valid <= asOf 且（t_invalid 为空或 > asOf），且 t_created <= asOf 且（t_expired 为空或 > asOf）。
//
// 因为旧版本会被置 t_expired，as_of 落在两个版本的 t_created 之间时，
// 命中的是**前一版**而不是 0 行（D12 / B-4）。
func (s *storeImpl) ListMemories(ctx context.Context, q store.MemoryQuery) ([]store.Memory, error) {
	// I-5：非法 Kinds 必须报错，不能静默返回空集。
	for _, k := range q.Kinds {
		if !k.Valid() {
			return nil, fmt.Errorf("sqlite: list memories: invalid kind %q", k)
		}
	}

	var where []string
	var args []any

	if q.Scope != "" {
		where = append(where, "scope = ?")
		args = append(args, string(q.Scope))
	}
	if len(q.Kinds) > 0 {
		ph := make([]string, len(q.Kinds))
		for i, k := range q.Kinds {
			ph[i] = "?"
			args = append(args, string(k))
		}
		where = append(where, "kind IN ("+strings.Join(ph, ",")+")")
	}
	if q.ConceptID != "" {
		where = append(where, "concept_id = ?")
		args = append(args, q.ConceptID)
	}
	if q.SourceEpisode != "" {
		where = append(where, "source_episode = ?")
		args = append(args, q.SourceEpisode)
	}
	if q.AsOf != nil {
		asOf := *q.AsOf
		where = append(where,
			"t_valid <= ?",
			"(t_invalid IS NULL OR t_invalid > ?)",
			"t_created <= ?",
			"(t_expired IS NULL OR t_expired > ?)")
		args = append(args, asOf, asOf, asOf, asOf)
	} else {
		if !q.IncludeInvalid {
			where = append(where, "t_invalid IS NULL")
		}
		if !q.IncludeExpired {
			where = append(where, "t_expired IS NULL")
		}
	}

	sqlText := `SELECT ` + memoryCols + ` FROM memories`
	if len(where) > 0 {
		sqlText += " WHERE " + strings.Join(where, " AND ")
	}
	sqlText += " ORDER BY t_valid DESC, id ASC, version DESC"
	// I-3：OFFSET 在无 LIMIT 时也必须生效。SQLite 的 OFFSET 依赖 LIMIT，
	// 因此无 Limit 时显式给 LIMIT -1（表示不限），而不是把 Offset 整个丢掉。
	if q.Limit > 0 {
		sqlText += " LIMIT ?"
		args = append(args, q.Limit)
	} else if q.Offset > 0 {
		sqlText += " LIMIT -1"
	}
	if q.Offset > 0 {
		sqlText += " OFFSET ?"
		args = append(args, q.Offset)
	}

	dq, err := s.c.querier(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := dq.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list memories: %w", err)
	}
	defer rows.Close()

	var out []store.Memory
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: scan memory: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// InvalidateMemory 只置世界时间 t_invalid，永不物理删除（§2 第 2 条）。
//
// B-3：t_invalid 必须走 nullableInt 落库，否则 0 会写成 {Valid:true Int64:0}，
// 让"未失效"变成"在纪元 0 失效"。
func (s *storeImpl) InvalidateMemory(ctx context.Context, id string, tInvalid int64) error {
	q, err := s.c.querier(ctx)
	if err != nil {
		return err
	}
	res, err := q.ExecContext(ctx,
		`UPDATE memories SET t_invalid = ?
		 WHERE id = ? AND t_expired IS NULL AND t_invalid IS NULL`,
		nullableInt(tInvalid), id)
	if err != nil {
		return fmt.Errorf("sqlite: invalidate memory %q: %w", id, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		// 要么不存在，要么已经失效——后者是幂等成功，因此需区分。
		var exists int
		if err := q.QueryRowContext(ctx, `SELECT COUNT(1) FROM memories WHERE id = ?`, id).
			Scan(&exists); err != nil {
			return fmt.Errorf("sqlite: invalidate memory %q: %w", id, err)
		}
		if exists == 0 {
			return store.ErrNotFound
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// concepts
// ---------------------------------------------------------------------------

const conceptCols = `id, name, parent_id, status, definition`

func (s *storeImpl) PutConcept(ctx context.Context, c store.Concept) error {
	if err := c.Validate(); err != nil {
		return err
	}
	q, err := s.c.querier(ctx)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, `
		INSERT INTO concepts (`+conceptCols+`) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name=excluded.name, parent_id=excluded.parent_id,
			status=excluded.status, definition=excluded.definition`,
		c.ID, c.Name, nullableText(c.ParentID), string(c.Status), c.Definition)
	if err != nil {
		return fmt.Errorf("sqlite: put concept %q: %w", c.ID, err)
	}
	return nil
}

func (s *storeImpl) GetConcept(ctx context.Context, id string) (store.Concept, error) {
	q, err := s.c.querier(ctx)
	if err != nil {
		return store.Concept{}, err
	}
	var c store.Concept
	var parentID sql.NullString
	var status string
	err = q.QueryRowContext(ctx, `SELECT `+conceptCols+` FROM concepts WHERE id = ?`, id).
		Scan(&c.ID, &c.Name, &parentID, &status, &c.Definition)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Concept{}, store.ErrNotFound
	}
	if err != nil {
		return store.Concept{}, fmt.Errorf("sqlite: get concept %q: %w", id, err)
	}
	c.ParentID = fromNullableText(parentID)
	c.Status = store.ConceptStatus(status)
	return c, nil
}

// ---------------------------------------------------------------------------
// aliases
// ---------------------------------------------------------------------------

func (s *storeImpl) PutAlias(ctx context.Context, a store.Alias) error {
	if err := a.Validate(); err != nil {
		return err
	}
	q, err := s.c.querier(ctx)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, `
		INSERT INTO aliases (surface, node_id, source, confidence) VALUES (?, ?, ?, ?)
		ON CONFLICT(surface, node_id) DO UPDATE SET
			source=excluded.source, confidence=excluded.confidence`,
		a.Surface, a.NodeID, a.Source, a.Confidence)
	if err != nil {
		return fmt.Errorf("sqlite: put alias %q: %w", a.Surface, err)
	}
	return nil
}

// GetAlias 取该 surface 置信度最高的一条映射（词典优先于 bigram 的落地）。
func (s *storeImpl) GetAlias(ctx context.Context, surface string) (store.Alias, error) {
	q, err := s.c.querier(ctx)
	if err != nil {
		return store.Alias{}, err
	}
	var a store.Alias
	err = q.QueryRowContext(ctx, `
		SELECT surface, node_id, source, confidence FROM aliases
		WHERE surface = ? ORDER BY confidence DESC, node_id ASC LIMIT 1`, surface).
		Scan(&a.Surface, &a.NodeID, &a.Source, &a.Confidence)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Alias{}, store.ErrNotFound
	}
	if err != nil {
		return store.Alias{}, fmt.Errorf("sqlite: get alias %q: %w", surface, err)
	}
	return a, nil
}
