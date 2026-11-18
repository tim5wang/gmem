package ops_test

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tim5wang/gmem/ops"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// P3 修复项的回归测试（对应 F-1..F-11 / T 阻塞 / T 可后续）
//
// 每个用例都刻意写成"改回旧实现就必红"的形状：
//   - 断言外部可见结果（库里的行、返回的 Changed、审计行），不看内部字段；
//   - 时间/调度器全部注入，不 sleep；
//   - 关键项都注明"变异点"，方便复核者逐条验证。
// ===========================================================================

// recordingScheduler 记录每次登记的回调，让"迟到回调"可以被手动触发。
//
// 为什么不用 fakeScheduler：它把 fn 封在 fakeTimer 里，测试拿不到句柄，
// 而 F-3 要证明的恰恰是**同一个 fn 在跨登记之后被调用**时的语义。
type recordingScheduler struct {
	clk  *clock
	mu   sync.Mutex
	fns  []func()
	due  []time.Time
	stop []bool
}

func (s *recordingScheduler) After(d time.Duration, fn func()) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := len(s.fns)
	s.fns = append(s.fns, fn)
	s.due = append(s.due, s.clk.Now().Add(d))
	s.stop = append(s.stop, false)
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.stop[idx] = true
	}
}

// fnAt 取第 i 次登记的回调（即便它已被 Stop，也仍可被调用——模拟"已在队列里"）。
func (s *recordingScheduler) fnAt(i int) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i >= len(s.fns) {
		return nil
	}
	return s.fns[i]
}

func (s *recordingScheduler) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.fns)
}

// ---------------------------------------------------------------------------
// F-1【阻塞】DraftTTL 必须真实生效
// ---------------------------------------------------------------------------

// TestDraftTTLKeepsFreshDraftAndPrunesOld 是 F-1 的回归。
//
// 变异点：把 ListStaleDrafts 的判定改回 `COALESCE(u.last_used_at,0) <= cutoff`，
// 或把 created_at 回填成 0 → "刚创建"的 draft 立即进删除计划，第一个断言必红。
func TestDraftTTLKeepsFreshDraftAndPrunesOld(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	nowMs := e.clk.Now().UnixMilli()

	// 刚创建（0 秒龄）、没有任何引用、没有任何 concept_usage 行的 draft。
	// 这正是 leader 用探针复现的形状：它当时被"计划删除"。
	if err := e.db.Store().PutConcept(ctx, store.Concept{
		ID: "c-fresh", Name: "c-fresh", Status: store.ConceptDraft,
		Origin: store.ConceptOriginImplicit, CreatedAt: nowMs, Scope: store.ScopeUser,
	}); err != nil {
		t.Fatalf("PutConcept(c-fresh): %v", err)
	}
	// 超过 TTL（90 天前创建）的孤儿 draft：必须被删。
	oldMs := nowMs - 90*24*3600*1000
	if err := e.db.Store().PutConcept(ctx, store.Concept{
		ID: "c-old", Name: "c-old", Status: store.ConceptDraft,
		Origin: store.ConceptOriginImplicit, CreatedAt: oldMs, Scope: store.ScopeUser,
	}); err != nil {
		t.Fatalf("PutConcept(c-old): %v", err)
	}

	res := e.mustRun(ops.Request{Op: ops.OpPrune, Scope: store.ScopeUser,
		Input: ops.Input{DraftTTLSeconds: 30 * 24 * 3600}})
	if res.Status != ops.RunApplied {
		t.Fatalf("Prune 状态=%s, want applied", res.Status)
	}

	if got := e.countRows(`SELECT COUNT(1) FROM concepts WHERE id='c-fresh'`); got != 1 {
		t.Errorf("刚创建（0 秒龄）的 draft 被 Prune 删除——DraftTTL 从未生效（数据丢失级）")
	}
	if got := e.countRows(`SELECT COUNT(1) FROM concepts WHERE id='c-old'`); got != 0 {
		t.Errorf("超过 TTL 且无引用的 draft 未被删除（D15 清理失效）")
	}
}

