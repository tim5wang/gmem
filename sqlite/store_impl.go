package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/tim5wang/gmem/store"
)

// querier 统一 *sql.DB 与 *sql.Tx，让同一份实现既能直接跑也能在事务里跑。
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type storeImpl struct{ q querier }

var _ store.Store = (*storeImpl)(nil)

// Tx 在同一事务内执行 fn；fn 返回错误或 panic 时整体回滚。
func (s *storeImpl) Tx(ctx context.Context, fn func(ctx context.Context, tx store.Store) error) error {
	sqlDB, ok := s.q.(*sql.DB)
	if !ok {
		// 已在事务中（嵌套调用）：直接复用当前事务，不再开新事务。
		return fn(ctx, s)
	}
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if err := fn(ctx, &storeImpl{q: tx}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit tx: %w", err)
	}
	committed = true
	return nil
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
	// episodes 是不可变来源：重复写入同一 ID 直接覆盖，仍不产生历史删除。
	_, err := s.q.ExecContext(ctx, `
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
	var e store.Episode
	var scope string
	err := s.q.QueryRowContext(ctx,
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
// memories（双时态）
// ---------------------------------------------------------------------------

const memoryCols = `id, scope, concept_id, kind, content, t_valid, t_invalid, t_created, t_expired,
	source_episode, importance, recall_count`

func (s *storeImpl) PutMemory(ctx context.Context, m store.Memory) error {
	if err := m.Validate(); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO memories (`+memoryCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			scope=excluded.scope, concept_id=excluded.concept_id, kind=excluded.kind,
			content=excluded.content, t_valid=excluded.t_valid, t_invalid=excluded.t_invalid,
			t_created=excluded.t_created, t_expired=excluded.t_expired,
			source_episode=excluded.source_episode, importance=excluded.importance,
			recall_count=excluded.recall_count`,
		m.ID, string(m.Scope), nullableText(m.ConceptID), string(m.Kind), m.Content,
		m.TValid, nullableInt(m.TInvalid), m.TCreated, nullableInt(m.TExpired),
		nullableText(m.SourceEpisode), m.Importance, m.RecallCount)
	if err != nil {
		return fmt.Errorf("sqlite: put memory %q: %w", m.ID, err)
	}
	return nil
}

func (s *storeImpl) GetMemory(ctx context.Context, id string) (store.Memory, error) {
	var m store.Memory
	var scope, kind string
	var conceptID, sourceEpisode sql.NullString
	var tInvalid, tExpired sql.NullInt64
	err := s.q.QueryRowContext(ctx,
		`SELECT `+memoryCols+` FROM memories WHERE id = ?`, id).
		Scan(&m.ID, &scope, &conceptID, &kind, &m.Content, &m.TValid, &tInvalid, &m.TCreated,
			&tExpired, &sourceEpisode, &m.Importance, &m.RecallCount)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Memory{}, store.ErrNotFound
	}
	if err != nil {
		return store.Memory{}, fmt.Errorf("sqlite: get memory %q: %w", id, err)
	}
	m.Scope = store.Scope(scope)
	m.ConceptID = fromNullableText(conceptID)
	m.Kind = store.MemoryKind(kind)
	m.TInvalid = fromNullableInt(tInvalid)
	m.TExpired = fromNullableInt(tExpired)
	m.SourceEpisode = fromNullableText(sourceEpisode)
	return m, nil
}

// ListMemories 支持双时态回溯：
//
//   - AsOf 为 nil：默认只返回**当前有效**的事实（t_invalid IS NULL 且 t_expired IS NULL）；
//     IncludeInvalid / IncludeExpired 可放开这两个限制，用于 history 与审计。
//   - AsOf 非 nil：返回"在该时刻同时满足世界时间与系统时间"的事实，
//     即 t_valid <= asOf 且（t_invalid 为空或 > asOf），且 t_created <= asOf 且（t_expired 为空或 > asOf）。
func (s *storeImpl) ListMemories(ctx context.Context, q store.MemoryQuery) ([]store.Memory, error) {
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
	sqlText += " ORDER BY t_valid DESC, id ASC"
	if q.Limit > 0 {
		sqlText += " LIMIT ?"
		args = append(args, q.Limit)
		if q.Offset > 0 {
			sqlText += " OFFSET ?"
			args = append(args, q.Offset)
		}
	}

	rows, err := s.q.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list memories: %w", err)
	}
	defer rows.Close()

	var out []store.Memory
	for rows.Next() {
		var m store.Memory
		var scope, kind string
		var conceptID, sourceEpisode sql.NullString
		var tInvalid, tExpired sql.NullInt64
		if err := rows.Scan(&m.ID, &scope, &conceptID, &kind, &m.Content, &m.TValid, &tInvalid,
			&m.TCreated, &tExpired, &sourceEpisode, &m.Importance, &m.RecallCount); err != nil {
			return nil, fmt.Errorf("sqlite: scan memory: %w", err)
		}
		m.Scope = store.Scope(scope)
		m.ConceptID = fromNullableText(conceptID)
		m.Kind = store.MemoryKind(kind)
		m.TInvalid = fromNullableInt(tInvalid)
		m.TExpired = fromNullableInt(tExpired)
		m.SourceEpisode = fromNullableText(sourceEpisode)
		out = append(out, m)
	}
	return out, rows.Err()
}

// InvalidateMemory 只置世界时间 t_invalid，永不物理删除（§2 第 2 条）。
func (s *storeImpl) InvalidateMemory(ctx context.Context, id string, tInvalid int64) error {
	res, err := s.q.ExecContext(ctx,
		`UPDATE memories SET t_invalid = ? WHERE id = ? AND t_invalid IS NULL`, tInvalid, id)
	if err != nil {
		return fmt.Errorf("sqlite: invalidate memory %q: %w", id, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		// 要么不存在，要么已经失效——后者是幂等成功，因此需区分。
		var exists int
		if err := s.q.QueryRowContext(ctx, `SELECT COUNT(1) FROM memories WHERE id = ?`, id).
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
	_, err := s.q.ExecContext(ctx, `
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
	var c store.Concept
	var parentID sql.NullString
	var status string
	err := s.q.QueryRowContext(ctx, `SELECT `+conceptCols+` FROM concepts WHERE id = ?`, id).
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
	_, err := s.q.ExecContext(ctx, `
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
	var a store.Alias
	err := s.q.QueryRowContext(ctx, `
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
