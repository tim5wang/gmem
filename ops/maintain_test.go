package ops_test

import (
	"context"
	"strings"
	"testing"

	"github.com/tim5wang/gmem/ops"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// 9) Agent 维护面：ReviewSimilar / Merge / Link / Supersede / Consolidate / PromoteConcept
//
// 六个方法都必须经由同一个 Runner（D3），且每个都有独立断言。
// ===========================================================================

// TestReviewSimilarListsGrayPairsWithoutMerging 是"灰区绝不合并"的专项用例：
// ReviewSimilar 只**列**出候选对，不得改任何图/本体状态。
func TestReviewSimilarListsGrayPairsWithoutMerging(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	seedGrayPair(t, e)

	res, err := e.maint.ReviewSimilar(ctx, ops.ReviewRequest{Scope: store.ScopeUser, Actor: "agent"})
	if err != nil {
		t.Fatalf("ReviewSimilar: %v", err)
	}
	if len(res.Similar) != 1 {
		t.Fatalf("疑似重复对数=%d, want 1（%+v）", len(res.Similar), res.Similar)
	}
	p := res.Similar[0]
	if p.LeftNode != "n-a" || p.RightNode != "n-b" {
		t.Errorf("返回的对=%s/%s, want n-a/n-b", p.LeftNode, p.RightNode)
	}
	if p.StepID == "" {
		t.Error("疑似重复对缺少 step id（无法被审计与后续裁决引用）")
	}

	// 关键断言：只列不改。合并数必须为 0，节点与边都不变。
	if got := e.countRows(`SELECT COUNT(1) FROM memory_redirects`); got != 0 {
		t.Errorf("ReviewSimilar 产生了 %d 条重定向（灰区绝不合并）", got)
	}
	if got := e.countRows(`SELECT COUNT(1) FROM nodes`); got != 2 {
		t.Errorf("ReviewSimilar 后节点数=%d, want 2", got)
	}
	if got := e.countRows(`SELECT COUNT(1) FROM edges WHERE relation='possiblySame'`); got != 1 {
		t.Errorf("ReviewSimilar 改动了待审边（possiblySame 数=%d）", got)
	}
	// 审计必须留下"审过哪一对"（Changed=false，但事件可见）。
	entries, err := e.maint.Audit(ctx, ops.AuditQuery{Op: ops.OpDisambiguate})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if len(entries) == 0 {
		t.Error("ReviewSimilar 没有留下审计记录")
	}
	for _, a := range entries {
		if a.Changed {
			t.Errorf("评审记录被标记为 changed（应当只读）")
		}
	}
}

// TestMergeMigratesEdgesAndKeepsRedirect 是 Merge 的硬要求：
// 迁移边、迁移 alias、**保留旧 ID 为重定向**。
func TestMergeMigratesEdgesAndKeepsRedirect(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, n := range []store.Node{
		{ID: "n-a", Scope: store.ScopeUser, CanonicalName: "张三(A)"},
		{ID: "n-b", Scope: store.ScopeUser, CanonicalName: "张三"},
		{ID: "n-c", Scope: store.ScopeUser, CanonicalName: "认证服务"},
	} {
		if err := e.db.Graph().PutNode(ctx, n); err != nil {
			t.Fatalf("PutNode(%s): %v", n.ID, err)
		}
	}
	// n-a 的两条边：一条入、一条出（迁移后都要指向/来自 n-b）。
	for _, ed := range []store.Edge{
		{ID: "e-a-out", FromID: "n-a", ToID: "n-c", Relation: store.RelationUses, Weight: 1, TValid: 100, TCreated: 100},
		{ID: "e-a-in", FromID: "n-c", ToID: "n-a", Relation: store.RelationDependsOn, Weight: 1, TValid: 100, TCreated: 100},
	} {
		if err := e.db.Graph().PutEdge(ctx, ed); err != nil {
			t.Fatalf("PutEdge(%s): %v", ed.ID, err)
		}
	}
	// n-a 上的 alias 必须迁到 n-b（只加新的不够，旧的会让 n-a 继续被对齐到）。
	if err := e.db.Store().PutAlias(ctx, store.Alias{
		Surface: "张三(A)", NodeID: "n-a", Source: "dict", Confidence: 1,
	}); err != nil {
		t.Fatalf("PutAlias: %v", err)
	}

	res, err := e.maint.Merge(ctx, ops.MergeRequest{
		Scope: store.ScopeUser, FromNode: "n-a", ToNode: "n-b", Reason: "同一人",
		Actor: "agent-7",
	})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if res.Status != ops.RunApplied {
		t.Fatalf("Merge 状态=%s, want applied", res.Status)
	}

	// 1) 边迁移：n-b 现在有 2 条有效边（入+出），n-a 的旧边已失效但仍在库里。
	var activeFrom, activeTo int
	if err := e.raw.QueryRow(
		`SELECT COUNT(1) FROM edges WHERE t_expired IS NULL AND t_invalid IS NULL AND from_id='n-b'`).
		Scan(&activeFrom); err != nil {
		t.Fatalf("count edges from n-b: %v", err)
	}
	if err := e.raw.QueryRow(
		`SELECT COUNT(1) FROM edges WHERE t_expired IS NULL AND t_invalid IS NULL AND to_id='n-b'`).
		Scan(&activeTo); err != nil {
		t.Fatalf("count edges to n-b: %v", err)
	}
	if activeFrom != 1 || activeTo != 1 {
		t.Errorf("迁移后 n-b 的有效边：出=%d 入=%d, want 1/1", activeFrom, activeTo)
	}
	// 旧边**不得物理删除**：只置 t_invalid（世界时间），版本历史全部保留。
	// 用 DISTINCT id 计数，因为失效会追加新版本（同一条边因此可能有多行）。
	if got := e.countRows(`SELECT COUNT(DISTINCT id) FROM edges WHERE from_id='n-a' OR to_id='n-a'`); got != 2 {
		t.Errorf("旧边被物理删除（剩 %d 条）——合并必须保留历史，只置 t_invalid", got)
	}
	if got := e.countRows(`SELECT COUNT(1) FROM edges WHERE (from_id='n-a' OR to_id='n-a') AND t_invalid IS NULL AND t_expired IS NULL`); got != 0 {
		t.Errorf("旧边仍有 %d 条有效——合并后它们必须失效", got)
	}

	// 2) alias 迁移：surface 现在指向 n-b，且不再指向 n-a。
	var nodeID string
	if err := e.raw.QueryRow(`SELECT node_id FROM aliases WHERE surface='张三(A)'`).Scan(&nodeID); err != nil {
		t.Fatalf("读 alias: %v", err)
	}
	if nodeID != "n-b" {
		t.Errorf("alias 指向=%q, want n-b（必须迁移，否则旧节点仍会被对齐到）", nodeID)
	}

	// 3) 旧 ID 保留为重定向（TD §4）。
	got, err := e.db.OpsBackend().ResolveRedirect(ctx, "n-a")
	if err != nil {
		t.Fatalf("ResolveRedirect: %v", err)
	}
	if got != "n-b" {
		t.Errorf("重定向解析=%q, want n-b", got)
	}
	// 链式解析：a → b → c 解析到 c（Merge 是可叠加的）。
	if err := e.db.OpsBackend().PutRedirect(ctx, ops.Redirect{OldID: "n-b", NewID: "n-c", Scope: store.ScopeUser}); err != nil {
		t.Fatalf("PutRedirect: %v", err)
	}
	if got, err := e.db.OpsBackend().ResolveRedirect(ctx, "n-a"); err != nil || got != "n-c" {
		t.Errorf("链式重定向=%q err=%v, want n-c", got, err)
	}

	// 4) 旧 ID 仍可解释：审计里能查到这次合并的动作、发起人与原因。
	entries, err := e.maint.Audit(ctx, ops.AuditQuery{Subject: "n-a"})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("合并后旧 ID 没有审计记录（不可解释）")
	}
	if entries[0].Actor != "agent-7" || entries[0].Reason != "同一人" {
		t.Errorf("审计元数据=%+v, want actor=agent-7 reason=同一人", entries[0])
	}
}

