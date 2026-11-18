package p3verify

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tim5wang/gmem/ops"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// 攻击面 3：幂等（TD §8：幂等 key = hash(scope, op_name, 输入摘要)，重跑安全）
//
// "重跑安全"有两层，缺一不可：
//   - **不重复应用**：同一 key 第二次必须返回 RunDuplicate，且不产生第二次业务变更；
//   - **不误挡真实工作**：输入真的变了（新窗口 / 新目标）就必须是新的一次运行。
//
// 只测前者会掩盖"永不执行"；只测后者会掩盖"每轮都跑"。本文件两边都测。
// ===========================================================================

// TestIdempotentRerunSkipsWithoutDoubleWrite 断言同 key 重跑被跳过且不留双写。
//
// 行级断言（不只是返回值）：memories 版本数、memory_state 行数、audit_log 的
// **变更行数**都不能增长。为什么看 audit_log 的变更行：noop 步骤也会写审计
// （那是"为什么没变化"的证据），真正不该多的是 changed=1 的那些。
func TestIdempotentRerunSkipsWithoutDoubleWrite(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	m := mem("m-idem", store.ScopeUser, "")
	m.TValid, m.TCreated = e.clk.Now().Unix(), e.clk.Now().Unix()
	e.put(m)

	req := ops.Request{
		Op: ops.OpInvalidate, Scope: store.ScopeUser,
		Trigger: ops.Trigger{Kind: ops.TriggerAgent, Actor: "a1", Reason: "r1"},
		Input:   ops.Input{Invalidate: &ops.InvalidateInput{MemoryID: "m-idem", TInvalid: 5000}},
	}
	first, err := e.runner.Run(ctx, req)
	if err != nil {
		t.Fatalf("Run#1: %v", err)
	}
	if first.Status != ops.RunApplied {
		t.Fatalf("Run#1 状态=%s, want applied", first.Status)
	}

	// 快照（重跑必须一字不动）。
	versionsBefore := e.count(`SELECT COUNT(1) FROM memories WHERE id='m-idem'`)
	changedBefore := e.count(`SELECT COUNT(1) FROM audit_log WHERE changed=1`)
	auditBefore := e.countRows("audit_log")
	runsBefore := e.countRows("op_runs")
	tInvalidBefore := e.str(`SELECT t_invalid FROM memories WHERE id='m-idem' AND t_expired IS NULL`)

	second, err := e.runner.Run(ctx, req)
	if err != nil {
		t.Fatalf("Run#2: %v", err)
	}
	if second.Status != ops.RunDuplicate {
		t.Fatalf("重跑状态=%s, want duplicate（同 key 必须安全跳过）", second.Status)
	}
	if second.Key != first.Key {
		t.Errorf("重跑的 key=%q 与原 key=%q 不同（幂等摘要不稳定）", second.Key, first.Key)
	}
	if len(second.Steps) != 0 {
		t.Errorf("重跑产生了 %d 个步骤，want 0", len(second.Steps))
	}

	if got := e.count(`SELECT COUNT(1) FROM memories WHERE id='m-idem'`); got != versionsBefore {
		t.Errorf("重跑后记忆版本数=%d, want %d（不得双写）", got, versionsBefore)
	}
	if got := e.count(`SELECT COUNT(1) FROM audit_log WHERE changed=1`); got != changedBefore {
		t.Errorf("重跑后变更审计=%d, want %d（不得多记一条应用记录）", got, changedBefore)
	}
	// noop 步骤的审计也不该新增：RunDuplicate 在 Plan 之前就返回了。
	if got := e.countRows("audit_log"); got != auditBefore {
		t.Errorf("重跑后审计总数=%d, want %d（duplicate 不应进入 Apply）", got, auditBefore)
	}
	if got := e.countRows("op_runs"); got != runsBefore {
		t.Errorf("重跑后 op_runs=%d, want %d（台账不应多一行）", got, runsBefore)
	}
	if got := e.str(`SELECT t_invalid FROM memories WHERE id='m-idem' AND t_expired IS NULL`); got != tInvalidBefore {
		t.Errorf("t_invalid 被重跑改写：%q → %q（基准不能漂移）", tInvalidBefore, got)
	}
}

