package server_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/tim5wang/gmem/server"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// GET /api/graph 的三种入口
// ===========================================================================

// TestGraphFocusEntry：(a) ?focus=<nodeID> 以节点为中心取邻域，hops 默认 1。
func TestGraphFocusEntry(t *testing.T) {
	h := newHarness(t)
	// alice --works_at--> acme --works_at--> other（两跳）
	h.node("alice", "Alice", store.ScopeUser, "person")
	h.node("acme", "Acme", store.ScopeUser, "org")
	h.node("other", "OtherCo", store.ScopeUser, "org")
	h.edge("e1", "alice", "acme", store.RelationWorksAt, 1, 100, 0)
	h.edge("e2", "acme", "other", store.RelationWorksAt, 1, 100, 0)

	status, resp, body := h.getGraph("?focus=alice")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if resp.Meta.Mode != "focus" {
		t.Errorf("mode=%q, want focus", resp.Meta.Mode)
	}
	if resp.Meta.Focus != "alice" {
		t.Errorf("focus=%q, want alice", resp.Meta.Focus)
	}
	if resp.Meta.Hops != 1 {
		t.Errorf("hops 默认应为 1，实际 %d", resp.Meta.Hops)
	}
	ids := nodeIDs(resp)
	// works_at 非传递 → 只能 1 跳：必须包含 acme，**不得**包含 other。
	if !hasID(ids, "alice") || !hasID(ids, "acme") {
		t.Fatalf("1 跳邻域应含 alice+acme，实际 %v", ids)
	}
	if hasID(ids, "other") {
		t.Fatalf("works_at 不得跳 2 跳，却出现了 other：%v", ids)
	}
	// focus 模式的 scope 由节点推导（不必显式传）。
	if resp.Meta.Scope != "user" {
		t.Errorf("scope 应从节点推导为 user，实际 %q", resp.Meta.Scope)
	}
	// 命中实体必须标 matched（前端红描边）。
	n, ok := findNode(resp, "alice")
	if !ok {
		t.Fatalf("alice 不在返回节点里")
	}
	if !n.Matched {
		t.Error("focus 的种子节点必须 matched=true")
	}
}

// TestGraphSearchEntry：(b) ?q=<关键词> 走 Recall 取命中实体作种子。
func TestGraphSearchEntry(t *testing.T) {
	h := newHarness(t)
	h.node("alice", "Alice", store.ScopeUser, "person")
	h.node("acme", "Acme", store.ScopeUser, "org")
	h.alias("Alice", "alice")
	h.edge("e1", "alice", "acme", store.RelationWorksAt, 1, 100, 0)

	status, resp, body := h.getGraph("?q=Alice")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if resp.Meta.Mode != "search" {
		t.Errorf("mode=%q, want search", resp.Meta.Mode)
	}
	if resp.Meta.Query != "Alice" {
		t.Errorf("query 未回显：%q", resp.Meta.Query)
	}
	if resp.Meta.Recall == nil {
		t.Fatal("search 模式必须回显 recall 摘要")
	}
	if !hasID(resp.Meta.Seeds, "alice") {
		t.Fatalf("q=Alice 应以 alice 为种子，实际 seeds=%v", resp.Meta.Seeds)
	}
	n, ok := findNode(resp, "alice")
	if !ok {
		t.Fatalf("种子节点 alice 应在返回图里，nodes=%v", nodeIDs(resp))
	}
	if !n.Matched {
		t.Error("q 命中实体必须 matched=true（红色描边）")
	}
	// 命中实体的 1 跳邻居也应出现（图而不是孤点）。
	if !hasID(nodeIDs(resp), "acme") {
		t.Errorf("种子的一跳邻居 acme 应出现在图里，nodes=%v", nodeIDs(resp))
	}
}

