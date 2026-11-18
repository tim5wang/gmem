package extract

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/tim5wang/gmem/recall"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// Pipeline：L0 → L1 → L2 → 三区消歧 → 写入（docs/TD.md §3 §4）
//
// # 写入路径的唯一性
//
// 所有 memories/edges 的落库都走 recall.Writer（P1 的写入编排）。
// 本包**没有任何** Store.PutMemory 调用点——这不是风格问题：绕过 Writer 就是
// P0 修过的"FTS 静默漏索引"缺陷（写入成功、检索永远查不到、不报错）。
// 结构性保证比约定更可靠，因此 Pipeline 只持有 *recall.Writer 与 store.Ports，
// 而后者的 PutMemory 在本包里没有调用点（测试会正面验证 FTS 真的能搜到）。
// ===========================================================================

// PipelineOptions 是 Pipeline 的可选注入项。
type PipelineOptions struct {
	// Extractor 是 L2 抽取器。nil 时用 NoopExtractor（显式降级，绝不静默）。
	Extractor Extractor
	// Embedder 是可选的向量能力；nil 时 L1 只用 bigram/BM25、消歧只用 BM25+结构。
	Embedder Embedder
}

// Pipeline 是整理层的编排器。
type Pipeline struct {
	ports  store.Ports
	cfg    Config
	writer *recall.Writer

	l0  *L0
	l1  *L1
	dis *Disambiguator
	ext Extractor
	// emb 保留注入值，用于在结果里**管线级**上报"向量通道缺席"。
	// 为什么不能只在 L1 里报：L1 在没有候选（<2 项）时会提前返回，
	// 于是"没注入 embedder"这件事在真实的小批次里会被静默吞掉。
	emb Embedder
}

// NewPipeline 构造管线。ports 必须含 Store 与 Lexical（recall.Writer 的前置要求）；
// Graph 缺失时带图元素的写入会报错而不是静默丢弃。
func NewPipeline(ports store.Ports, cfg Config, opts PipelineOptions) *Pipeline {
	cfg = cfg.withDefaults()
	ext := opts.Extractor
	if ext == nil {
		ext = NoopExtractor{}
	}
	return &Pipeline{
		ports:  ports,
		cfg:    cfg,
		writer: recall.NewWriter(ports),
		l0:     NewL0(cfg),
		l1:     NewL1(cfg, opts.Embedder),
		dis:    NewDisambiguator(ports, opts.Embedder, cfg),
		ext:    ext,
		emb:    opts.Embedder,
	}
}

// Config 返回生效配置。
func (p *Pipeline) Config() Config { return p.cfg }

// L0 暴露确定性提取器（便于单独测试与宿主预热词典）。
func (p *Pipeline) L0() *L0 { return p.l0 }

