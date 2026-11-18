package ops

import (
	"context"

	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// 依赖端口
//
// ops 层只依赖三个注入点 + 一个存储端口：
//   - Extractor：P2 的提取管线（内部经 recall.Writer 写入，不引入任何 LLM SDK）；
//   - Backend：生命周期状态、审计、重定向、概念统计、物理删除。
//
// Backend 刻意不并进 store.Ports：store 是 P0 已发布的公开契约，加方法会破坏所有
// 既有实现（含 internal/vtest 的内存后端）。这与 P1 用 store.OntologyReader 表达
// "可选扩展"是同一手法。
//
// 但 Backend **不是可选的**：没有它 op_runs / audit_log 就落不了库，D3 的两种触发
// 也就无法证明走同一条路径。NewRunner 在 Backend 为 nil 时直接报错。
// ===========================================================================

// # 为什么 ops 里没有"写 memories"的入口
//
// D13 的遗留缺陷是"主表写了、FTS 没写"，它不报错，只是让答案变差。P1 用
// recall.Writer 把两者收进一个方法；如果 ops 再暴露一个 Writer 参数，就等于给了
// 后人一条"在这里顺手写一条记忆"的路，而那条路迟早会绕过写入编排。
//
// 因此本包的**结构性不变量**是：ops 里根本没有任何写 memories 的代码路径——
//   - 需要新记忆时只调 Extractor（P2 的 Pipeline，内部必经 recall.Writer）；
//   - 写入结果里的 LexicalIndexed 会被逐条核验（见 Extract 的 Apply），
//     核验失败即显式降级，而不是"看起来写成功了"。
//
// 这条不变量由 ops/guard_test.go 的源码扫描 + 变异用例钉死（绕过应变红）。

// EpisodeInput 是待提取的一段原文（= P2 的 extract.RawEpisode，但**不带包依赖**）。
//
// # 为什么要复制这几个字段，而不是直接 import extract
//
// ops 依赖 recall.Writer（写入编排）与 Backend（存储端口），直接 import extract 会
// 形成 `extract(test) → sqlite → ops → extract` 的编译环（P2 的测试用 package extract
// 内白盒导入 sqlite）。把跨层契约收成一个窄结构，环就断了，而 Wiring 只需一个 3 行
// 闭包（见 ExtractFunc），仍然跑的是 P2 的同一个 Pipeline。
type EpisodeInput struct {
	ID        string
	Scope     store.Scope
	Namespace string
	Role      string
	Content   string
	At        int64
}

// WriteOutcome 是提取落库后单条记忆的写入结果（透传 recall.PutResult）。
type WriteOutcome struct {
	MemoryID       string
	Version        int
	LexicalIndexed bool
}

// ExtractOutcome 是一次提取的结论（P2 的 PipelineResult 的窄视图）。
type ExtractOutcome struct {
	Writes   []WriteOutcome
	Warnings []string
	// Degraded 是整条管线的显式降级清单（**必须可见**）。
	Degraded []string
	// Rejected 是质量门拒写的原因（TD §7 纪律 4：Rejected 也落库、也可见）。
	Rejected []string
}

// Extractor 是 P2 提取管线的适配接口。
//
// P3 不新增任何 LLM 调用：这里只决定"什么时候跑提取"，抽取本身仍是 P2 的
// Pipeline（LLMClient 由宿主注入）。测试可注入假实现，线上用 ExtractFunc 包一层。
type Extractor interface {
	ExtractEpisode(ctx context.Context, ep EpisodeInput) (ExtractOutcome, error)
}

// ExtractFunc 让普通函数满足 Extractor，便于把 P2 的 Pipeline 接进来：
//
//	ops.ExtractFunc(func(ctx context.Context, ep ops.EpisodeInput) (ops.ExtractOutcome, error) {
//		res, err := pipeline.Run(ctx, extract.RawEpisode{
//			ID: ep.ID, Scope: ep.Scope, Namespace: ep.Namespace,
//			Role: ep.Role, Content: ep.Content, At: ep.At,
//		})
//		...
//	})
type ExtractFunc func(ctx context.Context, ep EpisodeInput) (ExtractOutcome, error)

// ExtractEpisode 实现 Extractor。
func (f ExtractFunc) ExtractEpisode(ctx context.Context, ep EpisodeInput) (ExtractOutcome, error) {
	return f(ctx, ep)
}

// State 是一条记忆的生命周期状态（docs/TD.md §7）。
//
// # importance 为什么分四列存
//
// TD §7 纪律 2：`base × recency × access_boost × grade` 四项各存一列。
// 只存总分的话，调系数就得回扫全库重算；存分项可以直接 REPLAY（§6 边权重同款理由）。
type State struct {
	MemoryID string
	Scope    store.Scope
	Status   MemoryStatus
	// 四个分项（每一项都可单独 explain）。
	Base        float64
	Recency     float64
	AccessBoost float64
	Grade       float64
	// Importance = Base × Recency × AccessBoost × Grade（落库以便排序）。
	Importance float64
	// StatusChangedAt 是进入当前状态的时刻（unix 毫秒）。归档判定要用它。
	StatusChangedAt int64
	LastRecalledAt  int64
	CreatedAt       int64
	UpdatedAt       int64
}

// AuditEntry 是一条审计记录（TD §8：op_id · 触发源 · 时间 · before/after diff）。
//
// 它足以回答"谁在何时因何触发了什么变更"：Actor/Trigger/Reason 是前两问，
// At 是何时，BeforeJSON/AfterJSON + Subject/StepKind 是最后两问。
type AuditEntry struct {
	Seq        int64
	OpID       string // = 运行的幂等 key（同一次运行的全部步骤共用）
	Op         OpName
	Scope      store.Scope
	Trigger    TriggerKind
	Actor      string
	Reason     string
	StepID     string
	StepKind   string
	Subject    string
	BeforeJSON string
	AfterJSON  string
	Changed    bool
	At         int64
}

// AuditQuery 是审计检索条件。
type AuditQuery struct {
	OpID    string
	Scope   store.Scope
	Subject string
	Op      OpName
	Limit   int
	// SinceSeq 用于增量拉取（Seq > SinceSeq）。
	SinceSeq int64
}

// RunRecord 是一次运行在幂等台账里的一行。
//
// op_runs 的主键就是幂等 key：**先插后跑**，因此并发/重入的第二次提交会在主键上
// 直接失败，而不是靠"先查再插"的竞态窗口。
type RunRecord struct {
	Key        string
	Op         OpName
	Scope      store.Scope
	Trigger    TriggerKind
	Actor      string
	Reason     string
	Status     RunStatus
	StartedAt  int64
	FinishedAt int64
	Note       string
}

// Redirect 是 Merge 之后旧 ID → 新 ID 的重定向（TD §8 Merge 语义）。
type Redirect struct {
	OldID  string
	NewID  string
	Scope  store.Scope
	Reason string
	At     int64
}

// ConceptStat 是概念的使用统计（D15 Prune 的判定依据）。
type ConceptStat struct {
	ConceptID  string
	UseCount   int
	LastUsedAt int64
	RefCount   int
}

// Backend 是 op 层需要的存储端口（生命周期/审计/重定向/概念统计/物理删除）。
//
// 所有方法都必须接受**绑定事务的 ctx**（Runner 在 Store.Tx 回调里调用），
// 这样审计与业务变更同生共死：回滚时不会留下"审计说有变更、数据却没变"。
type Backend interface {
	// ---- 生命周期（memory_state） ----
	PutState(ctx context.Context, s State) error
	EnsureState(ctx context.Context, s State) error
	GetState(ctx context.Context, memoryID string) (State, bool, error)
	ListStates(ctx context.Context, scope store.Scope, statuses []MemoryStatus, limit int) ([]State, error)
	DeleteStates(ctx context.Context, ids []string) error

	// ---- 幂等台账（op_runs） ----
	// InsertRun 是**插入**语义：key 已存在时返回 ErrDuplicateRun（并发安全的重入闸）。
	InsertRun(ctx context.Context, r RunRecord) error
	UpdateRun(ctx context.Context, r RunRecord) error
	GetRun(ctx context.Context, key string) (RunRecord, bool, error)
	ListRuns(ctx context.Context, q RunQuery) ([]RunRecord, error)

	// ---- 审计（audit_log） ----
	AppendAudit(ctx context.Context, e AuditEntry) (int64, error)
	ListAudit(ctx context.Context, q AuditQuery) ([]AuditEntry, error)

	// ---- Merge 重定向 ----
	PutRedirect(ctx context.Context, r Redirect) error
	// ResolveRedirect 跟随链（A→B→C 解析到 C），没有重定向时返回原 ID。
	ResolveRedirect(ctx context.Context, id string) (string, error)
	// ReassignAlias 把 surface 原本指向 fromNode 的词典项改指向 toNode（Merge 用）。
	//
	// 不能只“新增一条指向新节点的 alias”：旧条目仍在，ListAliases 会把旧节点
	// 重新带回召回候选，合并等于没做。
	ReassignAlias(ctx context.Context, surface, fromNode, toNode string) error

	// ---- 进度水位（阈值触发用） ----
	GetWatermark(ctx context.Context, scope store.Scope, op OpName) (int64, bool, error)
	PutWatermark(ctx context.Context, scope store.Scope, op OpName, at int64) error

	// ---- 概念统计与清理（D15） ----
	TouchConceptUsage(ctx context.Context, conceptID string, at int64) error
	GetConceptStat(ctx context.Context, conceptID string) (ConceptStat, bool, error)
	CountConceptRefs(ctx context.Context, conceptID string) (int, error)
	// ListStaleDrafts 返回 scope 内“活得太久”且无引用的 draft 概念。
	//
	// 时间基准是 **MAX(last_used_at, created_at)**（F-1）：只看 last_used_at 时，
	// 从未登记使用统计的结构化写入稿子（last_used_at 缺失）会被当成“很久以前”，
	// 刚建出来就进删除计划——那是数据丢失级的。
	//
	// scope 是必填的隔离边界（F-4）：无归属（scope=''）的历史稿子也会被返回，
	// 因为没有任何 scope 能“拥有”它们；调用方必须把这一点上报，不得静默。
	ListStaleDrafts(ctx context.Context, scope store.Scope, cutoff int64, limit int) ([]store.Concept, error)
	// DeleteConcept 物理删除概念；调用方必须先用 CountConceptRefs 保证无引用。
	DeleteConcept(ctx context.Context, conceptID string) error

	// ---- 物理删除（Prune 专用） ----
	// DeleteMemoriesPermanently 删除该 id 的**全部版本**与状态行，返回删除行数。
	// 它是 Prune 的唯一物理删除路径；FTS/向量由调用方用 Port 一并清理。
	DeleteMemoriesPermanently(ctx context.Context, ids []string) (int, error)
	// MemoryVersions 返回该 id 的全部版本号（Prune 需要精确删 FTS 的 (id, version)）。
	MemoryVersions(ctx context.Context, id string) ([]int, error)

	// ---- 扫描型 op 的读取面 ----
	// ListEpisodesPendingExtraction 返回尚无派生记忆的 episode（Extract 的输入）。
	ListEpisodesPendingExtraction(ctx context.Context, scope store.Scope, limit int) ([]store.Episode, error)
	// ListNodes 返回该 scope 的全部节点（Consolidate 的孤立节点检测）。
	ListNodes(ctx context.Context, scope store.Scope, limit int) ([]store.Node, error)
	// ListEdges 返回该 scope 的**当前版本**边（归档与权重维护）。
	ListEdges(ctx context.Context, scope store.Scope, limit int) ([]store.Edge, error)
	// CountUnconsolidatedMemories 返回"上次 Consolidate 之后新增"的记忆条数（阈值触发用）。
	CountUnconsolidatedMemories(ctx context.Context, scope store.Scope) (int, error)
}

// RunQuery 是 op_runs 的检索条件。
type RunQuery struct {
	Scope store.Scope
	Op    OpName
	Limit int
}

// ErrDuplicateRun 表示幂等 key 已存在（插入冲突）。
var ErrDuplicateRun = errDuplicateRun{}

type errDuplicateRun struct{}

func (errDuplicateRun) Error() string { return "ops: duplicate run key" }

// IsDuplicateRun 报告 err 是否是幂等 key 冲突。
func IsDuplicateRun(err error) bool {
	if err == nil {
		return false
	}
	type iser interface{ Is(error) bool }
	if e, ok := err.(iser); ok && e.Is(ErrDuplicateRun) {
		return true
	}
	_, ok := err.(errDuplicateRun)
	return ok
}

// ---------------------------------------------------------------------------
// 注入选项
// ---------------------------------------------------------------------------

// Option 是 Runner 的构造选项。
type Option func(*Runner)

// WithExtractor 注入 P2 提取管线（不注入时 Extract op 显式降级）。
func WithExtractor(e Extractor) Option { return func(r *Runner) { r.extractor = e } }

// WithConfig 覆盖配置（零值会被 withDefaults 补齐）。
func WithConfig(c Config) Option { return func(r *Runner) { r.cfg = c } }