// TestGraphOverviewEntry：(c) 无参数 = 概览模式，按 concept 聚簇折叠。
func TestGraphOverviewEntry(t *testing.T) {
	h := newHarness(t)
	h.concept("person", "人", store.ConceptCertified, store.ScopeUser, "")
	h.concept("org", "组织", store.ConceptDraft, store.ScopeUser, store.ConceptOriginImplicit)
	h.node("alice", "Alice", store.ScopeUser, "person")
	h.node("bob", "Bob", store.ScopeUser, "person")
	h.node("acme", "Acme", store.ScopeUser, "org")
	h.edge("e1", "alice", "acme", store.RelationWorksAt, 1, 100, 0)

	status, resp, body := h.getGraph("")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if resp.Meta.Mode != "overview" {
		t.Errorf("mode=%q, want overview", resp.Meta.Mode)
	}
	if len(resp.Clusters) != 2 {
		t.Fatalf("概览应按 concept 聚成 2 团，实际 %d: %+v", len(resp.Clusters), resp.Clusters)
	}
	counts := map[string]int{}
	for _, c := range resp.Clusters {
		counts[c.ConceptID] = c.Count
	}
	if counts["person"] != 2 || counts["org"] != 1 {
		t.Fatalf("聚簇计数错误：%v", counts)
	}
	// 概念状态必须带上（含 draft + implicit origin，前端判斜纹）。
	for _, c := range resp.Clusters {
		if c.ConceptID == "org" {
			if c.ConceptStatus != string(store.ConceptDraft) {
				t.Errorf("org 簇 concept_status=%q, want draft", c.ConceptStatus)
			}
			if c.Origin != string(store.ConceptOriginImplicit) {
				t.Errorf("org 簇 origin=%q, want implicit", c.Origin)
			}
		}
	}
	// 未超预算 → 不折叠。
	if resp.Meta.Truncated {
		t.Error("3 个节点远小于预算 200，不应 truncated")
	}
	if len(resp.Nodes) != 3 {
		t.Errorf("未折叠时应返回全部 3 个节点，实际 %d", len(resp.Nodes))
	}
}

// ===========================================================================
// 跳数裁剪：必须复用 recall 的 relation 传递性表（TD §9）
// ===========================================================================

// TestHopsClippedByRecallTable 实测两条 TD 点名的规则：
// works_at 不得跳 2 跳、subtype_of 最多 3 跳。
func TestHopsClippedByRecallTable(t *testing.T) {
	h := newHarness(t)
	// 链：a → b → c → d
	h.node("a", "A", store.ScopeUser, "c0")
	h.node("b", "B", store.ScopeUser, "c0")
	h.node("c", "C", store.ScopeUser, "c0")
	h.node("d", "D", store.ScopeUser, "c0")

	t.Run("works_at 请求 3 跳也只能走 1 跳", func(t *testing.T) {
		h.edge("w1", "a", "b", store.RelationWorksAt, 1, 100, 0)
		h.edge("w2", "b", "c", store.RelationWorksAt, 1, 100, 0)
		h.edge("w3", "c", "d", store.RelationWorksAt, 1, 100, 0)
		status, resp, body := h.getGraph("?focus=a&hops=3")
		if status != http.StatusOK {
			t.Fatalf("status=%d body=%s", status, body)
		}
		ids := nodeIDs(resp)
		if !hasID(ids, "b") {
			t.Fatalf("works_at 1 跳邻居 b 应出现：%v", ids)
		}
		if hasID(ids, "c") || hasID(ids, "d") {
			t.Fatalf("works_at 非传递：请求 3 跳也不得走到 c/d，实际 %v", ids)
		}
		// meta.relations 必须回显这张表（视觉与检索共用一套语义的证据）。
		var found bool
		for _, r := range resp.Meta.Relations {
			if r.Relation == string(store.RelationWorksAt) {
				found = true
				if r.MaxHops != 1 || r.Transitive {
					t.Errorf("works_at 在表里应为 MaxHops=1/Transitive=false，实际 %+v", r)
				}
			}
		}
		if !found {
			t.Fatal("meta.relations 未回显 works_at")
		}
	})

	t.Run("subtype_of 可 3 跳但不得 4 跳", func(t *testing.T) {
		h.node("s1", "S1", store.ScopeUser, "c1")
		h.node("s2", "S2", store.ScopeUser, "c1")
		h.node("s3", "S3", store.ScopeUser, "c1")
		h.node("s4", "S4", store.ScopeUser, "c1")
		h.node("s5", "S5", store.ScopeUser, "c1")
		h.edge("t1", "s1", "s2", store.RelationSubtypeOf, 1, 100, 0)
		h.edge("t2", "s2", "s3", store.RelationSubtypeOf, 1, 100, 0)
		h.edge("t3", "s3", "s4", store.RelationSubtypeOf, 1, 100, 0)
		h.edge("t4", "s4", "s5", store.RelationSubtypeOf, 1, 100, 0)

		status, resp, body := h.getGraph("?focus=s1&hops=9")
		if status != http.StatusOK {
			t.Fatalf("status=%d body=%s", status, body)
		}
		ids := nodeIDs(resp)
		if !hasID(ids, "s4") {
			t.Fatalf("subtype_of 表内 3 跳应能到 s4，实际 %v", ids)
		}
		if hasID(ids, "s5") {
			t.Fatalf("subtype_of 表内上限 3 跳，不得到 s5，实际 %v", ids)
		}
	})
}

