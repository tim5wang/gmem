package recall

import (
	"context"
	"testing"

	"github.com/tim5wang/gmem/store"
)

// fakeOntology 是 Aligner 的测试替身：只提供词典与概念枚举，不含任何存储。
type fakeOntology struct {
	concepts []store.Concept
	aliases  []store.Alias
}

func (f *fakeOntology) ListConcepts(context.Context) ([]store.Concept, error) {
	return f.concepts, nil
}

func (f *fakeOntology) ListAliases(context.Context) ([]store.Alias, error) {
	return f.aliases, nil
}

func testOntology() *fakeOntology {
	return &fakeOntology{
		concepts: []store.Concept{
			{ID: "c-db", Name: "数据库", Status: store.ConceptCertified, Definition: "持久化存储数据的系统"},
			{ID: "c-draft", Name: "向量索引", Status: store.ConceptDraft, Definition: "近似最近邻检索结构"},
			{ID: "c-old", Name: "旧方案", Status: store.ConceptDeprecated, Definition: "已经弃用的方案"},
		},
		aliases: []store.Alias{
			{Surface: "Alice", NodeID: "n-alice", Source: "dict", Confidence: 1.0},
			{Surface: "认证服务", NodeID: "n-auth", Source: "dict", Confidence: 0.95},
		},
	}
}

// TestAlignerAliasAndConceptName：对齐必须无 embedder 也能命中 aliases 与 concepts.name。
func TestAlignerAliasAndConceptName(t *testing.T) {
	a := NewAligner(testOntology(), Config{})
	al, err := a.Align(context.Background(), "Alice 的 数据库 怎么配置")
	if err != nil {
		t.Fatalf("Align: %v", err)
	}

	var gotAlias, gotConcept bool
	for _, m := range al.Matched {
		switch m.Method {
		case MatchAlias:
			if m.NodeID == "n-alice" {
				gotAlias = true
			}
		case MatchConceptName:
			if m.ConceptID == "c-db" {
				gotConcept = true
			}
		}
	}
	if !gotAlias {
		t.Fatalf("未命中 alias Alice：matched=%+v", al.Matched)
	}
	if !gotConcept {
		t.Fatalf("未命中 concepts.name 数据库：matched=%+v", al.Matched)
	}
	if len(al.Anchors) == 0 || al.Anchors[0] != "n-alice" {
		t.Fatalf("Anchors=%v，want [n-alice]", al.Anchors)
	}
	// "怎么配置" 是残槽：库里没有这个维度，必须出现在 unmatched 里而不是被吞掉。
	if len(al.Unmatched) == 0 {
		t.Fatalf("Unmatched 为空：未对齐槽位必须被显式记录（R4 的来源）")
	}
	if al.Coverage <= 0 || al.Coverage >= 1 {
		t.Fatalf("Coverage=%.3f，want (0,1)：仍有未对齐槽位", al.Coverage)
	}
}

// TestAlignerDefinitionSimilarity：定义句相似度必须能在**没有 alias 命中**时对齐概念。
func TestAlignerDefinitionSimilarity(t *testing.T) {
	// "数据持久化" 不是任何 concept name/alias，但 bigram 高度重合 "持久化存储数据的系统"。
	a := NewAligner(testOntology(), Config{})
	al, err := a.Align(context.Background(), "数据持久化 怎么做")
	if err != nil {
		t.Fatalf("Align: %v", err)
	}
	found := false
	for _, m := range al.Matched {
		if m.Method == MatchDefinition && m.ConceptID == "c-db" {
			found = true
		}
	}
	if !found {
		t.Fatalf("定义句相似度未命中 c-db：matched=%+v", al.Matched)
	}
}

// TestAlignerStatusFactor：R2 —— draft 打折、deprecated 归零，但都不阻止召回。
func TestAlignerStatusFactor(t *testing.T) {
	a := NewAligner(testOntology(), Config{})
	cfg := a.cfg

	al, err := a.Align(context.Background(), "向量索引")
	if err != nil {
		t.Fatalf("Align: %v", err)
	}
	var draft Match
	for _, m := range al.Matched {
		if m.ConceptID == "c-draft" {
			draft = m
		}
	}
	if draft.Method == "" {
		t.Fatalf("draft 概念未参与对齐：matched=%+v", al.Matched)
	}
	if draft.StatusFactor != cfg.StatusFactors[store.ConceptDraft] {
		t.Fatalf("draft StatusFactor=%.2f，want %.2f（待校准表）", draft.StatusFactor, cfg.StatusFactors[store.ConceptDraft])
	}

	al2, err := a.Align(context.Background(), "旧方案")
	if err != nil {
		t.Fatalf("Align: %v", err)
	}
	var dep Match
	for _, m := range al2.Matched {
		if m.ConceptID == "c-old" {
			dep = m
		}
	}
	if dep.Method == "" {
		t.Fatalf("deprecated 概念未出现在 matched 里（应在，只是质量为 0）：%+v", al2.Matched)
	}
	if dep.Weight != 0 {
		t.Fatalf("deprecated 概念 Weight=%.3f，want 0", dep.Weight)
	}
}

// TestAlignerNoOntologyReader：后端没实现 OntologyReader 时必须显式降级。
func TestAlignerNoOntologyReader(t *testing.T) {
	a := NewAligner(nil, Config{})
	al, err := a.Align(context.Background(), "随便问问")
	if err != nil {
		t.Fatalf("Align: %v", err)
	}
	if len(al.Degraded) == 0 {
		t.Fatalf("Degraded 为空：缺少本体枚举必须显式上报，不得静默")
	}
}

