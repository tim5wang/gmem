// Package ops 实现 docs/TD.md §7（记忆生命周期）与 §8（op 层：两种触发统一，D3）。
//
// # 为什么有这一层
//
// 业界二选一：要么把整理挂在 hot-path 的 tool 上（Letta/LangMem），要么完全后台隐式
// （Mem0/Letta）。两条路径分开写会分别长出 bug，而且行为不可比。Gmem 的取舍（D3）是
// **同一套原子 op，两种编排**：触发器只决定"什么时候跑"，op 决定"跑什么"。
//
// 因此本包只有一份执行路径：
//
//	触发器（after_write 去抖 / threshold / idle·cron / agent）
//	    └─► Runner.Run（幂等 key · 预算 · 可取消 · 一个事务）
//	           └─► Op.Plan / Op.Apply（8 个原子 op + Promote）
//	                  └─► audit_log（op_id · 触发源 · 时间 · before/after diff）
//
// 任何"给 Agent 单独写一套"的实现都是缺陷：它会让自动与显式两条路径的语义漂移。
//
// # 三条硬约束
//
//   - 幂等 key = hash(scope, op_name, 输入摘要)。重跑安全：同一 key 第二次必然跳过，
//     不重复应用、不双写（op_runs 与业务变更在同一事务里）。
//   - 一律接 context.Context。ctx 取消 → 事务整体回滚，不留半截状态；随后用一条
//     独立事务记下 cancelled，保留"为什么没有变化"的痕迹。
//   - 所有 memories 写入仍必经 P1 的写入编排（recall.Writer → FTS 同步）。本包**从未**
//     提供写 memories 的路径：需要新记忆时只调 Extractor（见 backend.go 的说明与守卫测试）。
package ops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/tim5wang/gmem/store"
)

// ErrNotFound 表示请求的记录不存在（用于 op_runs / redirects 的查询）。
var ErrNotFound = errors.New("ops: not found")

// ---------------------------------------------------------------------------
// op 名
// ---------------------------------------------------------------------------

// OpName 是原子 op 的名字（docs/TD.md §8 的 8 个 op + Promote）。
type OpName string

const (
	OpExtract      OpName = "extract"
	OpDisambiguate OpName = "disambiguate"
	OpMerge        OpName = "merge"
	OpLinkUpdate   OpName = "link_update"
	OpInvalidate   OpName = "invalidate"
	OpConsolidate  OpName = "consolidate"
	OpDecay        OpName = "decay"
	OpPrune        OpName = "prune"
	// OpPromote 是 D15 的清理路径之一（draft → certified）。
	// TD §8 的图里列了 8 个 op，工具表里另有 Promote；P3 把它也做成 op，
	// 这样 Agent 认证与自动认证走同一套代码（D3）。
	OpPromote OpName = "promote"
)

// AllOps 返回全部 op 名（顺序固定，便于测试与文档）。
func AllOps() []OpName {
	return []OpName{
		OpExtract, OpDisambiguate, OpMerge, OpLinkUpdate,
		OpInvalidate, OpConsolidate, OpDecay, OpPrune, OpPromote,
	}
}

// ---------------------------------------------------------------------------
// 预算
// ---------------------------------------------------------------------------

// Budget 是一次 op 的预算（TD §8 op 契约）。
//
// 零值表示不限。Runner 会把 op 自报的 Budget 与 Trigger 携带的 Budget **取更严者**
// 后作为整次运行的超时/步骤上限——预算必须能被触发器下调，否则"自动跑全量"会把
// 一次交互卡死。
type Budget struct {
	Tokens   int
	Duration time.Duration
	IO       int
	// Steps 是本次运行最多应用多少步（<=0 不限）。
	Steps int
}

