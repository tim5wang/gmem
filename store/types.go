// Package store 定义 Gmem 的领域类型与存储 Port 接口（docs/TD.md §11）。
//
// 本包**不含任何 SQL**：上层（召回侧契约层、整理层）只依赖这里的接口，
// 具体后端（SQLite / PostgreSQL）在各自的子包里实现，从而保证 D6 双后端成立。
package store

import (
	"errors"
	"fmt"
)

// ErrNotFound 表示请求的记录不存在。
var ErrNotFound = errors.New("gmem: not found")

// ---------------------------------------------------------------------------
// 枚举
// ---------------------------------------------------------------------------

// Scope 是记忆的归属边界。见 docs/TD.md §10 RecallRequest.Scope。
type Scope string

const (
	ScopeUser    Scope = "user"
	ScopeAgent   Scope = "agent"
	ScopeProject Scope = "project"
	ScopeGlobal  Scope = "global"
)

func (s Scope) Valid() bool {
	switch s {
	case ScopeUser, ScopeAgent, ScopeProject, ScopeGlobal:
		return true
	}
	return false
}

func (s Scope) String() string { return string(s) }

// MemoryKind 是记忆的类型。见 docs/TD.md §10 RecallRequest.Kinds。
type MemoryKind string

const (
	KindSemantic   MemoryKind = "semantic"
	KindEpisodic   MemoryKind = "episodic"
	KindProcedural MemoryKind = "procedural"
	KindDecision   MemoryKind = "decision"
)

func (k MemoryKind) Valid() bool {
	switch k {
	case KindSemantic, KindEpisodic, KindProcedural, KindDecision:
		return true
	}
	return false
}

func (k MemoryKind) String() string { return string(k) }

// ConceptStatus 是本体的认证状态（D1 的关键）。
//
// 只有 certified 参与硬契约：概念先以 draft 累积，认证后才约束召回。
type ConceptStatus string

const (
	ConceptDraft      ConceptStatus = "draft"
	ConceptCertified  ConceptStatus = "certified"
	ConceptDeprecated ConceptStatus = "deprecated"
)

// Valid 用于写入前校验；数据库 CHECK 约束是最后一道防线。
func (s ConceptStatus) Valid() bool {
	switch s {
	case ConceptDraft, ConceptCertified, ConceptDeprecated:
		return true
	}
	return false
}

func (s ConceptStatus) String() string { return string(s) }

// Relation 是边的类型名。传递性/跳数白名单属 P1（§5 R3），P0 只要求非空。
type Relation string

func (r Relation) String() string { return string(r) }

// ---------------------------------------------------------------------------
// 实体（与 §2 的六张主表一一对应）
// ---------------------------------------------------------------------------

// Episode 是不可变的事实来源：任何 memories/nodes 都能溯源回它并重建。
type Episode struct {
	ID          string
	Scope       Scope
	Namespace   string
	Role        string
	Content     string
	ContentHash string
	At          int64 // unix 秒
}

// Memory 是一条记忆事实。
//
// 双时态约定（§2）：
//   - TValid / TInvalid 是**世界时间**，即这条事实在现实中何时成立/失效；
//   - TCreated / TExpired 是**系统时间**，即 Gmem 何时写入/何时判定不再使用。
//
// 零值的 TInvalid / TExpired 表示"尚未失效"，落库为 NULL。
// 矛盾只置 TInvalid，**永不物理删除**。
type Memory struct {
	ID            string
	Scope         Scope
	ConceptID     string // 可空
	Kind          MemoryKind
	Content       string
	TValid        int64
	TInvalid      int64
	TCreated      int64
	TExpired      int64
	SourceEpisode string // 可空
	Importance    float64
	RecallCount   int
}

// Node 是图中的一个实体节点。
type Node struct {
	ID            string
	Scope         Scope
	ConceptID     string // 可空
	CanonicalName string
	PropsJSON     string // 默认 "{}"
}

// Edge 是节点之间的有向关系。
//
// 与 Memory 一样带四个时间戳（§2 "memories 和 edges 都带四个时间戳"）。
type Edge struct {
	ID       string
	FromID   string
	ToID     string
	Relation Relation
	Weight   float64
	TValid   int64
	TInvalid int64
	TCreated int64
	TExpired int64
}

// Concept 是本体概念。
type Concept struct {
	ID         string
	Name       string
	ParentID   string // 可空
	Status     ConceptStatus
	Definition string
}

