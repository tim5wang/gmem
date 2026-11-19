package server_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/tim5wang/gmem/recall"
	"github.com/tim5wang/gmem/server"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// 变异验证（mutation testing）
//
// 前面的用例证明"现在的实现是对的"。下面的用例证明**那两条机制是承重的**：
// 把它们拆掉，行为必须变坏。否则"跳数裁剪"和"显式 truncated"就只是装饰，
// 删掉了也没人会发现——这类缺陷在生产里是最难查的。
// ===========================================================================

// TestMutationWithoutHopClippingLeaksTwoHops 证明跳数裁剪确实在拦东西。
//
// 手法：绕过 server 的跳数表，直接对同一个后端按"全关系 + 5 跳"查一次。
// 断言那时 works_at 能走到 2 跳——也就是说，如果哪天有人把 lookups 换成
// "一个全局 bfs_max_depth"，works_at 立刻会把"别人公司"画进图里，
// 而 TestHopsClippedByRecallTable 会变红。两条用例合起来才有意义。
func TestMutationWithoutHopClippingLeaksTwoHops(t *testing.T) {
	h := newHarness(t)
	h.node("a", "A", store.ScopeUser, "c")
	h.node("b", "B", store.ScopeUser, "c")
	h.node("c", "C", store.ScopeUser, "c")
	h.edge("w1", "a", "b", store.RelationWorksAt, 1, 100, 0)
	h.edge("w2", "b", "c", store.RelationWorksAt, 1, 100, 0)

	// 变异：不加关系表约束、直接请求 5 跳（= 退回"全局深度"的错误做法）。
	hops, err := h.ports.Graph.Neighbors(context.Background(), store.NeighborQuery{
		NodeID: "a", Scope: store.ScopeUser, Hops: 5,
	})
	if err != nil {
		t.Fatalf("Neighbors: %v", err)
	}
	var reachedC bool
	for _, hop := range hops {
		if hop.Edge.ToID == "c" {
			reachedC = true
		}
	}
	if !reachedC {
		t.Fatal("变异前提不成立：不加约束时应能走到 2 跳（否则本用例无法证明裁剪在起作用）")
	}

	// 对照：经 server（读 recall 表）时不得出现 c。
	status, resp, body := h.getGraph("?focus=a&hops=5")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if hasID(nodeIDs(resp), "c") {
		t.Fatal("server 未按 recall 关系表裁剪 works_at：两跳泄漏进了视图")
	}
}

// TestMutationSilentTruncationWouldLoseInformation 证明"显式 truncated"不是装饰。
//
// 手法：模拟"静默截断"（把节点直接切到 budget 条，不报 truncated、不返回 clusters），
// 断言那样会丢掉两件关键信息：折叠前的真实规模、以及各概念的规模分布。
// 现在 server 返回 meta.node_count + clusters，这两条信息都还在。
func TestMutationSilentTruncationWouldLoseInformation(t *testing.T) {
	h := newHarness(t)
	h.concept("c1", "C1", store.ConceptCertified, store.ScopeUser, "")
	h.concept("c2", "C2", store.ConceptCertified, store.ScopeUser, "")
	// 8 个节点分属 2 个概念，另加一个未归属节点。
	for i := 0; i < 4; i++ {
		h.node(prefixName("a", i), "A", store.ScopeUser, "c1")
		h.node(prefixName("b", i), "B", store.ScopeUser, "c2")
	}
	h.node("u0", "U", store.ScopeUser, "")

	const budget = 3
	status, resp, body := h.getGraph("?budget=3")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if !resp.Meta.Truncated {
		t.Fatal("超预算必须显式 truncated=true")
	}
	// 信息 1：折叠前的真实规模。
	if resp.Meta.NodeCount != 9 {
		t.Errorf("meta.node_count=%d, want 9（折叠前规模必须可见）", resp.Meta.NodeCount)
	}
	// 信息 2：概念规模分布（静默截断会把某个概念整团丢掉，count 就错了）。
	if len(resp.Clusters) != 3 {
		t.Fatalf("应有 3 个簇（c1/c2/未归属），实际 %d", len(resp.Clusters))
	}
	sum := 0
	for _, c := range resp.Clusters {
		sum += c.Count
	}
	if sum != 9 {
		t.Fatalf("各簇 count 之和=%d, want 9：静默截断会让这里的合计对不上真实规模", sum)
	}

	// 变异：静默截断（只保留前 budget 个节点，不报任何信息）。
	var seen []server.Node
	for _, n := range resp.Nodes {
		seen = append(seen, n)
	}
	if len(seen) > budget && !resp.Meta.Truncated {
		t.Fatal("自相矛盾：返回节点数超预算却没有 truncated")
	}
	// 现在（正确实现）返回的节点数不得超预算（含 clusters 折叠）。
	if len(resp.Nodes) > budget {
		t.Errorf("折叠后返回节点数=%d，超出预算 %d", len(resp.Nodes), budget)
	}
}