// stricter 返回更严的预算（非零且更小者胜）。
func (b Budget) stricter(o Budget) Budget {
	out := b
	if o.Tokens > 0 && (out.Tokens == 0 || o.Tokens < out.Tokens) {
		out.Tokens = o.Tokens
	}
	if o.Duration > 0 && (out.Duration == 0 || o.Duration < out.Duration) {
		out.Duration = o.Duration
	}
	if o.IO > 0 && (out.IO == 0 || o.IO < out.IO) {
		out.IO = o.IO
	}
	if o.Steps > 0 && (out.Steps == 0 || o.Steps < out.Steps) {
		out.Steps = o.Steps
	}
	return out
}

// ---------------------------------------------------------------------------
// 触发源
// ---------------------------------------------------------------------------

// TriggerKind 是触发源（TD §8 的四种 + 手动）。
type TriggerKind string

const (
	// TriggerAfterWrite 是写入后去抖：等对话安静再固化，而不是每轮都跑。
	TriggerAfterWrite TriggerKind = "after_write"
	// TriggerThreshold 是"未整理条目达到 N"。
	TriggerThreshold TriggerKind = "threshold"
	// TriggerIdle 是空闲触发。
	TriggerIdle TriggerKind = "idle"
	// TriggerCron 是定时触发（与 idle 共用同一执行路径，只是判定来源不同）。
	TriggerCron TriggerKind = "cron"
	// TriggerAgent 是 Agent 显式工具调用。
	TriggerAgent TriggerKind = "agent"
)

func (k TriggerKind) Valid() bool {
	switch k {
	case TriggerAfterWrite, TriggerThreshold, TriggerIdle, TriggerCron, TriggerAgent:
		return true
	}
	return false
}

// Automatic 报告该触发源是否属于"自动"（用于审计里区分谁发起的）。
func (k TriggerKind) Automatic() bool { return k != TriggerAgent }

// Trigger 描述一次运行的触发来源（TD §8 的 Trigger 结构）。
type Trigger struct {
	Kind   TriggerKind
	Key    string // 去抖键：scope 或 thread
	After  time.Duration
	Budget Budget
	// Actor 是"谁"：Agent 工具调用填 agent id，自动触发填 "auto"。
	Actor string
	// Reason 是"为什么"（审计可读性），例如 "agent 发现两条记忆冲突"。
	Reason string
	// At 是触发时刻（unix 毫秒）。仅作审计元数据，不参与幂等 key。
	At int64
}

// normalize 补齐零值：Actor 按触发源推断，Kind 缺省为 agent。
func (t Trigger) normalize() Trigger {
	if !t.Kind.Valid() {
		t.Kind = TriggerAgent
	}
	if t.Actor == "" {
		if t.Kind.Automatic() {
			t.Actor = "auto"
		} else {
			t.Actor = "agent"
		}
	}
	return t
}

// ---------------------------------------------------------------------------
// 幂等
// ---------------------------------------------------------------------------

// Input 是一次 op 的输入。
//
// 用一个联合结构而不是每个 op 一个类型：幂等摘要必须**跨 op 可比较**（
// hash(scope, op_name, 输入摘要) 里的"输入摘要"就是它的 JSON），
// 用联合结构可以让摘要算法只有一处实现。字段全部 omitempty，保证
// 同一语义输入产生同一段 JSON（json 对 struct 字段顺序是确定的）。
type Input struct {
	// Window 是本次扫描的时间上界（unix 毫秒）。
	//
	// 扫描型 op（Extract/Consolidate/Decay/Prune）用它把"这一次的时间窗口"
	// 纳入幂等摘要：同一窗口重跑安全跳过，新窗口才是新工作。
	Window int64 `json:"window,omitempty"`
	// Limit 限制本次处理的条目数（<=0 用 Config 默认）。
	Limit int `json:"limit,omitempty"`

	// Episodes 是显式指定的提取目标（为空则扫"尚无派生记忆的 episode"）。
	Episodes []ExtractTarget `json:"episodes,omitempty"`

	// Pairs 是显式指定的疑似重复对（Disambiguate）。
	Pairs []PairRef `json:"pairs,omitempty"`
	// Verdicts 是 Agent 对疑似重复对的裁决（Disambiguate）。
	Verdicts []PairVerdict `json:"verdicts,omitempty"`

	Merge      *MergeInput      `json:"merge,omitempty"`
	Link       *LinkInput       `json:"link,omitempty"`
	Invalidate *InvalidateInput `json:"invalidate,omitempty"`
	Promote    *PromoteInput    `json:"promote,omitempty"`

	// IncludeInvalidated 只影响 Prune：默认 false（TD §7 纪律 3：默认只作用于 Archived）。
	IncludeInvalidated bool `json:"include_invalidated,omitempty"`
	// IncludeDrafts 只影响 Prune：是否清理长期未用的 draft 概念（D15）。nil = 默认 true。
	IncludeDrafts *bool `json:"include_drafts,omitempty"`
	// DraftTTLSeconds 覆盖 Config.DraftTTL（测试用小窗口）。
	DraftTTLSeconds int64 `json:"draft_ttl_seconds,omitempty"`
}