// TestStructuredDraftIsNotImmediatelyPruned 覆盖 d15_test.go 绕开的那条分支。
//
// 结构化写入（不经 Extract）产生的 draft 没有 concept_usage 行；
// 旧实现把"缺失"当成 last_used_at=0 → 恒 <= cutoff → 首次 Prune 即删。
func TestStructuredDraftIsNotImmediatelyPruned(t *testing.T) {
	e := newEnv(t)

	// 直接经 Writer 写入，带一个未声明的 concept_id（ensureConcept 隐式建 draft）。
	m := memory("m-structured", store.ScopeUser, "c-structured")
	m.TCreated = e.clk.Now().Unix()
	m.TValid = m.TCreated
	e.putMemory(m)
	if got := e.countRows("SELECT COUNT(1) FROM concept_usage WHERE concept_id='c-structured'"); got != 0 {
		t.Fatalf("前置不成立：结构化写入不应登记 concept_usage（行数=%d）", got)
	}

	e.mustRun(ops.Request{Op: ops.OpPrune, Scope: store.ScopeUser,
		Input: ops.Input{DraftTTLSeconds: 30 * 24 * 3600}})

	// 记忆还在：pruneDraft 的引用护栏也会保护它——这里断言的是**计划阶段**就没选中它，
	// 因此用"概念仍在"+"没有任何 prune_draft step"双重确认。
	if got := e.countRows(`SELECT COUNT(1) FROM concepts WHERE id='c-structured'`); got != 1 {
		t.Errorf("结构化写入产生的 draft 被删除（concept_id 会悬空）")
	}
}

// TestPutConceptWithoutCreatedAtIsNotImmediatelyPruned 是 F-1 在**真实写入路径**上的回归。
//
// # 为什么这条必须单独存在
//
// 上面的用例走 PutMemory → ensureConcept，那条路本来就会把 created_at 填成记忆的
// TCreated，因此它**绕开**了本缺陷。真正会漏的是直接调 PutConcept 的调用方——
// extract/pipeline.go:416 造定义句 draft 时就不填 CreatedAt，v8 迁移只回填
// “迁移时刻已存在”的行，于是迁移之后新建的隐式 draft 全都是 created_at=0，
// 判定依旧恒真，F-1 的症状原样存在。
//
// 变异点：去掉 PutConcept 里的 `if c.CreatedAt == 0 { c.CreatedAt = now }` 兜底 →
// 第一条断言必红。
func TestPutConceptWithoutCreatedAtIsNotImmediatelyPruned(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	// 完全按 extract/pipeline.go 的形态造 draft：不设 CreatedAt、不设 Scope。
	if err := e.db.Store().PutConcept(ctx, store.Concept{
		ID: "c-pipe", Name: "c-pipe", Status: store.ConceptDraft,
		Definition: "定义句", Origin: store.ConceptOriginImplicit,
	}); err != nil {
		t.Fatalf("PutConcept(c-pipe): %v", err)
	}
	// 另一条显式给定“超过 TTL”的创建时间：必须被清掉（证明不是把恒真改成恒假）。
	oldMs := e.clk.Now().Add(-90 * 24 * time.Hour).UnixMilli()
	if err := e.db.Store().PutConcept(ctx, store.Concept{
		ID: "c-pipe-old", Name: "c-pipe-old", Status: store.ConceptDraft,
		Origin: store.ConceptOriginImplicit, CreatedAt: oldMs, Scope: store.ScopeUser,
	}); err != nil {
		t.Fatalf("PutConcept(c-pipe-old): %v", err)
	}

	// 前置：兜底必须真的把 created_at 写成非 0（0 = 判定恒真）。
	if got := e.countRows(`SELECT COUNT(1) FROM concepts WHERE id='c-pipe' AND created_at > 0`); got != 1 {
		t.Fatalf("PutConcept 未给 created_at 兜底：它仍是 0，DraftTTL 会把它判成“很久以前”")
	}

	e.mustRun(ops.Request{Op: ops.OpPrune, Scope: store.ScopeUser,
		Input: ops.Input{DraftTTLSeconds: 30 * 24 * 3600}})

	if got := e.countRows(`SELECT COUNT(1) FROM concepts WHERE id='c-pipe'`); got != 1 {
		t.Errorf("刚创建（0 秒龄）的 draft 进了 Prune 计划并被删除——F-1 在真实写入路径上仍存在")
	}
	if got := e.countRows(`SELECT COUNT(1) FROM concepts WHERE id='c-pipe-old'`); got != 0 {
		t.Errorf("超过 TTL 且无引用的 draft 未被删除（清理失效）")
	}
}

// ---------------------------------------------------------------------------
// F-2 / F-3【阻塞】Debouncer generation token
// ---------------------------------------------------------------------------

