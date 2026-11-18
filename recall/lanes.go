package recall

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/tim5wang/gmem/store"
)

// lanes 是 §5 时序图里的并行召回段：lexical / vector / graph 三路各自产出排名列表。
//
// 并行是刻意的（§5 "par 并行执行"）：三路之间没有数据依赖，串行只会让
// 召回延迟等于三路之和。这里用标准库 WaitGroup 而不是 errgroup（不引入第三方依赖）。
//
// 单路失败不使整次召回失败：该 lane 记入 Degraded（§13 "降级可见"），
// 其余通道继续——这比整体报错更符合"记忆系统可以部分可用"的现实。
// 唯一例外是 ctx 被取消：那是调用方意图，必须整体返回错误。

// laneOutcome 是一路通道的执行结果。
type laneOutcome struct {
	lane LaneName
	hits []store.Hit
	// evidenceByID 是 graph lane 每条候选的边证据（键=记忆 ID）。
	// 按候选分装而不是拍平：拍平后无法回答"这条记忆凭什么被召回"。
	evidenceByID map[string][]GraphEvidence
	// dropped 是本路内部就被拦截的候选（例如节点无法映射到记忆）。
	dropped  []DroppedItem
	degraded []string
	err      error
}

// laneInput 是一次多路召回的全部输入（只读，供并行 goroutine 共享）。
type laneInput struct {
	req        RecallRequest
	align      Alignment
	plan       *RecallPlan
	scope      store.Scope
	perLane    int
	embedQuery []float32
}

// runLanes 并行执行 plan 里 "被选中且未被跳过" 的通道。
//
// 选择条件是 Selected && SkipReason == ""，而**不是** Executed：
// Executed 由调用方在通道真正返回后置位（F1/F6），此时还未决定。
func (r *Recaller) runLanes(ctx context.Context, in laneInput) ([]laneOutcome, error) {
	type job struct {
		lane LanePlan
	}
	var jobs []job
	for _, lp := range in.plan.Lanes {
		if lp.Selected && lp.SkipReason == "" {
			jobs = append(jobs, job{lane: lp})
		}
	}

	out := make([]laneOutcome, len(jobs))
	var wg sync.WaitGroup
	for i := range jobs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			lp := jobs[i].lane
			res := laneOutcome{lane: lp.Lane}
			switch lp.Lane {
			case LaneLexical:
				res.hits, res.err = r.lexicalLane(ctx, in)
			case LaneVector:
				res.hits, res.err = r.vectorLane(ctx, in)
			case LaneGraph:
				res.hits, res.evidenceByID, res.dropped, res.err = r.graphLane(ctx, in)
			default:
				res.err = fmt.Errorf("recall: unknown lane %q", lp.Lane)
			}
			out[i] = res
		}(i)
	}
	wg.Wait()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// 有 lane 出错仍然返回结果，错误在 collectDegraded 里转成 Degraded 文本。
	return out, nil
}

// lexicalLane 走 FTS5 + bigram（P0 已验证双侧对称）。
func (r *Recaller) lexicalLane(ctx context.Context, in laneInput) ([]store.Hit, error) {
	hits, err := r.ports.Lexical.Search(ctx, store.LexicalQuery{
		Query: in.req.Query,
		Scope: in.req.Scope,
		AsOf:  in.req.AsOf,
		Limit: in.perLane,
	})
	if err != nil {
		return nil, fmt.Errorf("lexical lane: %w", err)
	}
	return hits, nil
}

// vectorLane 走 int8 近邻。无 Embedder 时根本不会被计划为 Executed，
// 因此这里只需处理"有 embedder 但调用失败"的情形（失败即降级，不静默）。
func (r *Recaller) vectorLane(ctx context.Context, in laneInput) ([]store.Hit, error) {
	if len(in.embedQuery) == 0 {
		return nil, fmt.Errorf("vector lane: empty query embedding")
	}
	hits, err := r.ports.Vector.Search(ctx, in.embedQuery, in.req.Scope, in.perLane)
	if err != nil {
		return nil, fmt.Errorf("vector lane: %w", err)
	}
	return hits, nil
}

