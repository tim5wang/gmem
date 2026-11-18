package recall

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/tim5wang/gmem/fusion"
	"github.com/tim5wang/gmem/store"
)

// Recaller 是 §5 的 Recall API 主体：
//
//	align（Concept Aligner） → plan（Recall Planner） → 多 lane 并行召回
//	→ RRF 融合 → 概念一致性加权 → 时效降权 → 结果 + plan + degraded
//
// 它只依赖 store.Ports，因此 SQLite 后端与内存后端跑同一套逻辑。
// Embedder 是可选的：未注入时 vector lane 优雅缺席，并在 Degraded 里显式报告。
type Recaller struct {
	ports store.Ports
	cfg   Config
	align *Aligner
	plan  *Planner
	emb   Embedder
}

// New 构造 Recaller。
//
// opts 里的 nil 字段按默认处理：cfg 零值 → DefaultConfig()；emb → nil（vector 降级）；
// 若 ports.Store 同时实现了 store.OntologyReader 且 opts.Ontology 为 nil，自动采用它——
// 这是"后端能枚举本体就用"的便捷路径，不需要调用方额外接线。
func New(ports store.Ports, cfg Config, emb Embedder) *Recaller {
	cfg = cfg.withDefaults()
	var or store.OntologyReader
	if o, ok := ports.Store.(store.OntologyReader); ok {
		or = o
	}
	return &Recaller{
		ports: ports,
		cfg:   cfg,
		align: NewAligner(or, cfg),
		plan:  NewPlanner(cfg),
		emb:   emb,
	}
}

// WithOntology 显式指定本体枚举端口（覆盖 New 的自动探测）。
//
// 单独一个 method 而不是改 New 签名：New 的签名一旦发布就不好动，
// 而"对齐的候选来源"在多后端下确实可能不是 Store。
func (r *Recaller) WithOntology(or store.OntologyReader) *Recaller {
	r.align = NewAligner(or, r.cfg)
	return r
}

// Config 返回生效的配置（已补齐默认值），便于调用方校验阈值。
func (r *Recaller) Config() Config { return r.cfg }

