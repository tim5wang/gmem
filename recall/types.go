// Package recall 实现 docs/TD.md §5 的召回侧本体契约（D1）。
//
// 五条规则（R1–R5）在本包里的落地方式：
//
//	R1 本体不拒绝写入   → 本包只**读**本体，从不因对齐失败而拒绝召回请求；
//	R2 只有 certified 参与硬契约 → 对齐权重按概念状态打折（见 Config.StatusFactors），
//	     draft/deprecated 仍能被召回，只是不构成硬约束；
//	R3 召回概念先行     → Recall 的固定顺序是 align → plan（选 lane 与跳数）→ 定向召回；
//	R4 覆盖不足返回 insufficient → 结构化 MissingSlot，而不是低分硬答；
//	R5 每次召回可解释   → RecallResult.Plan 恒非空，含 lane/跳数/分数分解/拦截明细。
//
// 本包不含任何 SQL，只依赖 store 包的 Port 与 fusion 两个纯函数包，
// 因此 SQLite 与内存后端（internal/vtest）跑同一套召回逻辑。
package recall

import (
	"context"
	"time"

	"github.com/tim5wang/gmem/store"
)

// LaneName 是一路召回通道的名字（docs/TD.md §5 的 Fuser 三路）。
type LaneName string

const (
	LaneLexical LaneName = "lexical" // FTS5 + bigram
	LaneVector  LaneName = "vector"  // int8 近邻
	LaneGraph   LaneName = "graph"   // 按 relation 白名单定向跳
)

// RecallStatus 是召回结论（docs/TD.md §5 R4）。
type RecallStatus string

const (
	// StatusOK：覆盖率达标，且没有通道降级。
	StatusOK RecallStatus = "ok"
	// StatusInsufficient：覆盖率 < θ_cov 或必需槽位缺失。
	// Items 为空——这是刻意的：返回三条低相关记忆比诚实说"我没有这个维度"更有害（R4 的反面）。
	StatusInsufficient RecallStatus = "insufficient"
	// StatusDegraded：覆盖率达标，但有通道缺席/出错（例如无 embedder 时的 vector lane）。
	// 结果仍然可用，但调用方必须知道它是降级结果（§13 "降级可见"）。
	StatusDegraded RecallStatus = "degraded"
)

// ---------------------------------------------------------------------------
// 请求
// ---------------------------------------------------------------------------

// Budget 是召回预算。
//
// PerLane 是每一路通道最多取多少条（喂给 RRF 的排名长度），
// Final 是最终返回条数。为零时使用 Config 的默认值。
type Budget struct {
	PerLane int
	Final   int
}

// RecallRequest 是一次召回请求（docs/TD.md §10 RecallRequest 的 P1 子集）。
//
// 没有 Explain 开关：R5 要求**每次**召回都可解释，因此 Plan 恒被填充。
// 刻意不暴露"关掉解释"的选项，避免出现"线上没有 plan 可复现"的情况。
type RecallRequest struct {
	Query  string
	Scope  store.Scope
	Kinds  []store.MemoryKind
	AsOf   *int64 // 双时态回溯；nil = 只看当前版本
	Budget Budget
	// Lanes 留空 = 由 Planner 决定；非空 = 调用方强制指定（仍受"能否执行"约束，
	// 例如强制 vector 但没注入 Embedder 时会被跳过并记入 Degraded）。
	Lanes []LaneName
}

// ---------------------------------------------------------------------------
// 对齐（Concept Aligner）
// ---------------------------------------------------------------------------

// SlotKind 是概念槽位的类型。
type SlotKind string

const (
	// SlotEntity：需要一个实体锚点（词典命中的节点，或未命中的主语）。
	SlotEntity SlotKind = "entity"
	// SlotConcept：需要一个概念（concepts.name / 定义句命中）。
	SlotConcept SlotKind = "concept"
	// SlotRelation：需要一个关系（"用什么" → uses）。
	SlotRelation SlotKind = "relation"
	// SlotLiteral：字面量/标识符/代码符号，只能走词法通道。
	SlotLiteral SlotKind = "literal"
)

// Slot 是 query 里的一个候选概念槽位。
type Slot struct {
	Kind     SlotKind
	Text     string
	Required bool    // 必需槽位未对齐 → insufficient（§5 R4）
	Weight   float64 // 槽位权重，coverage 的分母
}

// MatchMethod 说明一个槽位是靠什么对齐上的。
//
// 全部是**无 embedder** 的路径（P1 不得强依赖向量）：
type MatchMethod string

