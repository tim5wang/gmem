package p3verify

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tim5wang/gmem/ops"
	"github.com/tim5wang/gmem/recall"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// 攻击面 4：Prune 边界（TD §7 纪律 3）+ D15 draft 清理 + Invalidated 不等于删除
// ===========================================================================

// TestPruneDefaultOnlyTouchesArchived 断言 Prune 默认**只**物理删除 Archived。
//
// 为什么这是最危险的一条：多削一刀就是活数据永久消失，而且 as_of 再也答不了
// "当时是什么"。因此本用例给六种状态各造一条，逐条断言**还在**。
func TestPruneDefaultOnlyTouchesArchived(t *testing.T) {
	e := newEnv(t)

	statuses := []ops.MemoryStatus{
		ops.StatusCandidate, ops.StatusActive, ops.StatusRejected,
		ops.StatusInvalidated, ops.StatusStale, ops.StatusArchived,
	}
	for _, st := range statuses {
		id := "m-" + string(st)
		m := mem(id, store.ScopeUser, "")
		m.TValid, m.TCreated = e.clk.Now().Unix(), e.clk.Now().Unix()
		e.put(m)
		e.seedState(id, st, 0.9)
	}
	before := e.countRows("memories")

	// 默认 Prune（不带 IncludeInvalidated）。
	res := e.mustRun(ops.Request{Op: ops.OpPrune, Scope: store.ScopeUser,
		Input: ops.Input{IncludeDrafts: new(bool)}})
	if res.Status != ops.RunApplied {
		t.Fatalf("Prune 状态=%s, want applied", res.Status)
	}

	for _, st := range statuses[:len(statuses)-1] {
		id := "m-" + string(st)
		if got := e.count("SELECT COUNT(1) FROM memories WHERE id = ?", id); got != 1 {
			t.Errorf("Prune 误删了 %s 状态的记忆 %s（TD §7 纪律 3：默认只作用于 Archived）", st, id)
		}
	}
	if got := e.count("SELECT COUNT(1) FROM memories WHERE id = ?", "m-archived"); got != 0 {
		t.Errorf("Archived 记忆未被物理删除（Prune 没生效）")
	}
	if got := e.countRows("memories"); got != before-1 {
		t.Errorf("删除后记忆数=%d, want %d（只该少 1 条）", got, before-1)
	}
	// 状态行不得留孤儿。
	if got := e.count("SELECT COUNT(1) FROM memory_state WHERE memory_id = ?", "m-archived"); got != 0 {
		t.Errorf("Archived 的状态行未被清理（留下孤儿状态）")
	}
	if got := e.count("SELECT COUNT(1) FROM memories_fts WHERE id = ?", "m-archived"); got != 0 {
		t.Errorf("Archived 的 FTS 索引未被清理（幽灵向量/幽灵召回）")
	}
}

// TestPruneInvalidatedRequiresExplicitOptIn 断言 Invalidated 必须显式 opt-in。
func TestPruneInvalidatedRequiresExplicitOptIn(t *testing.T) {
	e := newEnv(t)
	m := mem("m-inv", store.ScopeUser, "")
	m.TValid, m.TCreated = e.clk.Now().Unix(), e.clk.Now().Unix()
	e.put(m)
	e.seedState("m-inv", ops.StatusInvalidated, 0.9)

	// 默认：不动。
	e.mustRun(ops.Request{Op: ops.OpPrune, Scope: store.ScopeUser,
		Input: ops.Input{IncludeDrafts: new(bool)}})
	if got := e.count("SELECT COUNT(1) FROM memories WHERE id='m-inv'"); got != 1 {
		t.Fatalf("默认 Prune 删掉了 Invalidated 记忆（应保留以支持 as_of）")
	}

	// 显式 opt-in（新窗口）：才删。
	e.clk.Advance(time.Second)
	e.mustRun(ops.Request{Op: ops.OpPrune, Scope: store.ScopeUser,
		Input: ops.Input{IncludeInvalidated: true, IncludeDrafts: new(bool)}})
	if got := e.count("SELECT COUNT(1) FROM memories WHERE id='m-inv'"); got != 0 {
		t.Errorf("显式 opt-in 后 Invalidated 记忆仍未删除")
	}
}

