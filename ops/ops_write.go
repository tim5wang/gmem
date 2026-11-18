package ops

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"time"

	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// 写入型原子 op：Merge / LinkUpdate / Invalidate / Promote
//
// 这四个 op 的 Apply 都只改"图、本体、状态"，**不写 memories 内容**。
// 唯一例外是 Invalidate 的 t_invalid（世界时间）——它也走 Store.InvalidateMemory
// 而不是 PutMemory，因此不会催生版本（D12/D13）。
// ===========================================================================

// 图关系里 P2 已经定义的两条记账边（与 extract 包同名，但 ops 不 import extract
// 的这个常量：重复一份字符串比让 ops 依赖提取层的配置更稳）。
const (
	relationPossiblySame   store.Relation = "possiblySame"
	relationNegativeSameAs store.Relation = "negative_SameAs"
)

// ---------------------------------------------------------------------------
// 输入结构
// ---------------------------------------------------------------------------

// ExtractTarget 指定一个要提取的 episode（为空则扫"尚无派生记忆"的 episode）。
type ExtractTarget struct {
	EpisodeID string `json:"episode_id"`
}

// PairRef 是一对疑似重复的实体。
type PairRef struct {
	LeftNode  string `json:"left_node"`
	RightNode string `json:"right_node"`
}

// PairVerdict 是 Agent/人对疑似重复对的裁决。
//
// Verdict 取值："same"（同一实体 → Merge）、"different"（不同实体 → negative_SameAs）、
// 空（保持待审）。
type PairVerdict struct {
	LeftNode  string `json:"left_node"`
	RightNode string `json:"right_node"`
	Verdict   string `json:"verdict"`
	Note      string `json:"note,omitempty"`
}

// MergeInput 是 Merge 的输入。
type MergeInput struct {
	FromNode string `json:"from_node"` // 被并入的一方（旧 ID）
	ToNode   string `json:"to_node"`   // 保留的一方（新 ID）
	Reason   string `json:"reason,omitempty"`
}

// LinkInput 是 LinkUpdate 的输入。
type LinkInput struct {
	FromNode string         `json:"from_node"`
	ToNode   string         `json:"to_node"`
	Relation store.Relation `json:"relation"`
	Weight   float64        `json:"weight,omitempty"`
	// Remove 为 true 则失效这条边（Agent 的 gmem_unlink）。
	Remove bool `json:"remove,omitempty"`
}

// InvalidateInput 是 Invalidate 的输入。
type InvalidateInput struct {
	MemoryID string `json:"memory_id"`
	TInvalid int64  `json:"t_invalid,omitempty"` // 世界时间；0 = 现在
	Reason   string `json:"reason,omitempty"`
}

// PromoteInput 是 Promote 的输入（D15 清理路径之一）。
type PromoteInput struct {
	ConceptIDs []string `json:"concept_ids,omitempty"`
	// All 为 true 时把"已在使用中"的 draft 全量认证（use_count >= MinUse）。
	All bool `json:"all,omitempty"`
	// MinUse 是 All 模式下的使用次数门槛（默认 1：至少被写过一次）。
	MinUse int `json:"min_use,omitempty"`
}

// ---------------------------------------------------------------------------
// 公共工具
// ---------------------------------------------------------------------------

// shortHash 是稳定的 8 位十六进制摘要（与 extract 同算法，但 ops 不跨包依赖内部符号）。
func shortHash(parts ...string) string {
	h := fnv.New32a()
	for i, p := range parts {
		if i > 0 {
			_, _ = h.Write([]byte{0})
		}
		_, _ = h.Write([]byte(strings.ToLower(strings.TrimSpace(p))))
	}
	const hexDigits = "0123456789abcdef"
	v := h.Sum32()
	buf := make([]byte, 8)
	for i := 0; i < 8; i++ {
		buf[7-i] = hexDigits[v&0xf]
		v >>= 4
	}
	return string(buf)
}

// edgeID 生成确定性边 ID：同一 (relation, from, to) 重复写入必须落到同一条边，
// 否则"单值关系失效旧边"（TD §6 T2）会变成"每次多一条并列边"。
func edgeID(rel store.Relation, from, to string) string {
	return "e-" + shortHash("edge", string(rel), from, to)
}

// nodePairKey 是**无序**对键（疑似重复不分左右）。
func nodePairKey(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return a + "\x00" + b
}

// sortStrings 返回排序后的副本（审计与幂等摘要都要确定性）。
func sortStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// marshalJSON 是审计用的稳定序列化（失败时退化为空串，不掩盖主结论）。
func marshalJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// Merge
// ---------------------------------------------------------------------------