// Run 对一段 episode 跑完整管线并落库。
//
// 返回的 PipelineResult 里 Warnings/Degraded/Rejected 都是**必须可见**的状态：
// 这个管线宁可多说一句"我降级了"，也不静默少抽一条。
func (p *Pipeline) Run(ctx context.Context, ep RawEpisode) (PipelineResult, error) {
	res := PipelineResult{EpisodeID: ep.ID}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return res, err
	}
	if !ep.Scope.Valid() {
		return res, fmt.Errorf("extract: invalid scope %q（Scope 是必填的隔离边界）", ep.Scope)
	}

	// ---- 向量通道（一次性、管线级降级上报） ----
	// 未注入 Embedder 不是错误：L0/L1/消歧都能在只有词法的形态下工作。
	// 但它**改变了判定口径**（消歧的 cosine 权重会被剔除），所以必须可见。
	if p.emb == nil {
		res.Degraded = append(res.Degraded,
			"向量通道缺席（未注入 Embedder）：L1 只用 bigram/BM25、消歧只用 BM25+结构一致性；"+
				"消歧余弦权重已按剩余通道重新归一")
	}

	// ---- 词典（L0 第一类规则的来源） ----
	or, _ := p.ports.Store.(store.OntologyReader)
	if or == nil {
		res.Degraded = append(res.Degraded,
			"L0 词典规则缺席：后端未实现 store.OntologyReader（其余六类规则仍生效）")
	}
	dict, err := LoadDictionary(ctx, or)
	if err != nil {
		return res, err
	}
	p.l0.SetDictionary(dict)

	// ---- L0 ----
	l0res, err := p.l0.Extract(ctx, ep)
	if err != nil {
		return res, err
	}
	res.L0 = l0res
	if len(l0res.Rejected) > 0 && len(l0res.Findings) == 0 {
		// 整段拒写：没有任何可写的产物，直接返回（原因已可见）。
		return res, nil
	}

	// ---- 护栏 1/2：节点数与灰区积压 ----
	if w := p.guardNodeRatio(ctx, ep.Scope); w != "" {
		res.Warnings = append(res.Warnings, w)
	}
	verdicts, err := p.dis.loadVerdicts(ctx, ep.Scope)
	if err != nil {
		return res, err
	}
	grayBacklog := verdicts.pendingCount()
	if grayBacklog > p.cfg.GrayBacklogLimit {
		res.SkippedL2 = true
		res.Degraded = append(res.Degraded, fmt.Sprintf(
			"L2 已暂停：灰区待审积压 %d 条 > GrayBacklogLimit=%d，本批仅跑 L0+L1（§4 护栏）",
			grayBacklog, p.cfg.GrayBacklogLimit))
	}

	// ---- L1（只产候选，不做决定） ----
	known := p.knownEntities(ctx, ep.Scope, or)
	clusters, l1Deg, err := p.clusterBatch(ctx, l0res, known)
	if err != nil {
		return res, err
	}
	res.Clusters = clusters
	res.Degraded = append(res.Degraded, l1Deg...)

	// ---- L2 ----
	if !res.SkippedL2 {
		extReq := ExtractRequest{
			Episode:  ep,
			Clusters: clusters,
			Known:    known,
			Budget: Budget{
				MaxTurns: p.cfg.MaxTurns, TokenBudget: p.cfg.TokenBudget, WallClock: p.cfg.WallClock,
			},
		}
		extracted, exErr := p.ext.Extract(ctx, extReq)
		if exErr != nil {
			// L2 失败不得让 L0/L1 的产物一起丢掉：记入 Degraded 后继续写 L0 部分。
			res.Degraded = append(res.Degraded, "L2 抽取失败（L0/L1 产物仍然落库）："+exErr.Error())
		}
		res.Extracted = extracted
		res.Degraded = append(res.Degraded, extracted.Degraded...)
	}

	// ---- 三区消歧 ----
	mentions := buildMentions(l0res, res.Extracted)
	decisions, err := p.dis.Disambiguate(ctx, ep.Scope, mentions)
	if err != nil {
		return res, err
	}
	res.Disambiguation = decisions

	// ---- 写入 ----
	if err := p.writeEpisode(ctx, ep, l0res, res.Extracted, decisions, &res); err != nil {
		return res, err
	}
	return res, nil
}

// knownEntities 枚举本 scope 已有实体摘要（供 L2 与消歧参考）。
func (p *Pipeline) knownEntities(ctx context.Context, scope store.Scope, or store.OntologyReader) []EntityBrief {
	if or == nil {
		return nil
	}
	aliases, err := or.ListAliases(ctx)
	if err != nil {
		return nil
	}
	byNode := make(map[string]*EntityBrief, 16)
	order := make([]string, 0, 16)
	for _, a := range aliases {
		n, err := p.ports.Graph.GetNode(ctx, a.NodeID)
		if err != nil || n.Scope != scope {
			continue
		}
		b, ok := byNode[n.ID]
		if !ok {
			b = &EntityBrief{NodeID: n.ID, Name: n.CanonicalName, ConceptID: n.ConceptID}
			byNode[n.ID] = b
			order = append(order, n.ID)
		}
		b.Aliases = append(b.Aliases, a.Surface)
	}
	sort.Strings(order)
	out := make([]EntityBrief, 0, len(order))
	for _, id := range order {
		out = append(out, *byNode[id])
	}
	if len(out) > 64 {
		out = out[:64] // 提示长度上界：L2 只需要"候选摘要"，不需要全量
	}
	return out
}

// clusterBatch 用 L1 对「本批候选名 + 已有节点名」做疑似同簇聚类。
func (p *Pipeline) clusterBatch(ctx context.Context, l0res L0Result, known []EntityBrief) ([]Cluster, []string, error) {
	items := make([]L1Item, 0, 16)
	seen := make(map[string]struct{}, 16)
	for _, f := range l0res.Findings {
		if f.Category != CatDictionary && f.Category != CatDefinition {
			continue
		}
		name := strings.TrimSpace(f.Text)
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		items = append(items, L1Item{ID: "surface:" + key, Surface: name})
	}
	for _, k := range known {
		key := strings.ToLower(strings.TrimSpace(k.Name))
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		items = append(items, L1Item{ID: k.NodeID, Surface: k.Name, It: true})
	}
	out, err := p.l1.Cluster(ctx, items)
	if err != nil {
		return nil, nil, err
	}
	return out.Clusters, out.Degraded, nil
}