// TestRelationHopsDeclarative：跳数由 relation 传递性表决定，不是全局常量。
func TestRelationHopsDeclarative(t *testing.T) {
	p := NewPlanner(Config{})
	want := map[store.Relation]struct {
		hops       int
		transitive bool
	}{
		store.RelationSubtypeOf: {3, true},
		store.RelationPartOf:    {3, true},
		store.RelationDependsOn: {3, true},
		store.RelationCausedBy:  {2, true},
		store.RelationWorksAt:   {1, false},
		store.RelationOwns:      {1, false},
	}
	for rel, w := range want {
		rule, ok := p.rule(rel)
		if !ok {
			t.Fatalf("关系 %q 不在声明式表内", rel)
		}
		if rule.MaxHops != w.hops || rule.Transitive != w.transitive {
			t.Fatalf("%s: got hops=%d transitive=%v, want %d/%v", rel, rule.MaxHops, rule.Transitive, w.hops, w.transitive)
		}
	}
	// 换表即换策略：证明跳数不是硬编码。
	custom := Config{RelationRules: []RelationRule{{Relation: store.RelationOwns, Transitive: true, MaxHops: 4, Weight: 1}}}
	cp := NewPlanner(custom)
	rule, _ := cp.rule(store.RelationOwns)
	if rule.MaxHops != 4 {
		t.Fatalf("自定义关系表未生效：hops=%d, want 4", rule.MaxHops)
	}
}

// TestPlannerLaneRulesDeclarative：lane 权重表可整表替换，且权重来自表而非分支。
func TestPlannerLaneRulesDeclarative(t *testing.T) {
	p := NewPlanner(Config{})
	rules := map[QueryShape]map[LaneName]float64{
		ShapeFullWithAnchor: {LaneGraph: 0.5, LaneVector: 0.3, LaneLexical: 0.2},
		ShapeFullNoAnchor:   {LaneLexical: 0.6, LaneVector: 0.4},
		ShapeLiteral:        {LaneLexical: 1.0},
		ShapeRelational:     {LaneGraph: 0.7, LaneVector: 0.3},
	}
	for shape, want := range rules {
		got := p.laneWeights(shape)
		if len(got) != len(want) {
			t.Fatalf("%s: got %d lanes, want %d", shape, len(got), len(want))
		}
		for _, lw := range got {
			if want[lw.Lane] != lw.Weight {
				t.Fatalf("%s/%s: weight=%.2f, want %.2f", shape, lw.Lane, lw.Weight, want[lw.Lane])
			}
		}
	}

	// 声明式替换：把 relational 的权重倒过来，Planner 必须读到新值。
	custom := Config{LaneRules: []LaneRule{{
		Shape:  ShapeRelational,
		Lanes:  []LaneWeight{{Lane: LaneVector, Weight: 0.9}, {Lane: LaneGraph, Weight: 0.1}},
		Reason: "自定义",
	}}}
	cp := NewPlanner(custom)
	got := cp.laneWeights(ShapeRelational)
	if got[0].Lane != LaneVector || got[0].Weight != 0.9 {
		t.Fatalf("自定义 lane 表未生效：%+v", got)
	}
}

// TestPlannerShapeAndSkip：lane 是否执行取决于执行能力，且跳过原因必须可读。
func TestPlannerShapeAndSkip(t *testing.T) {
	p := NewPlanner(Config{})
	al := Alignment{
		Query: "Alice 用什么编辑器", Normalized: "alice 用什么编辑器",
		Slots: []Slot{
			{Kind: SlotEntity, Text: "Alice", Required: true, Weight: 1},
			{Kind: SlotRelation, Text: "用什么", Required: true, Weight: 1.2},
		},
		Matched: []Match{
			{Kind: SlotEntity, Text: "Alice", NodeID: "n-alice", Method: MatchAlias, Quality: 1, StatusFactor: 1, Weight: 1},
			{Kind: SlotRelation, Text: "用什么", Relation: store.RelationUses, Method: MatchRelationCue, Quality: 1, StatusFactor: 1, Weight: 1.2},
		},
		Anchors:       []string{"n-alice"},
		RelationHints: []store.Relation{store.RelationUses},
		Coverage:      1,
	}
	pl := p.makePlan(planInput{Align: al, Request: RecallRequest{Query: al.Query, Scope: store.ScopeUser}, HasEmbedder: false, GraphScope: store.ScopeUser})
	if pl.Shape != ShapeRelational {
		t.Fatalf("Shape=%s, want relational", pl.Shape)
	}
	if pl.Hops != 1 {
		t.Fatalf("Hops=%d, want 1（uses 白名单 1 跳）", pl.Hops)
	}
	var vectorSkipped bool
	for _, lp := range pl.Lanes {
		if lp.Lane == LaneVector && !lp.Executed {
			vectorSkipped = true
			if lp.SkipReason == "" {
				t.Fatalf("vector lane 被跳过但没有 SkipReason")
			}
		}
	}
	if !vectorSkipped {
		t.Fatalf("无 embedder 时 vector lane 必须标记为未执行")
	}
	if len(pl.Steps) == 0 {
		t.Fatalf("Plan.Steps 为空")
	}
}
