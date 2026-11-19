package server

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"

	"github.com/tim5wang/gmem/recall"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// GET /api/entity/{id}：实体卡（TD §9 交互清单的"点击节点 → 侧栏"）
//
// 侧栏要能一句话答完："它是什么、它和谁有关系、为什么现在还生效、它替换了什么、
// 这段话是哪来的"。因此这里的字段是**人话优先**：Why 是自然语言解释，
// 而不是让前端自己从四个时间戳里推。
// ===========================================================================

// handleEntity 是 GET /api/entity/{id} 的处理器。
func (s *Server) handleEntity(w http.ResponseWriter, r *http.Request) {
	id, err := entityIDFromPath(r.URL.Path)
	if err != nil {
		s.writeError(w, err)
		return
	}
	asOf, err := parseAsOf(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	card, err := s.buildEntity(r.Context(), id, asOf)
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, card)
}

// buildEntity 组装实体卡。
func (s *Server) buildEntity(ctx context.Context, id string, asOf *int64) (EntityCard, error) {
	node, err := s.ports.Graph.GetNode(ctx, id)
	if err != nil {
		return EntityCard{}, nodeLookupError(id, err)
	}
	// 重定向（Merge 之后旧 ID → 新 ID，TD §4）：实体卡必须跟到新实体，
	// 否则"合并"在 UI 上看起来像"这个实体没了"。
	card := EntityCard{
		ID:            node.ID,
		CanonicalName: node.CanonicalName,
		Scope:         string(node.Scope),
		Meta: Meta{
			Mode: "entity", Scope: string(node.Scope), AsOf: asOf,
			Budget: defaultBudget, BudgetMax: maxBudget,
			Relations: relationLimits(newHopTable(s.recallerConfigRules())),
		},
	}
	var degraded []string

	// 概念：直接 GetConcept（本体端口在 Store 上，不需要全量枚举）。
	if node.ConceptID != "" {
		if c, err := s.ports.Store.GetConcept(ctx, node.ConceptID); err == nil {
			card.Concept = &ConceptRef{
				ID: c.ID, Name: c.Name, Status: string(c.Status),
				Origin: string(c.Origin), Definition: c.Definition,
			}
		} else {
			// 概念读不到不是致命错误（历史数据可能指向已删/未建的概念），但要显式说。
			degraded = append(degraded, fmt.Sprintf("概念 %q 不可读: %v", node.ConceptID, err))
		}
	}

	// 来源 episode（溯源：§2 第 1 条）。
	if node.SourceEpisode != "" {
		if ep, err := s.ports.Store.GetEpisode(ctx, node.SourceEpisode); err == nil {
			card.Source = &EpisodeRef{
				ID: ep.ID, At: ep.At, Role: ep.Role, Content: truncate(ep.Content, 400),
			}
		} else {
			degraded = append(degraded, fmt.Sprintf("来源 episode %q 不可读: %v", node.SourceEpisode, err))
		}
	}

	// 全部关系（含方向与对端）。含已失效边——"它替换了什么"必须有据可查。
	edges, edgeDeg := s.edgesOfNode(ctx, node.Scope, id)
	degraded = append(degraded, edgeDeg...)

	for _, e := range edges {
		ce := EntityEdge{
			Edge: Edge{
				ID: e.ID, From: e.FromID, To: e.ToID,
				Relation:     string(e.Relation),
				Weight:       e.Weight,
				TValid:       e.TValid,
				TInvalid:     e.TInvalid,
				Invalidated:  edgeInvalidated(e, asOf),
				PossiblySame: e.Relation == relationPossiblySame,
			},
			Direction: "out",
		}
		counterpartID := e.ToID
		if e.ToID == id {
			ce.Direction = "in"
			counterpartID = e.FromID
		}
		ce.Counterpart = counterpartID
		if cp, err := s.ports.Graph.GetNode(ctx, counterpartID); err == nil {
			ce.CounterpartName = cp.CanonicalName
			ce.CounterpartConcept = cp.ConceptID
		}
		card.Edges = append(card.Edges, ce)
	}
	sort.SliceStable(card.Edges, func(i, j int) bool {
		if card.Edges[i].Relation != card.Edges[j].Relation {
			return card.Edges[i].Relation < card.Edges[j].Relation
		}
		if card.Edges[i].Direction != card.Edges[j].Direction {
			return card.Edges[i].Direction < card.Edges[j].Direction
		}
		if card.Edges[i].Counterpart != card.Edges[j].Counterpart {
			return card.Edges[i].Counterpart < card.Edges[j].Counterpart
		}
		return card.Edges[i].ID < card.Edges[j].ID
	})

	card.Relations = groupRelations(card.Edges, asOf)
	card.Why = explainEntity(node, card, asOf)
	card.Meta.Degraded = dedupeStrings(degraded)
	card.Meta.NodeCount = 1
	card.Meta.EdgeCount = len(card.Edges)
	return card, nil
}