// TestHopsTableFollowsRecallConfig 证明跳数**读的是 recall 的配置**而不是本地常量：
// 把 recall.Config.RelationRules 换掉，可视化的跳数必须跟着变。
func TestHopsTableFollowsRecallConfig(t *testing.T) {
	// owns 默认非传递 1 跳；改成传递 4 跳后可视化必须能走 3 跳。
	h := newHarness(t, recallConfigWithOwnsTransitive())

	h.node("o1", "O1", store.ScopeUser, "c")
	h.node("o2", "O2", store.ScopeUser, "c")
	h.node("o3", "O3", store.ScopeUser, "c")
	h.node("o4", "O4", store.ScopeUser, "c")
	h.edge("k1", "o1", "o2", store.RelationOwns, 1, 100, 0)
	h.edge("k2", "o2", "o3", store.RelationOwns, 1, 100, 0)
	h.edge("k3", "o3", "o4", store.RelationOwns, 1, 100, 0)

	status, resp, body := h.getGraph("?focus=o1&hops=3")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	ids := nodeIDs(resp)
	if !hasID(ids, "o3") {
		t.Fatalf("换表后 owns 应可传递 3 跳（到 o3），实际 %v", ids)
	}
	var row server.RelationLimit
	for _, r := range resp.Meta.Relations {
		if r.Relation == string(store.RelationOwns) {
			row = r
		}
	}
	if row.MaxHops != 3 || !row.Transitive {
		t.Fatalf("meta.relations 应反映替换后的表（MaxHops=3/Transitive=true），实际 %+v", row)
	}
}

// ===========================================================================
// 预算折叠
// ===========================================================================

// TestBudgetFold 超预算时必须**先聚簇折叠**，且 meta 显式报 truncated + budget。
func TestBudgetFold(t *testing.T) {
	h := newHarness(t)
	// 3 个概念团，每团 4 个节点 = 12 个节点；含一个命中种子。
	h.concept("c1", "C1", store.ConceptCertified, store.ScopeUser, "")
	h.concept("c2", "C2", store.ConceptCertified, store.ScopeUser, "")
	h.concept("c3", "C3", store.ConceptCertified, store.ScopeUser, "")
	for i := 0; i < 4; i++ {
		h.node(prefixName("a", i), "A", store.ScopeUser, "c1")
		h.node(prefixName("b", i), "B", store.ScopeUser, "c2")
		h.node(prefixName("c", i), "C", store.ScopeUser, "c3")
	}
	h.edge("e-ab", "a0", "b0", store.RelationDependsOn, 1, 100, 0)
	h.edge("e-bc", "b0", "c0", store.RelationDependsOn, 1, 100, 0)

	// 概览 + 小预算：12 > 5 → 折叠。
	status, resp, body := h.getGraph("?budget=5")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if !resp.Meta.Truncated {
		t.Fatal("12 节点 > budget 5，必须 truncated=true（不得静默截断）")
	}
	if resp.Meta.Budget != 5 {
		t.Errorf("meta.budget=%d, want 5", resp.Meta.Budget)
	}
	if resp.Meta.NodeCount != 12 {
		t.Errorf("meta.node_count 应报折叠前的真实规模 12，实际 %d", resp.Meta.NodeCount)
	}
	// 必须返回 clusters（含 concept 与 count），而不是裸节点堆。
	if len(resp.Clusters) != 3 {
		t.Fatalf("折叠应返回 3 个概念团，实际 %d: %+v", len(resp.Clusters), resp.Clusters)
	}
	total := 0
	for _, c := range resp.Clusters {
		if c.Count != 4 {
			t.Errorf("簇 %s 的 count=%d, want 4", c.ID, c.Count)
		}
		if c.ConceptName == "" {
			t.Errorf("簇 %s 缺 concept_name", c.ID)
		}
		total += c.Count
	}
	if total != 12 {
		t.Errorf("各簇 count 之和=%d, want 12", total)
	}
	// 折叠视图里团间聚合边必须有（否则前端只能画散点）。
	if len(resp.ClusterEdges) == 0 {
		t.Error("折叠视图必须返回 cluster_edges（团间关系），实际为空")
	}
	// 未折叠时不得有 cluster_edges（开关必须是有效的，不是恒返回）。
	_, resp2, body2 := h.getGraph("")
	if resp2.Meta.Truncated {
		t.Fatalf("默认预算 200 下 12 节点不应折叠：%s", body2)
	}
	if len(resp2.ClusterEdges) != 0 {
		t.Errorf("未折叠时不应返回 cluster_edges，实际 %d 条", len(resp2.ClusterEdges))
	}
	if len(resp2.Nodes) != 12 {
		t.Errorf("未折叠应返回 12 个节点，实际 %d", len(resp2.Nodes))
	}
}

