package extract

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/tim5wang/gmem/bigram"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// 三区消歧（docs/TD.md §4）
//
//	s = w1·cosine(emb(name+definition)) + w2·BM25(name, aliases) + w3·结构一致性
//
//	s ≥ θ_hi          → 自动合并（写 alias，confidence=high）
//	θ_lo ≤ s < θ_hi   → 灰区（写 possiblySame 边进待审池，**绝不合并**）
//	s < θ_lo          → 新建节点
//
// **灰区绝不自动合并**是本节存在的全部理由：误合并是记忆系统最难恢复的错误
// （两条事实混一起，事后无法拆开）。CortexDB 的做法值得照抄——链接 + 待审，而不是猜。
//
// negative_SameAs（§4 第 3 条）：一旦某 (surface, 节点) 对被人/Agent 裁决为
// "不同实体"，后续不再进灰区。没有它，"张三(A)"和"张三(B)"会反复被重判。
// ===========================================================================

// Disambiguator 做三区判定。它**只读** ports（外加可选 embedder），
// 所有写入由 Pipeline 经 recall.Writer 完成。
type Disambiguator struct {
	ports store.Ports
	emb   Embedder
	cfg   Config
}

// NewDisambiguator 构造消歧器。emb 可为 nil：此时 cosine 通道缺席，
// 分项会记 0 并在结果 Warnings 里显式报告（不静默）。
func NewDisambiguator(ports store.Ports, emb Embedder, cfg Config) *Disambiguator {
	return &Disambiguator{ports: ports, emb: emb, cfg: cfg.withDefaults()}
}

// Config 返回生效配置（含 θ_hi / θ_lo，便于调用方校验）。
func (d *Disambiguator) Config() Config { return d.cfg }

// candidate 是一个消歧候选（已有节点）。
type candidate struct {
	node    store.Node
	aliases []store.Alias
	// relationTypes 是该节点在图中已有的关系类型（结构一致性信号）。
	relationTypes map[store.Relation]struct{}
}

// Disambiguate 对一组提及做三区判定。
//
// 返回值里的 Zone / NodeID 是**结论**，但只有 ZoneAutoMerge 允许合并；
// ZoneGray 只表示"要写 possiblySame"，由 Pipeline 落库。
func (d *Disambiguator) Disambiguate(ctx context.Context, scope store.Scope, mentions []Mention) ([]DisambiguateResult, error) {
	out := make([]DisambiguateResult, 0, len(mentions))
	if len(mentions) == 0 {
		return out, nil
	}
	cands, warnings, err := d.loadCandidates(ctx, scope)
	if err != nil {
		return nil, err
	}
	verdicts, err := d.loadVerdicts(ctx, scope)
	if err != nil {
		return nil, err
	}

	// 单批内查重：同一次抽取里同一 surface 只判一次（避免同一提及写两遍）。
	seen := make(map[string]struct{}, len(mentions))
	for _, m := range mentions {
		key := strings.ToLower(strings.TrimSpace(m.Surface))
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}

		res := d.scoreMention(ctx, scope, m, cands, verdicts)
		res.Warnings = append(res.Warnings, warnings...)
		out = append(out, res)
	}
	return out, nil
}

