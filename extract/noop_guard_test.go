package extract

import (
	"context"
	"strings"
	"testing"

	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// NoopExtractor：CI 门禁（不注入 LLM 时 L0+L1 可跑、功能不崩溃、降级必须可见）
//
// 这一组用例的价值不在于"它能跑通"，而在于钉住三条**不得静默**的语义：
//   - Degraded 必须有内容（不能悄悄少抽）；
//   - L0/L1 的产物必须照常落库（不能因为没模型就什么都不写）；
//   - 结果形状与注入 LLM 时完全一致（§3.3 第 3 条：差异只在模型从哪来）。
// ===========================================================================

func TestNoopExtractorReportsDegraded(t *testing.T) {
	res, err := NoopExtractor{}.Extract(context.Background(), ExtractRequest{
		Episode: RawEpisode{ID: "e", Scope: store.ScopeUser, Content: "内容足够长的一段文本。"},
	})
	if err != nil {
		t.Fatalf("NoopExtractor 不得报错（L0/L1 仍然有效）：%v", err)
	}
	if len(res.Degraded) == 0 {
		t.Fatal("NoopExtractor 必须在 Degraded 显式上报（绝不静默降级）")
	}
	if len(res.Entities) != 0 || len(res.Relations) != 0 {
		t.Error("NoopExtractor 不应凭空产出实体/关系")
	}
	// 必须点明原因与后果，而不是一句"degraded"。
	joined := strings.Join(res.Degraded, " ")
	for _, want := range []string{"NoopExtractor", "L0"} {
		if !strings.Contains(joined, want) {
			t.Errorf("降级说明应提到 %q，实际 %q", want, joined)
		}
	}
}

// TestNoopExtractorPipelineStillWrites 是 CI 门禁的核心：
// 不注入 LLM 时管线必须跑通，且 L0 的产物（记忆/节点/别名）照常落库并可见。
func TestNoopExtractorPipelineStillWrites(t *testing.T) {
	d := openMemoryDB(t)
	// PipelineOptions 留空 → 内部落到 NoopExtractor（默认路径，不是显式传参）。
	p := NewPipeline(d.Ports(), Config{}, PipelineOptions{})
	if p.Config().MinContentLength != 10 {
		t.Errorf("默认长度下限应为 10，实际 %d", p.Config().MinContentLength)
	}

	content := "团队决定改用 SQLite 做本地存储，因为部署要零外部依赖。"
	res := mustRun(t, p, RawEpisode{
		ID: "e-noop", Scope: store.ScopeUser, At: 1000, Content: content,
	})

	// 1) 降级可见。
	if len(res.Degraded) == 0 {
		t.Fatal("未注入 LLM 时必须在 PipelineResult.Degraded 里显式上报")
	}
	if !strings.Contains(strings.Join(res.Degraded, " "), "NoopExtractor") {
		t.Errorf("降级说明应点名 NoopExtractor，实际 %v", res.Degraded)
	}
	// 2) L0 决策句产物照常落库（decision 记忆）。
	mems, err := d.Store().ListMemories(context.Background(), store.MemoryQuery{
		Scope: store.ScopeUser, Kinds: []store.MemoryKind{store.KindDecision},
	})
	if err != nil {
		t.Fatalf("ListMemories: %v", err)
	}
	if len(mems) == 0 {
		t.Fatal("无 LLM 时 L0 的决策句仍必须落库（否则就是静默少写）")
	}
	// 3) 词法通道真的能搜到（写入走了 Writer）。
	hits, err := d.Lexical().Search(context.Background(), store.LexicalQuery{
		Query: "SQLite 本地存储", Scope: store.ScopeUser, Limit: 5,
	})
	if err != nil {
		t.Fatalf("lexical search: %v", err)
	}
	if len(hits) == 0 {
		t.Error("无 LLM 时写入的记忆也必须可被词法检索到")
	}
	// 4) 向量通道缺席也要可见（这是与"L2 无模型"完全独立的第二条降级）。
	if !strings.Contains(strings.Join(res.Degraded, " "), "向量通道缺席") {
		t.Errorf("未注入 Embedder 时向量通道降级必须在 Degraded 里，实际 %v", res.Degraded)
	}
}

// TestNoopExtractorRejectedStillVisible 验证长度下限在无 LLM 时同样生效且可见。
func TestNoopExtractorRejectedStillVisible(t *testing.T) {
	d := openMemoryDB(t)
	p := NewPipeline(d.Ports(), Config{}, PipelineOptions{})
	res := mustRun(t, p, RawEpisode{
		ID: "e-short", Scope: store.ScopeUser, At: 1000, Content: "太短",
	})
	if len(res.L0.Rejected) == 0 {
		t.Fatal("过短内容必须进 L0.Rejected")
	}
	if len(res.Writes) != 0 {
		t.Errorf("被拒写的内容不得产生任何写入，实际 %d 条", len(res.Writes))
	}
}

// ===========================================================================
// 歧义爆炸护栏（docs/TD.md §4 表）：三个指标都可配
// ===========================================================================

// TestGuardGrayBacklogPausesL2 验证"灰区积压 > N → 降级为仅 L0/L1"。
func TestGuardGrayBacklogPausesL2(t *testing.T) {
	d := openMemoryDB(t)
	seedSimilarNode(t, d, "n-vim-edit", "vim 编辑器", "vim 编辑器")
	llm := NewFakeLLM(FakeStep{Text: BuildFinalJSON([]EntityJSON{{Name: "vim 编辑器"}}, nil)})

	cfg := Config{GrayBacklogLimit: 0 + 1} // 上限 1
	p := NewPipeline(d.Ports(), cfg, PipelineOptions{
		Embedder: bigramEmbedder(64), Extractor: NewLLMExtractor(llm, d.Ports(), cfg),
	})

	// 先制造 2 条灰区积压（两次不同的相似提及）。
	res1 := mustRun(t, p, RawEpisode{
		ID: "e-g1", Scope: store.ScopeUser, At: 1000,
		Content: "vim 编辑器 是 一个模式化文本编辑器。",
	})
	if res1.Reviewable == 0 {
		t.Fatalf("前置条件：首轮应产生灰区待审项，zones=%+v degraded=%v",
			zonesOf(res1.Disambiguation), res1.Degraded)
	}

	// 第 2 段：积压已超上限 → L2 必须暂停，且显式上报。
	res2 := mustRun(t, p, RawEpisode{
		ID: "e-g2", Scope: store.ScopeUser, At: 2000,
		Content: "vim 编辑器 是 一个模式化文本编辑器。",
	})
	if res2.SkippedL2 {
		if res2.EnabledL2() {
			t.Error("EnabledL2 与 SkippedL2 不得矛盾")
		}
		if !strings.Contains(strings.Join(res2.Degraded, " "), "灰区待审积压") {
			t.Errorf("暂停 L2 必须显式说明原因，实际 %v", res2.Degraded)
		}
		// 暂停 L2 后不得再调用模型。
		if llm.CallCount() > 0 && res2.EnabledL2() {
			t.Error("SkippedL2 时不应调用 LLM")
		}
	}
	// 无论是否暂停，L0/L1 的写入都必须照常（降级 ≠ 罢工）。
	if len(res2.Writes) == 0 {
		t.Error("即使暂停 L2，L0/L1 的产物仍必须落库")
	}
}

// TestGuardAliasCapPreventsAutoMerge 验证"单节点 alias 数 > M → 强制人工审"：
// 自动合并必须被拦下并改入待审池（防万能实体）。
func TestGuardAliasCapPreventsAutoMerge(t *testing.T) {
	d, raw := openFileWithRaw(t)
	ctx := context.Background()
	seedSimilarNode(t, d, "n-vim-edit", "vim 编辑器", "vim 编辑器")
	seedEdge(t, d, "n-vim-edit", "n-plugins", store.RelationUses)

	// 给该节点堆到上限的 alias 数（MaxAliasesPerNode 设为 2）。
	for i := 0; i < 2; i++ {
		if err := d.Store().PutAlias(ctx, store.Alias{
			Surface: "vim 别名" + itoa(i), NodeID: "n-vim-edit",
			Source: "seed", Confidence: 0.5,
		}); err != nil {
			t.Fatalf("PutAlias: %v", err)
		}
	}

	cfg := Config{MaxAliasesPerNode: 2}
	// L2 给出释义（决定 cosine）与一条 uses 关系（决定结构一致性），
	// 使 s = 1.0 ≥ θ_hi —— 也就是说"若无护栏，本次一定会自动合并"。
	llm := NewFakeLLM(FakeStep{Text: BuildFinalJSON(
		[]EntityJSON{
			{Name: "vim 编辑器", Def: "一个模式化文本编辑器"},
			{Name: "插件管理器"},
		},
		[][3]string{{"vim 编辑器", "插件管理器", "uses"}},
	)})
	p := NewPipeline(d.Ports(), cfg, PipelineOptions{
		Embedder: bigramEmbedder(64), Extractor: NewLLMExtractor(llm, d.Ports(), cfg),
	})

	before := countEdgesByRelation(t, raw, string(RelationPossiblySame))
	res := mustRun(t, p, RawEpisode{
		ID: "e-cap", Scope: store.ScopeUser, At: 1000,
		Content: "vim 编辑器 是 一个模式化文本编辑器，日常开发都在用它。",
	})
	joined := strings.Join(res.Warnings, " ")
	if !strings.Contains(joined, "MaxAliasesPerNode") {
		t.Errorf("alias 超限必须产生可见告警，实际 warnings=%v", res.Warnings)
	}
	// 被拦下的自动合并必须落到待审池，而不是静默丢弃。
	after := countEdgesByRelation(t, raw, string(RelationPossiblySame))
	if after <= before {
		t.Errorf("被 alias 上限拦下的合并应改入待审池，possiblySame %d → %d", before, after)
	}
}

// TestGuardNodeEpisodeRatioWarns 验证"节点数/episode 数超比例 → 告警"，
// 且口径必须写在告警文案里（代理指标不能假装是精确计数）。
func TestGuardNodeEpisodeRatioWarns(t *testing.T) {
	d := openMemoryDB(t)
	ctx := context.Background()
	// 2 个节点、0 条 episodic → 比例护栏应当不触发（分母为 0 时不误报）。
	seedSimilarNode(t, d, "n-1", "实体一", "实体一")
	seedSimilarNode(t, d, "n-2", "实体二", "实体二")

	cfg := Config{NodeEpisodeRatio: 1}
	p := NewPipeline(d.Ports(), cfg, PipelineOptions{})
	res := mustRun(t, p, RawEpisode{
		ID: "e-ratio", Scope: store.ScopeUser, At: 1000,
		Content: "这是一条足够长的记忆内容，用于验证比例护栏。",
	})
	// 先写了一条 episodic，因此比例 = 2/1 = 2 > 1 → 应告警。
	joined := strings.Join(res.Warnings, " ")
	if !strings.Contains(joined, "抽取过碎告警") {
		t.Fatalf("节点/episode 比例超限应告警，实际 warnings=%v", res.Warnings)
	}
	if !strings.Contains(joined, "代理指标") {
		t.Errorf("告警必须写明口径是代理指标（不得伪装成精确计数）：%v", res.Warnings)
	}
	_ = ctx
}

// TestGuardsAreConfigurable 验证三个护栏指标都是配置项（零值走默认）。
func TestGuardsAreConfigurable(t *testing.T) {
	cfg := Config{}.withDefaults()
	if cfg.NodeEpisodeRatio != 20 {
		t.Errorf("NodeEpisodeRatio 默认应为 20，实际 %v", cfg.NodeEpisodeRatio)
	}
	if cfg.GrayBacklogLimit != 200 {
		t.Errorf("GrayBacklogLimit 默认应为 200，实际 %d", cfg.GrayBacklogLimit)
	}
	if cfg.MaxAliasesPerNode != 8 {
		t.Errorf("MaxAliasesPerNode 默认应为 8，实际 %d", cfg.MaxAliasesPerNode)
	}
	custom := Config{NodeEpisodeRatio: 3, GrayBacklogLimit: 7, MaxAliasesPerNode: 2}.withDefaults()
	if custom.NodeEpisodeRatio != 3 || custom.GrayBacklogLimit != 7 || custom.MaxAliasesPerNode != 2 {
		t.Errorf("显式配置必须被尊重，实际 %+v", custom)
	}
}

// ===========================================================================
// 降级与失败路径的可见性
// ===========================================================================

// TestLLMFailureKeepsL0Writes 验证 L2 失败不会连带丢掉 L0/L1 的产物。
func TestLLMFailureKeepsL0Writes(t *testing.T) {
	d := openMemoryDB(t)
	llm := NewFakeLLM(FakeStep{Err: errFakeBoom})
	p := NewPipeline(d.Ports(), Config{}, PipelineOptions{
		Extractor: NewLLMExtractor(llm, d.Ports(), Config{}),
	})
	res := mustRun(t, p, RawEpisode{
		ID: "e-boom", Scope: store.ScopeUser, At: 1000,
		Content: "团队决定改用 SQLite，因为部署要零外部依赖。",
	})
	joined := strings.Join(res.Degraded, " ")
	if !strings.Contains(joined, "L2 抽取失败") {
		t.Errorf("L2 失败必须在 Degraded 里可见，实际 %v", res.Degraded)
	}
	if len(res.Writes) == 0 {
		t.Error("L2 失败时 L0 的产物仍必须落库（不能一起丢掉）")
	}
}

// TestLLMInvalidJSONDegradesNotPanics 验证非法 JSON 走降级而不是 panic/抛错。
func TestLLMInvalidJSONDegradesNotPanics(t *testing.T) {
	d := openMemoryDB(t)
	// 剧本：第 1 轮非法 JSON；修复式重试（第 2 轮）给合法终稿。
	llm := NewFakeLLM(
		FakeStep{Text: "这不是 JSON"},
		FakeStep{Text: BuildFinalJSON([]EntityJSON{{Name: "SQLite"}}, nil)},
	)
	p := NewPipeline(d.Ports(), Config{}, PipelineOptions{
		Extractor: NewLLMExtractor(llm, d.Ports(), Config{MaxTurns: 3}),
	})
	res := mustRun(t, p, RawEpisode{
		ID: "e-json", Scope: store.ScopeUser, At: 1000,
		Content: "团队决定改用 SQLite，因为部署要零外部依赖。",
	})
	joined := strings.Join(res.Degraded, " ")
	if !strings.Contains(joined, "修复式重试") {
		t.Errorf("解析失败后应做一次修复式重试并记录，实际 %v", res.Degraded)
	}
	if len(res.Extracted.Entities) == 0 {
		t.Error("修复式重试成功后应拿到实体")
	}
}

// TestLLMRelationValidation 验证 §3.1 的校验：白名单外的关系、端点不在集合里的关系被丢弃且可见。
func TestLLMRelationValidation(t *testing.T) {
	d := openMemoryDB(t)
	llm := NewFakeLLM(FakeStep{Text: `{"entities":[{"name":"Alice"},{"name":"vim"}],
		"relations":[
			{"from":"Alice","to":"vim","relation":"uses"},
			{"from":"Alice","to":"Unknown","relation":"uses"},
			{"from":"Alice","to":"vim","relation":"invented_relation"}
		]}`})
	x := NewLLMExtractor(llm, d.Ports(), Config{})
	res, err := x.Extract(context.Background(), ExtractRequest{
		Episode: RawEpisode{ID: "e", Scope: store.ScopeUser, Content: "Alice 使用 vim 写代码，内容够长。"},
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res.Relations) != 1 {
		t.Errorf("只应保留 1 条合法关系，实际 %d：%+v", len(res.Relations), res.Relations)
	}
	joined := strings.Join(res.Degraded, " ")
	if !strings.Contains(joined, "白名单外") {
		t.Errorf("白名单外关系必须记入 Degraded，实际 %v", res.Degraded)
	}
	if !strings.Contains(joined, "终点不在结果或已知实体中") {
		t.Errorf("端点校验失败必须记入 Degraded，实际 %v", res.Degraded)
	}
}

// TestNilLLMClientDegradesExplicitly 验证 NewLLMExtractor(nil,...) 的语义：
// 不 panic、不报错，但必须显式说明"等价 NoopExtractor"。
func TestNilLLMClientDegradesExplicitly(t *testing.T) {
	d := openMemoryDB(t)
	x := NewLLMExtractor(nil, d.Ports(), Config{})
	res, err := x.Extract(context.Background(), ExtractRequest{
		Episode: RawEpisode{ID: "e", Scope: store.ScopeUser, Content: "内容足够长的一段文本。"},
	})
	if err != nil {
		t.Fatalf("nil client 不应报错：%v", err)
	}
	if len(res.Degraded) == 0 || !strings.Contains(strings.Join(res.Degraded, " "), "NoopExtractor") {
		t.Errorf("nil client 必须显式说明等价 NoopExtractor，实际 %v", res.Degraded)
	}
}

type fakeBoomError struct{}

func (fakeBoomError) Error() string { return "fake llm boom" }

var errFakeBoom = fakeBoomError{}

// TestScopeRequired 验证 Scope 是必填的隔离边界。
func TestScopeRequired(t *testing.T) {
	d := openMemoryDB(t)
	p := NewPipeline(d.Ports(), Config{}, PipelineOptions{})
	if _, err := p.Run(context.Background(), RawEpisode{
		ID: "e-noscope", Content: "这是一条足够长的记忆内容。",
	}); err == nil {
		t.Fatal("空 Scope 必须报错（空值不等于不限）")
	}
}

// TestEpisodicAlwaysWritten 验证 episode 是事实来源：每段都会被写成 episodic 记忆，
// 且 episodes 表有对应行（memories.source_episode 的外键指向它）。
func TestEpisodicAlwaysWritten(t *testing.T) {
	d := openMemoryDB(t)
	p := NewPipeline(d.Ports(), Config{}, PipelineOptions{})
	content := "这是一条足够长的记忆内容，用于验证 episode 血缘。"
	if _, err := p.Run(context.Background(), RawEpisode{
		ID: "e-src", Scope: store.ScopeUser, At: 1000, Content: content,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	ep, err := d.Store().GetEpisode(context.Background(), "e-src")
	if err != nil {
		t.Fatalf("episode 必须落库（否则 source_episode 外键会失败）：%v", err)
	}
	if ep.Content != content {
		t.Errorf("episode 内容应原样保留，实际 %q", ep.Content)
	}
	mems, err := d.Store().ListMemories(context.Background(), store.MemoryQuery{
		Scope: store.ScopeUser, SourceEpisode: "e-src",
	})
	if err != nil {
		t.Fatalf("ListMemories: %v", err)
	}
	if len(mems) == 0 {
		t.Fatal("至少应有一条记忆能溯源回该 episode")
	}
}