// TestBudgetFoldKeepsMatchedNode 折叠时"这次要看的命中实体"不能被折进团里。
func TestBudgetFoldKeepsMatchedNode(t *testing.T) {
	h := newHarness(t)
	h.concept("c1", "C1", store.ConceptCertified, store.ScopeUser, "")
	for i := 0; i < 10; i++ {
		h.node(prefixName("n", i), "N", store.ScopeUser, "c1")
	}
	// alice 是焦点，有 3 个邻居。
	h.node("alice", "Alice", store.ScopeUser, "c1")
	h.node("x1", "X1", store.ScopeUser, "c1")
	h.node("x2", "X2", store.ScopeUser, "c1")
	h.edge("ea1", "alice", "x1", store.RelationUses, 1, 100, 0)
	h.edge("ea2", "alice", "x2", store.RelationUses, 1, 100, 0)

	status, resp, body := h.getGraph("?focus=alice&hops=1&budget=2")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if !resp.Meta.Truncated {
		t.Fatalf("3 节点 > budget 2，应 truncated：%s", body)
	}
	n, ok := findNode(resp, "alice")
	if !ok {
		t.Fatal("折叠时焦点实体 alice 不得被折进概念团（那是这次要看的东西）")
	}
	if !n.Matched {
		t.Error("alice 必须 matched=true")
	}
}

// ===========================================================================
// 确定性坐标
// ===========================================================================

// TestCoordsDeterministic 同一输入多次请求必须**逐字节一致**（TD §9：避免前端抖动）。
func TestCoordsDeterministic(t *testing.T) {
	h := newHarness(t)
	h.concept("c1", "C1", store.ConceptCertified, store.ScopeUser, "")
	h.concept("c2", "C2", store.ConceptDraft, store.ScopeUser, "")
	for i := 0; i < 6; i++ {
		h.node(prefixName("a", i), "A", store.ScopeUser, "c1")
		h.node(prefixName("b", i), "B", store.ScopeUser, "c2")
	}
	h.edge("e1", "a0", "b0", store.RelationDependsOn, 1, 100, 0)
	h.edge("e2", "a1", "b1", store.RelationDependsOn, 1, 100, 0)

	queries := []string{
		"",          // 概览
		"?focus=a0", // 邻域
		"?focus=a0&hops=2&budget=50",
		"?focus=a1&as_of=200", // 双时态
	}
	for _, q := range queries {
		t.Run("query="+q, func(t *testing.T) {
			status1, body1 := h.get("/api/graph" + q)
			if status1 != http.StatusOK {
				t.Fatalf("status=%d body=%s", status1, body1)
			}
			for i := 0; i < 3; i++ {
				status2, body2 := h.get("/api/graph" + q)
				if status2 != http.StatusOK {
					t.Fatalf("第 %d 次请求 status=%d", i+2, status2)
				}
				if !bytes.Equal(body1, body2) {
					t.Fatalf("同一输入两次响应不逐字节一致（坐标抖动）：\n--- first ---\n%s\n--- second ---\n%s",
						body1, body2)
				}
			}
		})
	}
}