// loadCandidates 枚举同 scope 的候选节点（含其 alias 与已有关系类型）。
//
// 候选来源是 aliases 词典（唯一能按 scope 枚举的只读入口）：没有词典项的节点
// 无法按名称比对，因此不进入候选池。**这不是遗漏而是契约限制**，在 §13 风险清单里
// 记为"未接入候选枚举端口"——P3 的维护面会有专门查询，P2 不做。
func (d *Disambiguator) loadCandidates(ctx context.Context, scope store.Scope) ([]candidate, []string, error) {
	or, ok := d.ports.Store.(store.OntologyReader)
	if !ok || or == nil {
		return nil, []string{
			"消歧候选池为空：后端未实现 store.OntologyReader，无法枚举词典（属显式降级）",
		}, nil
	}
	aliases, err := or.ListAliases(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("extract: list aliases: %w", err)
	}
	var warnings []string
	byNode := make(map[string]*candidate, 16)
	order := make([]string, 0, 16)
	for _, a := range aliases {
		n, err := d.ports.Graph.GetNode(ctx, a.NodeID)
		if err != nil {
			// 词典指向不存在/不可读的节点：记警告继续，不中断整批判定。
			warnings = append(warnings, fmt.Sprintf("候选枚举：alias %q 指向的节点 %q 不可读（%v）",
				a.Surface, a.NodeID, err))
			continue
		}
		if n.Scope != scope {
			continue // 范围隔离：跨 scope 的节点绝不进候选池
		}
		c, ok := byNode[n.ID]
		if !ok {
			c = &candidate{node: n, relationTypes: make(map[store.Relation]struct{}, 4)}
			byNode[n.ID] = c
			order = append(order, n.ID)
		}
		c.aliases = append(c.aliases, a)
	}
	// 已有关系类型：逐个候选取 1 跳邻域（hops 硬上限 1，见 HardMaxHops）。
	for _, id := range order {
		c := byNode[id]
		edges, err := d.ports.Graph.Neighbors(ctx, store.NeighborQuery{
			NodeID: id, Scope: scope, Hops: HardMaxHops,
		})
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("候选枚举：节点 %q 的一跳邻域不可读（%v）", id, err))
			continue
		}
		for _, hop := range edges {
			c.relationTypes[hop.Edge.Relation] = struct{}{}
		}
	}

	sort.Strings(order)
	cands := make([]candidate, 0, len(order))
	for _, id := range order {
		cands = append(cands, *byNode[id])
	}
	if len(cands) > d.cfg.CandidateLimit {
		warnings = append(warnings, fmt.Sprintf(
			"候选池 %d 个节点超过上限 %d，按 id 序截断（消歧范围缩小，属显式降级）",
			len(cands), d.cfg.CandidateLimit))
		cands = cands[:d.cfg.CandidateLimit]
	}
	return cands, warnings, nil
}

// verdictSet 是已存在的消歧记账边：negative_SameAs 与 possiblySame 两类。
//
// 两类都要读：negative 决定"不再进灰区"（§4 第 3 条），possiblySame 决定
// "同一对反复写入不得重复进待审池"（灰区去重）。
type verdictSet struct {
	negative map[string]store.Edge // 无序对 → 已裁决边
	pending  map[string]store.Edge // 无序对 → 已在待审池的灰区边
}

func (v verdictSet) pendingCount() int { return len(v.pending) }

// loadVerdicts 收集已存在的记账边。
//
// 只沿 aliases 里的节点做 1 跳邻域收集（P0 的 GraphStore 没有"全表枚举边"的端口，
// 这是可见的契约限制，在 Pipeline 的 Warnings 里说明口径）。
func (d *Disambiguator) loadVerdicts(ctx context.Context, scope store.Scope) (verdictSet, error) {
	out := verdictSet{
		negative: make(map[string]store.Edge, 8),
		pending:  make(map[string]store.Edge, 8),
	}
	or, ok := d.ports.Store.(store.OntologyReader)
	if !ok || or == nil {
		return out, nil
	}
	aliases, err := or.ListAliases(ctx)
	if err != nil {
		return out, fmt.Errorf("extract: list aliases for verdicts: %w", err)
	}
	seen := make(map[string]struct{}, len(aliases))
	for _, a := range aliases {
		if _, dup := seen[a.NodeID]; dup {
			continue
		}
		seen[a.NodeID] = struct{}{}
		edges, err := d.ports.Graph.Neighbors(ctx, store.NeighborQuery{
			NodeID: a.NodeID, Scope: scope,
			Relations: []store.Relation{RelationNegativeSameAs, RelationPossiblySame},
			Hops:      HardMaxHops,
		})
		if err != nil {
			continue // 邻域不可读时不影响主判定；严格性已在候选枚举里上报
		}
		for _, hop := range edges {
			key := pairKey(hop.Edge.FromID, hop.Edge.ToID)
			switch hop.Edge.Relation {
			case RelationNegativeSameAs:
				out.negative[key] = hop.Edge
			case RelationPossiblySame:
				out.pending[key] = hop.Edge
			}
		}
	}
	return out, nil
}

