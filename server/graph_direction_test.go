package server_test

import (
	"net/http"
	"testing"

	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// F-8 回归：可视化必须看得见「扇入」，且 degree 与可见边不得自相矛盾
//
// 缺陷形状（leader 实测）：demo 库里 n-demo-acme 出边 0 / 入边 2，
// `GET /api/graph?focus=n-demo-acme&hops=1` 返回 nodes=1 edges=0——点开一个
// 公司/概念这种枢纽，看到的是一张空图。而同一响应里 degree 是**双向**统计的
// （server/graph.go 按最终边集 FromID/ToID 都 ++），于是界面上是
// "圆圈很大却一根边都没有"。
//
// 这里按任务卡的 A / D / 不变量三条各自断言**契约行为**（走真 HTTP + 真 sqlite）。
// B / C（方向语义、hop 计数）在 sqlite 层逐个方向测过：那是端口的语义所在。
// ===========================================================================

// TestGraphHubFanInVisible【回归 A + 必需的不变量】
//
// A works_at B 的单向边，focus=B（扇入型枢纽）&hops=1 必须同时返回 A 与 B，
// 且 edges ≥ 1。修前这里是 nodes=1 / edges=0。
func TestGraphHubFanInVisible(t *testing.T) {
	h := newHarness(t)
	h.node("n-a", "Alice", store.ScopeUser, "")
	h.node("n-b", "Acme", store.ScopeUser, "")
	h.edge("e-ab", "n-a", "n-b", store.RelationWorksAt, 1, 100, 0)

	status, resp, body := h.getGraph("?focus=n-b&hops=1")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	ids := nodeIDs(resp)
	if !hasID(ids, "n-b") || !hasID(ids, "n-a") {
		t.Fatalf("扇入型枢纽的 1 跳邻域必须同时含枢纽与邻居 n-a，实际 %v", ids)
	}
	if len(resp.Edges) < 1 {
		t.Fatalf("扇入型枢纽必须至少返回 1 条可见边，实际 %d 条（nodes=%v）", len(resp.Edges), ids)
	}
	// 枢纽节点不能是"圆圈很大却没有边"：degree>0 时边必须可见。
	hub, ok := findNode(resp, "n-b")
	if !ok {
		t.Fatal("枢纽节点必须在返回体里")
	}
	if hub.Degree == 0 {
		t.Error("枢纽节点 degree 不得为 0（它有入边）")
	}
	if hub.Matched != true {
		t.Error("focus 的枢纽节点必须 matched=true")
	}
}

// TestGraphDegreeMatchesVisibleEdges【回归 D — 内部一致性】
//
// degree 是双向统计的，因此必须满足 degree ≤ 该节点可见边数 × 2。
// 这条用例钉死"节点显示很大却没有任何可见边"的自相矛盾形态：
// 修前 n-b 的 degree=1（来自那条入边），可见边 0 条 → 0×2 < 1，直接变红。
func TestGraphDegreeMatchesVisibleEdges(t *testing.T) {
	h := newHarness(t)
	h.node("n-a", "Alice", store.ScopeUser, "")
	h.node("n-b", "Acme", store.ScopeUser, "")
	h.node("n-c", "Bob", store.ScopeUser, "")
	// 两条入边指向 n-b，一条出边 n-b → n-c：三种方向混在一个枢纽上。
	h.edge("e-ab", "n-a", "n-b", store.RelationWorksAt, 1, 100, 0)
	h.edge("e-cb", "n-c", "n-b", store.RelationWorksAt, 1, 100, 0)
	h.edge("e-bc", "n-b", "n-c", store.RelationUses, 1, 100, 0)

	for _, q := range []string{"?focus=n-b&hops=1", "?focus=n-a&hops=1", "?focus=n-c&hops=1"} {
		t.Run(q, func(t *testing.T) {
			status, resp, body := h.getGraph(q)
			if status != http.StatusOK {
				t.Fatalf("status=%d body=%s", status, body)
			}
			t.Logf("q=%s nodes=%v edges=%d", q, nodeIDs(resp), len(resp.Edges))
			visible := map[string]int{}
			for _, e := range resp.Edges {
				visible[e.From]++
				visible[e.To]++
			}
			for _, n := range resp.Nodes {
				if n.Degree > visible[n.ID]*2 {
					t.Errorf("节点 %s: degree=%d 但可见边只有 %d 条（≤%d）——"+
						"「圆圈很大却一根边都没有」的自相矛盾又回来了",
						n.ID, n.Degree, visible[n.ID]/2, visible[n.ID])
				}
			}
		})
	}
}

// TestGraphHubDepthEncodesDirection【回归 C 的 server 侧观察】
//
// 双向遍历下"被走到的那一端"与边同跳、另一端浅一跳。若 server 仍写死
// "from 浅 / to 深"，扇入场景里邻居的 depth 会算成 1 而枢纽是 0——
// 恰好对；但 focus 在出边端时（本用例的第二种视角）深度会整体错位，
// 前端"越深越淡"的视觉编码就说了谎。
func TestGraphHubDepthEncodesDirection(t *testing.T) {
	h := newHarness(t)
	h.node("n-a", "Alice", store.ScopeUser, "")
	h.node("n-b", "Acme", store.ScopeUser, "")
	h.edge("e-ab", "n-a", "n-b", store.RelationWorksAt, 1, 100, 0)

	// 视角 1：焦点在扇入端（枢纽）——邻居 n-a 必须是 depth=1，枢纽是 0。
	status, resp, body := h.getGraph("?focus=n-b&hops=1")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	hub, ok := findNode(resp, "n-b")
	if !ok {
		t.Fatal("枢纽 n-b 必须在返回体里")
	}
	neighbor, ok := findNode(resp, "n-a")
	if !ok {
		t.Fatal("邻居 n-a 必须在返回体里")
	}
	if hub.Depth != 0 || neighbor.Depth != 1 {
		t.Fatalf("扇入视角深度 = hub:%d neighbor:%d，want 0/1", hub.Depth, neighbor.Depth)
	}

	// 视角 2：焦点在出边端——同一对节点，深度必须镜像且仍然正确。
	status, resp2, body2 := h.getGraph("?focus=n-a&hops=1")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body2)
	}
	hub2, ok := findNode(resp2, "n-b")
	if !ok {
		t.Fatal("n-b 必须在返回体里")
	}
	focus2, ok := findNode(resp2, "n-a")
	if !ok {
		t.Fatal("n-a 必须在返回体里")
	}
	if focus2.Depth != 0 || hub2.Depth != 1 {
		t.Fatalf("出边视角深度 = focus:%d neighbor:%d，want 0/1", focus2.Depth, hub2.Depth)
	}
}

