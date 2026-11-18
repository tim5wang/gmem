package p2verify

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/tim5wang/gmem/extract"
	"github.com/tim5wang/gmem/sqlite"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// 发现清单（阻塞合并 / 可后续）的可复现用例
//
// 约定沿用 internal/vtest：**已知缺陷不得让 `go test ./...` 永久变红**
// （那会掩盖新回归）。因此阻塞项挂在 GMEM_VERIFY_BLOCKER=1 之后：
//   - 未置位：用例仍会**实测**并把事实打印出来（不让缺陷被静默遗忘）；
//   - 置位：用例以红色失败，缺陷修复后应转为绿（那时就该摘掉门禁）。
//
// 可后续项同理，用 GMEM_VERIFY_DEFERRED=1 控制是否强制失败。
// ===========================================================================

func blockerGate() bool  { return os.Getenv("GMEM_VERIFY_BLOCKER") == "1" }
func deferredGate() bool { return os.Getenv("GMEM_VERIFY_DEFERRED") == "1" }

// TestBlockerAutoMergeEdgeFK 是【阻塞合并】#1 的最小可复现用例。
//
// # 现象
//
// "自动合并 + 一条关系边"（L2 最常见的输出形态）会让整笔图写入回滚：
//
//	extract: 写入图元素/别名: writer: put edge "...": constraint failed:
//	FOREIGN KEY constraint failed (787)
//
// # 根因（extract/pipeline.go）
//
// 边端点用的是 `NodeID(ep.Scope, r.From)` —— 由**表面形式现算**的 id：
//
//	addEdge(NodeID(ep.Scope, r.From), NodeID(ep.Scope, r.To), ...)
//
// 但 ZoneAutoMerge 分支**只写 alias、不建节点**（这是对的：合并到已有节点）。
// 于是当候选节点的 id 与 NodeID(scope, 表面形式) 不一致时，端点节点在库里
// 根本不存在，edges 的外键直接失败。
//
// # 为什么这是现实路径而不是构造出来的怪例
//
// 候选节点 id 与表面形式脱钩有两条完全正常的生产路径：
//  1. Judge(Same=true) 写 alias（"vim编辑器" → n-vim-编辑器-xxxx）；
//  2. 自动合并时把 L2 给的 aliases 一并写成 alias（makeAlias 循环）。
//
// 一旦某个别名在词典里指向了另一个 id，后续文本里提到该别名 + 它的关系，
// 就是本用例的形态：整段 episode 的图/别名/边全部丢失（只留 L0 的记忆）。
//
// # 影响
//
// 写入直接报错（Pipeline.Run 返回 error）。宿主若按"抽取失败就跳过"处理，
// 这段 episode 的实体与关系永久缺失，且**没有任何降级记录**能解释原因。
func TestBlockerAutoMergeEdgeFK(t *testing.T) {
	d, raw := openFileDB(t)
	ctx := context.Background()

	// 步骤 1：库里已有 "缓存服务" 节点，但 id 与表面形式**脱钩**。
	//
	// 这不是刻意构造怪例：Judge(Same=true) 写 alias、以及自动合并时把 L2 给的
	// aliases 一并写成 alias，都会让词典项指向一个 id 与表面形式不一致的节点。
	// 反而是"自洽 id"的形态永远不会走到 auto_merge（见【可后续】#3）。
	cacheID := "n-cache-svc"
	if cacheID == extract.NodeID(store.ScopeUser, "缓存服务") {
		t.Fatalf("用例前提失效：候选 id 应脱钩，实际 = NodeID(scope,name)")
	}
	if err := d.Graph().PutNode(ctx, store.Node{
		ID: cacheID, Scope: store.ScopeUser, CanonicalName: "缓存服务",
	}); err != nil {
		t.Fatalf("PutNode: %v", err)
	}
	seedAlias(t, d, "缓存服务", cacheID, 0.99)
	seedNode(t, d, "n-redis", "Redis")
	seedAlias(t, d, "Redis", "n-redis", 0.99)
	// 结构一致性信号：候选节点已有一条 depends_on 边。
	seedNode(t, d, "n-store", "存储")
	seedEdge(t, d, "e-seed-dep", cacheID, "n-store", store.RelationDependsOn)

	// 步骤 2：正常管线跑一段含关系句的文本（零 LLM，只用 L0）。
	p := extract.NewPipeline(d.Ports(), extract.Config{}, extract.PipelineOptions{})
	res, err := p.Run(ctx, extract.RawEpisode{
		ID: "e-fk", Scope: store.ScopeUser,
		Content: "Redis 依赖 缓存服务 做会话存储。",
	})

	// 步骤 3：判定必须是 auto_merge（否则本用例没测到该路径）。
	var merged bool
	var zoneLine strings.Builder
	for _, z := range res.Disambiguation {
		fmt.Fprintf(&zoneLine, "\n    mention=%q zone=%s node=%q score=%.3f",
			z.Mention, z.Zone, z.NodeID, z.Score)
		if z.Zone == extract.ZoneAutoMerge {
			merged = true
		}
	}

	if !merged {
		t.Fatalf("用例前提失效：本应出现 auto_merge 判定，实际：%s\n  err=%v",
			zoneLine.String(), err)
	}

	if err == nil {
		// 已经修好：确认边真的落地了，并提示摘掉门禁。
		if n := countRelation(t, raw, string(store.RelationDependsOn)); n < 2 {
			t.Errorf("无报错但 depends_on 边只有 %d 条（期望 >=2：种子 1 + 本次 1）", n)
		}
		t.Logf("自动合并 + 关系边已能正常写入（本缺陷已修复，可摘除 GMEM_VERIFY_BLOCKER 门禁）%s",
			zoneLine.String())
		return
	}

	report := fmt.Sprintf(`【阻塞合并】自动合并 + 关系边导致整笔图写入回滚
  err      = %v
  盘面上的消歧结论：%s
  nodes=%d edges=%d aliases=%d（图元素全部丢失）

根因（extract/pipeline.go writeEpisode）：
  边端点由**表面形式现算**：
      addEdge(NodeID(ep.Scope, r.From), NodeID(ep.Scope, r.To), ...)
  而 ZoneAutoMerge 分支只写 alias、不建节点。当候选节点 id ≠ NodeID(scope,surface)
  时（Judge 写 alias、或自动合并写入 L2 给的 aliases 都会造成这种脱钩），
  端点节点不存在 → edges 外键失败（787）→ 整个 bundle 回滚。

复现条件（都是正常路径）：
  a) 文本里的实体与库里某个节点同名但 id 不同（别名合并后必然发生）；
  b) 该实体还出现在一条关系句里（L2 最常见的"实体 + 关系"输出）；
  c) 判别式为 auto_merge（cosine/BM25/结构一致性足够高）。

影响：Pipeline.Run 返回 error，这段 episode 的节点/别名/边全部丢失，
      且没有任何 Degraded 记录能解释原因。

最小修复方向（不由本测试实施）：
  把"表面形式 → 实际节点 id"收敛成一次映射：auto_merge 时记下
  surface → d.NodeID，gray/new 时记下 surface → newID；addEdge 用映射后的 id
  而不是现算 NodeID(scope, surface)。端点缺失时应跳过该边并记 Warnings，
  而不是让整笔写入失败（一条边不值得丢掉整段 episode）。`,
		err, zoneLine.String(),
		countRows(t, raw, `SELECT COUNT(1) FROM nodes`),
		countRows(t, raw, `SELECT COUNT(1) FROM edges`),
		countRows(t, raw, `SELECT COUNT(1) FROM aliases`))

	if !merged {
		t.Fatalf("用例前提失效：本应出现 auto_merge 判定，实际：%s\n%s",
			zoneLine.String(), report)
	}
	if blockerGate() {
		t.Fatal(report)
	}
	t.Log(report)
}

