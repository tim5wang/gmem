package p1verify

import (
	"context"
	"testing"

	"github.com/tim5wang/gmem/recall"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// 攻击面 1：无 embedder 时关系型问句必须靠 graph lane 答出
// ===========================================================================

// TestGraphLaneAnswersRelationalWithoutEmbedder 是本卡的硬要求之一：
// 完全没有 embedder 的情况下，"Alice 用什么" 必须由 graph lane 答出，
// 且 Plan.Lanes 里 graph 必须**真的执行**（Executed=true），不是只被"选中"。
//
// 作者测试 TestRelationalQuestionAnsweredByGraphLane 断言"有结果"；
// 本用例额外钉住三件作者没钉的事：
//   - vector lane 缺席必须是**显式**降级（Degraded 有文本，Status=degraded），
//   - 结果必须带**逐条边证据**（否则无法回答"凭什么"），
//   - graph 召回的记忆必须是 concept_id 映射来的那条，不能是 lexical 顺带捞的。
//
// 变异验证：
//   - 把 ShapeRelational 的 lane 规则里的 graph 删掉 → 本用例红（graph 未执行）；
//   - 把 works_at/uses 的 MaxHops 改成 3 → 用例 1 跳断言红（见 hops 测试）。
func TestGraphLaneAnswersRelationalWithoutEmbedder(t *testing.T) {
	d := openMemory(t)
	ctx := context.Background()
	seedEditorOntology(t, d)

	// 关键：这条记忆只有 concept_id 一条线索能连到图上（内容里的词与问句不逐字重合）。
	// 因此它被召回**只能**是因为 graph lane 走了 n-alice --uses--> n-vim --concept--> 记忆。
	w := recall.NewWriter(d.Ports())
	if _, err := w.PutMemory(ctx, store.Memory{
		ID: "m-editor", Scope: store.ScopeUser, Kind: store.KindSemantic,
		ConceptID: "c-editor", Content: "Alice 的默认编辑器是 vim", TValid: 1000, TCreated: 1000,
	}); err != nil {
		t.Fatalf("PutMemory: %v", err)
	}

	r := recall.New(d.Ports(), recall.Config{}, nil) // 明确不注入 embedder
	res, err := r.Recall(ctx, recall.RecallRequest{Query: "Alice 用什么 编辑器", Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}

	// (1) graph lane 真的执行了。
	lp, ok := laneOf(res.Plan, recall.LaneGraph)
	if !ok {
		t.Fatalf("Plan.Lanes 里没有 graph：%+v", res.Plan.Lanes)
	}
	if !lp.Executed {
		t.Fatalf("graph lane 未执行：lane=%+v（skip=%q）", lp, lp.SkipReason)
	}

	// (2) 无 embedder 时 vector lane 必须缺席且**显式**上报，不得静默。
	vlp, ok := laneOf(res.Plan, recall.LaneVector)
	if !ok {
		t.Fatalf("Plan.Lanes 里没有 vector：%+v", res.Plan.Lanes)
	}
	if vlp.Executed {
		t.Fatalf("无 embedder 时 vector lane 不该执行：%+v", vlp)
	}
	if vlp.SkipReason == "" {
		t.Fatalf("vector lane 缺席但 SkipReason 为空（静默降级）：%+v", vlp)
	}
	if !containsSubstr(res.Degraded, "vector lane skipped") {
		t.Fatalf("Degraded=%v，必须显式报告 vector lane 缺席", res.Degraded)
	}
	if res.Status == recall.StatusOK {
		t.Fatalf("status=ok，但有通道缺席，必须是 degraded：%v", res.Degraded)
	}

	// (3) 答出了哪条、凭什么。
	if len(res.Items) == 0 {
		t.Fatalf("无 embedder 时 graph lane 没召回任何记忆：plan=%+v", res.Plan)
	}
	top := res.Items[0]
	if top.Memory.ID != "m-editor" {
		t.Fatalf("top1=%q, want m-editor（got %v）", top.Memory.ID, itemIDs(res.Items))
	}
	if len(top.GraphEvidence) == 0 {
		t.Fatalf("graph 召回的条目没有 GraphEvidence：无法回答'凭什么'")
	}
	ev := top.GraphEvidence[0]
	if ev.EdgeID != "e-alice-vim" || ev.Relation != store.RelationUses {
		t.Fatalf("evidence=%+v, want e-alice-vim/uses", ev)
	}
	if ev.Depth != 1 {
		t.Fatalf("evidence.Depth=%d, want 1（uses 白名单 1 跳）", ev.Depth)
	}
	if ev.Anchor != "n-alice" {
		t.Fatalf("evidence.Anchor=%q, want n-alice（起点必须是词典命中的锚点）", ev.Anchor)
	}

	// (4) 这条记忆的贡献里必须有 graph 通道（而不是只靠 lexical 撞上的）。
	var graphHit bool
	for _, c := range top.Contributions {
		if c.Lane == recall.LaneGraph {
			graphHit = true
		}
	}
	if !graphHit {
		t.Fatalf("top1 的 Contributions 里没有 graph 通道：%+v", top.Contributions)
	}
}

// TestGraphLaneSkippedWithoutAnchorIsVisible 覆盖 graph lane 的另一半：
// 没有实体锚点时它必须**跳过并说明原因**，不能假装执行了。
func TestGraphLaneSkippedWithoutAnchorIsVisible(t *testing.T) {
	d := openMemory(t)
	ctx := context.Background()
	// 只有概念、没有 aliases，也没有能连到记忆的节点 → 无锚点。
	putConcept(t, d.Store(), "c-editor", "编辑器", store.ConceptCertified, "用于编写代码的文本编辑程序")

	w := recall.NewWriter(d.Ports())
	if _, err := w.PutMemory(ctx, store.Memory{
		ID: "m-editor", Scope: store.ScopeUser, Kind: store.KindSemantic,
		ConceptID: "c-editor", Content: "默认编辑器是 vim", TValid: 1000, TCreated: 1000,
	}); err != nil {
		t.Fatalf("PutMemory: %v", err)
	}

	r := recall.New(d.Ports(), recall.Config{}, nil)
	res, err := r.Recall(ctx, recall.RecallRequest{Query: "编辑器 用什么", Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	lp, ok := laneOf(res.Plan, recall.LaneGraph)
	if !ok {
		t.Fatalf("Plan.Lanes 里没有 graph：%+v", res.Plan.Lanes)
	}
	if lp.Executed {
		t.Fatalf("没有锚点时 graph lane 不该执行：%+v", lp)
	}
	if lp.SkipReason == "" {
		t.Fatalf("graph 跳过但 SkipReason 为空（静默）：%+v", lp)
	}
	if !containsSubstr(res.Degraded, "graph lane skipped") {
		t.Fatalf("Degraded=%v，graph 跳过必须显式上报", res.Degraded)
	}
}