// TestEntityCardFallbackSeesInEdges【F-8 的实体卡分支】
//
// server/entity.go 的 edgesOfNode 在没有边表端口时退化为 Neighbors。
// 修前它只收 `h.Edge.FromID == id` 的边（并把"入边可能缺失"写进 degraded），
// 于是实体卡的"全部关系（含方向与对端）"在退路上永远缺入边。
func TestEntityCardFallbackSeesInEdges(t *testing.T) {
	h := newHarnessNoCatalog(t)
	h.node("n-a", "Alice", store.ScopeUser, "")
	h.node("n-b", "Acme", store.ScopeUser, "")
	h.edge("e-ab", "n-a", "n-b", store.RelationWorksAt, 1, 100, 0)

	// 枢纽 n-b：它只有入边。退路必须仍能列出这条 in 边。
	status, card, body := h.getEntity("n-b", "")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if len(card.Edges) != 1 {
		t.Fatalf("枢纽实体卡应列出 1 条入边，实际 %d 条：%+v", len(card.Edges), card.Edges)
	}
	if card.Edges[0].Direction != "in" || card.Edges[0].Counterpart != "n-a" {
		t.Fatalf("入边应为 direction=in / counterpart=n-a，实际 %+v", card.Edges[0])
	}
	// 降级信息仍须保留（已失效边确实拿不到）——不得因为修了方向就把降级吞掉。
	if len(card.Meta.Degraded) == 0 {
		t.Error("缺边表端口时必须显式降级（已失效边拿不到这一点要说出来）")
	}
}