// IncludeDraftsDefault 返回"是否清理 draft"的生效值（默认 true）。
func (in Input) IncludeDraftsDefault() bool {
	if in.IncludeDrafts == nil {
		return true
	}
	return *in.IncludeDrafts
}

// IdempotencyKey 计算幂等 key = hash(scope, op_name, 输入摘要)。
//
// 前缀带上 op 名只是可读性（审计里一眼看出这是什么 op），哈希输入里也含 op，
// 因此不同 op 的 key 不可能碰撞成同一个。
func IdempotencyKey(scope store.Scope, op OpName, in Input) (string, error) {
	payload := struct {
		Scope string `json:"scope"`
		Op    string `json:"op"`
		Input Input  `json:"input"`
	}{Scope: scope.String(), Op: string(op), Input: in}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("ops: 幂等摘要序列化失败: %w", err)
	}
	sum := sha256.Sum256(b)
	return string(op) + "-" + hex.EncodeToString(sum[:16]), nil
}

// ---------------------------------------------------------------------------
// Step / Diff：op 契约的载荷
// ---------------------------------------------------------------------------

// Step 是 Plan 产出的一个可执行步骤。
//
// Plan 与 Apply 分开是刻意的：同一份计划既能被 Runner 逐条应用（带预算/取消点），
// 也能被 Agent 先看到（ReviewSimilar 返回的就是计划），两者不重复实现。
type Step struct {
	// ID 是 op 内唯一的步骤标识（参与审计主键）。
	ID string `json:"id"`
	// Kind 是子动作名（例如 "promote_concept"），审计里用它回答"改了什么"。
	Kind string `json:"kind"`
	// Subject 是受影响的实体 ID（便于按实体检索审计）。
	Subject string `json:"subject,omitempty"`
	// Data 是步骤载荷（具体类型由各 op 决定）。
	Data any `json:"-"`
	// Note 是给人看的一句话（审计与 explain 共用）。
	Note string `json:"note,omitempty"`
}

// Diff 是 Apply 的结果（TD §8 的 before/after diff）。
type Diff struct {
	// Changed 为 false 表示这一步是幂等空转（例如"已经认证过"，"已经失效"）。
	Changed bool
	// Summary 是一句话摘要（审计列表直接展示）。
	Summary string
	// Before/After 是 JSON 文本形式的变更前后状态。
	Before string
	After  string
	// Subjects 是本次变更涉及的实体 ID。
	Subjects []string
	// Degraded 记录显式降级（例如"没有 Graph port，边迁移跳过"）。
	Degraded []string
	// Warnings 是护栏命中（例如"概念仍被引用，跳过删除"）。
	Warnings []string
	// Trail 是本步骤内部**额外落库**的中间状态迁移（F-10a）。
	//
	// # 为什么需要它
	//
	// 有些步骤会跑两次 PutState（例如 Candidate → Active → Invalidated）。
	// audit_log 是按 Step 追加的，于是一次步骤只留一行，中间那一步在审计里彻底消失——
	// "这条记录怎么从 candidate 走到 invalidated 的"就无法重建。
	// Trail 让 op 把中间迁移交给 Runner，由 Runner 逐条写成 StepKind=<kind>_via 的审计行。
	Trail []Diff
}

