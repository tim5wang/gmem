package vtest

import (
	"context"
	"math"
	"sort"
	"sync"

	"github.com/tim5wang/gmem/bigram"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// 内存后端：用来验证 "Port 抽象是否真抽象"
//
// 设计依据 docs/TD.md §11 的两条要求：
//   - "编译期注册，不是插件系统"：这里就是一个显式的构造器（newMemoryPorts）。
//   - "一个测试体，两个数据库，同一答案 required"：conformance_test.go 里
//     同一组断言同时跑 SQLite 与内存后端。
//
// 判据（回答"抽象有没有泄漏"）：
//   本文件**不允许** import "database/sql"、任何 SQL 字符串，也不允许 import
//   gmem/sqlite 包。若实现不 import 这些就能满足全部 Port 语义，说明接口确实
//   把 SQL 挡在了后端内部；若某条 Port 语义必须靠 SQL 才能满足，那就是泄漏。
//
// 这个后端**故意不做持久化、不做事务隔离、不做 BM25**：
// 它只需要满足 Port 契约（含 bigram 双侧对称），用来证明上层不依赖 SQLite 细节。
// ===========================================================================

// newMemoryPorts 构造一套纯内存 Port 实现。
func newMemoryPorts() store.Ports {
	shared := &memDB{}
	return store.Ports{
		Store:   &memStore{db: shared},
		Lexical: &memLexical{db: shared},
		Vector:  &memVector{db: shared},
		Graph:   &memGraph{db: shared},
	}
}

// memDB 是四个内存端口共享的状态（对应 SQLite 里"同一个文件"）。
type memDB struct {
	mu       sync.Mutex
	episodes map[string]store.Episode
	memories map[string]store.Memory
	concepts map[string]store.Concept
	aliases  map[string]store.Alias
	docs     map[store.DocumentKey]store.Document
	vectors  map[string]store.Vector
	nodes    map[string]store.Node
	edges    map[string]store.Edge
}

func (m *memDB) init() {
	if m.episodes != nil {
		return
	}
	m.episodes = map[string]store.Episode{}
	m.memories = map[string]store.Memory{}
	m.concepts = map[string]store.Concept{}
	m.aliases = map[string]store.Alias{}
	m.docs = map[store.DocumentKey]store.Document{}
	m.vectors = map[string]store.Vector{}
	m.nodes = map[string]store.Node{}
	m.edges = map[string]store.Edge{}
}

// ---------------------------------------------------------------------------
// Store
// ---------------------------------------------------------------------------

type memStore struct{ db *memDB }

var _ store.Store = (*memStore)(nil)

func (s *memStore) PutEpisode(_ context.Context, e store.Episode) error {
	if err := e.Validate(); err != nil {
		return err
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	s.db.init()
	s.db.episodes[e.ID] = e
	return nil
}

func (s *memStore) GetEpisode(_ context.Context, id string) (store.Episode, error) {
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	s.db.init()
	e, ok := s.db.episodes[id]
	if !ok {
		return store.Episode{}, store.ErrNotFound
	}
	return e, nil
}

func (s *memStore) PutMemory(_ context.Context, m store.Memory) error {
	if err := m.Validate(); err != nil {
		return err
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	s.db.init()
	s.db.memories[m.ID] = m
	return nil
}

func (s *memStore) GetMemory(_ context.Context, id string) (store.Memory, error) {
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	s.db.init()
	m, ok := s.db.memories[id]
	if !ok {
		return store.Memory{}, store.ErrNotFound
	}
	return m, nil
}

func (s *memStore) ListMemories(_ context.Context, q store.MemoryQuery) ([]store.Memory, error) {
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	s.db.init()

	out := make([]store.Memory, 0, len(s.db.memories))
	for _, m := range s.db.memories {
		if !memMatch(m, q) {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TValid != out[j].TValid {
			return out[i].TValid > out[j].TValid
		}
		return out[i].ID < out[j].ID
	})
	if q.Offset > 0 {
		if q.Offset >= len(out) {
			return nil, nil
		}
		out = out[q.Offset:]
	}
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

// memMatch 复刻 SQLite 侧的双时态过滤语义（两条时间轴取交集）。
func memMatch(m store.Memory, q store.MemoryQuery) bool {
	if q.Scope != "" && m.Scope != q.Scope {
		return false
	}
	if len(q.Kinds) > 0 {
		ok := false
		for _, k := range q.Kinds {
			if m.Kind == k {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if q.ConceptID != "" && m.ConceptID != q.ConceptID {
		return false
	}
	if q.SourceEpisode != "" && m.SourceEpisode != q.SourceEpisode {
		return false
	}
	if q.AsOf != nil {
		t := *q.AsOf
		if !(m.TValid <= t) {
			return false
		}
		if !(m.TInvalid == 0 || m.TInvalid > t) {
			return false
		}
		if !(m.TCreated <= t) {
			return false
		}
		if !(m.TExpired == 0 || m.TExpired > t) {
			return false
		}
		return true
	}
	if !q.IncludeInvalid && m.TInvalid != 0 {
		return false
	}
	if !q.IncludeExpired && m.TExpired != 0 {
		return false
	}
	return true
}

func (s *memStore) InvalidateMemory(_ context.Context, id string, tInvalid int64) error {
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	s.db.init()
	m, ok := s.db.memories[id]
	if !ok {
		return store.ErrNotFound
	}
	if m.TInvalid != 0 {
		return nil // 幂等：已失效则保留原 t_invalid
	}
	m.TInvalid = tInvalid
	s.db.memories[id] = m
	return nil
}

// TouchRecall 复刻 D13：只改当前版本的 recall_count，不产生新版本。
func (s *memStore) TouchRecall(_ context.Context, id string) error {
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	s.db.init()
	m, ok := s.db.memories[id]
	if !ok {
		return store.ErrNotFound
	}
	m.RecallCount++
	s.db.memories[id] = m
	return nil
}

func (s *memStore) PutConcept(_ context.Context, c store.Concept) error {
	if err := c.Validate(); err != nil {
		return err
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	s.db.init()
	s.db.concepts[c.ID] = c
	return nil
}

func (s *memStore) GetConcept(_ context.Context, id string) (store.Concept, error) {
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	s.db.init()
	c, ok := s.db.concepts[id]
	if !ok {
		return store.Concept{}, store.ErrNotFound
	}
	return c, nil
}

func (s *memStore) PutAlias(_ context.Context, a store.Alias) error {
	if err := a.Validate(); err != nil {
		return err
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	s.db.init()
	key := a.Surface + "\x00" + a.NodeID
	s.db.aliases[key] = a
	return nil
}

func (s *memStore) GetAlias(_ context.Context, surface string) (store.Alias, error) {
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	s.db.init()
	var best store.Alias
	found := false
	for _, a := range s.db.aliases {
		if a.Surface != surface {
			continue
		}
		if !found || a.Confidence > best.Confidence ||
			(a.Confidence == best.Confidence && a.NodeID < best.NodeID) {
			best, found = a, true
		}
	}
	if !found {
		return store.Alias{}, store.ErrNotFound
	}
	return best, nil
}

// Tx 用"快照 + 回滚"模拟事务；不追求 SQLite 的隔离级别，只满足 Port 契约。
func (s *memStore) Tx(ctx context.Context, fn func(ctx context.Context, tx store.Store) error) error {
	s.db.mu.Lock()
	s.db.init()
	snapshot := s.db.cloneState()
	s.db.mu.Unlock()

	committed := false
	defer func() {
		if committed {
			return
		}
		s.db.mu.Lock()
		s.db.restoreState(snapshot)
		s.db.mu.Unlock()
	}()

	if err := fn(ctx, s); err != nil {
		return err
	}
	committed = true
	return nil
}

func (m *memDB) cloneState() map[string]any {
	mem := make(map[string]store.Memory, len(m.memories))
	for k, v := range m.memories {
		mem[k] = v
	}
	ep := make(map[string]store.Episode, len(m.episodes))
	for k, v := range m.episodes {
		ep[k] = v
	}
	return map[string]any{"memories": mem, "episodes": ep}
}

func (m *memDB) restoreState(snap map[string]any) {
	m.memories = snap["memories"].(map[string]store.Memory)
	m.episodes = snap["episodes"].(map[string]store.Episode)
}

// ---------------------------------------------------------------------------
// LexicalIndex（内存版）
//
// 关键：**与 SQLite 后端一样，两侧都调用 bigram.Tokens**。
// 若 bigram 逻辑只存在于 sqlite 包内部，这个后端就无法实现同一语义
// （那才是真正的抽象泄漏）——它能实现，恰好证明 bigram 是可复用的独立关注点。
// ---------------------------------------------------------------------------

type memLexical struct{ db *memDB }

var _ store.LexicalIndex = (*memLexical)(nil)

func (l *memLexical) Index(_ context.Context, docs []store.Document) error {
	l.db.mu.Lock()
	defer l.db.mu.Unlock()
	l.db.init()
	for _, d := range docs {
		if d.ID == "" {
			return errEmptyDocID
		}
		key := store.DocumentKey{ID: d.ID, Version: d.Key().VersionOr1()}
		// 只覆盖同一 (id, version)：同 ID 的其它版本必须原样保留（D13）。
		d.Version = key.Version
		l.db.docs[key] = d
	}
	return nil
}

func (l *memLexical) Delete(_ context.Context, keys []store.DocumentKey) error {
	l.db.mu.Lock()
	defer l.db.mu.Unlock()
	l.db.init()
	for _, k := range keys {
		delete(l.db.docs, store.DocumentKey{ID: k.ID, Version: k.VersionOr1()})
	}
	return nil
}

// Search 用 token 交集计数代替 BM25：仍然是"索引侧 bigram token vs 查询侧 bigram token"。
//
// 版本语义与 SQLite 后端一致（D13）：命中带 Version；
// AsOf 为 nil 时只返回主表 t_expired 为空的版本，非 nil 时按 t_created/t_expired 回溯。
func (l *memLexical) Search(_ context.Context, lq store.LexicalQuery) ([]store.Hit, error) {
	expr := bigram.QueryText(lq.Query)
	if expr == "" {
		return nil, nil
	}
	limit := lq.Limit
	if limit <= 0 {
		limit = 10
	}
	// 查询侧 token = 从 QueryText 的表达式里还原，强制走与 SQLite 侧相同的入口。
	queryToks := bigram.Tokens(lq.Query)

	l.db.mu.Lock()
	defer l.db.mu.Unlock()
	l.db.init()

	type scored struct {
		id      string
		version int
		s       float64
	}
	var all []scored
	for _, d := range l.db.docs {
		if lq.Scope != "" && d.Scope != lq.Scope {
			continue
		}
		if !memLexVisible(l.db, d, lq.AsOf) {
			continue
		}
		// 索引侧 token：同样来自 bigram，而不是原文整串。
		docToks := bigram.Tokens(d.Text)
		set := make(map[string]bool, len(docToks))
		for _, t := range docToks {
			set[t] = true
		}
		hits := 0
		for _, qt := range queryToks {
			if set[qt] {
				hits++
			}
		}
		if hits == 0 {
			continue
		}
		all = append(all, scored{id: d.ID, version: d.Key().VersionOr1(), s: float64(hits)})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].s != all[j].s {
			return all[i].s > all[j].s
		}
		if all[i].id != all[j].id {
			return all[i].id < all[j].id
		}
		return all[i].version < all[j].version
	})
	if len(all) > limit {
		all = all[:limit]
	}
	out := make([]store.Hit, len(all))
	for i, s := range all {
		out[i] = store.Hit{ID: s.id, Version: s.version, Score: s.s, Rank: i + 1}
	}
	return out, nil
}

// memLexVisible 复刻 SQLite 侧的双时态过滤（LEFT JOIN memories 的语义）：
//   - 主表没有该 ID：默认可见（索引可独立存在），as_of 时不可见（无法断言当时存在）；
//   - 主表有：以主表的 t_created / t_expired 为准。
func memLexVisible(db *memDB, d store.Document, asOf *int64) bool {
	m, ok := db.memories[d.ID]
	if !ok {
		return asOf == nil
	}
	if asOf != nil {
		t := *asOf
		return m.TCreated <= t && (m.TExpired == 0 || m.TExpired > t)
	}
	return m.TExpired == 0
}

// ---------------------------------------------------------------------------
// VectorIndex（内存版：不量化，直接存 float32）
// ---------------------------------------------------------------------------

type memVector struct{ db *memDB }

var _ store.VectorIndex = (*memVector)(nil)

func (v *memVector) Upsert(_ context.Context, vectors []store.Vector) error {
	v.db.mu.Lock()
	defer v.db.mu.Unlock()
	v.db.init()
	for _, vec := range vectors {
		if vec.ID == "" {
			return errEmptyVectorID
		}
		v.db.vectors[vec.ID] = vec
	}
	return nil
}

func (v *memVector) Delete(_ context.Context, ids []string) error {
	v.db.mu.Lock()
	defer v.db.mu.Unlock()
	v.db.init()
	for _, id := range ids {
		delete(v.db.vectors, id)
	}
	return nil
}

func (v *memVector) Search(_ context.Context, query []float32, scope store.Scope, k int) ([]store.Hit, error) {
	if len(query) == 0 {
		return nil, nil
	}
	if k <= 0 {
		k = 10
	}
	v.db.mu.Lock()
	defer v.db.mu.Unlock()
	v.db.init()

	type scored struct {
		id string
		s  float64
	}
	var all []scored
	for _, vec := range v.db.vectors {
		if scope != "" && vec.Scope != scope {
			continue
		}
		if len(vec.Values) != len(query) {
			continue
		}
		all = append(all, scored{id: vec.ID, s: memCosine(query, vec.Values)})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].s != all[j].s {
			return all[i].s > all[j].s
		}
		return all[i].id < all[j].id
	})
	if len(all) > k {
		all = all[:k]
	}
	out := make([]store.Hit, len(all))
	for i, s := range all {
		out[i] = store.Hit{ID: s.id, Score: s.s, Rank: i + 1}
	}
	return out, nil
}

// Snapshot 内存后端不提供快照格式（Port 注释明确写"快照只是加速物"），
// 返回空字节并可被上层忽略——这也是抽象是否真的可替换的检验点之一。
func (v *memVector) Snapshot(_ context.Context, _ store.Scope) ([]byte, error) {
	return nil, nil
}

func memCosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		av, bv := float64(a[i]), float64(b[i])
		dot += av * bv
		na += av * av
		nb += bv * bv
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// ---------------------------------------------------------------------------
// GraphStore（内存版：BFS 代替递归 CTE）
// ---------------------------------------------------------------------------

type memGraph struct{ db *memDB }

var _ store.GraphStore = (*memGraph)(nil)

func (g *memGraph) PutNode(_ context.Context, n store.Node) error {
	if err := n.Validate(); err != nil {
		return err
	}
	g.db.mu.Lock()
	defer g.db.mu.Unlock()
	g.db.init()
	if n.PropsJSON == "" {
		n.PropsJSON = "{}"
	}
	g.db.nodes[n.ID] = n
	return nil
}

func (g *memGraph) GetNode(_ context.Context, id string) (store.Node, error) {
	g.db.mu.Lock()
	defer g.db.mu.Unlock()
	g.db.init()
	n, ok := g.db.nodes[id]
	if !ok {
		return store.Node{}, store.ErrNotFound
	}
	return n, nil
}

func (g *memGraph) PutEdge(_ context.Context, e store.Edge) error {
	if err := e.Validate(); err != nil {
		return err
	}
	g.db.mu.Lock()
	defer g.db.mu.Unlock()
	g.db.init()
	// 模拟 SQLite 的 FOREIGN KEY 约束（外键检查在后端内部，不泄漏到上层）。
	if _, ok := g.db.nodes[e.FromID]; !ok {
		return errMissingNode
	}
	if _, ok := g.db.nodes[e.ToID]; !ok {
		return errMissingNode
	}
	g.db.edges[e.ID] = e
	return nil
}

func (g *memGraph) Neighbors(_ context.Context, q store.NeighborQuery) ([]store.EdgeHop, error) {
	if q.NodeID == "" {
		return nil, errEmptyNodeID
	}
	// 与 SQLite 后端一致：Scope 是必填的隔离边界，不是"不限"。
	if q.Scope == "" || !q.Scope.Valid() {
		return nil, errEmptyScope
	}
	hops := q.Hops
	if hops <= 0 {
		hops = 1
	}
	if hops > 5 {
		hops = 5
	}

	g.db.mu.Lock()
	defer g.db.mu.Unlock()
	g.db.init()

	allowed := func(e store.Edge) bool {
		// 范围隔离：起点与终点节点都必须属于该 scope。
		fn, ok := g.db.nodes[e.FromID]
		if !ok || fn.Scope != q.Scope {
			return false
		}
		tn, ok := g.db.nodes[e.ToID]
		if !ok || tn.Scope != q.Scope {
			return false
		}
		if len(q.Relations) > 0 {
			ok := false
			for _, r := range q.Relations {
				if e.Relation == r {
					ok = true
					break
				}
			}
			if !ok {
				return false
			}
		}
		if q.AsOf != nil {
			t := *q.AsOf
			if !(e.TValid <= t) {
				return false
			}
			if !(e.TInvalid == 0 || e.TInvalid > t) {
				return false
			}
			// 系统时间轴：t_created <= asOf < t_expired。
			if !(e.TCreated <= t) {
				return false
			}
			if !(e.TExpired == 0 || e.TExpired > t) {
				return false
			}
			return true
		}
		return e.TInvalid == 0 && e.TExpired == 0
	}

	if !q.Direction.Valid() {
		return nil, errEmptyDirection
	}
	direction := q.Direction.Resolve()

	// match / next 与 sqlite 后端的递归项同构：方向决定“看哪一端相等”与“前进目标”。
	// 在两端都要保持一致，否则 conformance 用例就会变成“一个测试体，两个答案”。
	match := func(e store.Edge, node string) bool {
		switch direction {
		case store.DirectionInbound:
			return e.ToID == node
		case store.DirectionBoth:
			return e.FromID == node || e.ToID == node
		}
		return e.FromID == node
	}
	next := func(e store.Edge, node string) string {
		if direction == store.DirectionBoth && e.FromID != node {
			return e.FromID
		}
		if direction == store.DirectionInbound {
			return e.FromID
		}
		return e.ToID
	}

	best := map[string]store.EdgeHop{}
	frontier := []string{q.NodeID}
	for depth := 1; depth <= hops; depth++ {
		var nextFrontier []string
		for _, node := range frontier {
			for _, e := range g.db.edges {
				if !match(e, node) || !allowed(e) {
					continue
				}
				target := next(e, node)
				if prev, ok := best[e.ID]; !ok || depth < prev.Depth {
					best[e.ID] = store.EdgeHop{Edge: e, Depth: depth, Reached: target}
				}
				nextFrontier = append(nextFrontier, target)
			}
		}
		frontier = nextFrontier
		if len(frontier) == 0 {
			break
		}
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

// ---------------------------------------------------------------------------
// 测试侧错误值（不引入生产代码的未导出错误）
// ---------------------------------------------------------------------------

type memErr string

func (e memErr) Error() string { return string(e) }

const (
	errEmptyDocID     = memErr("mem: empty document id")
	errEmptyVectorID  = memErr("mem: empty vector id")
	errMissingNode    = memErr("mem: missing node (foreign key)")
	errEmptyNodeID    = memErr("mem: empty node id")
	errEmptyScope     = memErr("mem: empty scope")
	errEmptyDirection = memErr("mem: invalid neighbor direction")
)