// TestCoordsDependOnlyOnNodeID 证明坐标不依赖请求顺序/时间：删掉别的节点，
// 剩余节点的坐标必须不变（否则前端每加一条记忆就整图重排）。
func TestCoordsDependOnlyOnNodeID(t *testing.T) {
	h1 := newHarness(t)
	h1.concept("c1", "C1", store.ConceptCertified, store.ScopeUser, "")
	h1.node("a", "A", store.ScopeUser, "c1")
	h1.node("b", "B", store.ScopeUser, "c1")
	_, r1, _ := h1.getGraph("")
	posA1 := mustPos(t, r1, "a")

	h2 := newHarness(t)
	h2.concept("c1", "C1", store.ConceptCertified, store.ScopeUser, "")
	h2.node("a", "A", store.ScopeUser, "c1")
	h2.node("b", "B", store.ScopeUser, "c1")
	h2.node("z", "Z", store.ScopeUser, "c1") // 多一个节点
	_, r2, _ := h2.getGraph("")
	posA2 := mustPos(t, r2, "a")

	if posA1 != posA2 {
		t.Fatalf("节点 a 的坐标因其它节点增减而漂移：%+v vs %+v", posA1, posA2)
	}
}

// ===========================================================================
// 双时态
// ===========================================================================

// TestAsOfKeepsInvalidatedEdge 已失效边**仍返回**但标 invalidated=true（前端画虚线）。
func TestAsOfKeepsInvalidatedEdge(t *testing.T) {
	h := newHarness(t)
	h.node("alice", "Alice", store.ScopeUser, "c1")
	h.node("acme", "Acme", store.ScopeUser, "c2")
	h.node("newco", "NewCo", store.ScopeUser, "c2")
	// 旧事实：100..300 期间 alice works_at acme（已失效）。
	h.edge("e-old", "alice", "acme", store.RelationWorksAt, 1, 100, 300)
	// 新事实：300 起 alice works_at newco（当前有效）。
	h.edge("e-new", "alice", "newco", store.RelationWorksAt, 1, 300, 0)

	// 当前视角：两条边都要在图里，旧边标 invalidated。
	status, resp, body := h.getGraph("?focus=alice")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	oldEdge, ok := findEdge(resp, "e-old")
	if !ok {
		t.Fatalf("已失效边必须**仍返回**（前端画虚线），实际缺失。edges=%v", resp.Edges)
	}
	if !oldEdge.Invalidated {
		t.Error("过期边的 invalidated 必须为 true")
	}
	if oldEdge.TValid != 100 || oldEdge.TInvalid != 300 {
		t.Errorf("边必须返回 t_valid/t_invalid：got %d/%d", oldEdge.TValid, oldEdge.TInvalid)
	}
	newEdge, ok := findEdge(resp, "e-new")
	if !ok {
		t.Fatal("当前有效的边 e-new 必须返回")
	}
	if newEdge.Invalidated {
		t.Error("有效边的 invalidated 必须为 false")
	}

	// as_of=200 视角：e-old 当时有效 → invalidated=false；e-new 尚未生效 → 视为无效。
	status, resp200, body200 := h.getGraph("?focus=alice&as_of=200")
	if status != http.StatusOK {
		t.Fatalf("as_of=200 status=%d body=%s", status, body200)
	}
	if resp200.Meta.AsOf == nil || *resp200.Meta.AsOf != 200 {
		t.Errorf("meta.as_of 未回显 200：%+v", resp200.Meta.AsOf)
	}
	if e, ok := findEdge(resp200, "e-old"); ok {
		if e.Invalidated {
			t.Error("as_of=200 时 e-old 应当有效（invalidated=false）")
		}
	} else {
		t.Error("as_of=200 时 e-old 应可见")
	}
}

// ===========================================================================
// 视觉编码字段（缺了前端就得二次请求）
// ===========================================================================

