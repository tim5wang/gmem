package recall

import (
	"time"

	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// 声明式配置：lane 权重表 / 关系跳数表 / 槽位权重表
//
// ⚠️ 全部数值都是**待校准起点**，不是实测结论（docs/TD.md §5 与 §13「证据边界」
// 明确写了「lane 权重表、跳数规则表全部是待校准的设计起点」）。
//
// 之所以做成 Config 里的数据表而不是 if/else 分支：
//  1. 召回质量要靠在评测集上回归来调，权重必须能替换而不能靠改代码；
//  2. Plan 需要能**回答**"这条 lane 为什么被选中、给了多少权重"——
//     从表里查出来的值天然可以回显，写死在分支里的值无法解释；
//  3. 同一份存储可能服务不同校准结果（不同领域、不同语言）。
// ===========================================================================

// RelationRule 是**关系白名单的跳数规则**（docs/TD.md §5「R3 的落地」表）。
//
// 跳数由本表按 relation 决定，**不是**一个全局 bfs_max_depth 常量：
// Graphiti 的全局深度 3 是所有边一视同仁，会从 works_at 两跳走到"别人公司"，
// 那是噪声的根源。这里把传递性作为第一等字段记录下来，供 explain 复述。
//
// ⚠️ MaxHops 为待校准起点：subtype_of/part_of=3、depends_on=3、caused_by=2、
// works_at/owns=1 直接来自 TD §5 的设计表，尚无评测集支撑。
type RelationRule struct {
	Relation   store.Relation
	Transitive bool
	MaxHops    int
	// Weight 是该关系在 graph lane 内的相对权重（未校准起点）。
	Weight float64
}

// DefaultRelationRules 是 §5 关系白名单表的可配置版本。
//
// 未在表内的关系（如 uses）按"非传递 1 跳"处理，并且**必须**在 plan 的
// Relations 里显式列出（Origin=shape-default），不允许出现"用了图却没有规则可查"。
func DefaultRelationRules() []RelationRule {
	return []RelationRule{
		{Relation: store.RelationSubtypeOf, Transitive: true, MaxHops: 3, Weight: 1.0},
		{Relation: store.RelationPartOf, Transitive: true, MaxHops: 3, Weight: 1.0},
		{Relation: store.RelationDependsOn, Transitive: true, MaxHops: 3, Weight: 0.9},
		{Relation: store.RelationCausedBy, Transitive: true, MaxHops: 2, Weight: 0.8},
		{Relation: store.RelationWorksAt, Transitive: false, MaxHops: 1, Weight: 0.7},
		{Relation: store.RelationOwns, Transitive: false, MaxHops: 1, Weight: 0.7},
		{Relation: store.RelationUses, Transitive: false, MaxHops: 1, Weight: 0.9},
	}
}

// LaneWeight 是一行 lane 选择规则（docs/TD.md §5「lane 选择规则表」）。
//
// ⚠️ Weight 是待校准起点：0.5/0.3/0.2 这类数字来自 TD 的设计表，
// 没有评测集回归。改这里即可换策略，不必改调用点。
type LaneWeight struct {
	Lane   LaneName
	Weight float64
}

// LaneRule 是"查询形态 → lane 与初始权重"的一行。
type LaneRule struct {
	Shape  QueryShape
	Lanes  []LaneWeight
	Reason string // 写进 plan 的一句话（规则表来源）
}

// DefaultLaneRules 是 §5 lane 选择规则表的可配置版本。
//
// 注意 partial 那行 TD 写的是 "vector + lexical（graph 弱参与）0.5 / 0.4 / 0.1"：
// 这里按 graph 弱参与的含义保留 graph 但给最低权重；它不是三路平等参与。
func DefaultLaneRules() []LaneRule {
	return []LaneRule{
		{
			Shape: ShapeFullWithAnchor,
			Lanes: []LaneWeight{
				{Lane: LaneGraph, Weight: 0.5},
				{Lane: LaneVector, Weight: 0.3},
				{Lane: LaneLexical, Weight: 0.2},
			},
			Reason: "概念全覆盖且有实体锚点：graph 主，vector/lexical 辅助（未校准起点）",
		},
		{
			Shape: ShapeFullNoAnchor,
			Lanes: []LaneWeight{
				{Lane: LaneLexical, Weight: 0.6},
				{Lane: LaneVector, Weight: 0.4},
			},
			Reason: "概念全覆盖但无实体锚点：graph 无起点，lexical 主（未校准起点）",
		},
		{
			Shape: ShapePartial,
			Lanes: []LaneWeight{
				{Lane: LaneVector, Weight: 0.5},
				{Lane: LaneLexical, Weight: 0.4},
				{Lane: LaneGraph, Weight: 0.1},
			},
			Reason: "概念部分覆盖：以语义/词法为主，graph 弱参与（未校准起点）",
		},
		{
			Shape: ShapeLiteral,
			Lanes: []LaneWeight{
				{Lane: LaneLexical, Weight: 1.0},
			},
			Reason: "纯字面/标识符/代码符号：只有词法通道有意义（未校准起点）",
		},
		{
			Shape: ShapeRelational,
			Lanes: []LaneWeight{
				{Lane: LaneGraph, Weight: 0.7},
				{Lane: LaneVector, Weight: 0.3},
			},
			Reason: "关系型问句：graph 主、vector 辅（未校准起点）",
		},
	}
}

// SlotWeights 是各类槽位参与 coverage 的分母权重（未校准起点）。
//
// 关系槽权重刻意高于实体槽：关系型问句里"没听懂它在问哪个关系"比
// "没认出某个人名"更能决定答案对错。
type SlotWeights struct {
	Entity   float64
	Concept  float64
	Relation float64
	Literal  float64
}

// Config 是 Recaller 的全部可调参数。
//
// 零值可直接用（DefaultConfig 会补齐默认），所有阈值都必须在注释里标注
// "待校准"，避免把设计起点误当实测结论。
type Config struct {
	// ThetaCoverage 是 R4 的覆盖率门槛 θ_cov。
	//
	// ⚠️ 默认 0.6 是**待校准**起点（docs/TD.md §5 只给了公式，没给数值；
	// 同段的 0.92/0.72 属消歧阈值，与这里无关）。覆盖率 < θ → insufficient。
	ThetaCoverage float64

	// LaneRules / RelationRules / SlotWeights 见上：声明式，可整表替换。
	LaneRules     []LaneRule
	RelationRules []RelationRule
	SlotWeights   SlotWeights

	// StatusFactors 是概念状态对对齐权重的因子（R2：只有 certified 参与硬契约）。
	// draft 打折而非 0，deprecated 归零但仍在 plan 里可见。
	StatusFactors map[store.ConceptStatus]float64

	// AliasQuality / ConceptNameQuality / DefinitionQuality 是对齐质量的基准分
	// （未校准起点）。定义句相似度的下限 DefinitionMinSimilarity 同样是待校准值。
	AliasQuality       float64
	ConceptNameQuality float64
	DefinitionQuality  float64
	DefinitionMinSim   float64

	// RelationCues 是"关系型问句"的线索词表（每条 = 一个关系 + 若干表面形式）。
	// 命中即产出 SlotRelation + RelationHint，Planner 据此选 1-3 跳。
	RelationCues []RelationCue
	// RelationalPhrases 是关系型问句的**疑问短语**（谁/什么/哪个/怎么……）。
	//
	// P1 的实际用途是"噪声掩码"：这些短语在残槽切分时必须被屏蔽，
	// 否则"什么"会被当成一个未对齐的实体维度，把 coverage 稀释成 0。
	// 查询形态判定（ShapeRelational）只认关系**线索词**（RelationCues）——
	// 单看"什么"无法判断用户问的是哪个关系，据此选跳数属于猜测。
	RelationalPhrases []string

	// ConceptBoost 是概念一致性加权的上限增量（未校准起点）：
	// 命中对齐概念的候选 ×(1+ConceptBoost)，未命中保持 1.0（不惩罚，
	// 因为 RRF 已经按通道内排名惩罚过它了）。
	ConceptBoost float64

	// RecencyHalfLife 是时效降权的半衰期（未校准起点）。0 或负值 = 关闭降权。
	RecencyHalfLife time.Duration
	// RecencyFloor 是降权因子的下界，避免旧但正确的记忆被压成 0。
	RecencyFloor float64

	// DefaultPerLane / DefaultFinal 是 Budget 为零时的默认预算。
	DefaultPerLane int
	DefaultFinal   int

	// Now 是可注入时钟（测试用）。nil = time.Now。
	Now func() time.Time
}

// RelationCue 是一条关系线索词规则。
//
// 例：{"use", "uses", "using", "用", "使用", "用什么"} → uses。
// 表本身可配置：换语言/换领域只改数据。
type RelationCue struct {
	Relation store.Relation
	// Phrases 是表面形式（会被 lower-case 归一后做子串匹配）。
	Phrases []string
}

// DefaultRelationCues 是 P1 的最小关系线索表（中文 + 英文，未校准起点）。
//
// 只覆盖 TD §5 关系白名单里的类型；表外的关系不走线索推断，
// 而是在 plan 里显式说明"未识别关系线索"。
func DefaultRelationCues() []RelationCue {
	return []RelationCue{
		{Relation: store.RelationUses, Phrases: []string{"用什么", "用的什么", "使用", "用了", "在用", "uses", "use ", "using", "which tool"}},
		{Relation: store.RelationWorksAt, Phrases: []string{"在哪工作", "哪个公司", "就职", "works at", "works for", "employer"}},
		{Relation: store.RelationOwns, Phrases: []string{"拥有", "持有", "owns", "owned by"}},
		{Relation: store.RelationDependsOn, Phrases: []string{"依赖", "取决于", "depends on", "dependency"}},
		{Relation: store.RelationCausedBy, Phrases: []string{"原因", "为什么", "导致", "因为", "caused by", "root cause"}},
		{Relation: store.RelationPartOf, Phrases: []string{"属于", "一部分", "part of", "belongs to"}},
		{Relation: store.RelationSubtypeOf, Phrases: []string{"是一种", "子类型", "kind of", "subtype of"}},
	}
}

// DefaultRelationalPhrases 说明"关系型问句"的形态（不锁定具体关系）。
func DefaultRelationalPhrases() []string {
	return []string{"谁", "什么", "哪个", "哪里", "怎么", "为何", "who", "what", "which", "where", "how"}
}

// DefaultConfig 返回标注了默认值的配置。所有数值都**待校准**。
func DefaultConfig() Config {
	return Config{
		// ⚠️ 待校准：0.6 是设计起点，需用评测集回归。
		ThetaCoverage: 0.6,

		LaneRules:     DefaultLaneRules(),
		RelationRules: DefaultRelationRules(),
		SlotWeights: SlotWeights{
			Entity:  1.0,
			Concept: 1.0,
			// 关系槽权重高于实体/概念（见 SlotWeights 注释）。待校准。
			Relation: 1.2,
			Literal:  0.8,
		},
		StatusFactors: map[store.ConceptStatus]float64{
			store.ConceptCertified:  1.0,
			store.ConceptDraft:      0.6, // 待校准：draft 打折而非 0，仍可召回
			store.ConceptDeprecated: 0.0,
		},

		AliasQuality:       1.0,
		ConceptNameQuality: 0.9,
		// ⚠️ 待校准：定义句对齐比词典弱，给 0.7 起点。
		DefinitionQuality: 0.7,
		// ⚠️ 待校准：0.5 的 bigram 重合度下限，避免共享高频 bigram 就误对齐。
		DefinitionMinSim: 0.5,

		RelationCues:      DefaultRelationCues(),
		RelationalPhrases: DefaultRelationalPhrases(),

		// ⚠️ 待校准：命中对齐概念的候选最高 +50%。
		ConceptBoost: 0.5,

		// ⚠️ 待校准：30 天半衰期是设计起点，不是实测结论。
		RecencyHalfLife: 30 * 24 * time.Hour,
		// ⚠️ 待校准：下界 0.3，保证旧而正确的记忆不会被压没。
		RecencyFloor: 0.3,

		DefaultPerLane: 20,
		DefaultFinal:   10,
	}
}

// withDefaults 补齐零值字段。零值 Config 必须可用（否则宿主一忘配就崩）。
func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if c.ThetaCoverage <= 0 {
		c.ThetaCoverage = d.ThetaCoverage
	}
	if len(c.LaneRules) == 0 {
		c.LaneRules = d.LaneRules
	}
	if len(c.RelationRules) == 0 {
		c.RelationRules = d.RelationRules
	}
	if c.SlotWeights == (SlotWeights{}) {
		c.SlotWeights = d.SlotWeights
	}
	if len(c.StatusFactors) == 0 {
		c.StatusFactors = d.StatusFactors
	}
	if c.AliasQuality <= 0 {
		c.AliasQuality = d.AliasQuality
	}
	if c.ConceptNameQuality <= 0 {
		c.ConceptNameQuality = d.ConceptNameQuality
	}
	if c.DefinitionQuality <= 0 {
		c.DefinitionQuality = d.DefinitionQuality
	}
	if c.DefinitionMinSim <= 0 {
		c.DefinitionMinSim = d.DefinitionMinSim
	}
	if len(c.RelationCues) == 0 {
		c.RelationCues = d.RelationCues
	}
	if len(c.RelationalPhrases) == 0 {
		c.RelationalPhrases = d.RelationalPhrases
	}
	if c.ConceptBoost <= 0 {
		c.ConceptBoost = d.ConceptBoost
	}
	if c.RecencyHalfLife == 0 {
		c.RecencyHalfLife = d.RecencyHalfLife
	}
	if c.RecencyFloor <= 0 {
		c.RecencyFloor = d.RecencyFloor
	}
	if c.DefaultPerLane <= 0 {
		c.DefaultPerLane = d.DefaultPerLane
	}
	if c.DefaultFinal <= 0 {
		c.DefaultFinal = d.DefaultFinal
	}
	return c
}