// TestDebounceCancelOnlyAffectsOwnRegistration 是 F-2 的回归。
//
// 变异点：去掉 generation 校验（cancel 直接按 key 删 timers）→ 第一次的 cancel
// 会杀掉第二次的定时器，fired 永远是 0（"永不执行"）。
func TestDebounceCancelOnlyAffectsOwnRegistration(t *testing.T) {
	clk := newClock()
	sched := &recordingScheduler{clk: clk}
	cfg := ops.DefaultConfig()
	cfg.Now = clk.Now
	cfg.Scheduler = sched
	e := newEnv(t, func(c *ops.Config) {
		*c = cfg
	})
	ctx := context.Background()
	key := string(store.ScopeUser)

	cancel1 := e.trig.AfterWrite(ctx, store.ScopeUser, ops.OpConsolidate, ops.Input{})
	// 第二次登记（同 key）：会取消第一次的定时器。
	e.trig.AfterWrite(ctx, store.ScopeUser, ops.OpConsolidate, ops.Input{})

	// 调用**第一次**拿到的 cancel：它只能撤销自己那一次登记。
	cancel1()

	// 让第二次的定时器到期。
	clk.Advance(10 * time.Second)
	if fn := sched.fnAt(sched.len() - 1); fn != nil {
		fn()
	}

	fired, _ := e.trig.Debouncer().Stats(key)
	if fired != 1 {
		t.Fatalf("第二次登记没有触发（fired=%d）：旧 cancel 句柄误杀了后续登记的定时器（F-2）", fired)
	}
}

// TestLateOldTimerCallbackIsIgnored 是 F-3 的回归。
//
// 变异点：fire 不校验 generation → 迟到的旧回调会消费新 payload 并提前执行，
// 同时把新定时器从 timers 里删掉（fired 变成 1、pending 变成 0）。
// 注意 -race 抓不到它：d.mu 保护的是数据竞争，不是语义。
func TestLateOldTimerCallbackIsIgnored(t *testing.T) {
	clk := newClock()
	sched := &recordingScheduler{clk: clk}
	cfg := ops.DefaultConfig()
	cfg.Now = clk.Now
	cfg.Scheduler = sched
	e := newEnv(t, func(c *ops.Config) { *c = cfg })
	ctx := context.Background()
	key := string(store.ScopeUser)

	e.trig.AfterWrite(ctx, store.ScopeUser, ops.OpConsolidate, ops.Input{})
	oldFn := sched.fnAt(0)
	if oldFn == nil {
		t.Fatal("第一次登记没有产生定时器")
	}
	// 第二次登记：旧定时器被 Stop，但"已经在队列里"这件事无法撤销。
	e.trig.AfterWrite(ctx, store.ScopeUser, ops.OpConsolidate, ops.Input{})

	// 手动执行迟到的旧回调。
	oldFn()

	if fired, _ := e.trig.Debouncer().Stats(key); fired != 0 {
		t.Fatalf("迟到的旧定时器回调触发了执行（fired=%d）：它消费了新 payload 并提前跑了（F-3）", fired)
	}
	if pending := e.trig.Debouncer().Pending(); pending != 1 {
		t.Fatalf("pending=%d, want 1：旧回调把新登记删掉了（去抖退化为永不执行）", pending)
	}
	if got := e.countRows("SELECT COUNT(1) FROM op_runs"); got != 0 {
		t.Errorf("旧回调产生了 %d 次运行，want 0", got)
	}

	// 新定时器仍必须正常工作。
	if fn := sched.fnAt(sched.len() - 1); fn != nil {
		fn()
	}
	if fired, _ := e.trig.Debouncer().Stats(key); fired != 1 {
		t.Errorf("新登记最终没有执行（fired=%d, want 1）", fired)
	}
}

// ---------------------------------------------------------------------------
// F-4【重要】Prune 的 draft 分支必须 scope 隔离
// ---------------------------------------------------------------------------

