package ops

import (
	"fmt"
	"time"

	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// Prune：唯一的物理删除路径（TD §7 纪律 3）+ D15 的 draft 清理
//
// 两条硬约束：
//  1. **默认只作用于 Archived**。Active / Invalidated / Stale / Candidate / Rejected
//     一律不动：Invalidated 还要能 as_of 回溯，删了就再也答不了"去年这时候是什么"。
//  2. draft 概念必须可清理（D15）。写入时未知 concept 会自动落成 draft，
//     若没有 Promote/Prune，草稿表会被 agent 的随意输入填满，本体漂移无界。
// ===========================================================================

type pruneOp struct{}

func (pruneOp) Name() OpName   { return OpPrune }
func (pruneOp) Budget() Budget { return Budget{Steps: 500, IO: 5000, Duration: 30 * time.Second} }

// pruneMemoryStep / pruneDraftStep 是两类可清理对象。
type pruneMemoryStep struct {
	MemoryID string
	Status   MemoryStatus
	Versions []int
	// OptIn 记录“本次运行是否显式 opt-in 了 IncludeInvalidated”（F-5）。
	//
	// 为什么要把它带进 Step：Apply 必须能独立复核“删 Invalidated 是不是被允许的”，
	// 而不是默信任 Plan（Step 可能被持久化后重放）。
	OptIn bool
}

type pruneDraftStep struct{ Concept store.Concept }

func (pruneOp) Plan(txc TxContext) ([]Step, error) {
	ctx := txc.Context()
	cfg := txc.Cfg()
	in := txc.Input()
	limit := in.Limit
	if limit <= 0 {
		limit = cfg.Limit
	}
	now := txc.Now()
	var steps []Step

	// ---- 1) 记忆：默认只清 Archived ----
	statuses := []MemoryStatus{StatusArchived}
	if in.IncludeInvalidated {
		// 显式 opt-in 才允许清 Invalidated（仍走同一条审计与物理删除路径）。
		statuses = append(statuses, StatusInvalidated)
	}
	states, err := txc.Backend().ListStates(ctx, txc.Scope(), statuses, limit)
	if err != nil {
		return nil, err
	}
	for _, st := range states {
		if err := txc.Check(); err != nil {
			return nil, err
		}
		versions, err := txc.Backend().MemoryVersions(ctx, st.MemoryID)
		if err != nil {
			return nil, err
		}
		steps = append(steps, Step{
			ID: "prune:" + st.MemoryID, Kind: "prune_memory", Subject: st.MemoryID,
			Data: pruneMemoryStep{MemoryID: st.MemoryID, Status: st.Status, Versions: versions,
				OptIn: in.IncludeInvalidated},
			Note: "物理删除（唯一删除路径）：全部版本 + 状态行 + FTS/向量索引",
		})
	}

	// ---- 2) D15：长期未用的 draft 概念 ----
	if in.IncludeDraftsDefault() {
		ttl := cfg.DraftTTL
		if in.DraftTTLSeconds > 0 {
			ttl = time.Duration(in.DraftTTLSeconds) * time.Second
		}
		before := now.Add(-ttl).UnixMilli()
		// scope 是必填的隔离边界（F-4）：没有它，agent scope 的清理会删掉 user scope 的草稿。
		drafts, err := txc.Backend().ListStaleDrafts(ctx, txc.Scope(), before, limit)
		if err != nil {
			return nil, err
		}
		for _, c := range drafts {
			if err := txc.Check(); err != nil {
				return nil, err
			}
			steps = append(steps, Step{
				ID: "prune_draft:" + c.ID, Kind: "prune_draft_concept", Subject: c.ID,
				Data: pruneDraftStep{Concept: c},
				Note: fmt.Sprintf("删除长期未用 draft（TTL=%s，且无任何引用）：本体漂移不会无界", ttl),
			})
		}
	}
	return steps, nil
}

func (pruneOp) Apply(txc TxContext, s Step) (Diff, error) {
	switch v := s.Data.(type) {
	case pruneMemoryStep:
		return pruneMemory(txc, v)
	case pruneDraftStep:
		return pruneDraft(txc, v)
	}
	return Diff{}, fmt.Errorf("ops: prune: 未识别的步骤载荷 %T", s.Data)
}

// pruneMemory 物理删除一条记忆的全部版本与派生索引。
//
// 顺序是刻意的：先删派生索引（FTS 按 (id, version) 精确删，D13），再删主表。
// 反过来的话，中间失败会留下"索引有行、主表无行"——召回侧只会静默 drop，
// 没人会发现。
func pruneMemory(txc TxContext, v pruneMemoryStep) (Diff, error) {
	ctx := txc.Context()
	// Prune 只对 Archived（或**显式 opt-in** 的 Invalidated）生效；这里再挡一次，
	// 因为 Step 可能被持久化后重放，不能只信 Plan。
	//
	// F-5：旧实现只判断"状态属于 {Archived, Invalidated}"，于是 Invalidated
	// **无条件放行**——这条"二次防线"到底没挡任何东西。现在 Invalidated 必须
	// 同时带上本次运行的 opt-in 标记，否则跳过并记 Warnings（不得静默）。
	if v.Status != StatusArchived {
		if v.Status != StatusInvalidated {
			return Diff{Changed: false,
				Summary:  fmt.Sprintf("跳过 %s：状态 %s 不是 Archived（Prune 默认只作用于 Archived）", v.MemoryID, v.Status),
				Warnings: []string{fmt.Sprintf("拒绝物理删除 %s（状态 %s）", v.MemoryID, v.Status)},
			}, nil
		}
		if !v.OptIn {
			return Diff{Changed: false,
				Summary: fmt.Sprintf("跳过 %s：状态为 Invalidated 但本次未 IncludeInvalidated（opt-in）", v.MemoryID),
				Warnings: []string{fmt.Sprintf(
					"拒绝物理删除 %s：Invalidated 默认保留（TD §7 纪律 1，as_of 仍可回溯）；需要显式 IncludeInvalidated", v.MemoryID)},
			}, nil
		}
	}

	before := marshalJSON(map[string]any{"memory": v.MemoryID, "status": string(v.Status), "versions": v.Versions})

	if txc.Ports().Lexical != nil {
		keys := make([]store.DocumentKey, 0, len(v.Versions))
		for _, ver := range v.Versions {
			keys = append(keys, store.DocumentKey{ID: v.MemoryID, Version: ver})
		}
		if len(keys) > 0 {
			if err := txc.Ports().Lexical.Delete(ctx, keys); err != nil {
				return Diff{}, fmt.Errorf("ops: prune: 删 FTS %q: %w", v.MemoryID, err)
			}
		}
	}
	if txc.Ports().Vector != nil {
		if err := txc.Ports().Vector.Delete(ctx, []string{v.MemoryID}); err != nil {
			// 向量是派生索引（可从主表重建）：删不掉只上报，不阻断物理删除。
			// 但必须可见——静默跳过会让"重建快照"反复带回幽灵向量。
			return Diff{}, fmt.Errorf("ops: prune: 删向量 %q: %w", v.MemoryID, err)
		}
	}
	n, err := txc.Backend().DeleteMemoriesPermanently(ctx, []string{v.MemoryID})
	if err != nil {
		return Diff{}, err
	}
	if err := txc.Backend().DeleteStates(ctx, []string{v.MemoryID}); err != nil {
		return Diff{}, err
	}
	return Diff{
		Changed:  n > 0,
		Summary:  fmt.Sprintf("物理删除记忆 %s（%d 行，状态 %s，索引已同步清理）", v.MemoryID, n, v.Status),
		Before:   before,
		After:    marshalJSON(map[string]any{"memory": v.MemoryID, "deleted_rows": n}),
		Subjects: []string{v.MemoryID},
	}, nil
}

// pruneDraft 删除一个长期未用的 draft 概念（D15 的清理路径）。
//
// 仍被引用的概念**不得删除**：删掉会让 memories.concept_id 变成悬空引用，
// 召回侧的对齐信息与 explain 会静默变差。因此有引用时跳过并记 Warnings。
func pruneDraft(txc TxContext, v pruneDraftStep) (Diff, error) {
	ctx := txc.Context()
	c := v.Concept
	if c.Status != store.ConceptDraft {
		return Diff{Changed: false,
			Summary:  fmt.Sprintf("跳过概念 %s：状态 %s 不是 draft", c.ID, c.Status),
			Warnings: []string{fmt.Sprintf("拒绝删除概念 %s（状态 %s，只有 draft 可被 Prune）", c.ID, c.Status)},
		}, nil
	}
	refs, err := txc.Backend().CountConceptRefs(ctx, c.ID)
	if err != nil {
		return Diff{}, err
	}
	before := marshalJSON(map[string]any{
		"concept": c.ID, "status": string(c.Status), "origin": string(c.Origin), "refs": refs,
	})
	if refs > 0 {
		return Diff{
			Changed: false,
			Summary: fmt.Sprintf("跳过 draft %s：仍被 %d 条记录引用", c.ID, refs),
			Before:  before,
			Warnings: []string{fmt.Sprintf(
				"draft 概念 %s 仍有 %d 条引用，未删除（删掉会留下悬空引用）", c.ID, refs)},
			Subjects: []string{c.ID},
		}, nil
	}
	if err := txc.Backend().DeleteConcept(ctx, c.ID); err != nil {
		return Diff{}, err
	}
	return Diff{
		Changed:  true,
		Summary:  fmt.Sprintf("删除长期未用 draft 概念 %s（origin=%s，无引用）", c.ID, c.Origin),
		Before:   before,
		After:    marshalJSON(map[string]any{"concept": c.ID, "deleted": true}),
		Subjects: []string{c.ID},
	}, nil
}