type mergeOp struct{}

func (mergeOp) Name() OpName  { return OpMerge }
func (mergeOp) Budget() Budget {
	// ⚠️ 待校准：20s 是设计起点（合并要迁移边与 alias，比单条写入重）。
	return Budget{Duration: 20 * time.Second, Steps: 500, IO: 2000}
}

func (mergeOp) Plan(txc TxContext) ([]Step, error) {
	in := txc.Input()
	if in.Merge == nil {
		return nil, fmt.Errorf("ops: merge 需要输入 Merge{FromNode, ToNode}")
	}
	m := in.Merge
	if m.FromNode == "" || m.ToNode == "" {
		return nil, fmt.Errorf("ops: merge 的两个节点都必填")
	}
	if m.FromNode == m.ToNode {
		return nil, fmt.Errorf("ops: merge 不能把 %q 并入自己", m.FromNode)
	}
	// 计划阶段就校验两个节点存在且同 scope：把不可执行的计划交给 Agent 看是误导。
	for _, id := range []string{m.FromNode, m.ToNode} {
		if _, err := txc.Ports().Graph.GetNode(txc.Context(), id); err != nil {
			return nil, fmt.Errorf("ops: merge 目标节点 %q 不可读: %w", id, err)
		}
	}
	// Subject 取**被并入的旧 ID**：它是审计检索的主键（"这个旧实体的下落"），
	// 新 ID 写在 Note 与 Diff.Summary 里，两者都能从一条记录里读到。
	return []Step{{
		ID:      "merge:" + shortHash(m.FromNode, m.ToNode),
		Kind:    "merge_nodes",
		Subject: m.FromNode,
		Data:    m,
		Note:    fmt.Sprintf("并入 %s：迁移边与 alias，旧 ID 保留为重定向", m.ToNode),
	}}, nil
}

func (mergeOp) Apply(txc TxContext, s Step) (Diff, error) {
	m, ok := s.Data.(*MergeInput)
	if !ok {
		return Diff{}, fmt.Errorf("ops: merge: 步骤载荷类型错误")
	}
	return mergeNodes(txc, m.FromNode, m.ToNode, m.Reason)
}

// mergeNodes 是 Merge 的唯一实现（Disambiguate 的"裁决为同一实体"也调它，
// 因此两条入口的行为不可能漂移 —— 这是 D3 的直接要求）。
func mergeNodes(txc TxContext, from, to, reason string) (Diff, error) {
	ctx := txc.Context()
	g := txc.Ports().Graph
	be := txc.Backend()
	now := txc.Now()

	src, err := g.GetNode(ctx, from)
	if err != nil {
		return Diff{}, fmt.Errorf("ops: merge: 读节点 %q: %w", from, err)
	}
	dst, err := g.GetNode(ctx, to)
	if err != nil {
		return Diff{}, fmt.Errorf("ops: merge: 读节点 %q: %w", to, err)
	}
	if src.Scope != txc.Scope() || dst.Scope != txc.Scope() {
		return Diff{}, fmt.Errorf("ops: merge: 节点 %q/%q 不在 scope %q 内（隔离边界）", from, to, txc.Scope())
	}

	edges, err := be.ListEdges(ctx, txc.Scope(), 0)
	if err != nil {
		return Diff{}, err
	}

	var (
		before = map[string]any{"from": from, "to": to, "edges": []string{}}
		after  = map[string]any{"from": from, "to": to, "edges": []string{}}
		moved  []string
		deg    []string
	)
	beforeEdges := []string{}
	afterEdges := []string{}
	for _, e := range edges {
		if e.FromID != from && e.ToID != from {
			continue
		}
		if err := txc.Check(); err != nil {
			return Diff{}, err
		}
		newFrom, newTo := e.FromID, e.ToID
		if newFrom == from {
			newFrom = to
		}
		if newTo == from {
			newTo = to
		}
		if newFrom == newTo {
			// 自环：合并后没有语义，直接失效旧边（保留历史）。
			if err := invalidateEdge(txc, e.ID, now.Unix()); err != nil {
				return Diff{}, err
			}
			beforeEdges = append(beforeEdges, e.ID)
			continue
		}
		nid := edgeID(e.Relation, newFrom, newTo)
		newEdge := store.Edge{
			ID: nid, FromID: newFrom, ToID: newTo, Relation: e.Relation, Weight: e.Weight,
			TValid: now.Unix(), TCreated: now.Unix(),
		}
		if err := g.PutEdge(ctx, newEdge); err != nil {
			return Diff{}, fmt.Errorf("ops: merge: 迁移边 %q → %q: %w", e.ID, nid, err)
		}
		// 旧边失效（世界时间），但它**留在库里**：as_of 回溯仍能看见它。
		if err := invalidateEdge(txc, e.ID, now.Unix()); err != nil {
			return Diff{}, err
		}
		moved = append(moved, nid)
		beforeEdges = append(beforeEdges, e.ID)
		afterEdges = append(afterEdges, nid)
	}

	// alias 迁移：只新增不够，必须把旧的指向改掉，否则旧节点仍会被召回候选带回来。
	or, ok := txc.Store().(store.OntologyReader)
	aliasesMigrated := 0
	if !ok || or == nil {
		deg = append(deg, "后端未实现 OntologyReader：alias 迁移跳过（词典可能仍指向旧节点）")
	} else {
		aliases, err := or.ListAliases(ctx)
		if err != nil {
			return Diff{}, err
		}
		for _, a := range aliases {
			if a.NodeID != from {
				continue
			}
			if err := be.ReassignAlias(ctx, a.Surface, from, to); err != nil {
				return Diff{}, fmt.Errorf("ops: merge: 迁移 alias %q: %w", a.Surface, err)
			}
			aliasesMigrated++
		}
	}

	// 旧 ID 保留为重定向（TD §4：Merge 保留旧 ID 为重定向）。
	if err := be.PutRedirect(ctx, Redirect{
		OldID: from, NewID: to, Scope: txc.Scope(), Reason: reason, At: now.UnixMilli(),
	}); err != nil {
		return Diff{}, err
	}

	before["edges"] = beforeEdges
	after["edges"] = afterEdges

	return Diff{
		Changed: true,
		Summary: fmt.Sprintf("merge %s → %s：迁移 %d 条边、%d 个 alias，旧 ID 保留为重定向",
			from, to, len(moved), aliasesMigrated),
		Before:   marshalJSON(before),
		After:    marshalJSON(after),
		Subjects: []string{from, to},
		Degraded: deg,
	}, nil
}