// Alias 是 surface form 到节点的词典映射（L0 词典规则的最高置信来源）。
type Alias struct {
	Surface    string
	NodeID     string
	Source     string
	Confidence float64
}

// ---------------------------------------------------------------------------
// 查询/输入结构
// ---------------------------------------------------------------------------

// MemoryQuery 是 ListMemories 的过滤条件。
//
// AsOf 非空时按双时态回溯：既看世界时间（事实当时是否成立），
// 也看系统时间（当时 Gmem 是否已写入且未废弃）。
type MemoryQuery struct {
	Scope          Scope        // 空 = 不限
	Kinds          []MemoryKind // 空 = 不限
	ConceptID      string       // 空 = 不限
	SourceEpisode  string       // 空 = 不限
	AsOf           *int64
	IncludeExpired bool // true = 连 TExpired 非空的记录一起返回
	IncludeInvalid bool // true = 连 TInvalid 非空的记录一起返回（默认只返回当前有效事实）
	Limit          int  // 0 = 不限
	Offset         int
}

// Document 是交给 LexicalIndex 的一篇可检索文本。
type Document struct {
	ID    string
	Scope Scope
	Text  string
}

// Hit 是一路通道返回的命中项。
type Hit struct {
	ID    string
	Score float64 // 通道内分值，语义由通道决定；融合只用 Rank
	Rank  int     // 1-based
}

// Ranked 把按序排列的 ID 转成带 rank 的 Hit 列表，便于直接喂给 fusion.RRF。
func Ranked(ids []string, scores []float64) []Hit {
	hits := make([]Hit, 0, len(ids))
	for i, id := range ids {
		var s float64
		if i < len(scores) {
			s = scores[i]
		}
		hits = append(hits, Hit{ID: id, Score: s, Rank: i + 1})
	}
	return hits
}

// Vector 是交给 VectorIndex 的一条向量。
type Vector struct {
	ID     string
	Scope  Scope
	Values []float32
}

// NeighborQuery 是 GraphStore 的邻域查询条件。
type NeighborQuery struct {
	NodeID    string
	Relations []Relation // 空 = 不限；非空 = 只看白名单内的关系
	Hops      int        // <=0 视为 1；上限由实现裁剪
	AsOf      *int64     // 非空 = 该时刻有效的边
}

// EdgeHop 是带跳数的边，供召回侧做关系型扩展与 explain。
type EdgeHop struct {
	Edge  Edge
	Depth int
}

// ---------------------------------------------------------------------------
// 校验
// ---------------------------------------------------------------------------

// Validate 做写入前的最小校验。数据库约束仍会兜底。
func (e Episode) Validate() error {
	if e.ID == "" {
		return errors.New("episode: empty id")
	}
	if !e.Scope.Valid() {
		return fmt.Errorf("episode: invalid scope %q", e.Scope)
	}
	if e.Content == "" {
		return errors.New("episode: empty content")
	}
	return nil
}

func (m Memory) Validate() error {
	if m.ID == "" {
		return errors.New("memory: empty id")
	}
	if !m.Scope.Valid() {
		return fmt.Errorf("memory: invalid scope %q", m.Scope)
	}
	if !m.Kind.Valid() {
		return fmt.Errorf("memory: invalid kind %q", m.Kind)
	}
	if m.Content == "" {
		return errors.New("memory: empty content")
	}
	return nil
}

func (c Concept) Validate() error {
	if c.ID == "" {
		return errors.New("concept: empty id")
	}
	if c.Name == "" {
		return errors.New("concept: empty name")
	}
	if !c.Status.Valid() {
		return fmt.Errorf("concept: invalid status %q", c.Status)
	}
	return nil
}

func (n Node) Validate() error {
	if n.ID == "" {
		return errors.New("node: empty id")
	}
	if !n.Scope.Valid() {
		return fmt.Errorf("node: invalid scope %q", n.Scope)
	}
	if n.CanonicalName == "" {
		return errors.New("node: empty canonical name")
	}
	return nil
}

func (e Edge) Validate() error {
	if e.ID == "" {
		return errors.New("edge: empty id")
	}
	if e.FromID == "" || e.ToID == "" {
		return errors.New("edge: empty endpoint")
	}
	if e.Relation == "" {
		return errors.New("edge: empty relation")
	}
	return nil
}

func (a Alias) Validate() error {
	if a.Surface == "" {
		return errors.New("alias: empty surface")
	}
	if a.NodeID == "" {
		return errors.New("alias: empty node id")
	}
	return nil
}
