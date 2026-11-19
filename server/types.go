package server

// ===========================================================================
// HTTP API 契约（docs/TD.md §9「数据接口」）
//
// 这些结构是**前后端唯一的契约**：字段就是前端画图需要的全部信息。
// 判断"够不够"的标准是 §9 的视觉编码表——任何一栏缺字段，前端就得二次请求，
// 那等于没做完：
//
//	节点颜色   → concept_id / concept_status / origin（draft 判斜纹）
//	节点大小   → degree
//	红色描边   → matched（本次召回命中）
//	蓝色边     → possibly_same
//	虚线淡边   → invalidated（t_invalid 已失效仍返回）
//	时间轴     → t_valid / t_invalid + as_of 回显
//
// 命名统一 snake_case（与 docs/TD.md 的示例一致），便于前端直接消费。
// ===========================================================================

// GraphResponse 是 GET /api/graph 的返回体。
//
// 三种入口共用同一个返回体：meta.mode 区分 focus / search / overview，
// 前端不需要为入口写三套解析。
type GraphResponse struct {
	Meta     Meta      `json:"meta"`
	Nodes    []Node    `json:"nodes"`
	Edges    []Edge    `json:"edges"`
	Clusters []Cluster `json:"clusters"`
	// ClusterEdges 只在**折叠视图**里出现（truncated=true）：节点被折叠成概念团后，
	// 团与团之间的关系也需要一条聚合边，否则前端只能画一堆散点。
	ClusterEdges []ClusterEdge `json:"cluster_edges,omitempty"`
}

// Meta 是本次视图的元信息（可解释性：为什么是这些节点、有没有被截断）。
type Meta struct {
	// Mode 是入口：focus | search | overview。
	Mode string `json:"mode"`
	// Focus / Query 回显请求参数（视图即 URL）。
	Focus string `json:"focus,omitempty"`
	Query string `json:"query,omitempty"`
	Scope string `json:"scope"`
	// Hops 是**本次请求**的跳数（0 表示概览模式不适用）。
	Hops int `json:"hops"`
	// AsOf 是双时态视角（unix 秒），nil = 当前。
	AsOf *int64 `json:"as_of,omitempty"`
	// Budget 是生效预算，BudgetMax 是硬上限（TD §9 渲染上限）。
	Budget    int `json:"budget"`
	BudgetMax int `json:"budget_max"`
	// Seeds 是本次的点集（focus 的节点 / q 命中的实体）。
	Seeds []string `json:"seeds,omitempty"`
	// NodeCount / EdgeCount 是**折叠前**的真实规模；
	// RenderedNodes / RenderedClusters 是实际返回的规模。
	NodeCount        int `json:"node_count"`
	EdgeCount        int `json:"edge_count"`
	ClusterCount     int `json:"cluster_count"`
	RenderedNodes    int `json:"rendered_nodes"`
	RenderedClusters int `json:"rendered_clusters"`
	// Truncated 必须在折叠时**显式**为 true（不得静默截断，TD §9）。
	Truncated bool `json:"truncated"`
	// Relations 回显生效的关系跳数表。
	//
	// 它是"视觉与检索共用一张表"的可验证证据：值直接来自
	// recall.Config().RelationRules，任何人都能拿着 /api/graph 的响应
	// 与 Recall 的 plan 对照，确认两边没有各写一套。
	Relations []RelationLimit `json:"relations"`
	// Degraded 是显式降级清单（缺本体枚举、缺边溯源端口等），不得静默。
	Degraded []string `json:"degraded,omitempty"`
	// Recall 只在 search 模式出现（q 命中的依据）。
	Recall *RecallMeta `json:"recall,omitempty"`
}

// RelationLimit 是一行关系跳数上限（来自 recall 的声明式表）。
type RelationLimit struct {
	Relation   string `json:"relation"`
	MaxHops    int    `json:"max_hops"`
	Transitive bool   `json:"transitive"`
}

// RecallMeta 是 search 模式下 Recall 的摘要（不复制整份 plan，避免响应膨胀）。
type RecallMeta struct {
	Status   string   `json:"status"`
	Coverage float64  `json:"coverage"`
	Items    int      `json:"items"`
	Anchors  []string `json:"anchors,omitempty"`
}

