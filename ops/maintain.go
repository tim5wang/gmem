package ops

import (
	"context"
	"fmt"

	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// Agent 维护面（TD §8「Agent 可调用的维护工具面」）
//
// # 关于"暴露给 recall 侧"
//
// TD §10 把维护面定义成一个 Maintainer 接口，与 Memory（五动词）并列。P3 的落点就是
// 这个 Maintainer：ReviewSimilar / Merge / Link / Supersede / Consolidate /
// PromoteConcept 六个方法。
//
// 为什么它住在 ops 而不是 recall：ops 依赖 recall（用 recall.Writer 写入），
// 反向依赖会成环。调用方（Go package / CLI / MCP）把 maintainer 与 recaller 并排持有，
// 这正是 TD §1「三个接口面，同一实现」的形状。
//
// 六个方法**全部**经由 Runner.Run → 同一个 Op 实例，因此 Agent 显式调用与自动触发
// 的行为不可能分叉（D3）。它们只负责构造 Input 与审计元数据。
// ===========================================================================

// Maintainer 是 Agent 维护面。
type Maintainer struct{ runner *Runner }

// NewMaintainer 构造维护面（与自动触发共用同一个 Runner）。
func NewMaintainer(r *Runner) *Maintainer { return &Maintainer{runner: r} }

// Runner 暴露底层执行器（调用方需要审计/配置时用）。
func (m *Maintainer) Runner() *Runner { return m.runner }

// ReviewRequest 是 ReviewSimilar 的请求。
type ReviewRequest struct {
	Scope store.Scope
	// Pairs 为空时枚举待审灰区（possiblySame）。
	Pairs []PairRef
	Limit int
	Actor string
}

// ReviewResult 是 ReviewSimilar 的结论。
//
// Similar 直接来自 Op 的 Plan：**看到的就是会执行的**（Plan 与 Apply 共用同一个实现）。
type ReviewResult struct {
	Similar []PairView
	Result  Result
}

// PairView 是一对疑似重复的可读视图。
type PairView struct {
	LeftNode  string
	RightNode string
	StepID    string
	Note      string
}

// ReviewSimilar 列出疑似重复的实体对（`gmem_review_similar`）。
func (m *Maintainer) ReviewSimilar(ctx context.Context, req ReviewRequest) (ReviewResult, error) {
	run, err := m.runner.Run(ctx, Request{
		Op: OpDisambiguate, Scope: req.Scope,
		Trigger: Trigger{Kind: TriggerAgent, Actor: actorOf(req.Actor), Reason: "Agent 主动查疑似重复"},
		Input:   Input{Pairs: req.Pairs, Limit: req.Limit},
	})
	if err != nil {
		return ReviewResult{}, err
	}
	var out ReviewResult
	out.Result = run
	for _, s := range run.Steps {
		if s.Step.Kind != "review_pair" {
			continue
		}
		p, ok := s.Step.Data.(PairRef)
		if !ok {
			continue
		}
		out.Similar = append(out.Similar, PairView{
			LeftNode: p.LeftNode, RightNode: p.RightNode, StepID: s.Step.ID, Note: s.Diff.Summary,
		})
	}
	return out, nil
}

// MergeRequest 是 Merge 的请求。
type MergeRequest struct {
	Scope    store.Scope
	FromNode string
	ToNode   string
	Reason   string
	Actor    string
}

// Merge 把 FromNode 并入 ToNode（`gmem_merge`）。
//
// 边与 alias 都会迁移，旧 ID **保留为重定向**（TD §4）——合并是可回溯的，
// 不是"两条事实混一起、事后无法拆开"。
func (m *Maintainer) Merge(ctx context.Context, req MergeRequest) (Result, error) {
	return m.runner.Run(ctx, Request{
		Op: OpMerge, Scope: req.Scope,
		Trigger: Trigger{Kind: TriggerAgent, Actor: actorOf(req.Actor), Reason: reasonOf(req.Reason, "Agent 确认两实体同一")},
		Input:   Input{Merge: &MergeInput{FromNode: req.FromNode, ToNode: req.ToNode, Reason: req.Reason}},
	})
}

// LinkRequest 是 Link 的请求。
type LinkRequest struct {
	Scope    store.Scope
	FromNode string
	ToNode   string
	Relation store.Relation
	Weight   float64
	// Remove 为 true 等价于 unlink。
	Remove bool
	Actor  string
	Reason string
}

// Link 写入或失效一条边（`gmem_link` / `gmem_unlink`）。
func (m *Maintainer) Link(ctx context.Context, req LinkRequest) (Result, error) {
	return m.runner.Run(ctx, Request{
		Op: OpLinkUpdate, Scope: req.Scope,
		Trigger: Trigger{Kind: TriggerAgent, Actor: actorOf(req.Actor), Reason: reasonOf(req.Reason, "Agent 明确知道两个概念的关系")},
		Input: Input{Link: &LinkInput{
			FromNode: req.FromNode, ToNode: req.ToNode, Relation: req.Relation,
			Weight: req.Weight, Remove: req.Remove,
		}},
	})
}

// SupersedeRequest 是 Supersede 的请求。
type SupersedeRequest struct {
	Scope    store.Scope
	MemoryID string
	TInvalid int64
	Reason   string
	Actor    string
}

// Supersede 宣布旧事实作废（`gmem_supersede`）。
//
// 语义是 Invalidate：只置 t_invalid，**不删除**，as_of 仍能回溯到它。
func (m *Maintainer) Supersede(ctx context.Context, req SupersedeRequest) (Result, error) {
	return m.runner.Run(ctx, Request{
		Op: OpInvalidate, Scope: req.Scope,
		Trigger: Trigger{Kind: TriggerAgent, Actor: actorOf(req.Actor), Reason: reasonOf(req.Reason, "Agent 主动宣布旧事实作废")},
		Input:   Input{Invalidate: &InvalidateInput{MemoryID: req.MemoryID, TInvalid: req.TInvalid, Reason: req.Reason}},
	})
}

// ConsolidateRequest 是 Consolidate 的请求。
type ConsolidateRequest struct {
	Scope store.Scope
	Limit int
	Actor string
}

// Consolidate 整理这个 scope（`gmem_consolidate`）：推进状态机、报告孤立节点。
func (m *Maintainer) Consolidate(ctx context.Context, req ConsolidateRequest) (Result, error) {
	res, err := m.runner.Run(ctx, Request{
		Op: OpConsolidate, Scope: req.Scope,
		Trigger: Trigger{Kind: TriggerAgent, Actor: actorOf(req.Actor), Reason: "Agent 被要求整理这个 scope"},
		Input:   Input{Limit: req.Limit},
	})
	if err == nil {
		// 水位推进：阈值触发靠它算"未整理条目"。
		m.advanceWatermark(ctx, req.Scope)
	}
	return res, err
}

// PromoteRequest 是 PromoteConcept 的请求。
type PromoteRequest struct {
	Scope      store.Scope
	ConceptIDs []string
	All        bool
	MinUse     int
	Actor      string
	Reason     string
}

// PromoteConcept 把 draft 概念认证为 certified（`gmem_promote_concept`）。
//
// 这是 D15 的第一条清理路径：认证后该概念**开始参与硬契约**（召回侧 R2），
// 因此它是"人的决定"，不是自动行为（All 模式也只覆盖"已在被使用"的草稿）。
func (m *Maintainer) PromoteConcept(ctx context.Context, req PromoteRequest) (Result, error) {
	return m.runner.Run(ctx, Request{
		Op: OpPromote, Scope: req.Scope,
		Trigger: Trigger{Kind: TriggerAgent, Actor: actorOf(req.Actor), Reason: reasonOf(req.Reason, "Agent 把 draft 概念认证为 certified")},
		Input: Input{Promote: &PromoteInput{
			ConceptIDs: req.ConceptIDs, All: req.All, MinUse: req.MinUse,
		}},
	})
}

// PruneRequest 是 Prune 的请求。
//
// Prune 在 TD §8 的工具表里**不暴露给 Agent**（"危险"）。这里仍然提供 Go 方法，
// 因为宿主可能需要在维护窗口里显式调用它；但不提供 MCP 工具（P5 的边界）。
type PruneRequest struct {
	Scope              store.Scope
	Limit              int
	IncludeInvalidated bool
	IncludeDrafts      *bool
	DraftTTLSeconds    int64
	Actor              string
	Reason             string
}

// Prune 物理删除 Archived 记忆与长期未用的 draft 概念（D15 的第二条清理路径）。
func (m *Maintainer) Prune(ctx context.Context, req PruneRequest) (Result, error) {
	if req.Actor == "" {
		req.Actor = "maintenance"
	}
	return m.runner.Run(ctx, Request{
		Op: OpPrune, Scope: req.Scope,
		Trigger: Trigger{Kind: TriggerAgent, Actor: req.Actor, Reason: reasonOf(req.Reason, "显式维护窗口：清理 Archived 与长期未用 draft")},
		Input: Input{
			Limit: req.Limit, IncludeInvalidated: req.IncludeInvalidated,
			IncludeDrafts: req.IncludeDrafts, DraftTTLSeconds: req.DraftTTLSeconds,
		},
	})
}

// advanceWatermark 把 Consolidate 的水位推到当前时刻（best effort：失败不影响主结论，
// 代价只是阈值触发会稍微早一点）。
func (m *Maintainer) advanceWatermark(ctx context.Context, scope store.Scope) {
	r := m.runner
	now := r.cfg.Now().UnixMilli()
	_ = r.ports.Store.Tx(ctx, func(tctx context.Context, _ store.Store) error {
		return r.backend.PutWatermark(tctx, scope, OpConsolidate, now)
	})
}

func actorOf(a string) string {
	if a == "" {
		return "agent"
	}
	return a
}

func reasonOf(given, fallback string) string {
	if given != "" {
		return given
	}
	return fallback
}

// Audit 是审计检索的便捷入口（"谁在何时因何触发了什么变更"）。
//
// 它是 P4 的 History API 与 Graph View 的数据来源，因此 P3 就把它做成公开方法，
// 而不是留给上层自己拼 SQL。
func (m *Maintainer) Audit(ctx context.Context, q AuditQuery) ([]AuditEntry, error) {
	var out []AuditEntry
	err := m.runner.ports.Store.Tx(ctx, func(tctx context.Context, _ store.Store) error {
		list, err := m.runner.backend.ListAudit(tctx, q)
		if err != nil {
			return err
		}
		out = list
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("ops: 审计检索失败: %w", err)
	}
	return out, nil
}

// Runs 是 op 台账检索的便捷入口。
func (m *Maintainer) Runs(ctx context.Context, q RunQuery) ([]RunRecord, error) {
	var out []RunRecord
	err := m.runner.ports.Store.Tx(ctx, func(tctx context.Context, _ store.Store) error {
		list, err := m.runner.backend.ListRuns(tctx, q)
		if err != nil {
			return err
		}
		out = list
		return nil
	})
	return out, err
}
