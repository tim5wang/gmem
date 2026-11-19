// Package memory 是 Gmem 的**接口面（D5）**：五个动词 + history + 维护面。
//
// # 为什么需要这一层
//
// docs/TD.md §10 的要求是"同一接口面三层薄壳"。CLI 与 MCP 各自解析参数，
// 但**动词语义只能有一份**：remember 走哪条写入路径、forget 是失效还是删除、
// history 的版本链怎么取，这些决定若在两个壳里各写一遍，就会出现
// "CLI 与 MCP 行为不同"的分叉，而且这类分叉不会报错，只会慢慢漂移。
//
// 因此本包只做**编排**，不做业务逻辑：
//
//	remember  → extract.Pipeline（L0/L1/消歧/写入，P2 的既有管线）
//	          或 recall.Writer（显式 kind 的结构化写入）
//	recall    → recall.Recaller
//	get/list  → store.Store
//	forget    → ops.Maintainer.Supersede（只置 t_invalid，不物理删除）
//	history   → 后端可选接口 HistoryReader（缺实现时显式降级）
//	维护面     → ops.Maintainer（与自动触发共用同一个 Runner）
//
// 它**不**import sqlite：后端由 cmd/gmem 装配后以 Port/接口注入，
// 因此 D6（双后端）与 server/guard_test.go 的依赖方向守卫继续成立。
package memory

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tim5wang/gmem/extract"
	"github.com/tim5wang/gmem/ops"
	"github.com/tim5wang/gmem/recall"
	"github.com/tim5wang/gmem/store"
)

// ErrNotFound 是"记录不存在"的对外错误。
var ErrNotFound = store.ErrNotFound

// HistoryReader 是**可选**的版本链端口（gmem history）。
//
// 为什么用可选接口而不是往 store.Store 里加方法：Store 是 P0 已发布的公开契约，
// 加方法会破坏所有既有实现（含 internal/vtest 的内存后端）。这与 P1 用
// store.OntologyReader、ops 用 ops.Backend 表达"可选扩展"是同一条纪律。
type HistoryReader interface {
	// MemoryHistory 返回该 id 的全部版本（按 version 升序，含已过期/已失效的旧版）。
	MemoryHistory(ctx context.Context, id string) ([]store.Memory, error)
}

// Config 是 Service 的装配参数。
type Config struct {
	// Ports 是四个存储端口（必需）。
	Ports store.Ports
	// Recaller 是召回面（必需：recall 动词与 remember 的种子）。
	Recaller *recall.Recaller
	// Maintainer 是维护面（必需：forget/history 之外的全部写侧维护动词）。
	Maintainer *ops.Maintainer
	// Extractor 是 L2 抽取器（nil → NoopExtractor：只跑 L0/L1，并显式降级）。
	Extractor extract.Extractor
	// ExtractorProvider 是**延迟**提供抽取器的钩子（优先于 Extractor）。
	//
	// # 为什么需要延迟
	//
	// MCP 的 sampling 能力只有在 initialize 之后才知道（客户端在那条报文里声明
	// capabilities.sampling）。若在构造 Service 时就把抽取器定死，客户端后来
	// 声明了 sampling 也用不上——"明明有能力却被判降级"是最容易做错的地方（D8）。
	// 因此把"现在有没有 L2"留给调用方在**每次写入时**决定。
	ExtractorProvider func() extract.Extractor
	// Embedder 是可选的查询侧向量化（nil → vector lane 优雅缺席并上报）。
	Embedder recall.Embedder
	// History 是可选版本链端口（nil → history 动词显式降级）。
	History HistoryReader
	// Now 是可注入时钟（测试用；nil → time.Now）。
	Now func() time.Time
}

// Service 是动词面的落地者。
type Service struct {
	cfg    Config
	writer *recall.Writer
	pipe   *extract.Pipeline
}

