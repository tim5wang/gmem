package ops

import (
	"context"
	"testing"

	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// F-5 的单元级回归：pruneMemory 的"二次防线"必须真校验 opt-in
//
// 为什么必须放在包内（package ops）：
// 这条防线只在"Step 被持久化后重放"时才会被命中——Plan 自己不会产出
// OptIn=false 的 Invalidated 步骤。从包外无法构造这种 Step，因此只能直接
// 调用 pruneMemory。用假 TxContext + 假 Backend 记录"是否真的删了"。
// ===========================================================================

// recordingBackend 只实现 pruneMemory 会用到的那两个方法，其余嵌入接口（nil）。
type recordingBackend struct {
	Backend
	deletedMemories int
	deletedStates   int
}

func (b *recordingBackend) DeleteMemoriesPermanently(ctx context.Context, ids []string) (int, error) {
	b.deletedMemories += len(ids)
	return len(ids), nil
}

func (b *recordingBackend) DeleteStates(ctx context.Context, ids []string) error {
	b.deletedStates += len(ids)
	return nil
}

// pruneTxContext 是最小 TxContext（只满足 pruneMemory 的读取需求）。
type pruneTxContext struct {
	TxContext
	be    Backend
	ports store.Ports
}

func (t *pruneTxContext) Context() context.Context { return context.Background() }
func (t *pruneTxContext) Backend() Backend         { return t.be }
func (t *pruneTxContext) Ports() store.Ports       { return t.ports }
func (t *pruneTxContext) Check() error             { return nil }

// TestPruneMemoryRejectsInvalidatedWithoutOptIn 是 F-5 的回归。
//
// 变异点：把守卫改回 `if v.Status != Archived && v.Status != Invalidated`（无条件放行）→
// Invalidated 会被真的删掉，deletedMemories 变成 1，断言必红。
func TestPruneMemoryRejectsInvalidatedWithoutOptIn(t *testing.T) {
	be := &recordingBackend{}
	txc := &pruneTxContext{be: be}

	// 未 opt-in 的 Invalidated：必须跳过（且原因可见）。
	d, err := pruneMemory(txc, pruneMemoryStep{
		MemoryID: "m-invalid", Status: StatusInvalidated, Versions: []int{1}, OptIn: false,
	})
	if err != nil {
		t.Fatalf("pruneMemory: %v", err)
	}
	if d.Changed {
		t.Errorf("未 opt-in 的 Invalidated 被报告为已变更（二次防线失效）")
	}
	if be.deletedMemories != 0 || be.deletedStates != 0 {
		t.Errorf("未 opt-in 却真的删了：memories=%d states=%d（F-5：无效的二次防线）",
			be.deletedMemories, be.deletedStates)
	}
	if len(d.Warnings) == 0 {
		t.Errorf("跳过必须上报 Warnings（不得静默）：%+v", d)
	}

	// 显式 opt-in 的 Invalidated：允许删除（证明守卫是"校验"而不是"一律拒绝"）。
	d2, err := pruneMemory(txc, pruneMemoryStep{
		MemoryID: "m-invalid2", Status: StatusInvalidated, Versions: []int{1}, OptIn: true,
	})
	if err != nil {
		t.Fatalf("pruneMemory(opt-in): %v", err)
	}
	if !d2.Changed {
		t.Errorf("显式 opt-in 的 Invalidated 未被删除（守卫把合法路径也挡了）")
	}
	if be.deletedMemories != 1 {
		t.Errorf("opt-in 后删除条数=%d, want 1", be.deletedMemories)
	}
}

// TestPruneMemoryRejectsNonPrunableStatus 顺带钉死 Active/Candidate 等一律不动。
func TestPruneMemoryRejectsNonPrunableStatus(t *testing.T) {
	for _, st := range []MemoryStatus{
		StatusActive, StatusCandidate, StatusStale, StatusRejected,
	} {
		be := &recordingBackend{}
		txc := &pruneTxContext{be: be}
		d, err := pruneMemory(txc, pruneMemoryStep{
			MemoryID: "m-" + string(st), Status: st, Versions: []int{1}, OptIn: true,
		})
		if err != nil {
			t.Fatalf("pruneMemory(%s): %v", st, err)
		}
		if d.Changed || be.deletedMemories != 0 {
			t.Errorf("状态 %s 被 Prune 物理删除（TD §7 纪律 3：默认只作用于 Archived）", st)
		}
	}
}