// guardNodeRatio 落实「同 scope 节点数 / episode 数 > 比例 → 告警」。
//
// # 为什么用代理指标（必须说明，否则是隐性缺陷）
//
// P0 的 Ports 没有"枚举全部节点"与"计数 episode"的接口（GraphStore 只有按 id 的
// 邻域查询）。因此这里用两个**可观测代理**：
//   - 节点数：带 alias 的节点数（aliases 是唯一能按 scope 枚举的只读入口）；
//   - episode 数：本管线每接受一段 episode 就写一条 episodic memory，
//     因此 episodic memory 数是 episode 数的下界代理。**分母必须 +1**：
//     护栏在写入前求值，若只数已有记忆，第一段 episode 的分母恒为 0，
//     这条护栏就永远不会生效（比没有更糟：看起来有保护，实际没有）。
//
// 代理会低估节点数、可能高估比率，因此它只用于**告警**（不阻断），
// 并在 Warnings 里写明口径。P3 的维护面接入真正的计数端口后替换这里。
func (p *Pipeline) guardNodeRatio(ctx context.Context, scope store.Scope) string {
	or, ok := p.ports.Store.(store.OntologyReader)
	if !ok || or == nil {
		return ""
	}
	aliases, err := or.ListAliases(ctx)
	if err != nil {
		return "节点/episode 比例护栏跳过：别名枚举失败 " + err.Error()
	}
	seen := make(map[string]struct{}, len(aliases))
	for _, a := range aliases {
		n, err := p.ports.Graph.GetNode(ctx, a.NodeID)
		if err != nil || n.Scope != scope {
			continue
		}
		seen[n.ID] = struct{}{}
	}
	episodes, err := p.ports.Store.ListMemories(ctx, store.MemoryQuery{
		Scope: scope, Kinds: []store.MemoryKind{store.KindEpisodic},
	})
	if err != nil {
		return "节点/episode 比例护栏跳过：episodic 记忆枚举失败 " + err.Error()
	}
	// +1：本次调用即将写入的这条 episodic 记忆（见上面的分母说明）。
	denom := len(episodes) + 1
	ratio := float64(len(seen)) / float64(denom)
	if ratio > p.cfg.NodeEpisodeRatio {
		return fmt.Sprintf(
			"抽取过碎告警：同 scope 节点/episode ≈ %.1f（%d/%d）> NodeEpisodeRatio=%.1f；"+
				"口径=带 alias 的节点数 / （既有 episodic 记忆数 + 本次即将写入的 1 条）"+
				"（代理指标，P3 换真实计数端口）",
			ratio, len(seen), denom, p.cfg.NodeEpisodeRatio)
	}
	return ""
}

// buildMentions 把 L0 与 L2 的实体候选合并成待消歧提及。
func buildMentions(l0res L0Result, extracted ExtractResult) []Mention {
	bySurface := make(map[string]*Mention, 16)
	order := make([]string, 0, 16)
	relBySubject := make(map[string][]store.Relation, 8)

	upsert := func(name string) *Mention {
		key := strings.ToLower(strings.TrimSpace(name))
		if m, ok := bySurface[key]; ok {
			return m
		}
		m := &Mention{Surface: strings.TrimSpace(name)}
		bySurface[key] = m
		order = append(order, key)
		return m
	}

	for _, f := range l0res.Findings {
		switch f.Category {
		case CatDictionary, CatDefinition:
			m := upsert(f.Text)
			if f.Category == CatDefinition && f.Detail != "" && m.Definition == "" {
				m.Definition = f.Detail
			}
		case CatRelation:
			upsert(f.Subject)
			upsert(f.Object)
			relBySubject[strings.ToLower(f.Subject)] = append(
				relBySubject[strings.ToLower(f.Subject)], f.Relation)
			relBySubject[strings.ToLower(f.Object)] = append(
				relBySubject[strings.ToLower(f.Object)], f.Relation)
		}
	}
	for _, e := range extracted.Entities {
		m := upsert(e.Name)
		m.Aliases = append(m.Aliases, e.Aliases...)
		if e.Definition != "" && m.Definition == "" {
			m.Definition = e.Definition
		}
		if e.ConceptID != "" && m.ConceptID == "" {
			m.ConceptID = e.ConceptID
		}
	}
	for _, r := range extracted.Relations {
		relBySubject[strings.ToLower(r.From)] = append(
			relBySubject[strings.ToLower(r.From)], r.Relation)
		relBySubject[strings.ToLower(r.To)] = append(
			relBySubject[strings.ToLower(r.To)], r.Relation)
	}

	out := make([]Mention, 0, len(order))
	for _, key := range order {
		m := bySurface[key]
		if m.Surface == "" {
			continue
		}
		m.RelationTypes = dedupeRelations(relBySubject[key])
		out = append(out, *m)
	}
	return out
}

