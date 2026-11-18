package recall

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/tim5wang/gmem/bigram"
	"github.com/tim5wang/gmem/store"
)

// Aligner 是 §5 R3 的第一步：query → 概念槽位对齐。
//
// 对齐路径**全部不依赖 embedder**（P1 不得强依赖向量），共四条：
//
//  1. aliases 词典（§3 L0 词典规则，最高置信来源，MatchAlias）；
//  2. concepts.name 直接命中（MatchConceptName）；
//  3. 定义句词法相似度（MatchDefinition）：用 bigram.Tokens 做**包含度**，
//     不是向量相似度——P1 不引入 embedder，也不该为了对齐硬拉一个进来；
//  4. 关系线索词（MatchRelationCue）："用什么" → uses，供 Planner 定跳数。
//
// 剩余未被上述任何一条覆盖的文本片段成为"残槽"：
//   - 拉丁/数字片段 → literal 槽（词法通道必然可检索，视为已对齐）；
//   - 中文片段 → 先试定义句相似度，命中即 concept 槽；否则成为**未对齐的
//     entity 槽且 Required=true**——这正是 R4「缺什么」的来源。
type Aligner struct {
	cfg Config
	or  store.OntologyReader // 可为 nil：后端没实现 OntologyReader 时降级
}

// NewAligner 构造对齐器。or 为 nil 表示后端不提供本体枚举，对齐只能靠词法与线索，
// 这种情形必须在 Alignment.Degraded 里显式上报（不得静默）。
func NewAligner(or store.OntologyReader, cfg Config) *Aligner {
	return &Aligner{cfg: cfg.withDefaults(), or: or}
}

// alignEntry 是内部用的"带位置的槽位"。
type alignEntry struct {
	start int // rune 位置，仅用于稳定排序
	end   int
	slot  Slot
	match *Match
}

