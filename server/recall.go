package server

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/tim5wang/gmem/recall"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// GET /api/recall：Recall Inspector 的数据源（TD §9 六页 ②）
//
// # 这一层为什么只是"逐字段透传"
//
// recall 包已经产出完整解释：RecallPlan（shape/coverage/lane/跳数/步骤/拦截）、
// RecallItem（Score = FusionScore × ConceptBoost × RecencyFactor + Adjustments）、
// LaneHit（分通道贡献）、GraphEvidence（"凭什么是它"的边证据）、DroppedItem、
// MissingSlot、Degraded。**R5 的可信度完全建立在"页面上的数就是召回算出来的数"**。
//
// 因此本端点只做三件事：
//
//	1. 解析/校验 query string（非法参数 400 + 字段名，绝不静默夹取）；
//	2. 调一次 s.recaller.Recall（唯一一次）并**原样复制**每个字段；
//	3. 序列化。
//
// 它**不得**重算分数、不得裁剪条目、不得把 plan 简化成摘要（/api/graph 里的
// RecallMeta 就是摘要，那是给图用的；Inspector 要的是全量）。
// 一旦这里出现 `item.Score = item.FusionScore * ...`，页面上的"可解释"就成了
// 伪造的证据 —— 这是本卡最容易做错的地方，也由 recall_test.go 的
// TestRecallEndpointPassesThroughFactors 钉死。
//
// # 为什么用 server 自己的 DTO 而不是直接序列化 recall 结构
//
// recall/types.go 的类型**没有 json tag**（它们是领域类型，不是契约）。直接
// 序列化会得到 "Query"/"ShapeReason" 这种大写键，与 §9 的 snake_case 契约不一致，
// 且任何字段重命名都会悄悄改掉 HTTP 契约。因此这里显式声明 DTO 并逐字段复制：
// 编译期就能发现"recall 加了字段而契约没跟上"（复制语句会少一行，CR 可见），
// 而反射式转换恰恰会把新字段默默漏掉。
// ===========================================================================

const (
	// maxRecallK 是最终返回条数的硬上限：与 /api/graph 的 k 同为 50，
	// 超过即 400（不静默夹取 —— 静默夹取会让"我要 200 条"看起来像"它给了"）。
	maxRecallK = 50
	// defaultRecallK 在请求未给 k 时使用（与 recall 的 DefaultFinal 同量级）。
	defaultRecallK = 10
)

// RecallResponse 是 GET /api/recall 的返回体。
//
// Plan + Items + Dropped + Coverage + Missing + Degraded 一个不少：
// 页面的六个区块（plan / lane / 分数分解 / dropped / as_of / 降级）各取所需。
type RecallResponse struct {
	Meta RecallViewMeta `json:"meta"`
	// Status 透传 recall.RecallStatus（ok / insufficient / degraded）。
	Status   string  `json:"status"`
	Query    string  `json:"query"`
	Scope    string  `json:"scope"`
	Coverage float64 `json:"coverage"`
	// AsOf 回显请求的双时态视角（nil = 当前）。
	AsOf *int64 `json:"as_of,omitempty"`
	// ThetaCoverage 是 R4 的覆盖率门槛（plan 里也有，顶层给页面直接画进度条）。
	ThetaCoverage float64 `json:"theta_coverage"`
	// Items 每条含 score / fusion_score / concept_boost / recency_factor /
	// contributions / adjustments / graph_evidence —— 全部来自 recall，未重算。
	Items []RecallItemJSON `json:"items"`
	// Missing 是 R4 的结构化"缺什么"（insufficient 时非空且必须能读）。
	Missing []RecallMissingJSON `json:"missing"`
	// Dropped 与 plan.dropped 同源（同一份切片的副本，便于页面直接渲染拦截表）。
	Dropped []RecallDroppedJSON `json:"dropped"`
	// Degraded 是显式降级清单，不得静默（§13 / 验收「降级可见」）。
	Degraded []string       `json:"degraded"`
	Plan     RecallPlanJSON `json:"plan"`
}