// ---------------------------------------------------------------------------
// 运行状态
// ---------------------------------------------------------------------------

// RunStatus 是一次运行的结论。
type RunStatus string

const (
	// RunRunning 表示运行已登记但尚未结束（幂等台账的中间态）。
	// 它同时是并发闸：第二次提交看到 running 就不再执行。
	RunRunning RunStatus = "running"
	// RunApplied：至少应用了一个产生变更的步骤。
	RunApplied RunStatus = "applied"
	// RunNoop：计划为空或全部步骤都是空转。
	RunNoop RunStatus = "noop"
	// RunDuplicate：同一幂等 key 已应用过，本次跳过（重跑安全）。
	RunDuplicate RunStatus = "duplicate"
	// RunCancelled：ctx 取消/超时，事务已回滚，没有半截状态。
	RunCancelled RunStatus = "cancelled"
	// RunFailed：返回了错误（同样回滚）。
	RunFailed RunStatus = "failed"
)

// Result 是一次运行的完整结果。
type Result struct {
	Op     OpName
	Scope  store.Scope
	Key    string
	Status RunStatus
	Steps  []StepResult
	// Budget 是本次生效的预算（触发器下调后的结果，便于审计"为什么没跑完"）。
	Budget Budget
	// Degraded / Warnings 汇总自各步骤，调用方必须能看到。
	Degraded []string
	Warnings []string
}

// StepResult 是单个步骤的结果。
type StepResult struct {
	Step Step
	Diff Diff
}

// Request 是一次 op 运行的请求（触发器与 Agent 共用）。
type Request struct {
	Op      OpName
	Scope   store.Scope
	Trigger Trigger
	Input   Input
	// Key 可显式覆盖幂等 key（去抖场景由 Debouncer 持有）；留空则按 Input 计算。
	Key string
}

// ---------------------------------------------------------------------------
// Op 契约
// ---------------------------------------------------------------------------

// Op 是原子操作（docs/TD.md §8 的 op 契约）。
//
// 自动触发与 Agent 显式调用**只允许**经由 Runner.Run → Plan/Apply，
// 不允许任何 op 另开一条"给 Agent 用"的入口（D3）。
type Op interface {
	// Name 是 op 名（参与幂等 key）。
	Name() OpName
	// Budget 是本 op 的默认预算（Runner 会与 Trigger 的预算取更严者）。
	Budget() Budget
	// Plan 在事务内产出计划。扫描型 op 在这里读库，因此计划与数据一致。
	Plan(ctx TxContext) ([]Step, error)
	// Apply 在**同一事务内**应用一步。
	Apply(ctx TxContext, s Step) (Diff, error)
}