// New 构造 Service。
func New(cfg Config) (*Service, error) {
	if cfg.Ports.Store == nil {
		return nil, errors.New("memory: nil store port")
	}
	if cfg.Recaller == nil {
		return nil, errors.New("memory: nil recaller")
	}
	if cfg.Maintainer == nil {
		return nil, errors.New("memory: nil maintainer")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	s := &Service{cfg: cfg, writer: recall.NewWriter(cfg.Ports)}
	return s, nil
}

// pipeline 按**当前**生效的抽取器构造整理层管线。
//
// 每次构造而不是缓存：抽取器可能随客户端能力变化（见 ExtractorProvider 的说明）。
// NewPipeline 只做字段装配，不触达 IO，因此这个开销可以忽略；换来的是
// "能力变化后立刻生效"，不必重建 Service。
func (s *Service) pipeline() *extract.Pipeline {
	ext := s.cfg.Extractor
	if s.cfg.ExtractorProvider != nil {
		ext = s.cfg.ExtractorProvider()
	}
	return extract.NewPipeline(s.cfg.Ports, extract.Config{}, extract.PipelineOptions{
		Extractor: ext, Embedder: s.cfg.Embedder,
	})
}

// SetExtractorProvider 安装**延迟**抽取器提供者（MCP 的 sampling 兜底需要它）。
//
// 语义：每次 remember 时调用一次；返回 nil 表示"当前没有 L2 能力"（管线会
// 用 NoopExtractor 并把降级写进 Degraded）。允许在运行期改变返回值的含义——
// 这正是 sampling 能力"在 initialize 之后才可知"所要求的（D8）。
func (s *Service) SetExtractorProvider(fn func() extract.Extractor) {
	s.cfg.ExtractorProvider = fn
}

// Ports 暴露底层端口（CLI 的 doctor 需要，且避免为它再造一套注入）。
func (s *Service) Ports() store.Ports { return s.cfg.Ports }

// Recaller 暴露召回面（explain 与 graph 的种子来源）。
func (s *Service) Recaller() *recall.Recaller { return s.cfg.Recaller }

// Maintainer 暴露维护面。
func (s *Service) Maintainer() *ops.Maintainer { return s.cfg.Maintainer }

// ---------------------------------------------------------------------------
// remember
// ---------------------------------------------------------------------------

// RememberRequest 是一次写入（对齐 Mem0 的 add）。
type RememberRequest struct {
	// Content 是原文（必填）。CLI 从 -f 文件或 stdin 读入。
	Content string
	Scope   store.Scope
	// Kind 非空时走**结构化写入**：单条记忆、指定 kind，不经抽取。
	// 为空时走整理层管线（L0/L1/消歧/成图），对齐 Mem0 add 的"记住这段话"。
	Kind store.MemoryKind
	// Namespace / Role 是 episode 的元信息（可空）。
	Namespace string
	Role      string
	// At 是这次事实的世界时间（unix 秒，0 → 现在）。
	At int64
	// Actor 是审计里的操作者标识。
	Actor string
}

// RememberResult 是一次写入的结论。
type RememberResult struct {
	// EpisodeID 是事实来源（结构化写入时为空）。
	EpisodeID string
	// Writes 是实际落库的记忆（ID/版本）。
	Writes []WriteEntry
	// Reviewable 是本次新增进待审池的灰区对数（**不是合并数**）。
	Reviewable int
	// Degraded / Warnings / Rejected 全部透传管线的显式状态（不得静默）。
	Degraded []string
	Warnings []string
	Rejected []string
}

// WriteEntry 是一条落库记忆的摘要。
type WriteEntry struct {
	ID      string
	Version int
	Kind    string
	Content string
}

// Remember 写入一段原文。
//
// 两条路径都是既有实现（不新增业务逻辑），区别只在调用方是否已经知道 kind：
//   - Kind 为空：整理层管线。L0 规则 + L1 聚类 + 消歧落图，产物是多条记忆 + 节点/边。
//   - Kind 非空：结构化写入。调用方明确知道这是什么记忆，跳过抽取省一次 LLM
//     （TD §10 的 InlineGraph / ExtractPolicy=skip 同一条思路）。
func (s *Service) Remember(ctx context.Context, req RememberRequest) (RememberResult, error) {
	content := strings.TrimSpace(req.Content)
	if content == "" {
		return RememberResult{}, errors.New("memory: remember: 内容为空（用 -f <file> 或 --stdin 提供正文）")
	}
	if !req.Scope.Valid() {
		return RememberResult{}, fmt.Errorf("memory: remember: scope 必须是 user / agent / project / global 之一，收到 %q", req.Scope)
	}
	if req.At == 0 {
		req.At = s.cfg.Now().Unix()
	}
	if req.Kind != "" {
		return s.rememberStructured(ctx, req, content)
	}

	epID := "ep-" + shortHash(string(req.Scope), content)
	res, err := s.pipeline().Run(ctx, extract.RawEpisode{
		ID: epID, Scope: req.Scope, Namespace: req.Namespace,
		Role: req.Role, Content: content, At: req.At,
	})
	if err != nil {
		return RememberResult{}, fmt.Errorf("memory: remember: %w", err)
	}
	out := RememberResult{
		EpisodeID: epID, Reviewable: res.Reviewable,
		Degraded: res.Degraded, Warnings: res.Warnings,
	}
	// L0 的拒写原因走 Rejected（它不是降级，而是"这条不该进记忆"）。
	out.Rejected = append(out.Rejected, res.L0.Rejected...)
	for _, w := range res.Writes {
		out.Writes = append(out.Writes, WriteEntry{ID: w.MemoryID, Version: w.Version})
	}
	// 回填 kind/content，便于 CLI 直接展示（管线不改写内容的语义）。
	for i := range out.Writes {
		if m, err := s.cfg.Ports.Store.GetMemory(ctx, out.Writes[i].ID); err == nil {
			out.Writes[i].Kind = string(m.Kind)
			out.Writes[i].Content = m.Content
		}
	}
	return out, nil
}

// rememberStructured 是显式 kind 的单条写入（经 recall.Writer，FTS 同事务同步）。
func (s *Service) rememberStructured(ctx context.Context, req RememberRequest, content string) (RememberResult, error) {
	if !req.Kind.Valid() {
		return RememberResult{}, fmt.Errorf(
			"memory: remember: kind 必须是 semantic / episodic / procedural / decision 之一，收到 %q", req.Kind)
	}
	m := store.Memory{
		ID:       "m-" + shortHash(string(req.Scope), string(req.Kind), content),
		Scope:    req.Scope,
		Kind:     req.Kind,
		Content:  content,
		TValid:   req.At,
		TCreated: req.At,
	}
	res, err := s.writer.PutMemory(ctx, m)
	if err != nil {
		return RememberResult{}, fmt.Errorf("memory: remember: %w", err)
	}
	return RememberResult{Writes: []WriteEntry{
		{ID: res.ID, Version: res.Version, Kind: string(req.Kind), Content: content},
	}}, nil
}

// ---------------------------------------------------------------------------
// recall
// ---------------------------------------------------------------------------

// RecallRequest 是一次召回。
type RecallRequest struct {
	Query      string
	Scope      store.Scope
	Kinds      []store.MemoryKind
	AsOf       *int64
	K          int
	PerLane    int
	Lanes      []recall.LaneName
	Actor      string
	NoTouching bool
}

// Recall 走召回侧契约（align → plan → 定向召回）。
func (s *Service) Recall(ctx context.Context, req RecallRequest) (recall.RecallResult, error) {
	if strings.TrimSpace(req.Query) == "" {
		return recall.RecallResult{}, errors.New("memory: recall: query 不能为空")
	}
	if req.Scope != "" && !req.Scope.Valid() {
		return recall.RecallResult{}, fmt.Errorf(
			"memory: recall: scope 必须是 user / agent / project / global 之一，收到 %q", req.Scope)
	}
	// 空 scope 落成默认值，而不是原样传下去。
	//
	// # 为什么必须在**这里**落
	//
	// recall.Recaller 的 S-4 守卫把空 Scope 当调用方缺陷（空 ≠ 不限，否则会跨
	// scope 泄漏）。那是正确的库级约束，不应放宽。但"不传 scope"对 CLI/MCP 的
	// 使用者是完全正常的事（对齐 Mem0 的 search 只给 query），所以默认值必须由
	// **接入层之上的这个统一入口**补齐——否则 CLI、MCP、将来的其他壳都得各补
	// 一遍，漏一个就是一个“内部守卫错误”暴露给用户。
	scope := req.Scope
	if scope == "" {
		scope = store.ScopeUser
	}
	for _, k := range req.Kinds {
		if !k.Valid() {
			return recall.RecallResult{}, fmt.Errorf(
				"memory: recall: kind 必须是 semantic / episodic / procedural / decision 之一，收到 %q", k)
		}
	}
	return s.cfg.Recaller.Recall(ctx, recall.RecallRequest{
		Query: req.Query, Scope: scope, Kinds: req.Kinds, AsOf: req.AsOf,
		Budget: recall.Budget{Final: req.K, PerLane: req.PerLane},
		Lanes:  req.Lanes,
	})
}

// ---------------------------------------------------------------------------
// get / list
// ---------------------------------------------------------------------------

// Get 取一条记忆的**当前版本**。
func (s *Service) Get(ctx context.Context, id string) (store.Memory, error) {
	if strings.TrimSpace(id) == "" {
		return store.Memory{}, errors.New("memory: get: id 不能为空")
	}
	m, err := s.cfg.Ports.Store.GetMemory(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.Memory{}, fmt.Errorf("memory: get: 记忆 %q 不存在: %w", id, err)
		}
		return store.Memory{}, fmt.Errorf("memory: get %q: %w", id, err)
	}
	return m, nil
}