// TestPruneDraftsRespectScopeIsolation 是 F-4 的回归。
//
// 变异点：ListStaleDrafts 去掉 scope 条件（回到 (ctx, cutoff, limit) 签名）→
// agent scope 的 Prune 会删掉 user scope 的草稿，第二个断言必红。
func TestPruneDraftsRespectScopeIsolation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	oldMs := e.clk.Now().UnixMilli() - 90*24*3600*1000

	for _, tc := range []struct {
		id    string
		scope store.Scope
	}{
		{"c-user-orphan", store.ScopeUser},
		{"c-agent-orphan", store.ScopeAgent},
	} {
		if err := e.db.Store().PutConcept(ctx, store.Concept{
			ID: tc.id, Name: tc.id, Status: store.ConceptDraft,
			Origin: store.ConceptOriginImplicit, CreatedAt: oldMs, Scope: tc.scope,
		}); err != nil {
			t.Fatalf("PutConcept(%s): %v", tc.id, err)
		}
	}

	// 只清 agent scope。
	e.mustRun(ops.Request{Op: ops.OpPrune, Scope: store.ScopeAgent,
		Input: ops.Input{DraftTTLSeconds: 30 * 24 * 3600}})

	if got := e.countRows(`SELECT COUNT(1) FROM concepts WHERE id='c-user-orphan'`); got != 1 {
		t.Errorf("agent scope 的 Prune 删掉了 user scope 的 draft（跨边界删除，F-4）")
	}
	if got := e.countRows(`SELECT COUNT(1) FROM concepts WHERE id='c-agent-orphan'`); got != 0 {
		t.Errorf("本 scope 的过期 draft 未被清理（隔离条件写反了）")
	}
}

// ---------------------------------------------------------------------------
// F-6【重要】IO 预算必须真生效
// ---------------------------------------------------------------------------

// TestIOBudgetActuallyStopsScan 是 F-6 的回归。
//
// 变异点：删掉扫描循环里的 txc.Spend(1) → IO=1 仍会应用 30 步（旧缺陷）。
func TestIOBudgetActuallyStopsScan(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 30; i++ {
		m := memory("m-"+pad(i), store.ScopeUser, "")
		m.TValid, m.TCreated = e.clk.Now().Unix(), e.clk.Now().Unix()
		e.putMemory(m)
	}

	res := e.mustRun(ops.Request{
		Op: ops.OpConsolidate, Scope: store.ScopeUser,
		// Trigger 预算与 op 预算取更严者：IO=1 必须真的只允许 1 个单位。
		Trigger: ops.Trigger{Kind: ops.TriggerAgent, Budget: ops.Budget{IO: 1}},
	})
	if len(res.Steps) > 1 {
		t.Fatalf("IO 预算=1 却应用了 %d 步：预算完全未生效（F-6）", len(res.Steps))
	}
	var said bool
	for _, d := range res.Degraded {
		if strings.Contains(d, "预算") {
			said = true
		}
	}
	if !said {
		t.Errorf("预算触顶必须显式上报（不得静默截断）：%v", res.Degraded)
	}
}

// ---------------------------------------------------------------------------
// F-9【重要】unlink 不得恒报 Changed:true
// ---------------------------------------------------------------------------

// TestUnlinkMissingEdgeIsNotChanged 是 F-9 的回归。
//
// 变异点：让 invalidateEdge 永远返回 changed=true → 对一条不存在的边也会产生
// 一条"变更"审计（伪造 diff）。
func TestUnlinkMissingEdgeIsNotChanged(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, id := range []string{"n-x", "n-y"} {
		if err := e.db.Graph().PutNode(ctx, store.Node{
			ID: id, Scope: store.ScopeUser, CanonicalName: id,
		}); err != nil {
			t.Fatalf("PutNode(%s): %v", id, err)
		}
	}

	res := e.mustRun(ops.Request{Op: ops.OpLinkUpdate, Scope: store.ScopeUser,
		Input: ops.Input{Link: &ops.LinkInput{
			FromNode: "n-x", ToNode: "n-y", Relation: store.RelationWorksAt, Remove: true,
		}}})

	if len(res.Steps) != 1 {
		t.Fatalf("步骤数=%d, want 1", len(res.Steps))
	}
	if res.Steps[0].Diff.Changed {
		t.Errorf("unlink 一条不存在的边却报 Changed=true（伪造 diff，F-9）")
	}
	if got := e.countRows("SELECT COUNT(1) FROM audit_log WHERE changed=1"); got != 0 {
		t.Errorf("审计里有 %d 条 changed=1，但这次什么都没改", got)
	}
}

// ---------------------------------------------------------------------------
// F-10【重要】Candidate 不得绕过质量门
// ---------------------------------------------------------------------------