// invalidateEdge 只置 t_invalid（世界时间），不产生新内容版本也不删除。
func invalidateEdge(txc TxContext, id string, tInvalid int64) error {
	ctx := txc.Context()
	g := txc.Ports().Graph
	// 读当前版本再写回：GraphStore 没有 InvalidateEdge（P0 契约），
	// 而"只置 t_invalid"必须走 PutEdge 的新版本——旧版本保留，as_of 仍可回溯。
	edges, err := txc.Backend().ListEdges(ctx, txc.Scope(), 0)
	if err != nil {
		return err
	}
	for _, e := range edges {
		if e.ID != id {
			continue
		}
		if e.TInvalid != 0 {
			return nil // 已失效：幂等
		}
		next := e
		next.Version = 0 // 由后端分配新版本
		next.TInvalid = tInvalid
		next.TExpired = 0
		return g.PutEdge(ctx, next)
	}
	return nil
}

// ---------------------------------------------------------------------------
// LinkUpdate
// ---------------------------------------------------------------------------

type linkOp struct{}

func (linkOp) Name() OpName  { return OpLinkUpdate }
func (linkOp) Budget() Budget { return Budget{Steps: 10, IO: 100} }

func (linkOp) Plan(txc TxContext) ([]Step, error) {
	in := txc.Input()
	if in.Link == nil {
		return nil, fmt.Errorf("ops: link_update 需要输入 Link{FromNode, ToNode, Relation}")
	}
	l := in.Link
	if l.FromNode == "" || l.ToNode == "" || l.Relation == "" {
		return nil, fmt.Errorf("ops: link_update: from/to/relation 都必填")
	}
	for _, id := range []string{l.FromNode, l.ToNode} {
		if _, err := txc.Ports().Graph.GetNode(txc.Context(), id); err != nil {
			return nil, fmt.Errorf("ops: link_update: 节点 %q 不可读: %w", id, err)
		}
	}
	kind := "create_edge"
	if l.Remove {
		kind = "remove_edge"
	}
	return []Step{{
		ID: kind + ":" + edgeID(l.Relation, l.FromNode, l.ToNode), Kind: kind,
		Subject: l.FromNode + " -[" + string(l.Relation) + "]-> " + l.ToNode, Data: l,
		Note: "单值关系写入时旧边按 t_invalid 失效（TD §6 T2）",
	}}, nil
}