// TestGraphHubFanInInvalidatedEdgeVisible【必需的不变量 — 已失效入边】
//
// F-8 还有同一类形状：枢纽节点唯一的那条入边**已失效**。
// Neighbors 不遍历失效边，于是对端只能靠 fillEdgesFromCatalog 补回来。
// 旧代码在那里写死“from 浅一跳 / to 深一跳”，当焦点本身就是 to 端时
// 会走“种子已是最浅，不能再往上补”而跳过——枢纽依旧是一张空图
// （实测 nodes=[n-b] edges=0）。方向改成 both 后，哪端更近只能由视图决定。
func TestGraphHubFanInInvalidatedEdgeVisible(t *testing.T) {
	h := newHarness(t)
	h.node("n-a", "Alice", store.ScopeUser, "")
	h.node("n-b", "Acme", store.ScopeUser, "")
	// 唯一的入边已失效（100..300，当前视角下已过期）。
	h.edge("e-ab", "n-a", "n-b", store.RelationWorksAt, 1, 100, 300)

	status, resp, body := h.getGraph("?focus=n-b&hops=1")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	ids := nodeIDs(resp)
	if !hasID(ids, "n-b") || !hasID(ids, "n-a") {
		t.Fatalf("已失效入边的对端也必须进入视图（否则虚线边画不出来），实际 %v", ids)
	}
	e, ok := findEdge(resp, "e-ab")
	if !ok {
		t.Fatalf("已失效的入边必须仍返回（前端画虚线），实际 edges=%v", resp.Edges)
	}
	if !e.Invalidated {
		t.Error("这条边当前视角下已失效，invalidated 必须为 true")
	}
	// 深度：焦点是 0，补出来的对端必须是 1（不是负值，也不是 0）。
	// 旧实现里的 maxInt(to.depth-1, 0) 会把它算成 0，与焦点同层。
	if n, ok := findNode(resp, "n-a"); ok && n.Depth != 1 {
		t.Errorf("补出来的对端 depth=%d, want 1（它比焦点深一跳，不是同层）", n.Depth)
	}

	// 同一形状反过来（出边端已失效）也不得回归：两个方向都要看得见。
	status, resp2, body2 := h.getGraph("?focus=n-a&hops=1")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body2)
	}
	if !hasID(nodeIDs(resp2), "n-b") || len(resp2.Edges) != 1 {
		t.Fatalf("出边端视角应看到 n-b 与 1 条边，实际 nodes=%v edges=%d", nodeIDs(resp2), len(resp2.Edges))
	}
}

// TestGraphBothDirectionStillFoldsByBudget【前端 / 预算折叠】
//
// both 方向会让一次 focus 的节点/边数量上涨（枢纽尤其明显）。预算折叠
// （节点数 > budget → 聚簇折叠）与渲染分档（SVG≤2000 / Canvas）必须仍然正确：
// 站不住的话，枢纽节点一点开就会把整屏塞满。
func TestGraphBothDirectionStillFoldsByBudget(t *testing.T) {
	h := newHarness(t)
	h.concept("c-org", "组织", store.ConceptCertified, store.ScopeUser, "")
	// 一个枢纽 + 12 个只用入边指向它的员工（典型的"公司看员工"形状）。
	h.node("n-acme", "Acme", store.ScopeUser, "c-org")
	for i := 0; i < 12; i++ {
		id := prefixName("n-e", i)
		h.node(id, "E", store.ScopeUser, "c-org")
		h.edge("e-w"+id, id, "n-acme", store.RelationWorksAt, 1, 100, 0)
	}

	// 不折叠时：both 必须让枢纽看见全部 12 个扇入邻居（修前这里是 0 条边）。
	status, resp, body := h.getGraph("?focus=n-acme&hops=1&budget=200")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if len(resp.Nodes) != 13 || len(resp.Edges) != 12 {
		t.Fatalf("枢纽 1 跳 both 应有 13 节点/12 边，实际 %d/%d", len(resp.Nodes), len(resp.Edges))
	}
	if resp.Meta.Truncated {
		t.Error("13 节点未超预算 200，不应折叠")
	}

	// 收紧预算：必须折叠，且折叠后渲染规模不得超预算。
	status, folded, body := h.getGraph("?focus=n-acme&hops=1&budget=3")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if !folded.Meta.Truncated {
		t.Fatal("13 节点 > budget 3，必须显式 truncated=true")
	}
	if len(folded.Nodes) > 3 {
		t.Errorf("折叠后返回节点数=%d，超出预算 3", len(folded.Nodes))
	}
	// 折叠前的真实规模必须仍然可见（否则使用者以为图里就这么多）。
	if folded.Meta.NodeCount != 13 || folded.Meta.EdgeCount != 12 {
		t.Errorf("meta 规模应为 13/12，实际 %d/%d", folded.Meta.NodeCount, folded.Meta.EdgeCount)
	}
	// 折叠视图不得留下指向不存在节点的悬空边。
	present := map[string]bool{}
	for _, n := range folded.Nodes {
		present[n.ID] = true
	}
	for _, e := range folded.Edges {
		if !present[e.From] || !present[e.To] {
			t.Errorf("悬空边 %s（%s→%s）", e.ID, e.From, e.To)
		}
	}
}
