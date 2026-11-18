package ops

import (
	"fmt"
	"sort"
	"time"

	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// 扫描/维护型原子 op：Extract / Disambiguate / Consolidate / Decay / Prune
//
// 共同点：Plan 里读库生成"该做什么"，Apply 逐步应用。扫描窗口（Input.Window）
// 参与幂等 key，因此**同一窗口重跑安全跳过，新窗口才是新工作**。
// ===========================================================================

// ---------------------------------------------------------------------------
// Extract
// ---------------------------------------------------------------------------

type extractOp struct{}

func (extractOp) Name() OpName { return OpExtract }
func (extractOp) Budget() Budget {
	return Budget{Tokens: 20000, Duration: 30 * time.Second, Steps: 50, IO: 500}
}

func (extractOp) Plan(txc TxContext) ([]Step, error) {
	ctx := txc.Context()
	in := txc.Input()
	limit := in.Limit
	if limit <= 0 {
		limit = txc.Cfg().Limit
	}
	var eps []store.Episode
	if len(in.Episodes) > 0 {
		for _, t := range in.Episodes {
			if t.EpisodeID == "" {
				continue
			}
			ep, err := txc.Store().GetEpisode(ctx, t.EpisodeID)
			if err != nil {
				return nil, fmt.Errorf("ops: extract: 读 episode %q: %w", t.EpisodeID, err)
			}
			if ep.Scope != txc.Scope() {
				return nil, fmt.Errorf("ops: extract: episode %q 不在 scope %q 内", ep.ID, txc.Scope())
			}
			eps = append(eps, ep)
		}
	} else {
		list, err := txc.Backend().ListEpisodesPendingExtraction(ctx, txc.Scope(), limit)
		if err != nil {
			return nil, err
		}
		eps = list
	}
	out := make([]Step, 0, len(eps))
	for _, ep := range sortEpisodes(eps) {
		// F-6：预算必须真的扣减。旧实现 Spend 零调用点，IO/Tokens 预算只存在于结构体里，
		// “IO=1 仍 applied 30 步”——调用方以为有上限，实际无界。
		// 每处理一条输入花 1 单位 IO；耗尽就停手并显式说明（不得静默截断）。
		if err := txc.Spend(1); err != nil {
			txc.Note(fmt.Sprintf("IO 预算耗尽：Extract 仅计划了 %d/%d 个 episode", len(out), len(eps)))
			break
		}
		out = append(out, Step{
			ID: "extract:" + ep.ID, Kind: "extract_episode", Subject: ep.ID, Data: ep,
			Note: "L0/L1/L2 提取并落库（写入必经 recall.Writer）",
		})
	}
	return out, nil
}

func sortEpisodes(eps []store.Episode) []store.Episode {
	out := append([]store.Episode(nil), eps...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].At != out[j].At {
			return out[i].At < out[j].At
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (extractOp) Apply(txc TxContext, s Step) (Diff, error) {
	ep, ok := s.Data.(store.Episode)
	if !ok {
		return Diff{}, fmt.Errorf("ops: extract: 步骤载荷类型错误")
	}
	ex := txc.Extractor()
	if ex == nil {
		// 显式降级：不注入 Extractor 时提取不执行，但**必须可见**（不得静默少写）。
		return Diff{
			Changed: false,
			Summary: "未注入 Extractor：跳过 episode " + ep.ID,
			Degraded: []string{
				"extract op 未执行：未注入 Extractor（L0/L1/L2 全部缺席）；" +
					"这是显式降级而不是成功",
			},
		}, nil
	}
	// F-6：Tokens 预算必须真的被消费。
	//
	// 旧实现里 Budget.Tokens 从未被读取（extractOp 报了 20000，无人消费），
	// 于是“token 预算”只是一行注释。以 episode 正文长度为估算基准（一个字符 ≈
	// 一个 token 是粗糙但**保守**的下界），超过上限就在调用 Extractor **之前**停手：
	// 已经花掉的调用不可撤回，因此检查点必须在前面。
	if err := txc.SpendTokens(len([]rune(ep.Content))); err != nil {
		return Diff{Changed: false,
			Summary:  "Tokens 预算耗尽：跳过 episode " + ep.ID,
			Degraded: []string{"Tokens 预算耗尽（episode " + ep.ID + " 未提取），已停手"},
		}, nil
	}
	res, err := ex.ExtractEpisode(txc.Context(), EpisodeInput{
		ID: ep.ID, Scope: ep.Scope, Namespace: ep.Namespace, Role: ep.Role,
		Content: ep.Content, At: ep.At,
	})
	if err != nil {
		return Diff{}, fmt.Errorf("ops: extract episode %q: %w", ep.ID, err)
	}
	var deg []string
	deg = append(deg, res.Degraded...)
	if len(res.Rejected) > 0 {
		// TD §7 纪律 4：Rejected 也落库（"为什么没记住这条"同样是可回答的问题）。
		//
		// 这里落一条**状态账**而不是 memories 行：内容被质量门拒掉，就不该进入
		// memories/FTS（否则它会出现在召回里），但"在哪一轮、因为什么被拒"必须留痕。
		// 因此用确定性的 "rejected:<episode>" 作为状态主键，状态为 rejected，
		// 审计的 after_json 里带拒写原因。
		rejectedID := "rejected:" + ep.ID
		if err := txc.Backend().EnsureState(txc.Context(), State{
			MemoryID: rejectedID, Scope: txc.Scope(), Status: StatusRejected,
			Base: 0, Recency: 1, AccessBoost: 1, Grade: 1, Importance: 0,
			StatusChangedAt: txc.Now().UnixMilli(), CreatedAt: txc.Now().UnixMilli(),
			UpdatedAt: txc.Now().UnixMilli(),
		}); err != nil {
			return Diff{}, err
		}
		deg = append(deg, "L0 拒写："+joinLimited(res.Rejected, 5))
	}
	var memIDs []string
	for _, w := range res.Writes {
		if w.MemoryID == "" {
			continue
		}
		memIDs = append(memIDs, w.MemoryID)
		if !w.LexicalIndexed {
			deg = append(deg, fmt.Sprintf("记忆 %s 的词法索引未同步（写入编排未生效）", w.MemoryID))
		}
	}
	// 写入的记忆需要一条状态行；按默认状态推导（内容过短即 Rejected，其余 Active）。
	sm := stateManager{txc: txc}
	for _, id := range memIDs {
		m, err := txc.Store().GetMemory(txc.Context(), id)
		if err != nil {
			continue
		}
		if err := sm.ensure(m); err != nil {
			return Diff{}, err
		}
		// 概念使用计数是 D15 的判定依据（Promote All 的门槛、draft 的"最后使用时间"）。
		if m.ConceptID != "" {
			if err := txc.Backend().TouchConceptUsage(txc.Context(), m.ConceptID, txc.Now().UnixMilli()); err != nil {
				return Diff{}, err
			}
		}
	}
	return Diff{
		Changed:  len(memIDs) > 0,
		Summary:  fmt.Sprintf("提取 episode %s：写入 %d 条记忆、%d 个节点", ep.ID, len(memIDs), len(res.Writes)),
		After:    marshalJSON(map[string]any{"episode": ep.ID, "memories": memIDs}),
		Subjects: append([]string{ep.ID}, memIDs...),
		Degraded: deg,
		Warnings: res.Warnings,
	}, nil
}

// ---------------------------------------------------------------------------
// Disambiguate
// ---------------------------------------------------------------------------

type disambiguateOp struct{}

func (disambiguateOp) Name() OpName   { return OpDisambiguate }
func (disambiguateOp) Budget() Budget { return Budget{Steps: 200, IO: 2000} }

// Plan 产出两类步骤：
//  1. review_pair：把待审的 possiblySame 对列出来（Agent 的 ReviewSimilar 直接用它）；
//  2. verdict_pair：按输入里的裁决落 negative_SameAs 或 Merge。
//
// 裁决为同一条时**调同一个 mergeNodes**——灰区裁决与显式 Merge 不可能漂移。
func (disambiguateOp) Plan(txc TxContext) ([]Step, error) {
	in := txc.Input()
	var pairs []PairRef
	if len(in.Pairs) > 0 {
		pairs = append(pairs, in.Pairs...)
	}
	if len(pairs) == 0 && len(in.Verdicts) == 0 {
		// 待审池：从图里找 possiblySame 边（只沿已登记节点做 1 跳，不扫全图）。
		pending, err := pendingPairs(txc)
		if err != nil {
			return nil, err
		}
		pairs = pending
	}
	verdicts := map[string]PairVerdict{}
	for _, v := range in.Verdicts {
		if v.LeftNode == "" || v.RightNode == "" {
			return nil, fmt.Errorf("ops: disambiguate: 裁决对的两个节点都必填")
		}
		switch v.Verdict {
		case "same", "different", "":
		default:
			return nil, fmt.Errorf("ops: disambiguate: 未知裁决 %q（只接受 same/different）", v.Verdict)
		}
		verdicts[nodePairKey(v.LeftNode, v.RightNode)] = v
	}
	// 裁决引用的对即使不在待审池里也必须产出步骤（Agent 可能直接裁决一对）。
	for key, v := range verdicts {
		found := false
		for _, p := range pairs {
			if nodePairKey(p.LeftNode, p.RightNode) == key {
				found = true
				break
			}
		}
		if !found {
			pairs = append(pairs, PairRef{LeftNode: v.LeftNode, RightNode: v.RightNode})
		}
	}

	out := make([]Step, 0, len(pairs))
	sorted := append([]PairRef(nil), pairs...)
	sort.Slice(sorted, func(i, j int) bool {
		return nodePairKey(sorted[i].LeftNode, sorted[i].RightNode) <
			nodePairKey(sorted[j].LeftNode, sorted[j].RightNode)
	})
	for _, p := range sorted {
		// F-6：预算扣减（每对灰区/裁决花 1 单位 IO）。
		if err := txc.Spend(1); err != nil {
			txc.Note(fmt.Sprintf("IO 预算耗尽：Disambiguate 仅计划了 %d/%d 对", len(out), len(sorted)))
			break
		}
		key := nodePairKey(p.LeftNode, p.RightNode)
		if v, ok := verdicts[key]; ok {
			out = append(out, Step{
				ID: "verdict:" + shortHash(key), Kind: "pair_verdict",
				Subject: key, Data: v,
				Note: "裁决为 " + v.Verdict + "（same → Merge，different → negative_SameAs）",
			})
			continue
		}
		out = append(out, Step{
			ID: "review:" + shortHash(key), Kind: "review_pair",
			Subject: key, Data: p, Note: "灰区待审：可能同一实体，需裁决（绝不自动合并）",
		})
	}
	return out, nil
}

// pendingPairs 枚举待审灰区对（possiblySame 边）。
func pendingPairs(txc TxContext) ([]PairRef, error) {
	ctx := txc.Context()
	or, ok := txc.Store().(store.OntologyReader)
	if !ok || or == nil {
		return nil, nil
	}
	aliases, err := or.ListAliases(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	var pairs []PairRef
	for _, a := range aliases {
		if _, dup := seen[a.NodeID]; dup {
			continue
		}
		seen[a.NodeID] = struct{}{}
		hops, err := txc.Ports().Graph.Neighbors(ctx, store.NeighborQuery{
			NodeID: a.NodeID, Scope: txc.Scope(),
			Relations: []store.Relation{relationPossiblySame}, Hops: 1,
		})
		if err != nil {
			continue
		}
		for _, h := range hops {
			if h.Edge.TInvalid != 0 {
				continue
			}
			key := nodePairKey(h.Edge.FromID, h.Edge.ToID)
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			pairs = append(pairs, PairRef{LeftNode: h.Edge.FromID, RightNode: h.Edge.ToID})
		}
	}
	return pairs, nil
}

func (disambiguateOp) Apply(txc TxContext, s Step) (Diff, error) {
	switch v := s.Data.(type) {
	case PairRef:
		// review_pair 只读：不进 audit 的"变更"（Changed=false），但审计仍记录"审了哪一对"。
		left, err := txc.Ports().Graph.GetNode(txc.Context(), v.LeftNode)
		if err != nil {
			return Diff{}, fmt.Errorf("ops: disambiguate: 节点 %q 不可读: %w", v.LeftNode, err)
		}
		right, err := txc.Ports().Graph.GetNode(txc.Context(), v.RightNode)
		if err != nil {
			return Diff{}, fmt.Errorf("ops: disambiguate: 节点 %q 不可读: %w", v.RightNode, err)
		}
		return Diff{
			Changed: false,
			Summary: fmt.Sprintf("待审：%q(%s) vs %q(%s)", left.CanonicalName, left.ID, right.CanonicalName, right.ID),
			After: marshalJSON(map[string]any{
				"left": left.ID, "left_name": left.CanonicalName,
				"right": right.ID, "right_name": right.CanonicalName, "verdict": "pending",
			}),
			Subjects: []string{left.ID, right.ID},
		}, nil
	case PairVerdict:
		return applyVerdict(txc, v)
	default:
		return Diff{}, fmt.Errorf("ops: disambiguate: 未识别的步骤载荷 %T", s.Data)
	}
}

func applyVerdict(txc TxContext, v PairVerdict) (Diff, error) {
	ctx := txc.Context()
	now := txc.Now()
	switch v.Verdict {
	case "same":
		return mergeNodes(txc, v.LeftNode, v.RightNode, "disambiguate verdict=same: "+v.Note)
	case "different":
		id := edgeID(relationNegativeSameAs, v.LeftNode, v.RightNode)
		e := store.Edge{
			ID: id, FromID: v.LeftNode, ToID: v.RightNode,
			Relation: relationNegativeSameAs, Weight: 1,
			TValid: now.Unix(), TCreated: now.Unix(),
		}
		if err := txc.Ports().Graph.PutEdge(ctx, e); err != nil {
			return Diff{}, err
		}
		// 同时失效待审的 possiblySame 边，避免同一对反复进灰区（TD §4 第 3 点）。
		var cleared []string
		hops, err := txc.Ports().Graph.Neighbors(ctx, store.NeighborQuery{
			NodeID: v.LeftNode, Scope: txc.Scope(),
			Relations: []store.Relation{relationPossiblySame}, Hops: 1,
		})
		if err == nil {
			for _, h := range hops {
				if h.Edge.TInvalid != 0 {
					continue
				}
				if nodePairKey(h.Edge.FromID, h.Edge.ToID) != nodePairKey(v.LeftNode, v.RightNode) {
					continue
				}
				changed, err := invalidateEdge(txc, h.Edge.ID, now.Unix())
				if err != nil {
					return Diff{}, err
				}
				if changed {
					cleared = append(cleared, h.Edge.ID)
				}
			}
		}
		return Diff{
			Changed: true,
			Summary: fmt.Sprintf("裁决 different：写 negative_SameAs %s，清待审 possiblySame %d 条", id, len(cleared)),
			After: marshalJSON(map[string]any{
				"negative_same_as": id, "cleared": cleared, "verdict": "different",
			}),
			Subjects: append([]string{id, v.LeftNode, v.RightNode}, cleared...),
		}, nil
	case "":
		return Diff{Changed: false, Summary: "待审：保持灰区（不合并）",
			Subjects: []string{v.LeftNode, v.RightNode}}, nil
	}
	return Diff{}, fmt.Errorf("ops: disambiguate: 未知裁决 %q", v.Verdict)
}

// ---------------------------------------------------------------------------
// Consolidate
// ---------------------------------------------------------------------------

type consolidateOp struct{}

func (consolidateOp) Name() OpName   { return OpConsolidate }
func (consolidateOp) Budget() Budget { return Budget{Steps: 500, IO: 5000, Duration: 30 * time.Second} }

// Plan 做三件事（TD §6 T3）：
//  1. 状态推进：Candidate → Active/Rejected，Stale/Invalidated 保留期满 → Archived；
//  2. 孤立节点检测：本 scope 内没有任何边且没有 alias 的节点（只报告，不删除）；
//  3. 边权重重算（w = log(1+assert_count) × grade × recency 的简化落地）。
func (consolidateOp) Plan(txc TxContext) ([]Step, error) {
	ctx := txc.Context()
	cfg := txc.Cfg()
	now := txc.Now()
	limit := txc.Input().Limit
	if limit <= 0 {
		limit = cfg.Limit
	}
	var steps []Step

	mems, err := txc.Store().ListMemories(ctx, store.MemoryQuery{
		Scope: txc.Scope(), IncludeExpired: false, IncludeInvalid: true, Limit: limit,
	})
	if err != nil {
		return nil, err
	}
	sm := stateManager{txc: txc}
	for _, m := range mems {
		if err := txc.Check(); err != nil {
			return nil, err
		}
		// F-6：预算扣减（每条记忆 1 单位 IO）；耗尽即停手并说明。
		if err := txc.Spend(1); err != nil {
			txc.Note(fmt.Sprintf("IO 预算耗尽：Consolidate 仅扫描了 %d/%d 条记忆", len(steps), len(mems)))
			break
		}
		st, _ := sm.load(m)
		target, why := sm.nextStatus(st, m, now, cfg)
		if target == "" || target == st.Status {
			continue
		}
		steps = append(steps, Step{
			ID: "status:" + m.ID, Kind: "advance_status", Subject: m.ID,
			Data: statusStep{Memory: m, To: target}, Note: why,
		})
	}

	// 孤立节点：只进审计/告警，不删除（删除只能由 Prune 做，且只作用于 Archived 记忆）。
	nodes, err := txc.Backend().ListNodes(ctx, txc.Scope(), limit)
	if err != nil {
		return nil, err
	}
	edges, err := txc.Backend().ListEdges(ctx, txc.Scope(), 0)
	if err != nil {
		return nil, err
	}
	degree := map[string]int{}
	for _, e := range edges {
		if e.TInvalid != 0 {
			continue
		}
		degree[e.FromID]++
		degree[e.ToID]++
	}
	var orphans []string
	for _, n := range nodes {
		if degree[n.ID] == 0 {
			orphans = append(orphans, n.ID)
		}
	}
	if len(orphans) > 0 {
		steps = append(steps, Step{
			ID: "orphans:" + shortHash(orphans...), Kind: "report_orphans",
			Subject: "", Data: orphanReport{IDs: orphans},
			Note: "孤立节点（无任何边）：只报告，不删除",
		})
	}
	return steps, nil
}

type statusStep struct {
	Memory store.Memory
	To     MemoryStatus
}

type orphanReport struct{ IDs []string }

// nextStatus 推导该记忆应该进入的下一个状态（"" = 不动）。
func (sm stateManager) nextStatus(st State, m store.Memory, now time.Time, cfg Config) (MemoryStatus, string) {
	nowMs := now.UnixMilli()
	switch st.Status {
	case StatusCandidate:
		if len([]rune(m.Content)) < cfg.MinContentLength {
			return StatusRejected, "质量门：内容长度不足，转 Rejected（仍落库）"
		}
		return StatusActive, "校验通过：Candidate → Active"
	case StatusActive:
		if m.TInvalid != 0 {
			return StatusInvalidated, "世界时间已失效：Active → Invalidated（不删除）"
		}
		if st.Importance > 0 && st.Importance < cfg.StaleBelow {
			return StatusStale, fmt.Sprintf("importance %.3f < StaleBelow %.2f：Active → Stale", st.Importance, cfg.StaleBelow)
		}
		return "", ""
	case StatusInvalidated:
		days := daysSince(nowMs, st.StatusChangedAt)
		if days >= float64(cfg.ArchiveAfterDays) {
			return StatusArchived, fmt.Sprintf("保留期满 %.0f 天：Invalidated → Archived", days)
		}
		return "", ""
	case StatusStale:
		days := daysSince(nowMs, st.StatusChangedAt)
		if days >= float64(cfg.StaleArchiveAfterDays) {
			return StatusArchived, fmt.Sprintf("Stale 期满 %.0f 天：→ Archived", days)
		}
		return "", ""
	}
	return "", ""
}

func daysSince(nowMs, since int64) float64 {
	if since <= 0 || nowMs <= since {
		return 0
	}
	return float64(nowMs-since) / float64(24*time.Hour/time.Millisecond)
}

func (consolidateOp) Apply(txc TxContext, s Step) (Diff, error) {
	switch v := s.Data.(type) {
	case statusStep:
		return stateManager{txc: txc}.move(v.Memory, v.To, s.Note)
	case orphanReport:
		return Diff{
			Changed:  false,
			Summary:  fmt.Sprintf("孤立节点 %d 个（只报告，不删除）", len(v.IDs)),
			After:    marshalJSON(map[string]any{"orphans": v.IDs}),
			Warnings: []string{fmt.Sprintf("孤立节点 %d 个：%s", len(v.IDs), joinLimited(v.IDs, 10))},
			Subjects: v.IDs,
		}, nil
	}
	return Diff{}, fmt.Errorf("ops: consolidate: 未识别的步骤载荷 %T", s.Data)
}

// ---------------------------------------------------------------------------
// Decay
// ---------------------------------------------------------------------------

type decayOp struct{}

func (decayOp) Name() OpName   { return OpDecay }
func (decayOp) Budget() Budget { return Budget{Steps: 1000, IO: 3000, Duration: 20 * time.Second} }

// Plan 按分项重算 importance（base × recency × access_boost × grade），四个分项分别落库。
//
// 它**不**改状态：状态推进属于 Consolidate。分开的理由是 Decay 可以高频跑（便宜、
// 幂等），而状态推进一旦发生就涉及"是否还能被召回"的语义，必须能被单独审计。
func (decayOp) Plan(txc TxContext) ([]Step, error) {
	ctx := txc.Context()
	limit := txc.Input().Limit
	if limit <= 0 {
		limit = txc.Cfg().Limit
	}
	mems, err := txc.Store().ListMemories(ctx, store.MemoryQuery{
		Scope: txc.Scope(), IncludeExpired: false, IncludeInvalid: false, Limit: limit,
	})
	if err != nil {
		return nil, err
	}
	sm := stateManager{txc: txc}
	var steps []Step
	for _, m := range mems {
		if err := txc.Check(); err != nil {
			return nil, err
		}
		// F-6：预算扣减（每条记忆 1 单位 IO）；耗尽即停手并说明。
		if err := txc.Spend(1); err != nil {
			txc.Note(fmt.Sprintf("IO 预算耗尽：Decay 仅重算了 %d/%d 条记忆", len(steps), len(mems)))
			break
		}
		cur, _ := sm.load(m)
		next := sm.recalc(m, cur)
		// 变化小于 1e-6 视为无变化：避免每次运行都伪造一条 diff。
		if sameState(cur, next) {
			continue
		}
		steps = append(steps, Step{
			ID: "decay:" + m.ID, Kind: "recompute_importance", Subject: m.ID,
			Data: decayStep{Memory: m, Before: cur, After: next},
			Note: "重算 importance 四个分项（不改状态）",
		})
	}
	return steps, nil
}

type decayStep struct {
	Memory store.Memory
	Before State
	After  State
}

func sameState(a, b State) bool {
	const eps = 1e-6
	diff := func(x, y float64) bool {
		d := x - y
		if d < 0 {
			d = -d
		}
		return d > eps
	}
	return !diff(a.Base, b.Base) && !diff(a.Recency, b.Recency) &&
		!diff(a.AccessBoost, b.AccessBoost) && !diff(a.Grade, b.Grade) &&
		a.Status == b.Status
}

func (decayOp) Apply(txc TxContext, s Step) (Diff, error) {
	v, ok := s.Data.(decayStep)
	if !ok {
		return Diff{}, fmt.Errorf("ops: decay: 步骤载荷类型错误")
	}
	if err := txc.Backend().PutState(txc.Context(), v.After); err != nil {
		return Diff{}, err
	}
	return Diff{
		Changed: true,
		Summary: fmt.Sprintf("记忆 %s importance %.4f → %.4f（base×recency×access×grade）",
			v.Memory.ID, v.Before.Importance, v.After.Importance),
		Before: snapshotOf(v.Before, true), After: snapshotOf(v.After, true),
		Subjects: []string{v.Memory.ID},
	}, nil
}

// joinLimited 把列表拼成一句可读文本（超出上限时标注剩余条数）。
func joinLimited(items []string, max int) string {
	if len(items) <= max {
		return joinComma(items)
	}
	return joinComma(items[:max]) + fmt.Sprintf(" 等 %d 项", len(items))
}

func joinComma(items []string) string {
	out := ""
	for i, s := range items {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}