func (linkOp) Apply(txc TxContext, s Step) (Diff, error) {
	l, ok := s.Data.(*LinkInput)
	if !ok {
		return Diff{}, fmt.Errorf("ops: link_update: 步骤载荷类型错误")
	}
	ctx := txc.Context()
	now := txc.Now()
	id := edgeID(l.Relation, l.FromNode, l.ToNode)

	if l.Remove {
		if err := invalidateEdge(txc, id, now.Unix()); err != nil {
			return Diff{}, err
		}
		return Diff{
			Changed: true, Summary: "失效边 " + id,
			Before: marshalJSON(map[string]any{"edge": id, "t_invalid": 0}),
			After:  marshalJSON(map[string]any{"edge": id, "t_invalid": now.Unix()}),
			Subjects: []string{id},
		}, nil
	}

	weight := l.Weight
	if weight <= 0 {
		weight = 1
	}
	e := store.Edge{
		ID: id, FromID: l.FromNode, ToID: l.ToNode, Relation: l.Relation,
		Weight: weight, TValid: now.Unix(), TCreated: now.Unix(),
	}
	if err := txc.Ports().Graph.PutEdge(ctx, e); err != nil {
		return Diff{}, err
	}
	// 单值关系：同一 (from, relation) 上其它目标的边必须失效（TD §6 T2）。
	var invalidated []string
	if singleValued(l.Relation) {
		edges, err := txc.Backend().ListEdges(ctx, txc.Scope(), 0)
		if err != nil {
			return Diff{}, err
		}
		for _, old := range edges {
			if old.ID == id || old.FromID != l.FromNode || old.Relation != l.Relation {
				continue
			}
			if old.TInvalid != 0 {
				continue
			}
			if err := invalidateEdge(txc, old.ID, now.Unix()); err != nil {
				return Diff{}, err
			}
			invalidated = append(invalidated, old.ID)
		}
	}
	return Diff{
		Changed: true,
		Summary: fmt.Sprintf("写入边 %s（%s），失效同源单值旧边 %d 条", id, l.Relation, len(invalidated)),
		Before:  "",
		After: marshalJSON(map[string]any{
			"edge": id, "from": l.FromNode, "to": l.ToNode,
			"relation": string(l.Relation), "weight": weight, "invalidated": invalidated,
		}),
		Subjects: append([]string{id}, invalidated...),
	}, nil
}