// TestLinkAndUnlink 覆盖 LinkUpdate 的单值关系语义与 unlink。
func TestLinkAndUnlink(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, n := range []store.Node{
		{ID: "n-alice", Scope: store.ScopeUser, CanonicalName: "Alice"},
		{ID: "n-acme", Scope: store.ScopeUser, CanonicalName: "Acme"},
		{ID: "n-other", Scope: store.ScopeUser, CanonicalName: "Other"},
	} {
		if err := e.db.Graph().PutNode(ctx, n); err != nil {
			t.Fatalf("PutNode: %v", err)
		}
	}

	// 第一次 link：建立边。
	if _, err := e.maint.Link(ctx, ops.LinkRequest{
		Scope: store.ScopeUser, FromNode: "n-alice", ToNode: "n-acme",
		Relation: store.RelationWorksAt, Actor: "agent",
	}); err != nil {
		t.Fatalf("Link: %v", err)
	}
	if got := e.countRows(`SELECT COUNT(1) FROM edges WHERE relation='works_at' AND t_invalid IS NULL AND t_expired IS NULL`); got != 1 {
		t.Fatalf("works_at 有效边=%d, want 1", got)
	}

	// 第二次 link（单值关系换目标）：旧边必须失效，而不是并列保留。
	if _, err := e.maint.Link(ctx, ops.LinkRequest{
		Scope: store.ScopeUser, FromNode: "n-alice", ToNode: "n-other",
		Relation: store.RelationWorksAt, Actor: "agent",
	}); err != nil {
		t.Fatalf("Link 换目标: %v", err)
	}
	var active int
	if err := e.raw.QueryRow(
		`SELECT COUNT(1) FROM edges WHERE relation='works_at' AND from_id='n-alice'
		   AND t_invalid IS NULL AND t_expired IS NULL`).
		Scan(&active); err != nil {
		t.Fatalf("count: %v", err)
	}
	if active != 1 {
		t.Errorf("单值关系（works_at）有效的目标数=%d, want 1（旧边应失效）", active)
	}
	if got := e.countRows(`SELECT COUNT(DISTINCT id) FROM edges WHERE relation='works_at'`); got != 2 {
		t.Errorf("works_at 边数=%d, want 2（旧边保留历史，只置 t_invalid）", got)
	}

	// unlink：失效边。
	if _, err := e.maint.Link(ctx, ops.LinkRequest{
		Scope: store.ScopeUser, FromNode: "n-alice", ToNode: "n-other",
		Relation: store.RelationWorksAt, Remove: true, Actor: "agent",
	}); err != nil {
		t.Fatalf("Unlink: %v", err)
	}
	if err := e.raw.QueryRow(
		`SELECT COUNT(1) FROM edges WHERE relation='works_at' AND from_id='n-alice'
		   AND t_invalid IS NULL AND t_expired IS NULL`).
		Scan(&active); err != nil {
		t.Fatalf("count after unlink: %v", err)
	}
	if active != 0 {
		t.Errorf("unlink 后仍有 %d 条有效边", active)
	}
}

