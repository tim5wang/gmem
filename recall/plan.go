package recall

import (
	"fmt"
	"sort"

	"github.com/tim5wang/gmem/store"
)

// Planner 是 §5 R3 的第二步：按对齐结果与关系传递性选 lane 与跳数。
//
// 三个决定全部**查表**，不写死在分支里：
//   - 查询形态（QueryShape）→ lane 与初始权重：Config.LaneRules；
//   - 关系类型 → 跳数：Config.RelationRules（传递性是表里的第一等字段）；
//   - 每 lane 预算：Budget / Config.DefaultPerLane。
//
// 这样召回策略是可回归的：换权重只换数据，plan 里回显的就是表里的值。
type Planner struct {
	cfg Config
}

// NewPlanner 构造 Planner。
func NewPlanner(cfg Config) *Planner { return &Planner{cfg: cfg.withDefaults()} }

// planInput 是 Planner 的输入（对齐结果 + 调用方约束 + 执行能力）。
type planInput struct {
	Align       Alignment
	Request     RecallRequest
	HasEmbedder bool
	// GraphScope 为空表示无法做图遍历（Scope 是 GraphStore 的必填隔离边界）。
	GraphScope store.Scope
}

// makePlan 产出 RecallPlan 的骨架（lane/跳数/步骤），不含分数分解与拦截明细。
func (p *Planner) makePlan(in planInput) *RecallPlan {
	al := in.Align
	pl := &RecallPlan{
		Query:         in.Request.Query,
		Coverage:      al.Coverage,
		ThetaCoverage: p.cfg.ThetaCoverage,
		Matched:       al.Matched,
		Unmatched:     al.Unmatched,
		Anchors:       al.Anchors,
	}

	shape, reason := p.shape(in)
	pl.Shape = shape
	pl.ShapeReason = reason
	pl.Steps = append(pl.Steps, PlanStep{
		Step: "align",
		Detail: fmt.Sprintf("query 切成 %d 个槽位，对齐 %d 个，coverage=%.3f（θ_cov=%.2f，%s）",
			len(al.Slots), len(al.Matched), al.Coverage, p.cfg.ThetaCoverage, shape),
	})

	// R3：定向扩展的跳数来自关系表，不是全局常量。
	pl.Relations = p.relations(shape, al.RelationHints)
	for _, rp := range pl.Relations {
		if rp.MaxHops > pl.Hops {
			pl.Hops = rp.MaxHops
		}
	}
	if len(pl.Relations) > 0 {
		pl.Steps = append(pl.Steps, PlanStep{
			Step:   "relations",
			Detail: fmt.Sprintf("选定 %d 条关系，最大跳数=%d（跳数由 relation 传递性决定）", len(pl.Relations), pl.Hops),
		})
	}

	// lane 选择：规则表 + 执行能力裁剪。
	laneCfg := p.laneWeights(shape)
	perLane := in.Request.Budget.PerLane
	if perLane <= 0 {
		perLane = p.cfg.DefaultPerLane
	}
	forced := len(in.Request.Lanes) > 0
	forcedSet := map[LaneName]bool{}
	for _, l := range in.Request.Lanes {
		forcedSet[l] = true
	}

	for _, lw := range laneCfg {
		lp := LanePlan{Lane: lw.Lane, Weight: lw.Weight, Budget: perLane, Selected: true, Reason: p.laneReason(shape, lw.Lane)}
		if forced && !forcedSet[lw.Lane] {
			lp.Selected = false
			lp.Reason += "（调用方显式指定 lane 集合，本 lane 未选）"
		}
		pl.Lanes = append(pl.Lanes, lp)
	}
	// 调用方强制指定、但规则表里没有的 lane：仍要列入 plan（并说明来源），
	// 否则"调用方要了 vector 但 plan 里没有"无法解释。
	if forced {
		known := map[LaneName]bool{}
		for _, lw := range laneCfg {
			known[lw.Lane] = true
		}
		for _, l := range in.Request.Lanes {
			if known[l] {
				continue
			}
			pl.Lanes = append(pl.Lanes, LanePlan{
				Lane: l, Weight: 1.0, Budget: perLane, Selected: true,
				Reason: "调用方显式指定（不在规则表内，权重取 1.0）",
			})
		}
	}

	// 执行能力裁剪：这里只标记 Executed/SkipReason，真正执行在 lanes.go。
	for i := range pl.Lanes {
		lp := &pl.Lanes[i]
		if !lp.Selected {
			continue
		}
		switch lp.Lane {
		case LaneVector:
			if !in.HasEmbedder {
				lp.SkipReason = "no embedder: vector lane 无查询向量可用"
			}
		case LaneGraph:
			if in.GraphScope == "" {
				lp.SkipReason = "no scope: graph lane 需要 Scope 作为隔离边界"
			}
			if len(al.Anchors) == 0 {
				lp.SkipReason = "no anchor: 没有实体锚点，图遍历没有起点"
			}
			if len(pl.Relations) == 0 {
				lp.SkipReason = "no relation: 没有可用关系白名单"
			}
		case LaneLexical:
			if al.Normalized == "" {
				lp.SkipReason = "empty query"
			}
		}
		if lp.SkipReason == "" {
			lp.Executed = true
		}
	}

	for _, l := range pl.Lanes {
		state := "跳过"
		if l.Executed {
			state = "执行"
		}
		detail := fmt.Sprintf("%s: weight=%.2f budget=%d %s；%s", l.Lane, l.Weight, l.Budget, state, l.Reason)
		if l.SkipReason != "" {
			detail += "；跳过原因=" + l.SkipReason
		}
		pl.Steps = append(pl.Steps, PlanStep{Step: "lane:" + string(l.Lane), Detail: detail})
	}
	return pl
}

