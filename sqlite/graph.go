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
// 执行句柄来自 ctx（见 conn.querier），所以在 Store.Tx 回调内可与 memories 同事务提交。
type graphStore struct{ c *conn }

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
	q, err := g.c.querier(ctx)
	if err != nil {
		return err
	}
	if _, err := q.ExecContext(ctx, `
		INSERT INTO nodes (id, scope, concept_id, canonical_name, source_episode, props_json)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			scope=excluded.scope, concept_id=excluded.concept_id,
			canonical_name=excluded.canonical_name,
			source_episode=COALESCE(excluded.source_episode, nodes.source_episode),
			props_json=excluded.props_json`,
		n.ID, string(n.Scope), nullableText(n.ConceptID), n.CanonicalName,
		nullableText(n.SourceEpisode), props,
	); err != nil {
		return fmt.Errorf("sqlite: put node %q: %w", n.ID, err)
	}
	return nil
}

func (g *graphStore) GetNode(ctx context.Context, id string) (store.Node, error) {
	q, err := g.c.querier(ctx)
	if err != nil {
		return store.Node{}, err
	}
	var n store.Node
	var scope string
	var conceptID, sourceEpisode sql.NullString
	err = q.QueryRowContext(ctx,
		`SELECT id, scope, concept_id, canonical_name, source_episode, props_json
		 FROM nodes WHERE id = ?`, id).
		Scan(&n.ID, &scope, &conceptID, &n.CanonicalName, &sourceEpisode, &n.PropsJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Node{}, store.ErrNotFound
	}
	if err != nil {
		return store.Node{}, fmt.Errorf("sqlite: get node %q: %w", id, err)
	}
	n.Scope = store.Scope(scope)
	n.ConceptID = fromNullableText(conceptID)
	n.SourceEpisode = fromNullableText(sourceEpisode)
	return n, nil
}

const edgeCols = `id, version, from_id, to_id, relation, weight, t_valid, t_invalid,
	t_created, t_expired`

func scanEdge(sc interface{ Scan(...any) error }) (store.Edge, error) {
	var e store.Edge
	var rel string
	var tInvalid, tExpired sql.NullInt64
	if err := sc.Scan(&e.ID, &e.Version, &e.FromID, &e.ToID, &rel, &e.Weight, &e.TValid,
		&tInvalid, &e.TCreated, &tExpired); err != nil {
		return store.Edge{}, err
	}
	e.Relation = store.Relation(rel)
	e.TInvalid = fromNullableInt(tInvalid)
	e.TExpired = fromNullableInt(tExpired)
	return e, nil
}

// PutEdge 写入一条边；同一 ID 的再次写入按 D12 产生新版本（与 memories 一致）。
func (g *graphStore) PutEdge(ctx context.Context, e store.Edge) error {
	if err := e.Validate(); err != nil {
		return err
	}
	q, err := g.c.querier(ctx)
	if err != nil {
		return err
	}
	latest, hasLatest, err := latestEdge(ctx, q, e.ID)
	if err != nil {
		return err
	}
	if !hasLatest {
		return insertEdge(ctx, q, e, 1)
	}
	cur, hasCur, err := currentEdge(ctx, q, e.ID)
	if err != nil {
		return err
	}
	if hasCur && edgeSame(cur, e) {
		return nil // 幂等重写
	}
	// 已失效（世界时间）的边不得因再次写入而复活。
	if e.TInvalid == 0 && latest.TInvalid != 0 {
		e.TInvalid = latest.TInvalid
	}
	next := latest.Version + 1
	if hasCur {
		// t_expired 的真实写入路径：旧版本在新版本生效时刻过期。
		expired := e.TCreated
		if expired <= cur.TCreated {
			expired = cur.TCreated + 1
		}
		if _, err := q.ExecContext(ctx,
			`UPDATE edges SET t_expired = ? WHERE id = ? AND version = ?`,
			expired, cur.ID, cur.Version); err != nil {
			return fmt.Errorf("sqlite: expire edge %q v%d: %w", cur.ID, cur.Version, err)
		}
	}
	return insertEdge(ctx, q, e, next)
}

func edgeSame(a, b store.Edge) bool {
	return a.FromID == b.FromID && a.ToID == b.ToID && a.Relation == b.Relation &&
		a.Weight == b.Weight && a.TValid == b.TValid && a.TInvalid == b.TInvalid &&
		a.TCreated == b.TCreated && a.TExpired == b.TExpired
}