// TestSupersedeKeepsHistory 覆盖 Supersede（Agent 主动宣布旧事实作废）。
func TestSupersedeKeepsHistory(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	m := memory("m-1", store.ScopeUser, "")
	m.TValid, m.TCreated = 100, 100
	e.putMemory(m)

	res, err := e.maint.Supersede(ctx, ops.SupersedeRequest{
		Scope: store.ScopeUser, MemoryID: "m-1", TInvalid: 500, Actor: "agent", Reason: "已被新事实推翻",
	})
	if err != nil {
		t.Fatalf("Supersede: %v", err)
	}
	if res.Status != ops.RunApplied {
		t.Fatalf("Status=%s, want applied", res.Status)
	}
	if got := e.scalar(`SELECT t_invalid FROM memories WHERE id='m-1'`); got != "500" {
		t.Errorf("t_invalid=%q, want 500", got)
	}
	if got := e.countRows(`SELECT COUNT(1) FROM memories WHERE id='m-1'`); got != 1 {
		t.Errorf("Supersede 物理删除了记忆（rows=%d）——Invalidated 不等于删除", got)
	}
	if st := e.stateStatus("m-1"); st != string(ops.StatusInvalidated) {
		t.Errorf("状态=%q, want invalidated", st)
	}
}

// TestConsolidateAdvancesWatermark 断言整理会推进水位（阈值触发的判据）。
func TestConsolidateAdvancesWatermark(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	fresh := memory("m-1", store.ScopeUser, "")
	fresh.TValid = e.clk.Now().Unix()
	fresh.TCreated = e.clk.Now().Unix()
	e.putMemory(fresh)

	if _, err := e.maint.Consolidate(ctx, ops.ConsolidateRequest{Scope: store.ScopeUser, Actor: "agent"}); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	at, ok, err := e.db.OpsBackend().GetWatermark(ctx, store.ScopeUser, ops.OpConsolidate)
	if err != nil {
		t.Fatalf("GetWatermark: %v", err)
	}
	if !ok || at <= 0 {
		t.Fatalf("整理后水位=%d ok=%v, want 已推进", at, ok)
	}
	n, err := e.db.OpsBackend().CountUnconsolidatedMemories(ctx, store.ScopeUser)
	if err != nil {
		t.Fatalf("CountUnconsolidatedMemories: %v", err)
	}
	if n != 0 {
		t.Errorf("整理后未整理条目=%d, want 0（水位应把它算进去）", n)
	}

	// 再写一条（时间更晚）→ 未整理条目回到 1。
	e.clk.Advance(2 * 1e9) // 2s（t_created 是秒）
	newer := memory("m-2", store.ScopeUser, "")
	newer.TValid = e.clk.Now().Unix()
	newer.TCreated = e.clk.Now().Unix()
	e.putMemory(newer)
	n, err = e.db.OpsBackend().CountUnconsolidatedMemories(ctx, store.ScopeUser)
	if err != nil {
		t.Fatalf("CountUnconsolidatedMemories: %v", err)
	}
	if n != 1 {
		t.Errorf("新写入后未整理条目=%d, want 1", n)
	}
}