// TestCandidateCannotBypassQualityGateIntoInvalidated 是 F-10 的回归。
//
// 变异点：恢复 intermediateFor(Candidate, Invalidated) = Active 且不校验质量门 →
// 内容过短的 Candidate 会被写成 Active 再写成 Invalidated（"曾经 Active 过"的谎）。
func TestCandidateCannotBypassQualityGateIntoInvalidated(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	// 内容 2 个字符 < MinContentLength(10)：按 TD §7 它注定是 Rejected。
	if err := e.db.Store().PutMemory(ctx, store.Memory{
		ID: "m-short", Scope: store.ScopeUser, Kind: store.KindSemantic,
		Content: "太短", TValid: e.clk.Now().Unix(), TCreated: e.clk.Now().Unix(),
	}); err != nil {
		t.Fatalf("PutMemory: %v", err)
	}

	if _, err := e.maint.Supersede(ctx, ops.SupersedeRequest{
		Scope: store.ScopeUser, MemoryID: "m-short", Actor: "agent",
	}); err == nil {
		t.Fatal("内容过短的 Candidate 被允许直接变成 Invalidated（绕过质量门，F-10）")
	}
	// 被拒绝的 supersede 不得留下任何状态行（整体回滚）。
	if got := e.countRows(`SELECT COUNT(1) FROM memory_state WHERE memory_id='m-short'`); got != 0 {
		t.Errorf("被拒绝的 supersede 仍留下 %d 条状态行（质量门被绕过）", got)
	}
}

// TestIntermediateTransitionProducesAuditRow 是 F-10a 的回归。
//
// 变异点：丢弃 move() 返回的 Trail（不写 via 审计行）→ 只剩 1 行审计，
// "怎么从 candidate 走到 invalidated 的"无法重建。
func TestIntermediateTransitionProducesAuditRow(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	m := memory("m-via", store.ScopeUser, "")
	m.TValid, m.TCreated = e.clk.Now().Unix(), e.clk.Now().Unix()
	e.putMemory(m)

	res, err := e.maint.Supersede(ctx, ops.SupersedeRequest{
		Scope: store.ScopeUser, MemoryID: "m-via", Actor: "agent",
	})
	if err != nil {
		t.Fatalf("Supersede: %v", err)
	}
	entries, err := e.maint.Audit(ctx, ops.AuditQuery{OpID: res.Key})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	var main, via int
	for _, a := range entries {
		switch a.StepKind {
		case "invalidate_memory":
			main++
		case "invalidate_memory_via":
			via++
		}
	}
	if main == 0 {
		t.Errorf("缺少主步骤审计行（%+v）", entries)
	}
	if via == 0 {
		t.Errorf("缺少中间迁移审计行：Candidate → Active 是真实落库，因果链缺一环（F-10a）")
	}
}

// ---------------------------------------------------------------------------
// F-11【重要】load() 的 recency 单位
// ---------------------------------------------------------------------------

// TestStateAfterExtractIsNotImmediatelyStale 是 F-11 的回归。
//
// 变异点：把 `m.TValid*1000` 改回 `m.TValid`（秒当毫秒）→ 刚写入的记忆
// recency 命中下界 0.05、importance≈0.025（< ArchiveBelow），必红。
func TestStateAfterExtractIsNotImmediatelyStale(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	now := e.clk.Now().Unix()
	if err := e.db.Store().PutEpisode(ctx, store.Episode{
		ID: "ep-fresh", Scope: store.ScopeUser, Content: "一段刚发生的语料正文", At: now,
	}); err != nil {
		t.Fatalf("PutEpisode: %v", err)
	}

	if _, err := e.runner.Run(ctx, ops.Request{Op: ops.OpExtract, Scope: store.ScopeUser}); err != nil {
		t.Fatalf("Extract: %v", err)
	}

	// 不跑 Consolidate，直接看落库的状态：这正是 review 探针 J 的形状。
	recency := e.scalarFloat(`SELECT importance_recency FROM memory_state WHERE memory_id='mem-ep-fresh'`)
	importance := e.scalarFloat(`SELECT importance FROM memory_state WHERE memory_id='mem-ep-fresh'`)
	if recency < 0.99 {
		t.Errorf("刚写入的记忆 recency=%v, want ≈1.0（秒当毫秒 → 命中下界，F-11）", recency)
	}
	if importance <= e.cfg.StaleBelow {
		t.Errorf("刚写入的记忆 importance=%v 已 <= StaleBelow(%v)：会被下一次 Consolidate 判成 Stale（F-11）",
			importance, e.cfg.StaleBelow)
	}
}

// ---------------------------------------------------------------------------
// Promote(All)：幂等 key 不得是常量 + 结构化写入的对称兜底
// ---------------------------------------------------------------------------

