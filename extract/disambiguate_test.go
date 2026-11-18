package extract

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tim5wang/gmem/sqlite"
	"github.com/tim5wang/gmem/store"

	_ "modernc.org/sqlite"
)

// ===========================================================================
// 三区消歧（docs/TD.md §4）
//
//	s = w1·cosine + w2·BM25 + w3·结构一致性
//
// 三区必须可分别命中，且**只有 θ_hi 以上才允许合并**。
// 默认权重 0.5/0.3/0.2、阈值 0.92/0.72，因此：
//   - cosine=1 且 BM25=1 且结构=1 → s=1.00 ≥ 0.92 → 自动合并
//   - cosine=1 且 BM25=1 且结构=0 → s=0.80 ∈ [0.72,0.92) → 灰区
//   - 名称不相似且无向量          → s 低 → 新建
// ===========================================================================

// constEmbedder 返回与文本长度无关的固定向量，便于精确控制 cosine。
// vectors 为空时返回零向量（模拟"embedder 给了空结果"的降级路径）。
type constEmbedder struct {
	vectors [][]float32
	err     error
}

func (c constEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if c.err != nil {
		return nil, c.err
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		if i < len(c.vectors) {
			out[i] = c.vectors[i]
		} else if len(c.vectors) > 0 {
			out[i] = c.vectors[len(c.vectors)-1]
		}
	}
	return out, nil
}

// openFileWithRaw 打开文件库并返回直连只读连接（用于 nodes 表行级断言）。
func openFileWithRaw(t *testing.T) (*sqlite.DB, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "extract-graph.db")
	d, err := sqlite.Open(context.Background(), sqlite.Config{Path: path})
	if err != nil {
		t.Fatalf("sqlite.Open(file): %v", err)
	}
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("close db: %v", err)
		}
	})
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open(raw): %v", err)
	}
	t.Cleanup(func() {
		if err := raw.Close(); err != nil {
			t.Errorf("close raw: %v", err)
		}
	})
	return d, raw
}

// countNodes 返回 nodes 表行数（灰区不合并的核心证据）。
func countNodes(t *testing.T, raw *sql.DB) int {
	t.Helper()
	var n int
	if err := raw.QueryRow(`SELECT COUNT(1) FROM nodes`).Scan(&n); err != nil {
		t.Fatalf("count nodes: %v", err)
	}
	return n
}

// countEdgesByRelation 返回指定关系的边数。
func countEdgesByRelation(t *testing.T, raw *sql.DB, rel string) int {
	t.Helper()
	var n int
	if err := raw.QueryRow(`SELECT COUNT(1) FROM edges WHERE relation = ?`, rel).Scan(&n); err != nil {
		t.Fatalf("count edges %s: %v", rel, err)
	}
	return n
}

// seedSimilarNode 写一个与 "vim 编辑器" 高度相似的已有节点。
func seedSimilarNode(t *testing.T, d *sqlite.DB, nodeID, name, surface string) {
	t.Helper()
	ctx := context.Background()
	if err := d.Graph().PutNode(ctx, store.Node{
		ID: nodeID, Scope: store.ScopeUser, CanonicalName: name,
		PropsJSON: `{"definition":"一个模式化文本编辑器"}`,
	}); err != nil {
		t.Fatalf("PutNode: %v", err)
	}
	if err := d.Store().PutAlias(ctx, store.Alias{
		Surface: surface, NodeID: nodeID, Source: "dict", Confidence: 0.9,
	}); err != nil {
		t.Fatalf("PutAlias: %v", err)
	}
}