// scoreMention 计算一个提及对全部候选的融合分并做三区判定。
func (d *Disambiguator) scoreMention(ctx context.Context, scope store.Scope, m Mention, cands []candidate, verdicts verdictSet) DisambiguateResult {
	res := DisambiguateResult{Mention: m.Surface, Zone: ZoneNew, Breakdown: []ScoreBreakdown{}}
	if len(cands) == 0 {
		return res
	}

	// 提及侧向量：name + definition（§4 的 cosine 输入）。
	mv := m.Values
	if len(mv) == 0 && d.emb != nil {
		vecs, err := d.emb.Embed(ctx, []string{mentionText(m)})
		if err == nil && len(vecs) == 1 && len(vecs[0]) > 0 {
			mv = vecs[0]
		} else if err != nil {
			res.Warnings = append(res.Warnings, "消歧 cosine 通道跳过：embedder 报错 "+err.Error())
		}
	}
	if len(mv) == 0 {
		res.Warnings = append(res.Warnings, "消歧 cosine 通道缺席（无向量）：权重会重新按剩余通道归一")
	}

	// BM25 语料 = 每个候选的 name + aliases（§4 的 BM25(name, aliases)）。
	texts := make([][]string, len(cands))
	for i, c := range cands {
		toks := bigram.Tokens(c.node.CanonicalName)
		for _, a := range c.aliases {
			toks = append(toks, bigram.Tokens(a.Surface)...)
		}
		texts[i] = toks
	}
	bm := newBM25Index(texts, d.cfg.L1BM25K1, d.cfg.L1BM25B)
	query := bigram.Tokens(strings.Join(append([]string{m.Surface}, m.Aliases...), " "))

	w1, w2, w3 := d.cfg.W1Cosine, d.cfg.W2BM25, d.cfg.W3Structural
	// 通道缺席时把它的权重剔除后重新归一：否则"没有 embedder"会把所有分压到 θ_hi 之下，
	// 让自动合并永不发生——那不是保守，而是把配置错误伪装成判定结果。
	if len(mv) == 0 {
		w1 = 0
	}
	if w1+w2+w3 <= 0 {
		w1, w2, w3 = 0.5, 0.3, 0.2
	}
	norm := w1 + w2 + w3

	// 排除"该提及自身派生出的节点"：重复写入同一段 episode 时，它已经存在于
	// 候选池里，与自己的 cosine 恒为 1，会把三区判定变成"永远自动合并到自身"。
	// 这不是合并，而是自指——必须排除，否则 repeat-write 的判定无法复现。
	selfID := NodeID(scope, m.Surface)

	for i, c := range cands {
		if c.node.ID == selfID {
			continue
		}
		bd := ScoreBreakdown{NodeID: c.node.ID}
		if len(mv) > 0 {
			if cv := d.candidateVector(ctx, c); len(cv) > 0 {
				bd.Cosine = cosine(mv, cv)
			}
		}
		bd.BM25 = normBM25(bm, query, i)
		bd.Structural = structuralOverlap(relationSet(m.RelationTypes), c.relationTypes)
		bd.Score = (w1*bd.Cosine + w2*bd.BM25 + w3*bd.Structural) / norm
		if bd.Score < 0 {
			bd.Score = 0
		}
		if bd.Score > 1 {
			bd.Score = 1
		}
		// negative_SameAs 的键是"该提及若新建会得到的节点 id"与候选节点的无序对：
		// 裁决时写的正是这对 id，因此后续同一对必然命中，不再进灰区。
		if _, neg := verdicts.negative[pairKey(NodeID(scope, m.Surface), c.node.ID)]; neg {
			bd.Negative = true
		}
		res.Breakdown = append(res.Breakdown, bd)
	}
	sort.SliceStable(res.Breakdown, func(i, j int) bool {
		if res.Breakdown[i].Score != res.Breakdown[j].Score {
			return res.Breakdown[i].Score > res.Breakdown[j].Score
		}
		return res.Breakdown[i].NodeID < res.Breakdown[j].NodeID
	})
	if len(res.Breakdown) == 0 {
		// 候选池里只有"提及自身"：这次提及没有可比较的对象，按新实体处理。
		return res
	}

	top := res.Breakdown[0]
	res.Score = top.Score
	if top.Negative {
		// 已裁决为不同实体：不再进灰区（§4 第 3 条），按新实体处理。
		res.Zone = ZoneNegative
		return res
	}
	switch {
	case top.Score >= d.cfg.ThetaHi:
		res.Zone = ZoneAutoMerge
		res.NodeID = top.NodeID
	case top.Score >= d.cfg.ThetaLo:
		res.Zone = ZoneGray
		res.NodeID = top.NodeID
	default:
		res.Zone = ZoneNew
	}
	return res
}