// shape 判定查询形态（§5 lane 选择规则表的行键）。
func (p *Planner) shape(in planInput) (QueryShape, string) {
	al := in.Align
	// 关系型问句优先：有 relation 槽，或命中关系型短语且存在未对齐实体。
	if len(al.RelationHints) > 0 {
		return ShapeRelational, fmt.Sprintf("命中关系线索词（relation=%s），按关系型问句处理", joinRelations(al.RelationHints))
	}
	// 纯字面：除字面量外没有其它槽位（标识符/代码符号查询）。
	onlyLiteral := len(al.Slots) > 0
	for _, s := range al.Slots {
		if s.Kind != SlotLiteral {
			onlyLiteral = false
			break
		}
	}
	if onlyLiteral {
		return ShapeLiteral, "全部槽位都是字面量/标识符，只有词法通道有意义"
	}
	if al.Coverage >= p.cfg.ThetaCoverage {
		if len(al.Anchors) > 0 {
			return ShapeFullWithAnchor, fmt.Sprintf("coverage=%.3f ≥ θ，且有 %d 个实体锚点", al.Coverage, len(al.Anchors))
		}
		return ShapeFullNoAnchor, fmt.Sprintf("coverage=%.3f ≥ θ，但没有实体锚点", al.Coverage)
	}
	return ShapePartial, fmt.Sprintf("coverage=%.3f < θ_cov=%.2f，概念部分覆盖", al.Coverage, p.cfg.ThetaCoverage)
}

// laneWeights 查 lane 规则表。表里没有该形态时退化为 partial 行（并保留至上层可见）。
func (p *Planner) laneWeights(shape QueryShape) []LaneWeight {
	for _, r := range p.cfg.LaneRules {
		if r.Shape == shape {
			return r.Lanes
		}
	}
	for _, r := range p.cfg.LaneRules {
		if r.Shape == ShapePartial {
			return r.Lanes
		}
	}
	return []LaneWeight{{Lane: LaneLexical, Weight: 1.0}}
}

// laneReason 从规则表取该 lane 的理由，保证 Reason 可追溯到表。
func (p *Planner) laneReason(shape QueryShape, lane LaneName) string {
	for _, r := range p.cfg.LaneRules {
		if r.Shape != shape {
			continue
		}
		for _, lw := range r.Lanes {
			if lw.Lane == lane {
				return r.Reason
			}
		}
	}
	return "规则表未覆盖该 lane（默认 1.0）"
}

// relations 决定本轮要用的关系与跳数。
//
// 优先用查询线索（query-cue）；没有线索时用"形态默认白名单"：
// 有锚点的图召回至少需要一条边类型，否则 graph lane 无从下手。
func (p *Planner) relations(shape QueryShape, hints []store.Relation) []RelationPlan {
	var out []RelationPlan
	seen := map[store.Relation]bool{}
	add := func(rel store.Relation, origin string) {
		if rel == "" || seen[rel] {
			return
		}
		rule, ok := p.rule(rel)
		if !ok {
			// 表外关系：非传递、1 跳。**仍然列出**，并要求 plan 能解释它。
			rule = RelationRule{Relation: rel, Transitive: false, MaxHops: 1, Weight: 0.5}
		}
		seen[rel] = true
		out = append(out, RelationPlan{
			Relation: rule.Relation, Transitive: rule.Transitive,
			MaxHops: rule.MaxHops, Weight: rule.Weight, Origin: origin,
		})
	}
	for _, h := range hints {
		add(h, "query-cue")
	}
	if len(out) == 0 {
		switch shape {
		case ShapeRelational:
			// 关系型问句但没识别出具体关系：用白名单里权重最高的一条做保守猜测。
			// 注意这里仍然查表取跳数，不引入全局常量。
			if rel, ok := p.topRelation(); ok {
				add(rel, "shape-default")
			}
		case ShapeFullWithAnchor, ShapePartial:
			// 有锚点的图召回至少需要一条边类型；取表内权重最高的前几条（含跳数），
			// 数量设上界以免盲 BFS 式噪声。
			for _, rel := range p.topRelations(defaultRelationFanout) {
				add(rel, "shape-default")
			}
		}
	}
	return out
}

// defaultRelationFanout 是形态默认白名单的条数上界（未校准起点）。
const defaultRelationFanout = 3

func (p *Planner) rule(rel store.Relation) (RelationRule, bool) {
	for _, r := range p.cfg.RelationRules {
		if r.Relation == rel {
			return r, true
		}
	}
	return RelationRule{}, false
}

func (p *Planner) topRelation() (store.Relation, bool) {
	rels := p.topRelations(1)
	if len(rels) == 0 {
		return "", false
	}
	return rels[0], true
}

// topRelations 按表内权重降序返回前 n 条关系（同权重按名字排序，保证确定性）。
// 跳数不在这里决定：调用方仍然从表里查 MaxHops。
func (p *Planner) topRelations(n int) []store.Relation {
	rules := append([]RelationRule(nil), p.cfg.RelationRules...)
	sort.SliceStable(rules, func(i, j int) bool {
		if rules[i].Weight != rules[j].Weight {
			return rules[i].Weight > rules[j].Weight
		}
		return rules[i].Relation < rules[j].Relation
	})
	if n > 0 && len(rules) > n {
		rules = rules[:n]
	}
	out := make([]store.Relation, len(rules))
	for i, r := range rules {
		out[i] = r.Relation
	}
	return out
}

func joinRelations(rels []store.Relation) string {
	s := ""
	for i, r := range rels {
		if i > 0 {
			s += ","
		}
		s += r.String()
	}
	return s
}
