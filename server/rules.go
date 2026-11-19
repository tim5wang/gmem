package server

import (
	"github.com/tim5wang/gmem/recall"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// 跳数规则：**直接读 recall 的关系传递性表**，不另写一套
//
// docs/TD.md §9 明确要求："这和 §5 的定向扩展表是**同一张表**——视觉与检索
// 共用一套关系语义，不能各管各的。"
//
// 因此本文件里**没有任何跳数常量**：所有 MaxHops 都来自
// recall.Recaller.Config().RelationRules（即 §5 R3 的落地表）。
// 若将来有人只改可视化侧的跳数，这里读不到新值——那正是我们想要的效果：
// 可视化不可能与检索漂移，因为压根没有第二处可改。
// ===========================================================================

// hopRule 是一条关系的可视化跳数上限。
type hopRule struct {
	Relation   store.Relation
	MaxHops    int
	Transitive bool
}

// hopTable 是规整后的关系跳数表。
type hopTable struct {
	byRelation map[store.Relation]hopRule
	// order 是表内关系的固定顺序（按 MaxHops 降序、名字升序），
	// 保证遍历与响应输出与 map 迭代顺序无关。
	order []hopRule
}

// newHopTable 把 recall 的声明式表规整成可视化用的查表结构。
//
// 规整规则与 recall.Planner.relations 完全一致（F8 的语义）：
// **非传递关系的跳数一律夹到 1**——表里写多大都不允许连走两条，
// 否则 works_at 两跳就到别人公司。这条夹取是"同一套语义"的一部分，
// 漏了它可视化就会画出检索永远不会用的边。
func newHopTable(rules []recall.RelationRule) hopTable {
	t := hopTable{byRelation: make(map[store.Relation]hopRule, len(rules))}
	for _, r := range rules {
		if r.Relation == "" {
			continue
		}
		maxHops := r.MaxHops
		if maxHops <= 0 {
			maxHops = 1
		}
		if !r.Transitive && maxHops > 1 {
			maxHops = 1
		}
		t.byRelation[r.Relation] = hopRule{
			Relation: r.Relation, MaxHops: maxHops, Transitive: r.Transitive,
		}
	}
	t.order = make([]hopRule, 0, len(t.byRelation))
	for _, r := range t.byRelation {
		t.order = append(t.order, r)
	}
	// 固定顺序：跳数降序、名字升序（确定性）。
	for i := 1; i < len(t.order); i++ {
		for j := i; j > 0; j-- {
			a, b := t.order[j-1], t.order[j]
			if a.MaxHops > b.MaxHops || (a.MaxHops == b.MaxHops && a.Relation <= b.Relation) {
				break
			}
			t.order[j-1], t.order[j] = b, a
		}
	}
	return t
}

// lookup 返回该关系的跳数规则。
//
// 表外关系（例如 possiblySame / negative_SameAs 这类记账边、或用户自定义关系）
// 按 recall.Planner 的同一约定处理：**非传递、1 跳**。绝不静默当成"不限"。
func (t hopTable) lookup(rel store.Relation) hopRule {
	if r, ok := t.byRelation[rel]; ok {
		return r
	}
	return hopRule{Relation: rel, MaxHops: 1, Transitive: false}
}

// effectiveHops 返回"请求跳数"与"关系极限"中更严的一个。
func (t hopTable) effectiveHops(rel store.Relation, requested int) hopRule {
	r := t.lookup(rel)
	eff := r
	if requested > 0 && requested < eff.MaxHops {
		eff.MaxHops = requested
	}
	return eff
}

// relations 返回表内全部关系（固定顺序），供回退路径按关系分批遍历。
func (t hopTable) relations() []store.Relation {
	out := make([]store.Relation, len(t.order))
	for i, r := range t.order {
		out[i] = r.Relation
	}
	return out
}