func insertEdge(ctx context.Context, q querier, e store.Edge, version int) error {
	if _, err := q.ExecContext(ctx, `
		INSERT INTO edges (`+edgeCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id, version) DO UPDATE SET
			from_id=excluded.from_id, to_id=excluded.to_id, relation=excluded.relation,
			weight=excluded.weight, t_valid=excluded.t_valid,
			t_invalid=COALESCE(excluded.t_invalid, edges.t_invalid),
			t_created=excluded.t_created,
			t_expired=COALESCE(excluded.t_expired, edges.t_expired)`,
		e.ID, version, e.FromID, e.ToID, string(e.Relation), e.Weight, e.TValid,
		nullableInt(e.TInvalid), e.TCreated, nullableInt(e.TExpired),
	); err != nil {
		return fmt.Errorf("sqlite: put edge %q v%d: %w", e.ID, version, err)
	}
	return nil
}

func currentEdge(ctx context.Context, q querier, id string) (store.Edge, bool, error) {
	row := q.QueryRowContext(ctx,
		`SELECT `+edgeCols+` FROM edges
		 WHERE id = ? AND t_expired IS NULL ORDER BY version DESC LIMIT 1`, id)
	e, err := scanEdge(row)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Edge{}, false, nil
	}
	if err != nil {
		return store.Edge{}, false, fmt.Errorf("sqlite: get current edge %q: %w", id, err)
	}
	return e, true, nil
}

func latestEdge(ctx context.Context, q querier, id string) (store.Edge, bool, error) {
	row := q.QueryRowContext(ctx,
		`SELECT `+edgeCols+` FROM edges WHERE id = ? ORDER BY version DESC LIMIT 1`, id)
	e, err := scanEdge(row)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Edge{}, false, nil
	}
	if err != nil {
		return store.Edge{}, false, fmt.Errorf("sqlite: get latest edge %q: %w", id, err)
	}
	return e, true, nil
}

// Neighbors 用递归 CTE 从 NodeID 出发做 ≤Hops 跳的**定向**遍历（只沿 from→to）。
//
// 关系白名单（§5 R3）由 Relations 传入：传递性关系允许更多跳、非传递关系只给 1 跳，
// 这个决定属于 Recall Planner，GraphStore 只按约束遍历，不猜意图。
//
// 范围隔离（§2、I-2）：Scope 是必填项，SQL 里强制要求起点与终点节点都属于该 scope，
// 因此跨 scope 的边永远不会被遍历出来——这不是性能问题而是隔离缺陷。
//
// 系统时间（B-5）：t_expired 参与判定。默认只返回当前版本（t_expired IS NULL）；
// AsOf 时同时要求 t_created <= asOf < t_expired 与世界时间区间。
//
// 同一跳数内若因环导致同一条边被多次命中，只保留最小 depth 的那次。
func (g *graphStore) Neighbors(ctx context.Context, q store.NeighborQuery) ([]store.EdgeHop, error) {
	if q.NodeID == "" {
		return nil, fmt.Errorf("sqlite: neighbors: empty node id")
	}
	if q.Scope == "" {
		return nil, fmt.Errorf("sqlite: neighbors: empty scope（Scope 是必填的隔离边界）")
	}
	if !q.Scope.Valid() {
		return nil, fmt.Errorf("sqlite: neighbors: invalid scope %q", q.Scope)
	}
	hops := q.Hops
	if hops <= 0 {
		hops = 1
	}
	if hops > maxHops {
		hops = maxHops
	}

	// filterSQL 会被拼进两处（递归项与终极 SELECT），因此 filterArgs 也要重复两遍。
	// 三组条件：scope 隔离（2 个参数）、关系白名单、时间有效性。
	scopeSQL := ` AND fn.scope = ? AND tn.scope = ?`
	scopeArgs := []any{string(q.Scope), string(q.Scope)}

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
		// 世界时间 + 系统时间同时判定（B-5）。
		filterSQL += ` AND e.t_valid <= ? AND (e.t_invalid IS NULL OR e.t_invalid > ?)` +
			` AND e.t_created <= ? AND (e.t_expired IS NULL OR e.t_expired > ?)`
		asOf := *q.AsOf
		filterArgs = append(filterArgs, asOf, asOf, asOf, asOf)
	} else {
		// 默认只看当前版本、当前有效的边，与 memories 的默认语义一致。
		filterSQL += ` AND e.t_invalid IS NULL AND e.t_expired IS NULL`
	}

	// 两处拼接共用同一段过滤条件（递归项与终极 SELECT 各一份）。
	joinSQL := `
		JOIN edges e ON e.from_id = w.node
		JOIN nodes fn ON fn.id = e.from_id
		JOIN nodes tn ON tn.id = e.to_id
		WHERE true` + scopeSQL + filterSQL

	sqlText := `
WITH RECURSIVE walk(node, depth) AS (
	SELECT ?, 0
	UNION
	SELECT e.to_id, w.depth + 1
	FROM walk w` + joinSQL + `
	  AND w.depth < ?
)
SELECT e.id, e.version, e.from_id, e.to_id, e.relation, e.weight, e.t_valid, e.t_invalid,
       e.t_created, e.t_expired, w.depth + 1 AS hop
FROM walk w` + joinSQL + `
  AND w.depth < ?
ORDER BY hop ASC, e.id ASC, e.version DESC`

	args := make([]any, 0, 2+2*(len(scopeArgs)+len(filterArgs)))
	args = append(args, q.NodeID)
	args = append(args, scopeArgs...)
	args = append(args, filterArgs...)
	args = append(args, hops)
	args = append(args, scopeArgs...)
	args = append(args, filterArgs...)
	args = append(args, hops)

	dq, err := g.c.querier(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := dq.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: neighbors %q: %w", q.NodeID, err)
	}
	defer rows.Close()

	best := make(map[string]store.EdgeHop, 8)
	for rows.Next() {
		var hop int
		var raw struct {
			ID  string
			Ver int
			F   string
			T   string
			R   string
			W   float64
			TV  int64
			TI  sql.NullInt64
			TC  int64
			TE  sql.NullInt64
		}
		if err := rows.Scan(&raw.ID, &raw.Ver, &raw.F, &raw.T, &raw.R, &raw.W, &raw.TV,
			&raw.TI, &raw.TC, &raw.TE, &hop); err != nil {
			return nil, fmt.Errorf("sqlite: neighbors scan: %w", err)
		}
		e := store.Edge{
			ID: raw.ID, Version: raw.Ver, FromID: raw.F, ToID: raw.T,
			Relation: store.Relation(raw.R), Weight: raw.W, TValid: raw.TV,
			TInvalid: fromNullableInt(raw.TI), TCreated: raw.TC,
			TExpired: fromNullableInt(raw.TE),
		}
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