// TestScanOpSameWindowSkippedNewWindowWorks 断言扫描型 op 的窗口语义。
//
// 这条是"整理是否能持续发生"的命门：
//   - 同一窗口重跑 → 跳过（否则一次交互里重复整理，成本翻倍）
//   - 新窗口 → 新工作（否则第一次之后**永远**不再整理）
func TestScanOpSameWindowSkippedNewWindowWorks(t *testing.T) {
	e := newEnv(t)

	// 第一轮：一条记忆 → Consolidate 推进到 Active。
	m := mem("m-win", store.ScopeUser, "")
	m.TValid, m.TCreated = e.clk.Now().Unix(), e.clk.Now().Unix()
	e.put(m)

	r1 := e.mustRun(ops.Request{Op: ops.OpConsolidate, Scope: store.ScopeUser})
	if r1.Status != ops.RunApplied {
		t.Fatalf("首轮状态=%s, want applied", r1.Status)
	}
	status1 := e.str(`SELECT status FROM memory_state WHERE memory_id='m-win'`)
	if status1 != string(ops.StatusActive) {
		t.Fatalf("首轮后状态=%q, want active", status1)
	}

	// 同一窗口（时钟未推进）重跑 → duplicate。
	r2 := e.mustRun(ops.Request{Op: ops.OpConsolidate, Scope: store.ScopeUser})
	if r2.Status != ops.RunDuplicate {
		t.Fatalf("同窗口重跑状态=%s, want duplicate", r2.Status)
	}
	if r2.Key != r1.Key {
		t.Errorf("同窗口的 key 不同：%q vs %q（窗口未参与摘要）", r2.Key, r1.Key)
	}

	// 推进时钟 = 新窗口 → 必须是一次真正的新运行（不是 duplicate）。
	e.clk.Advance(time.Hour)
	// 造一件新工作：一条新记忆需要推进状态。
	m2 := mem("m-win-2", store.ScopeUser, "")
	m2.TValid, m2.TCreated = e.clk.Now().Unix(), e.clk.Now().Unix()
	e.put(m2)

	r3 := e.mustRun(ops.Request{Op: ops.OpConsolidate, Scope: store.ScopeUser})
	if r3.Status == ops.RunDuplicate {
		t.Fatalf("新窗口仍被判为 duplicate（整理会永远不再发生）")
	}
	if r3.Key == r1.Key {
		t.Errorf("新窗口的 key 与旧窗口相同：%q（window 未参与幂等摘要）", r3.Key)
	}
	if got := e.str(`SELECT status FROM memory_state WHERE memory_id='m-win-2'`); got != string(ops.StatusActive) {
		t.Errorf("新窗口运行未推进新记忆：m-win-2 status=%q, want active", got)
	}
	if n := e.countRows("op_runs WHERE op='consolidate'"); n != 2 {
		t.Errorf("consolidate 运行行数=%d, want 2（首轮 + 新窗口）", n)
	}
}

