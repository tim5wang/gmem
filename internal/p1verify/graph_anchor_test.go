package p1verify

import (
	"context"
	"testing"

	"github.com/tim5wang/gmem/recall"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// 多锚点：graph lane 必须遍历**全部**锚点，而不是只取第一个
// ===========================================================================

// TestGraphLaneTraversesAllAnchors 钉住"每个锚点都要遍历"。
//
// 为什么要单独一条：单锚点用例对"只取第一个锚点"这类变异是**不敏感**的
// （变异 M18 在只有 n-alice 一个锚点的用例上保持绿色）。真实问句经常一次带多个实体，
// 漏掉第二个锚点不会报错，只会少召回——正是最难发现的那类缺陷。
//
// 变异验证：把 lanes.go 的 range in.align.Anchors 改成 range in.align.Anchors[:1]
// → 本用例红（m-emacs 消失）。
func TestGraphLaneTraversesAllAnchors(t *testing.T) {
	d := openMemory(t)
	ctx := context.Background()
	// 两个锚点各自一个独立概念 → 节点到记忆是 1:1 映射，多锚点效应不被扇出掩盖。
	putConcept(t, d.Store(), "c-vim", "vim工具", store.ConceptCertified, "一种模态文本编辑器")
	putConcept(t, d.Store(), "c-emacs", "emacs工具", store.ConceptCertified, "一种可扩展文本编辑器")
	putNode(t, d.Graph(), "n-alice", "Alice", store.ScopeUser, "", "")
	putNode(t, d.Graph(), "n-bob", "Bob", store.ScopeUser, "", "")
	putNode(t, d.Graph(), "n-vim", "vim", store.ScopeUser, "c-vim", "")
	putNode(t, d.Graph(), "n-emacs", "emacs", store.ScopeUser, "c-emacs", "")
	putEdge(t, d.Graph(), "e-alice", "n-alice", "n-vim", store.RelationUses, 1000, 1000)
	putEdge(t, d.Graph(), "e-bob", "n-bob", "n-emacs", store.RelationUses, 1000, 1000)
	putAlias(t, d.Store(), "Alice", "n-alice", 1.0)
	putAlias(t, d.Store(), "Bob", "n-bob", 1.0)

	w := recall.NewWriter(d.Ports())
	for _, m := range []struct{ id, cid string }{
		{"m-vim", "c-vim"}, {"m-emacs", "c-emacs"},
	} {
		if _, err := w.PutMemory(ctx, store.Memory{
			ID: m.id, Scope: store.ScopeUser, Kind: store.KindSemantic, ConceptID: m.cid,
			Content: "工具记录 " + m.id, TValid: 1000, TCreated: 1000,
		}); err != nil {
			t.Fatalf("PutMemory: %v", err)
		}
	}

	r := recall.New(d.Ports(), recall.Config{}, nil)
	res, err := r.Recall(ctx, recall.RecallRequest{Query: "Alice Bob 用什么", Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}

	// 前置：两个实体都必须成为锚点。
	for _, want := range []string{"n-alice", "n-bob"} {
		if !containsID(res.Plan.Anchors, want) {
			t.Fatalf("锚点集合缺 %s：%+v", want, res.Plan.Anchors)
		}
	}
	// graph lane 必须执行（关系线索 uses + 有锚点 + 有 scope）。
	lp, ok := laneOf(res.Plan, recall.LaneGraph)
	if !ok || !lp.Executed {
		t.Fatalf("graph lane 未执行：%+v", res.Plan.Lanes)
	}

	// 两个锚点各自的答案都必须到场。
	ids := itemIDs(res.Items)
	for _, want := range []string{"m-vim", "m-emacs"} {
		if !containsID(ids, want) {
			t.Fatalf("锚点集合里的 %s 未被遍历到（只走了部分锚点）：items=%v anchors=%+v",
				want, ids, res.Plan.Anchors)
		}
	}
	// 证据里两个锚点都必须出现（否则"遍历了"无法解释）。
	anchorsSeen := map[string]bool{}
	for _, it := range res.Items {
		for _, ev := range it.GraphEvidence {
			anchorsSeen[ev.Anchor] = true
		}
	}
	for _, want := range []string{"n-alice", "n-bob"} {
		if !anchorsSeen[want] {
			t.Fatalf("GraphEvidence 里没有锚点 %s 的证据：%+v", want, anchorsSeen)
		}
	}
}

// TestFollowUpOneAnchorFailureDiscardsWholeGraphLane 记录一个**可后续**缺陷。
//
// graph lane 是"全有或全无"的：只要**任一**锚点（或任一跳数组）的 Neighbors 报错，
// graphLane 就返回 nil hits（见 lanes.go 的 if len(degraded) > 0 { return nil, ... }），
// 于是**成功**锚点已经算出的候选也一起被丢掉。
//
// 危害：多锚点问句里一个实体恰好查询失败，整条图通道归零；若该形态的 lane 表里
// 没有别的通道（relational 只有 graph+vector，而 vector 无 embedder 时又缺席），
// 最终结果是 insufficient + 0 条目——用户看到的是"库里没有这个维度"。
func TestFollowUpOneAnchorFailureDiscardsWholeGraphLane(t *testing.T) {
	d := openMemory(t)
	ctx := context.Background()
	putConcept(t, d.Store(), "c-vim", "vim工具", store.ConceptCertified, "一种模态文本编辑器")
	putConcept(t, d.Store(), "c-emacs", "emacs工具", store.ConceptCertified, "一种可扩展文本编辑器")
	putNode(t, d.Graph(), "n-alice", "Alice", store.ScopeUser, "", "")
	putNode(t, d.Graph(), "n-bob", "Bob", store.ScopeUser, "", "")
	putNode(t, d.Graph(), "n-vim", "vim", store.ScopeUser, "c-vim", "")
	putNode(t, d.Graph(), "n-emacs", "emacs", store.ScopeUser, "c-emacs", "")
	putEdge(t, d.Graph(), "e-alice", "n-alice", "n-vim", store.RelationUses, 1000, 1000)
	putEdge(t, d.Graph(), "e-bob", "n-bob", "n-emacs", store.RelationUses, 1000, 1000)
	putAlias(t, d.Store(), "Alice", "n-alice", 1.0)
	putAlias(t, d.Store(), "Bob", "n-bob", 1.0)

	w := recall.NewWriter(d.Ports())
	for _, m := range []struct{ id, cid string }{
		{"m-vim", "c-vim"}, {"m-emacs", "c-emacs"},
	} {
		if _, err := w.PutMemory(ctx, store.Memory{
			ID: m.id, Scope: store.ScopeUser, Kind: store.KindSemantic, ConceptID: m.cid,
			Content: "工具记录 " + m.id, TValid: 1000, TCreated: 1000,
		}); err != nil {
			t.Fatalf("PutMemory: %v", err)
		}
	}

	// 只让 bob 的邻域查询失败，alice 的遍历完全正常。
	ports := d.Ports()
	ports.Graph = &failNeighborsFor{inner: d.Graph(), nodeID: "n-bob"}
	r := recall.New(ports, recall.Config{}, nil)
	res, err := r.Recall(ctx, recall.RecallRequest{Query: "Alice Bob 用什么", Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("单锚点失败不应让整次召回失败：%v", err)
	}

	pin(t, pinFollowUp, "F3", "任一锚点失败即丢弃整个 graph lane 的成功结果（全有或全无）",
		func() string {
			if !containsSubstr(res.Degraded, "n-bob") {
				return "预期 Degraded 点名失败的锚点 n-bob，实际 " + sprintList(res.Degraded)
			}
			if containsID(itemIDs(res.Items), "m-vim") {
				return "成功锚点 n-alice 的结果已保留（缺陷已修）：items=" + sprintList(itemIDs(res.Items))
			}
			return ""
		},
		func() string {
			// 修复方向：失败的锚点记入 Degraded，但成功的锚点结果必须保留。
			if !containsID(itemIDs(res.Items), "m-vim") {
				return "n-alice 遍历成功，却被 n-bob 的失败连累丢弃：items=" +
					sprintList(itemIDs(res.Items)) + " degraded=" + sprintList(res.Degraded)
			}
			return ""
		})
}