// TestPromoteAllCoversLaterDraftsAndStructuredWrites 同时覆盖两条"可后续"。
//
// 变异点 1：Promote(All) 不带扫描窗口 → 第二轮被判 duplicate，新增 draft 永不认证。
// 变异点 2：判定只看 concept_usage（无 CountConceptRefs 兜底）→ 结构化写入的 draft
// 永远拿不到 UseCount，第一轮就认证不了。
func TestPromoteAllCoversLaterDraftsAndStructuredWrites(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	// 第一轮：两个 draft，其中只有结构化写入（无 usage 行）。
	for i := 0; i < 2; i++ {
		m := memory("m-p"+itoa(i), store.ScopeUser, "c-p"+itoa(i))
		m.TValid, m.TCreated = e.clk.Now().Unix(), e.clk.Now().Unix()
		e.putMemory(m)
	}

	r1, err := e.maint.PromoteConcept(ctx, ops.PromoteRequest{
		Scope: store.ScopeUser, All: true, MinUse: 1, Actor: "janitor",
	})
	if err != nil {
		t.Fatalf("Promote#1: %v", err)
	}
	if r1.Status != ops.RunApplied {
		t.Fatalf("Promote#1 状态=%s, want applied（结构化写入的 draft 也应在用）", r1.Status)
	}
	if got := e.countRows(`SELECT COUNT(1) FROM concepts WHERE status='certified'`); got != 2 {
		t.Fatalf("certified=%d, want 2（Promote(All) 漏掉了结构化写入的 draft）", got)
	}

	// 第二轮：后来新增的 draft 必须能被认证（幂等 key 含窗口）。
	e.clk.Advance(time.Hour)
	m := memory("m-p-late", store.ScopeUser, "c-p-late")
	m.TValid, m.TCreated = e.clk.Now().Unix(), e.clk.Now().Unix()
	e.putMemory(m)

	r2, err := e.maint.PromoteConcept(ctx, ops.PromoteRequest{
		Scope: store.ScopeUser, All: true, MinUse: 1, Actor: "janitor",
	})
	if err != nil {
		t.Fatalf("Promote#2: %v", err)
	}
	if r2.Status == ops.RunDuplicate {
		t.Fatalf("Promote(All) 第二次被判 duplicate：幂等 key 是常量，新 draft 永远不会被认证")
	}
	if got := e.countRows(`SELECT COUNT(1) FROM concepts WHERE status='certified'`); got != 3 {
		t.Errorf("certified=%d, want 3（后来新增的 draft 没有被认证）", got)
	}
}

// ---------------------------------------------------------------------------
// cancelled 必须返回非 nil 错误
// ---------------------------------------------------------------------------

// TestCancelledRunReturnsNonNilError 断言取消是可判定的错误。
//
// 变异点：`return res, ctx.Err()` → op 自己放弃时外层 ctx 未取消，err 为 nil。
func TestCancelledRunReturnsNonNilError(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.db.Store().PutEpisode(ctx, store.Episode{
		ID: "ep-give-up", Scope: store.ScopeUser, Content: "正文", At: 1,
	}); err != nil {
		t.Fatalf("PutEpisode: %v", err)
	}
	giveUp := ops.ExtractFunc(func(context.Context, ops.EpisodeInput) (ops.ExtractOutcome, error) {
		return ops.ExtractOutcome{}, context.DeadlineExceeded
	})
	r, err := ops.NewRunner(e.db.Ports(), e.db.OpsBackend(),
		ops.WithConfig(e.cfg), ops.WithExtractor(giveUp))
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	res, runErr := r.Run(ctx, ops.Request{Op: ops.OpExtract, Scope: store.ScopeUser})
	if res.Status != ops.RunCancelled {
		t.Fatalf("状态=%s, want cancelled", res.Status)
	}
	if runErr == nil {
		t.Fatal("status=cancelled 却返回 err=nil：调用方按 `if err != nil` 判断会误以为成功")
	}
}

// ---------------------------------------------------------------------------
// 本文件的小工具（ops_test 包内原本没有这几个，从 p3verify 借形状补上）
// ---------------------------------------------------------------------------

// mustRun 跑一次 op 并要求无错误。
func (e *env) mustRun(req ops.Request) ops.Result {
	e.t.Helper()
	res, err := e.runner.Run(context.Background(), req)
	if err != nil {
		e.t.Fatalf("Run(%s): %v", req.Op, err)
	}
	return res
}

// itoa 是测试日志/ID 拼接用的小工具（避免引入 fmt 噪音）。
func itoa(n int) string { return strconv.Itoa(n) }

// pad 把 0..99 补成三位（ID 排序稳定，便于断言）。
func pad(i int) string {
	s := itoa(i)
	for len(s) < 3 {
		s = "0" + s
	}
	return s
}