// Align 把 query 对齐到概念槽位，输出 matched[] / unmatched_slots[] / coverage。
func (a *Aligner) Align(ctx context.Context, query string) (Alignment, error) {
	al := Alignment{Query: query, Normalized: normalizeQuery(query)}
	if strings.TrimSpace(query) == "" {
		return al, fmt.Errorf("recall: empty query")
	}

	var concepts []store.Concept
	var aliases []store.Alias
	if a.or != nil {
		var err error
		if concepts, err = a.or.ListConcepts(ctx); err != nil {
			return al, fmt.Errorf("recall: list concepts: %w", err)
		}
		if aliases, err = a.or.ListAliases(ctx); err != nil {
			return al, fmt.Errorf("recall: list aliases: %w", err)
		}
		if len(concepts) == 0 && len(aliases) == 0 {
			al.Degraded = append(al.Degraded,
				"ontology empty: 没有 aliases/concepts，对齐退化为 关系线索 + 字面量；coverage 不作 R4 判据")
		} else {
			al.CoverageAuthoritative = true
		}
	} else {
		al.Degraded = append(al.Degraded,
			"ontology reader unavailable: 后端未实现 store.OntologyReader，对齐仅剩 关系线索 + 字面量；coverage 不作 R4 判据（降级）")
	}

	orig := []rune(query)
	low := []rune(al.Normalized)
	blocked := make([]bool, len(orig)) // 已被某个槽位/分隔词占用

	conceptByName := make(map[string]store.Concept, len(concepts))
	for _, c := range concepts {
		nm := normalizeQuery(c.Name)
		if nm == "" {
			continue
		}
		if old, ok := conceptByName[nm]; !ok || c.Status == store.ConceptCertified && old.Status != store.ConceptCertified {
			conceptByName[nm] = c
		}
	}

	var entries []alignEntry

	// --- 1/2. aliases 与 concepts.name：按"最长优先、不重叠"贪心接受 ------------
	type surfaceCand struct {
		start, end int
		alias      *store.Alias
		concept    *store.Concept
	}
	var cands []surfaceCand
	for i := range aliases {
		al2 := aliases[i]
		pat := []rune(normalizeQuery(al2.Surface))
		for _, o := range findOccurrences(low, pat) {
			cands = append(cands, surfaceCand{start: o[0], end: o[1], alias: &aliases[i]})
		}
	}
	for _, c := range conceptByName {
		cc := c
		pat := []rune(normalizeQuery(cc.Name))
		for _, o := range findOccurrences(low, pat) {
			cands = append(cands, surfaceCand{start: o[0], end: o[1], concept: &cc})
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		li := cands[i].end - cands[i].start
		lj := cands[j].end - cands[j].start
		if li != lj {
			return li > lj // 最长优先：避免 "Al" 抢走 "Alice"
		}
		if cands[i].start != cands[j].start {
			return cands[i].start < cands[j].start
		}
		// alias 优先于 concept name（词典置信更高）
		return cands[i].alias != nil && cands[j].alias == nil
	})
	for _, c := range cands {
		if overlaps(blocked, c.start, c.end) {
			continue
		}
		mark(blocked, c.start, c.end)
		text := string(orig[c.start:c.end])
		if c.alias != nil {
			conf := c.alias.Confidence
			if conf <= 0 {
				conf = 1
			}
			q := clamp01(conf * a.cfg.AliasQuality)
			entries = append(entries, alignEntry{
				start: c.start, end: c.end,
				slot: Slot{Kind: SlotEntity, Text: text, Required: true, Weight: a.slotWeight(SlotEntity)},
				match: &Match{
					Kind: SlotEntity, Text: text, NodeID: c.alias.NodeID,
					Method: MatchAlias, Quality: q, StatusFactor: 1,
				},
			})
			continue
		}
		q := clamp01(a.cfg.ConceptNameQuality)
		sf := a.statusFactor(c.concept.Status)
		entries = append(entries, alignEntry{
			start: c.start, end: c.end,
			slot: Slot{Kind: SlotConcept, Text: text, Required: true, Weight: a.slotWeight(SlotConcept)},
			match: &Match{
				Kind: SlotConcept, Text: text, ConceptID: c.concept.ID, ConceptName: c.concept.Name,
				Method: MatchConceptName, Quality: q, StatusFactor: sf,
			},
		})
	}

	// --- 4. 关系线索词（最长优先；与已接受槽位重叠时**只取 relation hint**） -------------
	//
	// F4：原先线索与已接受槽位（例如概念名）有任何重叠就整条丢弃，于是
	// "Alice 在哪个公司工作"里的「哪个公司」被概念名「公司」吃掉，
	// RelationHints 为空 → 形态退化为 full_with_anchor → 走 shape-default 的 3 跳盲扩展，
	// 而同义的"Alice 在哪工作"能靠 works_at 1 跳答出。两个问法必须行为一致。
	//
	// 修法：允许重叠匹配。重叠时**不占用** blocked（不去掩盖已接受的概念槽位），
	// 只把 relation 作为线索附加在同一个槽位上，供 Planner 定跳数。
	type cueCand struct {
		start, end int
		relation   store.Relation
		phrase     string
	}
	var cueCands []cueCand
	for _, cue := range a.cfg.RelationCues {
		for _, p := range cue.Phrases {
			pat := []rune(normalizeQuery(p))
			if len(pat) == 0 {
				continue
			}
			for _, o := range findOccurrences(low, pat) {
				cueCands = append(cueCands, cueCand{start: o[0], end: o[1], relation: cue.Relation, phrase: p})
			}
		}
	}
	sort.SliceStable(cueCands, func(i, j int) bool {
		li := cueCands[i].end - cueCands[i].start
		lj := cueCands[j].end - cueCands[j].start
		if li != lj {
			return li > lj
		}
		return cueCands[i].start < cueCands[j].start
	})
	cueSeen := map[store.Relation]bool{}
	var overlapHints []store.Relation
	for _, c := range cueCands {
		if cueSeen[c.relation] {
			continue
		}
		cueSeen[c.relation] = true
		if overlaps(blocked, c.start, c.end) {
			// 已被概念/别名占用：只取 relation hint，不新增槽位，也不改 blocked。
			// 这样 coverage 不被重复计入，而 Planner 能拿到真实关系（F4）。
			overlapHints = append(overlapHints, c.relation)
			continue
		}
		mark(blocked, c.start, c.end)
		text := string(orig[c.start:c.end])
		entries = append(entries, alignEntry{
			start: c.start, end: c.end,
			slot: Slot{Kind: SlotRelation, Text: text, Required: false, Weight: a.slotWeight(SlotRelation)},
			match: &Match{
				Kind: SlotRelation, Text: text, Relation: c.relation,
				Method: MatchRelationCue, Quality: 1, StatusFactor: 1,
			},
		})
	}

	// --- 分隔词：关系型短语与停用词不进入残槽，否则 "什么" 会被当成实体 --------
	sepBlocked := make([]bool, len(orig))
	copy(sepBlocked, blocked)
	for _, list := range [][]string{a.cfg.RelationalPhrases, hanStopwords} {
		for _, p := range list {
			pat := []rune(normalizeQuery(p))
			for _, o := range findOccurrences(low, pat) {
				mark(sepBlocked, o[0], o[1])
			}
		}
	}

	// --- 3/5. 残槽：拉丁→literal；中文→定义句相似度或未对齐 entity ---------------
	for _, seg := range residualSegments(orig, sepBlocked) {
		text := seg.String()
		if isLatinLike(text) {
			// 字面量/标识符：词法通道必然能检索到，视为已对齐（R5 里可解释）。
			entries = append(entries, alignEntry{
				start: seg.start, end: seg.end,
				slot: Slot{Kind: SlotLiteral, Text: text, Required: false, Weight: a.slotWeight(SlotLiteral)},
				match: &Match{
					Kind: SlotLiteral, Text: text, Method: MatchLiteral, Quality: 1, StatusFactor: 1,
				},
			})
			continue
		}
		if m, ok := a.matchByDefinition(text, concepts); ok {
			entries = append(entries, alignEntry{
				start: seg.start, end: seg.end,
				slot:  Slot{Kind: SlotConcept, Text: text, Required: true, Weight: a.slotWeight(SlotConcept)},
				match: &m,
			})
			continue
		}
		// 未对齐：库里没有这个维度的概念。
		//
		// Required 留 false：D14 要求"本体里没这个概念"不得先验地等同于
		// "我没有这个维度"——是否构成硬缺口由 Recall 在跑完所有通道、
		// 确认零证据之后裁定（见 recall.go 的 missingForZeroEvidence）。
		entries = append(entries, alignEntry{
			start: seg.start, end: seg.end,
			slot: Slot{Kind: SlotEntity, Text: text, Required: false, Weight: a.slotWeight(SlotEntity)},
		})
	}

	// D14：残槽**默认不构成硬缺口**。"本体里没这个概念"不能先验地当成
	// "我没有这个维度"——词法通道很可能能答（记忆正文里就有这个词）。
	// 是否算硬缺口由 Recall 在**跑完所有通道且真零证据**之后裁定（见 recall.go）。
	// 这里只把残槽记为 Unmatched（参与 coverage 分母），不置 Required。

	// --- 汇总 -----------------------------------------------------------------
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].start != entries[j].start {
			return entries[i].start < entries[j].start
		}
		return entries[i].end < entries[j].end
	})

	var total, got float64
	anchorSeen := map[string]bool{}
	conceptSeen := map[string]bool{}
	for i := range entries {
		e := &entries[i]
		al.Slots = append(al.Slots, e.slot)
		total += e.slot.Weight
		if e.match == nil {
			al.Unmatched = append(al.Unmatched, e.slot)
			if e.slot.Required {
				al.RequiredMissing = append(al.RequiredMissing, e.slot)
			}
			continue
		}
		m := *e.match
		m.SlotIndex = len(al.Slots) - 1
		m.Weight = e.slot.Weight * m.Quality * m.StatusFactor
		// F2b：coverage 只衡量"有没有听懂 query 的各个维度"，因此**不含** StatusFactor。
		//
		// 旧实现让 StatusFactor 进入 coverage，于是 draft 概念的精确查询得到
		// 0.9（ConceptNameQuality）× 0.6（draft 打折）= 0.54 < θ 0.6 → 必失败。
		// 本体越丰富、draft 越多，越容易把"完全听懂的问题"判成 insufficient，
		// 这是反向的：认知度不该被认证状态惩罚。
		//
		// StatusFactor 只影响排序权重（R2：只有 certified 参与硬契约 → draft 打折、
		// deprecated 归零都在 Match.Weight 与最终排序里体现）。
		got += e.slot.Weight * m.Quality
		al.Matched = append(al.Matched, m)
		if m.NodeID != "" && !anchorSeen[m.NodeID] {
			anchorSeen[m.NodeID] = true
			al.Anchors = append(al.Anchors, m.NodeID)
		}
		if m.ConceptID != "" && !conceptSeen[m.ConceptID] {
			conceptSeen[m.ConceptID] = true
			al.ConceptIDs = append(al.ConceptIDs, m.ConceptID)
		}
		if m.Relation != "" {
			al.RelationHints = append(al.RelationHints, m.Relation)
		}
	}
	// F4：与已接受槽位重叠的关系线索只能以 hint 形式存在（没有自己的槽位），
	// 必须单独补进 RelationHints，否则 Planner 看不到它。
	for _, rel := range overlapHints {
		dup := false
		for _, got := range al.RelationHints {
			if got == rel {
				dup = true
				break
			}
		}
		if !dup {
			al.RelationHints = append(al.RelationHints, rel)
		}
	}
	if total > 0 {
		al.Coverage = clamp01(got / total)
	}
	// 覆盖率 100% 但"缺"的那个槽位质量为 0（如 deprecated 概念）时，
	// RequiredMissing 仍然是权威结论，因此两者同时上报。
	return al, nil
}