func dedupeRelations(in []store.Relation) []store.Relation {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[store.Relation]struct{}, len(in))
	out := make([]store.Relation, 0, len(in))
	for _, r := range in {
		if _, dup := seen[r]; dup {
			continue
		}
		seen[r] = struct{}{}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// writeEpisode 把 L0/L2/消歧的结论落库。
//
// 第一步必须是写 episode 本身：episodes 是**不可变事实来源**（§2 第 1 条），
// 而 memories.source_episode 有外键指向它（D15 明确要求保留这条血缘约束）。
// 先写 memories 会直接 FK 失败（787）并把整笔写入回滚。
func (p *Pipeline) writeEpisode(
	ctx context.Context,
	ep RawEpisode,
	l0res L0Result,
	extracted ExtractResult,
	decisions []DisambiguateResult,
	res *PipelineResult,
) error {
	// 0) 事实来源先落地（immutable episodes）。ContentHash 由内容派生，便于去重审计。
	if ep.ID != "" {
		e := ep.Episode()
		e.ContentHash = hash8("episode", string(ep.Scope), ep.Content)
		if err := p.ports.Store.PutEpisode(ctx, e); err != nil {
			return fmt.Errorf("extract: 写入 episode %q: %w", ep.ID, err)
		}
	}

	// 概念建议（含定义句）先落 draft（D15：写入时隐式建 draft 也是同一条路径）。
	var concepts []store.Concept
	seenConcept := make(map[string]struct{}, 8)
	addConcept := func(id, name, def string) {
		if id == "" || name == "" {
			return
		}
		if _, dup := seenConcept[id]; dup {
			return
		}
		seenConcept[id] = struct{}{}
		concepts = append(concepts, store.Concept{
			ID: id, Name: name, Status: store.ConceptDraft,
			Definition: def, Origin: store.ConceptOriginImplicit,
		})
	}
	for _, f := range l0res.Findings {
		if f.Category != CatDefinition {
			continue
		}
		addConcept(conceptIDFromName(f.Text), f.Text, f.Detail)
	}
	for _, c := range extracted.Concepts {
		addConcept(c.ID, c.Name, c.Definition)
	}
	for _, c := range concepts {
		if err := p.ports.Store.PutConcept(ctx, c); err != nil {
			// 概念写入失败不该让事实丢失（D1：本体不拒绝写入），但要可见。
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"概念 %q 落 draft 失败（事实仍会落库）：%v", c.Name, err))
		}
	}

	// ---- 记忆（episodic / decision / semantic） ----
	mems := p.buildMemories(ep, l0res)
	for _, m := range mems {
		rep, err := p.write(ctx, recall.WriteBundle{Memory: m}, nil)
		if err != nil {
			return fmt.Errorf("extract: 写入记忆 %q: %w", m.ID, err)
		}
		res.Writes = append(res.Writes, rep)
	}

	// ---- 实体节点 + alias（消歧结论） ----
	decisionBySurface := make(map[string]DisambiguateResult, len(decisions))
	for _, d := range decisions {
		decisionBySurface[strings.ToLower(strings.TrimSpace(d.Mention))] = d
	}

	var nodes []store.Node
	var aliases []store.Alias
	var edges []store.Edge
	seenNode := make(map[string]struct{}, 8)
	seenAlias := make(map[string]struct{}, 8)
	seenEdge := make(map[string]struct{}, 8)

	mentionBySurface := make(map[string]Mention, len(decisions))
	for _, m := range buildMentions(l0res, extracted) {
		mentionBySurface[strings.ToLower(strings.TrimSpace(m.Surface))] = m
	}

	// 灰区/新建需要新建节点；自动合并只写 alias，不新建节点。
	for _, d := range decisions {
		surface := strings.TrimSpace(d.Mention)
		if surface == "" {
			continue
		}
		m := mentionBySurface[strings.ToLower(surface)]
		newID := NodeID(ep.Scope, surface)

		switch d.Zone {
		case ZoneAutoMerge:
			// 自动合并：**只写 alias**（confidence=high），节点已存在。
			// 护栏 3：单节点 alias 数超 M 时强制人工审 → 降级为灰区，
			// 因为"万能实体"一旦形成，后续所有提及都会被它吸走。
			if warn := p.aliasCapWarning(ctx, ep.Scope, d.NodeID); warn != "" {
				res.Warnings = append(res.Warnings, warn)
				n, w := p.appendGrayCandidate(ctx, ep, newID, surface, m, d.NodeID,
					&nodes, &aliases, &edges, seenNode, seenAlias, seenEdge)
				res.Reviewable += n
				if w != "" {
					res.Warnings = append(res.Warnings, w)
				}
				continue
			}
			if a, ok := makeAlias(surface, d.NodeID, "extract:auto", 0.95, seenAlias); ok {
				aliases = append(aliases, a)
			}
			for _, alt := range m.Aliases {
				if a, ok := makeAlias(alt, d.NodeID, "extract:auto", 0.9, seenAlias); ok {
					aliases = append(aliases, a)
				}
			}
		case ZoneGray:
			// 灰区：**绝不合并**。新建自己的节点 + 一条 possiblySame 边入待审池；
			// 同一对反复写入不重复进池（pair 去重 + 已有 pending 检查）。
			n, w := p.appendGrayCandidate(ctx, ep, newID, surface, m, d.NodeID,
				&nodes, &aliases, &edges, seenNode, seenAlias, seenEdge)
			res.Reviewable += n
			if w != "" {
				res.Warnings = append(res.Warnings, w)
			}
		case ZoneNegative, ZoneNew:
			if n, ok := makeNode(ep, newID, surface, m, seenNode); ok {
				nodes = append(nodes, n)
			}
			if a, ok := makeAlias(surface, newID, "extract:new", 0.8, seenAlias); ok {
				aliases = append(aliases, a)
			}
		}
	}

	// ---- 字面量节点（L0 结构化规则） ----
	seenLiteral := make(map[string]struct{}, 8)
	for _, f := range l0res.Findings {
		if f.Category != CatStructured {
			continue
		}
		key := strings.ToLower(f.Text)
		if _, dup := seenLiteral[key]; dup {
			continue
		}
		seenLiteral[key] = struct{}{}
		id := NodeID(ep.Scope, "literal:"+f.Text)
		if _, dup := seenNode[id]; dup {
			continue
		}
		seenNode[id] = struct{}{}
		nodes = append(nodes, store.Node{
			ID: id, Scope: ep.Scope, CanonicalName: f.Text,
			SourceEpisode: ep.ID,
			PropsJSON:     fmt.Sprintf(`{"kind":"literal","type":%q}`, f.Detail),
		})
	}

	// ---- 关系边（L0 三元组 + L2 关系） ----
	addEdge := func(from, to string, rel store.Relation, conf float64) {
		if from == "" || to == "" || from == to || !relationWhitelisted(rel) {
			return
		}
		id := EdgeID(from, to, rel)
		if _, dup := seenEdge[id]; dup {
			return
		}
		seenEdge[id] = struct{}{}
		edges = append(edges, store.Edge{
			ID: id, FromID: from, ToID: to, Relation: rel, Weight: conf,
			TCreated: p.cfg.now().Unix(),
		})
	}
	for _, f := range l0res.Findings {
		if f.Category != CatRelation {
			continue
		}
		addEdge(NodeID(ep.Scope, f.Subject), NodeID(ep.Scope, f.Object), f.Relation, f.Confidence)
	}
	for _, r := range extracted.Relations {
		addEdge(NodeID(ep.Scope, r.From), NodeID(ep.Scope, r.To), r.Relation, r.Confidence)
	}

	if len(nodes) == 0 && len(edges) == 0 && len(aliases) == 0 {
		return nil
	}

	// 图元素与 aliases 必须与一条记忆同事务落地：
	// recall.Writer 要求 bundle 里至少有一条 Memory（它先写主表再写派生索引）。
	// 因此这里用一个"图元素载体"记忆承载同一事务，内容可解释。
	carrier := store.Memory{
		ID: MemoryID(ep.Scope, "graph:"+ep.ID), Scope: ep.Scope,
		Kind: store.KindEpisodic, Content: graphCarrierContent(ep, nodes, edges, aliases),
		SourceEpisode: ep.ID, TValid: ep.At, TCreated: p.cfg.now().Unix(),
		Importance: 0.3,
	}
	rep, err := p.write(ctx, recall.WriteBundle{
		Memory: carrier, Nodes: nodes, Edges: edges,
	}, aliases)
	if err != nil {
		return fmt.Errorf("extract: 写入图元素/别名: %w", err)
	}
	res.Writes = append(res.Writes, rep)
	return nil
}

