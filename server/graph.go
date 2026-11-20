package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"

	"github.com/tim5wang/gmem/recall"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// GET /api/graph：三种入口，同一套聚合
//
//	(a) ?focus=<nodeID>  以节点为中心取邻域（hops 默认 1）
//	(b) ?q=<关键词>       走 Recall 取命中实体作种子（前 K）
//	(c) 无参数            概览模式，按 concept 聚簇折叠
//
// 三条路径在 expand 之后完全合流：都变成 (节点集, 边集) → 预算折叠 → 布局。
// 这样"预算折叠"与"确定性坐标"只需实现一次，不会出现"某个入口忘了裁剪"。
// ===========================================================================

const (
	// defaultBudget 是默认节点预算（TD §9 渲染上限的起点）。
	defaultBudget = 200
	// maxBudget 是硬上限：超过即 400，**不静默夹取**。
	//
	// 为什么硬上限而不是无限：SVG 2k 节点已是可交互上限，再多只会白屏。
	// 报错比"悄悄只画 2000 个"诚实——后者会让使用者以为图里就这么多。
	maxBudget = 2000
	// defaultHops 是 focus/q 的默认跳数（TD §9：以节点为中心默认 1 跳）。
	defaultHops = 1
	// defaultSeedK 是 q 模式取命中实体的默认条数。
	defaultSeedK = 5
	// maxSeedK 是 k 的硬上限。
	maxSeedK = 50
)

// graphParams 是已校验的请求参数。
type graphParams struct {
	focus   string
	query   string
	concept string
	scope   store.Scope
	hops    int
	asOf    *int64
	budget  int
	k       int
}

// validate 是**唯一**的参数校验实现（HTTP / CLI / MCP 共用）。
//
// 为什么单独抽出来：P5 的 `gmem graph` 与 MCP `gmem_graph` 必须与 HTTP 接受
// 完全相同的输入、给出完全相同的错误。若三处各写一遍校验，就会出现
// "HTTP 拒绝、CLI 放行"这类静默漂移（而它不会报错，只会让两边行为不一致）。
func (p graphParams) validate() error {
	if p.focus != "" && p.query != "" {
		return badRequest("focus", "focus 与 q 只能二选一（概览模式请两者都不传）")
	}
	if p.scope != "" && !p.scope.Valid() {
		return badRequest("scope", "scope 必须是 user / agent / project / global 之一，收到 %q", p.scope)
	}
	if p.hops < 1 {
		return badRequest("hops", "hops 必须 >= 1，收到 %d", p.hops)
	}
	if p.budget < 1 {
		return badRequest("budget", "budget 必须 >= 1，收到 %d", p.budget)
	}
	if p.budget > maxBudget {
		return badRequest("budget", "budget 不能超过硬上限 %d，收到 %d", maxBudget, p.budget)
	}
	if p.k < 1 || p.k > maxSeedK {
		return badRequest("k", "k 必须在 1..%d 之间，收到 %d", maxSeedK, p.k)
	}
	return nil
}

// parseGraphParams 解析并校验 query string。
//
// 所有非法参数都返回 400 + 可读信息（含出错字段名），绝不 panic、
// 也绝不静默用默认值兜底——静默兜底会让"参数写错"看起来像"功能不对"。
func parseGraphParams(r *http.Request) (graphParams, error) {
	q := r.URL.Query()
	p := graphParams{
		focus:   q.Get("focus"),
		query:   q.Get("q"),
		concept: q.Get("concept"),
		hops:    defaultHops,
		budget:  defaultBudget,
		k:       defaultSeedK,
	}
	if p.focus != "" && p.query != "" {
		return p, badRequest("focus", "focus 与 q 只能二选一（概览模式请两者都不传）")
	}
	if v := q.Get("scope"); v != "" {
		sc := store.Scope(v)
		if !sc.Valid() {
			return p, badRequest("scope", "scope 必须是 user / agent / project / global 之一，收到 %q", v)
		}
		p.scope = sc
	}
	if v := q.Get("hops"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return p, badRequest("hops", "hops 必须是整数，收到 %q", v)
		}
		p.hops = n
	}
	if v := q.Get("budget"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return p, badRequest("budget", "budget 必须是整数，收到 %q", v)
		}
		p.budget = n
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
	// 范围校验统一走 validate（与 CLI / MCP 共用同一实现）。
	if err := p.validate(); err != nil {
		return p, err
	}
	return p, nil
}