// TestBlockerForcedFinalGraceOnlyIsNotWallClock 已由 TestG5WallClockBoundsBlockingToolRound 覆盖
// （G5 不约束工具轮）。这里只记录它属于同一类"门禁只在轮之间判定"的问题。

// TestDeferredJudgeKeepsStalePending 是【可后续】#2：Judge 只沿一个方向作废 possiblySame。
//
// # 现象
//
// 裁决为"不同实体"后，那条 possiblySame 边是否被置失效，取决于调用方传的
// Keep/Other 顺序：Judge 只从 KeepNodeID 出发查它的**出边**。
// 灰区边是「新提及 → 候选」方向；若调用方把 Keep 传成候选节点（这是更自然的
// 语义：保留既有节点），出边一条都没有 → 陈旧边保持有效。
//
// # 影响
//
// 事实不会被弄错（negative_SameAs 的判定优先），但待审池里会留下一条
// 已裁决过的陈旧边。它计入 §4 的"灰区待审积压"护栏：积压超过
// GrayBacklogLimit 会触发"暂停 L2，只跑 L0/L1"——于是抽取质量在无人察觉的
// 情况下下降，而根因是几条早就裁决完的僵尸边。
func TestDeferredJudgeKeepsStalePending(t *testing.T) {
	d, raw := openFileDB(t)
	ctx := context.Background()
	seedCandidate(t, d, "n-vim-edit", candidateName, false)
	p := newZonePipeline(t, d, extract.Config{}, grayMention(), nil)

	res := runZone(t, p, "e-pending")
	gray := zoneOf(t, res, candidateName)
	if gray.Zone != extract.ZoneGray {
		t.Fatalf("前置条件：应为灰区，实际 %q", gray.Zone)
	}
	newID := extract.NodeID(store.ScopeUser, candidateName)
	// livePending 数的是**当前版本**里仍然有效的 possiblySame 边。
	//
	// 为什么必须带 t_expired 条件（这个断言原先漏了它）：
	// D12/D13 要求实效写“新版本 + 旧版本 t_expired”，旧行是**审计保留**的。
	// 只查 `t_invalid IS NULL` 会把已被新版本取代的历史行也算成“有效”，
	// 于是即使实现完全正确，计数也不会归零——那是断言写错了，不是缺陷。
	// 本用例的原意图不变：裁决后不得再有**有效**的 possiblySame 边。
	livePending := func() int {
		return countRows(t, raw,
			`SELECT COUNT(1) FROM edges WHERE relation = ? AND t_invalid IS NULL AND t_expired IS NULL`,
			string(extract.RelationPossiblySame))
	}
	if got := livePending(); got != 1 {
		t.Fatalf("前置条件：应有 1 条有效 possiblySame，实际 %d", got)
	}

	// 自然的调用顺序：保留既有节点（Keep=候选），把新提及判为不同（Other=新节点）。
	if _, err := p.Judge(ctx, extract.Verdict{
		Scope: store.ScopeUser, KeepNodeID: gray.NodeID, OtherNodeID: newID,
		Same: false, Reason: "同名不同产品",
	}); err != nil {
		t.Fatalf("Judge: %v", err)
	}
	stale := livePending()
	// 第二个观测面（更接近真实消费方）：图端口沿两个端点都不应再看到它，
	// 灰区积压护栏正是靠这个邻域查询计数的。
	for _, id := range []string{gray.NodeID, newID} {
		hops, err := d.Graph().Neighbors(ctx, store.NeighborQuery{
			NodeID: id, Scope: store.ScopeUser,
			Relations: []store.Relation{extract.RelationPossiblySame}, Hops: 1,
		})
		if err != nil {
			t.Fatalf("Neighbors(%q): %v", id, err)
		}
		if len(hops) != 0 {
			t.Errorf("裁决后 Neighbors(%q) 仍返回 %d 条 possiblySame（僵尸边）", id, len(hops))
		}
	}
	if stale == 0 {
		t.Log("裁决已作废对应 possiblySame 边（本项已修复，可摘除 GMEM_VERIFY_DEFERRED 门禁）")
		return
	}
	report := fmt.Sprintf(`【可后续】Judge 未作废 possiblySame 边（取决于 Keep/Other 方向）
  Judge(Keep=%q, Other=%q, Same=false) 之后仍有效 possiblySame 边 = %d 条（期望 0）
  negative_SameAs = %d 条（裁决本身写成功了）

根因（extract/pipeline.go Judge）：
  只沿 KeepNodeID 的**出边**查 possiblySame：
      Neighbors(NodeID: v.KeepNodeID, Relations: [possiblySame])
  灰区边是「新提及 → 候选」方向；把 Keep 传成候选节点时出边为空，
  于是陈旧边保持 t_invalid IS NULL。

影响：不改变判定正确性（negative_SameAs 优先），但待审池里留下僵尸边；
      它计入 §4 灰区积压护栏，积压超限会让 L2 静默暂停、抽取质量下降。

最小修复方向：两端各查一次（参考 pairHasPending 已有的双向实现）。
同一问题也适用于 Same=true：合并后那条 possiblySame 同样不会被作废。`,
		gray.NodeID, newID, stale,
		countRelation(t, raw, string(extract.RelationNegativeSameAs)))

	if deferredGate() {
		t.Fatal(report)
	}
	t.Log(report)
}