// graphCarrierContent 是图元素载体记忆的可读内容。
func graphCarrierContent(ep RawEpisode, nodes []store.Node, edges []store.Edge, aliases []store.Alias) string {
	return fmt.Sprintf("图元素载体：episode=%s 节点 %d 个、边 %d 条、别名 %d 个",
		ep.ID, len(nodes), len(edges), len(aliases))
}

// buildMemories 把 L0 的偏好句/决策句与整段 episode 落成记忆。
//
// 为什么总有且只有一条 episodic 记忆：episode 是**不可变事实来源**（§2 第 1 条），
// 任何抽取产物都要能溯源回它；同时它也提供节点/episode 比例护栏的分母代理。
func (p *Pipeline) buildMemories(ep RawEpisode, l0res L0Result) []store.Memory {
	now := p.cfg.now().Unix()
	out := make([]store.Memory, 0, 3)
	out = append(out, store.Memory{
		ID: MemoryID(ep.Scope, ep.Content), Scope: ep.Scope,
		Kind: store.KindEpisodic, Content: ep.Content,
		SourceEpisode: ep.ID, TValid: ep.At, TCreated: now,
		Importance: importance(l0res.Ephemeral),
	})
	for _, f := range l0res.Findings {
		switch f.Category {
		case CatDecision:
			out = append(out, store.Memory{
				ID: MemoryID(ep.Scope, "decision:"+f.Text), Scope: ep.Scope,
				Kind: store.KindDecision, Content: f.Text,
				SourceEpisode: ep.ID, TValid: ep.At, TCreated: now, Importance: 0.8,
			})
		case CatPreference:
			// store 的 MemoryKind 没有 preference（P0 枚举固定为四类），
			// 因此偏好落成 semantic：内容是"我偏好 X"这整句，语义不改。
			out = append(out, store.Memory{
				ID: MemoryID(ep.Scope, "preference:"+f.Text), Scope: ep.Scope,
				Kind: store.KindSemantic, Content: f.Text,
				SourceEpisode: ep.ID, TValid: ep.At, TCreated: now, Importance: 0.7,
			})
		}
	}
	return out
}