// nodeAcc 是一个节点在本次视图里的累计状态。
type nodeAcc struct {
	node    store.Node
	depth   int
	degree  int
	matched bool
}

// edgeAcc 是一条边在本次视图里的累计状态。
type edgeAcc struct {
	edge  store.Edge
	depth int
	// fromNeighbors 表示这条边由 Neighbors 遍历得到（而不是从边表补的）。
	fromNeighbors bool
}

// nodeRef 是布局输入的极简投影（只保留布局需要的东西）。
type nodeRef struct {
	id         string
	conceptKey string
}

// handleGraph 是 GET /api/graph 的处理器。
func (s *Server) handleGraph(w http.ResponseWriter, r *http.Request) {
	p, err := parseGraphParams(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	resp, err := s.buildGraph(r.Context(), p)
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// buildGraph 产出一次图视图。三条入口在此收敛。
func (s *Server) buildGraph(ctx context.Context, p graphParams) (GraphResponse, error) {
	table := newHopTable(s.recaller.Config().RelationRules)
	meta := Meta{
		Scope: string(p.scope), Hops: p.hops, AsOf: p.asOf,
		Budget: p.budget, BudgetMax: maxBudget,
		Relations:    relationLimits(table),
		Capabilities: s.capabilities(),
	}

	nodes := map[string]*nodeAcc{}
	edges := map[string]*edgeAcc{}
	seeds := map[string]bool{}
	var degraded []string

	switch {
	case p.focus != "":
		meta.Mode = "focus"
		meta.Focus = p.focus
		if p.scope == "" {
			// focus 模式的 scope 可以由节点自身推导——使用者不该为了点开一个节点
			// 再传一遍 scope（视图即 URL，越短越好）。
			n, err := s.ports.Graph.GetNode(ctx, p.focus)
			if err != nil {
				return GraphResponse{}, nodeLookupError(p.focus, err)
			}
			p.scope = n.Scope
			meta.Scope = string(p.scope)
		} else if n, err := s.ports.Graph.GetNode(ctx, p.focus); err != nil {
			return GraphResponse{}, nodeLookupError(p.focus, err)
		} else if n.Scope != p.scope {
			// 隔离边界：节点存在但不属于该 scope 时一律 404（不得泄漏"它在别处"）。
			return GraphResponse{}, notFound(fmt.Sprintf("节点 %q 不在 scope %q 内", p.focus, p.scope))
		}
		seeds[p.focus] = true
		meta.Seeds = []string{p.focus}

	case p.query != "":
		meta.Mode = "search"
		meta.Query = p.query
		if p.scope == "" {
			p.scope = store.ScopeUser
			meta.Scope = string(p.scope)
		}
		res, err := s.recallSeeds(ctx, p.query, p.scope, p.asOf, p.k)
		if err != nil {
			return GraphResponse{}, err
		}
		degraded = append(degraded, res.degraded...)
		for _, id := range res.seeds {
			seeds[id] = true
		}
		meta.Seeds = res.seeds
		meta.Recall = res.meta

	default:
		meta.Mode = "overview"
		if p.scope == "" {
			p.scope = store.ScopeUser
			meta.Scope = string(p.scope)
		}
		if err := s.loadOverview(ctx, p.scope, nodes, edges); err != nil {
			return GraphResponse{}, err
		}
	}

	// focus / search 走图遍历；概览已经直接取到全集。
	if meta.Mode != "overview" && len(seeds) > 0 {
		seedIDs := make([]string, 0, len(seeds))
		for id := range seeds {
			seedIDs = append(seedIDs, id)
		}
		sort.Strings(seedIDs) // 确定性：遍历顺序不能依赖 map 迭代
		expDeg, err := s.expand(ctx, p.scope, seedIDs, p.hops, p.asOf, table, nodes, edges)
		if err != nil {
			return GraphResponse{}, err
		}
		degraded = append(degraded, expDeg...)
	}

	// 边表补齐：Neighbors 在 as_of=nil 时**只返回当前有效边**，
	// 因此已失效边必须从边表补回来——否则前端无法画虚线（TD §9 视觉编码）。
	degraded = append(degraded, s.fillEdgesFromCatalog(ctx, p.scope, p, table, nodes, edges)...)

	// 解析节点本体（Neighbors 只给 ID）。
	degraded = append(degraded, s.resolveNodes(ctx, p.scope, nodes)...)

	// concept 过滤（P5 的 gmem_graph 参数）：只保留该概念下的节点。
	// 放在 resolveNodes 之后：概念归属要靠解析出来的 node.ConceptID 判定。
	if p.concept != "" {
		kept := 0
		for id, acc := range nodes {
			if acc.node.ConceptID != p.concept {
				delete(nodes, id)
				continue
			}
			kept++
		}
		if kept == 0 {
			// 不得静默返回一张空图：明确告知“该概念下没有节点”。
			degraded = append(degraded, fmt.Sprintf("concept 过滤 %q 未命中任何节点", p.concept))
		}
	}

	// 丢边：任何一端的节点都解析不出来 → 这条边画不出来。
	pruneDanglingEdges(nodes, edges)

	// 度数（决定节点大小）：用最终边集算，含已失效边。
	for _, e := range edges {
		if n, ok := nodes[e.edge.FromID]; ok {
			n.degree++
		}
		if n, ok := nodes[e.edge.ToID]; ok {
			n.degree++
		}
	}
	for id := range seeds {
		if n, ok := nodes[id]; ok {
			n.matched = true
		}
	}

	meta.NodeCount = len(nodes)
	meta.EdgeCount = len(edges)

	resp := GraphResponse{Meta: meta}
	nodesOut, clusters, clusterEdges, truncated, layoutDeg := s.layout(ctx, nodes, edges, p)
	degraded = append(degraded, layoutDeg...)
	resp.Nodes = nodesOut
	resp.Clusters = clusters
	if truncated {
		resp.ClusterEdges = clusterEdges
	}
	resp.Edges = s.edgeViews(nodesOut, edges, p)
	resp.Meta.Truncated = truncated
	resp.Meta.ClusterCount = len(clusters)
	resp.Meta.RenderedNodes = len(nodesOut)
	resp.Meta.RenderedClusters = len(clusters)
	resp.Meta.Degraded = dedupeStrings(degraded)
	return resp, nil
}

// expand 从种子出发做定向遍历。
//
// **跳数裁剪复用 recall 的关系传递性表**：每个关系先查表得到 MaxHops，
// 再与请求跳数取更严者；跳数相同的关系统一放进一次 Neighbors 调用
// （GraphStore 的白名单是按关系传的，因此不能把不同跳数的关系混在一起）。
//
// **方向固定为 both（F-8）**：可视化要回答“这个实体旁边有什么”，答案不分正反。
// 之前只沿 from→to 走，于是扇入型枢纽（如 acme：入边 2/出边 0）focus 出来
// 是 nodes=1 edges=0 的空图，而 degree（下一段按最终边集双向统计）却很大——
// 界面上就是“圆圈很大却一根边都没有”。注意这**不是**把端口默认值改成双向：
// recall 的 graph lane 继续用零值（= outbound），它的定向语义不能被可视化拖下水。
func (s *Server) expand(ctx context.Context, scope store.Scope, seeds []string, requested int,
	asOf *int64, table hopTable, nodes map[string]*nodeAcc, edges map[string]*edgeAcc) ([]string, error) {
	if s.ports.Graph == nil {
		return nil, internalError("图端口未注入（store.Ports.Graph 为 nil）")
	}
	// 按"生效跳数"分桶。分桶顺序由表的固定顺序决定，因此可复现。
	groups := map[int][]store.Relation{}
	for _, rel := range table.relations() {
		eff := table.effectiveHops(rel, requested)
		groups[eff.MaxHops] = append(groups[eff.MaxHops], rel)
	}
	hopValues := make([]int, 0, len(groups))
	for h := range groups {
		hopValues = append(hopValues, h)
	}
	sort.Ints(hopValues)

	var degraded []string
	for _, seed := range seeds {
		if _, ok := nodes[seed]; !ok {
			nodes[seed] = &nodeAcc{depth: 0}
		}
		for _, h := range hopValues {
			hops, err := s.ports.Graph.Neighbors(ctx, store.NeighborQuery{
				NodeID: seed, Scope: scope, Relations: groups[h], Hops: h, AsOf: asOf,
				Direction: store.DirectionBoth,
			})
			if err != nil {
				// 单次遍历失败不整单失败：显式降级并继续其它关系簇，
				// 否则一个坏关系会让整张图空白（而且使用者看不出为什么）。
				degraded = append(degraded, fmt.Sprintf("neighbors(%s, hops=%d): %v", seed, h, err))
				continue
			}
			for _, eh := range hops {
				if eh.Edge.FromID == "" || eh.Edge.ToID == "" {
					continue
				}
				key := eh.Edge.ID
				prev, ok := edges[key]
				if !ok || eh.Depth < prev.depth {
					edges[key] = &edgeAcc{edge: eh.Edge, depth: eh.Depth, fromNeighbors: true}
				}
				// 节点深度：被走到的那端与边同跳，另一端浅一跳。
				//
				// 不能写死 "from 浅/to 深"：Direction=both 时一条边可能从 to 端走过来，
				// 此时 closer 的是 to（不是 from）。写死会让枢纽节点的全部邻居都
				// 被算成比种子还浅或更深，前端 depth 视觉编码跟着说谎。
				reached, far := hopEnds(eh)
				recordDepth(nodes, reached, eh.Depth)
				recordDepth(nodes, far, eh.Depth-1)
			}
		}
	}
	return degraded, nil
}

// hopEnds 返回 (被走到的端点, 另一端)。
//
// Reached 为零值时退化为 outbound 语义（to 端是前进目标）：那是 F-8 之前的
// 后端约定，不能因为新字段缺失就让老实现算出镜像的深度。
func hopEnds(eh store.EdgeHop) (reached, far string) {
	switch eh.Reached {
	case eh.Edge.FromID:
		return eh.Edge.FromID, eh.Edge.ToID
	case eh.Edge.ToID:
		return eh.Edge.ToID, eh.Edge.FromID
	}
	return eh.Edge.ToID, eh.Edge.FromID
}

// recordDepth 只在更浅时更新节点深度（同节点经多条边到达时取最短）。
func recordDepth(nodes map[string]*nodeAcc, id string, depth int) {
	if depth < 0 {
		depth = 0
	}
	n, ok := nodes[id]
	if !ok {
		nodes[id] = &nodeAcc{depth: depth}
		return
	}
	if depth < n.depth {
		n.depth = depth
	}
}

// fillEdgesFromCatalog 用边表补齐 Neighbors 看不到的边（主要是已失效边）。
//
// # 为什么必须补
//
// sqlite 的 Neighbors 在 AsOf 为空时带 `t_invalid IS NULL AND t_expired IS NULL`，
// 也就是**只返回当前有效边**。而 TD §9 要求已失效边"变浅但仍可见"（虚线）。
// 只在 Neighbors 的结果上画图，历史边就永远画不出来——那不是"视觉简化"，
// 而是把双时态这个卖点从图里删掉了。
//
// 补边**不改变拓扑**：只补一端已在视图里的边，且对端深度必须仍在
// 该关系的跳数上限内，因此不会因为补边把图悄悄扩大一跳。
// 缺边表端口时显式降级，绝不静默。
// 补边**不得突破跳数上限**：已失效边虽然不参与遍历，但它两端节点的深度
// 仍受 recall 的关系表约束。否则一条失效的 works_at 边就会把两跳之外的
// "别人公司"带进图里——那正是 TD §9 要避免的噪声。
func (s *Server) fillEdgesFromCatalog(ctx context.Context, scope store.Scope, p graphParams,
	table hopTable, nodes map[string]*nodeAcc, edges map[string]*edgeAcc) []string {
	if s.catalog == nil || len(nodes) == 0 {
		return nil
	}
	all, err := s.catalog.ListEdges(ctx, scope, 0)
	if err != nil {
		return []string{fmt.Sprintf("边表不可读（已失效边可能缺失）: %v", err)}
	}
	for _, e := range all {
		from, okF := nodes[e.FromID]
		to, okT := nodes[e.ToID]
		if !okF && !okT {
			continue
		}
		if p.asOf != nil && e.TCreated > *p.asOf {
			// 该时刻这条边还没写进来：回溯视角不得把"未来"的边画进去（系统时间）。
			continue
		}
		// 已失效边仍要返回（前端画虚线），但它的对端可能只靠这条失效边才可达：
		// Neighbors 不会遍历失效边，于是对端不会出现在节点集里，虚线边就永远画不出来。
		// 因此在这里把缺失的对端补成节点。
		//
		// **缺失的一端一律比已知的一端深一跳（F-8）**：补出来的面只能是"从视图里
		// 已可见的部分再往外一层"，因为这条边是我们唯一能看到它的途径。
		//
		// 曾经这里写死了"from 浅一跳 / to 深一跳"，于是当焦点本身就是 to 端
		// （扇入型枢纽）且它唯一的入边已失效时，代码会认为"种子已是最浅，不能再往上补"
		// 而直接跳过——枢纽依旧是一张空图（实测 nodes=[n-b] edges=0）。
		// 方向改成 both 之后，哪一端离种子更近不再由边自身决定，只能由视图决定。
		//
		// 概览模式（无 focus/q）不受跳数约束：那时节点集是全量的，没有"种子"概念。
		if p.focus != "" || p.query != "" {
			limit := table.effectiveHops(e.Relation, p.hops).MaxHops
			known := from
			if okT {
				known = to
			}
			if known != nil && known.depth+1 > limit {
				continue
			}
		}
		if !okF {
			from = &nodeAcc{depth: to.depth + 1}
			nodes[e.FromID] = from
		}
		if !okT {
			to = &nodeAcc{depth: from.depth + 1}
			nodes[e.ToID] = to
		}
		if _, ok := edges[e.ID]; ok {
			// Neighbors 已经给过这条边（可能是 as_of 视角下的历史版本）：
			// 保留遍历结果（它的时间视角更准确），不覆盖。
			continue
		}
		edges[e.ID] = &edgeAcc{edge: e, depth: maxInt(from.depth, to.depth)}
	}
	return nil
}

// loadOverview 是概览模式：直接取该 scope 的全量节点与边。
//
// 为什么这里可以"取全量"：概览本身就是"按 concept 聚簇"，聚簇需要准确的
// 规模计数（否则 clusters 的 count 是假的）。取全量后用预算折叠，**渲染规模**
// 仍受 budget 约束；真正的代价是计数时扫一遍节点表（TD 的规模假设是万级）。
func (s *Server) loadOverview(ctx context.Context, scope store.Scope, nodes map[string]*nodeAcc, edges map[string]*edgeAcc) error {
	if s.catalog == nil {
		// 没有节点枚举端口时，概览模式无内容可画：显式报错而不是返回一张空图。
		return internalError("概览模式需要节点枚举端口（ListNodes/ListEdges）；当前注入的后端未实现")
	}
	allNodes, err := s.catalog.ListNodes(ctx, scope, 0)
	if err != nil {
		return internalError("读取节点失败: " + err.Error())
	}
	for _, n := range allNodes {
		nodes[n.ID] = &nodeAcc{node: n, depth: 0}
	}
	allEdges, err := s.catalog.ListEdges(ctx, scope, 0)
	if err != nil {
		return internalError("读取边失败: " + err.Error())
	}
	for _, e := range allEdges {
		if _, ok := nodes[e.FromID]; !ok {
			continue
		}
		if _, ok := nodes[e.ToID]; !ok {
			continue
		}
		edges[e.ID] = &edgeAcc{edge: e}
	}
	return nil
}

// resolveNodes 补齐节点的本体信息（CanonicalName / ConceptID / Scope）。
func (s *Server) resolveNodes(ctx context.Context, scope store.Scope, nodes map[string]*nodeAcc) []string {
	var degraded []string
	ids := make([]string, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids) // 确定性 + 错误信息顺序稳定
	for _, id := range ids {
		acc := nodes[id]
		if acc.node.ID != "" {
			continue // 概览模式已经从 ListNodes 拿到了
		}
		n, err := s.ports.Graph.GetNode(ctx, id)
		if err != nil {
			// 标记为不可解析（ID 留空），后续 pruneDanglingEdges 会连边一起丢掉。
			degraded = append(degraded, fmt.Sprintf("节点 %q 不可读: %v", id, err))
			continue
		}
		if n.Scope != scope {
			// 跨 scope 的节点不得进入视图（隔离边界）。
			degraded = append(degraded, fmt.Sprintf("节点 %q 属于 scope %q，已从 scope %q 的视图中排除", id, n.Scope, scope))
			continue
		}
		acc.node = n
	}
	return degraded
}

// pruneDanglingEdges 丢掉端点不可解析的边，并清理孤立空节点。
func pruneDanglingEdges(nodes map[string]*nodeAcc, edges map[string]*edgeAcc) {
	for id, acc := range nodes {
		if acc.node.ID == "" {
			delete(nodes, id)
		}
	}
	for id, acc := range edges {
		if _, ok := nodes[acc.edge.FromID]; !ok {
			delete(edges, id)
			continue
		}
		if _, ok := nodes[acc.edge.ToID]; !ok {
			delete(edges, id)
		}
	}
}

// seedResult 是 q 模式的种子集合与回显信息。
type seedResult struct {
	seeds    []string
	meta     *RecallMeta
	degraded []string
}

// recallSeeds 走 Recall 取命中实体作种子（前 K）。
//
// 种子来源按可信度排序：对齐出的实体锚点（词典/概念命中，最确定）→
// graph lane 给出的边端点（间接命中）→ 命中概念下的节点（兜底）。
// 三条来源都空时返回空种子（前端显示"没有命中实体"），而不是拿全图充数。
func (s *Server) recallSeeds(ctx context.Context, query string, scope store.Scope, asOf *int64, k int) (seedResult, error) {
	if s.recaller == nil {
		return seedResult{}, internalError("召回端口未注入")
	}
	res, err := s.recaller.Recall(ctx, recall.RecallRequest{
		Query: query, Scope: scope, AsOf: asOf,
		Budget: recall.Budget{Final: k},
	})
	if err != nil {
		return seedResult{}, internalError("召回失败: " + err.Error())
	}
	out := seedResult{
		meta: &RecallMeta{
			Status:   string(res.Status),
			Coverage: res.Coverage,
			Items:    len(res.Items),
		},
		degraded: append([]string(nil), res.Degraded...),
	}
	seen := map[string]bool{}
	add := func(id string) {
		if id == "" || seen[id] || len(out.seeds) >= k {
			return
		}
		seen[id] = true
		out.seeds = append(out.seeds, id)
	}
	if res.Plan != nil {
		out.meta.Anchors = append([]string(nil), res.Plan.Anchors...)
		for _, id := range res.Plan.Anchors {
			add(id)
		}
	}
	for _, item := range res.Items {
		for _, ev := range item.GraphEvidence {
			add(ev.From)
			add(ev.To)
		}
	}
	if len(out.seeds) == 0 && s.catalog != nil && res.Plan != nil {
		conceptIDs := map[string]bool{}
		for _, m := range res.Plan.Matched {
			if m.ConceptID != "" {
				conceptIDs[m.ConceptID] = true
			}
		}
		if len(conceptIDs) > 0 {
			all, err := s.catalog.ListNodes(ctx, scope, 0)
			if err != nil {
				out.degraded = append(out.degraded, "概念兜底取种子失败: "+err.Error())
			} else {
				ids := make([]string, 0, len(all))
				for _, n := range all {
					if conceptIDs[n.ConceptID] {
						ids = append(ids, n.ID)
					}
				}
				sort.Strings(ids) // 确定性：不能依赖后端返回顺序
				for _, id := range ids {
					add(id)
				}
			}
		}
	}
	return out, nil
}

// layout 把节点集变成"可直接渲染"的视图：算坐标，并在超预算时折叠。
func (s *Server) layout(ctx context.Context, nodes map[string]*nodeAcc, edges map[string]*edgeAcc, p graphParams) ([]Node, []Cluster, []ClusterEdge, bool, []string) {
	var degraded []string
	concepts := map[string]store.Concept{}
	if or, ok := s.ports.Store.(store.OntologyReader); ok && or != nil {
		list, err := or.ListConcepts(ctx)
		if err != nil {
			degraded = append(degraded, "概念枚举失败（concept_status/origin 可能缺失）: "+err.Error())
		} else {
			for _, c := range list {
				concepts[c.ID] = c
			}
		}
	} else {
		degraded = append(degraded, "后端未实现 store.OntologyReader：concept_status/origin 缺失")
	}

	// 1) 聚簇键（确定性排序）→ 团心坐标。
	ids := make([]string, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	refs := make([]nodeRef, 0, len(ids))
	for _, id := range ids {
		refs = append(refs, nodeRef{id: id, conceptKey: conceptKeyOf(nodes[id].node.ConceptID)})
	}
	keys := clusterKeys(refs)
	centers := make(map[string][2]float64, len(keys))
	for i, k := range keys {
		x, y := clusterCenter(i)
		centers[k] = [2]float64{x, y}
	}

	// 2) 折叠判定：节点数 > budget → 先聚簇折叠（不是降帧，也不是静默截断）。
	total := len(nodes)
	truncated := total > p.budget

	// 3) 折叠视图只保留**命中种子**为真实节点，其余进 concept 团。
	//    理由：命中实体是使用者这次要看的东西，把它一起折叠掉等于把答案藏起来。
	keep := func(acc *nodeAcc) bool {
		if !truncated {
			return true
		}
		return acc.matched
	}

	outNodes := make([]Node, 0, len(ids))
	for _, id := range ids {
		acc := nodes[id]
		if !keep(acc) {
			continue
		}
		key := conceptKeyOf(acc.node.ConceptID)
		c := centers[key]
		x, y := nodePosition(id, c[0], c[1])
		outNodes = append(outNodes, Node{
			ID:            acc.node.ID,
			CanonicalName: acc.node.CanonicalName,
			Scope:         string(acc.node.Scope),
			ConceptID:     acc.node.ConceptID,
			ConceptName:   conceptName(concepts, acc.node.ConceptID),
			ConceptStatus: conceptStatus(concepts, acc.node.ConceptID),
			Origin:        conceptOrigin(concepts, acc.node.ConceptID),
			SourceEpisode: acc.node.SourceEpisode,
			Degree:        acc.degree,
			Depth:         acc.depth,
			Matched:       acc.matched,
			Cluster:       clusterDisplayKey(acc.node.ConceptID),
			X:             x,
			Y:             y,
		})
	}

	// 4) 聚簇汇总（无论是否折叠都返回：它是"本体覆盖"的免费信息）。
	counts := map[string]int{}
	matchedIn := map[string]bool{}
	for _, id := range ids {
		acc := nodes[id]
		k := conceptKeyOf(acc.node.ConceptID)
		counts[k]++
		if acc.matched {
			matchedIn[k] = true
		}
	}
	clusters := make([]Cluster, 0, len(keys))
	conceptOf := map[string]string{}
	for _, id := range ids {
		acc := nodes[id]
		k := conceptKeyOf(acc.node.ConceptID)
		if acc.node.ConceptID != "" {
			conceptOf[k] = acc.node.ConceptID
		}
	}
	for _, k := range keys {
		c := centers[k]
		cid := conceptOf[k]
		clusters = append(clusters, Cluster{
			ID:            clusterDisplayKey(cid),
			ConceptID:     cid,
			ConceptName:   clusterConceptName(concepts, cid, k),
			ConceptStatus: conceptStatus(concepts, cid),
			Origin:        conceptOrigin(concepts, cid),
			Count:         counts[k],
			X:             c[0],
			Y:             c[1],
			Matched:       matchedIn[k],
		})
	}

	// 5) 折叠视图的聚合边（团 → 团）。
	var clusterEdges []ClusterEdge
	if truncated {
		type agg struct {
			count      int
			allInvalid bool
		}
		m := map[[2]string]*agg{}
		for _, edge := range sortEdges(edges) {
			fromKey := conceptKeyOf(nodes[edge.edge.FromID].node.ConceptID)
			toKey := conceptKeyOf(nodes[edge.edge.ToID].node.ConceptID)
			if fromKey == toKey {
				continue // 团内边：折叠后没有意义
			}
			k := [2]string{fromKey, toKey}
			a, ok := m[k]
			if !ok {
				a = &agg{allInvalid: true}
				m[k] = a
			}
			a.count++
			if !edgeInvalidated(edge.edge, p.asOf) {
				a.allInvalid = false
			}
		}
		keysSorted := make([][2]string, 0, len(m))
		for k := range m {
			keysSorted = append(keysSorted, k)
		}
		sort.Slice(keysSorted, func(i, j int) bool {
			if keysSorted[i][0] != keysSorted[j][0] {
				return keysSorted[i][0] < keysSorted[j][0]
			}
			return keysSorted[i][1] < keysSorted[j][1]
		})
		for _, k := range keysSorted {
			a := m[k]
			clusterEdges = append(clusterEdges, ClusterEdge{
				From: clusterDisplayKey(conceptOf[k[0]]), To: clusterDisplayKey(conceptOf[k[1]]),
				Count: a.count, Invalidated: a.allInvalid,
			})
		}
	}

	return outNodes, clusters, clusterEdges, truncated, degraded
}

// edgeViews 把边集投影成响应体。
//
// 只输出**两端都实际渲染**的边：折叠视图里被折掉的节点如果还留着边，
// 前端会拿到一堆指向不存在节点的悬空边（画出来就是一堆线段）。
// 输出按边 ID 排序——响应必须是确定的，否则"逐字节一致"的回归用例不成立。
func (s *Server) edgeViews(rendered []Node, edges map[string]*edgeAcc, p graphParams) []Edge {
	present := make(map[string]bool, len(rendered))
	for _, n := range rendered {
		present[n.ID] = true
	}
	out := make([]Edge, 0, len(edges))
	for _, acc := range sortEdges(edges) {
		e := acc.edge
		if !present[e.FromID] || !present[e.ToID] {
			continue
		}
		out = append(out, Edge{
			ID: e.ID, From: e.FromID, To: e.ToID,
			Relation: string(e.Relation), Weight: e.Weight,
			TValid: e.TValid, TInvalid: e.TInvalid,
			Invalidated:  edgeInvalidated(e, p.asOf),
			PossiblySame: e.Relation == relationPossiblySame,
			Depth:        acc.depth,
		})
	}
	return out
}

// sortEdges 按边 ID 返回排序后的边（确定性，避免 map 迭代顺序影响聚合）。
func sortEdges(edges map[string]*edgeAcc) []*edgeAcc {
	out := make([]*edgeAcc, 0, len(edges))
	for _, e := range edges {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].edge.ID < out[j].edge.ID })
	return out
}

// relationLimits 把跳数表回显成响应里的 meta.relations。
func relationLimits(t hopTable) []RelationLimit {
	out := make([]RelationLimit, 0, len(t.order))
	for _, r := range t.order {
		out = append(out, RelationLimit{
			Relation: string(r.Relation), MaxHops: r.MaxHops, Transitive: r.Transitive,
		})
	}
	return out
}

// edgeInvalidated 判断一条边在本次视角下是否已失效。
//
// as_of 为空 = 当前视角：t_invalid 非零即失效（这正是"已失效但仍返回"的来源）。
// as_of 非空 = 回溯视角：世界时间区间不覆盖 as_of 即失效。
func edgeInvalidated(e store.Edge, asOf *int64) bool {
	if asOf == nil {
		return e.TInvalid != 0
	}
	valid := e.TValid <= *asOf && (e.TInvalid == 0 || e.TInvalid > *asOf)
	return !valid
}

func conceptName(concepts map[string]store.Concept, id string) string {
	if id == "" {
		return ""
	}
	return concepts[id].Name
}

func conceptStatus(concepts map[string]store.Concept, id string) string {
	if id == "" {
		return ""
	}
	return string(concepts[id].Status)
}

func conceptOrigin(concepts map[string]store.Concept, id string) string {
	if id == "" {
		return ""
	}
	return string(concepts[id].Origin)
}

func clusterConceptName(concepts map[string]store.Concept, cid, key string) string {
	if cid == "" {
		return unclusteredLabel
	}
	return concepts[cid].Name
}

// clusterDisplayKey 是给前端看的聚簇键（未归属概念用一个可读值）。
func clusterDisplayKey(conceptID string) string {
	if conceptID == "" {
		return "(unclustered)"
	}
	return conceptID
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out) // 确定性：降级清单的顺序不该随调用次数变化
	return out
}

// nodeLookupError 把 GetNode 的错误映射成 HTTP 语义。
func nodeLookupError(id string, err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return notFound(fmt.Sprintf("节点 %q 不存在", id))
	}
	return internalError(fmt.Sprintf("读取节点 %q 失败: %v", id, err))
}