// matchByDefinition 用定义句词法包含度做对齐（不依赖 embedder）。
func (a *Aligner) matchByDefinition(term string, concepts []store.Concept) (Match, bool) {
	q := bigram.Tokens(term)
	if len(q) == 0 {
		return Match{}, false
	}
	var best *store.Concept
	bestSim := 0.0
	for i := range concepts {
		c := &concepts[i]
		if strings.TrimSpace(c.Definition) == "" {
			continue
		}
		def := bigram.Tokens(c.Definition)
		if len(def) == 0 {
			continue
		}
		set := make(map[string]bool, len(def))
		for _, t := range def {
			set[t] = true
		}
		hit := 0
		for _, t := range q {
			if set[t] {
				hit++
			}
		}
		// 包含度：查询的 token 有多少出现在定义句里。
		sim := float64(hit) / float64(len(q))
		// 至少命中 2 个 token 才算对齐（单 bigram 重合纯属巧合）。
		if hit < 2 || len(q) < 2 {
			continue
		}
		if sim > bestSim {
			bestSim, best = sim, c
		}
	}
	if best == nil || bestSim < a.cfg.DefinitionMinSim {
		return Match{}, false
	}
	return Match{
		Kind: SlotConcept, Text: term, ConceptID: best.ID, ConceptName: best.Name,
		Method:       MatchDefinition,
		Quality:      clamp01(bestSim * a.cfg.DefinitionQuality),
		StatusFactor: a.statusFactor(best.Status),
	}, true
}

