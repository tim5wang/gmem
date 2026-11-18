package p1verify

import (
	"context"
	"math"
	"regexp"
	"strconv"
	"testing"

	"github.com/tim5wang/gmem/fusion"
	"github.com/tim5wang/gmem/recall"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// 攻击面 4：Explain 必须**真的可复现**，不是"非空就算过"
//
// 断言口径：把 plan / item 里的每个数都拿出来**重算**一遍，并与实际输出比对。
//   - lane：集合、权重必须等于声明式规则表（表改则测试跟着改，不允许写死分支）；
//   - 跳数：每条 relation 的 MaxHops/Transitive 必须等于关系白名单表，Plan.Hops = max；
//   - 贡献：每条 LaneHit 的 Value 必须 = Weight/(K+Rank)，且 ΣValue = FusionScore；
//   - 分数分解：FusionScore × ConceptBoost × RecencyFactor = Score（容差）；
//   - plan 文本：Plan.Steps 里 "score:<id>" 记录的四个数必须能解析出来并与结构体逐项相等。
// ===========================================================================

var scoreStepRe = regexp.MustCompile(
	`fusion=([0-9.]+) × concept=([0-9.]+) × recency=([0-9.]+) = ([0-9.]+)`)

// TestExplainIsReproducible 端到端复算（关系型 + graph lane + 概念一致性加权）。
func TestExplainIsReproducible(t *testing.T) {
	d := openMemory(t)
	ctx := context.Background()
	seedEditorOntology(t, d)

	w := recall.NewWriter(d.Ports())
	if _, err := w.PutMemory(ctx, store.Memory{
		ID: "m-editor", Scope: store.ScopeUser, Kind: store.KindSemantic,
		ConceptID: "c-editor", Content: "Alice 的默认编辑器是 vim", TValid: 1000, TCreated: 1000,
	}); err != nil {
		t.Fatalf("PutMemory: %v", err)
	}

	r := recall.New(d.Ports(), recall.Config{}, nil)
	res, err := r.Recall(ctx, recall.RecallRequest{Query: "Alice 用什么 编辑器", Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	plan := res.Plan
	if plan == nil {
		t.Fatal("Plan 为 nil（R5 要求恒有 plan）")
	}

	// --- (a) lane 选择必须来自声明式表 -------------------------------------
	if plan.Shape != recall.ShapeRelational {
		t.Fatalf("Shape=%s, want relational（命中关系线索词）", plan.Shape)
	}
	if plan.ShapeReason == "" {
		t.Fatalf("ShapeReason 为空：无法解释形态判定")
	}
	rule, ok := laneRuleFor(plan.Shape)
	if !ok {
		t.Fatalf("规则表里没有形态 %s 的行", plan.Shape)
	}
	if len(plan.Lanes) != len(rule.Lanes) {
		t.Fatalf("Plan.Lanes=%d 条，规则表 %d 条：%+v", len(plan.Lanes), len(rule.Lanes), plan.Lanes)
	}
	for i, lw := range rule.Lanes {
		lp := plan.Lanes[i]
		if lp.Lane != lw.Lane {
			t.Fatalf("Lanes[%d].Lane=%s, 规则表=%s（顺序/内容必须与表一致）", i, lp.Lane, lw.Lane)
		}
		if math.Abs(lp.Weight-lw.Weight) > 1e-9 {
			t.Fatalf("lane %s weight=%.4f, 规则表=%.4f（权重必须查表，不得写死）",
				lp.Lane, lp.Weight, lw.Weight)
		}
		if lp.Reason == "" {
			t.Fatalf("lane %s 没有 Reason", lp.Lane)
		}
		if lp.Budget <= 0 {
			t.Fatalf("lane %s Budget=%d，必须记录每 lane 预算", lp.Lane, lp.Budget)
		}
		if !lp.Selected {
			t.Fatalf("lane %s 未被选中（调用方未强制指定 lane）", lp.Lane)
		}
	}

	// --- (b) 跳数：表 ↔ plan 双向一致 --------------------------------------
	tdHops := map[store.Relation]int{
		store.RelationSubtypeOf: 3, store.RelationPartOf: 3, store.RelationDependsOn: 3,
		store.RelationCausedBy: 2, store.RelationWorksAt: 1, store.RelationOwns: 1,
	}
	tableHops := map[store.Relation]int{}
	for _, rule := range recall.DefaultRelationRules() {
		tableHops[rule.Relation] = rule.MaxHops
	}
	for rel, want := range tdHops {
		if got, ok := tableHops[rel]; !ok || got != want {
			t.Errorf("关系白名单表 %s 跳数=%d(存在=%v), TD §5 要求 %d", rel, got, ok, want)
		}
	}
	if len(plan.Relations) == 0 {
		t.Fatalf("Plan.Relations 为空：用了图却没有规则可查")
	}
	maxHops := 0
	for _, rp := range plan.Relations {
		want, ok := tableHops[rp.Relation]
		if !ok {
			// 表外关系按"非传递 1 跳"兜底，也必须显式列出。
			if rp.MaxHops != 1 || rp.Transitive {
				t.Fatalf("表外关系 %s 未按 1 跳兜底：%+v", rp.Relation, rp)
			}
			want = 1
		}
		if rp.MaxHops != want {
			t.Fatalf("关系 %s MaxHops=%d, 表=%d（跳数必须查表）", rp.Relation, rp.MaxHops, want)
		}
		if rp.Origin != "query-cue" && rp.Origin != "shape-default" {
			t.Fatalf("关系 %s Origin=%q，必须是 query-cue 或 shape-default", rp.Relation, rp.Origin)
		}
		if rp.MaxHops > maxHops {
			maxHops = rp.MaxHops
		}
	}
	if plan.Hops != maxHops {
		t.Fatalf("Plan.Hops=%d, want %d（= max(每条 relation 的 MaxHops)）", plan.Hops, maxHops)
	}

	// --- (c) 结果级复算 ----------------------------------------------------
	if len(res.Items) == 0 {
		t.Fatalf("无结果，无法复算分数分解")
	}
	it := res.Items[0]
	if len(it.Contributions) == 0 {
		t.Fatalf("Contributions 为空：无法回答'每 lane 贡献'")
	}
	var sum float64
	for _, c := range it.Contributions {
		if c.Lane == "" {
			t.Fatalf("LaneHit.Lane 为空：%+v", c)
		}
		if c.Rank < 1 {
			t.Fatalf("LaneHit.Rank=%d, 必须 1-based", c.Rank)
		}
		if c.Weight <= 0 {
			t.Fatalf("LaneHit.Weight=%.4f，必须来自 lane 规则表（>0）", c.Weight)
		}
		wantValue := c.Weight / float64(fusion.DefaultK+c.Rank)
		if math.Abs(c.Value-wantValue) > 1e-12 {
			t.Fatalf("lane %s Value=%.12f, want %.12f = weight/(K+rank)",
				c.Lane, c.Value, wantValue)
		}
		sum += c.Value
	}
	if math.Abs(sum-it.FusionScore) > 1e-9 {
		t.Fatalf("ΣContributions=%.12f ≠ FusionScore=%.12f（贡献不可复算）", sum, it.FusionScore)
	}
	if math.Abs(it.FusionScore*it.ConceptBoost*it.RecencyFactor-it.Score) > 1e-9 {
		t.Fatalf("分数分解不自洽：%.12f × %.4f × %.4f ≠ %.12f",
			it.FusionScore, it.ConceptBoost, it.RecencyFactor, it.Score)
	}
	if it.ConceptBoost < 1 || it.RecencyFactor <= 0 || it.RecencyFactor > 1 {
		t.Fatalf("乘子越界：boost=%.3f recency=%.3f", it.ConceptBoost, it.RecencyFactor)
	}
	// 两类调整都必须留痕且带 Reason。
	adjust := map[string]recall.Adjustment{}
	for _, a := range it.Adjustments {
		if a.Reason == "" {
			t.Fatalf("Adjustment 缺 Reason：%+v", a)
		}
		adjust[a.Kind] = a
	}
	if _, ok := adjust["concept_consistency"]; !ok {
		t.Fatalf("缺 concept_consistency 调整：%+v", it.Adjustments)
	}
	if _, ok := adjust["recency_decay"]; !ok {
		t.Fatalf("缺 recency_decay 调整：%+v", it.Adjustments)
	}
	// Adjustments 里的因子必须与结构体字段一致（否则"解释"与"计分"两套数）。
	if math.Abs(adjust["concept_consistency"].Factor-it.ConceptBoost) > 1e-12 {
		t.Fatalf("Adjustments 的 boost=%.6f ≠ Item.ConceptBoost=%.6f",
			adjust["concept_consistency"].Factor, it.ConceptBoost)
	}
	if math.Abs(adjust["recency_decay"].Factor-it.RecencyFactor) > 1e-12 {
		t.Fatalf("Adjustments 的 recency=%.6f ≠ Item.RecencyFactor=%.6f",
			adjust["recency_decay"].Factor, it.RecencyFactor)
	}

	// --- (d) plan 文本必须能解析出同样的数 ---------------------------------
	detail, ok := stepDetail(plan.Steps, "score:"+it.Memory.ID)
	if !ok {
		t.Fatalf("Plan.Steps 缺 score:%s（逐条分数分解）：%+v", it.Memory.ID, plan.Steps)
	}
	m := scoreStepRe.FindStringSubmatch(detail)
	if m == nil {
		t.Fatalf("score 步骤不可解析（格式漂移会让人读不了 explain）：%q", detail)
	}
	got := make([]float64, 4)
	for i := range got {
		v, err := strconv.ParseFloat(m[i+1], 64)
		if err != nil {
			t.Fatalf("score 步骤第 %d 个数不可解析：%q", i+1, m[i+1])
		}
		got[i] = v
	}
	// plan 文本是 %.5f / %.2f / %.3f 截断，因此按对应精度比较。
	if math.Abs(got[0]-it.FusionScore) > 1e-5 {
		t.Fatalf("plan 里 fusion=%.5f ≠ 结构体 %.6f", got[0], it.FusionScore)
	}
	if math.Abs(got[1]-it.ConceptBoost) > 1e-2 {
		t.Fatalf("plan 里 concept=%.2f ≠ 结构体 %.4f", got[1], it.ConceptBoost)
	}
	if math.Abs(got[2]-it.RecencyFactor) > 1e-3 {
		t.Fatalf("plan 里 recency=%.3f ≠ 结构体 %.4f", got[2], it.RecencyFactor)
	}
	if math.Abs(got[3]-it.Score) > 1e-5 {
		t.Fatalf("plan 里 score=%.5f ≠ 结构体 %.6f", got[3], it.Score)
	}

	// --- (e) 关键步骤齐全 --------------------------------------------------
	for _, want := range []string{"align", "relations", "fuse"} {
		if !hasStep(plan.Steps, want) {
			t.Fatalf("Plan.Steps 缺 %q：%+v", want, plan.Steps)
		}
	}
	for _, lp := range plan.Lanes {
		if want := "lane:" + string(lp.Lane); !hasStep(plan.Steps, want) {
			t.Fatalf("Plan.Steps 缺 %q（每 lane 的决定必须可解释）：%+v", want, plan.Steps)
		}
	}
}

// TestExplainDroppedIsReproducible 断言"哪些被拦截"可复算：每条 Dropped 必须带 ID 与非空 Reason，
// 且被预算切断的条数 + 最终返回条数 = 融合候选条数（不静默丢）。
func TestExplainDroppedIsReproducible(t *testing.T) {
	d := openMemory(t)
	ctx := context.Background()

	w := recall.NewWriter(d.Ports())
	for _, id := range []string{"m-a", "m-b", "m-c"} {
		if _, err := w.PutMemory(ctx, store.Memory{
			ID: id, Scope: store.ScopeUser, Kind: store.KindSemantic,
			Content: "编辑器 vim 配置 " + id, TValid: 1000, TCreated: 1000,
		}); err != nil {
			t.Fatalf("PutMemory(%s): %v", id, err)
		}
	}

	r := recall.New(d.Ports(), recall.Config{}, nil)
	res, err := r.Recall(ctx, recall.RecallRequest{
		Query: "编辑器 vim 配置", Scope: store.ScopeUser,
		Budget: recall.Budget{PerLane: 10, Final: 1},
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(res.Items) != 1 {
		t.Fatalf("final=1 时应只返回 1 条，got %d", len(res.Items))
	}
	if len(res.Plan.Dropped) == 0 {
		t.Fatalf("有 3 条命中但 final=1，Plan.Dropped 必须记录被切断的候选")
	}
	for _, dr := range res.Plan.Dropped {
		if dr.ID == "" {
			t.Fatalf("Dropped 缺 ID：%+v", dr)
		}
		if dr.Reason == "" {
			t.Fatalf("Dropped 缺 Reason：%+v", dr)
		}
	}
	// 每条被切断的候选都必须有解释，且不能与被返回的条目重复。
	returned := map[string]bool{}
	for _, id := range itemIDs(res.Items) {
		returned[id] = true
	}
	var budgetDropped int
	for _, dr := range res.Plan.Dropped {
		if returned[dr.ID] {
			t.Fatalf("Dropped 里出现了已返回的条目 %q（解释与实际不一致）", dr.ID)
		}
		if containsSubstr([]string{dr.Reason}, "final budget") {
			budgetDropped++
		}
	}
	if budgetDropped != 2 {
		t.Fatalf("final budget 切断数=%d, want 2（3 条命中 - 1 条返回）：%+v", budgetDropped, res.Plan.Dropped)
	}
}

// laneRuleFor 从声明式表里取某形态的规则行。
func laneRuleFor(shape recall.QueryShape) (recall.LaneRule, bool) {
	for _, r := range recall.DefaultLaneRules() {
		if r.Shape == shape {
			return r, true
		}
	}
	return recall.LaneRule{}, false
}
