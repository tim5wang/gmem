package recall_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tim5wang/gmem/recall"
	"github.com/tim5wang/gmem/sqlite"
	"github.com/tim5wang/gmem/store"
)

// 本文件是**跨包集成测试**：只用 store 的四个 Port + recall 的公开 API 观察行为，
// 不 import 任何内部实现细节。它覆盖 P1 的四条硬要求：
//  1. 写入编排（Writer）后词法通道必然可检索，且索引版本 = 写入版本；
//  2. 关系型问句在无 embedder 时靠 graph lane 能答出；
//  3. 覆盖率不足返回 insufficient + 有意义的 Missing；
//  4. Explain 的 Plan 内容可断言（lane/跳数/贡献/分数分解）。

func openDB(t *testing.T) *sqlite.DB {
	t.Helper()
	d, err := sqlite.Open(context.Background(), sqlite.Config{})
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return d
}

func newRecaller(d *sqlite.DB, emb recall.Embedder) *recall.Recaller {
	return recall.New(d.Ports(), recall.Config{}, emb)
}

// seedOntology 准备"Alice 用 vim"这一组本体与图元素。
//
// 顺序不能反：nodes.concept_id 与 memories.concept_id 都有外键到 concepts，
// 因此概念必须先落库。
func seedOntology(t *testing.T, d *sqlite.DB) {
	t.Helper()
	ctx := context.Background()
	if err := d.Store().PutConcept(ctx, store.Concept{
		ID: "c-editor", Name: "编辑器", Status: store.ConceptCertified,
		Definition: "用于编写代码的文本编辑程序",
	}); err != nil {
		t.Fatalf("PutConcept: %v", err)
	}
	for _, n := range []store.Node{
		{ID: "n-alice", Scope: store.ScopeUser, CanonicalName: "Alice"},
		{ID: "n-vim", Scope: store.ScopeUser, CanonicalName: "vim", ConceptID: "c-editor"},
	} {
		if err := d.Graph().PutNode(ctx, n); err != nil {
			t.Fatalf("PutNode(%s): %v", n.ID, err)
		}
	}
	if err := d.Graph().PutEdge(ctx, store.Edge{
		ID: "e-alice-vim", FromID: "n-alice", ToID: "n-vim",
		Relation: store.RelationUses, Weight: 1, TValid: 1000, TCreated: 1000,
	}); err != nil {
		t.Fatalf("PutEdge: %v", err)
	}
	if err := d.Store().PutAlias(ctx, store.Alias{
		Surface: "Alice", NodeID: "n-alice", Source: "dict", Confidence: 1.0,
	}); err != nil {
		t.Fatalf("PutAlias: %v", err)
	}
}

// ===========================================================================
// 1. 写入编排（P1 最关键的一条）
// ===========================================================================