func (a *Aligner) statusFactor(s store.ConceptStatus) float64 {
	if f, ok := a.cfg.StatusFactors[s]; ok {
		return f
	}
	// 未知状态按 certified 对待会高估 draft；按 1 同样高估。
	// 选择 1.0 并在 plan 里保留 StatusFactor 字段，让 explain 能看出来。
	return 1
}

func (a *Aligner) slotWeight(k SlotKind) float64 {
	switch k {
	case SlotEntity:
		return a.cfg.SlotWeights.Entity
	case SlotConcept:
		return a.cfg.SlotWeights.Concept
	case SlotRelation:
		return a.cfg.SlotWeights.Relation
	case SlotLiteral:
		return a.cfg.SlotWeights.Literal
	}
	return 1
}

// ---------------------------------------------------------------------------
// 文本工具
// ---------------------------------------------------------------------------

// hanStopwords 是残槽切分用的中文停用词（避免"的/是/什么"被判成实体维度）。
var hanStopwords = []string{
	"什么", "是", "的", "了", "吗", "呢", "在", "和", "与", "我", "你", "他", "她", "它",
	"谁", "哪", "怎么", "如何", "为什么", "请问", "一下", "有", "个", "这", "那", "会", "能",
}

// normalizeQuery 做对齐用的归一：小写 + 去首尾空白。
//
// 不做 bigram 切分：对齐靠**子串**匹配已知 surface（aliases/concepts.name），
// 切分反而会把"Alice"打散。bigram 只用在定义句相似度里。
func normalizeQuery(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// findOccurrences 返回 pat 在 hay 中所有（可重叠）出现位置，[start,end) 以 rune 计。
func findOccurrences(hay, pat []rune) [][2]int {
	if len(pat) == 0 || len(hay) < len(pat) {
		return nil
	}
	var out [][2]int
	for i := 0; i+len(pat) <= len(hay); i++ {
		ok := true
		for j := range pat {
			if hay[i+j] != pat[j] {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, [2]int{i, i + len(pat)})
		}
	}
	return out
}

func overlaps(mask []bool, start, end int) bool {
	for i := start; i < end && i < len(mask); i++ {
		if mask[i] {
			return true
		}
	}
	return false
}

func mark(mask []bool, start, end int) {
	for i := start; i < end && i < len(mask); i++ {
		mask[i] = true
	}
}

// segment 是一段连续有意义文本（rune 区间）。
type segment struct {
	start, end int
	runes      []rune
}

// residualSegments 收集未被任何槽位/分隔词占用的连续有意义片段。
func residualSegments(orig []rune, blocked []bool) []segment {
	var out []segment
	start := -1
	flush := func(end int) {
		if start < 0 {
			return
		}
		r := orig[start:end]
		if len(r) >= 2 && !allStopword(r) {
			out = append(out, segment{start: start, end: end, runes: r})
		}
		start = -1
	}
	for i := 0; i < len(orig); i++ {
		if blocked[i] || !meaningful(orig[i]) {
			flush(i)
			continue
		}
		if start < 0 {
			start = i
		}
	}
	flush(len(orig))
	if len(out) > 8 { // 上限：避免长句炸出几十个残槽把 coverage 稀释成 0
		out = out[:8]
	}
	return out
}

func (s segment) String() string { return string(s.runes) }

func meaningful(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

func allStopword(r []rune) bool {
	s := normalizeQuery(string(r))
	for _, w := range hanStopwords {
		if s == w {
			return true
		}
	}
	for _, w := range latinStopwords {
		if s == w {
			return true
		}
	}
	return false
}

var latinStopwords = []string{
	"the", "a", "an", "is", "are", "was", "were", "of", "to", "and", "or", "in", "on",
	"for", "with", "how", "what", "which", "who", "whose", "where", "when", "why",
	"do", "does", "did", "i", "you", "he", "she", "it", "they", "we", "my", "your",
	"me", "us", "them", "this", "that", "these", "those", "be", "been", "being",
}

// isLatinLike 判断片段是否应作为字面量槽：拉丁字母/数字/常见标识符字符。
func isLatinLike(s string) bool {
	hasAlnum := false
	for _, r := range s {
		switch {
		case r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '.' || r == '/' || r == ':' || r == '#'):
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				hasAlnum = true
			}
		default:
			return false
		}
	}
	return hasAlnum
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
