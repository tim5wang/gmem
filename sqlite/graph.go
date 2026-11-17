package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/tim5wang/gmem/store"
)

// graphStore 是 GraphStore 的实现：nodes / edges 两张表 + 递归 CTE。
//
// §11 第 3 条："图不外包给图数据库"——图与记忆在同一个事务边界里，
// 因此邻域查询直接用递归 CTE 完成，无需跨系统一致性。
type graphStore struct{ q querier }

var _ store.GraphStore = (*graphStore)(nil)

// maxHops 是递归深度的硬上限，防止畸形边表让查询失控。
const maxHops = 5

func (g *graphStore) PutNode(ctx context.Context, n store.Node) error {
	if err := n.Validate(); err != nil {
		return err
	}
	props := n.PropsJSON
	if props == "" {
		props = "{}"
	}
	if _, err := g.q.ExecContext(ctx, `
		INSERT INTO nodes (id, scope, concept_id, canonical_name, props_json) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			scope=excluded.scope, concept_id=excluded.concept_id,
			canonical_name=excluded.canonical_name, props_json=excluded.props_json`,
		n.ID, string(n.Scope), nullableText(n.ConceptID), n.CanonicalName, props,
	); err != nil {
		return fmt.Errorf("sqlite: put node %q: %w", n.ID, err)
	}
	return nil
}

func (g *graphStore) GetNode(ctx context.Context, id string) (store.Node, error) {
	var n store.Node
	var scope string
	var conceptID sql.NullString
	err := g.q.QueryRowContext(ctx,
		`SELECT id, scope, concept_id, canonical_name, props_json FROM nodes WHERE id = ?`, id).
		Scan(&n.ID, &scope, &conceptID, &n.CanonicalName, &n.PropsJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Node{}, store.ErrNotFound
	}
	if err != nil {
		return store.Node{}, fmt.Errorf("sqlite: get node %q: %w", id, err)
	}
	n.Scope = store.Scope(scope)
	n.ConceptID = fromNullableText(conceptID)
	return n, nil
}

func (g *graphStore) PutEdge(ctx context.Context, e store.Edge) error {
	if err := e.Validate(); err != nil {
		return err
	}
	if _, err := g.q.ExecContext(ctx, `
		INSERT INTO edges (id, from_id, to_id, relation, weight, t_valid, t_invalid, t_created, t_expired)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			from_id=excluded.from_id, to_id=excluded.to_id, relation=excluded.relation,
			weight=excluded.weight, t_valid=excluded.t_valid, t_invalid=excluded.t_invalid,
			t_created=excluded.t_created, t_expired=excluded.t_expired`,
		e.ID, e.FromID, e.ToID, string(e.Relation), e.Weight, e.TValid,
		nullableInt(e.TInvalid), e.TCreated, nullableInt(e.TExpired),
	); err != nil {
		return fmt.Errorf("sqlite: put edge %q: %w", e.ID, err)
	}
	return nil
}

// Neighbors 用递归 CTE 从 NodeID 出发做 ≤Hops 跳的**定向**遍历（只沿 from→to）。
//
// 关系白名单（§5 R3）由 Relations 传入：传递性关系允许更多跳、非传递关系只给 1 跳，
// 这个决定属于 Recall Planner，GraphStore 只按约束遍历，不猜意图。
//
// 同一跳数内若因环导致同一条边被多次命中，只保留最小 depth 的那次。
func (g *graphStore) Neighbors(ctx context.Context, q store.NeighborQuery) ([]store.EdgeHop, error) {
	if q.NodeID == "" {
		return nil, fmt.Errorf("sqlite: neighbors: empty node id")
	}
	hops := q.Hops
	if hops <= 0 {
		hops = 1
	}
	if hops > maxHops {
		hops = maxHops
	}

	// filterSQL 会被拼进两处（递归项与终极 SELECT），因此 filterArgs 也要重复两遍。
	var filterSQL string
	var filterArgs []any
	if len(q.Relations) > 0 {
		ph := make([]string, len(q.Relations))
		for i, r := range q.Relations {
			ph[i] = "?"
			filterArgs = append(filterArgs, string(r))
		}
		filterSQL += ` AND e.relation IN (` + strings.Join(ph, ",") + `)`
	}
	if q.AsOf != nil {
		// 世界时间：只看该时刻成立的边。
		filterSQL += ` AND e.t_valid <= ? AND (e.t_invalid IS NULL OR e.t_invalid > ?)`
		filterArgs = append(filterArgs, *q.AsOf, *q.AsOf)
	} else {
		// 默认只看当前有效的边，与 memories 的默认语义一致。
		filterSQL += ` AND e.t_invalid IS NULL`
	}

	sqlText := `
WITH RECURSIVE walk(node, depth) AS (
	SELECT ?, 0
	UNION
	SELECT e.to_id, w.depth + 1
	FROM walk w
	JOIN edges e ON e.from_id = w.node
	WHERE w.depth < ?` + filterSQL + `
)
SELECT e.id, e.from_id, e.to_id, e.relation, e.weight, e.t_valid, e.t_invalid,
       e.t_created, e.t_expired, w.depth + 1 AS hop
FROM walk w
JOIN edges e ON e.from_id = w.node
WHERE w.depth < ?` + filterSQL + `
ORDER BY hop ASC, e.id ASC`

	args := make([]any, 0, 2+2*len(filterArgs))
	args = append(args, q.NodeID, hops)
	args = append(args, filterArgs...)
	args = append(args, hops)
	args = append(args, filterArgs...)

	rows, err := g.q.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: neighbors %q: %w", q.NodeID, err)
	}
	defer rows.Close()

	best := make(map[string]store.EdgeHop, 8)
	for rows.Next() {
		var e store.Edge
		var rel string
		var tInvalid, tExpired sql.NullInt64
		var hop int
		if err := rows.Scan(&e.ID, &e.FromID, &e.ToID, &rel, &e.Weight, &e.TValid,
			&tInvalid, &e.TCreated, &tExpired, &hop); err != nil {
			return nil, fmt.Errorf("sqlite: neighbors scan: %w", err)
		}
		e.Relation = store.Relation(rel)
		e.TInvalid = fromNullableInt(tInvalid)
		e.TExpired = fromNullableInt(tExpired)
		if prev, ok := best[e.ID]; ok && prev.Depth <= hop {
			continue // 环导致重复：保留更短的路径
		}
		best[e.ID] = store.EdgeHop{Edge: e, Depth: hop}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: neighbors iterate: %w", err)
	}

	out := make([]store.EdgeHop, 0, len(best))
	for _, h := range best {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Depth != out[j].Depth {
			return out[i].Depth < out[j].Depth
		}
		return out[i].Edge.ID < out[j].Edge.ID
	})
	return out, nil
}