// Recall 执行一次召回。
//
// 错误只用于"无法继续"的情形（空 query、ctx 取消）。覆盖率不足**不是**错误：
// 那是 R4 的正常结论，走 RecallResult.Status + Missing 表达。
func (r *Recaller) Recall(ctx context.Context, req RecallRequest) (RecallResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return RecallResult{}, err
	}

	// --- 1. align -----------------------------------------------------------
	al, err := r.align.Align(ctx, req.Query)
	if err != nil {
		return RecallResult{}, err
	}

	// --- 2. plan ------------------------------------------------------------
	in := planInput{Align: al, Request: req, HasEmbedder: r.emb != nil, GraphScope: req.Scope}
	plan := r.plan.makePlan(in)
	degraded := append([]string(nil), al.Degraded...)
	for _, lp := range plan.Lanes {
		if lp.Selected && !lp.Executed && lp.SkipReason != "" {
			degraded = append(degraded, fmt.Sprintf("%s lane skipped: %s", lp.Lane, lp.SkipReason))
		}
	}

	res := RecallResult{Coverage: al.Coverage, Plan: plan}

	// --- 3. R4：覆盖率不足先判，不硬答 ---------------------------------------
	// 触发条件（§5 R4）：coverage < θ_cov，或未对齐槽位里有必需槽。
	// 两条都判：覆盖率可能因分母小而虚高（只有一个槽且它对齐了 → 1.0），
	// 但只要存在"必需且未对齐"的槽，就已经是"我问的维度你没有"。
	//
	// coverage 只有在**有本体可对齐**时才作判据（al.CoverageAuthoritative）。
	// 库是空本体时算出的覆盖率没有语义：query 里的词全都"不在空集合里"，
	// 用它会退化成"任何查询都 insufficient"。这种情形走 Degraded 上报，
	// 让调用方知道召回是靠词法/图硬撑的。
	lowCoverage := al.CoverageAuthoritative && al.Coverage < r.cfg.ThetaCoverage
	if lowCoverage || len(al.RequiredMissing) > 0 {
		res.Status = StatusInsufficient
		res.Missing = missingFrom(al)
		res.Degraded = dedupeStrings(degraded)
		plan.Steps = append(plan.Steps, PlanStep{
			Step: "insufficient",
			Detail: fmt.Sprintf("coverage=%.3f (θ=%.2f, 可判定=%v)，必需未对齐槽=%d → 返回 insufficient 而非低分硬答",
				al.Coverage, r.cfg.ThetaCoverage, al.CoverageAuthoritative, len(al.RequiredMissing)),
		})
		for _, m := range res.Missing {
			plan.Steps = append(plan.Steps, PlanStep{Step: "missing", Detail: m.Reason})
		}
		return res, nil
	}

	// --- 3. 查询向量（可选） -------------------------------------------------
	var embedQuery []float32
	if r.emb != nil && laneSelected(plan, LaneVector) {
		vecs, err := r.emb.Embed(ctx, []string{req.Query})
		switch {
		case err != nil:
			// 降级而不是失败：宿主 embedder 挂了不该让词法通道一起不可用。
			degraded = append(degraded, fmt.Sprintf("vector lane skipped: embedder error: %v", err))
			markLaneSkipped(plan, LaneVector, "embedder error: "+err.Error())
		case len(vecs) == 0 || len(vecs[0]) == 0:
			degraded = append(degraded, "vector lane skipped: embedder 返回空向量")
			markLaneSkipped(plan, LaneVector, "embedder 返回空向量")
		default:
			embedQuery = vecs[0]
		}
	}

	perLane := req.Budget.PerLane
	if perLane <= 0 {
		perLane = r.cfg.DefaultPerLane
	}
	lin := laneInput{
		req: req, align: al, plan: plan, scope: req.Scope,
		perLane: perLane, embedQuery: embedQuery,
	}

	// --- 4. 多 lane 并行召回 -------------------------------------------------
	outcomes, err := r.runLanes(ctx, lin)
	if err != nil {
		return RecallResult{}, err
	}
	for _, o := range outcomes {
		if o.err != nil {
			degraded = append(degraded, fmt.Sprintf("%s lane degraded: %v", o.lane, o.err))
			markLaneFailed(plan, o.lane, o.err.Error())
		}
		plan.Dropped = append(plan.Dropped, o.dropped...)
	}

	// --- 5. RRF 融合 --------------------------------------------------------
	channels := make([]fusion.Channel, 0, len(outcomes))
	for _, o := range outcomes {
		if len(o.hits) == 0 {
			continue
		}
		items := make([]fusion.Item, 0, len(o.hits))
		for _, h := range o.hits {
			items = append(items, fusion.Item{ID: h.ID, Score: h.Score})
		}
		channels = append(channels, fusion.Channel{
			Name: string(o.lane), Weight: laneWeight(plan, o.lane), Items: items,
		})
	}
	plan.Steps = append(plan.Steps, PlanStep{
		Step:   "fuse",
		Detail: fmt.Sprintf("RRF(k=%d) 融合 %d 路通道（%s）", fusion.DefaultK, len(channels), laneNames(channels)),
	})
	fused := fusion.RRF(fusion.DefaultK, channels...)

	// --- 6. 取记忆主体 + 一致性加权 + 时效降权 --------------------------------
	evidence := make(map[string][]GraphEvidence)
	for _, o := range outcomes {
		for id, ev := range o.evidenceByID {
			evidence[id] = append(evidence[id], ev...)
		}
	}
	items, dropped, err := r.buildItems(ctx, req, al, plan, fused, evidence)
	if err != nil {
		return RecallResult{}, err
	}
	plan.Dropped = append(plan.Dropped, dropped...)

	res.Items = items
	res.Degraded = dedupeStrings(degraded)
	if len(res.Degraded) > 0 {
		res.Status = StatusDegraded
	} else {
		res.Status = StatusOK
	}
	// 覆盖率达标但一条都没召回：这是"通道都空"的现实，仍是 insufficient
	// （Missing 用空覆盖描述），不能返回 ok + 空 Items 让人以为查到了。
	if len(res.Items) == 0 {
		res.Status = StatusInsufficient
		res.Missing = []MissingSlot{{
			Kind:       SlotConcept,
			Text:       req.Query,
			Required:   true,
			Reason:     fmt.Sprintf("槽位已对齐，但在 scope=%q 内没有任何通道召回结果", req.Scope),
			Suggestion: "确认该 scope 已写入相关记忆（写入请走 Writer.PutMemory 单一入口）",
		}}
		plan.Steps = append(plan.Steps, PlanStep{Step: "empty", Detail: "所有通道 0 命中 → insufficient"})
	}
	return res, nil
}