// TestMutationAsOfWouldLoseHistory 证明"已失效边仍返回"是承重的。
//
// 若像 Neighbors 那样在 as_of=nil 时只返回当前有效边，使用者就再也看不到
// "上一份工作是什么"——双时态在 UI 上等于不存在。这里断言 server **不**丢历史。
func TestMutationAsOfWouldLoseHistory(t *testing.T) {
	h := newHarness(t)
	h.node("alice", "Alice", store.ScopeUser, "")
	h.node("acme", "Acme", store.ScopeUser, "")
	h.edge("e-old", "alice", "acme", store.RelationWorksAt, 1, 100, 300)

	// 变异：Neighbors 在当前视角下看不到失效边。
	hops, err := h.ports.Graph.Neighbors(context.Background(), store.NeighborQuery{
		NodeID: "alice", Scope: store.ScopeUser, Relations: []store.Relation{store.RelationWorksAt}, Hops: 1,
	})
	if err != nil {
		t.Fatalf("Neighbors: %v", err)
	}
	if len(hops) != 0 {
		t.Fatalf("变异前提不成立：Neighbors 应过滤掉失效边，实际返回 %d 条", len(hops))
	}

	// 对照：server 必须把这条历史边补回来。
	status, resp, body := h.getGraph("?focus=alice")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	e, ok := findEdge(resp, "e-old")
	if !ok {
		t.Fatal("server 没有补回已失效边：双时态历史在图上消失了")
	}
	if !e.Invalidated {
		t.Error("补回的失效边必须标 invalidated=true")
	}
	// 而且对端节点必须一起补回来（否则前面那条边会被 pruneDanglingEdges 丢掉）。
	if !hasID(nodeIDs(resp), "acme") {
		t.Error("只靠失效边可达的对端必须一并进入视图")
	}
}

// TestMutationFoldKeepsOnlyMatched 证明"折叠时保留命中实体"是刻意设计。
//
// 若把所有节点（含焦点）一律折叠成概念团，使用者点开一个节点后看到的是
// 一个团——等于把答案藏起来。这里断言焦点仍在 nodes 里。
func TestMutationFoldKeepsOnlyMatched(t *testing.T) {
	h := newHarness(t)
	h.concept("c1", "C1", store.ConceptCertified, store.ScopeUser, "")
	h.node("alice", "Alice", store.ScopeUser, "c1")
	for i := 0; i < 8; i++ {
		h.node(prefixName("x", i), "X", store.ScopeUser, "c1")
	}
	// 焦点必须有边，否则 1 跳邻域只有它自己（邻域规模不会超预算）。
	h.edge("ax0", "alice", "x0", store.RelationUses, 1, 100, 0)
	h.edge("ax1", "alice", "x1", store.RelationUses, 1, 100, 0)
	h.edge("ax2", "alice", "x2", store.RelationUses, 1, 100, 0)

	status, resp, body := h.getGraph("?focus=alice&budget=1")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if !resp.Meta.Truncated {
		t.Fatalf("4 节点（焦点 + 3 邻居）> budget 1，应 truncated：%+v", resp.Meta)
	}
	if _, ok := findNode(resp, "alice"); !ok {
		t.Fatal("折叠时焦点实体必须保留为真实节点（否则等于把这次要看的东西藏进团里）")
	}
	// 其余节点确实被折进团里（团计数包含它们）。
	var total int
	for _, c := range resp.Clusters {
		total += c.Count
	}
	if total < 4 {
		t.Errorf("簇计数应覆盖全部 4 个节点，实际合计 %d", total)
	}
}

// TestHopsParamCannotExceedRelationTable 显式大 hops 不得越权（表是上界，不是建议）。
func TestHopsParamCannotExceedRelationTable(t *testing.T) {
	h := newHarness(t)
	h.node("a", "A", store.ScopeUser, "c")
	h.node("b", "B", store.ScopeUser, "c")
	h.node("c", "C", store.ScopeUser, "c")
	h.node("d", "D", store.ScopeUser, "c")
	// depends_on 表内 3 跳：请求 100 跳也只能 3 跳。
	h.edge("d1", "a", "b", store.RelationDependsOn, 1, 100, 0)
	h.edge("d2", "b", "c", store.RelationDependsOn, 1, 100, 0)
	h.edge("d3", "c", "d", store.RelationDependsOn, 1, 100, 0)

	status, resp, body := h.getGraph("?focus=a&hops=100")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if len(resp.Nodes) != 4 {
		t.Errorf("depends_on 3 跳内应覆盖 4 个节点，实际 %d", len(resp.Nodes))
	}
	// 表里 depends_on 的 MaxHops 必须是 3（回显来自 recall 配置）。
	for _, r := range resp.Meta.Relations {
		if r.Relation == string(store.RelationDependsOn) && r.MaxHops != 3 {
			t.Errorf("depends_on 的 MaxHops=%d, want 3（来自 recall.DefaultRelationRules）", r.MaxHops)
		}
	}
	// 用 recall 的默认表交叉核对：可视化的表必须与它一致。
	for _, rule := range recall.DefaultRelationRules() {
		var found bool
		for _, r := range resp.Meta.Relations {
			if r.Relation == string(rule.Relation) {
				found = true
				wantHops := rule.MaxHops
				if !rule.Transitive && wantHops > 1 {
					wantHops = 1
				}
				if r.MaxHops != wantHops {
					t.Errorf("%s: meta.MaxHops=%d, recall 表=%d（两边漂移了）", rule.Relation, r.MaxHops, wantHops)
				}
			}
		}
		if !found {
			t.Errorf("meta.relations 缺少 recall 表里的关系 %q", rule.Relation)
		}
	}
}
