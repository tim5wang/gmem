package p1verify

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/tim5wang/gmem/recall"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// 本卡修复项的回归用例（每条都对应一个已修缺陷 / 已定裁决）
//
// 这些用例断言**修复后的正确行为**，且各自做过变异验证：
// 把生产代码改回旧行为时，对应的用例必须变红（变异记录见卡评论与提交信息）。
// ===========================================================================

// TestFixD15ImplicitDraftConcept 是 B-1 的回归：写入未声明的 concept_id 必须成功，
// 并隐式落成一条 status=draft 概念（origin=implicit）。
//
// 旧行为：memories.concept_id 有外键 → PutMemory 返回
// "FOREIGN KEY constraint failed (787)"，整笔写入（含 FTS）一起回滚，违背 D1/R1。
// 变异验证：把隐式概念的状态改成 certified → 本用例红（断言 draft 失败）。
func TestFixD15ImplicitDraftConcept(t *testing.T) {
	d, raw := openFile(t)
	ctx := context.Background()
	w := recall.NewWriter(d.Ports())

	res, err := w.PutMemory(ctx, store.Memory{
		ID: "m-implicit", Scope: store.ScopeUser, Kind: store.KindSemantic,
		ConceptID: "c-never-declared", Content: "隐式概念写入", TValid: 1000, TCreated: 1000,
	})
	if err != nil {
		t.Fatalf("写入未声明的 concept_id 必须成功（D15/R1），实际错误：%v", err)
	}
	if res.Version != 1 || !res.LexicalIndexed {
		t.Fatalf("写入结果不完整：%+v", res)
	}

	// FTS 也真的落了（旧行为会随外键失败一起回滚）。
	hits, err := d.Lexical().Search(ctx, store.LexicalQuery{Query: "隐式概念写入", Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != "m-implicit" {
		t.Fatalf("词法索引未随写入落地：%+v", hits)
	}

	// 概念表多了一条 draft，且 origin=implicit。
	var status, origin string
	if err := raw.QueryRow(
		`SELECT status, origin FROM concepts WHERE id = ?`, "c-never-declared",
	).Scan(&status, &origin); err != nil {
		t.Fatalf("未隐式创建概念行（D15 B 方案）：%v", err)
	}
	if status != string(store.ConceptDraft) {
		t.Fatalf("隐式概念 status=%q, want draft（草稿不得参与硬契约）", status)
	}
	if origin != string(store.ConceptOriginImplicit) {
		t.Fatalf("隐式概念 origin=%q, want implicit", origin)
	}

	// 已声明的概念不受影响（不得被隐式逻辑覆盖成 draft）。
	if err := d.Store().PutConcept(ctx, store.Concept{
		ID: "c-declared", Name: "显式概念", Status: store.ConceptCertified,
	}); err != nil {
		t.Fatalf("PutConcept: %v", err)
	}
	if _, err := w.PutMemory(ctx, store.Memory{
		ID: "m-declared", Scope: store.ScopeUser, Kind: store.KindSemantic,
		ConceptID: "c-declared", Content: "显式概念写入", TValid: 1000, TCreated: 1000,
	}); err != nil {
		t.Fatalf("PutMemory: %v", err)
	}
	if err := raw.QueryRow(`SELECT status FROM concepts WHERE id = 'c-declared'`).Scan(&status); err != nil {
		t.Fatalf("query declared concept: %v", err)
	}
	if status != string(store.ConceptCertified) {
		t.Fatalf("已声明概念被改写为 %q（隐式逻辑误伤）", status)
	}
}

// TestFixD15DraftIsDiscountNotRejection 是 B-1 的另一半：draft 概念“只打折、不打回”。
//
// 断言：draft 概念的精确查询**仍能答出**（不得因状态被拒答），且离开 coverage 判定后
// 它不会把一次完全听懂的问题判成 insufficient（F2b）。
//
// 变异验证：让 aligner 跳过 draft 概念（等价于把 StatusFactor 当准入闸）→ 本用例红。
func TestFixD15DraftIsDiscountNotRejection(t *testing.T) {
	d := openMemory(t)
	ctx := context.Background()
	putConcept(t, d.Store(), "c-draft", "向量索引", store.ConceptDraft, "近似最近邻检索结构")

	w := recall.NewWriter(d.Ports())
	if _, err := w.PutMemory(ctx, store.Memory{
		ID: "m-vector", Scope: store.ScopeUser, Kind: store.KindSemantic,
		ConceptID: "c-draft", Content: "向量索引 用 HNSW 构建", TValid: 1000, TCreated: 1000,
	}); err != nil {
		t.Fatalf("PutMemory: %v", err)
	}

	r := recall.New(d.Ports(), recall.Config{}, nil)
	// 精确查询 draft 概念名。旧行为：0.9 × 0.6 = 0.54 < θ 0.6 → insufficient。
	res, err := r.Recall(ctx, recall.RecallRequest{Query: "向量索引", Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if res.Status == recall.StatusInsufficient {
		t.Fatalf("draft 概念的精确查询被判 insufficient（StatusFactor 进了 coverage 闸）：missing=%+v", res.Missing)
	}
	if !containsID(itemIDs(res.Items), "m-vector") {
		t.Fatalf("draft 概念的记忆未被召回：items=%v", itemIDs(res.Items))
	}
	// 但 draft 仍被“打折”：StatusFactor < 1 必须留在 Match 里（不是被当成 certified）。
	var sawDraft bool
	for _, m := range res.Plan.Matched {
		if m.ConceptID == "c-draft" {
			sawDraft = true
			if m.StatusFactor >= 1 {
				t.Fatalf("draft 概念的 StatusFactor=%.2f，应 <1（打折而非等同 certified）", m.StatusFactor)
			}
			if m.Weight >= 1 {
				t.Fatalf("draft 的 Match.Weight=%.3f，应被 StatusFactor 打折（<1）", m.Weight)
			}
		}
	}
	if !sawDraft {
		t.Fatalf("Plan.Matched 未记录 draft 概念：%+v", res.Plan.Matched)
	}

	// F2b 的直接口径：coverage 不得含 StatusFactor。
	// draft 概念名精确命中时，coverage 应等于对齐质量（0.9），而不是 0.9×0.6=0.54。
	// 若 StatusFactor 重新进入 coverage，本条断言会立即变红（变异验证点）。
	if res.Plan.Coverage < r.Config().ThetaCoverage {
		t.Fatalf("draft 概念精确查询的 coverage=%.3f < θ=%.2f：StatusFactor 又被算进了 coverage（F2b）",
			res.Plan.Coverage, r.Config().ThetaCoverage)
	}
	if res.Plan.Coverage < 0.9-1e-9 {
		t.Fatalf("draft 概念精确查询的 coverage=%.3f, want 0.9（= ConceptNameQuality，未乘 StatusFactor）",
			res.Plan.Coverage)
	}
}

// TestFixD14InsufficientIsNotShortCircuit 是 B-2 的回归（D14）。
//
// 旧行为：align 之后先判 coverage 就 return insufficient，于是词法通道能查到的记忆
// 也被弃答（“本体越丰富越容易失败”）。
//
// 变异验证：把 Recall 改回“lane 之前先判 coverage/RequiredMissing 并 return”→ 本用例红。
func TestFixD14InsufficientIsNotShortCircuit(t *testing.T) {
	d := openMemory(t)
	ctx := context.Background()
	// 本体里有"编辑器"，但**没有**"超时"这个维度 → coverage 一定不达标。
	putConcept(t, d.Store(), "c-editor", "编辑器", store.ConceptCertified, "用于编写代码的文本编辑程序")

	w := recall.NewWriter(d.Ports())
	if _, err := w.PutMemory(ctx, store.Memory{
		ID: "m-timeout", Scope: store.ScopeUser, Kind: store.KindSemantic,
		Content: "编辑器 超时设为 30s", TValid: 1000, TCreated: 1000,
	}); err != nil {
		t.Fatalf("PutMemory: %v", err)
	}

	r := recall.New(d.Ports(), recall.Config{}, nil)
	res, err := r.Recall(ctx, recall.RecallRequest{Query: "编辑器 超时", Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if res.Status == recall.StatusInsufficient {
		t.Fatalf("词法能答却返回 insufficient（D14 被违反）：missing=%+v lanes=%+v",
			res.Missing, res.Plan.Lanes)
	}
	if !containsID(itemIDs(res.Items), "m-timeout") {
		t.Fatalf("「编辑器 超时」未召回 m-timeout：items=%v", itemIDs(res.Items))
	}
	// plan 必须解释“为什么不判 insufficient”（R5）。
	if !hasStep(res.Plan.Steps, "fuse") && !hasStep(res.Plan.Steps, "low_coverage") {
		t.Fatalf("Plan.Steps 未记录融合/低覆盖解释：%+v", res.Plan.Steps)
	}
}

// TestFixD14StillInsufficientOnTrueZeroEvidence 是 D14 的**反向**守护：
// 问库里完全没记过的维度仍必须返回 insufficient + Missing（不能因为放宽而永远答）。
func TestFixD14StillInsufficientOnTrueZeroEvidence(t *testing.T) {
	d := openMemory(t)
	ctx := context.Background()
	putConcept(t, d.Store(), "c-editor", "编辑器", store.ConceptCertified, "用于编写代码的文本编辑程序")

	w := recall.NewWriter(d.Ports())
	if _, err := w.PutMemory(ctx, store.Memory{
		ID: "m-editor", Scope: store.ScopeUser, Kind: store.KindSemantic,
		ConceptID: "c-editor", Content: "默认编辑器是 vim", TValid: 1000, TCreated: 1000,
	}); err != nil {
		t.Fatalf("PutMemory: %v", err)
	}

	r := recall.New(d.Ports(), recall.Config{}, nil)
	res, err := r.Recall(ctx, recall.RecallRequest{Query: "生产环境的部署环境 是什么", Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if res.Status != recall.StatusInsufficient {
		t.Fatalf("零证据时 status=%s, want insufficient", res.Status)
	}
	if len(res.Items) != 0 {
		t.Fatalf("insufficient 不得返回低分硬答：items=%v", itemIDs(res.Items))
	}
	if len(res.Missing) == 0 {
		t.Fatalf("Missing 为空：R4 要求结构化说明缺什么")
	}
	var named bool
	for _, m := range res.Missing {
		if m.Text == "部署环境" {
			named = true
			if !m.Required {
				t.Fatalf("真零证据时该维度应标 Required=true：%+v", m)
			}
			if m.Reason == "" || m.Suggestion == "" {
				t.Fatalf("Missing 项缺 Reason/Suggestion：%+v", m)
			}
		}
	}
	if !named {
		t.Fatalf("Missing 未点名缺失维度「部署环境」：%+v", res.Missing)
	}
}

// TestFixF3RelationalHasLexicalFallback 是 F3 的回归。
//
// 旧行为：ShapeRelational 的 lane 表只有 [graph, vector]。graph 因无锚点跳过、
// vector 因无 embedder 跳过 → 融合 0 路通道 → insufficient，而词法本来有命中。
//
// 变异验证：从 relational 的 lane 表里删掉 lexical → 本用例红。
func TestFixF3RelationalHasLexicalFallback(t *testing.T) {
	d := openMemory(t)
	ctx := context.Background()
	// 本体非空但与问句无关（让 coverage 可判定、且不足以答出）。
	putConcept(t, d.Store(), "c-editor", "编辑器", store.ConceptCertified, "用于编写代码的文本编辑程序")

	w := recall.NewWriter(d.Ports())
	if _, err := w.PutMemory(ctx, store.Memory{
		ID: "m-auth", Scope: store.ScopeUser, Kind: store.KindDecision,
		Content: "认证服务从 session 换成 JWT", TValid: 1000, TCreated: 1000,
	}); err != nil {
		t.Fatalf("PutMemory: %v", err)
	}

	const query = "为什么换成 JWT"
	r := recall.New(d.Ports(), recall.Config{}, nil) // 无 embedder
	res, err := r.Recall(ctx, recall.RecallRequest{Query: query, Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}

	lp, ok := laneOf(res.Plan, recall.LaneLexical)
	if !ok {
		t.Fatalf("relational 的 lane 表里没有 lexical（F3）：%+v", res.Plan.Lanes)
	}
	if !lp.Selected {
		t.Fatalf("lexical 未被选中：%+v", lp)
	}
	// 关键区分：lexical 必须来自**声明式 lane 规则表的 relational 行**，
	// 而不是“全 lane 被跳过后的 fallback”。两者都能让结果答出，但只有前者是 F3 的修复；
	// 后者是 F3 的不变量兼底（另一个用例覆盖）。变异验证点就在这里。
	if strings.Contains(lp.Reason, "fallback") {
		t.Fatalf("lexical 是 fallback 追加的，不是 relational 规则表自带的（F3 未真正修复）：%+v", lp)
	}
	rule, hasRule := laneRuleFor(recall.ShapeRelational)
	if !hasRule {
		t.Fatalf("声明式表里没有 relational 行")
	}
	var inTable bool
	for _, lw := range rule.Lanes {
		if lw.Lane == recall.LaneLexical {
			inTable = true
			if math.Abs(lp.Weight-lw.Weight) > 1e-9 {
				t.Fatalf("lexical weight=%.2f, 表=%.2f（权重必须查表）", lp.Weight, lw.Weight)
			}
		}
	}
	if !inTable {
		t.Fatalf("relational 的声明式 lane 表不含 lexical（F3）：%+v", rule.Lanes)
	}
	if res.Status == recall.StatusInsufficient {
		t.Fatalf("返回 insufficient（词法本可答出）：missing=%+v lanes=%+v", res.Missing, res.Plan.Lanes)
	}
	if !containsID(itemIDs(res.Items), "m-auth") {
		t.Fatalf("未召回 m-auth：items=%v", itemIDs(res.Items))
	}
}

// TestFixF3InvariantFallbackToLexical 覆盖 F3 的不变量：
// 所有选中 lane 都被跳过时，必须降级到 lexical，而不是直接 insufficient。
//
// 变异验证：删掉 plan.go 里 hasExecutableLane 的 fallback 分支 → 本用例红。
func TestFixF3InvariantFallbackToLexical(t *testing.T) {
	d := openMemory(t)
	ctx := context.Background()
	putConcept(t, d.Store(), "c-editor", "编辑器", store.ConceptCertified, "用于编写代码的文本编辑程序")

	w := recall.NewWriter(d.Ports())
	if _, err := w.PutMemory(ctx, store.Memory{
		ID: "m-cfg", Scope: store.ScopeUser, Kind: store.KindSemantic,
		Content: "编辑器 主题配色 是 solarized", TValid: 1000, TCreated: 1000,
	}); err != nil {
		t.Fatalf("PutMemory: %v", err)
	}

	r := recall.New(d.Ports(), recall.Config{}, nil)
	// 强制只要 vector：没有 embedder → 唯一选中的 lane 被跳过 → 必须降级到 lexical。
	res, err := r.Recall(ctx, recall.RecallRequest{
		Query: "编辑器 主题配色", Scope: store.ScopeUser,
		Lanes: []recall.LaneName{recall.LaneVector},
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	details := stepDetails(res.Plan.Steps)
	if !strings.Contains(details, "fallback") && !strings.Contains(details, "兼底") {
		t.Fatalf("未记录 lexical fallback：%+v", res.Plan.Steps)
	}
	if !containsID(itemIDs(res.Items), "m-cfg") {
		t.Fatalf("全 lane 被跳过后未降级到 lexical：status=%s items=%v",
			res.Status, itemIDs(res.Items))
	}
}

// TestFixF4MaskedCueKeepsRelation 是 B2（test）的回归。
//
// 旧行为：关系线索与已接受概念槽重叠时整条丢弃 → 「Alice 在哪个公司工作」的
// works_at 线索被概念名「公司」吃掉 → 退化为 3 跳盲扩展且答不出，
// 而同义的「Alice 在哪工作」1 跳就能答出。
//
// 变异验证：把 align.go 的重叠分支改回“整条丢弃”（不收集 overlapHints）→ 本用例红。
func TestFixF4MaskedCueKeepsRelation(t *testing.T) {
	d := openMemory(t)
	ctx := context.Background()
	putConcept(t, d.Store(), "c-company", "公司", store.ConceptCertified, "雇佣员工的组织实体")
	putNode(t, d.Graph(), "n-alice", "Alice", store.ScopeUser, "", "")
	putNode(t, d.Graph(), "n-acme", "Acme", store.ScopeUser, "c-company", "")
	putEdge(t, d.Graph(), "e-w", "n-alice", "n-acme", store.RelationWorksAt, 1000, 1000)
	putAlias(t, d.Store(), "Alice", "n-alice", 1.0)

	w := recall.NewWriter(d.Ports())
	// 内容与两个问句都无 bigram 重合 → 只有 graph lane 可能答对。
	if _, err := w.PutMemory(ctx, store.Memory{
		ID: "m-acme", Scope: store.ScopeUser, Kind: store.KindSemantic, ConceptID: "c-company",
		Content: "Acme 雇佣", TValid: 1000, TCreated: 1000,
	}); err != nil {
		t.Fatalf("PutMemory: %v", err)
	}

	r := recall.New(d.Ports(), recall.Config{}, nil)
	const masked = "Alice 在哪个公司工作"
	const plain = "Alice 在哪工作"

	resMasked, err := r.Recall(ctx, recall.RecallRequest{Query: masked, Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("Recall(%q): %v", masked, err)
	}
	resPlain, err := r.Recall(ctx, recall.RecallRequest{Query: plain, Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("Recall(%q): %v", plain, err)
	}

	// 两种问法都必须识别出 works_at 且 1 跳。
	for _, tc := range []struct {
		q  string
		rs recall.RecallResult
	}{
		{masked, resMasked}, {plain, resPlain},
	} {
		rp, ok := relationPlanOf(tc.rs.Plan, store.RelationWorksAt)
		if !ok {
			t.Fatalf("%q 的 plan 未包含 works_at：%+v", tc.q, tc.rs.Plan.Relations)
		}
		if rp.MaxHops != 1 {
			t.Fatalf("%q 的 works_at 跳数=%d, want 1", tc.q, rp.MaxHops)
		}
		if !containsID(itemIDs(tc.rs.Items), "m-acme") {
			t.Fatalf("%q 未召回 m-acme：items=%v", tc.q, itemIDs(tc.rs.Items))
		}
	}
	// 行为一致：两个问法都答得出（不再 1 跳 vs 3 跳分叉）。
	if resMasked.Plan.Hops != resPlain.Plan.Hops {
		t.Fatalf("两种问法跳数不一致：masked=%d plain=%d",
			resMasked.Plan.Hops, resPlain.Plan.Hops)
	}
}

// TestFixF6ExecutedMeansReallyRan 是 F1（Explain 失真）的回归。
//
// 历史缺陷（两个条件叠加才成立）：R4 在 lane 之前短路 + plan.go 计划期就置
// Executed=true。于是 insufficient 的 plan 声称“lanes 执行过了”，但事实上一路
// 通道都没调过（没有 fuse 步骤）。
//
// 本用例的可观测判据：任何 Executed=true 的 lane 都必须有“真的跑过”的痕迹。
// 现在 fuse 步骤恒被追加，因此“有 Executed 却无 fuse”就等价于“声称跑了但没跑”。
//
// 变异验证：恢复“计划期置 Executed + lane 前短路”→ 本用例红。
func TestFixF6ExecutedMeansReallyRan(t *testing.T) {
	d := openMemory(t)
	ctx := context.Background()
	putConcept(t, d.Store(), "c-editor", "编辑器", store.ConceptCertified, "用于编写代码的文本编辑程序")

	r := recall.New(d.Ports(), recall.Config{}, nil)
	// 关键：这条 query 的维度在库里**完全没记过**（只有概念“编辑器”存在，本体因此可判定），
	// 于是 coverage=0 < θ —— 这正是旧 R4 先验短路会触发的场景。
	// 搭配“计划期就置 Executed”才能复现 F1：声称跑过、实际一路通道都没调。
	// （若用一个高覆盖 query，短路根本不会触发，用例就抓不住该变异。）
	res, err := r.Recall(ctx, recall.RecallRequest{Query: "生产环境的部署环境 是什么", Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if res.Status != recall.StatusInsufficient {
		t.Fatalf("空库查询 status=%s, want insufficient", res.Status)
	}

	ran := hasStep(res.Plan.Steps, "fuse")
	var executed []recall.LaneName
	for _, lp := range res.Plan.Lanes {
		if lp.Executed {
			executed = append(executed, lp.Lane)
		}
	}
	// 核心断言：声称执行过，就必须有融合步骤作为“真的跑了”的证据。
	if len(executed) > 0 && !ran {
		t.Fatalf("lane %v 标了 Executed=true，但没有任何 fuse 步骤（声称跑了却没跑）：%+v",
			executed, res.Plan.Steps)
	}
	// 未执行且被选中的 lane 必须解释原因（不得静默）。
	for _, lp := range res.Plan.Lanes {
		if lp.Selected && !lp.Executed && lp.SkipReason == "" {
			t.Fatalf("lane %s 未执行、无 SkipReason，也无执行记录（Explain 失真）：%+v", lp.Lane, lp)
		}
		// 未被选中的 lane 一律不得声称执行。
		if !lp.Selected && lp.Executed {
			t.Fatalf("lane %s 未被选中却标了 Executed：%+v", lp.Lane, lp)
		}
	}
}

// TestFixF8NonTransitiveRelationCannotChain 是 F8 的回归：Transitive 必须真正控制遍历。
//
// 历史缺陷：Transitive 只在 plan.go 被复制进 plan，从不参与跳数判定 —— 一个纯粹的
// 装饰字段。于是把非传递关系的 MaxHops 配成 3（或从表里漏掉跳数限制）时，
// works_at 会连走两条，到“别人公司”。
//
// 本用例用自定义 Config 把 works_at 宣示为非传递但 MaxHops=3：规划必须把跳数夹到 1。
// 变异验证：删掉 relations() 里的钳制分支 → 本用例红（MaxHops=3）。
func TestFixF8NonTransitiveRelationCannotChain(t *testing.T) {
	d := openMemory(t)
	ctx := context.Background()

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

	// 故意把 works_at 配成“非传递 + 3 跳”：Transitive 必须比 MaxHops 权威。
	cfg := recall.Config{RelationRules: []recall.RelationRule{
		{Relation: store.RelationWorksAt, Transitive: false, MaxHops: 3, Weight: 0.7},
	}}
	r := recall.New(d.Ports(), cfg, nil)
	res, err := r.Recall(ctx, recall.RecallRequest{Query: "Alice 就职 单位", Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}

	rp, ok := relationPlanOf(res.Plan, store.RelationWorksAt)
	if !ok {
		t.Fatalf("Plan.Relations 未包含 works_at：%+v", res.Plan.Relations)
	}
	if rp.Transitive {
		t.Fatalf("配置里 works_at 是非传递，plan 却标成传递：%+v", rp)
	}
	if rp.MaxHops != 1 {
		t.Fatalf("非传递关系的 MaxHops=%d，应被夹到 1（Transitive 必须真正生效，不是装饰字段）",
			rp.MaxHops)
	}
	if res.Plan.Hops != 1 {
		t.Fatalf("Plan.Hops=%d, want 1", res.Plan.Hops)
	}

	// 结果集：graph 证据不得超过 1 跳（m-c/m-d 可能因词法被召回，但绝不能带图证据）。
	for _, it := range res.Items {
		for _, ev := range it.GraphEvidence {
			if ev.Depth > 1 {
				t.Fatalf("%s 的图证据 depth=%d > 1：非传递关系被连走了两条：%+v",
					it.Memory.ID, ev.Depth, ev)
			}
		}
	}
}

// TestFixS2WriterIndexesVectors：Writer 必须编排 Vector（S-2），且不静默丢掉。
//
// 变异验证：删掉 writer.go 里的 Vector.Upsert 调用 → 本用例红（向量查不到）。
func TestFixS2WriterIndexesVectors(t *testing.T) {
	d := openMemory(t)
	ctx := context.Background()
	w := recall.NewWriter(d.Ports())

	res, err := w.Put(ctx, recall.WriteBundle{
		Memory: store.Memory{
			ID: "m-vec", Scope: store.ScopeUser, Kind: store.KindSemantic,
			Content: "向量写入编排", TValid: 1000, TCreated: 1000,
		},
		Vectors: []store.Vector{{ID: "m-vec", Scope: store.ScopeUser, Values: []float32{1, 0, 0}}},
	})
	if err != nil {
		t.Fatalf("带向量的写入必须成功：%v", err)
	}
	if !res.VectorIndexed {
		t.Fatalf("PutResult.VectorIndexed=false：向量未被编排（S-2）")
	}
	hits, err := d.Vector().Search(ctx, []float32{1, 0, 0}, store.ScopeUser, 5)
	if err != nil {
		t.Fatalf("Vector.Search: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != "m-vec" {
		t.Fatalf("向量索引里查不到 m-vec：%+v", hits)
	}

	// 带向量但 vector port 为 nil：必须报错，不得静默丢弃。
	noVec := recall.NewWriter(store.Ports{Store: d.Store(), Lexical: d.Lexical()})
	if _, err := noVec.Put(ctx, recall.WriteBundle{
		Memory: store.Memory{
			ID: "m-vec2", Scope: store.ScopeUser, Kind: store.KindSemantic,
			Content: "无向量端口", TValid: 1000, TCreated: 1000,
		},
		Vectors: []store.Vector{{ID: "m-vec2", Scope: store.ScopeUser, Values: []float32{1}}},
	}); err == nil {
		t.Fatalf("带向量但 vector port 为 nil 时必须报错（不得静默丢通道）")
	}
	if _, gerr := d.Store().GetMemory(ctx, "m-vec2"); gerr == nil {
		t.Fatalf("失败写入留下了记忆（未回滚）")
	}
}

// TestFixS4EmptyScopeRejected：Scope 是必填隔离边界，空值不得退化成“不限”（S-4）。
//
// 旧行为：空 Scope 被 matchesFilter 解释成“不限”，实测出 user→agent 的跨 scope 泄漏。
//
// 变异验证：删掉 recall.go 的 Scope.Valid() 检查 → 本用例红。
func TestFixS4EmptyScopeRejected(t *testing.T) {
	d := openMemory(t)
	ctx := context.Background()
	w := recall.NewWriter(d.Ports())
	for _, m := range []struct {
		id    string
		scope store.Scope
	}{
		{"m-user", store.ScopeUser},
		{"m-agent", store.ScopeAgent},
	} {
		if _, err := w.PutMemory(ctx, store.Memory{
			ID: m.id, Scope: m.scope, Kind: store.KindSemantic,
			Content: "认证方式 JWT", TValid: 1000, TCreated: 1000,
		}); err != nil {
			t.Fatalf("PutMemory: %v", err)
		}
	}

	r := recall.New(d.Ports(), recall.Config{}, nil)
	res, err := r.Recall(ctx, recall.RecallRequest{Query: "认证方式 JWT"}) // Scope 为空
	if err == nil {
		t.Fatalf("空 Scope 必须报错（types.go 与 ports.go 都把空 Scope 当调用方缺陷），"+
			"实际 status=%s items=%v", res.Status, itemIDs(res.Items))
	}
	if !strings.Contains(err.Error(), "scope") {
		t.Fatalf("错误信息应点明 scope 问题：%v", err)
	}
}

// stepDetails 把所有 step 的 Step+Detail 拼成一段，便于做包含性断言。
func stepDetails(steps []recall.PlanStep) string {
	var b strings.Builder
	for _, s := range steps {
		b.WriteString(s.Step)
		b.WriteString(" ")
		b.WriteString(s.Detail)
		b.WriteString("\n")
	}
	return b.String()
}
