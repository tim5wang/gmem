package extract

import (
	"context"
	"encoding/json"
	"time"

	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// 场景输入
// ===========================================================================

// RawEpisode 是待提取的一段原文（= store.Episode 的整理层视图）。
//
// 与 store.Episode 分开定义，是为了让 Extractor 的实现者（可能是宿主的 agent 框架）
// 不必依赖存储层类型；转换用 Episode() 完成。
type RawEpisode struct {
	ID        string
	Scope     store.Scope
	Namespace string
	Role      string
	Content   string
	At        int64 // unix 秒
}

// Episode 转成可落库的 store.Episode（补齐 ContentHash 由调用方/后端决定）。
func (e RawEpisode) Episode() store.Episode {
	return store.Episode{
		ID: e.ID, Scope: e.Scope, Namespace: e.Namespace,
		Role: e.Role, Content: e.Content, At: e.At,
	}
}

// EntityBrief 是"当前 scope 已有实体"的摘要，供 L2 与消歧参考。
//
// 刻意只给摘要而不是整行：L2 拿到的是**候选**，不是数据库句柄；
// 需要更多细节时由 tool-calling 的只读工具按上限去查（§3.2）。
type EntityBrief struct {
	NodeID    string
	Name      string
	Aliases   []string
	ConceptID string
}

// Cluster 是 L1 交出的"疑似同一实体"簇（**候选，不是决定**）。
type Cluster struct {
	// ID 是稳定的簇标识（由成员名排序后哈希得出，便于测试与审计）。
	ID string
	// Surfaces 是簇内成员表面形式（第 0 个是代表项）。
	Surfaces []string
	// Similarity 是簇内最小成对相似度（簇的"最弱一环"）。
	Similarity float64
}

// Budget 是一次抽取的预算（§3.2 G2/G5）。
type Budget struct {
	MaxTurns    int
	TokenBudget int
	WallClock   time.Duration
}

// ===========================================================================
// D7 第一层：LLMClient（单轮 primitive）
// ===========================================================================

// LLMClient 是**单轮**模型调用 primitive（docs/TD.md §3.1）。
//
// 刻意保持单轮：没有 streaming，也不由宿主自己跑循环。
// tool-calling 的循环由 Gmem 拥有（LLMExtractor），这样收敛才能被硬保证（§3.2 D10）。
//
// Gmem 不实现它、不 import 任何 LLM SDK、不保存密钥：宿主用自己的客户端实现本接口。
type LLMClient interface {
	Complete(ctx context.Context, req LLMRequest) (LLMResponse, error)
}

// LLMRequest 是一轮请求。
type LLMRequest struct {
	System      string          // Gmem 提供的抽取指令（宿主可覆盖）
	User        string          // 待抽片段 + 已知实体候选
	JSONSchema  json.RawMessage // 强约束输出形状；宿主应尽量用结构化输出能力
	Tools       []ToolSpec      // D10：只读查询工具声明；为空则纯单轮
	MaxTokens   int
	Temperature float32 // Gmem 默认 0，抽取任务不需要随机性
}

// LLMResponse 是一轮响应。
type LLMResponse struct {
	Text      string     // 期望是符合 JSONSchema 的 JSON
	ToolCalls []ToolCall // D10：模型想先查点什么；为空则本次已是终稿
	Usage     LLMUsage
	Model     string
	Truncated bool
}

// LLMUsage 是 token 记账（写入 audit/日志；G2 用它累计预算）。
type LLMUsage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// Tokens 返回参与 G2 累计的 token 数。
//
// 优先用 TotalTokens；宿主没填时退化为两者之和，都没有则为 0
// （0 是"宿主没报账"，G2 因此不会因为缺失字段而误杀）。
func (u LLMUsage) Tokens() int {
	if u.TotalTokens > 0 {
		return u.TotalTokens
	}
	return u.PromptTokens + u.CompletionTokens
}

// ToolSpec 是一个只读工具的声明（JSONSchema 描述参数）。
type ToolSpec struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// ToolCall 是模型发起的一次工具调用。
type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

// ===========================================================================
// D7 第二层：Extractor（整段可替换）
// ===========================================================================

// Extractor 是整段可替换的抽取器（docs/TD.md §3.1）。
//
// 宿主实现它即可把抽取交给自己的 agent 框架；此时**循环与收敛归宿主**，
// Gmem 只用 context 强制中断，不再能保证它不死循环。
type Extractor interface {
	Extract(ctx context.Context, req ExtractRequest) (ExtractResult, error)
}

// ExtractRequest 是一次抽取请求。
type ExtractRequest struct {
	Episode  RawEpisode
	Clusters []Cluster     // L1 给出的疑似同簇
	Known    []EntityBrief // 当前 scope 已有实体，供消歧参考
	Budget   Budget
}

// ExtractedEntity 是抽取出的候选实体。
type ExtractedEntity struct {
	Name       string
	Aliases    []string
	ConceptID  string
	Confidence float64
	// Definition 是定义句/模型给出的释义，参与消歧的 cosine 输入。
	Definition string
}

// ExtractedRelation 是抽取出的候选关系（两端必须能在结果或已知实体里找到）。
type ExtractedRelation struct {
	From       string
	To         string
	Relation   store.Relation
	Confidence float64
}

// ConceptHint 是概念建议（落为 draft，D15）。
type ConceptHint struct {
	ID         string
	Name       string
	Definition string
}

// ExtractResult 是抽取产物。**两条 LLM 路径共用这一种形状**（§3.3 第 3 条）。
type ExtractResult struct {
	Entities  []ExtractedEntity
	Relations []ExtractedRelation
	Concepts  []ConceptHint
	// Degraded 必须显式：NoopExtractor、预算截断、解析失败、门禁拦停都要进来。
	// 绝不静默降级（§13 风险表）。
	Degraded []string
}

// ===========================================================================
// L0 确定性提取的类型
// ===========================================================================

// FindingCategory 是 L0 的七类规则。
type FindingCategory string

const (
	// CatStructured：代码块、URL、文件路径、版本号、日期、数字+单位。
	CatStructured FindingCategory = "structured"
	// CatDictionary：命中 aliases / concepts.name（最高置信来源）。
	CatDictionary FindingCategory = "dictionary"
	// CatDefinition：定义句 X 是 Y / X 指 Y / X：Y / X means Y。
	CatDefinition FindingCategory = "definition"
	// CatPreference：偏好句。
	CatPreference FindingCategory = "preference"
	// CatDecision：决策句。
	CatDecision FindingCategory = "decision"
	// CatRelation：关系三元组（动词映射到关系白名单）。
	CatRelation FindingCategory = "relation"
	// CatRecency：时效信号（ephemeral）。
	CatRecency FindingCategory = "recency"
)

// AllCategories 是七类规则的稳定顺序，测试据此逐类做变异验证。
func AllCategories() []FindingCategory {
	return []FindingCategory{
		CatStructured, CatDictionary, CatDefinition, CatPreference,
		CatDecision, CatRelation, CatRecency,
	}
}

// Finding 是一条 L0 确定性提取结果。
//
// 刻意用"一个扁平结构 + Category"而不是七个类型：L0 的产物最终都要落成
// memory / entity / edge 三类候选，扁平结构让管线与测试都能按类遍历，
// 也让"拆掉某类规则"的变异验证只需看 Category 是否还在。
type Finding struct {
	Category FindingCategory
	// Kind 是它倾向落成的形态：memory / entity / edge / concept / literal。
	Kind string
	// Text 是表面形式（实体名、字面量或整句）。
	Text string
	// Detail 是补充信息（定义文本、单位、日期归一化等）。
	Detail string
	// Subject/Object/Relation 只在 CatRelation 里使用。
	Subject  string
	Object   string
	Relation store.Relation
	// Ephemeral 是时效信号命中（CatRecency 或句子里出现"临时/待定"）。
	Ephemeral bool
	// Confidence 是该规则给的置信度（0..1，字典命中最高）。
	Confidence float64
}

// L0Result 是 L0 的输出。
type L0Result struct {
	Findings []Finding
	// Rejected 是被质量门拒掉的原因（长度下限等）。**必须可见**。
	Rejected []string
	// Ephemeral 表示整段含时效信号：落库时标为临时态而不是长期记忆。
	Ephemeral bool
	Degraded  []string
}

// ===========================================================================
// L1 词法聚类的类型
// ===========================================================================

// L1Item 是聚类输入的一个名称（可能是新候选，也可能是已有节点）。
type L1Item struct {
	// ID 是稳定标识（已有节点用 node id；新候选可用名称本身）。
	ID      string
	Surface string
	// It 表示这是已有节点（聚类结果里只会作为"候选邻居"出现，不会被合并）。
	It bool
	// Values 是可选的向量（宿主注入 embedder 时才有）。
	Values []float32
}

// L1Result 是聚类输出。
type L1Result struct {
	Clusters []Cluster
	// Pairs 是保留的成对候选（便于 explain 与测试单点断言）。
	Pairs []L1Pair
	// Degraded 显式记录"没有向量通道"等降级。
	Degraded []string
}

// L1Pair 是一对"疑似同一实体"的候选。
type L1Pair struct {
	A, B       string
	Lexical    float64
	Vector     float64
	Similarity float64
}

// ===========================================================================
// 消歧的类型
// ===========================================================================

// Mention 是一条待消歧的实体提及。
type Mention struct {
	Surface    string
	Aliases    []string
	Definition string
	ConceptID  string
	// Values 是该提及的向量（name+definition 的 embedding），可选。
	Values []float32
	// RelationTypes 是该提及的上下文关系类型（结构一致性信号）。
	RelationTypes []store.Relation
}

// ScoreBreakdown 是融合分的逐项分解（R5 可解释性延伸到消歧侧）。
type ScoreBreakdown struct {
	NodeID     string
	Cosine     float64
	BM25       float64
	Structural float64
	Score      float64
	// Mapped 标记该候选是被 negative_SameAs 排除的（不再进灰区）。
	Negative bool
}

// DisambiguateResult 是一个提及的消歧结论。
type DisambiguateResult struct {
	Mention string
	Zone    Zone
	// NodeID 是命中的已有节点（ZoneAutoMerge / ZoneGray 时非空）。
	NodeID string
	Score  float64
	// Breakdown 是全部候选的分数分解，按 Score 降序。
	Breakdown []ScoreBreakdown
	// Warnings 是护栏命中（如 alias 数超限强制人工审）。
	Warnings []string
}

// WriteReport 汇报一次写入实际落地了什么。
type WriteReport struct {
	MemoryID string
	Version  int
	NodeIDs  []string
	EdgeIDs  []string
	AliasIDs []string
}

// PipelineResult 是一次完整管线运行的结果。
type PipelineResult struct {
	EpisodeID string
	L0        L0Result
	Clusters  []Cluster
	Extracted ExtractResult
	// Disambiguation 是每个实体提及的三区结论。
	Disambiguation []DisambiguateResult
	Writes         []WriteReport
	// Reviewable 是本次新增进待审池的灰区对数（**不是合并数**）。
	Reviewable int
	// Warnings 是护栏命中（节点/episode 比例、灰区积压、alias 超限）。
	Warnings []string
	// Degraded 是整条管线的显式降级清单（L0 的拒写原因不在这里，走 Rejected）。
	Degraded []string
	// SkippedL2 表示因灰区积压护栏而暂停了 L2（只跑 L0/L1）。
	SkippedL2 bool
}

// EnabledL2 报告本次是否真的跑了 L2。
func (r PipelineResult) EnabledL2() bool { return !r.SkippedL2 }