// Node 是图中的一个实体节点。
type Node struct {
	ID            string `json:"id"`
	CanonicalName string `json:"canonical_name"`
	Scope         string `json:"scope"`
	ConceptID     string `json:"concept_id,omitempty"`
	// ConceptName / ConceptStatus / Origin 是视觉编码的输入：
	// 颜色按 concept，draft 判斜纹，origin=implicit 说明概念是写入时隐式落成的。
	ConceptName   string `json:"concept_name,omitempty"`
	ConceptStatus string `json:"concept_status,omitempty"`
	Origin        string `json:"origin,omitempty"`
	SourceEpisode string `json:"source_episode,omitempty"`
	// Degree 是**本次返回子图内**的关联边数（决定节点大小）。
	Degree int `json:"degree"`
	// Depth 是从种子出发的跳数（0 = 种子本身）。
	Depth int `json:"depth"`
	// Matched 标记"本次召回/焦点命中的实体"（决定红描边）。
	Matched bool `json:"matched"`
	// Cluster 是所属聚簇键（concept_id，未归属时为空）。
	Cluster string `json:"cluster,omitempty"`
	// X / Y 是服务端算好的预布局坐标（确定性，同一输入必得同一坐标）。
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// Edge 是节点之间的一条有向关系。
type Edge struct {
	ID       string  `json:"id"`
	From     string  `json:"from"`
	To       string  `json:"to"`
	Relation string  `json:"relation"`
	Weight   float64 `json:"weight"`
	// TValid / TInvalid 是**世界时间**（如实返回，前端画时间轴用）。
	TValid   int64 `json:"t_valid"`
	TInvalid int64 `json:"t_invalid"`
	// Invalidated 表示"在本次 as_of 视角下这条边已失效"→ 前端画虚线淡边。
	// 它**不会**让边被过滤掉：历史必须可见（Graphiti/Zep 的做法，§9）。
	Invalidated bool `json:"invalidated"`
	// PossiblySame 标记待审的疑似重复边（蓝色，可直接在图上裁决）。
	PossiblySame bool `json:"possibly_same"`
	// Depth 是遍历到这条边时的跳数。
	Depth int `json:"depth"`
}

// Cluster 是一个概念团（超预算时的折叠单位）。
type Cluster struct {
	// ID 是聚簇键（concept_id；未归属概念为 "(unclustered)"）。
	ID            string  `json:"id"`
	ConceptID     string  `json:"concept_id,omitempty"`
	ConceptName   string  `json:"concept_name,omitempty"`
	ConceptStatus string  `json:"concept_status,omitempty"`
	Origin        string  `json:"origin,omitempty"`
	Count         int     `json:"count"`
	X             float64 `json:"x"`
	Y             float64 `json:"y"`
	// Matched 表示该团里有命中实体（前端可给整团描红边）。
	Matched bool `json:"matched"`
}

// ClusterEdge 是折叠视图里两个概念团之间的聚合边。
type ClusterEdge struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Count int    `json:"count"`
	// Invalidated 表示这条聚合边**只**由已失效的边构成（画虚线）。
	Invalidated bool `json:"invalidated"`
}

// EntityCard 是 GET /api/entity/{id} 的返回体（TD §9「点击节点 → 侧栏实体卡」）。
type EntityCard struct {
	ID            string      `json:"id"`
	CanonicalName string      `json:"canonical_name"`
	Scope         string      `json:"scope"`
	Concept       *ConceptRef `json:"concept,omitempty"`
	// Source 是溯源：该实体从哪段原文来的（§2 第 1 条）。
	Source *EpisodeRef `json:"source,omitempty"`
	// Relations 是按关系分组的关系视图：每组给出**当前生效**的一条与被它替换掉的边。
	Relations []RelationGroup `json:"relations"`
	// Edges 是扁平的全量关系（含方向与对端），前端列表直接用。
	Edges []EntityEdge `json:"edges"`
	// Why 是 as_of 视角下"为何生效 / 它替换了什么"的人话解释。
	Why  []string `json:"why"`
	Meta Meta     `json:"meta"`
}

// ConceptRef 是实体卡里的概念摘要。
type ConceptRef struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Origin     string `json:"origin,omitempty"`
	Definition string `json:"definition,omitempty"`
}

// EpisodeRef 是实体卡里的来源摘要。
type EpisodeRef struct {
	ID      string `json:"id"`
	At      int64  `json:"at"`
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

// EntityEdge 是实体卡里的一条关系（带方向与对端）。
type EntityEdge struct {
	Edge
	// Direction 是 out（本实体是 from）或 in（本实体是 to）。
	Direction string `json:"direction"`
	// Counterpart 是对端节点 ID / 名字，便于侧栏直接渲染"它 → 谁"。
	Counterpart        string `json:"counterpart"`
	CounterpartName    string `json:"counterpart_name,omitempty"`
	CounterpartConcept string `json:"counterpart_concept,omitempty"`
}

// RelationGroup 是同一关系的归组视图。
type RelationGroup struct {
	Relation string `json:"relation"`
	// Effective 是 as_of 视角下仍然有效的那条边（单值关系里就是"现在的答案"）。
	Effective *EntityEdge `json:"effective,omitempty"`
	// Replaced 是被它替换掉的旧边（已失效但不删除）。
	Replaced []EntityEdge `json:"replaced,omitempty"`
	// Reason 解释这一组的生效判定。
	Reason string `json:"reason"`
}

// ReviewRequest 是 POST /api/review 的请求体。
//
// **二段确认**（TD §9「唯一的写入口是待审裁决，且必须二次确认」）：
// Confirmed 必须显式为 true，缺省即拒绝——误合并是记忆系统最难恢复的错误。
type ReviewRequest struct {
	EdgeID    string `json:"edge_id"`
	Verdict   string `json:"verdict"`
	Confirmed bool   `json:"confirmed"`
	// Scope 可省略：默认由边自身的节点归属推导。
	Scope string `json:"scope,omitempty"`
	Note  string `json:"note,omitempty"`
}

// ReviewResponse 是裁决结果。
type ReviewResponse struct {
	EdgeID  string `json:"edge_id"`
	Verdict string `json:"verdict"`
	// Op 是实际执行的 op（merge / disambiguate），便于调用方核对走了哪条路。
	Op     string   `json:"op"`
	Status string   `json:"status"`
	Result string   `json:"result,omitempty"`
	Steps  []string `json:"steps,omitempty"`
	// Degraded / Warnings 透传 op 的显式降级信息。
	Degraded []string `json:"degraded,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// ErrorResponse 是统一的错误体：message 必须可读（不是裸 err.Error()）。
type ErrorResponse struct {
	Error   string `json:"error"`           // 机器可读代码：bad_request / not_found / ...
	Message string `json:"message"`         // 人可读的一句话
	Field   string `json:"field,omitempty"` // 出错的参数名（若有）
}