// TestMaintainerRequiresScope 断言维护面同样强制 scope 隔离（空 scope 必须报错）。
func TestMaintainerRequiresScope(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.maint.Consolidate(ctx, ops.ConsolidateRequest{}); err == nil {
		t.Error("空 scope 的 Consolidate 应当报错（Scope 是必填的隔离边界）")
	} else if !strings.Contains(err.Error(), "scope") {
		t.Errorf("错误信息应点明 scope：%v", err)
	}
	if _, err := e.maint.Merge(ctx, ops.MergeRequest{FromNode: "a", ToNode: "b"}); err == nil {
		t.Error("空 scope 的 Merge 应当报错")
	}
}

// seedGrayPair 造一对灰区待审（possiblySame 边 + alias）。
func seedGrayPair(t *testing.T, e *env) {
	t.Helper()
	ctx := context.Background()
	for _, n := range []store.Node{
		{ID: "n-a", Scope: store.ScopeUser, CanonicalName: "张三(A)"},
		{ID: "n-b", Scope: store.ScopeUser, CanonicalName: "张三"},
	} {
		if err := e.db.Graph().PutNode(ctx, n); err != nil {
			t.Fatalf("PutNode(%s): %v", n.ID, err)
		}
	}
	if err := e.db.Graph().PutEdge(ctx, store.Edge{
		ID: "e-gray", FromID: "n-a", ToID: "n-b", Relation: "possiblySame",
		Weight: 1, TValid: 100, TCreated: 100,
	}); err != nil {
		t.Fatalf("PutEdge(gray): %v", err)
	}
	// 待审池的枚举从 alias 出发（GraphStore 没有"全表枚举边"的端口）。
	for _, a := range []store.Alias{
		{Surface: "张三(A)", NodeID: "n-a", Source: "dict", Confidence: 1},
		{Surface: "张三", NodeID: "n-b", Source: "dict", Confidence: 1},
	} {
		if err := e.db.Store().PutAlias(ctx, a); err != nil {
			t.Fatalf("PutAlias(%s): %v", a.Surface, err)
		}
	}
}
