package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/tim5wang/gmem/store"
)

// OntologyReader 让 storeImpl 满足 store.OntologyReader（P1 Concept Aligner 的候选来源）。
//
// 为什么挂在 storeImpl 上而不是另建一个类型：它必须与 Store 是**同一个**执行句柄，
// 这样在 Store.Tx 回调里枚举本体时也落在同一事务边界（虽然对齐是只读操作，
// 但"看得见未提交写入"对写入后的即时召回是必要的）。
var _ store.OntologyReader = (*storeImpl)(nil)

// ListConcepts 返回全部概念。
//
// 不做状态过滤：R2 要求"只有 certified 参与硬契约"，但 draft 与 deprecated
// 仍需出现在对齐候选里（前者打折、后者归零），因此状态判定归召回侧，
// 存储层只如实返回。排序保证确定性（对齐时的并列取先出现的）。
func (s *storeImpl) ListConcepts(ctx context.Context) ([]store.Concept, error) {
	q, err := s.c.querier(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx,
		`SELECT `+conceptCols+` FROM concepts ORDER BY status DESC, name ASC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list concepts: %w", err)
	}
	defer rows.Close()

	out := make([]store.Concept, 0, 16)
	for rows.Next() {
		var c store.Concept
		var parentID sql.NullString
		var status, origin string
		if err := rows.Scan(&c.ID, &c.Name, &parentID, &status, &c.Definition, &origin); err != nil {
			return nil, fmt.Errorf("sqlite: list concepts scan: %w", err)
		}
		c.ParentID = fromNullableText(parentID)
		c.Status = store.ConceptStatus(status)
		c.Origin = store.ConceptOrigin(origin)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list concepts iterate: %w", err)
	}
	return out, nil
}

// ListAliases 返回全部词典项。
//
// 同一 surface 可能映射到多个 node（歧义尚未消解），这里**全部返回**，
// 由对齐侧决定取舍（P1 按置信度+最长匹配，P2 消歧管线接入后再做三区判定）。
func (s *storeImpl) ListAliases(ctx context.Context) ([]store.Alias, error) {
	q, err := s.c.querier(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx,
		`SELECT surface, node_id, source, confidence FROM aliases
		 ORDER BY confidence DESC, surface ASC, node_id ASC`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list aliases: %w", err)
	}
	defer rows.Close()

	out := make([]store.Alias, 0, 16)
	for rows.Next() {
		var a store.Alias
		if err := rows.Scan(&a.Surface, &a.NodeID, &a.Source, &a.Confidence); err != nil {
			return nil, fmt.Errorf("sqlite: list aliases scan: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list aliases iterate: %w", err)
	}
	return out, nil
}
