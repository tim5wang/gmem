package p1verify

import (
	"context"
	"fmt"
	"testing"

	"github.com/tim5wang/gmem/recall"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// 攻击面 2：insufficient 的**双向**正确性
//
// 正向（该报必须报）：库里完全没有的概念维度 → insufficient + Missing[] 非空 + 每项可读 Reason。
// 反向（不该报不能报）：有候选/逐字命中时不得误报 insufficient——这是更危险的反向缺陷，
// 因为"该答的答不出来"看起来像"库里没有"，用户不会发现。
// ===========================================================================

// TestInsufficientForUnknownDimension 正向：问库里完全没有的维度。
//
// 断言口径（比作者测试更严）：
//   - Status=insufficient 且 Items 必须为**空**（R4 的反面就是不返回低分硬答）；
//   - Missing 每项 Reason 必须非空、必须包含缺失文本、Suggestion 必须非空；
//   - 缺口必须覆盖到"部署环境"这个真正缺失的维度，而不是给一条泛泛的整体说明；
//   - Plan 必须解释这一步（R5），且 Plan.ThetaCoverage 记录了阈值。
func TestInsufficientForUnknownDimension(t *testing.T) {
	d := openMemory(t)
	ctx := context.Background()
	seedEditorOntology(t, d)

	// 库里有编辑器维度的记忆，但没有"部署环境"这个维度。
	w := recall.NewWriter(d.Ports())
	if _, err := w.PutMemory(ctx, store.Memory{
		ID: "m-editor", Scope: store.ScopeUser, Kind: store.KindSemantic,
		ConceptID: "c-editor", Content: "Alice 的默认编辑器是 vim", TValid: 1000, TCreated: 1000,
	}); err != nil {
		t.Fatalf("PutMemory: %v", err)
	}

	r := recall.New(d.Ports(), recall.Config{}, nil)
	res, err := r.Recall(ctx, recall.RecallRequest{Query: "生产环境的部署环境 是什么", Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}

	if res.Status != recall.StatusInsufficient {
		t.Fatalf("status=%s, want insufficient（库里没有'部署环境'维度）", res.Status)
	}
	if len(res.Items) != 0 {
		t.Fatalf("insufficient 时不得返回低分硬答：items=%v", itemIDs(res.Items))
	}
	if len(res.Missing) == 0 {
		t.Fatalf("Missing 为空：R4 要求结构化说明缺什么")
	}

	var coveredDeploy bool
	for _, m := range res.Missing {
		if m.Reason == "" {
			t.Fatalf("Missing 项缺 Reason：%+v", m)
		}
		if m.Suggestion == "" {
			t.Fatalf("Missing 项缺 Suggestion（不可执行）：%+v", m)
		}
		if !m.Required {
			t.Fatalf("完全未对齐的维度必须标 Required=true：%+v", m)
		}
		if m.Text == "" {
			t.Fatalf("Missing 项缺 Text：%+v", m)
		}
		// Reason 必须点名该槽位本身（可直接回显给用户）。
		if !containsSubstr([]string{m.Reason}, m.Text) {
			t.Fatalf("Reason 未包含缺失文本 %q：%q", m.Text, m.Reason)
		}
		if m.Text == "部署环境" {
			coveredDeploy = true
		}
	}
	if !coveredDeploy {
		t.Fatalf("Missing 未点名真正缺失的维度「部署环境」：%+v", res.Missing)
	}

	if res.Plan == nil {
		t.Fatalf("insufficient 时 Plan 也必须非空（R5）")
	}
	if !hasStep(res.Plan.Steps, "insufficient") {
		t.Fatalf("Plan.Steps 未记录 insufficient 判定：%+v", res.Plan.Steps)
	}
	if res.Plan.ThetaCoverage <= 0 {
		t.Fatalf("Plan.ThetaCoverage=%.2f，未记录 θ_cov", res.Plan.ThetaCoverage)
	}
	if !hasStepPrefix(res.Plan.Steps, "missing") {
		t.Fatalf("Plan.Steps 未把 Missing 的 Reason 记进 plan：%+v", res.Plan.Steps)
	}
}

// TestNoFalseInsufficientForLiteralHit 反向：库里**逐字**有答案时不得报 insufficient。
//
// 这是本卡明确要求的方向："相关度低但有候选"不得误报。
// 若实现把"本体没有这个概念"直接当成"我没有这个维度"，就会把
// "记忆写好了但本体还没长出来"的库判成完全不可用。
func TestNoFalseInsufficientForLiteralHit(t *testing.T) {
	d := openMemory(t)
	ctx := context.Background()

	// 故意不建任何 concepts/aliases：模拟"本体还没长出来"的库。
	w := recall.NewWriter(d.Ports())
	if _, err := w.PutMemory(ctx, store.Memory{
		ID: "m-auth", Scope: store.ScopeUser, Kind: store.KindDecision,
		Content: "认证服务从 session 换成 JWT", TValid: 1000, TCreated: 1000,
	}); err != nil {
		t.Fatalf("PutMemory: %v", err)
	}

	// 先确认词法通道**确实**有命中（否则本用例没有意义）。
	hits, err := d.Lexical().Search(ctx, store.LexicalQuery{Query: "JWT", Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("Lexical.Search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatalf("前置条件不成立：词法通道对 'JWT' 也没有命中")
	}

	r := recall.New(d.Ports(), recall.Config{}, nil)
	res, err := r.Recall(ctx, recall.RecallRequest{Query: "JWT", Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if res.Status == recall.StatusInsufficient {
		t.Fatalf("误报 insufficient：词法通道有命中 %v，Missing=%+v", hits, res.Missing)
	}
	if len(res.Items) == 0 {
		t.Fatalf("词法有命中却没有返回任何条目（静默漏召回）：plan=%+v", res.Plan)
	}
	if res.Items[0].Memory.ID != "m-auth" {
		t.Fatalf("top1=%q, want m-auth", res.Items[0].Memory.ID)
	}
}

// TestNoFalseInsufficientForPartialMatch 反向：部分维度命中时不得整体判不足。
//
// 构造"有候选但相关度不高"的现实场景：查询里既有库里有的词，也有库里没有的词。
// 只要求"不得整体 return insufficient + 空 Items"，不要求排序质量（那是评测集的事）。
func TestNoFalseInsufficientForPartialMatch(t *testing.T) {
	d := openMemory(t)
	ctx := context.Background()

	w := recall.NewWriter(d.Ports())
	if _, err := w.PutMemory(ctx, store.Memory{
		ID: "m-auth", Scope: store.ScopeUser, Kind: store.KindDecision,
		Content: "认证服务从 session 换成 JWT，因为旧方案无法水平扩展", TValid: 1000, TCreated: 1000,
	}); err != nil {
		t.Fatalf("PutMemory: %v", err)
	}

	r := recall.New(d.Ports(), recall.Config{}, nil)
	// "session" 逐字在库中；"横向扩容" 完全不在库里——一半有、一半没有。
	res, err := r.Recall(ctx, recall.RecallRequest{Query: "session 横向扩容", Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(res.Items) == 0 {
		t.Fatalf("查询里有一半维度在库中（session 逐字命中）却 0 条目：status=%s missing=%+v plan=%+v",
			res.Status, res.Missing, res.Plan.Steps)
	}
}

// ---------------------------------------------------------------------------
// BLOCKER 1：关系型/部分覆盖形态的计划里没有 lexical 通道
// ---------------------------------------------------------------------------
//
// 缺陷：ShapeRelational 的 lane 表是 [graph 0.7, vector 0.3]（见 recall/config.go），
// 没有 lexical。当 graph 因**没有实体锚点**被跳过、vector 因**没有 embedder**被跳过时，
// 融合阶段只剩 0 路通道，于是"所有通道 0 命中 → insufficient"，Items 为空。
//
// 但同一个 query 直接调 Lexical.Search 是有命中的（见下面最小用例）。
// 也就是说：**词法通道明明能答，却因为没被计划进来而答不出**，且用户看到的是
// "库里没有这个维度"——这是最容易被误当成数据问题的一类静默失败。
//
// 最小复现：库里只有一条记忆 "认证服务从 session 换成 JWT"，
// 查询 "为什么换成 JWT"（时态关系线索 caused_by + 字面量 JWT），
// alignment coverage=0.667 ≥ θ=0.6（不是覆盖率不足），RequiredMissing=0，
// 但最终 status=insufficient、items=0、plan 里写着"融合 0 路通道"。
func TestBlockerRelationalPlanOmitsLexical(t *testing.T) {
	d := openMemory(t)
	ctx := context.Background()
	// 本体存在但与"认证"无关（只要非空，coverage 就是权威的）。
	putConcept(t, d.Store(), "c-editor", "编辑器", store.ConceptCertified, "用于编写代码的文本编辑程序")

	w := recall.NewWriter(d.Ports())
	if _, err := w.PutMemory(ctx, store.Memory{
		ID: "m-auth", Scope: store.ScopeUser, Kind: store.KindDecision,
		Content: "认证服务从 session 换成 JWT", TValid: 1000, TCreated: 1000,
	}); err != nil {
		t.Fatalf("PutMemory: %v", err)
	}

	const query = "为什么换成 JWT"
	// 前置证据：词法通道对本 query 有命中。
	hits, err := d.Lexical().Search(ctx, store.LexicalQuery{Query: query, Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("Lexical.Search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatalf("前置条件不成立：词法通道对 %q 无命中", query)
	}

	r := recall.New(d.Ports(), recall.Config{}, nil)
	res, err := r.Recall(ctx, recall.RecallRequest{Query: query, Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}

	// 现状（缺陷）：plan 里没有 lexical；结果为空。
	pin(t, pinBlocker, "B1", "关系型计划的 lane 表不含 lexical，导致'词法能答却答不出'",
		func() string {
			var hasLexical bool
			for _, lp := range res.Plan.Lanes {
				if lp.Lane == recall.LaneLexical {
					hasLexical = true
				}
			}
			if hasLexical {
				return fmt.Sprintf("Shape=%s 的 lane 表已包含 lexical（缺陷已改变）：%+v", res.Plan.Shape, res.Plan.Lanes)
			}
			if !containsSubstr(droppedReasons(res.Plan), "融合 0 路通道") &&
				!hasStepDetailContains(res.Plan.Steps, "fuse", "融合 0 路通道") {
				return "预期 plan 记录'融合 0 路通道'，实际未记录（缺陷形态已变）"
			}
			if len(res.Items) != 0 || res.Status != recall.StatusInsufficient {
				return fmt.Sprintf("预期 items=0/insufficient，实际 items=%v status=%s（缺陷形态已变）",
					itemIDs(res.Items), res.Status)
			}
			return ""
		},
		func() string {
			if len(res.Items) == 0 {
				return fmt.Sprintf("词法通道有 %d 条命中，但 Recall 仍返回 0 条（status=%s，lanes=%+v）",
					len(hits), res.Status, res.Plan.Lanes)
			}
			if res.Status == recall.StatusInsufficient {
				return fmt.Sprintf("有结果却仍标 insufficient：missing=%+v", res.Missing)
			}
			return ""
		})
}

// TestBlockerMaskedCueLosesRelation 是 BLOCKER 2：概念名贪心匹配**遮挡**关系线索词。
//
// 缺陷：align.go 先按"最长优先"接受 aliases/concepts.name，再处理关系线索词，
// 且线索候选只要与已接受槽位**有任何重叠**就整条丢弃。
// 于是 "Alice 在哪个公司工作" 里，概念名「公司」先吃掉 "公司"，
// 线索短语 "哪个公司" 因重叠被丢 → RelationHints 为空 → 形态退化为
// full_with_anchor → 走 shape-default 的 3 跳白名单（part_of/subtype_of/depends_on，
// **不含 works_at**）。
//
// 最小复现的关键：让这条记忆在词法上**打不中**问句（内容设为 "Acme 雇佣"），
// 此时 graph lane 是唯一能答的通道，而它正因为线索被遮挡而走错关系类型。
// 对照：换一个不与概念名重叠的问法（"Alice 在哪工作"），works_at 线索能被识别，
// 1 跳就答得出来。两个问法语义相同，结果一个答得出一个答不出。
func TestBlockerMaskedCueLosesRelation(t *testing.T) {
	d := openMemory(t)
	ctx := context.Background()
	putConcept(t, d.Store(), "c-company", "公司", store.ConceptCertified, "雇佣员工的组织实体")
	putNode(t, d.Graph(), "n-alice", "Alice", store.ScopeUser, "", "")
	putNode(t, d.Graph(), "n-acme", "Acme", store.ScopeUser, "c-company", "")
	putEdge(t, d.Graph(), "e-w", "n-alice", "n-acme", store.RelationWorksAt, 1000, 1000)
	putAlias(t, d.Store(), "Alice", "n-alice", 1.0)

	w := recall.NewWriter(d.Ports())
	// 关键：内容与两个问句都没有 bigram 重合 → 词法通道命中不了，
	// 唯一可能答对的通道是 graph lane。
	if _, err := w.PutMemory(ctx, store.Memory{
		ID: "m-acme", Scope: store.ScopeUser, Kind: store.KindSemantic, ConceptID: "c-company",
		Content: "Acme 雇佣", TValid: 1000, TCreated: 1000,
	}); err != nil {
		t.Fatalf("PutMemory: %v", err)
	}

	r := recall.New(d.Ports(), recall.Config{}, nil)
	const masked = "Alice 在哪个公司工作"
	const plain = "Alice 在哪工作"

	resMasked, err := r.Recall(ctx, recall.RecallRequest{Query: masked, Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("Recall(%q): %v", masked, err)
	}
	resPlain, err := r.Recall(ctx, recall.RecallRequest{Query: plain, Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("Recall(%q): %v", plain, err)
	}

	pin(t, pinBlocker, "B2", "概念名遮挡关系线索词，同义问法从 1 跳退化为 3 跳盲扩展且答不出",
		func() string {
			// 对照组必须成立：否则不能把差异归因于线索遮挡。
			rp, ok := relationPlanOf(resPlain.Plan, store.RelationWorksAt)
			if !ok || rp.MaxHops != 1 || rp.Origin != "query-cue" {
				return fmt.Sprintf("对照组失效：%q 未按 query-cue 走 works_at 1 跳：%+v",
					plain, resPlain.Plan.Relations)
			}
			if !containsID(itemIDs(resPlain.Items), "m-acme") {
				return fmt.Sprintf("对照组失效：%q 也召回不到 m-acme", plain)
			}
			// 缺陷现状：遮挡后 relation hints 丢失、plan 里没有 works_at、形态退化、答不出。
			if _, ok := relationPlanOf(resMasked.Plan, store.RelationWorksAt); ok {
				return fmt.Sprintf("遮挡现象已消失：%q 的 plan 已包含 works_at：%+v",
					masked, resMasked.Plan.Relations)
			}
			if resMasked.Plan.Shape != recall.ShapeFullWithAnchor {
				return fmt.Sprintf("预期形态退化为 full_with_anchor，实际 %s", resMasked.Plan.Shape)
			}
			if resMasked.Plan.Hops != 3 {
				return fmt.Sprintf("预期退化到 3 跳，实际 %d", resMasked.Plan.Hops)
			}
			if len(resMasked.Items) != 0 {
				return fmt.Sprintf("预期无结果，实际 %v", itemIDs(resMasked.Items))
			}
			return ""
		},
		func() string {
			rp, ok := relationPlanOf(resMasked.Plan, store.RelationWorksAt)
			if !ok {
				return fmt.Sprintf("%q 的 plan 未包含 works_at：%+v", masked, resMasked.Plan.Relations)
			}
			if rp.MaxHops != 1 {

				return fmt.Sprintf("works_at 跳数=%d, want 1", rp.MaxHops)
			}
			if !containsID(itemIDs(resMasked.Items), "m-acme") {
				return fmt.Sprintf("%q 未召回 m-acme：items=%v", masked, itemIDs(resMasked.Items))
			}
			return ""
		})
}

// ---------------------------------------------------------------------------
// 可后续
// ---------------------------------------------------------------------------

// TestFollowUpInsufficientClaimsLaneExecuted 记录一个**Explain 保真度**缺口（可后续）：
//
// R4 的判定发生在任何 lane 执行之前（recall.go 第 101 行直接 return），
// 但 Plan.Lanes 里的 Executed 是**计划期**就置位的标志（plan.go 末尾）。
// 于是 insufficient 的结果里，plan 声称 "lexical: … 执行"，
// 而实际一次通道调用都没发生（Plan.Steps 里没有 fuse 步骤，也没有任何命中数）。
//
// 危害：读 plan 的人会得出"词法跑过了、什么都没找到"的结论，
// 而事实上词法对同一个 query 是有命中的（下面用独立 Search 证明）。
// 这正是 B1（关系型计划里没有 lexical）能被长期掩盖的原因：
// 降级与短路共享同一套"看起来执行过"的 plan 字段。
func TestFollowUpInsufficientClaimsLaneExecuted(t *testing.T) {
	d := openMemory(t)
	ctx := context.Background()
	putConcept(t, d.Store(), "c-editor", "编辑器", store.ConceptCertified, "用于编写代码的文本编辑程序")

	w := recall.NewWriter(d.Ports())
	if _, err := w.PutMemory(ctx, store.Memory{
		ID: "m-auth", Scope: store.ScopeUser, Kind: store.KindDecision,
		Content: "认证服务从 session 换成 JWT", TValid: 1000, TCreated: 1000,
	}); err != nil {
		t.Fatalf("PutMemory: %v", err)
	}

	const query = "认证"
	r := recall.New(d.Ports(), recall.Config{}, nil)
	// R4 短路：本体里没有该概念，coverage=0 → 直接 insufficient。
	res, err := r.Recall(ctx, recall.RecallRequest{Query: query, Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	// 独立的、不经过 Recall 的词法检索：证明"词法其实有命中"。
	hits, err := d.Lexical().Search(ctx, store.LexicalQuery{Query: query, Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("Lexical.Search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatalf("前置条件不成立：词法对 %q 无命中", query)
	}

	pin(t, pinFollowUp, "F1", "insufficient 短路时 plan 仍声称 lane 已执行，且无任何通道命中信息",
		func() string {
			if res.Status != recall.StatusInsufficient {
				return fmt.Sprintf("预期 insufficient，实际 %s", res.Status)
			}
			lp, ok := laneOf(res.Plan, recall.LaneLexical)
			if !ok {
				return "Plan.Lanes 里没有 lexical：缺陷形态已变"
			}
			if !lp.Executed {
				return "lexical 的 Executed 已是 false（缺陷已修或形态已变）"
			}
			if hasStep(res.Plan.Steps, "fuse") {
				return "预期短路时没有 fuse 步骤，实际存在（缺陷形态已变）"
			}
			return ""
		},
		func() string {
			// 修复方向（二者其一即算修好）：
			//   a) 短路时不得把未执行的 lane 标为 Executed；
			//   b) 短路前也跑一次轻量探测，把"词法本可命中 N 条"写进 plan。
			lp, _ := laneOf(res.Plan, recall.LaneLexical)
			if !lp.Executed {
				return ""
			}
			if hasStep(res.Plan.Steps, "fuse") || hasStepDetailContains(res.Plan.Steps, "insufficient", "命中") {
				return ""
			}
			return fmt.Sprintf("plan 仍声称 lexical 已执行，但没有 fuse 步骤/命中数（词法实际有 %d 条命中）：steps=%+v",
				len(hits), res.Plan.Steps)
		})
}

// hasStepDetailContains 判断某个 step 的 Detail 是否包含子串。
func hasStepDetailContains(steps []recall.PlanStep, name, sub string) bool {
	d, ok := stepDetail(steps, name)
	return ok && containsSubstr([]string{d}, sub)
}

// containsID 判断 ID 列表里是否包含 want。
func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