// edgesOfNode 取该节点在 scope 内的全部边（含已失效）。
//
// 用 Neighbors 拿不到已失效边（后端默认只返回当前有效边），
// 因此这里用边表端口按端点过滤；缺端口时退化为 Neighbors（并显式降级）。
func (s *Server) edgesOfNode(ctx context.Context, scope store.Scope, id string) ([]store.Edge, []string) {
	if s.catalog != nil {
		all, err := s.catalog.ListEdges(ctx, scope, 0)
		if err == nil {
			out := make([]store.Edge, 0, 8)
			for _, e := range all {
				if e.FromID == id || e.ToID == id {
					out = append(out, e)
				}
			}
			return out, nil
		}
		return nil, []string{fmt.Sprintf("边表不可读，退化为仅当前有效边: %v", err)}
	}
	if s.ports.Graph == nil {
		return nil, []string{"边表与图端口都不可用"}
	}
	// 退路：按关系逐个 Neighbors。方向必须显式取 both：实体卡要列出“全部关系
	// （含方向与对端）”，而只走出边时入边永远看不到（F-8 之前就是这样）。
	table := newHopTable(s.recallerConfigRules())
	var out []store.Edge
	for _, rel := range table.relations() {
		hops, err := s.ports.Graph.Neighbors(ctx, store.NeighborQuery{
			NodeID: id, Scope: scope, Relations: []store.Relation{rel}, Hops: 1,
			Direction: store.DirectionBoth,
		})
		if err != nil {
			continue
		}
		for _, h := range hops {
			// both 方向下每条返回的边都必然有一端是这个节点（起点），直接收集。
			out = append(out, h.Edge)
		}
	}
	return out, []string{"后端未提供边表端口：已失效边可能缺失（实体卡不完整）"}
}

// groupRelations 把边按关系归组，并给出"为何生效 / 替换了什么"。
func groupRelations(edges []EntityEdge, asOf *int64) []RelationGroup {
	byRel := map[string][]EntityEdge{}
	order := make([]string, 0, 8)
	for _, e := range edges {
		if _, ok := byRel[e.Relation]; !ok {
			order = append(order, e.Relation)
		}
		byRel[e.Relation] = append(byRel[e.Relation], e)
	}
	sort.Strings(order)

	out := make([]RelationGroup, 0, len(order))
	for _, rel := range order {
		group := RelationGroup{Relation: rel}
		for i := range byRel[rel] {
			e := byRel[rel][i]
			if !e.Invalidated {
				if group.Effective == nil {
					cp := e
					group.Effective = &cp
				}
				continue
			}
			group.Replaced = append(group.Replaced, e)
		}
		group.Reason = groupReason(group, asOf)
		out = append(out, group)
	}
	return out
}

// groupReason 用人话说清这一组的生效判定。
func groupReason(g RelationGroup, asOf *int64) string {
	scope := "当前视角"
	if asOf != nil {
		scope = fmt.Sprintf("as_of=%d 视角", *asOf)
	}
	switch {
	case g.Effective != nil && len(g.Replaced) > 0:
		return fmt.Sprintf("%s：%d 条有效，另 %d 条已被替换（保留历史，不删除）", scope, 1, len(g.Replaced))
	case g.Effective != nil:
		return scope + "：生效"
	case len(g.Replaced) > 0:
		return fmt.Sprintf("%s：全部已失效（%d 条），仍保留以便回溯", scope, len(g.Replaced))
	default:
		return scope + "：无"
	}
}

// explainEntity 生成"为何生效 / 它替换了什么"的解释串。
func explainEntity(node store.Node, card EntityCard, asOf *int64) []string {
	var why []string
	if asOf != nil {
		why = append(why, fmt.Sprintf("以 as_of=%d（unix 秒）回溯：只把世界时间覆盖该时刻的事实当作生效", *asOf))
	} else {
		why = append(why, "当前视角：t_invalid 非零的边视为已失效（画虚线），但仍返回以便回溯")
	}
	if card.Concept != nil {
		line := fmt.Sprintf("概念 %s（%s）", card.Concept.Name, card.Concept.Status)
		if card.Concept.Origin == string(store.ConceptOriginImplicit) {
			line += "——由写入路径隐式落成的草稿，按 R2 不参与硬契约"
		}
		why = append(why, line)
	}
	if card.Source != nil {
		why = append(why, fmt.Sprintf("来源 episode %s（at=%d）", card.Source.ID, card.Source.At))
	} else if node.SourceEpisode == "" {
		why = append(why, "该实体没有来源 episode（缺少溯源）")
	}
	replaced := 0
	for _, g := range card.Relations {
		replaced += len(g.Replaced)
	}
	if replaced > 0 {
		why = append(why, fmt.Sprintf("它替换了 %d 条旧关系：旧边置 t_invalid 但保留行，因此 as_of 仍能看见它们", replaced))
	} else {
		why = append(why, "没有需要替换的旧关系")
	}
	return why
}

// parseAsOf 解析 as_of query 参数。
func parseAsOf(r *http.Request) (*int64, error) {
	v := r.URL.Query().Get("as_of")
	if v == "" {
		return nil, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return nil, badRequest("as_of", "as_of 必须是 unix 秒（整数），收到 %q", v)
	}
	return &n, nil
}

func truncate(s string, max int) string {
	rs := []rune(s)
	if len(rs) <= max {
		return s
	}
	return string(rs[:max]) + "…"
}

// recallerConfigRules 返回 recall 的关系跳数表（缺注入时退化为 recall 的默认表）。
//
// 退化时用 recall.DefaultRelationRules() 而不是本地常量：即便没有 Recaller，
// 语义来源仍然只有一处。
func (s *Server) recallerConfigRules() []recall.RelationRule {
	if s.recaller != nil {
		return s.recaller.Config().RelationRules
	}
	return recall.DefaultRelationRules()
}