// TestZoneAutoMerge 验证 s ≥ θ_hi 时自动合并：写 alias，**不新建节点**。
func TestZoneAutoMerge(t *testing.T) {
	d, raw := openFileWithRaw(t)
	seedSimilarNode(t, d, "n-vim-edit", "vim 编辑器", "vim 编辑器")
	// 给候选节点种一条 uses 边，使"结构一致性"有真实证据（Jaccard=1）。
	seedEdge(t, d, "n-vim-edit", "n-plugins", store.RelationUses)
	before := countNodes(t, raw)

	// 完全相同名称 + 相同向量 + 相同关系类型 → s = 1.0
	emb := bigramEmbedder(64)
	dis := NewDisambiguator(d.Ports(), emb, Config{})
	res, err := dis.Disambiguate(context.Background(), store.ScopeUser, []Mention{{
		Surface: "vim 编辑器", Definition: "一个模式化文本编辑器",
		RelationTypes: []store.Relation{store.RelationUses},
	}})
	if err != nil {
		t.Fatalf("Disambiguate: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("应有一个提及结论，实际 %d", len(res))
	}
	got := res[0]
	if got.Zone != ZoneAutoMerge {
		t.Fatalf("s=%.3f 应判为自动合并（θ_hi=0.92），实际 %q", got.Score, got.Zone)
	}
	if got.NodeID != "n-vim-edit" {
		t.Errorf("应命中已有节点 n-vim-edit，实际 %q", got.NodeID)
	}
	if got.Score < 0.92 {
		t.Errorf("自动合并的分数应 ≥ 0.92，实际 %.3f", got.Score)
	}
	// 分数必须可分解（R5 可解释性）。
	if len(got.Breakdown) == 0 {
		t.Error("必须给出分数分解")
	} else {
		sum := 0.5*got.Breakdown[0].Cosine + 0.3*got.Breakdown[0].BM25 + 0.2*got.Breakdown[0].Structural
		if diff := sum - got.Breakdown[0].Score; diff > 1e-6 || diff < -1e-6 {
			t.Errorf("分解项加权应等于总分：%.6f vs %.6f", sum, got.Breakdown[0].Score)
		}
	}
	_ = before
}

// TestZoneGrayWritesPossiblySameOnly 是「灰区不合并」的专项用例：
// 灰区候选写入后 nodes 表**不得被合并**，只能多一条 possiblySame 边。
//
// 构造方式：让 L2 抽出一个与候选**文本完全一致**的实体（cosine=1、BM25=1），
// 但不给关系证据（structural=0），于是 s = 0.5+0.3 = 0.80，落在默认
// [0.72, 0.92) 的灰区里。
func TestZoneGrayWritesPossiblySameOnly(t *testing.T) {
	d, raw := openFileWithRaw(t)
	seedSimilarNode(t, d, "n-vim-edit", "vim 编辑器", "vim 编辑器")

	emb := bigramEmbedder(64)
	llm := NewFakeLLM(FakeStep{Text: BuildFinalJSON([]EntityJSON{{
		Name: "vim 编辑器", Def: "一个模式化文本编辑器",
	}}, nil)})
	_ = llm
	cfg := Config{}
	p := NewPipeline(d.Ports(), cfg, PipelineOptions{
		Embedder: emb, Extractor: NewLLMExtractor(llm, d.Ports(), cfg),
	})

	nodesBefore := countNodes(t, raw)
	psBefore := countEdgesByRelation(t, raw, string(RelationPossiblySame))

	res := mustRun(t, p, RawEpisode{
		ID: "e-gray", Scope: store.ScopeUser, At: 1000,
		// 定义句的 Detail 必须与候选节点的 definition 完全一致，
		// 这样 mentionText == 候选文本 → cosine=1、BM25=1，而 structural=0，
		// s 恰好落在 [θ_lo, θ_hi) 的灰区里（这是本用例要固定住的构造）。
		Content: "vim 编辑器 是 一个模式化文本编辑器。",
	})
	if len(res.Disambiguation) == 0 {
		t.Fatalf("应产出消歧结论，Degraded=%v", res.Degraded)
	}

	var gray *DisambiguateResult
	for i := range res.Disambiguation {
		if res.Disambiguation[i].Zone == ZoneGray {
			gray = &res.Disambiguation[i]
			break
		}
	}
	if gray == nil {
		t.Fatalf("应至少有一个提及落入灰区，实际 %+v", zonesOf(res.Disambiguation))
	}

	// 1) 灰区必须只多一条 possiblySame 边。
	psAfter := countEdgesByRelation(t, raw, string(RelationPossiblySame))
	if psAfter != psBefore+1 {
		t.Errorf("灰区必须恰好新增 1 条 possiblySame 边，实际 %d → %d", psBefore, psAfter)
	}
	// 2) **绝不合并**：既有节点必须原样存在且名称未变。
	n, err := d.Graph().GetNode(context.Background(), "n-vim-edit")
	if err != nil {
		t.Fatalf("既有节点被删掉了（这就是合并！）：%v", err)
	}
	if n.CanonicalName != "vim 编辑器" {
		t.Errorf("既有节点名称被改写：%q", n.CanonicalName)
	}
	// 3) 没有任何 negative_SameAs 被误写。
	if got := countEdgesByRelation(t, raw, string(RelationNegativeSameAs)); got != 0 {
		t.Errorf("灰区不应写 negative_SameAs，实际 %d 条", got)
	}
	// 4) 提及自己得到独立节点（没有被吸进既有节点）。
	selfID := NodeID(store.ScopeUser, gray.Mention)
	if _, err := d.Graph().GetNode(context.Background(), selfID); err != nil {
		t.Errorf("灰区提及应有自己的节点 %q（未被合并）：%v", selfID, err)
	}
	// nodes 表增量 = 提及节点 + 可能的字面量节点，但**绝不能少于** 1，
	// 更不能出现"总数为 1 说明合并了"的情形。
	nodesAfter := countNodes(t, raw)
	if nodesAfter <= nodesBefore {
		t.Errorf("灰区后 nodes 数应增加（新增提及节点），实际 %d → %d", nodesBefore, nodesAfter)
	}
	if res.Reviewable == 0 {
		t.Error("灰区必须记账 Reviewable（进待审池的数量）")
	}
}

// TestZoneNewCreatesNode 验证 s < θ_lo 时新建节点。
func TestZoneNewCreatesNode(t *testing.T) {
	d, raw := openFileWithRaw(t)
	seedSimilarNode(t, d, "n-vim-edit", "vim 编辑器", "vim 编辑器")
	before := countNodes(t, raw)

	dis := NewDisambiguator(d.Ports(), bigramEmbedder(64), Config{})
	res, err := dis.Disambiguate(context.Background(), store.ScopeUser, []Mention{{
		Surface: "Kubernetes 集群调度器",
	}})
	if err != nil {
		t.Fatalf("Disambiguate: %v", err)
	}
	if len(res) != 1 || res[0].Zone != ZoneNew {
		t.Fatalf("不相似名称应判为新建，实际 %+v", zonesOf(res))
	}
	_ = before

	p := NewPipeline(d.Ports(), Config{}, PipelineOptions{})
	// 关系三元组的主语是连续字符类（不含空格），因此这里用无空格写法；
	// 这不是测试取巧：L0 的候选本就由 L2 修正，L0 只负责"抓住这个三元组"。
	_ = mustRun(t, p, RawEpisode{
		ID: "e-new", Scope: store.ScopeUser, At: 1000,
		Content: "Kubernetes调度器 使用 etcd 存储，这是一条足够长的内容。",
	})
	id := NodeID(store.ScopeUser, "Kubernetes调度器")
	if _, err := d.Graph().GetNode(context.Background(), id); err != nil {
		t.Errorf("应新建节点 %q：%v", id, err)
	}
	if got := countEdgesByRelation(t, raw, string(RelationPossiblySame)); got != 0 {
		t.Errorf("新建不应写 possiblySame，实际 %d 条", got)
	}
}

// TestThresholdsAreConfigurable 验证 θ_hi/θ_lo 是配置项（不是硬编码）。
func TestThresholdsAreConfigurable(t *testing.T) {
	defaults := Config{}.withDefaults()
	if defaults.ThetaHi != 0.92 || defaults.ThetaLo != 0.72 {
		t.Errorf("默认阈值必须是 0.92/0.72，实际 %.2f/%.2f", defaults.ThetaHi, defaults.ThetaLo)
	}
	// 把 θ_hi 降到 0.75，s=0.80 就会从灰区变成自动合并。
	d, _ := openFileWithRaw(t)
	seedSimilarNode(t, d, "n-vim-edit", "vim 编辑器", "vim 编辑器")
	emb := constEmbedder{vectors: [][]float32{{1, 0}}}
	dis := NewDisambiguator(d.Ports(), emb, Config{ThetaHi: 0.75, ThetaLo: 0.5})

	res, err := dis.Disambiguate(context.Background(), store.ScopeUser, []Mention{{
		Surface: "vim 编辑器", RelationTypes: []store.Relation{store.RelationUses},
	}})
	if err != nil {
		t.Fatalf("Disambiguate: %v", err)
	}
	if res[0].Zone != ZoneAutoMerge {
		t.Errorf("θ_hi=0.75 时 s=%.3f 应为自动合并，实际 %q", res[0].Score, res[0].Zone)
	}
}

// TestStructuralOverlap 单测结构一致性（Jaccard，无证据时返回 0 而不是 1）。
func TestStructuralOverlap(t *testing.T) {
	a := relationSet([]store.Relation{store.RelationUses, store.RelationPartOf})
	b := relationSet([]store.Relation{store.RelationUses})
	if got := structuralOverlap(a, b); got != 0.5 {
		t.Errorf("Jaccard 应为 0.5，实际 %.3f", got)
	}
	// 无证据必须返回 0：返回 1 会凭空抬分。
	if got := structuralOverlap(nil, b); got != 0 {
		t.Errorf("无关系类型时应为 0（无证据≠完全一致），实际 %.3f", got)
	}
	if got := structuralOverlap(a, a); got != 1 {
		t.Errorf("完全一致应为 1，实际 %.3f", got)
	}
}

// TestNegativeSameAsStopsGrayZone 验证 negative_SameAs：裁决"不同实体"后
// **同一对反复写入不得重复进待审池**，且后续不再进灰区。
func TestNegativeSameAsStopsGrayZone(t *testing.T) {
	d, raw := openFileWithRaw(t)
	seedSimilarNode(t, d, "n-vim-edit", "vim 编辑器", "vim 编辑器")
	// 定义句给出的释义与候选节点的 definition 完全一致 → cosine=1、BM25=1，
	// structural=0 → s=0.80 落在默认灰区（[0.72,0.92)）。
	emb := bigramEmbedder(64)
	p := NewPipeline(d.Ports(), Config{}, PipelineOptions{Embedder: emb})
	ep := RawEpisode{
		ID: "e-neg", Scope: store.ScopeUser, At: 1000,
		Content: "vim 编辑器 是 一个模式化文本编辑器。",
	}

	if got := countEdgesByRelation(t, raw, string(RelationPossiblySame)); got != 0 {
		t.Fatalf("前置条件：初始不应有 possiblySame 边，实际 %d", got)
	}

	// 第一次：每个落入灰区的提及恰好写 1 条 possiblySame（不变量，不写死条数）。
	res1 := mustRun(t, p, ep)
	grayMentions := grayMentionsOf(res1.Disambiguation)
	if len(grayMentions) == 0 {
		t.Fatalf("首轮应至少有一个提及进灰区，实际 %+v", zonesOf(res1.Disambiguation))
	}
	if got, want := countEdgesByRelation(t, raw, string(RelationPossiblySame)), len(grayMentions); got != want {
		t.Fatalf("每个灰区提及应恰好 1 条 possiblySame（提到 %d 个，实际 %d 条）", want, got)
	}
	if res1.Reviewable != len(grayMentions) {
		t.Errorf("Reviewable 应等于灰区提及数 %d，实际 %d", len(grayMentions), res1.Reviewable)
	}

	gray := grayMentions[0]
	selfID := NodeID(store.ScopeUser, gray.Mention)

	// 裁决为"不同实体"。
	if _, err := p.Judge(context.Background(), Verdict{
		Scope: store.ScopeUser, KeepNodeID: "n-vim-edit", OtherNodeID: selfID,
		Same: false, Reason: "同名但语境不同",
	}); err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if got := countEdgesByRelation(t, raw, string(RelationNegativeSameAs)); got != 1 {
		t.Fatalf("裁决后应写 1 条 negative_SameAs，实际 %d", got)
	}

	// 第二次写同一段：不得再进灰区、不得重复入池。
	psBefore := countEdgesByRelation(t, raw, string(RelationPossiblySame))
	res2 := mustRun(t, p, ep)
	for _, dd := range res2.Disambiguation {
		if dd.Mention == gray.Mention && dd.Zone == ZoneGray {
			t.Errorf("已裁决为不同实体后不得再进灰区：%+v", dd)
		}
	}
	psAfter := countEdgesByRelation(t, raw, string(RelationPossiblySame))
	if psAfter > psBefore {
		t.Errorf("同一对反复写入不得重复进待审池：possiblySame %d → %d", psBefore, psAfter)
	}
	if res2.Reviewable != 0 {
		t.Errorf("第二轮不应再产生待审项，实际 Reviewable=%d", res2.Reviewable)
	}
	// negative_SameAs 只有一条（幂等）。
	if got := countEdgesByRelation(t, raw, string(RelationNegativeSameAs)); got != 1 {
		t.Errorf("negative_SameAs 应保持 1 条，实际 %d", got)
	}
}

// TestGrayZoneDedupOnRepeatedWrite 验证灰区本身也去重（没有 negative 的情况）。
func TestGrayZoneDedupOnRepeatedWrite(t *testing.T) {
	d, raw := openFileWithRaw(t)
	seedSimilarNode(t, d, "n-vim-edit", "vim 编辑器", "vim 编辑器")

	emb := bigramEmbedder(64)
	p := NewPipeline(d.Ports(), Config{}, PipelineOptions{Embedder: emb})
	ep := RawEpisode{
		ID: "e-gray2", Scope: store.ScopeUser, At: 1000,
		Content: "vim 编辑器 是 一个模式化文本编辑器。",
	}
	res1 := mustRun(t, p, ep)
	if res1.Reviewable == 0 {
		t.Fatalf("首轮应产生待审项，Degraded=%v zones=%+v", res1.Degraded, zonesOf(res1.Disambiguation))
	}
	psAfter1 := countEdgesByRelation(t, raw, string(RelationPossiblySame))
	res2 := mustRun(t, p, ep)
	psAfter2 := countEdgesByRelation(t, raw, string(RelationPossiblySame))
	if psAfter2 != psAfter1 {
		t.Errorf("重复写入不得新增 possiblySame 边：%d → %d", psAfter1, psAfter2)
	}
	if res2.Reviewable != 0 {
		t.Errorf("重复写入不应再产生待审项，实际 %d", res2.Reviewable)
	}
}

func zonesOf(rs []DisambiguateResult) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Mention+"→"+string(r.Zone))
	}
	return out
}

