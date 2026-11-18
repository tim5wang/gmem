package ops_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tim5wang/gmem/ops"
	"github.com/tim5wang/gmem/recall"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// 8) D15 的 draft 清理：Promote + Prune（本阶段的硬验收项）
//
// D15（B 方案）让写入时未知的 concept 自动落成 status=draft、origin=implicit。
// 代价是草稿表会被 agent 的随意输入填满。P3 必须提供两条清理路径，否则本体漂移无界。
// ===========================================================================

// TestD15DraftCleanupKeepsOntologyBounded 是 D15 的主回归：
// 100 个隐式 draft → Promote 一批、Prune 一批 → certified 保留、draft 不无界增长。
func TestD15DraftCleanupKeepsOntologyBounded(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	const (
		total      = 100
		promoted   = 10                            // 显式认证（仍被记忆引用）
		stillDraft = 10                            // 被引用但仍是 draft：必须拒绝删除
		pruned     = total - promoted - stillDraft // 无人引用：可清理
	)
	if pruned != 80 {
		t.Fatalf("测试参数不一致")
	}

	// 造 100 个隐式 draft：全部经**真实写入路径**产生——
	// 写入一个未声明的 concept_id，sqlite 的 ensureConcept 会把它落成 draft+implicit。
	for i := 0; i < total; i++ {
		cid := fmt.Sprintf("c-%03d", i)
		m := memory(fmt.Sprintf("m-%03d", i), store.ScopeUser, cid)
		m.TValid = e.clk.Now().Unix()
		m.TCreated = e.clk.Now().Unix()
		e.putMemory(m)
	}
	if got := e.countRows(`SELECT COUNT(1) FROM concepts WHERE status='draft'`); got != total {
		t.Fatalf("隐式 draft 数=%d, want %d（写入路径未按 D15 落 draft）", got, total)
	}
	if got := e.countRows(`SELECT COUNT(1) FROM concepts WHERE origin='implicit'`); got != total {
		t.Fatalf("origin=implicit 的概念数=%d, want %d", got, total)
	}

	// Promote 一批（前 10 个）。
	var promoteIDs []string
	for i := 0; i < promoted; i++ {
		promoteIDs = append(promoteIDs, fmt.Sprintf("c-%03d", i))
	}
	pres, err := e.maint.PromoteConcept(ctx, ops.PromoteRequest{
		Scope: store.ScopeUser, ConceptIDs: promoteIDs, Actor: "reviewer", Reason: "人工认证",
	})
	if err != nil {
		t.Fatalf("PromoteConcept: %v", err)
	}
	if pres.Status != ops.RunApplied {
		t.Fatalf("Promote 状态=%s, want applied", pres.Status)
	}
	if got := e.countRows(`SELECT COUNT(1) FROM concepts WHERE status='certified'`); got != promoted {
		t.Fatalf("certified 数=%d, want %d", got, promoted)
	}

	// 让后 80 个 draft 变成孤儿：它们的记忆走完整生命周期被物理清掉。
	// （这不是为了"造数据"，而是唯一允许的删除路径——Prune 只作用于 Archived。）
	//
	// 【T-1 修复】原处有一个空转的 for 循环（`m := memory(...)` 后 `_ = m`），
	// 它什么都不做却让读者以为"这里把状态机推进了"。真正推进状态机的是下面的
	// Invalidate + Consolidate + Prune 链路，因此这里直接删除死代码。
	// 用真实 op 推进：Consolidate（Active）→ Invalidate → 时钟推进 → Consolidate（Archived）→ Prune。
	// 为了不让用例跑 80 次状态机往返，这里只对 80 条记忆的**状态行**直接用 op 的 Invalidate
	// 循环一次；op 的语义与上面用例一致（已单独覆盖），这里验证的是"记忆消失后 draft 可清理"。
	for i := promoted + stillDraft; i < total; i++ {
		if _, err := e.runner.Run(ctx, ops.Request{
			Op: ops.OpInvalidate, Scope: store.ScopeUser,
			Input: ops.Input{Invalidate: &ops.InvalidateInput{MemoryID: fmt.Sprintf("m-%03d", i)}},
		}); err != nil {
			t.Fatalf("invalidate m-%03d: %v", i, err)
		}
	}
	e.clk.Advance(time.Duration(e.cfg.ArchiveAfterDays+1) * 24 * time.Hour)
	if _, err := e.runner.Run(ctx, ops.Request{Op: ops.OpConsolidate, Scope: store.ScopeUser}); err != nil {
		t.Fatalf("consolidate: %v", err)
	}
	if got := e.countRows(`SELECT COUNT(1) FROM memory_state WHERE status='archived'`); got != pruned {
		t.Fatalf("archived 状态行=%d, want %d", got, pruned)
	}
	if _, err := e.runner.Run(ctx, ops.Request{
		Op: ops.OpPrune, Scope: store.ScopeUser,
		Input: ops.Input{IncludeDrafts: new(bool)}, // 先只删记忆
	}); err != nil {
		t.Fatalf("prune memories: %v", err)
	}
	if got := e.countRows(`SELECT COUNT(1) FROM memories`); got != total-pruned {
		t.Fatalf("物理删除后记忆数=%d, want %d", got, total-pruned)
	}

	// Prune draft：只有那 80 个孤儿 draft 会被删；被引用的 10 个 draft 必须被跳过。
	dres, err := e.maint.Prune(ctx, ops.PruneRequest{Scope: store.ScopeUser, Actor: "maintenance"})
	if err != nil {
		t.Fatalf("Prune drafts: %v", err)
	}
	if dres.Status != ops.RunApplied {
		t.Fatalf("Prune 状态=%s, want applied", dres.Status)
	}
	if got := e.countRows(`SELECT COUNT(1) FROM concepts WHERE status='draft'`); got != stillDraft {
		t.Errorf("Prune 后 draft 数=%d, want %d（草稿表必须不无界增长）", got, stillDraft)
	}
	if got := e.countRows(`SELECT COUNT(1) FROM concepts WHERE status='certified'`); got != promoted {
		t.Errorf("Prune 误删了 certified 概念：剩余 %d, want %d", got, promoted)
	}
	if got := e.countRows(`SELECT COUNT(1) FROM concepts`); got != promoted+stillDraft {
		t.Errorf("概念总数=%d, want %d", got, promoted+stillDraft)
	}
	// 被引用的 draft 必须留下来，并且删不掉的原因必须**具体可读**。
	//
	// 【T-2 修复】原断言只检查 `len(w) > 0`：任何无关 warning 都能让它变绿，
	// 甚至"删掉了但顺手报个别的不相干警告"也能通过。现在要求 warning 点名
	// 那个被引用的 draft ID，且说明原因是"引用"——变异成"删了"就必红。
	var sawRefWarning bool
	for _, s := range dres.Steps {
		for _, w := range s.Diff.Warnings {
			if strings.Contains(w, "引用") && strings.Contains(w, "c-010") {
				sawRefWarning = true
			}
		}
	}
	if !sawRefWarning {
		t.Error("被引用的 draft 被跳过时必须上报**点名该 draft**且说明原因（引用）的 Warnings（不得静默）")
	}

	// 由于有 80 个 draft 被清理，下一次 Prune 不应再有任何 draft 可删（收敛）。
	again, err := e.maint.Prune(ctx, ops.PruneRequest{Scope: store.ScopeUser, Actor: "maintenance"})
	if err != nil {
		t.Fatalf("Prune 第二次: %v", err)
	}
	for _, s := range again.Steps {
		if s.Diff.Changed && s.Step.Kind == "prune_draft_concept" {
			t.Errorf("第二次 Prune 仍删掉了 draft %q（未收敛）", s.Step.Subject)
		}
	}
}