// TestNonScanOpConstantKeyBlocksLaterWork 是【可后续】的最小可复现用例。
//
// # 现象
//
// 非扫描型 op（Merge/LinkUpdate/Invalidate/Promote）的幂等 key 只由输入决定
// （runner.go 的 scanOps 表刻意不含它们）。对 Merge/Invalidate 这是对的：
// "同输入 = 同一件事"。但对**Promote{All:true}** 不是：
//
//	Promote(All=true) 的输入是常量（{all:true}），所以 key 也是常量。
//	第一次运行把当时的 draft 认证完；之后新长出来的 draft 再调 Promote(All=true)，
//	输入仍然一模一样 → 被判 RunDuplicate → **新草稿永远不会被认证**。
//
// # 生产形状
//
// 宿主定时跑 `PromoteConcept(All: true, MinUse: 1)` 作为 D15 的自动清理。
// 第一次之后它静默变成空操作，草稿表重新开始无界增长——正是 D15 要防的事，
// 而且没有任何报错（RunDuplicate 是"成功"返回值）。
//
// # 判定
//
//   - 期望语义：Promote/Prune 这类**作用域是"当下集合"**的 op，其幂等摘要
//     应含窗口（与扫描型 op 同口径），或 All 模式不参与幂等缓存。
//   - 默认不红（避免掩盖新回归），GMEM_VERIFY_DEFERRED=1 时红。
func TestNonScanOpConstantKeyBlocksLaterWork(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	// 第一批：2 个隐式 draft（真经写入路径产生），并登记为"在用"。
	// concept_usage 只由 Extract 的 Apply 填充（ops_scan.go 的 TouchConceptUsage），
	// 因此直接经 Writer 写入的 draft 需要显式登记，否则 Promote(All) 的 MinUse 门槛
	// 永远看不到它们——这本身也是一条发现，见 TestPromoteAllMissesDirectlyWrittenDrafts。
	for i := 0; i < 2; i++ {
		cid := "c-batch1-" + itoa(i)
		m := mem("m-batch1-"+itoa(i), store.ScopeUser, cid)
		m.TValid, m.TCreated = e.clk.Now().Unix(), e.clk.Now().Unix()
		e.put(m)
		if err := e.db.OpsBackend().TouchConceptUsage(ctx, cid, e.clk.Now().UnixMilli()); err != nil {
			t.Fatalf("TouchConceptUsage(%s): %v", cid, err)
		}
	}
	if got := e.count(`SELECT COUNT(1) FROM concepts WHERE status='draft'`); got != 2 {
		t.Fatalf("第一批 draft 数=%d, want 2", got)
	}

	r1, err := e.maint.PromoteConcept(ctx, ops.PromoteRequest{
		Scope: store.ScopeUser, All: true, MinUse: 1, Actor: "janitor",
	})
	if err != nil {
		t.Fatalf("Promote#1: %v", err)
	}
	if r1.Status != ops.RunApplied {
		t.Fatalf("Promote#1 状态=%s, want applied", r1.Status)
	}
	cert1 := e.count(`SELECT COUNT(1) FROM concepts WHERE status='certified'`)
	if cert1 != 2 {
		t.Fatalf("Promote#1 后 certified=%d, want 2", cert1)
	}

	// 第二批：时间推进（真实世界里是后来的对话），3 个新 draft。
	e.clk.Advance(24 * time.Hour)
	for i := 0; i < 3; i++ {
		cid := "c-batch2-" + itoa(i)
		m := mem("m-batch2-"+itoa(i), store.ScopeUser, cid)
		m.TValid, m.TCreated = e.clk.Now().Unix(), e.clk.Now().Unix()
		e.put(m)
		if err := e.db.OpsBackend().TouchConceptUsage(ctx, cid, e.clk.Now().UnixMilli()); err != nil {
			t.Fatalf("TouchConceptUsage(%s): %v", cid, err)
		}
	}
	if got := e.count(`SELECT COUNT(1) FROM concepts WHERE status='draft'`); got != 3 {
		t.Fatalf("第二批 draft 数=%d, want 3", got)
	}

	r2, err := e.maint.PromoteConcept(ctx, ops.PromoteRequest{
		Scope: store.ScopeUser, All: true, MinUse: 1, Actor: "janitor",
	})
	if err != nil {
		t.Fatalf("Promote#2: %v", err)
	}

	certedNow := e.count(`SELECT COUNT(1) FROM concepts WHERE status='certified'`)
	draftsLeft := e.count(`SELECT COUNT(1) FROM concepts WHERE status='draft'`)
	if r2.Status != ops.RunDuplicate || draftsLeft == 0 {
		// 期望行为：第二次 All 认证必须覆盖新 draft。
		if draftsLeft != 0 {
			t.Logf("Promote#2 状态=%s，仍剩 %d 个 draft", r2.Status, draftsLeft)
		}
		return
	}

	msg := "Promote(All=true) 第二次被判 RunDuplicate（状态=" + string(r2.Status) +
		"）：输入是常量，幂等 key 因此也是常量，后来新增的 " + itoa(draftsLeft) +
		" 个隐式 draft 永远不会被自动认证（certified=" + itoa(certedNow) + "）。" +
		"宿主把 Promote(All) 当定时清理时，第一次之后就静默变成空操作。"
	if deferredGate() {
		t.Fatal(msg)
	}
	t.Log("【可后续 / GMEM_VERIFY_DEFERRED=1 时红】" + msg)
}