// TxContext 是 op 在事务内可见的上下文。
//
// 把它定义成接口而不是结构体：op 只需要"读库/写状态/取时钟/查取消"这几件事，
// 暴露成窄接口能让守卫测试（不得绕过 Writer）与假实现都更容易。
type TxContext interface {
	// Context 返回**携带事务句柄**的 ctx：传给任何 Port/Backend 调用即落入同一事务。
	Context() context.Context
	// Scope 是本次运行的隔离边界（必填）。
	Scope() store.Scope
	// Store 是事务内的 Store（当前版本读、失效标记、概念与别名）。
	Store() store.Store
	// Ports 是四个端口（op 需要 Lexical/Vector/Graph 做同事务的派生索引维护）。
	Ports() store.Ports
	// Input 是本次运行的输入（含扫描窗口、显式目标、裁决）。
	Input() Input
	// Backend 是生命周期/审计/概念计数的存储端口。
	Backend() Backend
	// Extractor 是 P2 的提取管线适配器（可能为 nil：显式降级）。
	Extractor() Extractor
	// Cfg 是生效配置。
	Cfg() Config
	// Now 返回本次运行固定的时钟（同一次运行里时间不前移，避免 diff 抖动）。
	Now() time.Time
	// Check 返回 ctx 的取消原因（op 的循环里必须定期调用）。
	Check() error
	// Spend 记一次 IO 消耗并检查预算；超预算返回 ErrBudgetExceeded（正常停手点）。
	Spend(io int) error
	// SpendTokens 记一次 token 消耗并检查预算；语义同 Spend。
	//
	// 为什么单独一个方法：Tokens 与 IO 是两个独立维度（抽取按 token 估、扫描按条数估），
	// 合成一个“消耗量”会让两者互相污染，预算也就不可解释了。
	SpendTokens(n int) error
	// Note 记录一条“本次运行里的显式降级/停手原因”，由 Runner 汇总进 Degraded。
	//
	// 预算触顶时必须能说出来：“预算耗尽，只计划了 N 步”不得是静默截断（F-6）。
	Note(msg string)
}

// ---------------------------------------------------------------------------
// 配置
// ---------------------------------------------------------------------------

// MemoryStatus 是生命周期状态（docs/TD.md §7）。
type MemoryStatus string

const (
	StatusCandidate   MemoryStatus = "candidate"
	StatusActive      MemoryStatus = "active"
	StatusRejected    MemoryStatus = "rejected"
	StatusInvalidated MemoryStatus = "invalidated"
	StatusStale       MemoryStatus = "stale"
	StatusArchived    MemoryStatus = "archived"
)

// Valid 校验状态机里的合法取值（数据库 CHECK 是最后一道防线）。
func (s MemoryStatus) Valid() bool {
	switch s {
	case StatusCandidate, StatusActive, StatusRejected, StatusInvalidated, StatusStale, StatusArchived:
		return true
	}
	return false
}

// Terminal 报告该状态是否是终态（只有 Archived 是；Invalidated 不是）。
func (s MemoryStatus) Terminal() bool { return s == StatusArchived }

// Config 是 op 层的可调参数。
//
// 所有阈值都是**待校准**的设计起点（docs/TD.md §13 已明确：阈值不是实测结论）。
type Config struct {
	// AfterWrite 是写入后去抖延迟（LangMem 的 after=N）。
	// ⚠️ 待校准：默认 400ms。
	AfterWrite time.Duration
	// IdleAfter 是空闲触发阈值（多久没有写入算"安静"）。
	// ⚠️ 待校准：默认 2s。
	IdleAfter time.Duration
	// Threshold 是"未整理条目达到 N"的 N。
	// ⚠️ 待校准：默认 20。
	Threshold int

	// DecayHalfLife 是 importance 的时间衰减半衰期。
	// ⚠️ 待校准：30 天（与 recall 的 RecencyHalfLife 同起点）。
	DecayHalfLife time.Duration
	// DecayFloor 是 recency 分项下界，保证旧而正确的记忆不会被压没。
	DecayFloor float64
	// AccessBoostK 是 access_boost = 1 + K·ln(1+recall_count) 的 K。
	// ⚠️ 待校准：默认 0.2。
	AccessBoostK float64
	// GradeFactors 是概念状态对 importance 的折扣（draft 打折而非 0）。
	GradeFactors map[store.ConceptStatus]float64

	// StaleBelow：composite < 它就转 Stale。
	StaleBelow float64
	// ArchiveBelow：composite < 它就（在 Consolidate 里）转 Archived。
	ArchiveBelow float64
	// ArchiveAfterDays：Invalidated 保留期满多少天后归档（§7 的"保留期满"）。
	ArchiveAfterDays int
	// StaleArchiveAfterDays：Stale 多少天后归档。
	StaleArchiveAfterDays int

	// DraftTTL：draft 概念多久未被使用就进 Prune 候选（D15 的清理窗口）。
	// ⚠️ 待校准：默认 30 天。
	DraftTTL time.Duration
	// MinContentLength 是 Candidate → Active 的最小内容长度（与 extract 的 L0 同口径）。
	MinContentLength int

	// Limit 是扫描型 op 的默认单次上限（防止一次运行无界）。
	Limit int
	// Budget 是 op 层默认预算。
	Budget Budget

	// Now 是可注入时钟（测试用；nil = time.Now）。
	Now func() time.Time
	// Scheduler 是可注入调度器（去抖用；nil = 真实定时器）。
	Scheduler Scheduler
}