// TestWriterMakesLexicalSearchable 证明"走写入入口后词汇通道必然可检索"。
//
// 变异验证（见 P1-NOTES.md）：把 Writer.Put 里的 Lexical.Index 调用删掉，
// 本用例立刻变红（Search 返回 0 命中）——这正是 D13 遗留的静默漏召回缺陷形态。
func TestWriterMakesLexicalSearchable(t *testing.T) {
	d := openDB(t)
	ctx := context.Background()
	w := recall.NewWriter(d.Ports())

	res, err := w.PutMemory(ctx, store.Memory{
		ID: "m-auth", Scope: store.ScopeUser, Kind: store.KindDecision,
		Content: "认证服务从 session 换成 JWT", TValid: 1000, TCreated: 1000,
	})
	if err != nil {
		t.Fatalf("Writer.PutMemory: %v", err)
	}
	if !res.LexicalIndexed {
		t.Fatalf("LexicalIndexed=false：写入入口必须同步词法索引")
	}
	if res.Version != 1 {
		t.Fatalf("Version=%d, want 1", res.Version)
	}

	hits, err := d.Lexical().Search(ctx, store.LexicalQuery{Query: "JWT", Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("Lexical.Search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatalf("走写入入口后词法通道查不到（静默漏召回缺陷）：hits=0")
	}
	if hits[0].ID != "m-auth" || hits[0].Version != 1 {
		t.Fatalf("hit=%+v, want m-auth v1", hits[0])
	}

	// 中文双侧对称同样成立（bigram 索引侧由后端负责）。
	hits2, err := d.Lexical().Search(ctx, store.LexicalQuery{Query: "认证", Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("Lexical.Search(认证): %v", err)
	}
	if len(hits2) == 0 {
		t.Fatalf("中文 bigram 查不到：hits=0")
	}
}

// TestWriterIndexesActualVersionPerRewrite 证明写入入口传的是**真实版本号**
// 而不是 0 或写死 1（D13 的 (id,version) 索引），并且旧版本索引不被覆盖。
//
// 变异验证：把 Writer 里的 Document.Version 改成写死 1，
// 第二个断言（as_of 回溯命中 v1 且是旧内容）仍然通过，但 v2 的 as_of 查询会
// 命中错误版本 → 用例在 "v2 查 JWT" 一步变红。见 P1-NOTES.md 的变异表。
func TestWriterIndexesActualVersionPerRewrite(t *testing.T) {
	d := openDB(t)
	ctx := context.Background()
	w := recall.NewWriter(d.Ports())

	if _, err := w.PutMemory(ctx, store.Memory{
		ID: "m-scheme", Scope: store.ScopeUser, Kind: store.KindDecision,
		Content: "认证用 session", TValid: 1000, TCreated: 1000,
	}); err != nil {
		t.Fatalf("first put: %v", err)
	}
	res2, err := w.PutMemory(ctx, store.Memory{
		ID: "m-scheme", Scope: store.ScopeUser, Kind: store.KindDecision,
		Content: "认证用 JWT", TValid: 1000, TCreated: 2000,
	})
	if err != nil {
		t.Fatalf("second put: %v", err)
	}
	if res2.Version != 2 {
		t.Fatalf("第二次写入 Version=%d, want 2（内容变化必须产生新版本）", res2.Version)
	}

	// as_of=1500：当时只有 v1，且 v1 的内容是 session。
	asOf := int64(1500)
	hits, err := d.Lexical().Search(ctx, store.LexicalQuery{Query: "session", Scope: store.ScopeUser, AsOf: &asOf})
	if err != nil {
		t.Fatalf("as_of search: %v", err)
	}
	if len(hits) != 1 || hits[0].Version != 1 {
		t.Fatalf("as_of=1500 查 session: hits=%+v, want 单条 v1（旧版本索引必须保留）", hits)
	}

	// 同样 as_of=1500 查 JWT 必须 0 命中：v2 的文本不能泄漏到过去。
	jwtHits, err := d.Lexical().Search(ctx, store.LexicalQuery{Query: "JWT", Scope: store.ScopeUser, AsOf: &asOf})
	if err != nil {
		t.Fatalf("as_of search JWT: %v", err)
	}
	if len(jwtHits) != 0 {
		t.Fatalf("as_of=1500 查 JWT: hits=%+v, want 0（新版本内容不得出现在过去）", jwtHits)
	}

	// 当前（as_of=nil）：只应命中的是 v2。
	cur, err := d.Lexical().Search(ctx, store.LexicalQuery{Query: "JWT", Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("current search: %v", err)
	}
	if len(cur) != 1 || cur[0].Version != 2 {
		t.Fatalf("当前查 JWT: hits=%+v, want 单条 v2（版本号必须真实传入 Index）", cur)
	}
}

// TestWriterRollsBackBothSides 证明主表与词法索引在同一事务里：
// 图写入失败时，记忆与索引都不得留下。
func TestWriterRollsBackBothSides(t *testing.T) {
	d := openDB(t)
	ctx := context.Background()
	// 只给 Store+Lexical，不给 Graph：带节点的 bundle 必须报错。
	w := recall.NewWriter(store.Ports{Store: d.Store(), Lexical: d.Lexical()})

	_, err := w.Put(ctx, recall.WriteBundle{
		Memory: store.Memory{
			ID: "m-rollback", Scope: store.ScopeUser, Kind: store.KindSemantic,
			Content: "这条不该活下来", TValid: 1000, TCreated: 1000,
		},
		Nodes: []store.Node{{ID: "n-x", Scope: store.ScopeUser, CanonicalName: "x"}},
	})
	if err == nil {
		t.Fatalf("带节点但 graph port 为 nil，必须报错而不是静默丢图")
	}

	if _, err := d.Store().GetMemory(ctx, "m-rollback"); err == nil {
		t.Fatalf("失败的写入留下了记忆记录：事务未回滚")
	}
	hits, err := d.Lexical().Search(ctx, store.LexicalQuery{Query: "不该活下来", Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("失败的写入留下了词法索引：hits=%+v", hits)
	}
}

// TestWriterWithInlineGraph 证明"InlineGraph"路径：一次写入同时成图。
func TestWriterWithInlineGraph(t *testing.T) {
	d := openDB(t)
	ctx := context.Background()
	// m-vim 带 concept_id=c-editor，必须先有该概念，否则外键约束会拒绝写入。
	if err := d.Store().PutConcept(ctx, store.Concept{
		ID: "c-editor", Name: "编辑器", Status: store.ConceptCertified,
		Definition: "用于编写代码的文本编辑程序",
	}); err != nil {
		t.Fatalf("PutConcept: %v", err)
	}
	w := recall.NewWriter(d.Ports())

	res, err := w.Put(ctx, recall.WriteBundle{
		Memory: store.Memory{
			ID: "m-vim", Scope: store.ScopeUser, Kind: store.KindSemantic,
			ConceptID: "c-editor", Content: "Alice 用 vim 写 Go", TValid: 1000, TCreated: 1000,
		},
		Nodes: []store.Node{
			{ID: "n-alice", Scope: store.ScopeUser, CanonicalName: "Alice"},
			{ID: "n-vim", Scope: store.ScopeUser, CanonicalName: "vim", ConceptID: "c-editor"},
		},
		Edges: []store.Edge{{
			ID: "e-alice-vim", FromID: "n-alice", ToID: "n-vim",
			Relation: store.RelationUses, Weight: 1, TValid: 1000, TCreated: 1000,
		}},
	})
	if err != nil {
		t.Fatalf("Writer.Put: %v", err)
	}
	if res.NodeCount != 2 || res.EdgeCount != 1 {
		t.Fatalf("res=%+v, want 2 nodes / 1 edge", res)
	}

	hits, err := d.Graph().Neighbors(ctx, store.NeighborQuery{
		NodeID: "n-alice", Scope: store.ScopeUser,
		Relations: []store.Relation{store.RelationUses}, Hops: 1,
	})
	if err != nil {
		t.Fatalf("Neighbors: %v", err)
	}
	if len(hits) != 1 || hits[0].Edge.ID != "e-alice-vim" {
		t.Fatalf("图未落地：%+v", hits)
	}
}

// ===========================================================================
// 2. 关系型问句：无 embedder 时靠 graph lane 答出
// ===========================================================================

// TestRelationalQuestionAnsweredByGraphLane 是 P1 的硬要求：
// "Alice 用什么" 在**完全没有 embedder** 的情况下必须由 graph lane 答出。
func TestRelationalQuestionAnsweredByGraphLane(t *testing.T) {
	d := openDB(t)
	ctx := context.Background()
	seedOntology(t, d)

	w := recall.NewWriter(d.Ports())
	if _, err := w.PutMemory(ctx, store.Memory{
		ID: "m-editor", Scope: store.ScopeUser, Kind: store.KindSemantic,
		ConceptID: "c-editor", Content: "Alice 的默认编辑器是 vim", TValid: 1000, TCreated: 1000,
	}); err != nil {
		t.Fatalf("PutMemory: %v", err)
	}

	r := newRecaller(d, nil) // 明确不注入 embedder
	res, err := r.Recall(ctx, recall.RecallRequest{Query: "Alice 用什么 编辑器", Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}

	if res.Status == recall.StatusInsufficient {
		t.Fatalf("status=insufficient，关系型问句应能靠 graph lane 答出：missing=%+v", res.Missing)
	}
	if len(res.Items) == 0 {
		t.Fatalf("无 embedder 时 graph lane 未召回任何记忆：plan=%+v", res.Plan)
	}
	if res.Items[0].Memory.ID != "m-editor" {
		t.Fatalf("top1=%q, want m-editor", res.Items[0].Memory.ID)
	}

	// graph lane 必须真的被执行，并且给出可解释的证据。
	var graphExecuted bool
	for _, lp := range res.Plan.Lanes {
		if lp.Lane == recall.LaneGraph && lp.Executed {
			graphExecuted = true
		}
	}
	if !graphExecuted {
		t.Fatalf("graph lane 未执行：lanes=%+v", res.Plan.Lanes)
	}
	if len(res.Items[0].GraphEvidence) == 0 {
		t.Fatalf("graph lane 召回的条目没有 GraphEvidence，无法解释'凭什么'")
	}
	ev := res.Items[0].GraphEvidence[0]
	if ev.Relation != store.RelationUses || ev.EdgeID != "e-alice-vim" {
		t.Fatalf("evidence=%+v, want uses/e-alice-vim", ev)
	}

	// 无 embedder 必须在 Degraded 里显式报告，不得静默。
	if !containsSubstr(res.Degraded, "vector lane skipped") {
		t.Fatalf("Degraded=%v，必须显式报告 vector lane 因无 embedder 而缺席", res.Degraded)
	}
	if res.Status != recall.StatusDegraded {
		t.Fatalf("status=%s, want degraded（有通道降级时必须上报）", res.Status)
	}
}

// ===========================================================================
// 3. insufficient：覆盖不足返回"缺什么"
// ===========================================================================

// TestInsufficientForMissingDimension 覆盖"问一个库里完全没有的维度"。
func TestInsufficientForMissingDimension(t *testing.T) {
	d := openDB(t)
	ctx := context.Background()
	seedOntology(t, d)

	w := recall.NewWriter(d.Ports())
	if _, err := w.PutMemory(ctx, store.Memory{
		ID: "m-editor", Scope: store.ScopeUser, Kind: store.KindSemantic,
		ConceptID: "c-editor", Content: "Alice 的默认编辑器是 vim", TValid: 1000, TCreated: 1000,
	}); err != nil {
		t.Fatalf("PutMemory: %v", err)
	}

	r := newRecaller(d, nil)
	res, err := r.Recall(ctx, recall.RecallRequest{Query: "生产环境的部署环境 是什么", Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if res.Status != recall.StatusInsufficient {
		t.Fatalf("status=%s, want insufficient（库里没有这个维度）", res.Status)
	}
	// R4：不返回低分硬答。
	if len(res.Items) != 0 {
		t.Fatalf("insufficient 时不应返回低分硬答条目：items=%d", len(res.Items))
	}
	if len(res.Missing) == 0 {
		t.Fatalf("Missing 为空：必须结构化说明缺什么")
	}
	var found bool
	for _, m := range res.Missing {
		if strings.Contains(m.Text, "部署环境") {
			found = true
			if m.Reason == "" || m.Suggestion == "" {
				t.Fatalf("Missing 描述不完整：%+v", m)
			}
			if !m.Required {
				t.Fatalf("完全未对齐的维度应标记 Required=true：%+v", m)
			}
		}
	}
	if !found {
		t.Fatalf("Missing 未包含缺失槽位「部署环境」：%+v", res.Missing)
	}
	// plan 必须解释为什么 insufficient（R5）。
	if res.Plan == nil {
		t.Fatalf("insufficient 时 Plan 也必须非空（R5）")
	}
	if res.Plan.ThetaCoverage <= 0 {
		t.Fatalf("Plan.ThetaCoverage=%.2f，未记录 θ_cov", res.Plan.ThetaCoverage)
	}
	if !hasStep(res.Plan.Steps, "insufficient") {
		t.Fatalf("Plan.Steps 未记录 insufficient 判定：%+v", res.Plan.Steps)
	}
}

// TestInsufficientWhenNoChannelHits 覆盖"槽位都对齐了但库是空的"。
func TestInsufficientWhenNoChannelHits(t *testing.T) {
	d := openDB(t)
	ctx := context.Background()
	seedOntology(t, d)

	r := newRecaller(d, nil)
	res, err := r.Recall(ctx, recall.RecallRequest{Query: "Alice 用什么 编辑器", Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if res.Status != recall.StatusInsufficient {
		t.Fatalf("status=%s, want insufficient（对齐成功但没有任何通道命中）", res.Status)
	}
	if len(res.Missing) == 0 {
		t.Fatalf("Missing 为空：必须说明为什么没有结果")
	}
	if !strings.Contains(res.Missing[0].Reason, "没有任何通道召回结果") {
		t.Fatalf("Missing[0].Reason=%q，应说明通道为空", res.Missing[0].Reason)
	}
}

// ===========================================================================
// 4. Explain：Plan 内容可被断言（不只是非空）
// ===========================================================================

// TestPlanIsAssertable 断言 Plan 里确实有：选中的 lane、跳数、每 lane 贡献、分数分解。
func TestPlanIsAssertable(t *testing.T) {
	d := openDB(t)
	ctx := context.Background()
	seedOntology(t, d)
	w := recall.NewWriter(d.Ports())
	if _, err := w.PutMemory(ctx, store.Memory{
		ID: "m-editor", Scope: store.ScopeUser, Kind: store.KindSemantic,
		ConceptID: "c-editor", Content: "Alice 的默认编辑器是 vim", TValid: 1000, TCreated: 1000,
	}); err != nil {
		t.Fatalf("PutMemory: %v", err)
	}

	r := newRecaller(d, nil)
	res, err := r.Recall(ctx, recall.RecallRequest{Query: "Alice 用什么 编辑器", Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	plan := res.Plan
	if plan == nil {
		t.Fatal("Plan 为 nil")
	}

	// (a) 对齐了哪些概念 / 未对齐哪些
	if len(plan.Matched) == 0 {
		t.Fatalf("Plan.Matched 为空：必须能回答'对齐了哪些概念'")
	}
	var sawAliasMatch bool
	for _, m := range plan.Matched {
		if m.Method == recall.MatchAlias && m.NodeID == "n-alice" {
			sawAliasMatch = true
		}
	}
	if !sawAliasMatch {
		t.Fatalf("Plan.Matched 未记录 alias 对齐：%+v", plan.Matched)
	}

	// (b) lane 怎么选的：relational 形态 → graph 0.7 / vector 0.3
	if plan.Shape != recall.ShapeRelational {
		t.Fatalf("Shape=%s, want relational", plan.Shape)
	}
	wantWeights := map[recall.LaneName]float64{recall.LaneGraph: 0.7, recall.LaneVector: 0.3}
	if len(plan.Lanes) != len(wantWeights) {
		t.Fatalf("Lanes=%+v, want %d 条", plan.Lanes, len(wantWeights))
	}
	for _, lp := range plan.Lanes {
		if w, ok := wantWeights[lp.Lane]; !ok || w != lp.Weight {
			t.Fatalf("lane %s weight=%.2f, want %.2f（必须来自声明式表）", lp.Lane, lp.Weight, wantWeights[lp.Lane])
		}
		if lp.Reason == "" {
			t.Fatalf("lane %s 没有 Reason", lp.Lane)
		}
	}

	// (c) 跳数：关系型问句的 uses 必须 1 跳，且记录传递性来源
	if plan.Hops != 1 {
		t.Fatalf("Plan.Hops=%d, want 1（uses 白名单）", plan.Hops)
	}
	var sawRelation bool
	for _, rp := range plan.Relations {
		if rp.Relation == store.RelationUses {
			sawRelation = true
			if rp.MaxHops != 1 || rp.Transitive {
				t.Fatalf("uses 规则=%+v, want 1 hop / 非传递", rp)
			}
			if rp.Origin != "query-cue" {
				t.Fatalf("uses 规则来源=%q, want query-cue", rp.Origin)
			}
		}
	}
	if !sawRelation {
		t.Fatalf("Plan.Relations 未记录 uses：%+v", plan.Relations)
	}

	// (d) 每 lane 贡献 + 分数分解
	if len(res.Items) == 0 {
		t.Fatalf("无结果，无法断言分数分解")
	}
	it := res.Items[0]
	if len(it.Contributions) == 0 {
		t.Fatalf("RecallItem.Contributions 为空：无法回答'每条分数如何分解'")
	}
	for _, c := range it.Contributions {
		if c.Lane == "" || c.Rank <= 0 {
			t.Fatalf("贡献项不完整：%+v", c)
		}
	}
	// 乘性分解必须能复算出 Score。
	want := it.FusionScore * it.ConceptBoost * it.RecencyFactor
	if diff := want - it.Score; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("分数分解不自洽：fusion=%.6f×concept=%.2f×recency=%.3f=%.6f, Score=%.6f",
			it.FusionScore, it.ConceptBoost, it.RecencyFactor, want, it.Score)
	}
	if !it.ConceptConsistent || it.ConceptBoost <= 1 {
		t.Fatalf("concept_id=c-editor 命中对齐概念，应被一致性加权：boost=%.2f consistent=%v",
			it.ConceptBoost, it.ConceptConsistent)
	}
	// 两类调整都要在 Adjustments 里留痕。
	if len(it.Adjustments) < 2 {
		t.Fatalf("Adjustments=%+v, want 至少 concept_consistency + recency_decay", it.Adjustments)
	}
	kinds := map[string]bool{}
	for _, a := range it.Adjustments {
		kinds[a.Kind] = true
		if a.Reason == "" {
			t.Fatalf("Adjustment 缺 Reason：%+v", a)
		}
	}
	if !kinds["concept_consistency"] || !kinds["recency_decay"] {
		t.Fatalf("Adjustments 缺类别：%+v", kinds)
	}

	// (e) Plan.Steps 必须记录可复现的步骤
	if !hasStep(plan.Steps, "align") || !hasStep(plan.Steps, "relations") || !hasStep(plan.Steps, "fuse") {
		t.Fatalf("Plan.Steps 不完整：%+v", plan.Steps)
	}
	if !hasStepPrefix(plan.Steps, "score:") {
		t.Fatalf("Plan.Steps 未记录逐条分数分解：%+v", plan.Steps)
	}
}

// TestPlanRecordsDroppedItems 断言"哪些被拦截"可被读出。
func TestPlanRecordsDroppedItems(t *testing.T) {
	d := openDB(t)
	ctx := context.Background()
	seedOntology(t, d)
	r := newRecaller(d, nil)

	// 写两条同 scope 的记忆，把 final budget 压到 1，必然有一条被预算切断或过滤。
	w := recall.NewWriter(d.Ports())
	for _, id := range []string{"m-a", "m-b"} {
		if _, err := w.PutMemory(ctx, store.Memory{
			ID: id, Scope: store.ScopeUser, Kind: store.KindSemantic,
			Content: "编辑器 vim 配置" + id, TValid: 1000, TCreated: 1000,
		}); err != nil {
			t.Fatalf("PutMemory(%s): %v", id, err)
		}
	}
	res, err := r.Recall(ctx, recall.RecallRequest{
		Query: "编辑器 vim 配置", Scope: store.ScopeUser,
		Budget: recall.Budget{Final: 1, PerLane: 10},
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(res.Items) != 1 {
		t.Fatalf("final budget=1 时应只返回 1 条，got %d", len(res.Items))
	}
	if len(res.Plan.Dropped) == 0 {
		t.Fatalf("Plan.Dropped 为空：被预算切断的候选必须记录（R5 '哪些被拦截'）")
	}
	if res.Plan.Dropped[0].Reason == "" {
		t.Fatalf("Dropped 缺 Reason：%+v", res.Plan.Dropped[0])
	}
}

// TestKindFilterDroppedIsVisible：被 kind 过滤拦截时也必须记进 plan。
func TestKindFilterDroppedIsVisible(t *testing.T) {
	d := openDB(t)
	ctx := context.Background()
	w := recall.NewWriter(d.Ports())
	if _, err := w.PutMemory(ctx, store.Memory{
		ID: "m-dec", Scope: store.ScopeUser, Kind: store.KindDecision,
		Content: "认证用 JWT", TValid: 1000, TCreated: 1000,
	}); err != nil {
		t.Fatalf("PutMemory: %v", err)
	}
	r := newRecaller(d, nil)
	res, err := r.Recall(ctx, recall.RecallRequest{
		Query: "JWT 认证", Scope: store.ScopeUser, Kinds: []store.MemoryKind{store.KindEpisodic},
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(res.Items) != 0 {
		t.Fatalf("kind 过滤未生效：items=%d", len(res.Items))
	}
	if !containsSubstr(planReasons(res.Plan), "kind") {
		t.Fatalf("plan 未记录 kind 拦截：%+v", res.Plan.Dropped)
	}
}

// ===========================================================================
// 5. 时效降权与 embedder 路径
// ===========================================================================

// TestRecencyDecayApplied：老事实的分数必须被降权，且降权因子可从 plan 读到。
func TestRecencyDecayApplied(t *testing.T) {
	d := openDB(t)
	ctx := context.Background()
	w := recall.NewWriter(d.Ports())
	now := time.Unix(2_000_000_000, 0)

	// 一年前的事实（半衰期 30 天 → 因子远低于 1，但受下界保护）。
	old := now.Add(-365 * 24 * time.Hour).Unix()
	if _, err := w.PutMemory(ctx, store.Memory{
		ID: "m-old", Scope: store.ScopeUser, Kind: store.KindSemantic,
		Content: "认证方式 JWT", TValid: old, TCreated: old,
	}); err != nil {
		t.Fatalf("PutMemory: %v", err)
	}

	cfg := recall.Config{Now: func() time.Time { return now }}
	r := recall.New(d.Ports(), cfg, nil)
	res, err := r.Recall(ctx, recall.RecallRequest{Query: "认证方式 JWT", Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(res.Items) == 0 {
		t.Fatalf("无结果")
	}
	it := res.Items[0]
	if it.RecencyFactor >= 1 {
		t.Fatalf("RecencyFactor=%.3f，一年前的事实必须被降权", it.RecencyFactor)
	}
	if it.RecencyFactor < r.Config().RecencyFloor {
		t.Fatalf("RecencyFactor=%.3f 低于下界 %.2f：降权必须有地板", it.RecencyFactor, r.Config().RecencyFloor)
	}
}

// TestEmbedderEnablesVectorLane：注入 embedder 后 vector lane 参与，且不再报降级。
func TestEmbedderEnablesVectorLane(t *testing.T) {
	d := openDB(t)
	ctx := context.Background()
	w := recall.NewWriter(d.Ports())
	if _, err := w.PutMemory(ctx, store.Memory{
		ID: "m-vec", Scope: store.ScopeUser, Kind: store.KindSemantic,
		Content: "认证方式 JWT", TValid: 1000, TCreated: 1000,
	}); err != nil {
		t.Fatalf("PutMemory: %v", err)
	}
	if err := d.Vector().Upsert(ctx, []store.Vector{
		{ID: "m-vec", Scope: store.ScopeUser, Values: []float32{1, 0, 0}},
	}); err != nil {
		t.Fatalf("Vector.Upsert: %v", err)
	}

	emb := recall.EmbedderFunc(func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i := range texts {
			out[i] = []float32{1, 0, 0}
		}
		return out, nil
	})
	r := recall.New(d.Ports(), recall.Config{}, emb)

	res, err := r.Recall(ctx, recall.RecallRequest{Query: "认证方式 JWT", Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	var vectorExecuted bool
	for _, lp := range res.Plan.Lanes {
		if lp.Lane == recall.LaneVector && lp.Executed {
			vectorExecuted = true
		}
	}
	if !vectorExecuted {
		t.Fatalf("注入 embedder 后 vector lane 应参与：lanes=%+v", res.Plan.Lanes)
	}
	if containsSubstr(res.Degraded, "vector lane skipped") {
		t.Fatalf("注入 embedder 后不应再报 vector 降级：%v", res.Degraded)
	}
	if len(res.Items) == 0 {
		t.Fatalf("无结果")
	}
	var sawVector bool
	for _, c := range res.Items[0].Contributions {
		if c.Lane == recall.LaneVector {
			sawVector = true
		}
	}
	if !sawVector {
		t.Fatalf("vector lane 的贡献未出现在 Contributions：%+v", res.Items[0].Contributions)
	}
}

// TestBrokenEmbedderDegradesNotFails：embedder 报错时降级而不是整体失败。
func TestBrokenEmbedderDegradesNotFails(t *testing.T) {
	d := openDB(t)
	ctx := context.Background()
	w := recall.NewWriter(d.Ports())
	if _, err := w.PutMemory(ctx, store.Memory{
		ID: "m-x", Scope: store.ScopeUser, Kind: store.KindSemantic,
		Content: "认证方式 JWT", TValid: 1000, TCreated: 1000,
	}); err != nil {
		t.Fatalf("PutMemory: %v", err)
	}
	emb := recall.EmbedderFunc(func(context.Context, []string) ([][]float32, error) {
		return nil, errTestEmbedder
	})
	r := recall.New(d.Ports(), recall.Config{}, emb)
	res, err := r.Recall(ctx, recall.RecallRequest{Query: "JWT 认证方式", Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("embedder 失败不应让整次召回失败：%v", err)
	}
	if !containsSubstr(res.Degraded, "embedder error") {
		t.Fatalf("Degraded=%v，embedder 失败必须显式上报", res.Degraded)
	}
	if len(res.Items) == 0 {
		t.Fatalf("词法通道应仍然可用")
	}
}

// TestEmptyQueryErrors：空 query 是调用方缺陷，报错而不是返回空结果。
func TestEmptyQueryErrors(t *testing.T) {
	d := openDB(t)
	r := newRecaller(d, nil)
	if _, err := r.Recall(context.Background(), recall.RecallRequest{Query: "   "}); err == nil {
		t.Fatalf("空 query 必须报错")
	}
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

var errTestEmbedder = &testErr{"embedder unavailable"}

type testErr struct{ msg string }

func (e *testErr) Error() string { return e.msg }

func containsSubstr(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func hasStep(steps []recall.PlanStep, name string) bool {
	for _, s := range steps {
		if s.Step == name {
			return true
		}
	}
	return false
}

func hasStepPrefix(steps []recall.PlanStep, prefix string) bool {
	for _, s := range steps {
		if strings.HasPrefix(s.Step, prefix) {
			return true
		}
	}
	return false
}

func planReasons(plan *recall.RecallPlan) []string {
	if plan == nil {
		return nil
	}
	out := make([]string, 0, len(plan.Dropped))
	for _, d := range plan.Dropped {
		out = append(out, d.Reason)
	}
	return out
}