// ListRequest 是记忆枚举条件。
type ListRequest struct {
	Scope store.Scope
	Kinds []store.MemoryKind
	// IncludeInvalid 为 true 时连已失效的事实一起返回（默认只看当前有效）。
	IncludeInvalid bool
	Limit          int
}

// List 枚举记忆（按 t_valid 倒序，与后端一致）。
func (s *Service) List(ctx context.Context, req ListRequest) ([]store.Memory, error) {
	if req.Scope != "" && !req.Scope.Valid() {
		return nil, fmt.Errorf("memory: list: scope 必须是 user / agent / project / global 之一，收到 %q", req.Scope)
	}
	for _, k := range req.Kinds {
		if !k.Valid() {
			return nil, fmt.Errorf("memory: list: kind 非法 %q", k)
		}
	}
	return s.cfg.Ports.Store.ListMemories(ctx, store.MemoryQuery{
		Scope: req.Scope, Kinds: req.Kinds, Limit: req.Limit,
		IncludeInvalid: req.IncludeInvalid,
	})
}

// ---------------------------------------------------------------------------
// forget
// ---------------------------------------------------------------------------

// ForgetRequest 是一次删除请求。
//
// 语义沿用 TD §2：**只置 t_invalid，不物理删除**。理由是 as_of 回溯——
// 物理删掉之后就再也答不了"去年这时候它是什么"。真正的物理删除只有
// Prune（且默认只清 Archived）。因此 `forget` 的动词名对齐 Mem0 的 delete，
// 但落地是 Invalidate：这是刻意的取舍，help 与 README 里都写明。
type ForgetRequest struct {
	// ID 与 Scope 二选一：ID 删一条，Scope 删该 scope 下**当前有效**的全部。
	ID     string
	Scope  store.Scope
	Reason string
	Actor  string
	// Confirmed 必须显式为 true（CLI 由 --yes 或交互确认置位）。
	Confirmed bool
}