// TestPromoteMakesConceptParticipateInHardContract 断言 Promote 后该概念**开始参与硬契约**（R2）。
//
// 判据用召回侧的对齐结果：draft 的 StatusFactor 打折（0.6），certified 为 1.0。
// 这比"库里 status 字符串变了"强得多——它证明的是召回行为真的变了。
func TestPromoteMakesConceptParticipateInHardContract(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	// 经写入路径产生一个隐式 draft。
	e.putMemory(memory("m-1", store.ScopeUser, "c-auth"))
	c, err := e.db.Store().GetConcept(ctx, "c-auth")
	if err != nil {
		t.Fatalf("GetConcept: %v", err)
	}
	if c.Status != store.ConceptDraft || c.Origin != store.ConceptOriginImplicit {
		t.Fatalf("隐式概念 status=%q origin=%q, want draft/implicit", c.Status, c.Origin)
	}
	// 命名要能在 query 里对齐（否则 StatusFactor 无从比较）。
	c.Name = "认证服务"
	if err := e.db.Store().PutConcept(ctx, c); err != nil {
		t.Fatalf("PutConcept: %v", err)
	}

	before := alignStatusFactor(t, e, "认证服务")
	if before >= 1.0 {
		t.Fatalf("draft 概念的 StatusFactor=%v, want < 1.0（草稿必须打折）", before)
	}

	if _, err := e.maint.PromoteConcept(ctx, ops.PromoteRequest{
		Scope: store.ScopeUser, ConceptIDs: []string{"c-auth"}, Actor: "reviewer",
	}); err != nil {
		t.Fatalf("PromoteConcept: %v", err)
	}

	after := alignStatusFactor(t, e, "认证服务")
	if after != 1.0 {
		t.Errorf("certified 概念的 StatusFactor=%v, want 1.0（Promote 后应参与硬契约）", after)
	}
	if after <= before {
		t.Errorf("Promote 后 StatusFactor 没有提升：%v → %v", before, after)
	}

	// origin 必须保留（D15：认证后仍需保留"最初是隐式的"这条审计线索）。
	got, err := e.db.Store().GetConcept(ctx, "c-auth")
	if err != nil {
		t.Fatalf("GetConcept after promote: %v", err)
	}
	if got.Status != store.ConceptCertified {
		t.Errorf("status=%q, want certified", got.Status)
	}
	if got.Origin != store.ConceptOriginImplicit {
		t.Errorf("origin=%q, want implicit（认证不得抹掉来源）", got.Origin)
	}
}