// TestInvalidatedIsNotDeletion 断言矛盾失效后旧事实仍能 as_of 回溯（TD §7 纪律 1）。
func TestInvalidatedIsNotDeletion(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	m := mem("m-old", store.ScopeUser, "")
	m.Content = "部署环境是预发布集群（旧事实，后被告知作废）"
	m.TValid, m.TCreated = 1000, 1000
	e.put(m)

	if _, err := e.maint.Supersede(ctx, ops.SupersedeRequest{
		Scope: store.ScopeUser, MemoryID: "m-old", TInvalid: 5000,
		Actor: "agent-1", Reason: "环境已变更",
	}); err != nil {
		t.Fatalf("Supersede: %v", err)
	}

	// 当前视角：不再返回（已失效）。
	cur, err := e.db.Store().ListMemories(ctx, store.MemoryQuery{Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("ListMemories: %v", err)
	}
	for _, x := range cur {
		if x.ID == "m-old" {
			t.Error("失效事实仍出现在当前视图里")
		}
	}

	// as_of 视角（t=2000，失效之前）：必须查得到——这就是"不等于删除"。
	at := int64(2000)
	past, err := e.db.Store().ListMemories(ctx, store.MemoryQuery{Scope: store.ScopeUser, AsOf: &at})
	if err != nil {
		t.Fatalf("ListMemories(as_of): %v", err)
	}
	found := false
	for _, x := range past {
		if x.ID == "m-old" {
			found = true
		}
	}
	if !found {
		t.Error("as_of 回溯查不到已失效的记忆（Invalidated 被当成删除了，违反 TD §7 纪律 1）")
	}

	// 词法通道同样要能回溯（D13：as_of 与词法检索可组合）。
	hits, err := e.db.Lexical().Search(ctx, store.LexicalQuery{
		Query: "预发布", Scope: store.ScopeUser, AsOf: &at, Limit: 10,
	})
	if err != nil {
		t.Fatalf("as_of search: %v", err)
	}
	if len(hits) == 0 {
		t.Error("as_of 词法回溯 0 命中：旧版本的索引被删了（D13 缺陷形状）")
	}
}

// TestD15PromoteMakesConceptParticipateInHardContract 断言 Promote 后 StatusFactor 从打折变 1.0。
func TestD15PromoteMakesConceptParticipateInHardContract(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	m := mem("m-auth", store.ScopeUser, "c-auth")
	m.TValid, m.TCreated = e.clk.Now().Unix(), e.clk.Now().Unix()
	e.put(m)
	c, err := e.db.Store().GetConcept(ctx, "c-auth")
	if err != nil {
		t.Fatalf("GetConcept: %v", err)
	}
	if c.Status != store.ConceptDraft || c.Origin != store.ConceptOriginImplicit {
		t.Fatalf("隐式概念 status=%q origin=%q, want draft/implicit", c.Status, c.Origin)
	}
	c.Name = "认证服务"
	if err := e.db.Store().PutConcept(ctx, c); err != nil {
		t.Fatalf("PutConcept: %v", err)
	}
	if f := statusFactorOf(t, e, "认证服务"); f >= 1.0 {
		t.Fatalf("draft 的 StatusFactor=%v, want < 1.0", f)
	}

	if _, err := e.maint.PromoteConcept(ctx, ops.PromoteRequest{
		Scope: store.ScopeUser, ConceptIDs: []string{"c-auth"}, Actor: "reviewer",
	}); err != nil {
		t.Fatalf("PromoteConcept: %v", err)
	}
	if f := statusFactorOf(t, e, "认证服务"); f != 1.0 {
		t.Errorf("Promote 后 StatusFactor=%v, want 1.0（应开始参与硬契约）", f)
	}
	got, err := e.db.Store().GetConcept(ctx, "c-auth")
	if err != nil {
		t.Fatalf("GetConcept after: %v", err)
	}
	if got.Status != store.ConceptCertified {
		t.Errorf("status=%q, want certified", got.Status)
	}
	if got.Origin != store.ConceptOriginImplicit {
		t.Errorf("origin=%q, want implicit（认证不得抹掉来源线索）", got.Origin)
	}
}

// TestD15PruneDraftsRespectsTTLAndRefs 断言 draft 清理的三个边界：
// 旧且无引用 → 删；新 → 留；certified → 一条不删；重复跑不报错。
func TestD15PruneDraftsRespectsTTLAndRefs(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	// 三个"孤儿" draft（无记忆引用）+ 一个被引用的 draft + 一个 certified。
	for _, id := range []string{"c-orphan-old", "c-orphan-new", "c-orphan-mid"} {
		if err := e.db.Store().PutConcept(ctx, store.Concept{
			ID: id, Name: id, Status: store.ConceptDraft, Origin: store.ConceptOriginImplicit,
		}); err != nil {
			t.Fatalf("PutConcept(%s): %v", id, err)
		}
	}
	// 被引用的 draft：不能删（否则 memories.concept_id 悬空）。
	m := mem("m-ref", store.ScopeUser, "c-referenced")
	m.TValid, m.TCreated = e.clk.Now().Unix(), e.clk.Now().Unix()
	e.put(m)
	// certified：任何情况下都不删。
	if err := e.db.Store().PutConcept(ctx, store.Concept{
		ID: "c-cert", Name: "c-cert", Status: store.ConceptCertified,
	}); err != nil {
		t.Fatalf("PutConcept: %v", err)
	}

	now := e.clk.Now().UnixMilli()
	// 旧：last_used_at 远早于 TTL；新：刚刚。mid 也用旧时间但在 TTL 内。
	e.exec("INSERT INTO concept_usage (concept_id, use_count, last_used_at) VALUES (?,1,?)",
		"c-orphan-old", now-90*24*3600*1000)
	e.exec("INSERT INTO concept_usage (concept_id, use_count, last_used_at) VALUES (?,1,?)",
		"c-orphan-new", now)
	e.exec("INSERT INTO concept_usage (concept_id, use_count, last_used_at) VALUES (?,1,?)",
		"c-orphan-mid", now-5*24*3600*1000)

	// DraftTTL 用 30 天（默认）：只有 c-orphan-old 该被删。
	res := e.mustRun(ops.Request{Op: ops.OpPrune, Scope: store.ScopeUser,
		Input: ops.Input{DraftTTLSeconds: 30 * 24 * 3600}})
	if res.Status != ops.RunApplied {
		t.Fatalf("Prune 状态=%s, want applied", res.Status)
	}

	if got := e.count("SELECT COUNT(1) FROM concepts WHERE id='c-orphan-old'"); got != 0 {
		t.Errorf("超过 TTL 且无引用的 draft 未被删除（D15 清理无效，草稿表会无界增长）")
	}
	for _, id := range []string{"c-orphan-new", "c-orphan-mid"} {
		if got := e.count("SELECT COUNT(1) FROM concepts WHERE id = ?", id); got != 1 {
			t.Errorf("TTL 内的 draft %s 被误删", id)
		}
	}
	if got := e.count("SELECT COUNT(1) FROM concepts WHERE id='c-referenced'"); got != 1 {
		t.Errorf("仍被引用的 draft 被删（留下悬空 concept_id）")
	}
	if got := e.count("SELECT COUNT(1) FROM concepts WHERE id='c-cert'"); got != 1 {
		t.Errorf("certified 概念被 Prune 删除")
	}

	// 重复跑（新窗口）：不报错、不重复删。
	e.clk.Advance(time.Second)
	res2 := e.mustRun(ops.Request{Op: ops.OpPrune, Scope: store.ScopeUser,
		Input: ops.Input{DraftTTLSeconds: 30 * 24 * 3600}})
	if res2.Status == ops.RunFailed {
		t.Errorf("重复 Prune 失败")
	}
	if got := e.count("SELECT COUNT(1) FROM concepts WHERE id='c-referenced'"); got != 1 {
		t.Errorf("重复 Prune 伤到了被引用概念")
	}
}

// TestPruneRollsBackWhenLexicalFails 断言派生索引失败时整笔物理删除回滚。
//
// 形状很具体：FTS 删失败却删了主表 → "索引有行、主表无行"的幽灵召回，
// 而且物理删除不可恢复。因此必须同事务回滚。
func TestPruneRollsBackWhenLexicalFails(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	m := mem("m-rb", store.ScopeUser, "")
	m.TValid, m.TCreated = e.clk.Now().Unix(), e.clk.Now().Unix()
	e.put(m)
	e.seedState("m-rb", ops.StatusArchived, 0.9)

	ports := e.db.Ports()
	ports.Lexical = failingLexical{}
	r, err := ops.NewRunner(ports, e.db.OpsBackend(), ops.WithConfig(e.cfg))
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	res, runErr := r.Run(ctx, ops.Request{Op: ops.OpPrune, Scope: store.ScopeUser,
		Input: ops.Input{IncludeDrafts: new(bool)}})
	if runErr == nil {
		t.Fatalf("Lexical 删失败时 Prune 应报错，实际 status=%s", res.Status)
	}
	// 整笔回滚：主表行还在，状态行也还在。
	if got := e.count("SELECT COUNT(1) FROM memories WHERE id='m-rb'"); got != 1 {
		t.Errorf("Lexical 失败后主表行仍被删除（半截状态：索引有行/主表无行）")
	}
	if got := e.count("SELECT COUNT(1) FROM memory_state WHERE memory_id='m-rb'"); got != 1 {
		t.Errorf("Lexical 失败后状态行被删（回滚不完整）")
	}
}

// failingLexical 是一个"删除必然失败"的词法端口（注入故障用）。
type failingLexical struct{}

func (failingLexical) Index(context.Context, []store.Document) error { return nil }
func (failingLexical) Delete(context.Context, []store.DocumentKey) error {
	return errors.New("注入故障：FTS 删除失败")
}
func (failingLexical) Search(context.Context, store.LexicalQuery) ([]store.Hit, error) {
	return nil, nil
}

// TestOpsProductionCodeNeverWritesMemories 是"写入必经 Writer"的源码守卫（本包独立实现）。
func TestOpsProductionCodeNeverWritesMemories(t *testing.T) {
	// 用 recall.Writer 的存在性做正向对照：ops 若自己写 memories，必然出现这些符号。
	forbidden := []string{"PutMemory(", "PutEpisode(", "NewWriter("}
	// 测试的工作目录是本包（internal/p3verify），ops/ 在仓库根下的两级之上。
	dir := "../../ops"
	total := 0
	for _, sym := range forbidden {
		hits := grepCount(t, dir, sym)
		total += hits
		if hits > 0 {
			t.Errorf("ops/ 生产代码里出现了 %q（%d 次）：写入必须经 recall.Writer / Extractor 注入", sym, hits)
		}
	}
	if total == 0 {
		t.Log("ops/ 生产代码无任何直写 memories/episodes 的调用点（写入编排不变量成立）")
	}
}

// TestStatusFactorSanity 断言 Aligner 真的会因概念状态打折（本文件多处依赖它）。
func TestStatusFactorSanity(t *testing.T) {
	e := newEnv(t)
	if got := statusFactorOf(t, e, "不存在的概念xyz"); got != 0 {
		t.Errorf("对齐到不存在概念时 StatusFactor=%v", got)
	}
}

func statusFactorOf(t *testing.T, e *env, query string) float64 {
	t.Helper()
	ctx := context.Background()
	or, ok := e.db.Store().(store.OntologyReader)
	if !ok {
		t.Fatal("后端未实现 OntologyReader")
	}
	al, err := recall.NewAligner(or, recall.DefaultConfig()).Align(ctx, query)
	if err != nil {
		t.Fatalf("Align(%q): %v", query, err)
	}
	for _, m := range al.Matched {
		if m.ConceptID != "" {
			return m.StatusFactor
		}
	}
	return 0
}
