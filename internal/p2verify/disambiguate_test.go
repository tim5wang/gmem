package p2verify

import (
	"context"
	"strings"
	"testing"

	"github.com/tim5wang/gmem/extract"
	"github.com/tim5wang/gmem/sqlite"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// 攻击面 3：三区消歧（docs/TD.md §4）
//
// "灰区绝不合并"是本节唯一不能妥协的一条：误合并是记忆系统最难恢复的错误
// （两条事实混进一个节点，事后无法拆开）。因此断言不看"判定写着 gray"，
// 而看**库里的行**：nodes 表有没有多一个节点、aliases 有没有多一条、edges
// 里是不是只有一条 possiblySame。
//
// 为什么要把 s 精确钉在某区间：只有 s ∈ [θ_lo, θ_hi) 才是灰区。本包用
// 固定向量 + 同名 + 空结构一致性把它算成 0.80（推导见 grayMention），
// 而不是"碰一个看起来像灰区的例子"。
// ===========================================================================

// grayMention 说明本包如何把融合分 s 钉到灰区：
//
//	s = (w1·cosine + w2·BM25 + w3·structural) / (w1+w2+w3)
//	  = (0.5·1     + 0.3·1    + 0.2·0)         / 1.0
//	  = 0.80                              ∈ [0.72, 0.92) → 灰区
//
// 三个信号各自怎么钉死：
//   - cosine=1：mention 侧向量与候选侧向量用同一个常量向量；
//   - BM25=1：提及表面形式与候选节点的 canonical name 完全相同（BM25 归一化后取满分）；
//   - structural=0：候选节点没有任何边，提及也不带关系类型（Jaccard 无证据取 0）。
//
// 换成 structural=1（候选有 uses 边、提及也带 uses）就得到 s=1.00 ≥ θ_hi → 自动合并。
// 两条用例共用同一套种子，因此"差一点就合并"与"合并"之间的分界是被精确验证的。
func grayMention() extract.ExtractedEntity {
	return extract.ExtractedEntity{Name: candidateName, Confidence: 0.9}
}

// candidateName 是已存在节点的名字，与 grayMention 的表面形式完全相同。
const candidateName = "vim 编辑器"

// clampContent 是不触发 L0 词典/关系规则的正文（避免噪声节点污染行级断言）。
const clampContent = "本次讨论只涉及缓存策略的细节记录。"

// pluginNodeName 是结构一致性信号里的另一端（候选节点的一条 uses 边）。
const pluginNodeName = "plugins"

// seedCandidate 种一个"已有实体"：节点 + 词典项（词典是消歧候选池的唯一入口）。
//
// nodeID 刻意可以**不等于** NodeID(scope, name)：真实库里节点 id 由首次写入派生，
// 而宿主导入、P3 的 Merge（"保留旧 ID 为重定向"）都会让 canonical name 与 id 脱钩。
// 本包两种形态都覆盖：灰区用例用脱钩 id（更接近真实），自动合并用例用自洽 id。
func seedCandidate(t *testing.T, d *sqlite.DB, nodeID, name string, withEdge bool) {
	t.Helper()
	if err := d.Graph().PutNode(context.Background(), store.Node{
		ID: nodeID, Scope: store.ScopeUser, CanonicalName: name,
	}); err != nil {
		t.Fatalf("PutNode(%q): %v", nodeID, err)
	}
	seedAlias(t, d, name, nodeID, 0.99)
	if withEdge {
		seedNode(t, d, pluginNodeID, pluginNodeName)
		seedAlias(t, d, pluginNodeName, pluginNodeID, 0.9)
		seedEdge(t, d, "e-seed-uses", nodeID, pluginNodeID, store.RelationUses)
	}
}

// pluginNodeID 是 plugins 节点 id（自洽 id：与 NodeID(scope,"plugins") 一致），
// 这样边端点必然存在，用例不会撞上"脱钩 id"那条缺陷（那条另有专门用例）。
var pluginNodeID = extract.NodeID(store.ScopeUser, pluginNodeName)

// fixedEmbedder 返回同一个常量向量：cosine 恒为 1。
func fixedEmbedder() extract.Embedder { return constEmbedder{vec: []float32{1, 0, 0}} }

// newZonePipeline 构造"钉死输入"的管线：L2 由固定 extractor 提供，向量固定。
//
// 不用真实 LLM：三区判定的输入必须可复现，否则断言的是模型而不是阈值。
func newZonePipeline(t *testing.T, d *sqlite.DB, cfg extract.Config,
	entity extract.ExtractedEntity, rels []extract.ExtractedRelation) *extract.Pipeline {
	t.Helper()
	return extract.NewPipeline(d.Ports(), cfg, extract.PipelineOptions{
		Extractor: entityExtractor{entities: []extract.ExtractedEntity{entity}, relations: rels},
		Embedder:  fixedEmbedder(),
	})
}

// runZone 跑一次管线。
func runZone(t *testing.T, p *extract.Pipeline, epID string) extract.PipelineResult {
	t.Helper()
	res, err := p.Run(context.Background(), extract.RawEpisode{
		ID: epID, Scope: store.ScopeUser, Content: clampContent,
	})
	if err != nil {
		t.Fatalf("Pipeline.Run(%s): %v", epID, err)
	}
	return res
}

// zoneOf 从结果里取出某个提及的三区判定。
func zoneOf(t *testing.T, res extract.PipelineResult, surface string) extract.DisambiguateResult {
	t.Helper()
	for _, d := range res.Disambiguation {
		if strings.EqualFold(d.Mention, surface) {
			return d
		}
	}
	t.Fatalf("结果里没有提及 %q 的消歧结论；实际 %d 条", surface, len(res.Disambiguation))
	return extract.DisambiguateResult{}
}

// TestGrayZoneNeverMerges 是本节的主判据：灰区**只**写一条 possiblySame 边。
//
// 断言四件事（全部落在库行上，不看"判定标签"）：
//  1. 判定真的是 gray，且 s 落在 [θ_lo, θ_hi) 区间内（自我校验：若 s 漂出区间，
//     说明本用例的前提失效，后面的断言就没有意义）；
//  2. 候选节点的 canonical name 未被改写、alias 未被合并（nodes 表未合并）；
//  3. 提及被落成**自己的新节点**，而不是被并进候选节点；
//  4. 灰区边恰好一条，且关系名是 possiblySame（不是任何真实语义关系）。
func TestGrayZoneNeverMerges(t *testing.T) {
	d, raw := openFileDB(t)
	// 脱钩 id：候选节点 id 与 NodeID(scope,"vim 编辑器") 不同（真实库的常见形态）。
	seedCandidate(t, d, "n-vim-edit", candidateName, false)
	p := newZonePipeline(t, d, extract.Config{}, grayMention(), nil)

	res := runZone(t, p, "e-zone")
	got := zoneOf(t, res, candidateName)
	if got.Zone != extract.ZoneGray {
		t.Fatalf("s=0.80 落在 [0.72,0.92) 应为灰区，实际 %q（score=%.4f，breakdown=%+v）",
			got.Zone, got.Score, got.Breakdown)
	}
	// 自我校验：分数必须真的在区间内，否则用例前提失效。
	if got.Score < 0.72 || got.Score >= 0.92 {
		t.Fatalf("用例前提失效：期望 s ∈ [0.72,0.92)，实际 %.4f", got.Score)
	}
	if got.NodeID != "n-vim-edit" {
		t.Errorf("灰区应指向候选节点 n-vim-edit，实际 %q", got.NodeID)
	}

	// ---- 1) nodes 表未被合并 ----
	var canonical string
	if err := raw.QueryRowContext(context.Background(),
		`SELECT canonical_name FROM nodes WHERE id = 'n-vim-edit'`).Scan(&canonical); err != nil {
		t.Fatalf("候选节点 n-vim-edit 应仍然存在：%v", err)
	}
	if canonical != candidateName {
		t.Errorf("候选节点的 canonical_name 被改写成 %q（合并发生了）", canonical)
	}
	// 提及自己必须落成一个**独立**节点：这是"没合并"的最直接证据。
	newID := extract.NodeID(store.ScopeUser, candidateName)
	if newID == "n-vim-edit" {
		t.Fatalf("用例前提失效：新提及节点 id 与候选节点相同（%q）", newID)
	}
	if n := countRows(t, raw, `SELECT COUNT(1) FROM nodes WHERE id = ?`, newID); n != 1 {
		t.Errorf("灰区提及应落成独立节点 %q，实际 %d 行", newID, n)
	}
	if n := countRows(t, raw, `SELECT COUNT(1) FROM nodes`); n != 2 {
		t.Errorf("nodes 表应恰好 2 行（候选 + 新提及），实际 %d 行（合并或漏写）", n)
	}

	// ---- 2) 灰区边恰好一条，且是 possiblySame ----
	if n := countRelation(t, raw, string(extract.RelationPossiblySame)); n != 1 {
		t.Fatalf("灰区应恰好写 1 条 possiblySame 边，实际 %d 条", n)
	}
	// 也不能顺手写下任何真实语义关系（灰区不是"关系已确定"）。
	for _, rel := range []store.Relation{
		store.RelationUses, store.RelationPartOf, store.RelationDependsOn,
		store.RelationSubtypeOf, store.RelationCausedBy, store.RelationWorksAt,
		store.RelationOwns,
	} {
		if n := countRelation(t, raw, string(rel)); n != 0 {
			t.Errorf("灰区不应写语义关系 %q，实际 %d 条", rel, n)
		}
	}

	// ---- 3) 自动合并的标志（extract:auto 别名）未出现 ----
	if n := countRows(t, raw,
		`SELECT COUNT(1) FROM aliases WHERE source = 'extract:auto'`); n != 0 {
		t.Errorf("灰区不应写任何 extract:auto 别名（那是自动合并的标志），实际 %d 条", n)
	}
	// 待审计数必须 > 0：它进的是待审池，不是静默丢弃。
	if res.Reviewable != 1 {
		t.Errorf("灰区应计入待审池 1 条（Reviewable），实际 %d", res.Reviewable)
	}
}

// TestAutoMergeStillWorksAtThetaHi 是灰区用例的对照组。
//
// 若只验"灰区不合并"，一个"永远判灰区"的实现也能通过——那同样是缺陷
// （θ_hi 形同虚设）。因此必须同时证明自动合并路径真的会合并：写高置信 alias、
// **不**新建同义节点、不写 possiblySame。
//
// 为什么把 θ_hi 降到 0.75 而不是用默认 0.92：本例的目标是"合并语义本身"，
// 需要把 structural 固定为 0（不碰关系边），于是 s=0.80。若用默认 θ_hi=0.92，
// s=0.80 会落回灰区，对照组就退化成重复用例。
// 阈值可配已由 TestThresholdsAreConfigurableNotHardcoded 单独证明。
func TestAutoMergeStillWorksAtThetaHi(t *testing.T) {
	d, raw := openFileDB(t)
	// 脱钩 id：候选节点 id 与 NodeID(scope,name) 不同。
	// 这不是为了绕过缺陷——相反，这是自动合并**唯一能到达**的形态：
	// 消歧会排除"自指"候选（cid == NodeID(scope,surface)），因此自洽 id
	// 永远判不出 auto_merge。见 findings_test.go 的【可后续】#3。
	seedCandidate(t, d, "n-vim-edit", candidateName, false)

	p := newZonePipeline(t, d, extract.Config{ThetaHi: 0.75, ThetaLo: 0.70},
		grayMention(), nil)
	res := runZone(t, p, "e-auto")

	got := zoneOf(t, res, candidateName)
	if got.Zone != extract.ZoneAutoMerge {
		t.Fatalf("s=%.4f ≥ θ_hi=0.75 应自动合并，实际 %q（breakdown=%+v）",
			got.Score, got.Zone, got.Breakdown)
	}
	if got.NodeID != "n-vim-edit" {
		t.Errorf("自动合并应指向候选节点 n-vim-edit，实际 %q", got.NodeID)
	}
	// 合并的证据 1：候选节点多了一条高置信 alias。
	if n := countRows(t, raw,
		`SELECT COUNT(1) FROM aliases WHERE node_id = 'n-vim-edit' AND source = 'extract:auto'`); n != 1 {
		t.Errorf("自动合并应写 1 条 extract:auto 别名到候选节点，实际 %d 条", n)
	}
	// 合并的证据 2：**没有**写 possiblySame（那会把它降级成待审）。
	if n := countRelation(t, raw, string(extract.RelationPossiblySame)); n != 0 {
		t.Errorf("自动合并不应写 possiblySame 边，实际 %d 条", n)
	}
	// 合并的证据 3：没有多出一个同义节点。
	if n := countRows(t, raw, `SELECT COUNT(1) FROM nodes`); n != 1 {
		t.Errorf("自动合并后 nodes 应为 1 行（只有候选，未新建），实际 %d 行", n)
	}
	if res.Reviewable != 0 {
		t.Errorf("自动合并不应入待审池，实际 Reviewable=%d", res.Reviewable)
	}
}

// TestGrayZoneDedupOnRepeatedWrite 攻击"同一对反复写入不重复进待审池"。
//
// 待审池无限膨胀会让人工裁决彻底不可行（§4 护栏正是为此）。
// 因此连跑 3 次同一段 episode，possiblySame 边必须始终只有 1 条。
func TestGrayZoneDedupOnRepeatedWrite(t *testing.T) {
	d, raw := openFileDB(t)
	seedCandidate(t, d, "n-vim-edit", candidateName, false)
	p := newZonePipeline(t, d, extract.Config{}, grayMention(), nil)

	for i := 0; i < 3; i++ {
		res := runZone(t, p, "e-dedup")
		z := zoneOf(t, res, candidateName)
		if z.Zone != extract.ZoneGray {
			t.Fatalf("第 %d 次运行判定变为 %q（应持续为灰区）", i+1, z.Zone)
		}
		if n := countRelation(t, raw, string(extract.RelationPossiblySame)); n != 1 {
			t.Fatalf("第 %d 次运行后 possiblySame 边应为 1 条，实际 %d 条（待审池重复膨胀）",
				i+1, n)
		}
		if i > 0 && res.Reviewable != 0 {
			t.Errorf("第 %d 次运行不应再新增待审条目（Reviewable=%d）", i+1, res.Reviewable)
		}
	}
	// 节点也不能每次多一个（否则"重复写入"会造出 N 个同义节点）。
	if n := countRows(t, raw, `SELECT COUNT(1) FROM nodes`); n != 2 {
		t.Errorf("3 次重复写入后 nodes 仍应为 2 行（候选 + 一个提及节点），实际 %d 行", n)
	}
}

// TestNegativeSameAsStopsGrayZone 攻击 negative_SameAs：裁决为不同后不再进灰区。
//
// 断言分两层，因为它们是**两个不同的问题**：
//   - 判定层（关键）：同一对再出现时必须是 ZoneNegative，且不再进待审池；
//   - 记账层（可后续）：裁决后原有的 possiblySame 边应被置失效。
//     这一层目前取决于 Judge 的 Keep/Other 方向（见 pending_invalidate_test.go），
//     因此这里只断言"不再进灰区"，记账层由专门的用例记录现状。
func TestNegativeSameAsStopsGrayZone(t *testing.T) {
	d, raw := openFileDB(t)
	seedCandidate(t, d, "n-vim-edit", candidateName, false)
	p := newZonePipeline(t, d, extract.Config{}, grayMention(), nil)

	res := runZone(t, p, "e-neg")
	first := zoneOf(t, res, candidateName)
	if first.Zone != extract.ZoneGray || first.NodeID != "n-vim-edit" {
		t.Fatalf("前置条件：首次应为灰区并指向 n-vim-edit，实际 %q/%q", first.Zone, first.NodeID)
	}
	newID := extract.NodeID(store.ScopeUser, candidateName)

	// 人工/Agent 裁决：这对是不同实体。
	if _, err := p.Judge(context.Background(), extract.Verdict{
		Scope: store.ScopeUser, KeepNodeID: first.NodeID, OtherNodeID: newID,
		Same: false, Reason: "同名但不同产品",
	}); err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if n := countRelation(t, raw, string(extract.RelationNegativeSameAs)); n != 1 {
		t.Fatalf("裁决为不同应写 1 条 negative_SameAs 边，实际 %d 条", n)
	}

	// 同一段 episode 再来一次：不得再进灰区。
	res2 := runZone(t, p, "e-neg")
	second := zoneOf(t, res2, candidateName)
	if second.Zone != extract.ZoneNegative {
		t.Fatalf("已裁决为不同实体的对不应再进灰区，实际判定 %q（score=%.4f）",
			second.Zone, second.Score)
	}
	if res2.Reviewable != 0 {
		t.Errorf("已裁决的对不应再入待审池，实际 Reviewable=%d", res2.Reviewable)
	}
	// negative_SameAs 不得增殖（每次重跑都写一条会让账本无限长）。
	if n := countRelation(t, raw, string(extract.RelationNegativeSameAs)); n != 1 {
		t.Errorf("negative_SameAs 应保持 1 条（幂等），实际 %d 条", n)
	}
	// 也不能把裁决误当成"合并"：两个节点都必须还在。
	for _, id := range []string{"n-vim-edit", newID} {
		if n := countRows(t, raw, `SELECT COUNT(1) FROM nodes WHERE id = ?`, id); n != 1 {
			t.Errorf("裁决为不同后节点 %q 必须保留，实际 %d 行", id, n)
		}
	}
}

// TestNegativeSameAsDoesNotPoisonOtherPairs 验证 negative_SameAs **不会误伤**真同一实体。
//
// 机制上要防的缺陷：把 negative 的匹配键写成"surface 字符串"或"任一节点 id"，
// 于是一旦裁决过 "vim 编辑器"≠A，后续 "vim 编辑器" 与 B 的判定也会被一起跳过——
// 真正的同一实体再也合并不上，且不报错（只是答案变慢且变差）。
//
// 直接调 Disambiguator（公开 API）：本用例验的是"记账边的键粒度"，
// 与 Pipeline 的写入顺序无关，走管线只会引入无关的 FK 噪声。
func TestNegativeSameAsDoesNotPoisonOtherPairs(t *testing.T) {
	d, raw := openFileDB(t)
	ctx := context.Background()
	selfID := extract.NodeID(store.ScopeUser, candidateName)

	// 三个节点：
	//   selfID      —— 提及若新建会得到的 id（negative 键的另一半）
	//   n-vim-edit  —— 将被裁决为"不同实体"（无关系证据 → structural=0）
	//   n-vim-edit2 —— 真同一实体（有 uses 边 → structural=1）
	for _, id := range []string{selfID, "n-vim-edit", "n-vim-edit2"} {
		if err := d.Graph().PutNode(ctx, store.Node{
			ID: id, Scope: store.ScopeUser, CanonicalName: candidateName,
		}); err != nil {
			t.Fatalf("PutNode(%q): %v", id, err)
		}
		seedAlias(t, d, candidateName+"#"+id, id, 0.98)
	}
	seedNode(t, d, pluginNodeID, pluginNodeName)
	seedEdge(t, d, "e-u2", "n-vim-edit2", pluginNodeID, store.RelationUses)

	// 用 Judge 写 negative_SameAs —— 键是 (selfID, n-vim-edit) 的无序对。
	// 两个节点都真实存在，因此这一笔写入本身不受端点缺陷影响。
	p := extract.NewPipeline(d.Ports(), extract.Config{}, extract.PipelineOptions{})
	if _, err := p.Judge(ctx, extract.Verdict{
		Scope: store.ScopeUser, KeepNodeID: selfID, OtherNodeID: "n-vim-edit",
		Same: false, Reason: "同名不同产品",
	}); err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if n := countRelation(t, raw, string(extract.RelationNegativeSameAs)); n != 1 {
		t.Fatalf("前置条件：应写 1 条 negative_SameAs，实际 %d 条", n)
	}

	// 现在判一次：n-vim-edit2（真同一实体）必须仍然可以自动合并。
	dis := extract.NewDisambiguator(d.Ports(), fixedEmbedder(), extract.Config{})
	res, err := dis.Disambiguate(ctx, store.ScopeUser, []extract.Mention{{
		Surface: candidateName, Values: []float32{1, 0, 0},
		RelationTypes: []store.Relation{store.RelationUses},
	}})
	if err != nil {
		t.Fatalf("Disambiguate: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("应有一个提及结论，实际 %d", len(res))
	}
	got := res[0]

	// 关键断言：未被裁决的那一对仍能合并 —— negative 只作用于**那一对**。
	if got.Zone != extract.ZoneAutoMerge || got.NodeID != "n-vim-edit2" {
		t.Fatalf("negative_SameAs 误伤了真同一实体：判定 %q → %q"+
			"（期望 auto_merge → n-vim-edit2；breakdown=%+v）",
			got.Zone, got.NodeID, got.Breakdown)
	}
	// 被裁决的那一对必须在 breakdown 里标 Negative（可解释性，R5）。
	var marked bool
	for _, bd := range got.Breakdown {
		if bd.NodeID == "n-vim-edit" {
			marked = bd.Negative
		}
	}
	if !marked {
		t.Errorf("被裁决为不同的候选应在 breakdown 里标 Negative=true；breakdown=%+v", got.Breakdown)
	}
	if n := countRelation(t, raw, string(extract.RelationNegativeSameAs)); n != 1 {
		t.Errorf("negative_SameAs 应恰好 1 条（不因误伤而增殖），实际 %d 条", n)
	}
}

// TestThresholdsAreConfigurableNotHardcoded 攻击 θ_hi / θ_lo 是否真的可配。
//
// 同一对候选（s=0.80）在三种配置下必须给出三种不同判定。若任一阈值被硬编码，
// 至少一种配置会给出错误的区间 —— 这条用例正是"写死"的探针。
func TestThresholdsAreConfigurableNotHardcoded(t *testing.T) {
	const want = 0.80 // 由 grayMention 的推导固定
	cases := []struct {
		name string
		cfg  extract.Config
		want extract.Zone
	}{
		{name: "默认 0.92/0.72 → 灰区", cfg: extract.Config{}, want: extract.ZoneGray},
		{
			name: "θ_hi 降到 0.75 → 同一对变成自动合并",
			cfg:  extract.Config{ThetaHi: 0.75, ThetaLo: 0.70},
			want: extract.ZoneAutoMerge,
		},
		{
			name: "θ_lo 升到 0.85 → 同一对变成新建",
			cfg:  extract.Config{ThetaHi: 0.95, ThetaLo: 0.85},
			want: extract.ZoneNew,
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			d, _ := openFileDB(t)
			seedCandidate(t, d, "n-vim-edit", candidateName, false)
			res := runZone(t, newZonePipeline(t, d, c.cfg, grayMention(), nil), "e-th")
			got := zoneOf(t, res, candidateName)
			if got.Score < want-0.02 || got.Score > want+0.02 {
				t.Fatalf("用例前提失效：融合分应≈%.2f，实际 %.4f", want, got.Score)
			}
			if got.Zone != c.want {
				t.Fatalf("配置 θ_hi=%.2f/θ_lo=%.2f 下判定应为 %q，实际 %q（score=%.4f）——阈值疑似写死",
					c.cfg.ThetaHi, c.cfg.ThetaLo, c.want, got.Zone, got.Score)
			}
		})
	}
}

// TestDefaultThresholdsAreCalibrationPending 断言默认值是 0.92/0.72 且注释标明待校准。
//
// 第二半必须查源码：它是**文档承诺**而非运行时行为。没有这句话，
// 后来者会把 0.92 当成实测结论去依赖（TD §4 第 1 条明确禁止）。
func TestDefaultThresholdsAreCalibrationPending(t *testing.T) {
	d := openMemoryDB(t)
	cfg := extract.NewDisambiguator(d.Ports(), nil, extract.Config{}).Config()
	if cfg.ThetaHi != 0.92 {
		t.Errorf("默认 θ_hi 应为 0.92（TD §4 起点值），实际 %v", cfg.ThetaHi)
	}
	if cfg.ThetaLo != 0.72 {
		t.Errorf("默认 θ_lo 应为 0.72（TD §4 起点值），实际 %v", cfg.ThetaLo)
	}
	if !(cfg.ThetaLo < cfg.ThetaHi) {
		t.Errorf("灰区不能是空集：θ_lo=%v 必须 < θ_hi=%v", cfg.ThetaLo, cfg.ThetaHi)
	}

	src := readSource(t, "../../extract/config.go")
	i := strings.Index(src, "ThetaHi float64")
	if i < 0 {
		t.Fatalf("extract/config.go 里找不到 ThetaHi 字段声明——测试前提失效")
	}
	start := strings.LastIndex(src[:i], "// ThetaHi")
	if start < 0 {
		t.Fatalf("ThetaHi 缺少文档注释")
	}
	comment := src[start:i]
	mustContain(t, "ThetaHi 的文档注释", comment, "0.92")
	mustContain(t, "ThetaHi 的文档注释", comment, "0.72")
	mustContain(t, "ThetaHi 的文档注释", comment, "配置项")
	// "必须用真实数据校准"这句话是承诺本身；换成"待校准"同义即可。
	if !strings.Contains(comment, "校准") {
		t.Fatalf("ThetaHi 的文档注释必须标明阈值待真实数据校准，实际：\n%s", comment)
	}
}

// TestThetaLoAboveThetaHiFallsBackToNonEmpty 验证"配置反了也不会让灰区变空集"。
//
// 若 θ_lo > θ_hi 被原样接受，灰区就是空集，"灰区绝不合并"这条保证会
// 静默退化成"要么合并要么新建"——最危险的误合并路径反而被打开。
func TestThetaLoAboveThetaHiFallsBackToNonEmpty(t *testing.T) {
	d := openMemoryDB(t)
	cfg := extract.NewDisambiguator(d.Ports(), nil,
		extract.Config{ThetaHi: 0.50, ThetaLo: 0.90}).Config()
	if !(cfg.ThetaLo < cfg.ThetaHi) {
		t.Fatalf("区间反了必须回退到非空灰区，实际 θ_lo=%v θ_hi=%v", cfg.ThetaLo, cfg.ThetaHi)
	}
	// 且同一对候选仍应落进灰区（而不是被并到某一侧）。
	_, raw := openFileDB(t)
	d2, _ := openFileDB(t)
	_ = raw
	seedCandidate(t, d2, "n-vim-edit", candidateName, false)
	res := runZone(t, newZonePipeline(t, d2, extract.Config{ThetaHi: 0.50, ThetaLo: 0.90},
		grayMention(), nil), "e-inverted")
	if got := zoneOf(t, res, candidateName); got.Zone != extract.ZoneGray {
		t.Errorf("区间反转后 s=0.80 仍应落灰区，实际 %q", got.Zone)
	}
}