// importance 给临时态降权（时效信号命中）。
func importance(ephemeral bool) float64 {
	if ephemeral {
		return 0.2
	}
	return 0.5
}

// write 是**唯一**写入入口：记忆经 recall.Writer，别名落在同一事务里。
//
// 为什么用外层 Store.Tx 包住 Writer：别名（aliases）不是 FTS 文档，Writer 的
// WriteBundle 没有它的位置；但别名与节点必须原子落地，否则会出现
// "节点存在、词典查不到"（召回侧静默漏命中）。P0 的 Port 契约明确支持这个用法：
// Tx 回调收到的 ctx 携带事务句柄，在回调内调其它 Port 即落入同一事务边界。
func (p *Pipeline) write(ctx context.Context, bundle recall.WriteBundle, aliases []store.Alias) (WriteReport, error) {
	var rep WriteReport
	err := p.ports.Store.Tx(ctx, func(tctx context.Context, tx store.Store) error {
		put, err := p.writer.Put(tctx, bundle)
		if err != nil {
			return err
		}
		rep.MemoryID = put.ID
		rep.Version = put.Version
		for _, n := range bundle.Nodes {
			rep.NodeIDs = append(rep.NodeIDs, n.ID)
		}
		for _, e := range bundle.Edges {
			rep.EdgeIDs = append(rep.EdgeIDs, e.ID)
		}
		for _, a := range aliases {
			if err := tx.PutAlias(tctx, a); err != nil {
				return fmt.Errorf("put alias %q → %q: %w", a.Surface, a.NodeID, err)
			}
			rep.AliasIDs = append(rep.AliasIDs, AliasID(a.Surface, a.NodeID))
		}
		return nil
	})
	if err != nil {
		return WriteReport{}, err
	}
	return rep, nil
}

// makeNode 构造节点；同一批次内重复时返回 ok=false（**不得 append 零值**：
// PutNode 会因空 id/name 校验失败，整笔写入回滚）。
func makeNode(ep RawEpisode, id, name string, m Mention, seen map[string]struct{}) (store.Node, bool) {
	if id == "" || name == "" {
		return store.Node{}, false
	}
	if _, dup := seen[id]; dup {
		return store.Node{}, false
	}
	seen[id] = struct{}{}
	props := "{}"
	if m.Definition != "" {
		props = fmt.Sprintf(`{"definition":%q}`, m.Definition)
	}
	return store.Node{
		ID: id, Scope: ep.Scope, ConceptID: m.ConceptID,
		CanonicalName: name, SourceEpisode: ep.ID, PropsJSON: props,
	}, true
}

// makeAlias 构造词典项；重复或非法时返回 ok=false（同 makeNode 的理由）。
func makeAlias(surface, nodeID, source string, conf float64, seen map[string]struct{}) (store.Alias, bool) {
	surface = strings.TrimSpace(surface)
	if surface == "" || nodeID == "" {
		return store.Alias{}, false
	}
	key := strings.ToLower(surface) + "\x00" + nodeID
	if _, dup := seen[key]; dup {
		return store.Alias{}, false
	}
	seen[key] = struct{}{}
	return store.Alias{Surface: surface, NodeID: nodeID, Source: source, Confidence: conf}, true
}

