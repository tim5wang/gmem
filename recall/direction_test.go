package recall_test

import (
	"context"
	"testing"

	"github.com/tim5wang/gmem/recall"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// F-8 的召回侧护栏：graph lane 必须仍然是**定向**的（outbound）
//
// F-8 修复给 store.NeighborQuery 加了显式 Direction（零值 = outbound），
// 可视化侧改成 both。这条用例钉死召回侧**没有**被顺带改成双向：
//
//	图：n-hidden --uses--> n-alice --uses--> n-vim
//	m-hidden  <- concept c-hidden <- n-hidden
//
// 问句"Alice 用什么 编辑器"的锚点是 n-alice（只有它有别名）。
// 若 graph lane 变成双向（或零值语义被改成 both），它就会从 n-alice 反向走到
// n-hidden，把 m-hidden 也召回来——而"谁在用 Alice"根本不是这个问题在问什么。
// 这种变化不会报错，只会让召回突然多出一堆反向噪声，因此必须用用例拦住。
// ===========================================================================

func TestRecallGraphLaneStaysOutbound(t *testing.T) {
	d := openDB(t)
	ctx := context.Background()
	seedOntology(t, d) // n-alice / n-vim / e-alice-vim(uses) / c-editor / alias Alice

	// 反向邻居：n-hidden --uses--> n-alice，概念 c-hidden 挂着一条记忆。
	if err := d.Store().PutConcept(ctx, store.Concept{
		ID: "c-hidden", Name: "点文件", Status: store.ConceptCertified,
		Definition: "存放编辑器配置的仓库",
	}); err != nil {
		t.Fatalf("PutConcept(c-hidden): %v", err)
	}
	if err := d.Graph().PutNode(ctx, store.Node{
		ID: "n-hidden", Scope: store.ScopeUser, CanonicalName: "dotfiles", ConceptID: "c-hidden",
	}); err != nil {
		t.Fatalf("PutNode(n-hidden): %v", err)
	}
	if err := d.Graph().PutEdge(ctx, store.Edge{
		ID: "e-hidden-alice", FromID: "n-hidden", ToID: "n-alice",
		Relation: store.RelationUses, Weight: 1, TValid: 1000, TCreated: 1000,
	}); err != nil {
		t.Fatalf("PutEdge(e-hidden-alice): %v", err)
	}
	// 文本刻意不含问句里的任何词：这样它只可能经**图通道的反向边**被召回。
	if _, err := recall.NewWriter(d.Ports()).PutMemory(ctx, store.Memory{
		ID: "m-hidden", Scope: store.ScopeUser, Kind: store.KindSemantic,
		ConceptID: "c-hidden", Content: "dotfiles 放在 ~/src", TValid: 1000, TCreated: 1000,
	}); err != nil {
		t.Fatalf("PutMemory(m-hidden): %v", err)
	}
	if _, err := recall.NewWriter(d.Ports()).PutMemory(ctx, store.Memory{
		ID: "m-editor", Scope: store.ScopeUser, Kind: store.KindSemantic,
		ConceptID: "c-editor", Content: "Alice 的默认编辑器是 vim", TValid: 1000, TCreated: 1000,
	}); err != nil {
		t.Fatalf("PutMemory(m-editor): %v", err)
	}

	r := recall.New(d.Ports(), recall.Config{}, nil)
	res, err := r.Recall(ctx, recall.RecallRequest{Query: "Alice 用什么 编辑器", Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}

	var gotIDs []string
	for _, it := range res.Items {
		gotIDs = append(gotIDs, it.Memory.ID)
	}
	// 正向仍然要能答出来（修 F-8 不得顺手把召回弄坏）。
	var forward bool
	for _, id := range gotIDs {
		if id == "m-editor" {
			forward = true
		}
	}
	if !forward {
		t.Fatalf("定向召回退化了：正向关系（n-alice -uses-> n-vim）没召回 m-editor，items=%v", gotIDs)
	}
	// 反向不得被召回：graph lane 必须仍是 outbound，不得被 F-8 的 both 带跑。
	for _, id := range gotIDs {
		if id == "m-hidden" {
			t.Fatalf("graph lane 被静默改成双向了：反向邻居 n-hidden 的记忆 m-hidden 出现在结果里（items=%v）", gotIDs)
		}
	}
}
