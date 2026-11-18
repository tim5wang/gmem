package p1verify

import (
	"context"
	"fmt"
	"testing"

	"github.com/tim5wang/gmem/recall"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// 攻击面 3：跳数白名单是否**真的**生效
//
// 跳数错了不会报错，只会召回噪声——所以必须在"结果集"上验，不能只看 plan 的数字。
// 手法：每个节点挂**独立 concept**（1 节点 → 1 concept → 1 记忆），
// 这样跳数效应不会被 mapHopsToMemories 的"同概念扇出"掩盖
// （扇出问题单独由 TestFollowUpConceptFanout 记录）。
// ===========================================================================

// TestHopWhitelistWorksAtIsOneHop 构造 works_at 链 A→B→C→D：
// 只允许 1 跳，因此 B 可召回、C/D 必须**不出现**在结果与 plan 中。
//
// 变异验证：把 works_at 的 MaxHops 改成 3（Graphiti 式盲 BFS）→ 本用例红（C/D 冒出来）。
func TestHopWhitelistWorksAtIsOneHop(t *testing.T) {
	d := openMemory(t)
	ctx := context.Background()

	// 每个节点一个独立概念，各自的记忆只能由该节点映射到。
	for _, c := range []struct{ id, name string }{
		{"c-b", "B单位"}, {"c-c", "C单位"}, {"c-d", "D单位"},
	} {
		putConcept(t, d.Store(), c.id, c.name, store.ConceptCertified, "单位定义 "+c.name)
	}
	putNode(t, d.Graph(), "n-alice", "Alice", store.ScopeUser, "", "")
	putNode(t, d.Graph(), "n-b", "B", store.ScopeUser, "c-b", "")
	putNode(t, d.Graph(), "n-c", "C", store.ScopeUser, "c-c", "")
	putNode(t, d.Graph(), "n-d", "D", store.ScopeUser, "c-d", "")
	putEdge(t, d.Graph(), "e1", "n-alice", "n-b", store.RelationWorksAt, 1000, 1000)
	putEdge(t, d.Graph(), "e2", "n-b", "n-c", store.RelationWorksAt, 1000, 1000)
	putEdge(t, d.Graph(), "e3", "n-c", "n-d", store.RelationWorksAt, 1000, 1000)
	putAlias(t, d.Store(), "Alice", "n-alice", 1.0)

	w := recall.NewWriter(d.Ports())
	for _, m := range []struct{ id, cid string }{
		{"m-b", "c-b"}, {"m-c", "c-c"}, {"m-d", "c-d"},
	} {
		if _, err := w.PutMemory(ctx, store.Memory{
			ID: m.id, Scope: store.ScopeUser, Kind: store.KindSemantic, ConceptID: m.cid,
			Content: "单位记录 " + m.id, TValid: 1000, TCreated: 1000,
		}); err != nil {
			t.Fatalf("PutMemory: %v", err)
		}
	}

	r := recall.New(d.Ports(), recall.Config{}, nil)
	// "就职" 是 works_at 的线索词，且不与任何概念名重叠（避开 B2 的遮挡缺陷）。
	res, err := r.Recall(ctx, recall.RecallRequest{Query: "Alice 就职 单位", Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}

	// (1) plan 里 works_at 必须 1 跳、非传递、来源是 query-cue。
	rp, ok := relationPlanOf(res.Plan, store.RelationWorksAt)
	if !ok {
		t.Fatalf("Plan.Relations 未包含 works_at：%+v", res.Plan.Relations)
	}
	if rp.MaxHops != 1 {
		t.Fatalf("works_at MaxHops=%d, want 1（TD §5：两跳就到别人公司）", rp.MaxHops)
	}
	if rp.Transitive {
		t.Fatalf("works_at 被标成传递关系：%+v", rp)
	}
	if rp.Origin != "query-cue" {
		t.Fatalf("works_at Origin=%q, want query-cue", rp.Origin)
	}
	if res.Plan.Hops != 1 {
		t.Fatalf("Plan.Hops=%d, want 1", res.Plan.Hops)
	}

	// (2) 结果集：1 跳答案 m-b 必须出现；m-c/m-d 绝不能被 **graph 通道**走到。
	//
	// 断言口径在原意图（“works_at 只允许 1 跳”）上不变，只把观测面从 items 收窄到
	// graph 证据：F3 给 relational 形态加上了 lexical 通道（词法能答就不该弃答），
	// 而 m-c/m-d 的正文里就有“单位”二字，词法本来就能命中它们（与跳数无关）。
	// 因此“不出现在 items 里”不再是跳数白名单的性质，而是词法相关性的性质；
	// 真正的跳数证据是**它们不能被 graph 通道召回**（不挂任何边证据）。
	ids := itemIDs(res.Items)
	if !containsID(ids, "m-b") {
		t.Fatalf("1 跳答案 m-b 未召回：items=%v", ids)
	}
	for _, banned := range []string{"m-c", "m-d"} {
		for _, it := range res.Items {
			if it.Memory.ID != banned {
				continue
			}
			if len(it.GraphEvidence) != 0 {
				t.Fatalf("%s 在 works_at 的 2/3 跳之外，却挂了图证据（跳数白名单失效）：%+v",
					banned, it.GraphEvidence)
			}
		}
	}

	// (3) 每条结果的边证据深度都必须是 1（不能"结果对但证据说走了 2 跳"）。
	for _, it := range res.Items {
		for _, ev := range it.GraphEvidence {
			if ev.Depth != 1 {
				t.Fatalf("%s 的证据 depth=%d, want 1（%+v）", it.Memory.ID, ev.Depth, ev)
			}
			if ev.Relation != store.RelationWorksAt {
				t.Fatalf("%s 的证据关系=%s, want works_at", it.Memory.ID, ev.Relation)
			}
		}
	}

	// (4) 没有节点被静默丢弃：m-c/m-d 不该出现在 Dropped 里被当成"召回了但被切掉"。
	for _, dr := range res.Plan.Dropped {
		if dr.ID == "m-c" || dr.ID == "m-d" {
			t.Fatalf("%s 出现在 Dropped 里：说明它被遍历出来过（跳数越界）：%+v", dr.ID, res.Plan.Dropped)
		}
	}
}

// TestHopWhitelistSubtypeOfIsThreeHops 构造 subtype_of 链 A→B→C→D→E：
// 允许 3 跳，因此 B/C/D 可召回、E（4 跳）必须不出现。
//
// 变异验证：把 subtype_of 的 MaxHops 改成 1 → 本用例红（C/D 缺失）。
func TestHopWhitelistSubtypeOfIsThreeHops(t *testing.T) {
	d := openMemory(t)
	ctx := context.Background()

	for _, c := range []struct{ id, name string }{
		{"c-b", "B类"}, {"c-c", "C类"}, {"c-d", "D类"}, {"c-e", "E类"},
	} {
		putConcept(t, d.Store(), c.id, c.name, store.ConceptCertified, "类型定义 "+c.name)
	}
	putNode(t, d.Graph(), "n-a", "A根", store.ScopeUser, "", "")
	putNode(t, d.Graph(), "n-b", "B", store.ScopeUser, "c-b", "")
	putNode(t, d.Graph(), "n-c", "C", store.ScopeUser, "c-c", "")
	putNode(t, d.Graph(), "n-d", "D", store.ScopeUser, "c-d", "")
	putNode(t, d.Graph(), "n-e", "E", store.ScopeUser, "c-e", "")
	putEdge(t, d.Graph(), "s1", "n-a", "n-b", store.RelationSubtypeOf, 1000, 1000)
	putEdge(t, d.Graph(), "s2", "n-b", "n-c", store.RelationSubtypeOf, 1000, 1000)
	putEdge(t, d.Graph(), "s3", "n-c", "n-d", store.RelationSubtypeOf, 1000, 1000)
	putEdge(t, d.Graph(), "s4", "n-d", "n-e", store.RelationSubtypeOf, 1000, 1000)
	putAlias(t, d.Store(), "A根", "n-a", 1.0)

	w := recall.NewWriter(d.Ports())
	for _, m := range []struct{ id, cid string }{
		{"m-b", "c-b"}, {"m-c", "c-c"}, {"m-d", "c-d"}, {"m-e", "c-e"},
	} {
		if _, err := w.PutMemory(ctx, store.Memory{
			ID: m.id, Scope: store.ScopeUser, Kind: store.KindSemantic, ConceptID: m.cid,
			Content: "类型记录 " + m.id, TValid: 1000, TCreated: 1000,
		}); err != nil {
			t.Fatalf("PutMemory: %v", err)
		}
	}

	r := recall.New(d.Ports(), recall.Config{}, nil)
	res, err := r.Recall(ctx, recall.RecallRequest{Query: "A根 是一种 什么", Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}

	rp, ok := relationPlanOf(res.Plan, store.RelationSubtypeOf)
	if !ok {
		t.Fatalf("Plan.Relations 未包含 subtype_of：%+v", res.Plan.Relations)
	}
	if rp.MaxHops != 3 {
		t.Fatalf("subtype_of MaxHops=%d, want 3（抽象层级天然可传）", rp.MaxHops)
	}
	if !rp.Transitive {
		t.Fatalf("subtype_of 未标为传递关系：%+v", rp)
	}
	if res.Plan.Hops != 3 {
		t.Fatalf("Plan.Hops=%d, want 3", res.Plan.Hops)
	}

	ids := itemIDs(res.Items)
	for _, want := range []string{"m-b", "m-c", "m-d"} {
		if !containsID(ids, want) {
			t.Fatalf("3 跳内的 %s 未召回：items=%v", want, ids)
		}
	}
	if containsID(ids, "m-e") {
		t.Fatalf("m-e 在 4 跳之外（subtype_of 上限 3），却被召回：items=%v", ids)
	}

	// 深度必须如实反映：B=1、C=2、D=3。
	wantDepth := map[string]int{"m-b": 1, "m-c": 2, "m-d": 3}
	for _, it := range res.Items {
		want, ok := wantDepth[it.Memory.ID]
		if !ok {
			continue
		}
		var depths []int
		for _, ev := range it.GraphEvidence {
			depths = append(depths, ev.Depth)
		}
		var found bool
		for _, dp := range depths {
			if dp == want {
				found = true
			}
			if dp > 3 {
				t.Fatalf("%s 的证据 depth=%d 超出 3 跳上限：%+v", it.Memory.ID, dp, it.GraphEvidence)
			}
		}
		if !found {
			t.Fatalf("%s 的证据里没有 depth=%d（实际 %v）", it.Memory.ID, want, depths)
		}
	}
}

// TestHopWhitelistPartOfIsThreeHopsOwnsIsOne 覆盖同一张图上不同关系走不同跳数：
// 这是"跳数由关系决定，不由全局常量决定"的直接检验。
//
// 构造：alice --owns--> x --part_of--> y --part_of--> z --part_of--> w。
//   - owns=1 跳：只能到 x；
//   - part_of=3 跳：从 x 出发可到 y/z/w 中的 3 跳内者。
//
// 若实现退回全局 max_depth=3，owns 也会走到 y/z，本用例红。
func TestHopWhitelistPartOfIsThreeHopsOwnsIsOne(t *testing.T) {
	d := openMemory(t)
	ctx := context.Background()

	for _, c := range []struct{ id, name string }{
		{"c-x", "X资产"}, {"c-y", "Y资产"}, {"c-z", "Z资产"},
	} {
		putConcept(t, d.Store(), c.id, c.name, store.ConceptCertified, "资产定义 "+c.name)
	}
	putNode(t, d.Graph(), "n-alice", "Alice", store.ScopeUser, "", "")
	putNode(t, d.Graph(), "n-x", "X", store.ScopeUser, "c-x", "")
	putNode(t, d.Graph(), "n-y", "Y", store.ScopeUser, "c-y", "")
	putNode(t, d.Graph(), "n-z", "Z", store.ScopeUser, "c-z", "")
	putEdge(t, d.Graph(), "e-owns", "n-alice", "n-x", store.RelationOwns, 1000, 1000)
	putEdge(t, d.Graph(), "e-p1", "n-x", "n-y", store.RelationPartOf, 1000, 1000)
	putEdge(t, d.Graph(), "e-p2", "n-y", "n-z", store.RelationPartOf, 1000, 1000)
	putAlias(t, d.Store(), "Alice", "n-alice", 1.0)

	w := recall.NewWriter(d.Ports())
	for _, m := range []struct{ id, cid string }{
		{"m-x", "c-x"}, {"m-y", "c-y"}, {"m-z", "c-z"},
	} {
		if _, err := w.PutMemory(ctx, store.Memory{
			ID: m.id, Scope: store.ScopeUser, Kind: store.KindSemantic, ConceptID: m.cid,
			Content: "资产记录 " + m.id, TValid: 1000, TCreated: 1000,
		}); err != nil {
			t.Fatalf("PutMemory: %v", err)
		}
	}

	// 纯 owns 问句：线索 "持有" → owns，只允许 1 跳。
	r := recall.New(d.Ports(), recall.Config{}, nil)
	resOwns, err := r.Recall(ctx, recall.RecallRequest{Query: "Alice 持有 什么", Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("Recall(owns): %v", err)
	}
	rp, ok := relationPlanOf(resOwns.Plan, store.RelationOwns)
	if !ok {
		t.Fatalf("Plan.Relations 未包含 owns：%+v", resOwns.Plan.Relations)
	}
	if rp.MaxHops != 1 {
		t.Fatalf("owns MaxHops=%d, want 1（三跳全是无关资产）", rp.MaxHops)
	}
	ownsItems := itemIDs(resOwns.Items)
	if !containsID(ownsItems, "m-x") {
		t.Fatalf("owns 的 1 跳答案 m-x 未召回：%v", ownsItems)
	}
	for _, banned := range []string{"m-y", "m-z"} {
		if containsID(ownsItems, banned) {
			t.Fatalf("%s 超出 owns 的 1 跳，却被召回（跳数是全局常量的症状）：%v", banned, ownsItems)
		}
	}

	// part_of 问句：线索 "属于" → part_of，允许 3 跳。
	// 注意：这里起点是 Alice（有别名），part_of 边从 X 出发，因此 1 跳内没有 part_of 邻居——
	// 只断言 plan 的跳数决定正确（结果集由起点决定，不作为跳数证据）。
	resPart, err := r.Recall(ctx, recall.RecallRequest{Query: "Alice 属于 什么", Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("Recall(part_of): %v", err)
	}
	rpPart, ok := relationPlanOf(resPart.Plan, store.RelationPartOf)
	if !ok {
		t.Fatalf("Plan.Relations 未包含 part_of：%+v", resPart.Plan.Relations)
	}
	if rpPart.MaxHops != 3 {
		t.Fatalf("part_of MaxHops=%d, want 3", rpPart.MaxHops)
	}
	if !rpPart.Transitive {
		t.Fatalf("part_of 未标为传递关系：%+v", rpPart)
	}
}

// TestHopWhitelistUnknownRelationFallsBackToOneHop 表外关系必须按"非传递 1 跳"兜底，
// **并且**仍出现在 plan 里（不允许"用了图却没有规则可查"）。
func TestHopWhitelistUnknownRelationFallsBackToOneHop(t *testing.T) {
	d := openMemory(t)
	ctx := context.Background()

	putConcept(t, d.Store(), "c-x", "X物", store.ConceptCertified, "某个物的定义")
	putNode(t, d.Graph(), "n-alice", "Alice", store.ScopeUser, "", "")
	putNode(t, d.Graph(), "n-x", "X", store.ScopeUser, "c-x", "")
	// related_to 不在 §5 白名单里。
	putEdge(t, d.Graph(), "e-r", "n-alice", "n-x", store.Relation("related_to"), 1000, 1000)
	putAlias(t, d.Store(), "Alice", "n-alice", 1.0)

	w := recall.NewWriter(d.Ports())
	if _, err := w.PutMemory(ctx, store.Memory{
		ID: "m-x", Scope: store.ScopeUser, Kind: store.KindSemantic, ConceptID: "c-x",
		Content: "X 的记录", TValid: 1000, TCreated: 1000,
	}); err != nil {
		t.Fatalf("PutMemory: %v", err)
	}

	// 用自定义 Config 让 Planner 把 related_to 也纳入白名单（模拟 P2 自定义关系）。
	cfg := recall.Config{
		RelationCues: []recall.RelationCue{
			{Relation: store.Relation("related_to"), Phrases: []string{"相关", "related to"}},
		},
	}
	r := recall.New(d.Ports(), cfg, nil)
	res, err := r.Recall(ctx, recall.RecallRequest{Query: "Alice 相关 什么", Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}

	rp, ok := relationPlanOf(res.Plan, store.Relation("related_to"))
	if !ok {
		t.Fatalf("表外关系 related_to 未出现在 Plan.Relations（违反'用了图必须有规则可查'）：%+v",
			res.Plan.Relations)
	}
	if rp.MaxHops != 1 || rp.Transitive {
		t.Fatalf("表外关系未按'非传递 1 跳'兜底：%+v", rp)
	}
}

// TestFollowUpConceptFanoutIsImprecise 记录一个**可后续**的精确率问题：
//
// 节点→记忆的映射按 concept_id 扇出（见 lanes.go mapHopsToMemories），
// 因此图上只要命中一个节点，同概念下的**全部**记忆都会被召回，
// 并且全部挂同一条边证据（证据无法区分"它凭什么"）。
// 这是 P2 抽取质量要解决的问题（节点应尽量一一对应），P1 阶段作为已知限制记录。
func TestFollowUpConceptFanoutIsImprecise(t *testing.T) {
	d := openMemory(t)
	ctx := context.Background()
	putConcept(t, d.Store(), "c-company", "公司", store.ConceptCertified, "雇佣员工的组织实体")
	putNode(t, d.Graph(), "n-alice", "Alice", store.ScopeUser, "", "")
	putNode(t, d.Graph(), "n-acme", "Acme", store.ScopeUser, "c-company", "")
	putEdge(t, d.Graph(), "e-w", "n-alice", "n-acme", store.RelationWorksAt, 1000, 1000)
	putAlias(t, d.Store(), "Alice", "n-alice", 1.0)

	w := recall.NewWriter(d.Ports())
	// 5 条同概念但内容无关的记忆。
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("m-other-%d", i)
		if _, err := w.PutMemory(ctx, store.Memory{
			ID: id, Scope: store.ScopeUser, Kind: store.KindSemantic, ConceptID: "c-company",
			Content: "无关的公司记录 " + id, TValid: int64(1000 + i), TCreated: int64(1000 + i),
		}); err != nil {
			t.Fatalf("PutMemory: %v", err)
		}
	}

	r := recall.New(d.Ports(), recall.Config{}, nil)
	res, err := r.Recall(ctx, recall.RecallRequest{Query: "Alice 就职 公司", Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}

	pinKnownLimitation(t, pinFollowUp, "F2", "节点→记忆按 concept 扇出，一抢全中且证据不可区分",
		"P2 抽取质量（节点应近 1:1 对应记忆），本卡只修 P1 召回契约",
		func() string {
			if len(res.Items) < 5 {
				return fmt.Sprintf("预期 5 条同概念记忆全部被召回，实际 %v（现象已变化）", itemIDs(res.Items))
			}
			var sameEdge int
			for _, it := range res.Items {
				if len(it.GraphEvidence) == 1 && it.GraphEvidence[0].EdgeID == "e-w" {
					sameEdge++
				}
			}
			if sameEdge != len(res.Items) {
				return fmt.Sprintf("预期全部条目挂同一条边证据，实际 %d/%d", sameEdge, len(res.Items))
			}
			return ""
		})
}