// TestVisualEncodingFields 节点/边必须自带画图所需的全部字段。
func TestVisualEncodingFields(t *testing.T) {
	h := newHarness(t)
	h.concept("person", "人", store.ConceptCertified, store.ScopeUser, "")
	h.concept("org", "组织", store.ConceptDraft, store.ScopeUser, store.ConceptOriginImplicit)
	// nodes.source_episode 有外键约束：先落 episode，否则 PutNode 会被拒。
	h.episode("ep-1", 100, "Alice 在 Acme 工作。")
	h.nodeWithSource("alice", "Alice", store.ScopeUser, "person", "ep-1")
	h.node("acme", "Acme", store.ScopeUser, "org")
	h.node("bob", "Bob", store.ScopeUser, "person")
	h.edge("e1", "alice", "acme", store.RelationWorksAt, 0.8, 100, 0)
	h.edge("e2", "alice", "bob", store.Relation("possiblySame"), 1, 100, 0)

	status, resp, body := h.getGraph("?focus=alice")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}

	alice, ok := findNode(resp, "alice")
	if !ok {
		t.Fatal("alice 应出现")
	}
	if alice.ConceptID != "person" {
		t.Errorf("concept_id=%q, want person", alice.ConceptID)
	}
	if alice.ConceptStatus != string(store.ConceptCertified) {
		t.Errorf("concept_status=%q, want certified", alice.ConceptStatus)
	}
	if alice.Degree == 0 {
		t.Error("degree 决定节点大小，不能为 0（alice 有边）")
	}
	if alice.SourceEpisode != "ep-1" {
		t.Errorf("source_episode=%q, want ep-1", alice.SourceEpisode)
	}
	// draft + implicit 概念必须能看出来（斜纹）。
	acme, ok := findNode(resp, "acme")
	if !ok {
		t.Fatal("acme 应出现")
	}
	if acme.ConceptStatus != string(store.ConceptDraft) {
		t.Errorf("acme concept_status=%q, want draft", acme.ConceptStatus)
	}
	if acme.Origin != string(store.ConceptOriginImplicit) {
		t.Errorf("acme origin=%q, want implicit", acme.Origin)
	}

	e1, ok := findEdge(resp, "e1")
	if !ok {
		t.Fatal("e1 应出现")
	}
	if e1.Relation != string(store.RelationWorksAt) {
		t.Errorf("relation=%q", e1.Relation)
	}
	if e1.Weight != 0.8 {
		t.Errorf("weight=%v, want 0.8", e1.Weight)
	}
	if e1.PossiblySame {
		t.Error("works_at 不是待审边，possibly_same 应为 false")
	}
	e2, ok := findEdge(resp, "e2")
	if !ok {
		t.Fatal("possiblySame 边应出现")
	}
	if !e2.PossiblySame {
		t.Error("possiblySame 边的 possibly_same 必须为 true（前端画蓝色）")
	}
}

// ===========================================================================
// 错误处理：404 / 400，且不得 panic
// ===========================================================================

// TestGraphErrors 覆盖 404 与各类非法参数（必须 400 且信息可读）。
func TestGraphErrors(t *testing.T) {
	h := newHarness(t)
	h.node("alice", "Alice", store.ScopeUser, "c")

	cases := []struct {
		name       string
		query      string
		wantStatus int
		wantField  string
	}{
		{"未知 focus 节点 → 404", "?focus=nope", http.StatusNotFound, ""},
		{"hops 非数字 → 400", "?focus=alice&hops=abc", http.StatusBadRequest, "hops"},
		{"hops=0 → 400", "?focus=alice&hops=0", http.StatusBadRequest, "hops"},
		{"budget 超硬上限 → 400", "?budget=5000", http.StatusBadRequest, "budget"},
		{"budget 非数字 → 400", "?budget=x", http.StatusBadRequest, "budget"},
		{"as_of 非数字 → 400", "?as_of=yesterday", http.StatusBadRequest, "as_of"},
		{"scope 非法 → 400", "?scope=galaxy", http.StatusBadRequest, "scope"},
		{"focus 与 q 同时给 → 400", "?focus=alice&q=x", http.StatusBadRequest, "focus"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := h.get("/api/graph" + tc.query)
			if status != tc.wantStatus {
				t.Fatalf("status=%d, want %d (body=%s)", status, tc.wantStatus, body)
			}
			var e server.ErrorResponse
			if err := json.Unmarshal(body, &e); err != nil {
				t.Fatalf("错误体不是 JSON: %v (body=%s)", err, body)
			}
			if e.Message == "" {
				t.Error("错误信息必须可读（message 不得为空）")
			}
			if tc.wantField != "" && e.Field != tc.wantField {
				t.Errorf("field=%q, want %q", e.Field, tc.wantField)
			}
		})
	}
}