// DefaultConfig 返回补齐后的默认配置。
func DefaultConfig() Config {
	return Config{
		AfterWrite:            400 * time.Millisecond,
		IdleAfter:             2 * time.Second,
		Threshold:             20,
		DecayHalfLife:         30 * 24 * time.Hour,
		DecayFloor:            0.05,
		AccessBoostK:          0.2,
		GradeFactors:          map[store.ConceptStatus]float64{store.ConceptCertified: 1.0, store.ConceptDraft: 0.6, store.ConceptDeprecated: 0.0},
		StaleBelow:            0.20,
		ArchiveBelow:          0.08,
		ArchiveAfterDays:      30,
		StaleArchiveAfterDays: 90,
		DraftTTL:              30 * 24 * time.Hour,
		MinContentLength:      10,
		Limit:                 100,
		Budget:                Budget{Duration: 30 * time.Second, Steps: 1000, IO: 2000},
	}
}

// withDefaults 补齐零值字段（零值 Config 必须可用）。
func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if c.AfterWrite <= 0 {
		c.AfterWrite = d.AfterWrite
	}
	if c.IdleAfter <= 0 {
		c.IdleAfter = d.IdleAfter
	}
	if c.Threshold <= 0 {
		c.Threshold = d.Threshold
	}
	if c.DecayHalfLife <= 0 {
		c.DecayHalfLife = d.DecayHalfLife
	}
	if c.DecayFloor <= 0 {
		c.DecayFloor = d.DecayFloor
	}
	if c.AccessBoostK <= 0 {
		c.AccessBoostK = d.AccessBoostK
	}
	if len(c.GradeFactors) == 0 {
		c.GradeFactors = d.GradeFactors
	}
	if c.StaleBelow <= 0 {
		c.StaleBelow = d.StaleBelow
	}
	if c.ArchiveBelow <= 0 {
		c.ArchiveBelow = d.ArchiveBelow
	}
	if c.ArchiveAfterDays <= 0 {
		c.ArchiveAfterDays = d.ArchiveAfterDays
	}
	if c.StaleArchiveAfterDays <= 0 {
		c.StaleArchiveAfterDays = d.StaleArchiveAfterDays
	}
	if c.DraftTTL <= 0 {
		c.DraftTTL = d.DraftTTL
	}
	if c.MinContentLength <= 0 {
		c.MinContentLength = d.MinContentLength
	}
	if c.Limit <= 0 {
		c.Limit = d.Limit
	}
	if c.Budget == (Budget{}) {
		c.Budget = d.Budget
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Scheduler == nil {
		c.Scheduler = RealScheduler{}
	}
	return c
}

// nowMillis 是统一的"当前时刻"（unix 毫秒）。
func nowMillis(c Config) int64 { return c.Now().UnixMilli() }

// gradeOf 返回概念状态对应的 grade 分项。
func gradeOf(c Config, status store.ConceptStatus, hasConcept bool) float64 {
	if !hasConcept {
		// 没有概念的记忆不因本体未对齐而受罚（D14 的口径：缺维度 ≠ 有反证）。
		return 1
	}
	if g, ok := c.GradeFactors[status]; ok {
		return g
	}
	return 1
}