// graphLane 做**定向**扩展（§5 R3）：从对齐出的实体锚点出发，
// 只沿 plan 里选定的关系与跳数走，然后把节点映射回记忆。
//
// 与 Graphiti 盲 BFS 的区别就在这里：跳数由每条 relation 的传递性决定，
// 而不是一个全局 max_depth=3 套到所有边类型上。
func (r *Recaller) graphLane(ctx context.Context, in laneInput) ([]store.Hit, map[string][]GraphEvidence, []DroppedItem, error) {
	// 按跳数分组：同一跳数上限的关系可以一次遍历（减少图查询次数）。
	byHops := map[int][]store.Relation{}
	var hopList []int
	for _, rp := range in.plan.Relations {
		if rp.MaxHops <= 0 {
			continue
		}
		if _, ok := byHops[rp.MaxHops]; !ok {
			hopList = append(hopList, rp.MaxHops)
		}
		byHops[rp.MaxHops] = append(byHops[rp.MaxHops], rp.Relation)
	}
	sort.Ints(hopList)

	type cand struct {
		id    string
		score float64
		depth int
		ev    []GraphEvidence
	}
	best := map[string]*cand{}
	var mu sync.Mutex
	var dropped []DroppedItem
	var degraded []string

	var wg sync.WaitGroup
	for _, anchor := range in.align.Anchors {
		for _, hops := range hopList {
			rels := byHops[hops]
			wg.Add(1)
			go func(anchor string, hops int, rels []store.Relation) {
				defer wg.Done()
				hopsOut, err := r.ports.Graph.Neighbors(ctx, store.NeighborQuery{
					NodeID:    anchor,
					Scope:     in.scope,
					Relations: rels,
					Hops:      hops,
					AsOf:      in.req.AsOf,
				})
				if err != nil {
					mu.Lock()
					degraded = append(degraded, fmt.Sprintf("graph lane: neighbors(%s, hops=%d): %v", anchor, hops, err))
					mu.Unlock()
					return
				}
				ids, ev, drop := r.mapHopsToMemories(ctx, anchor, hopsOut, in)
				mu.Lock()
				defer mu.Unlock()
				dropped = append(dropped, drop...)
				for i, id := range ids {
					c, ok := best[id]
					if !ok {
						c = &cand{id: id, depth: ev[i].Depth, score: 0}
						best[id] = c
					}
					if ev[i].Depth < c.depth {
						c.depth = ev[i].Depth
					}
					c.score += relationScore(in.plan, ev[i])
					c.ev = append(c.ev, ev[i])
				}
			}(anchor, hops, rels)
		}
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, err
	}
	// F7：单个锚点/跳数组失败不再拖垮整条 graph lane。
	//
	// 旧实现只要有一个 Neighbors 报错就 return nil —— 成功锚点已经算出的候选
	// 一起被丢掉，多锚点问句里一个实体查询失败整条图通道归零。现在：失败记进
	// degraded（并在 plan 里可见），但**保留其他锚点的候选**；没有候选时才冒错。
	var partialErr error
	if len(degraded) > 0 {
		partialErr = fmt.Errorf("%s", joinStrings(degraded, "; "))
	}

	cands := make([]*cand, 0, len(best))
	for _, c := range best {
		cands = append(cands, c)
	}
	// 近的优先，同深度按关系权重累加分降序，最后按 ID 保证确定性。
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].depth != cands[j].depth {
			return cands[i].depth < cands[j].depth
		}
		if cands[i].score != cands[j].score {
			return cands[i].score > cands[j].score
		}
		return cands[i].id < cands[j].id
	})
	if in.perLane > 0 && len(cands) > in.perLane {
		for _, c := range cands[in.perLane:] {
			dropped = append(dropped, DroppedItem{ID: c.id, Reason: "graph lane 预算切断（per-lane budget）", Score: c.score})
		}
		cands = cands[:in.perLane]
	}

	hits := make([]store.Hit, 0, len(cands))
	evidenceByID := make(map[string][]GraphEvidence, len(cands))
	for i, c := range cands {
		// Version 留 0：graph 通道按节点映射回记忆，不知道命中的是哪个系统时间版本，
		// 后端的当前/db 版本解析在 buildItems 里由 resolveMemories 完成（store.Hit 的约定）。
		hits = append(hits, store.Hit{ID: c.id, Score: c.score, Rank: i + 1})
		evidenceByID[c.id] = c.ev
	}
	return hits, evidenceByID, dropped, partialErr
}

