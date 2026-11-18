package p3verify

import (
	"context"
	"testing"
	"time"

	"github.com/tim5wang/gmem/ops"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// 攻击面 5：ctx 取消不留半截状态 + 幂等台账不被脏写入卡住
// ===========================================================================

// TestCancelMidRunLeavesNoPartialState 断言事务中途取消时业务变更整体回滚。
//
// 形状：让 Extractor 在处理第 2 条 episode 时 cancel ctx。此时第 1 条已经被
// Apply 过（写了 memories + 状态行 + 审计），必须**一起回滚**——
// 否则库里出现"审计说有 1 条变更、但那次运行被标成 cancelled"的自相矛盾。
func TestCancelMidRunLeavesNoPartialState(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	const n = 3
	for i := 0; i < n; i++ {
		if err := e.db.Store().PutEpisode(ctx, store.Episode{
			ID: "ep-" + itoa(i), Scope: store.ScopeUser,
			Content: "第 " + itoa(i) + " 条语料正文", At: int64(100 + i),
		}); err != nil {
			t.Fatalf("PutEpisode: %v", err)
		}
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// 第 2 次调用时取消：第 1 条已 Apply，第 2 条会在提取时看到 ctx 已取消。
	e.ext.onCall = func(call int, ep ops.EpisodeInput) {
		if call == 2 {
			cancel()
		}
	}

	res, err := e.runner.Run(runCtx, ops.Request{Op: ops.OpExtract, Scope: store.ScopeUser})
	if err == nil {
		// 见 TestCanceledRunReturnsNilErrorIsFinding：cancel 分支可能返回 nil。
		if res.Status != ops.RunCancelled {
			t.Fatalf("预期 cancelled，实际 status=%s err=nil", res.Status)
		}
	}
	if res.Status != ops.RunCancelled {
		t.Fatalf("状态=%s, want cancelled", res.Status)
	}
	if len(res.Steps) != 0 {
		t.Errorf("取消的运行返回了 %d 个步骤（应整体回滚，不留半截）", len(res.Steps))
	}

	// 关键断言：第 1 条 episode 提取出的记忆必须**不存在**（同事务回滚）。
	if got := e.count("SELECT COUNT(1) FROM memories"); got != 0 {
		t.Errorf("取消后仍有 %d 条记忆落库（半截状态：部分 episode 已写入）", got)
	}
	if got := e.count("SELECT COUNT(1) FROM memories_fts"); got != 0 {
		t.Errorf("取消后仍有 %d 条 FTS 行（派生索引与主表不一致）", got)
	}
	if got := e.count("SELECT COUNT(1) FROM memory_state"); got != 0 {
		t.Errorf("取消后仍有 %d 条状态行", got)
	}
	if got := e.count("SELECT COUNT(1) FROM audit_log"); got != 0 {
		t.Errorf("取消后仍有 %d 条审计（审计与业务变更必须同生共死）", got)
	}

	// 台账：该 key 应被标成 cancelled（不是卡在 running）。
	var status string
	if err := e.raw.QueryRow("SELECT status FROM op_runs WHERE key = ?", res.Key).Scan(&status); err != nil {
		t.Fatalf("取消运行未留台账（key=%s）: %v", res.Key, err)
	}
	if status != string(ops.RunCancelled) {
		t.Errorf("台账状态=%q, want cancelled（卡在 running 会让后续重跑永久被挡）", status)
	}
}

// TestCancelDoesNotBlockRetry 断言取消后同一 key 可以重跑（台账不卡死）。
func TestCancelDoesNotBlockRetry(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.db.Store().PutEpisode(ctx, store.Episode{
		ID: "ep-r", Scope: store.ScopeUser, Content: "一条语料正文", At: 100,
	}); err != nil {
		t.Fatalf("PutEpisode: %v", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	e.ext.onCall = func(int, ops.EpisodeInput) { cancel() }
	first, err := e.runner.Run(runCtx, ops.Request{Op: ops.OpExtract, Scope: store.ScopeUser})
	if err != nil && first.Status != ops.RunCancelled {
		t.Fatalf("首次运行异常：status=%s err=%v", first.Status, err)
	}
	if first.Status != ops.RunCancelled {
		t.Fatalf("首次运行状态=%s, want cancelled", first.Status)
	}

	// 清掉注入的取消副作用，用同一 key 正常重跑。
	e.ext.onCall = nil
	retry, err := e.runner.Run(ctx, ops.Request{
		Op: ops.OpExtract, Scope: store.ScopeUser, Key: first.Key,
	})
	if err != nil {
		t.Fatalf("重跑: %v", err)
	}
	if retry.Status == ops.RunDuplicate {
		t.Fatalf("取消后的重跑被判 duplicate（台账被脏状态卡死，该窗口永不执行）")
	}
	if retry.Status != ops.RunApplied {
		t.Errorf("重跑状态=%s, want applied", retry.Status)
	}
	if got := e.count("SELECT COUNT(1) FROM memories WHERE id='mem-ep-r'"); got != 1 {
		t.Errorf("重跑后记忆数=%d, want 1（重跑没有真正执行）", got)
	}
}

// ===========================================================================
// 攻击面 6：D15 draft 清理在 100+ 规模下是否真的有效
// ===========================================================================

// TestD15HundredDraftsStayBounded 是 D15 的规模回归：
// 100 个隐式 draft → Promote 一批 + Prune 一批 → 断言草稿表**不无界增长**，
// 且 certified 一个不少、仍被引用的 draft 一个不删。
func TestD15HundredDraftsStayBounded(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	const (
		total      = 100
		promoted   = 15 // 显式认证
		referenced = 15 // 仍是 draft，但被活跃记忆引用 → 不可删
		// 其余 70 个：记忆被物理删除后的孤儿 draft → 可清理
	)
	orphans := total - promoted - referenced

	// 造 100 条记忆，各带一个隐式 draft（真经写入路径 → ensureConcept 落 draft）。
	for i := 0; i < total; i++ {
		m := mem("m-"+pad(i), store.ScopeUser, "c-"+pad(i))
		m.TValid, m.TCreated = e.clk.Now().Unix(), e.clk.Now().Unix()
		e.put(m)
	}
	if got := e.count("SELECT COUNT(1) FROM concepts WHERE status='draft'"); got != total {
		t.Fatalf("隐式 draft 数=%d, want %d", got, total)
	}
	if got := e.count("SELECT COUNT(1) FROM concepts WHERE origin='implicit'"); got != total {
		t.Fatalf("origin=implicit 数=%d, want %d", got, total)
	}

	// (a) Promote 一批：断言变 certified 且参与硬契约。
	probe := "c-" + pad(0)
	c, err := e.db.Store().GetConcept(ctx, probe)
	if err != nil {
		t.Fatalf("GetConcept: %v", err)
	}
	// 给第一个一个可在 query 里对齐的名字，用于 StatusFactor 断言。
	c.Name = "认证服务"
	if err := e.db.Store().PutConcept(ctx, c); err != nil {
		t.Fatalf("PutConcept: %v", err)
	}
	draftFactor := statusFactorOf(t, e, "认证服务")
	if draftFactor >= 1.0 {
		t.Fatalf("draft 的 StatusFactor=%v, want < 1.0", draftFactor)
	}

	var promoteIDs []string
	for i := 0; i < promoted; i++ {
		promoteIDs = append(promoteIDs, "c-"+pad(i))
	}
	pres, err := e.maint.PromoteConcept(ctx, ops.PromoteRequest{
		Scope: store.ScopeUser, ConceptIDs: promoteIDs, Actor: "reviewer",
	})
	if err != nil {
		t.Fatalf("PromoteConcept: %v", err)
	}
	if pres.Status != ops.RunApplied {
		t.Fatalf("Promote 状态=%s, want applied", pres.Status)
	}
	if got := e.count("SELECT COUNT(1) FROM concepts WHERE status='certified'"); got != promoted {
		t.Errorf("certified 数=%d, want %d", got, promoted)
	}
	// (a) 第二条：Promote 后开始参与硬契约（不再打折）。
	if f := statusFactorOf(t, e, "认证服务"); f != 1.0 {
		t.Errorf("Promote 后 StatusFactor=%v, want 1.0（未参与硬契约）", f)
	}

	// 把 70 个孤儿 draft 的记忆物理删除：只能走合法生命周期
	// （Consolidate→Active → Invalidate → 时钟推进 → Consolidate→Archived → Prune）。
	for i := promoted + referenced; i < total; i++ {
		if _, err := e.maint.Supersede(ctx, ops.SupersedeRequest{
			Scope: store.ScopeUser, MemoryID: "m-" + pad(i),
		}); err != nil {
			t.Fatalf("Supersede m-%s: %v", pad(i), err)
		}
	}
	// 保留期满 → Archived。
	e.clk.Advance(time.Duration(e.cfg.ArchiveAfterDays+1) * 24 * time.Hour)
	e.mustRun(ops.Request{
		Op: ops.OpConsolidate, Scope: store.ScopeUser,
		Trigger: ops.Trigger{Kind: ops.TriggerIdle, Actor: "idle", Reason: "保留期满归档"},
	})
	if got := e.count("SELECT COUNT(1) FROM memory_state WHERE status='archived'"); got != orphans {
		t.Fatalf("archived 状态行=%d, want %d（生命周期没跑到预期状态）", got, orphans)
	}
	// Prune 记忆（先不碰 draft，看到中间状态）。
	e.mustRun(ops.Request{Op: ops.OpPrune, Scope: store.ScopeUser,
		Input: ops.Input{IncludeDrafts: new(bool)}})
	if got := e.count("SELECT COUNT(1) FROM memories"); got != total-orphans {
		t.Fatalf("物理删除后记忆数=%d, want %d", got, total-orphans)
	}

	// (b) Prune 带 DraftTTL：旧孤儿 draft 被删，新/被引用的保留，certified 一条不删。
	// 所有 draft 的 last_used_at 默认为 0（未知=很久以前），因此 TTL=1s 就能清。
	before := e.count("SELECT COUNT(1) FROM concepts WHERE status='draft'")
	dres, err := e.maint.Prune(ctx, ops.PruneRequest{
		Scope: store.ScopeUser, Actor: "maintenance", DraftTTLSeconds: 1,
	})
	if err != nil {
		t.Fatalf("Prune drafts: %v", err)
	}
	if dres.Status != ops.RunApplied {
		t.Fatalf("Prune drafts 状态=%s, want applied", dres.Status)
	}
	after := e.count("SELECT COUNT(1) FROM concepts WHERE status='draft'")
	if after != referenced {
		t.Errorf("Prune 后 draft 数=%d, want %d（before=%d）：草稿表必须只留下仍被引用的",
			after, referenced, before)
	}
	if got := e.count("SELECT COUNT(1) FROM concepts WHERE status='certified'"); got != promoted {
		t.Errorf("Prune 误删 certified：剩 %d, want %d", got, promoted)
	}
	// 被引用的 draft 一个都不能少（否则 concept_id 悬空）。
	for i := promoted; i < promoted+referenced; i++ {
		if got := e.count("SELECT COUNT(1) FROM concepts WHERE id = ?", "c-"+pad(i)); got != 1 {
			t.Errorf("仍被引用的 draft c-%s 被删除（留下悬空 concept_id）", pad(i))
		}
	}

	// (c) 重复跑 Promote/Prune：不报错、不重复删。
	e.clk.Advance(time.Second)
	if _, err := e.maint.Prune(ctx, ops.PruneRequest{
		Scope: store.ScopeUser, DraftTTLSeconds: 1,
	}); err != nil {
		t.Errorf("重复 Prune 报错: %v", err)
	}
	e.clk.Advance(time.Second)
	if _, err := e.maint.PromoteConcept(ctx, ops.PromoteRequest{
		Scope: store.ScopeUser, ConceptIDs: promoteIDs,
	}); err != nil {
		t.Errorf("重复 Promote 报错: %v", err)
	}
	if got := e.count("SELECT COUNT(1) FROM concepts WHERE status='certified'"); got != promoted {
		t.Errorf("重复 Promote 后 certified=%d, want %d（不重复应用）", got, promoted)
	}
	if got := e.count("SELECT COUNT(1) FROM concepts WHERE status='draft'"); got != referenced {
		t.Errorf("重复 Prune 后 draft=%d, want %d", got, referenced)
	}

	// 总量收束：concepts 表不得无界（certified + 被引用 draft）。
	if got := e.count("SELECT COUNT(1) FROM concepts"); got != promoted+referenced {
		t.Errorf("concepts 总数=%d, want %d（草稿漂移未收束）", got, promoted+referenced)
	}
}

// pad 把 0..99 补成三位（id 排序稳定，便于断言）。
func pad(i int) string {
	s := itoa(i)
	for len(s) < 3 {
		s = "0" + s
	}
	return s
}