const (
	// MatchAlias：aliases 词典命中（最高置信，§3 L0 词典规则）。
	MatchAlias MatchMethod = "alias"
	// MatchConceptName：concepts.name 命中。
	MatchConceptName MatchMethod = "concept_name"
	// MatchDefinition：定义句词法相似度命中（bigram 重合度，不是向量）。
	MatchDefinition MatchMethod = "definition"
	// MatchRelationCue：关系线索词命中（"用什么" → uses）。
	MatchRelationCue MatchMethod = "relation_cue"
	// MatchLiteral：字面量自动命中（词法通道必然可检索）。
	MatchLiteral MatchMethod = "literal"
)

// Match 是一个已对齐的槽位。
type Match struct {
	SlotIndex    int
	Kind         SlotKind
	Text         string
	NodeID       string         // entity 槽：命中的节点
	ConceptID    string         // concept 槽：命中的概念
	ConceptName  string         // 便于 explain（不额外查库）
	Relation     store.Relation // relation 槽：推断出的关系类型
	Method       MatchMethod
	Quality      float64 // 0..1 的对齐质量
	StatusFactor float64 // 概念状态因子（certified 1.0 / draft 打折 / deprecated 0）
	// Weight 是参与 coverage 的分量：Slot.Weight × Quality × StatusFactor。
	Weight float64
}

// Alignment 是 Concept Aligner 的输出（§5 时序图里的 matched[] · unmatched_slots[] · coverage）。
type Alignment struct {
	Query      string
	Normalized string
	Slots      []Slot
	Matched    []Match
	Unmatched  []Slot
	Coverage   float64
	// RequiredMissing 是"必需槽"里没对齐的那些（R4 的第二条触发条件）。
	RequiredMissing []Slot
	// Anchors 是实体锚点（节点 ID），Planner 用它决定 graph lane。
	Anchors []string
	// ConceptIDs 是命中的概念（供概念一致性加权与 explain）。
	ConceptIDs []string
	// RelationHints 是关系槽推断出的关系（Planner 用它选跳数）。
	RelationHints []store.Relation
	// CoverageAuthoritative 表示本次 Coverage 是否可用于 R4 判定。
	//
	// 为 false 的唯一情形是**没有本体可对齐**（后端未实现 OntologyReader，
	// 或 concepts/aliases 全空）：此时算出来的"覆盖率"只反映"query 里有几个词
	// 不在一个空集合里"，用它判 insufficient 会把"库里有数据但还没建本体"
	// 误报成"我没有这个维度"。这种降级必须在 Degraded 里显式说明。
	CoverageAuthoritative bool
	// Degraded 记录对齐阶段的降级（例如后端没实现 OntologyReader）。
	Degraded []string
}

// MissingSlot 是 R4 的结构化"缺什么"。
//
// 它的作用是把"我不知道"说清楚（Auto-Ontology 的 check_answerable 搬到记忆场景）：
// "你问的『部署环境』这个维度我完全没有记录" 比三条低相关记忆有用得多。
type MissingSlot struct {
	Kind     SlotKind
	Text     string
	Required bool
	// Reason 是给人看的一句话，必须包含槽位文本，便于直接回显给用户。
	Reason string
	// Suggestion 是可执行的补救方向（例如"先写入该维度的记忆"）。
	Suggestion string
}

// ---------------------------------------------------------------------------
// 计划（Recall Planner）
// ---------------------------------------------------------------------------

// QueryShape 是 lane 选择规则表的行键（docs/TD.md §5「lane 选择规则表」）。
type QueryShape string

const (
	ShapeFullWithAnchor QueryShape = "full_with_anchor" // 概念全覆盖 + 有实体锚点
	ShapeFullNoAnchor   QueryShape = "full_no_anchor"   // 概念全覆盖 + 无实体锚点
	ShapePartial        QueryShape = "partial"          // 概念部分覆盖
	ShapeLiteral        QueryShape = "literal"          // 纯字面 / 标识符 / 代码符号
	ShapeRelational     QueryShape = "relational"       // 关系型问句（"谁用的什么"）
)

// LanePlan 是 Planner 对一路通道的决定。
type LanePlan struct {
	Lane   LaneName
	Weight float64 // 未校准起点，见 DefaultConfig 的 LaneWeights
	Budget int
	// Selected 表示这条 lane 被规则表选中。
	Selected bool
	// Executed 表示它最终真的执行了（被选中且具备执行条件）。
	Executed bool
	// Reason 说明为什么选它（规则表来源）。
	Reason string
	// SkipReason 说明为什么没执行（无 embedder / 无锚点 / 无 scope / 空 query……）。
	SkipReason string
}