// buildItems 把融合结果落成 RecallItem（含分数分解），并做两次乘性调整：
// 概念一致性加权（提升）与时效降权（衰减）。两条都记录在 Adjustments 里。
func (r *Recaller) buildItems(
	ctx context.Context,
	req RecallRequest,
	al Alignment,
	plan *RecallPlan,
	fused []fusion.Fused,
	evidence map[string][]GraphEvidence,
) ([]RecallItem, []DroppedItem, error) {
	if len(fused) == 0 {
		return nil, nil, nil
	}

	// 一次性把候选的内存取出来：逐条查会产生 N 次往返，且 N 可能到数十。
	final := req.Budget.Final
	if final <= 0 {
		final = r.cfg.DefaultFinal
	}
	// 解析窗口留出余量：降权后排序会变，先取足够多再排序截断。
	window := len(fused)
	if window < final*3 {
		window = final * 3
	}
	if window > len(fused) {
		window = len(fused)
	}

	conceptSet := map[string]bool{}
	for _, c := range al.ConceptIDs {
		conceptSet[c] = true
	}

	now := clockNow(r.cfg)
	// as_of 回溯时命中项必须取**当时有效**的版本，不能取最新版：
	// 用最新版回答"上个月我们用什么方案"会给出未来的答案，且 plan 里看不出来。
	resolved, resolveDropped, err := r.resolveMemories(ctx, req, fusion.IDs(fused[:window]))
	if err != nil {
		return nil, nil, err
	}
	dropped := append([]DroppedItem(nil), resolveDropped...)

	var items []RecallItem
	for _, f := range fused[:window] {
		m, ok := resolved[f.ID]
		if !ok {
			// 索引里有、主表里没有（或该时刻无有效版本）：这是索引与主表不一致的信号，
			// 必须记进 Dropped 而不是静默跳过。
			continue
		}
		if !matchesFilter(m, req) {
			dropped = append(dropped, DroppedItem{ID: f.ID, Score: f.Score, Reason: "被 scope/kind 过滤拦截"})
			continue
		}

		it := RecallItem{
			Memory: m, FusionScore: f.Score, ConceptBoost: 1, RecencyFactor: 1,
			GraphEvidence: evidence[m.ID],
		}
		for _, lane := range sortedLaneNames(f.Contributions) {
			c := f.Contributions[lane]
			it.Contributions = append(it.Contributions, LaneHit{
				Lane: LaneName(lane), Rank: c.Rank, Weight: c.Weight, Value: c.Value,
			})
		}

		// 概念一致性加权
		if m.ConceptID != "" && conceptSet[m.ConceptID] {
			it.ConceptConsistent = true
			it.ConceptBoost = 1 + r.cfg.ConceptBoost
			it.Adjustments = append(it.Adjustments, Adjustment{
				Kind: "concept_consistency", Factor: it.ConceptBoost,
				Reason: fmt.Sprintf("memory.concept_id=%s 命中对齐概念集合 → ×%.2f", m.ConceptID, it.ConceptBoost),
			})
		} else {
			it.Adjustments = append(it.Adjustments, Adjustment{
				Kind: "concept_consistency", Factor: 1,
				Reason: "concept_id 未命中对齐概念集合（不惩罚，RRF 已按通道内排名惩罚）",
			})
		}

		// 时效降权（双时态：用世界时间 t_valid 作为"事实年龄"的锚）
		factor, reason := r.recency(m, now)
		it.RecencyFactor = factor
		it.Adjustments = append(it.Adjustments, Adjustment{Kind: "recency_decay", Factor: factor, Reason: reason})

		it.Score = it.FusionScore * it.ConceptBoost * it.RecencyFactor
		items = append(items, it)
	}

	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Score != items[j].Score {
			return items[i].Score > items[j].Score
		}
		return items[i].Memory.ID < items[j].Memory.ID
	})
	if len(items) > final {
		for _, it := range items[final:] {
			dropped = append(dropped, DroppedItem{
				ID: it.Memory.ID, Score: it.Score,
				Reason: fmt.Sprintf("最终预算切断（final budget=%d，调整后排名 %d 之后）", final, final),
			})
		}
		items = items[:final]
	}

	for _, it := range items {
		plan.Steps = append(plan.Steps, PlanStep{
			Step: "score:" + it.Memory.ID,
			Detail: fmt.Sprintf("fusion=%.5f × concept=%.2f × recency=%.3f = %.5f（channels=%s）",
				it.FusionScore, it.ConceptBoost, it.RecencyFactor, it.Score, contributionsOf(it)),
		})
	}
	return items, dropped, nil
}