// mapHopsToMemories 把图上的边映射回记忆 ID，并给出可解释的证据。
//
// 两条映射路径（都写进 GraphEvidence.LinkedBy，便于 explain 回答"凭什么"）：
//  1. 节点 concept_id → memories.concept_id（概念型记忆）；
//  2. 节点 source_episode → memories.source_episode（事件型记忆，同一 episode 产出）。
//
// 走不通的节点记入 dropped，而不是静默丢弃——"图上有边但映射不到记忆"
// 是 P2 抽取质量问题的直接信号，必须可见。
func (r *Recaller) mapHopsToMemories(ctx context.Context, anchor string, hops []store.EdgeHop, in laneInput) ([]string, []GraphEvidence, []DroppedItem) {
	var ids []string
	var ev []GraphEvidence
	var dropped []DroppedItem
	seen := map[string]bool{}
	visited := map[string]bool{anchor: true}

	for _, h := range hops {
		for _, nodeID := range []string{h.Edge.FromID, h.Edge.ToID} {
			if visited[nodeID] {
				continue
			}
			visited[nodeID] = true
			n, err := r.ports.Graph.GetNode(ctx, nodeID)
			if err != nil {
				dropped = append(dropped, DroppedItem{
					ID: nodeID, Reason: fmt.Sprintf("graph: get node %q: %v", nodeID, err),
				})
				continue
			}
			linked := false
			if n.ConceptID != "" {
				mems, err := r.ports.Store.ListMemories(ctx, store.MemoryQuery{
					Scope: n.Scope, ConceptID: n.ConceptID, AsOf: in.req.AsOf, Limit: in.perLane,
				})
				if err != nil {
					dropped = append(dropped, DroppedItem{
						ID: nodeID, Reason: fmt.Sprintf("graph: list memories by concept %q: %v", n.ConceptID, err),
					})
				}
				for _, m := range mems {
					ids = append(ids, m.ID)
					ev = append(ev, GraphEvidence{
						Anchor: anchor, EdgeID: h.Edge.ID, From: h.Edge.FromID, To: h.Edge.ToID,
						Relation: h.Edge.Relation, Depth: h.Depth, LinkedBy: "concept_id:" + n.ConceptID,
					})
					linked = true
				}
			}
			if !linked && n.SourceEpisode != "" {
				mems, err := r.ports.Store.ListMemories(ctx, store.MemoryQuery{
					Scope: n.Scope, SourceEpisode: n.SourceEpisode, AsOf: in.req.AsOf, Limit: in.perLane,
				})
				if err != nil {
					dropped = append(dropped, DroppedItem{
						ID: nodeID, Reason: fmt.Sprintf("graph: list memories by episode %q: %v", n.SourceEpisode, err),
					})
				}
				for _, m := range mems {
					ids = append(ids, m.ID)
					ev = append(ev, GraphEvidence{
						Anchor: anchor, EdgeID: h.Edge.ID, From: h.Edge.FromID, To: h.Edge.ToID,
						Relation: h.Edge.Relation, Depth: h.Depth, LinkedBy: "source_episode:" + n.SourceEpisode,
					})
					linked = true
				}
			}
			if !linked && !seen[nodeID] {
				dropped = append(dropped, DroppedItem{
					ID:     nodeID,
					Reason: fmt.Sprintf("graph: 节点 %q（%s）没有 concept_id/source_episode，无法映射到任何记忆", nodeID, n.CanonicalName),
				})
			}
			if linked {
				seen[nodeID] = true
			}
		}
	}
	return ids, ev, dropped
}

// relationScore 是一条图证据在 graph lane 内的排序分：
// 关系权重（表内）× 边权重 × 深度衰减。全部来自声明式表，便于 explain 复述。
func relationScore(plan *RecallPlan, e GraphEvidence) float64 {
	w := 0.5
	for _, rp := range plan.Relations {
		if rp.Relation == e.Relation {
			w = rp.Weight
			break
		}
	}
	edgeW := 1.0 // EdgeHop 的 Edge.Weight 未参与，避免与关系权重重复计数
	depth := e.Depth
	if depth <= 0 {
		depth = 1
	}
	return w * edgeW / float64(depth)
}

func joinStrings(ss []string, sep string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += sep
		}
		out += s
	}
	return out
}