// TestDeferredSelfDerivedCandidatesNeverAutoMerge 是【可后续】#3：自指排除过宽。
//
// # 现象
//
// scoreMention 排除 cid == NodeID(scope, surface) 的候选（防"与自身 cosine=1"）。
// 但这条排除也顺带把"库里那个节点**就是**由这个表面形式创建的"的正常情况
// 一起排掉了：此时候选池清空 → 判 new → 走 makeNode 重写同一个 id。
//
// # 为什么算可后续而不是阻塞
//
// 结果无害（同 id 覆盖写，不会产生重复节点），但有两个副作用：
//  1. 自动合并路径对"自洽 id"的实体永远不触发 —— 也就是 §4 的
//     "写 alias · confidence=high" 在最常见的实体上从不发生；
//  2. 它掩盖了 #1 那条缺陷的触发面（FK 只在脱钩 id 上出现），
//     所以修 #1 时必须同时想清楚这里的语义。
func TestDeferredSelfDerivedCandidatesNeverAutoMerge(t *testing.T) {
	d, _ := openFileDB(t)
	ctx := context.Background()
	selfID := extract.NodeID(store.ScopeUser, candidateName)
	if err := d.Graph().PutNode(ctx, store.Node{
		ID: selfID, Scope: store.ScopeUser, CanonicalName: candidateName,
	}); err != nil {
		t.Fatalf("PutNode: %v", err)
	}
	seedAlias(t, d, candidateName, selfID, 0.99)
	seedNode(t, d, pluginNodeID, pluginNodeName)
	seedEdge(t, d, "e-self-uses", selfID, pluginNodeID, store.RelationUses)

	dis := extract.NewDisambiguator(d.Ports(), fixedEmbedder(), extract.Config{})
	res, err := dis.Disambiguate(ctx, store.ScopeUser, []extract.Mention{{
		Surface: candidateName, Values: []float32{1, 0, 0},
		RelationTypes: []store.Relation{store.RelationUses},
	}})
	if err != nil {
		t.Fatalf("Disambiguate: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("应有一个提及结论，实际 %d", len(res))
	}
	got := res[0]

	// 对照组：id 脱钩的同一节点 → 同样的输入应当 auto_merge。
	d2, _ := openFileDB(t)
	seedNode(t, d2, "n-decoupled", candidateName)
	seedAlias(t, d2, candidateName, "n-decoupled", 0.99)
	seedNode(t, d2, pluginNodeID, pluginNodeName)
	seedEdge(t, d2, "e-dec-uses", "n-decoupled", pluginNodeID, store.RelationUses)
	dis2 := extract.NewDisambiguator(d2.Ports(), fixedEmbedder(), extract.Config{})
	res2, err := dis2.Disambiguate(ctx, store.ScopeUser, []extract.Mention{{
		Surface: candidateName, Values: []float32{1, 0, 0},
		RelationTypes: []store.Relation{store.RelationUses},
	}})
	if err != nil {
		t.Fatalf("Disambiguate(control): %v", err)
	}

	if got.Zone == extract.ZoneAutoMerge {
		t.Log("自洽 id 的候选已能自动合并（本项已修复，可摘除 GMEM_VERIFY_DEFERRED 门禁）")
		return
	}
	report := fmt.Sprintf(`【可后续】自洽 id（cid == NodeID(scope,surface)）的候选永不自动合并
  自洽 id  ：zone=%s score=%.3f breakdown=%d 项（候选池被清空）
  脱钩 id  ：zone=%s score=%.3f node=%q（同样输入就会合并）

根因：scoreMention 里
      if c.node.ID == selfID { continue }
  排除得很彻底。当"库里这个节点本来就是由该表面形式创建"时，
  候选池变空 → 判 new → 走 makeNode 重写同一个 id（幂等，所以无害）。

影响：
  1. §4 的"自动合并 → 写 alias · confidence=high"在最常见的实体上从不发生；
  2. 它把 #1（FK 回滚）的触发面掩盖成"只在脱钩 id 上出现"。

最小修复方向：排除条件收紧为"候选**没有**其它别名证据时"，
或按 (mention surface, candidate surface) 而非节点 id 判自指。`,
		got.Zone, got.Score, len(got.Breakdown), res2[0].Zone, res2[0].Score, res2[0].NodeID)

	if deferredGate() {
		t.Fatal(report)
	}
	t.Log(report)
}

// TestDeferredWriteReportLacksLexicalFlag 是【可后续】#4：索引同步不可观测。
//
// Writer 的 PutResult.LexicalIndexed 恒为 true（那是它存在的理由），
// 但 Pipeline 把它折叠成 extract.WriteReport 时丢掉了这个字段，
// 于是调用方无法从结果里确认"这条写入的索引真的同步了"——只能靠事后检索间接验证。
func TestDeferredWriteReportLacksLexicalFlag(t *testing.T) {
	d, _ := openFileDB(t)
	p := extract.NewPipeline(d.Ports(), extract.Config{}, extract.PipelineOptions{})
	res, err := p.Run(context.Background(), extract.RawEpisode{
		ID: "e-report", Scope: store.ScopeUser,
		Content: "团队决定采用 SQLite 作为默认后端。",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Writes) == 0 {
		t.Fatal("应有 WriteReport")
	}
	// 行为层面是对的（内容可搜到），只是结果里看不到这个标记。
	hits, err := d.Lexical().Search(context.Background(), store.LexicalQuery{
		Query: "SQLite", Scope: store.ScopeUser, Limit: 5,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatalf("写入内容搜不到——这属于阻塞项（见 TestWrittenContentIsLexicallySearchable）")
	}
	t.Logf(`【可后续】extract.WriteReport 不含 LexicalIndexed：
  WriteReport 字段 = {MemoryID, Version, NodeIDs, EdgeIDs, AliasIDs}
  行为层面索引已同步（%q 命中 %d 条），但结果里无法直接断言。
影响：宿主无法在写入点就发现"索引没同步"，只能靠事后检索——那时已经晚了。
最小修法：给 extract.WriteReport 加 LexicalIndexed bool 并从 recall.PutResult 透传。`,
		"SQLite", len(hits))
	_ = sqlite.Config{}
}

// 保证 sqlite 包被引用（openFileDB 返回 *sqlite.DB）。
var _ = (*sqlite.DB)(nil)