// RecallViewMeta 是本页的元信息（回显请求 + 能力位）。
type RecallViewMeta struct {
	// Mode 恒为 "recall"（前端按 mode 判断页面，而不是靠 URL 猜）。
	Mode  string   `json:"mode"`
	Query string   `json:"query"`
	Scope string   `json:"scope"`
	Kinds []string `json:"kinds"`
	// Lanes 是请求**强制指定**的通道（空 = 由 Planner 决定）。
	Lanes []string `json:"lanes"`
	K     int      `json:"k"`
	AsOf  *int64   `json:"as_of,omitempty"`
	// Explain 回显 explain=1。注意：Plan **恒**返回，explain 只是 URL 兼容位
	// （R5 明确不提供"关掉解释"的开关）。
	Explain       bool         `json:"explain"`
	KMax          int          `json:"k_max"`
	Capabilities  Capabilities `json:"capabilities"`
	ThetaCoverage float64      `json:"theta_coverage"`
}

// RecallPlanJSON 逐字段透传 recall.RecallPlan。
type RecallPlanJSON struct {
	Query         string                   `json:"query"`
	Shape         string                   `json:"shape"`
	ShapeReason   string                   `json:"shape_reason"`
	Coverage      float64                  `json:"coverage"`
	ThetaCoverage float64                  `json:"theta_coverage"`
	Matched       []RecallMatchJSON        `json:"matched"`
	Unmatched     []RecallSlotJSON         `json:"unmatched"`
	Anchors       []string                 `json:"anchors"`
	Lanes         []RecallLanePlanJSON     `json:"lanes"`
	Relations     []RecallRelationPlanJSON `json:"relations"`
	Hops          int                      `json:"hops"`
	Steps         []RecallStepJSON         `json:"steps"`
	Dropped       []RecallDroppedJSON      `json:"dropped"`
}

// RecallMatchJSON 是一个已对齐槽位（对齐方法/质量/状态因子都要能看见）。
type RecallMatchJSON struct {
	SlotIndex    int     `json:"slot_index"`
	Kind         string  `json:"kind"`
	Text         string  `json:"text"`
	NodeID       string  `json:"node_id,omitempty"`
	ConceptID    string  `json:"concept_id,omitempty"`
	ConceptName  string  `json:"concept_name,omitempty"`
	Relation     string  `json:"relation,omitempty"`
	Method       string  `json:"method"`
	Quality      float64 `json:"quality"`
	StatusFactor float64 `json:"status_factor"`
	Weight       float64 `json:"weight"`
}

// RecallSlotJSON 是一个槽位（未对齐列表 / missing 共用形状）。
type RecallSlotJSON struct {
	Kind     string  `json:"kind"`
	Text     string  `json:"text"`
	Required bool    `json:"required"`
	Weight   float64 `json:"weight"`
}

// RecallLanePlanJSON 是 Planner 对一路通道的决定。
type RecallLanePlanJSON struct {
	Lane       string  `json:"lane"`
	Weight     float64 `json:"weight"`
	Budget     int     `json:"budget"`
	Selected   bool    `json:"selected"`
	Executed   bool    `json:"executed"`
	Reason     string  `json:"reason"`
	SkipReason string  `json:"skip_reason,omitempty"`
}

// RecallRelationPlanJSON 是一条关系的跳数决定（MaxHops / Origin 必须可见）。
type RecallRelationPlanJSON struct {
	Relation   string  `json:"relation"`
	Transitive bool    `json:"transitive"`
	MaxHops    int     `json:"max_hops"`
	Weight     float64 `json:"weight"`
	Origin     string  `json:"origin"`
}

// RecallStepJSON 是一步可复现的解释记录。
type RecallStepJSON struct {
	Step   string `json:"step"`
	Detail string `json:"detail"`
}

// RecallDroppedJSON 是一条被拦截的候选。
type RecallDroppedJSON struct {
	ID     string  `json:"id"`
	Reason string  `json:"reason"`
	Score  float64 `json:"score"`
}

// RecallLaneHitJSON 是一条候选在某一路通道里的贡献（来自 fusion.Contribution）。
type RecallLaneHitJSON struct {
	Lane   string  `json:"lane"`
	Rank   int     `json:"rank"`
	Weight float64 `json:"weight"`
	Value  float64 `json:"value"`
}

// RecallAdjustmentJSON 是分数调整明细（概念一致性 / 时效衰减）。
type RecallAdjustmentJSON struct {
	Kind   string  `json:"kind"`
	Factor float64 `json:"factor"`
	Reason string  `json:"reason"`
}