// candidateVector 取候选节点 name+definition 的向量（可选通道）。
func (d *Disambiguator) candidateVector(ctx context.Context, c candidate) []float32 {
	if d.emb == nil {
		return nil
	}
	text := c.node.CanonicalName
	if c.node.PropsJSON != "" && c.node.PropsJSON != "{}" {
		if def := definitionFromProps(c.node.PropsJSON); def != "" {
			text += " " + def
		}
	}
	vecs, err := d.emb.Embed(ctx, []string{text})
	if err != nil || len(vecs) != 1 || len(vecs[0]) == 0 {
		return nil
	}
	return vecs[0]
}

// relationSet 把提及侧的关系类型切片转成集合（Jaccard 需要集合形态）。
func relationSet(in []store.Relation) map[store.Relation]struct{} {
	if len(in) == 0 {
		return nil
	}
	out := make(map[store.Relation]struct{}, len(in))
	for _, r := range in {
		out[r] = struct{}{}
	}
	return out
}

// structuralOverlap 是"结构一致性"：已有 relation 类型重叠度（Jaccard）。
//
// 两边都没有关系类型时返回 0 而不是 1：0 表示"无证据"，1 会凭空把分数抬高。
func structuralOverlap(a, b map[store.Relation]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	var inter int
	for r := range a {
		if _, ok := b[r]; ok {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// normBM25 把候选的 BM25 分归一化到 [0,1]（与 L1 同一套归一化口径）。
func normBM25(bm *bm25Index, query []string, doc int) float64 {
	den := bm.maxScore(query)
	if den <= 0 {
		return 0
	}
	s := bm.score(query, doc) / den
	if s < 0 {
		return 0
	}
	if s > 1 {
		return 1
	}
	return s
}

func mentionText(m Mention) string {
	t := m.Surface
	if m.Definition != "" {
		t += " " + m.Definition
	}
	return t
}

// definitionFromProps 从 node.PropsJSON 里取 "definition" 字段（可选）。
func definitionFromProps(props string) string {
	// 只做最小解析：props 是内部结构，不需要完整 JSON 语义。
	const key = `"definition"`
	i := strings.Index(props, key)
	if i < 0 {
		return ""
	}
	rest := props[i+len(key):]
	c := strings.Index(rest, ":")
	if c < 0 {
		return ""
	}
	rest = strings.TrimSpace(rest[c+1:])
	if !strings.HasPrefix(rest, `"`) {
		return ""
	}
	rest = rest[1:]
	if j := strings.Index(rest, `"`); j >= 0 {
		return rest[:j]
	}
	return ""
}