// recency 计算时效降权因子。
//
// 语义：越旧的**事实**（t_valid 越小）分越低，但下界 RecencyFloor 保证
// "旧而正确"不会被压成 0。半衰期来自配置（待校准），而不是写死常量。
func (r *Recaller) recency(m store.Memory, now time.Time) (float64, string) {
	half := r.cfg.RecencyHalfLife
	if half <= 0 {
		return 1, "时效降权已关闭（RecencyHalfLife<=0）"
	}
	anchor := m.TValid
	if anchor <= 0 {
		anchor = m.TCreated
	}
	if anchor <= 0 {
		return 1, "无 t_valid/t_created，无法计算时效，不降权"
	}
	age := now.Sub(time.Unix(anchor, 0))
	if age <= 0 {
		return 1, fmt.Sprintf("t_valid=%d 在未来或为当前，不降权", anchor)
	}
	f := math.Pow(0.5, age.Hours()/half.Hours())
	if f < r.cfg.RecencyFloor {
		f = r.cfg.RecencyFloor
	}
	if f > 1 {
		f = 1
	}
	return f, fmt.Sprintf("age=%.1fh，半衰期=%.0fh → ×%.3f（下界 %.2f）",
		age.Hours(), half.Hours(), f, r.cfg.RecencyFloor)
}

// missingFrom 把对齐缺口翻成 R4 的结构化 Missing[]。
func missingFrom(al Alignment) []MissingSlot {
	var out []MissingSlot
	seen := map[string]bool{}
	add := func(s Slot, required bool) {
		key := string(s.Kind) + "|" + s.Text
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, MissingSlot{
			Kind:     s.Kind,
			Text:     s.Text,
			Required: required,
			Reason: fmt.Sprintf("查询中的「%s」(%s 槽) 在库中没有任何对齐记录：没有出现过的别名、概念名，也没有相似的定义句",
				s.Text, s.Kind),
			Suggestion: suggestionFor(s.Kind),
		})
	}
	for _, s := range al.RequiredMissing {
		add(s, true)
	}
	// 非必需槽也报（如 deprecated 概念质量为 0）：它们解释了"为什么覆盖率偏低"。
	for _, s := range al.Unmatched {
		if s.Required {
			continue
		}
		add(s, false)
	}
	// 覆盖率不达标但没有具体缺口（例如槽位全对齐、只是因为权重太低）：
	// 必须给一条**有意义**的说明，不能返回空的 Missing[]。
	if len(out) == 0 {
		out = append(out, MissingSlot{
			Kind:       SlotConcept,
			Text:       al.Query,
			Required:   false,
			Reason:     fmt.Sprintf("整体覆盖率 %.3f 低于 θ_cov，但没有单一槽位完全缺失（多为已弃用概念或低质量对齐）", al.Coverage),
			Suggestion: "确认 concepts.status 是否已 certified；draft 概念的权重被打折",
		})
	}
	return out
}

func suggestionFor(k SlotKind) string {
	switch k {
	case SlotEntity:
		return "先写入该实体的记忆（或为它补 aliases 词典项），再重试召回"
	case SlotConcept:
		return "补一条该概念的定义句或把 draft 概念 certify，让对齐能命中"
	case SlotRelation:
		return "写入显式关系（edges），或把问句换成库里已有的关系线索词"
	default:
		return "检查查询串是否包含错别字或未收录的标识符"
	}
}

func matchesFilter(m store.Memory, req RecallRequest) bool {
	if req.Scope != "" && m.Scope != req.Scope {
		return false
	}
	if len(req.Kinds) == 0 {
		return true
	}
	for _, k := range req.Kinds {
		if m.Kind == k {
			return true
		}
	}
	return false
}

func laneSelected(plan *RecallPlan, lane LaneName) bool {
	for _, lp := range plan.Lanes {
		if lp.Lane == lane {
			return lp.Selected
		}
	}
	return false
}

func laneWeight(plan *RecallPlan, lane LaneName) float64 {
	for _, lp := range plan.Lanes {
		if lp.Lane == lane {
			return lp.Weight
		}
	}
	return 1
}

func markLaneSkipped(plan *RecallPlan, lane LaneName, reason string) {
	for i := range plan.Lanes {
		if plan.Lanes[i].Lane != lane {
			continue
		}
		plan.Lanes[i].Executed = false
		plan.Lanes[i].SkipReason = reason
	}
}

func markLaneFailed(plan *RecallPlan, lane LaneName, reason string) {
	for i := range plan.Lanes {
		if plan.Lanes[i].Lane != lane {
			continue
		}
		plan.Lanes[i].Executed = false
		plan.Lanes[i].SkipReason = "执行失败: " + reason
	}
}

func laneNames(chs []fusion.Channel) string {
	out := ""
	for i, c := range chs {
		if i > 0 {
			out += ","
		}
		out += c.Name
	}
	return out
}

func sortedLaneNames(cs map[string]fusion.Contribution) []string {
	out := make([]string, 0, len(cs))
	for k := range cs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func contributionsOf(it RecallItem) string {
	out := ""
	for i, c := range it.Contributions {
		if i > 0 {
			out += ","
		}
		out += fmt.Sprintf("%s#%d", c.Lane, c.Rank)
	}
	return out
}

func dedupeStrings(ss []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