// RecallGraphEvidenceJSON 是 graph lane 的边证据（"凭什么"）。
type RecallGraphEvidenceJSON struct {
	Anchor   string `json:"anchor"`
	EdgeID   string `json:"edge_id"`
	From     string `json:"from"`
	To       string `json:"to"`
	Relation string `json:"relation"`
	Depth    int    `json:"depth"`
	LinkedBy string `json:"linked_by"`
}

// RecallMissingJSON 是 R4 的结构化缺槽（含可执行建议，不只是一句话）。
type RecallMissingJSON struct {
	Kind       string `json:"kind"`
	Text       string `json:"text"`
	Required   bool   `json:"required"`
	Reason     string `json:"reason"`
	Suggestion string `json:"suggestion"`
}

// RecallItemJSON 是一条召回结果 + 它的完整分数分解。
type RecallItemJSON struct {
	// 记忆本身（页面要显示内容、类型、双时态时间与溯源）。
	ID            string  `json:"id"`
	Version       int     `json:"version"`
	Scope         string  `json:"scope"`
	ConceptID     string  `json:"concept_id,omitempty"`
	Kind          string  `json:"kind"`
	Content       string  `json:"content"`
	TValid        int64   `json:"t_valid"`
	TInvalid      int64   `json:"t_invalid"`
	TCreated      int64   `json:"t_created"`
	TExpired      int64   `json:"t_expired"`
	SourceEpisode string  `json:"source_episode,omitempty"`
	Importance    float64 `json:"importance"`
	RecallCount   int     `json:"recall_count"`

	// 分数分解：Score == FusionScore × ConceptBoost × RecencyFactor（由 recall 计算）。
	Score         float64 `json:"score"`
	FusionScore   float64 `json:"fusion_score"`
	ConceptBoost  float64 `json:"concept_boost"`
	RecencyFactor float64 `json:"recency_factor"`
	// ConceptConsistent 说明 concept_boost > 1 的来由（对齐概念是否命中）。
	ConceptConsistent bool `json:"concept_consistent"`

	Contributions []RecallLaneHitJSON       `json:"contributions"`
	Adjustments   []RecallAdjustmentJSON    `json:"adjustments"`
	GraphEvidence []RecallGraphEvidenceJSON `json:"graph_evidence"`
}

// recallParams 是已校验的 /api/recall 请求参数。
type recallParams struct {
	query   string
	scope   store.Scope
	kinds   []store.MemoryKind
	asOf    *int64
	lanes   []recall.LaneName
	k       int
	explain bool
}

// parseRecallParams 解析并校验 query string。
//
// 与 parseGraphParams 同一手法：每个非法值都返回 400 + 字段名，
// 让"参数写错"与"功能不对"在现象上可区分（前端也必须显式报错而非白屏）。
func parseRecallParams(r *http.Request) (recallParams, error) {
	q := r.URL.Query()
	p := recallParams{
		query:   strings.TrimSpace(q.Get("q")),
		scope:   store.ScopeUser, // scope 是必填隔离边界；默认 user 与 /api/graph 的 search 一致
		k:       defaultRecallK,
		explain: q.Get("explain") == "1" || q.Get("explain") == "true",
	}
	if p.query == "" {
		return p, badRequest("q", "缺少 q：Recall Inspector 需要一个查询（用法 /api/recall?q=acme）")
	}
	if v := q.Get("scope"); v != "" {
		sc := store.Scope(v)
		if !sc.Valid() {
			return p, badRequest("scope", "scope 必须是 user / agent / project / global 之一，收到 %q", v)
		}
		p.scope = sc
	}
	if v := q.Get("k"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return p, badRequest("k", "k 必须是整数，收到 %q", v)
		}
		p.k = n
	}
	if v := q.Get("as_of"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return p, badRequest("as_of", "as_of 必须是 unix 秒（整数），收到 %q", v)
		}
		p.asOf = &n
	}
	if v := q.Get("kinds"); v != "" {
		for _, raw := range strings.Split(v, ",") {
			raw = strings.TrimSpace(raw)
			if raw == "" {
				continue
			}
			kind := store.MemoryKind(raw)
			if !kind.Valid() {
				return p, badRequest("kinds",
					"kinds 只能是 semantic / episodic / procedural / decision（逗号分隔），收到 %q", raw)
			}
			p.kinds = append(p.kinds, kind)
		}
	}
	if v := q.Get("lanes"); v != "" {
		for _, raw := range strings.Split(v, ",") {
			raw = strings.TrimSpace(raw)
			if raw == "" {
				continue
			}
			lane := recall.LaneName(raw)
			switch lane {
			case recall.LaneLexical, recall.LaneVector, recall.LaneGraph:
				p.lanes = append(p.lanes, lane)
			default:
				return p, badRequest("lanes",
					"lanes 只能是 lexical / vector / graph（逗号分隔），收到 %q", raw)
			}
		}
	}
	if p.k < 1 {
		return p, badRequest("k", "k 必须 >= 1，收到 %d", p.k)
	}
	if p.k > maxRecallK {
		return p, badRequest("k", "k 不能超过硬上限 %d，收到 %d（不会静默夹取）", maxRecallK, p.k)
	}
	return p, nil
}