// RelationPlan 是一条关系在本轮召回里的跳数决定。
//
// 跳数由 relation 的传递性决定，不由全局常量决定（§5 R3 与 Graphiti 盲 BFS 的分歧点）。
type RelationPlan struct {
	Relation   store.Relation
	Transitive bool
	MaxHops    int
	Weight     float64
	// Origin 说明这条关系从哪来："query-cue"（问句线索）或 "shape-default"（形状默认白名单）。
	Origin string
}

// PlanStep 是一步可复现的解释记录（R5：任意一次召回能重放 plan）。
type PlanStep struct {
	Step   string
	Detail string
}

// DroppedItem 记录"被拦截"的候选（R5 要求能回答"哪些被降权或拦截"）。
type DroppedItem struct {
	ID     string
	Reason string
	Score  float64
}

// RecallPlan 是 R5 的载体：对齐了哪些概念 / 未对齐哪些 / lane 怎么选的 /
// 跳数是多少 / 每条分数如何分解 / 哪些被降权或拦截。
type RecallPlan struct {
	Query         string
	Shape         QueryShape
	ShapeReason   string
	Coverage      float64
	ThetaCoverage float64
	Matched       []Match
	Unmatched     []Slot
	Anchors       []string
	Lanes         []LanePlan
	Relations     []RelationPlan
	Hops          int // 本轮使用的最大跳数
	Steps         []PlanStep
	Dropped       []DroppedItem
}

// ---------------------------------------------------------------------------
// 结果
// ---------------------------------------------------------------------------

// Adjustment 是分数调整明细（概念一致性加权 / 时效降权）。
type Adjustment struct {
	Kind   string  // "concept_consistency" | "recency_decay"
	Factor float64 // 乘性因子
	Reason string
}

// LaneHit 是一条候选在某一路通道里的贡献（来自 fusion.Contribution）。
type LaneHit struct {
	Lane   LaneName
	Rank   int
	Weight float64
	Value  float64
}

// GraphEvidence 是 graph lane 给出的一条边证据（可解释"凭什么是它"）。
type GraphEvidence struct {
	Anchor   string
	EdgeID   string
	From     string
	To       string
	Relation store.Relation
	Depth    int
	// LinkedBy 说明节点是如何映射到这条记忆的：concept_id 或 source_episode。
	LinkedBy string
}

// RecallItem 是一条召回结果，带完整分数分解。
//
// 最终分：Score = FusionScore × ConceptBoost × RecencyFactor。
// 三个因子都保留在结构里，因此任何一条结果都能被复算（R5）。
type RecallItem struct {
	Memory        store.Memory
	Score         float64
	FusionScore   float64 // RRF 融合分（未加权、未衰减）
	ConceptBoost  float64 // 概念一致性乘子，>=1
	RecencyFactor float64 // 时效乘子，(0,1]
	Contributions []LaneHit
	Adjustments   []Adjustment
	// ConceptConsistent 表示这条记忆的概念命中了对齐出的概念（或经图锚点间接命中）。
	ConceptConsistent bool
	GraphEvidence     []GraphEvidence
}

// RecallResult 是召回结论（结构对齐 docs/TD.md §5 的返回体）。
type RecallResult struct {
	Status   RecallStatus
	Items    []RecallItem
	Coverage float64
	Missing  []MissingSlot
	Plan     *RecallPlan
	// Degraded 是**显式**的降级清单，例如 "vector lane skipped: no embedder"。
	// R5 与 §13 都要求它不得静默。
	Degraded []string
}

// Embedder 是可选的查询侧向量化能力。
//
// P1 不引入任何 LLM SDK：宿主自己实现 Embedder（本地小模型、外部服务或测试替身）。
// 未注入时 vector lane **优雅缺席**，并在 Degraded 里显式报告。
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// EmbedderFunc 让普通函数满足 Embedder，便于宿主与测试注入。
type EmbedderFunc func(ctx context.Context, texts []string) ([][]float32, error)

// Embed 实现 Embedder。
func (f EmbedderFunc) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return f(ctx, texts)
}

// clockNow 返回可注入时钟的结果，避免测试依赖真实时间。
func clockNow(cfg Config) time.Time {
	if cfg.Now != nil {
		return cfg.Now()
	}
	return time.Now()
}