// ForgetResult 是一次删除的结论。
type ForgetResult struct {
	// Invalidated 是被置 t_invalid 的记忆 ID（升序）。
	Invalidated []string
	// Degraded / Warnings 透传 op 的显式状态。
	Degraded []string
	Warnings []string
}

// Forget 让一条（或一个 scope 下的一批）记忆失效。
//
// **二次确认**是这里的第一等公民（TD §9 对裁决的要求同理）：删除是记忆系统里
// 最难恢复的操作，未确认时一律拒绝并返回可读原因，绝不"默认同意"。
func (s *Service) Forget(ctx context.Context, req ForgetRequest) (ForgetResult, error) {
	if !req.Confirmed {
		return ForgetResult{}, errors.New("memory: forget: 需要二次确认（CLI 请加 -y，或在提示里输入 yes）")
	}
	if req.ID == "" && req.Scope == "" {
		return ForgetResult{}, errors.New("memory: forget: 必须给出 <id> 或 --scope（两者都没有就不知道该删什么）")
	}
	if req.ID != "" && req.Scope != "" {
		return ForgetResult{}, errors.New("memory: forget: <id> 与 --scope 只能二选一")
	}
	if req.Actor == "" {
		req.Actor = "gmem-cli"
	}

	var out ForgetResult
	if req.ID != "" {
		m, err := s.Get(ctx, req.ID)
		if err != nil {
			return ForgetResult{}, err
		}
		res, err := s.cfg.Maintainer.Supersede(ctx, ops.SupersedeRequest{
			Scope: m.Scope, MemoryID: m.ID, Reason: req.Reason, Actor: req.Actor,
		})
		if err != nil {
			return ForgetResult{}, fmt.Errorf("memory: forget %q: %w", req.ID, err)
		}
		out.Degraded = append(out.Degraded, res.Degraded...)
		out.Warnings = append(out.Warnings, res.Warnings...)
		out.Invalidated = []string{m.ID}
		return out, nil
	}

	if !req.Scope.Valid() {
		return ForgetResult{}, fmt.Errorf("memory: forget: scope 必须是 user / agent / project / global 之一，收到 %q", req.Scope)
	}
	mems, err := s.cfg.Ports.Store.ListMemories(ctx, store.MemoryQuery{Scope: req.Scope})
	if err != nil {
		return ForgetResult{}, fmt.Errorf("memory: forget: 枚举 scope %q 的记忆: %w", req.Scope, err)
	}
	for _, m := range mems {
		res, err := s.cfg.Maintainer.Supersede(ctx, ops.SupersedeRequest{
			Scope: m.Scope, MemoryID: m.ID,
			Reason: reasonOr(req.Reason, fmt.Sprintf("按 scope=%s 批量失效", req.Scope)),
			Actor:  req.Actor,
		})
		if err != nil {
			// 单条失败不掩盖已成功的事实：把已完成的部分一起返回（调用方要能核对）。
			return out, fmt.Errorf("memory: forget %q: %w（已完成 %d 条）", m.ID, err, len(out.Invalidated))
		}
		out.Degraded = append(out.Degraded, res.Degraded...)
		out.Warnings = append(out.Warnings, res.Warnings...)
		out.Invalidated = append(out.Invalidated, m.ID)
	}
	sort.Strings(out.Invalidated)
	if len(out.Invalidated) == 0 {
		// 没有可删的 ≠ 出错，但必须说清楚（否则看起来像"删成功了"）。
		out.Warnings = append(out.Warnings,
			fmt.Sprintf("scope %q 下没有当前有效的记忆，本次没有产生任何变更", req.Scope))
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// history
// ---------------------------------------------------------------------------

// HistoryEntry 是一个版本（before/after 版本链的一环）。
type HistoryEntry struct {
	Version   int
	Content   string
	Kind      string
	Scope     string
	TValid    int64
	TInvalid  int64
	TCreated  int64
	TExpired  int64
	IsCurrent bool
}

// HistoryResult 是一条记忆的版本链。
type HistoryResult struct {
	ID string
	// Entries 按 version 升序：链的第一环是初版，最后环比上一环是 before→after。
	Entries []HistoryEntry
	// Degraded 显式记录"后端未提供版本链端口"这类降级（不得静默）。
	Degraded []string
}

// History 取一条记忆的版本链（before/after）。
func (s *Service) History(ctx context.Context, id string) (HistoryResult, error) {
	if strings.TrimSpace(id) == "" {
		return HistoryResult{}, errors.New("memory: history: id 不能为空")
	}
	if s.cfg.History == nil {
		// 显式降级：不假装"没有历史"，而是说清楚为什么拿不到。
		return HistoryResult{ID: id, Degraded: []string{
			"当前后端未提供版本链端口（HistoryReader）：history 只反映当前版本",
		}}, nil
	}
	versions, err := s.cfg.History.MemoryHistory(ctx, id)
	if err != nil {
		return HistoryResult{}, fmt.Errorf("memory: history %q: %w", id, err)
	}
	if len(versions) == 0 {
		return HistoryResult{}, fmt.Errorf("memory: history: 记忆 %q 不存在: %w", id, store.ErrNotFound)
	}
	out := HistoryResult{ID: id}
	for _, m := range versions {
		out.Entries = append(out.Entries, HistoryEntry{
			Version: m.Version, Content: m.Content, Kind: string(m.Kind), Scope: string(m.Scope),
			TValid: m.TValid, TInvalid: m.TInvalid, TCreated: m.TCreated, TExpired: m.TExpired,
			IsCurrent: m.TExpired == 0,
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 维护面（与自动触发共用 ops.Maintainer，D3）
// ---------------------------------------------------------------------------

// Consolidate 手动触发整理。
func (s *Service) Consolidate(ctx context.Context, scope store.Scope, limit int, actor string) (ops.Result, error) {
	if !scope.Valid() {
		return ops.Result{}, fmt.Errorf("memory: consolidate: scope 必须是 user / agent / project / global 之一，收到 %q", scope)
	}
	return s.cfg.Maintainer.Consolidate(ctx, ops.ConsolidateRequest{
		Scope: scope, Limit: limit, Actor: actorOr(actor),
	})
}

// ReviewSimilar 列疑似重复的实体对（不写库）。
func (s *Service) ReviewSimilar(ctx context.Context, scope store.Scope, limit int, actor string) (ops.ReviewResult, error) {
	if scope != "" && !scope.Valid() {
		return ops.ReviewResult{}, fmt.Errorf("memory: review_similar: scope 非法 %q", scope)
	}
	return s.cfg.Maintainer.ReviewSimilar(ctx, ops.ReviewRequest{
		Scope: scope, Limit: limit, Actor: actorOr(actor),
	})
}

// Merge 把 From 并入 To（可回溯的重定向）。
func (s *Service) Merge(ctx context.Context, scope store.Scope, from, to, reason, actor string) (ops.Result, error) {
	if from == "" || to == "" {
		return ops.Result{}, errors.New("memory: merge: from / to 都必填")
	}
	if from == to {
		return ops.Result{}, fmt.Errorf("memory: merge: from 与 to 不能相同（%q）", from)
	}
	return s.cfg.Maintainer.Merge(ctx, ops.MergeRequest{
		Scope: scope, FromNode: from, ToNode: to, Reason: reason, Actor: actorOr(actor),
	})
}

// Link 写入或失效一条边。
func (s *Service) Link(ctx context.Context, scope store.Scope, from, to string,
	rel store.Relation, weight float64, remove bool, reason, actor string) (ops.Result, error) {
	if from == "" || to == "" {
		return ops.Result{}, errors.New("memory: link: from / to 都必填")
	}
	if rel == "" {
		return ops.Result{}, errors.New("memory: link: relation 必填（例：works_at / part_of / uses）")
	}
	return s.cfg.Maintainer.Link(ctx, ops.LinkRequest{
		Scope: scope, FromNode: from, ToNode: to, Relation: rel,
		Weight: weight, Remove: remove, Reason: reason, Actor: actorOr(actor),
	})
}

// Supersede 宣布一条记忆作废（只置 t_invalid）。
func (s *Service) Supersede(ctx context.Context, scope store.Scope, id string, tInvalid int64,
	reason, actor string) (ops.Result, error) {
	if id == "" {
		return ops.Result{}, errors.New("memory: supersede: id 必填")
	}
	return s.cfg.Maintainer.Supersede(ctx, ops.SupersedeRequest{
		Scope: scope, MemoryID: id, TInvalid: tInvalid, Reason: reason, Actor: actorOr(actor),
	})
}

// Promote 把一个 draft 概念提升为 certified。
func (s *Service) Promote(ctx context.Context, scope store.Scope, conceptID, actor string) (ops.Result, error) {
	if conceptID == "" {
		return ops.Result{}, errors.New("memory: promote_concept: concept_id 必填")
	}
	return s.cfg.Maintainer.PromoteConcept(ctx, ops.PromoteRequest{
		Scope: scope, ConceptIDs: []string{conceptID}, Actor: actorOr(actor),
	})
}

// ---------------------------------------------------------------------------
// 小事
// ---------------------------------------------------------------------------

func actorOr(s string) string {
	if strings.TrimSpace(s) == "" {
		return "gmem-cli"
	}
	return s
}

func reasonOr(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// shortHash 是 8 位十六进制摘要（与 extract 的同名实现同口径，避免循环依赖）。
func shortHash(parts ...string) string {
	const mod = uint64(1099511628211)
	const off = uint64(14695981039346656037)
	h := off
	for _, p := range parts {
		for i := 0; i < len(p); i++ {
			h ^= uint64(p[i])
			h *= mod
		}
		h ^= 0
		h *= mod
	}
	const hex = "0123456789abcdef"
	buf := make([]byte, 8)
	for i := 0; i < 8; i++ {
		buf[7-i] = hex[h&0xf]
		h >>= 4
	}
	return string(buf)
}