// possiblySameEdgeIfNew 构造一条灰区候选边（**不是合并**）。
//
// TCreated 必须给：世界时间/系统时间零值会让这条边在 as_of 查询里语义含糊
// （看起来"自古存在"），而它其实只是刚刚才被标记为待审。
func possiblySameEdgeIfNew(from, to, key string, tCreated int64, seen map[string]struct{}) (store.Edge, bool) {
	if from == "" || to == "" || from == to {
		return store.Edge{}, false
	}
	id := "e-ps-" + hash8("possiblySame", key)
	if _, dup := seen[id]; dup {
		return store.Edge{}, false
	}
	seen[id] = struct{}{}
	return store.Edge{
		ID: id, FromID: from, ToID: to, Relation: RelationPossiblySame,
		Weight: 0.5, TCreated: tCreated,
	}, true
}

// appendGrayCandidate 把一条灰区候选落成「新节点 + possiblySame 边」。
//
// 它被两处复用：真正的灰区（θ_lo ≤ s < θ_hi），以及"自动合并被 alias 上限
// 护栏拦下"的降级路径。两处语义相同：**绝不合并**，只入待审池。
//
// 返回 1 表示本次新增了一条待审边（供 Reviewable 计数），0 表示已存在/未入池；
// 第二个返回值是需要可见的警告（邻域不可读时不能静默跳过）。
func (p *Pipeline) appendGrayCandidate(
	ctx context.Context,
	ep RawEpisode, newID, surface string, m Mention, candidateID string,
	nodes *[]store.Node, aliases *[]store.Alias, edges *[]store.Edge,
	seenNode, seenAlias, seenEdge map[string]struct{},
) (int, string) {
	if n, ok := makeNode(ep, newID, surface, m, seenNode); ok {
		*nodes = append(*nodes, n)
	}
	if a, ok := makeAlias(surface, newID, "extract:gray", 0.6, seenAlias); ok {
		*aliases = append(*aliases, a)
	}
	already, err := pairHasPending(ctx, p.ports, ep.Scope, newID, candidateID)
	if err != nil {
		// 邻域不可读时**不猜**：宁可不入池（下次还能再判），也不重复写。
		return 0, fmt.Sprintf(
			"灰区去重检查跳过（%q ↔ %q 邻域不可读）：%v", newID, candidateID, err)
	}
	if already {
		return 0, ""
	}
	e, ok := possiblySameEdgeIfNew(newID, candidateID, pairKey(newID, candidateID),
		p.cfg.now().Unix(), seenEdge)
	if !ok {
		return 0, ""
	}
	*edges = append(*edges, e)
	return 1, ""
}

// pairHasPending 报告这一对节点是否已有进待审池的灰区边（避免重复入池）。
//
// # 必须**双向**检查（这是一个真实缺陷的修复）
//
// GraphStore.Neighbors 只沿 from→to 单向遍历，而灰区边的方向是
// 「新提及节点 → 候选节点」。若只查候选节点的出边，第二次写入同一段文本时
// 会查不到首轮写入的那条入边，于是同一对反复进待审池 —— 正是
// 「同一对反复写入不得重复进待审池」要禁止的行为。
//
// 因此两端各查一次，并按无序对精确匹配：宁可多花一次只读查询，
// 也不要因为方向假设而让待审池无限膨胀。
func pairHasPending(ctx context.Context, ports store.Ports, scope store.Scope, a, b string) (bool, error) {
	want := pairKey(a, b)
	for _, id := range []string{a, b} {
		if id == "" {
			continue
		}
		edges, err := ports.Graph.Neighbors(ctx, store.NeighborQuery{
			NodeID: id, Scope: scope,
			Relations: []store.Relation{RelationPossiblySame}, Hops: HardMaxHops,
		})
		if err != nil {
			// 节点可能还不存在（首次写入时新节点尚未落库）：这不是错误，
			// 那一侧自然没有入池边。只有"存在但读不出邻域"才可能是真问题，
			// 由另一端的结果决定，因此这里不立即失败。
			continue
		}
		for _, hop := range edges {
			if hop.Edge.Relation != RelationPossiblySame {
				continue
			}
			if pairKey(hop.Edge.FromID, hop.Edge.ToID) == want {
				return true, nil
			}
		}
	}
	return false, nil
}