// TestPromoteIsIdempotentAndRejectsDeprecated 断言 Promote 的边界：
// 已 certified 是空转，deprecated 不得复活。
func TestPromoteIsIdempotentAndRejectsDeprecated(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, c := range []store.Concept{
		{ID: "c-cert", Name: "已认证", Status: store.ConceptCertified},
		{ID: "c-dep", Name: "已弃用", Status: store.ConceptDeprecated},
	} {
		if err := e.db.Store().PutConcept(ctx, c); err != nil {
			t.Fatalf("PutConcept(%s): %v", c.ID, err)
		}
	}

	res, err := e.maint.PromoteConcept(ctx, ops.PromoteRequest{
		Scope: store.ScopeUser, ConceptIDs: []string{"c-cert"},
	})
	if err != nil {
		t.Fatalf("promote certified: %v", err)
	}
	if res.Status != ops.RunNoop {
		t.Errorf("已 certified 的 promote 状态=%s, want noop（幂等）", res.Status)
	}

	res, err = e.maint.PromoteConcept(ctx, ops.PromoteRequest{
		Scope: store.ScopeUser, ConceptIDs: []string{"c-dep"},
	})
	if err != nil {
		t.Fatalf("promote deprecated: %v", err)
	}
	if res.Status != ops.RunNoop {
		t.Errorf("deprecated 的 promote 状态=%s, want noop（不得复活）", res.Status)
	}
	if st := e.scalar(`SELECT status FROM concepts WHERE id='c-dep'`); st != "deprecated" {
		t.Errorf("deprecated 概念状态被改成 %q", st)
	}
}

// alignStatusFactor 用召回侧 Aligner 对齐一次 query，返回该概念的对齐因子。
func alignStatusFactor(t *testing.T, e *env, query string) float64 {
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
	t.Fatalf("query %q 没有对齐到任何概念（matched=%+v）", query, al.Matched)
	return 0
}