// TestFailedRunCanBeRetried 断言 failed 的运行不把该窗口永久锁死。
//
// 为什么关键：一次瞬时故障（磁盘满、外部依赖 5xx）若把 key 标成"已完成"，
// 这个窗口就永远不再整理——而且没有任何信号。Runner 的注释明确说
// failed/cancelled 允许重跑，这里验证它真的成立。
//
// 注意用**普通 error** 而不是 context.DeadlineExceeded：后者会被 Runner 判为
// cancelled（那条路径见 TestCanceledRunReturnsNilErrorIsFinding）。两条路径的
// 台账形态不同，混用会让用例测不到 failed 分支。
func TestFailedRunCanBeRetried(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	// 用一个会失败的 Extractor 造出 failed 运行。
	boom := ops.ExtractFunc(func(context.Context, ops.EpisodeInput) (ops.ExtractOutcome, error) {
		return ops.ExtractOutcome{}, errors.New("模拟外部抽取服务 503")
	})
	r, err := ops.NewRunner(e.db.Ports(), e.db.OpsBackend(),
		ops.WithConfig(e.cfg), ops.WithExtractor(boom))
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	if err := e.db.Store().PutEpisode(ctx, store.Episode{
		ID: "ep-fail", Scope: store.ScopeUser, Content: "需要提取的语料正文", At: e.clk.Now().Unix(),
	}); err != nil {
		t.Fatalf("PutEpisode: %v", err)
	}

	first, err := r.Run(ctx, ops.Request{Op: ops.OpExtract, Scope: store.ScopeUser})
	if err == nil {
		t.Fatalf("预期 Extract 失败（Extractor 返回 error），实际成功：%+v", first)
	}
	if first.Status != ops.RunFailed {
		t.Fatalf("失败运行的状态=%s, want failed", first.Status)
	}

	// 台账里该 key 必须存在且不是"成功"形态（否则会被当成已完成而跳过）。
	var status string
	if err := e.raw.QueryRow(`SELECT status FROM op_runs WHERE key = ?`, first.Key).Scan(&status); err != nil {
		t.Fatalf("失败运行未在台账里留痕（key=%s）: %v", first.Key, err)
	}
	if status == string(ops.RunApplied) || status == string(ops.RunNoop) {
		t.Fatalf("失败运行被记成 %q（会永久跳过该窗口）", status)
	}

	// 同一个 key 用正常 Extractor 重跑 → 必须真的执行（不是 duplicate）。
	retry, err := e.runner.Run(ctx, ops.Request{Op: ops.OpExtract, Scope: store.ScopeUser, Key: first.Key})
	if err != nil {
		t.Fatalf("重试: %v", err)
	}
	if retry.Status == ops.RunDuplicate {
		t.Fatalf("失败运行的重试被判 duplicate（一次瞬时故障就锁死了这个窗口）")
	}
	if got := e.count(`SELECT COUNT(1) FROM memories WHERE id='mem-ep-fail'`); got != 1 {
		t.Errorf("重试后提取出的记忆数=%d, want 1（重试没有真正执行）", got)
	}
}

// TestCanceledRunReturnsNilErrorIsFinding 是【可后续】的最小可复现用例。
//
// # 现象
//
// op 内部返回 cancel 形状的错误（context.Canceled / DeadlineExceeded）时，
// Runner 走 cancelled 分支，最后 `return res, ctx.Err()`——用的是**外层 ctx**。
// 外层 ctx 没取消（是 op 自己因预算/内部超时放弃的），所以 `ctx.Err()` 是 nil：
//
//	status=cancelled, err=nil
//
// # 为什么这是缺陷
//
// 对调用方而言 `err == nil` 等于"跑完了"，但实际上什么都没做（事务已回滚）。
// 宿主的典型写法 `if err != nil { log/retry }` 会静默跳过这次整理；
// 而 Res=RunCancelled 又需要调用方额外检查才看得到。
//
// # 判定
//
// 期望：status=cancelled 时应返回非 nil 错误（携带真实原因），
// 或文档明确要求调用方必须检查 Status 而非 err。
// 默认不红，GMEM_VERIFY_DEFERRED=1 时红。
func TestCanceledRunReturnsNilErrorIsFinding(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.db.Store().PutEpisode(ctx, store.Episode{
		ID: "ep-cancel", Scope: store.ScopeUser, Content: "正文", At: 1,
	}); err != nil {
		t.Fatalf("PutEpisode: %v", err)
	}

	// op 自己放弃（预算/内部超时），外层 ctx 完全正常。
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
		t.Fatalf("前置不成立：状态=%s, want cancelled", res.Status)
	}
	if runErr != nil {
		return // 期望行为已成立
	}

	msg := "op 内部放弃（返回 context.DeadlineExceeded）时，Run 返回 status=cancelled 但 err=nil：" +
		"调用方按 `if err != nil` 判断会以为成功，实际上事务已回滚、什么都没做"
	if deferredGate() {
		t.Fatal(msg)
	}
	t.Log("【可后续 / GMEM_VERIFY_DEFERRED=1 时红】" + msg)
}