// ===========================================================================
// 写入必须走 recall.Writer
//
// 这条契约只能从"FTS 能否搜到"来验证：绕过 Writer 的写入不会报错，
// 它只是让记忆在词法通道上永远查不到。P0 修过的正是这个缺陷。
// ===========================================================================

func TestWritesGoThroughWriterAndAreSearchable(t *testing.T) {
	d := openMemoryDB(t)
	p := NewPipeline(d.Ports(), Config{}, PipelineOptions{})

	content := "认证服务依赖 Redis 做会话缓存，这是决策记录。"
	if _, err := p.Run(context.Background(), RawEpisode{
		ID: "e-writer", Scope: store.ScopeUser, At: 1000, Content: content,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	hits, err := d.Lexical().Search(context.Background(), store.LexicalQuery{
		Query: "Redis 会话缓存", Scope: store.ScopeUser, Limit: 10,
	})
	if err != nil {
		t.Fatalf("lexical search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("写入后词法通道查不到——说明绕过了 recall.Writer（FTS 静默漏索引）")
	}
	found := false
	for _, h := range hits {
		if h.Version >= 1 {
			found = true
		}
	}
	if !found {
		t.Error("FTS 命中必须带合法 version（D13：索引粒度是 (id, version)）")
	}

	// 记忆本身必须可读，且与 FTS 一致。
	m, err := d.Store().GetMemory(context.Background(), hits[0].ID)
	if err != nil {
		t.Fatalf("GetMemory(%q): %v", hits[0].ID, err)
	}
	if !strings.Contains(m.Content, "Redis") {
		t.Errorf("FTS 命中的记忆内容不符：%q", m.Content)
	}
	if m.Version != hits[0].Version {
		t.Errorf("FTS 版本 %d 与主表当前版本 %d 不一致（= 索引漏更新的信号）",
			hits[0].Version, m.Version)
	}
}

// TestWriteReportLexicalIndexed 验证写入报告显式声明词法已同步。
func TestWriteReportLexicalIndexed(t *testing.T) {
	d := openMemoryDB(t)
	p := NewPipeline(d.Ports(), Config{}, PipelineOptions{})
	res := mustRun(t, p, RawEpisode{
		ID: "e-rep", Scope: store.ScopeUser, At: 1000,
		Content: "这是一条足够长的记忆内容，用于验证写入报告。",
	})
	if len(res.Writes) == 0 {
		t.Fatal("应至少有一条写入报告")
	}
	for _, w := range res.Writes {
		if w.MemoryID == "" || w.Version < 1 {
			t.Errorf("写入报告必须有 id 与合法版本：%+v", w)
		}
	}
}

// grayMentionsOf 返回落入灰区的提及结论。
func grayMentionsOf(rs []DisambiguateResult) []DisambiguateResult {
	out := make([]DisambiguateResult, 0, len(rs))
	for _, r := range rs {
		if r.Zone == ZoneGray {
			out = append(out, r)
		}
	}
	return out
}

// TestThetaHiBoundaryNotAutoMerge 钉住 θ_hi 的**行为边界**（不只是默认值）。
//
// 构造一个分数严格落在 (0.80, 0.92) 的提及：cosine=1、BM25=1、结构一致性=1/3
//
//	s = 0.5·1 + 0.3·1 + 0.2·(1/3) = 0.8667
//
// 于是：
//   - θ_hi=0.92（默认）→ 灰区，**绝不合并**；
//   - 若把 θ_hi 调低到 0.80（一次典型的"手滑放宽"）→ 变成自动合并。
//
// 这一条存在的理由：灰区用例的分数恰好是 0.80，落在浮点误差的边界上
// （0.7999999999999998 >= 0.80 为 false），因此"把 θ_hi 改成 0.80"的变异
// 不会被它发现。用一个严格落在区间内部的分数才能真正锁住这道门。
func TestThetaHiBoundaryNotAutoMerge(t *testing.T) {
	d, _ := openFileWithRaw(t)
	ctx := context.Background()
	seedSimilarNode(t, d, "n-vim-edit", "vim 编辑器", "vim 编辑器")
	// 让候选节点拥有 3 种关系类型，而提及只有 1 种 → Jaccard = 1/3。
	for i, rel := range []store.Relation{
		store.RelationUses, store.RelationPartOf, store.RelationDependsOn,
	} {
		seedEdge(t, d, "n-vim-edit", "n-target-"+itoa(i), rel)
	}

	dis := NewDisambiguator(d.Ports(), bigramEmbedder(64), Config{})
	res, err := dis.Disambiguate(ctx, store.ScopeUser, []Mention{{
		Surface: "vim 编辑器", Definition: "一个模式化文本编辑器",
		RelationTypes: []store.Relation{store.RelationUses},
	}})
	if err != nil {
		t.Fatalf("Disambiguate: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("应有一个结论，实际 %d", len(res))
	}
	got := res[0]
	// 先确认分数真的落在灰区区间内部（否则下面的断言是空的）。
	if !(got.Score > 0.80 && got.Score < 0.92) {
		t.Fatalf("构造失败：分数应严格落在 (0.80, 0.92)，实际 %.4f（breakdown=%+v）",
			got.Score, got.Breakdown)
	}
	if got.Zone != ZoneGray {
		t.Errorf("θ_hi=0.92 时 s=%.4f 必须是灰区（绝不自动合并），实际 %q",
			got.Score, got.Zone)
	}
}

// TestSelfNodeExcludedFromCandidates 验证消歧会排除"该提及自身派生的节点"。
//
// # 为什么必须有这条
//
// 重复写入同一段文本时，首轮为提及建出的节点已经在候选池里，且它与自己
// 的 cosine=1、BM25=1。若它还有关系边（例如 L2 抽出了 uses），结构一致性也是 1，
// 于是 s=1.0 ≥ θ_hi → 判定"自动合并到自身"。这不是合并而是自指，
// 后果是真正需要审的那个候选（n-vim-edit）被永远跳过。
func TestSelfNodeExcludedFromCandidates(t *testing.T) {
	d, _ := openFileWithRaw(t)
	ctx := context.Background()
	seedSimilarNode(t, d, "n-vim-edit", "vim 编辑器", "vim 编辑器")

	// 首轮写入会产生的那个"自身节点"：给它词典项（进入候选池）+ 关系边（结构=1）。
	selfID := NodeID(store.ScopeUser, "vim 编辑器")
	if err := d.Graph().PutNode(ctx, store.Node{
		ID: selfID, Scope: store.ScopeUser, CanonicalName: "vim 编辑器",
		PropsJSON: `{"definition":"一个模式化文本编辑器"}`,
	}); err != nil {
		t.Fatalf("PutNode(self): %v", err)
	}
	if err := d.Store().PutAlias(ctx, store.Alias{
		Surface: "vim 编辑器", NodeID: selfID, Source: "extract:gray", Confidence: 0.6,
	}); err != nil {
		t.Fatalf("PutAlias(self): %v", err)
	}
	seedEdge(t, d, selfID, "n-target", store.RelationUses)

	dis := NewDisambiguator(d.Ports(), bigramEmbedder(64), Config{})
	res, err := dis.Disambiguate(ctx, store.ScopeUser, []Mention{{
		Surface: "vim 编辑器", Definition: "一个模式化文本编辑器",
		RelationTypes: []store.Relation{store.RelationUses},
	}})
	if err != nil {
		t.Fatalf("Disambiguate: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("应有一个结论，实际 %d", len(res))
	}
	got := res[0]
	if got.NodeID == selfID {
		t.Errorf("消歧命中了提及自身的派生节点 %q（自指，必须排除）；breakdown=%+v",
			selfID, got.Breakdown)
	}
	for _, bd := range got.Breakdown {
		if bd.NodeID == selfID {
			t.Errorf("分数分解里不得出现自身节点 %q：%+v", selfID, bd)
		}
	}
	// 排除自身后，应比对到真正的候选并落入灰区（而不是被自身那个 1.0 吸走）。
	if got.Zone != ZoneGray {
		t.Errorf("排除自身后应判为灰区，实际 %q（score=%.4f）", got.Zone, got.Score)
	}
	if got.NodeID != "n-vim-edit" {
		t.Errorf("应比对到 n-vim-edit，实际 %q", got.NodeID)
	}
}