// aliasCapWarning 是护栏 3：单节点 alias 数超 M → 强制人工审。
func (p *Pipeline) aliasCapWarning(ctx context.Context, scope store.Scope, nodeID string) string {
	or, ok := p.ports.Store.(store.OntologyReader)
	if !ok || or == nil {
		return ""
	}
	aliases, err := or.ListAliases(ctx)
	if err != nil {
		return "alias 上限护栏跳过：别名枚举失败 " + err.Error()
	}
	n := 0
	for _, a := range aliases {
		if a.NodeID == nodeID {
			n++
		}
	}
	if n < p.cfg.MaxAliasesPerNode {
		return ""
	}
	return fmt.Sprintf(
		"节点 %q 的 alias 数 %d ≥ MaxAliasesPerNode=%d：已阻止本次自动合并，改入待审池（防万能实体）",
		nodeID, n, p.cfg.MaxAliasesPerNode)
}

// ===========================================================================
// 人工/Agent 裁决（§4 的"待审裁决"）
//
// P2 只做**记账**：裁决为"不同实体"时写 negative_SameAs，让这对以后不再进灰区。
// 完整的 Merge op（迁移边、保留旧 ID 为重定向）属于 P3 的 op 层。
// ===========================================================================

// Verdict 是一次灰区裁决。
type Verdict struct {
	Scope store.Scope
	// KeepNodeID / OtherNodeID 是这对候选的两个节点。
	KeepNodeID  string
	OtherNodeID string
	// Same 为 true 表示"同一实体"，false 表示"不同实体"。
	Same bool
	// Reason 可选，仅用于审计日志（不落库，P3 的 audit_log 负责）。
	Reason string
}

// Judge 应用一条灰区裁决。
//
// 两条路径：
//   - Same=false：写 negative_SameAs 边（**以后不再进灰区**），并把已有的
//     possiblySame 边置为世界时间失效（它不是删除，保留审计）。
//   - Same=true：写 alias（confidence=high）把 other 的名称指向 keep；
//     完整 Merge（迁移边、旧 ID 重定向）是 P3 的 op 层，这里不做。
//
// 写入同样走 recall.Writer（载体记忆 + 边/别名同事务）。
func (p *Pipeline) Judge(ctx context.Context, v Verdict) (WriteReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !v.Scope.Valid() {
		return WriteReport{}, fmt.Errorf("extract: judge: invalid scope %q", v.Scope)
	}
	if v.KeepNodeID == "" || v.OtherNodeID == "" {
		return WriteReport{}, fmt.Errorf("extract: judge: 两个节点 id 都必须给出")
	}
	if v.KeepNodeID == v.OtherNodeID {
		return WriteReport{}, fmt.Errorf("extract: judge: 两个节点 id 相同（%q）", v.KeepNodeID)
	}

	now := p.cfg.now().Unix()
	var edges []store.Edge
	var aliases []store.Alias

	if v.Same {
		n, err := p.ports.Graph.GetNode(ctx, v.OtherNodeID)
		if err != nil {
			return WriteReport{}, fmt.Errorf("extract: judge: 读取节点 %q: %w", v.OtherNodeID, err)
		}
		aliases = append(aliases, store.Alias{
			Surface: n.CanonicalName, NodeID: v.KeepNodeID, Source: "extract:verdict", Confidence: 0.95,
		})
	} else {
		key := pairKey(v.KeepNodeID, v.OtherNodeID)
		edges = append(edges, store.Edge{
			ID:     "e-neg-" + hash8("negative_SameAs", key),
			FromID: v.KeepNodeID, ToID: v.OtherNodeID,
			Relation: RelationNegativeSameAs, Weight: 1, TCreated: now,
		})
		// 已有的 possiblySame 边置失效（世界时间），保留行以便审计。
		if pending, err := p.ports.Graph.Neighbors(ctx, store.NeighborQuery{
			NodeID: v.KeepNodeID, Scope: v.Scope,
			Relations: []store.Relation{RelationPossiblySame}, Hops: HardMaxHops,
		}); err == nil {
			for _, hop := range pending {
				if pairKey(hop.Edge.FromID, hop.Edge.ToID) != key {
					continue
				}
				e := hop.Edge
				e.TInvalid = now
				edges = append(edges, e)
			}
		}
	}

	carrier := store.Memory{
		ID:    MemoryID(v.Scope, fmt.Sprintf("verdict:%s:%s:%t", v.KeepNodeID, v.OtherNodeID, v.Same)),
		Scope: v.Scope, Kind: store.KindSemantic,
		Content: fmt.Sprintf("消歧裁决：%s ↔ %s → %s（%s）",
			v.KeepNodeID, v.OtherNodeID, sameLabel(v.Same), v.Reason),
		TCreated: now, Importance: 0.4,
	}
	return p.write(ctx, recall.WriteBundle{Memory: carrier, Edges: edges}, aliases)
}

func sameLabel(same bool) string {
	if same {
		return "同一实体"
	}
	return "不同实体（写 negative_SameAs，后续不再进灰区）"
}