// TestBudgetHardLimitBoundary 【F-6 回归】maxBudget=2000 的**边界**必须被钉死。
//
// 背景：既有用例只用 budget=5000 测越界，而 5000 > 3000 也成立，所以把
// maxBudget 从 2000 改成 3000（甚至 4999）时全部测试仍然绿 —— 上限附近有一个
// 宽度未知的“静默漂移窗口”。这里直接钉住边界两侧：
//
//	budget=2000 → 200，且 meta.budget=2000 / meta.budget_max=2000
//	budget=2001 → 400，field=budget，message 可读
//
// 变异验证：把 server/graph.go 的 maxBudget 改成 2001/3000，下面第一条会红。
func TestBudgetHardLimitBoundary(t *testing.T) {
	h := newHarness(t)
	h.node("alice", "Alice", store.ScopeUser, "c")

	// 上限本身必须被接受，并把 budget / budget_max 如实回显。
	status, resp, body := h.getGraph("?budget=2000")
	if status != http.StatusOK {
		t.Fatalf("budget=2000 是硬上限内的合法值，必须 200，实际 %d (body=%s)", status, body)
	}
	if resp.Meta.Budget != 2000 {
		t.Errorf("meta.budget=%d, want 2000（不得静默夹取）", resp.Meta.Budget)
	}
	if resp.Meta.BudgetMax != 2000 {
		t.Errorf("meta.budget_max=%d, want 2000（外壳靠它校验上限）", resp.Meta.BudgetMax)
	}

	// 超出 1 也必须拒绝：报错比“悄悄只画 2000 个”诚实。
	status, body = h.get("/api/graph?budget=2001")
	if status != http.StatusBadRequest {
		t.Fatalf("budget=2001 超硬上限，必须 400，实际 %d (body=%s)", status, body)
	}
	var e server.ErrorResponse
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("错误体不是 JSON: %v (body=%s)", err, body)
	}
	if e.Field != "budget" {
		t.Errorf("field=%q, want budget", e.Field)
	}
	if e.Message == "" {
		t.Error("错误信息必须可读（不得空字符串）")
	}
}

// TestUnknownRouteAndMethod 未知路径 404、方法不符 405（不得 panic）。
func TestUnknownRouteAndMethod(t *testing.T) {
	h := newHarness(t)
	if status, _ := h.get("/api/nope"); status != http.StatusNotFound {
		t.Errorf("未知路径 status=%d, want 404", status)
	}
	// POST /api/graph 必须 405（只读路由不接受写方法）。
	if status, body := h.postJSON("/api/graph", "{}"); status != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/graph status=%d, want 405 (body=%s)", status, body)
	}
	// GET /api/review 也必须 405（写入口只接受 POST）。
	if status, body := h.get("/api/review"); status != http.StatusMethodNotAllowed {
		t.Errorf("GET /api/review status=%d, want 405 (body=%s)", status, body)
	}
	// DELETE /api/entity/xxx → 405。
	if status, body := h.raw(http.MethodDelete, "/api/entity/alice", ""); status != http.StatusMethodNotAllowed {
		t.Errorf("DELETE /api/entity status=%d, want 405 (body=%s)", status, body)
	}
}

// TestEntityUnknown404 未知实体 404；缺 ID 400。
func TestEntityUnknown404(t *testing.T) {
	h := newHarness(t)
	if status, _, body := h.getEntity("nope", ""); status != http.StatusNotFound {
		t.Errorf("未知实体 status=%d, want 404 (body=%s)", status, body)
	}
	if status, body := h.get("/api/entity/"); status != http.StatusBadRequest {
		t.Errorf("缺 ID status=%d, want 400 (body=%s)", status, body)
	}
}

// TestNoPanicOnWeirdInputs 用畸形输入扫一遍，确保不 panic（httptest 覆盖）。
func TestNoPanicOnWeirdInputs(t *testing.T) {
	h := newHarness(t)
	weird := []string{
		"/api/graph?focus=", "/api/graph?q=", "/api/graph?budget=", "/api/graph?hops=",
		"/api/graph?as_of=", "/api/graph?k=0", "/api/graph?k=999",
		"/api/graph?focus=%00", "/api/graph?budget=999999999999999999999",
		"/api/entity/%2F", "/api/entity/a/b",
	}
	for _, path := range weird {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("%s 触发 panic: %v", path, r)
				}
			}()
			status, _ := h.get(path)
			if status >= 500 && status != http.StatusInternalServerError {
				t.Logf("%s → %d", path, status)
			}
		}()
	}
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func prefixName(prefix string, i int) string {
	// '0'..'9' 足够用，避免引 fmt。
	return prefix + string(rune('0'+i))
}

func mustPos(t *testing.T, resp server.GraphResponse, id string) [2]float64 {
	t.Helper()
	n, ok := findNode(resp, id)
	if !ok {
		t.Fatalf("节点 %q 不在返回体里", id)
	}
	return [2]float64{n.X, n.Y}
}