// handleRecall 是 GET /api/recall 的处理器。
func (s *Server) handleRecall(w http.ResponseWriter, r *http.Request) {
	p, err := parseRecallParams(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	resp, err := s.buildRecall(r.Context(), p)
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// buildRecall 调一次 Recall 并逐字段复制成契约结构。
//
// 复制是刻意的（见文件头）：任何"顺手算一下"都会让解释不再是证据。
func (s *Server) buildRecall(ctx context.Context, p recallParams) (RecallResponse, error) {
	if s.recaller == nil {
		// 缺端口必须显式失败（而不是回一个空结果，让页面以为"库里没数据"）。
		// 前端据此显示 capabilities.recall_plan=false 的原因。
		return RecallResponse{}, internalError(
			"召回端口未注入：/api/recall 需要 recall.Recaller（见 meta.capabilities.recall_plan=false）")
	}
	res, err := s.recaller.Recall(ctx, recall.RecallRequest{
		Query: p.query, Scope: p.scope, Kinds: p.kinds, AsOf: p.asOf,
		Budget: recall.Budget{Final: p.k}, Lanes: p.lanes,
	})
	if err != nil {
		return RecallResponse{}, internalError("召回失败: " + err.Error())
	}

	theta := s.recaller.Config().ThetaCoverage
	out := RecallResponse{
		Meta: RecallViewMeta{
			Mode: "recall", Query: p.query, Scope: string(p.scope),
			Kinds: laneStrings(p.kinds), Lanes: laneStrings(p.lanes), K: p.k,
			AsOf: p.asOf, Explain: p.explain, KMax: maxRecallK,
			Capabilities: s.capabilities(), ThetaCoverage: theta,
		},
		Status:        string(res.Status),
		Query:         p.query,
		Scope:         string(p.scope),
		Coverage:      res.Coverage,
		AsOf:          p.asOf,
		ThetaCoverage: theta,
		Items:         make([]RecallItemJSON, 0, len(res.Items)),
		Missing:       make([]RecallMissingJSON, 0, len(res.Missing)),
		Dropped:       make([]RecallDroppedJSON, 0),
		Degraded:      append([]string{}, res.Degraded...),
	}
	for _, it := range res.Items {
		out.Items = append(out.Items, recallItemJSON(it))
	}
	for _, m := range res.Missing {
		out.Missing = append(out.Missing, RecallMissingJSON{
			Kind: string(m.Kind), Text: m.Text, Required: m.Required,
			Reason: m.Reason, Suggestion: m.Suggestion,
		})
	}
	out.Plan = recallPlanJSON(res.Plan, theta)
	// dropped 是 plan.dropped 的同源副本（同一份切片的内容，不重算不裁剪）。
	out.Dropped = append(out.Dropped, out.Plan.Dropped...)
	return out, nil
}

// recallItemJSON 复制一条结果的全部字段（含三因子与证据）。
func recallItemJSON(it recall.RecallItem) RecallItemJSON {
	m := it.Memory
	out := RecallItemJSON{
		ID: m.ID, Version: m.Version, Scope: string(m.Scope), ConceptID: m.ConceptID,
		Kind: string(m.Kind), Content: m.Content,
		TValid: m.TValid, TInvalid: m.TInvalid, TCreated: m.TCreated, TExpired: m.TExpired,
		SourceEpisode: m.SourceEpisode, Importance: m.Importance, RecallCount: m.RecallCount,

		Score: it.Score, FusionScore: it.FusionScore,
		ConceptBoost: it.ConceptBoost, RecencyFactor: it.RecencyFactor,
		ConceptConsistent: it.ConceptConsistent,

		Contributions: make([]RecallLaneHitJSON, 0, len(it.Contributions)),
		Adjustments:   make([]RecallAdjustmentJSON, 0, len(it.Adjustments)),
		GraphEvidence: make([]RecallGraphEvidenceJSON, 0, len(it.GraphEvidence)),
	}
	for _, c := range it.Contributions {
		out.Contributions = append(out.Contributions, RecallLaneHitJSON{
			Lane: string(c.Lane), Rank: c.Rank, Weight: c.Weight, Value: c.Value,
		})
	}
	for _, a := range it.Adjustments {
		out.Adjustments = append(out.Adjustments, RecallAdjustmentJSON{
			Kind: a.Kind, Factor: a.Factor, Reason: a.Reason,
		})
	}
	for _, e := range it.GraphEvidence {
		out.GraphEvidence = append(out.GraphEvidence, RecallGraphEvidenceJSON{
			Anchor: e.Anchor, EdgeID: e.EdgeID, From: e.From, To: e.To,
			Relation: string(e.Relation), Depth: e.Depth, LinkedBy: e.LinkedBy,
		})
	}
	return out
}

// recallPlanJSON 复制整份 plan（含 lane / relation / step / dropped）。
//
// plan 为 nil 时返回零值结构：R5 要求 Plan 恒非空，但契约层不能因为后端
// 违约就 panic —— 页面会看到空 shape 而不是白屏。
func recallPlanJSON(plan *recall.RecallPlan, theta float64) RecallPlanJSON {
	out := RecallPlanJSON{
		ThetaCoverage: theta,
		Matched:       make([]RecallMatchJSON, 0),
		Unmatched:     make([]RecallSlotJSON, 0),
		Anchors:       make([]string, 0),
		Lanes:         make([]RecallLanePlanJSON, 0),
		Relations:     make([]RecallRelationPlanJSON, 0),
		Steps:         make([]RecallStepJSON, 0),
		Dropped:       make([]RecallDroppedJSON, 0),
	}
	if plan == nil {
		return out
	}
	out.Query = plan.Query
	out.Shape = string(plan.Shape)
	out.ShapeReason = plan.ShapeReason
	out.Coverage = plan.Coverage
	out.ThetaCoverage = plan.ThetaCoverage
	out.Hops = plan.Hops
	out.Anchors = append(out.Anchors, plan.Anchors...)
	for _, m := range plan.Matched {
		out.Matched = append(out.Matched, RecallMatchJSON{
			SlotIndex: m.SlotIndex, Kind: string(m.Kind), Text: m.Text,
			NodeID: m.NodeID, ConceptID: m.ConceptID, ConceptName: m.ConceptName,
			Relation: string(m.Relation), Method: string(m.Method),
			Quality: m.Quality, StatusFactor: m.StatusFactor, Weight: m.Weight,
		})
	}
	for _, s := range plan.Unmatched {
		out.Unmatched = append(out.Unmatched, RecallSlotJSON{
			Kind: string(s.Kind), Text: s.Text, Required: s.Required, Weight: s.Weight,
		})
	}
	for _, lp := range plan.Lanes {
		out.Lanes = append(out.Lanes, RecallLanePlanJSON{
			Lane: string(lp.Lane), Weight: lp.Weight, Budget: lp.Budget,
			Selected: lp.Selected, Executed: lp.Executed,
			Reason: lp.Reason, SkipReason: lp.SkipReason,
		})
	}
	for _, rp := range plan.Relations {
		out.Relations = append(out.Relations, RecallRelationPlanJSON{
			Relation: string(rp.Relation), Transitive: rp.Transitive,
			MaxHops: rp.MaxHops, Weight: rp.Weight, Origin: rp.Origin,
		})
	}
	for _, st := range plan.Steps {
		out.Steps = append(out.Steps, RecallStepJSON{Step: st.Step, Detail: st.Detail})
	}
	for _, d := range plan.Dropped {
		out.Dropped = append(out.Dropped, RecallDroppedJSON{ID: d.ID, Reason: d.Reason, Score: d.Score})
	}
	return out
}

// laneStrings 把任意 Stringer-ish 的字符串类型切片转成 []string（nil → 空切片）。
func laneStrings[T ~string](in []T) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		out = append(out, string(v))
	}
	return out
}