// singleValued 报告该关系是否单值（同 from+relation 只能有一个 to）。
//
// 这份表与 TD §5 的关系白名单同口径：works_at / owns 是典型单值关系
// （一个人一个雇主、一个资产一个所有者），part_of/subtype_of 不是。
func singleValued(r store.Relation) bool {
	switch r {
	case store.RelationWorksAt, store.RelationOwns:
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Invalidate
// ---------------------------------------------------------------------------

type invalidateOp struct{}

func (invalidateOp) Name() OpName    { return OpInvalidate }
func (invalidateOp) Budget() Budget  { return Budget{Steps: 50, IO: 500} }

func (invalidateOp) Plan(txc TxContext) ([]Step, error) {
	in := txc.Input()
	if in.Invalidate == nil {
		return nil, fmt.Errorf("ops: invalidate 需要输入 Invalidate{MemoryID}")
	}
	if in.Invalidate.MemoryID == "" {
		return nil, fmt.Errorf("ops: invalidate: MemoryID 必填")
	}
	m, err := txc.Store().GetMemory(txc.Context(), in.Invalidate.MemoryID)
	if err != nil {
		return nil, fmt.Errorf("ops: invalidate: 读记忆 %q: %w", in.Invalidate.MemoryID, err)
	}
	if m.Scope != txc.Scope() {
		return nil, fmt.Errorf("ops: invalidate: 记忆 %q 不在 scope %q 内", m.ID, txc.Scope())
	}
	return []Step{{
		ID: "invalidate:" + m.ID, Kind: "invalidate_memory", Subject: m.ID,
		Data: in.Invalidate, Note: "只置 t_invalid，不删除（as_of 仍可回溯）",
	}}, nil
}

func (invalidateOp) Apply(txc TxContext, s Step) (Diff, error) {
	in, ok := s.Data.(*InvalidateInput)
	if !ok {
		return Diff{}, fmt.Errorf("ops: invalidate: 步骤载荷类型错误")
	}
	ctx := txc.Context()
	m, err := txc.Store().GetMemory(ctx, in.MemoryID)
	if err != nil {
		return Diff{}, err
	}
	tInvalid := in.TInvalid
	if tInvalid <= 0 {
		tInvalid = txc.Now().Unix()
	}
	before := marshalJSON(map[string]any{"id": m.ID, "t_invalid": m.TInvalid, "content": m.Content})
	if m.TInvalid != 0 {
		// 已失效：幂等空转（重复 supersede 不该产生第二次 diff）。
		d, err := stateManager{txc: txc}.move(m, StatusInvalidated, "supersede")
		if err != nil {
			return Diff{}, err
		}
		return Diff{Changed: false, Summary: "记忆 " + m.ID + " 已失效（幂等）",
			Before: before, After: before, Subjects: []string{m.ID}, Warnings: d.Warnings}, nil
	}
	if err := txc.Store().InvalidateMemory(ctx, m.ID, tInvalid); err != nil {
		return Diff{}, err
	}
	sm := stateManager{txc: txc}
	if _, err := sm.move(m, StatusInvalidated, "supersede"); err != nil {
		return Diff{}, err
	}
	return Diff{
		Changed: true, Summary: fmt.Sprintf("记忆 %s 置 t_invalid=%d（状态 → invalidated，仍可 as_of 回溯）", m.ID, tInvalid),
		Before: before,
		After:  marshalJSON(map[string]any{"id": m.ID, "t_invalid": tInvalid, "status": string(StatusInvalidated)}),
		Subjects: []string{m.ID},
	}, nil
}

// ---------------------------------------------------------------------------
// Promote（D15：draft → certified）
// ---------------------------------------------------------------------------

type promoteOp struct{}

func (promoteOp) Name() OpName   { return OpPromote }
func (promoteOp) Budget() Budget { return Budget{Steps: 200, IO: 500} }

func (promoteOp) Plan(txc TxContext) ([]Step, error) {
	ctx := txc.Context()
	in := txc.Input()
	p := in.Promote
	if p == nil {
		p = &PromoteInput{}
	}
	var ids []string
	if len(p.ConceptIDs) > 0 {
		ids = append(ids, p.ConceptIDs...)
	}
	if p.All {
		or, ok := txc.Store().(store.OntologyReader)
		if !ok || or == nil {
			return nil, fmt.Errorf("ops: promote: 后端未实现 OntologyReader，无法枚举 draft")
		}
		concepts, err := or.ListConcepts(ctx)
		if err != nil {
			return nil, err
		}
		minUse := p.MinUse
		if minUse <= 0 {
			minUse = 1
		}
		for _, c := range concepts {
			if c.Status != store.ConceptDraft {
				continue
			}
			stat, ok, err := txc.Backend().GetConceptStat(ctx, c.ID)
			if err != nil {
				return nil, err
			}
			if ok && stat.UseCount >= minUse {
				ids = append(ids, c.ID)
			}
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	out := make([]Step, 0, len(ids))
	for _, id := range sortStrings(ids) {
		c, err := txc.Store().GetConcept(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("ops: promote: 读概念 %q: %w", id, err)
		}
		if c.Status == store.ConceptCertified {
			continue // 幂等：已是 certified 不进计划
		}
		if c.Status == store.ConceptDeprecated {
			continue // 弃用概念不自动复活
		}
		out = append(out, Step{
			ID: "promote:" + id, Kind: "promote_concept", Subject: id,
			Data: c.ID, Note: "draft → certified：此后参与召回侧硬契约（R2）",
		})
	}
	return out, nil
}

func (promoteOp) Apply(txc TxContext, s Step) (Diff, error) {
	id, ok := s.Data.(string)
	if !ok {
		return Diff{}, fmt.Errorf("ops: promote: 步骤载荷类型错误")
	}
	ctx := txc.Context()
	c, err := txc.Store().GetConcept(ctx, id)
	if err != nil {
		return Diff{}, err
	}
	if c.Status == store.ConceptCertified {
		return Diff{Changed: false, Summary: "概念 " + id + " 已是 certified（幂等）"}, nil
	}
	if c.Status != store.ConceptDraft {
		return Diff{}, fmt.Errorf("ops: promote: 概念 %q 状态为 %s，只允许 draft → certified", id, c.Status)
	}
	before := marshalJSON(map[string]any{"id": c.ID, "status": string(c.Status), "origin": string(c.Origin)})
	next := c
	next.Status = store.ConceptCertified
	// Origin 保留（D15）：认证后仍需保留"它最初是隐式长出来的"这条审计线索。
	if err := txc.Store().PutConcept(ctx, next); err != nil {
		return Diff{}, err
	}
	return Diff{
		Changed: true, Summary: fmt.Sprintf("概念 %s：draft → certified（origin=%s 保留）", id, c.Origin),
		Before: before,
		After:  marshalJSON(map[string]any{"id": next.ID, "status": string(next.Status), "origin": string(next.Origin)}),
		Subjects: []string{id},
	}, nil
}
