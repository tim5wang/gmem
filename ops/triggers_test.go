package ops_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tim5wang/gmem/ops"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// 5) 去抖：after=N + 同 key 取消重排（LangMem ReflectionExecutor 语义）
// ===========================================================================

// TestDebounceCollapsesBurstIntoOneRun 是去抖的核心回归：
// 连续 N 次写入**只触发 1 次**整理，且执行时刻是**最后一次写入 + after**。
func TestDebounceCollapsesBurstIntoOneRun(t *testing.T) {
	e := newEnv(t, func(c *ops.Config) { c.AfterWrite = 400 * time.Millisecond })
	ctx := context.Background()
	key := string(store.ScopeUser)

	// 连续 5 次写入，每次间隔 100ms（< after），最后再等 300ms 仍不该触发。
	for i := 0; i < 5; i++ {
		e.trig.AfterWrite(ctx, store.ScopeUser, ops.OpConsolidate, ops.Input{})
		e.sched.Advance(100 * time.Millisecond)
		if fired, _ := e.trig.Debouncer().Stats(key); fired != 0 {
			t.Fatalf("第 %d 次写入后就触发了（去抖失效：这是「每轮都跑」）", i+1)
		}
	}

	// 时间线：第 5 次（最后一次）写入发生在 t0+400ms，其到期时刻是 t0+800ms。
	// 循环结束时时钟在 t0+500ms（最后一次写入后又推进了 100ms）。
	// 再推 200ms → t0+700ms（距最后一次写入 300ms < 400ms），仍不该执行。
	e.sched.Advance(200 * time.Millisecond)
	if fired, _ := e.trig.Debouncer().Stats(key); fired != 0 {
		t.Fatalf("最后一次写入后 300ms 就触发了（早于 after=400ms）")
	}
	if got := e.countRows(`SELECT COUNT(1) FROM op_runs`); got != 0 {
		t.Fatalf("尚未到 after 时间就有 %d 次运行", got)
	}

	// 再推 100ms → t0+800ms（距最后一次写入正好 400ms = after），此时才执行，且只一次。
	e.sched.Advance(100 * time.Millisecond)
	fired, canceled := e.trig.Debouncer().Stats(key)
	if fired != 1 {
		t.Fatalf("连续 5 次写入触发了 %d 次，want 1（去抖必须合并）", fired)
	}
	if canceled != 4 {
		t.Errorf("被取消重排的次数=%d, want 4（前 4 次都应被后一次取消）", canceled)
	}
	if got := e.countRows(`SELECT COUNT(1) FROM op_runs`); got != 1 {
		t.Errorf("op_runs 行数=%d, want 1（只跑了一次整理）", got)
	}

	// 不再有任何待触发的登记（「永不执行」的另一个极端同样不允许）。
	e.sched.Advance(10 * time.Second)
	if pending := e.sched.Pending(); pending != 0 {
		t.Errorf("仍有 %d 个待触发定时器", pending)
	}
	if firedAfter, _ := e.trig.Debouncer().Stats(key); firedAfter != 1 {
		t.Errorf("安静之后又触发了 %d 次（应只触发 1 次）", firedAfter-1)
	}
}

// TestDebounceCancelPreventsRun 断言 Agent 打断时定时器不再触发（可取消到底）。
func TestDebounceCancelPreventsRun(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	cancel := e.trig.AfterWrite(ctx, store.ScopeUser, ops.OpConsolidate, ops.Input{})
	cancel()
	e.sched.Advance(10 * time.Second)
	if fired, _ := e.trig.Debouncer().Stats(string(store.ScopeUser)); fired != 0 {
		t.Errorf("已取消的去抖仍触发了 %d 次", fired)
	}
	if got := e.countRows(`SELECT COUNT(1) FROM op_runs`); got != 0 {
		t.Errorf("取消后仍产生 %d 次运行", got)
	}
}

// TestDebounceSeparateKeysDoNotMerge 断言去抖键是隔离的（不同 scope 不互相取消）。
func TestDebounceSeparateKeysDoNotMerge(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.trig.AfterWrite(ctx, store.ScopeUser, ops.OpConsolidate, ops.Input{})
	e.trig.AfterWrite(ctx, store.ScopeAgent, ops.OpConsolidate, ops.Input{})
	if pending := e.sched.Pending(); pending != 2 {
		t.Fatalf("两个不同 key 的待触发数=%d, want 2", pending)
	}
	e.sched.Advance(time.Second)
	if u, _ := e.trig.Debouncer().Stats(string(store.ScopeUser)); u != 1 {
		t.Errorf("user key 触发 %d 次, want 1", u)
	}
	if a, _ := e.trig.Debouncer().Stats(string(store.ScopeAgent)); a != 1 {
		t.Errorf("agent key 触发 %d 次, want 1", a)
	}
}

// ===========================================================================
// 6) 四种触发源都能触发同一个 op，且行为一致
// ===========================================================================

// TestFourTriggerSourcesProduceSameBehavior 用同一个 op（Consolidate）跑四条路径，
// 断言落库结果（状态推进 + 审计口径）一致，只有 trigger_kind/actor 不同。
func TestFourTriggerSourcesProduceSameBehavior(t *testing.T) {
	type outcome struct {
		statuses map[string]string
		kinds    map[string]int
	}
	run := func(t *testing.T, setup func(e *env, ctx context.Context), fire func(e *env, ctx context.Context)) outcome {
		t.Helper()
		e := newEnv(t)
		ctx := context.Background()
		setup(e, ctx)
		fire(e, ctx)

		out := outcome{statuses: map[string]string{}, kinds: map[string]int{}}
		rows, err := e.raw.Query(`SELECT memory_id, status FROM memory_state ORDER BY memory_id`)
		if err != nil {
			t.Fatalf("读 memory_state: %v", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id, st string
			if err := rows.Scan(&id, &st); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out.statuses[id] = st
		}
		krows, err := e.raw.Query(`SELECT trigger_kind, COUNT(1) FROM audit_log GROUP BY trigger_kind ORDER BY trigger_kind`)
		if err != nil {
			t.Fatalf("读 audit_log: %v", err)
		}
		defer krows.Close()
		for krows.Next() {
			var k string
			var n int
			if err := krows.Scan(&k, &n); err != nil {
				t.Fatalf("scan kinds: %v", err)
			}
			out.kinds[k] = n
		}
		return out
	}

	setup := func(e *env, ctx context.Context) {
		e.putMemory(memory("m-a", store.ScopeUser, ""))
		e.putMemory(store.Memory{
			ID: "m-short", Scope: store.ScopeUser, Kind: store.KindSemantic,
			Content: "太短", TValid: 100,
		})
	}

	afterWrite := run(t, setup, func(e *env, ctx context.Context) {
		e.trig.AfterWrite(ctx, store.ScopeUser, ops.OpConsolidate, ops.Input{})
		e.sched.Advance(10 * time.Second)
	})
	threshold := run(t, setup, func(e *env, ctx context.Context) {
		// 把阈值降到 1（两条记忆），让阈值触发必然成立。
		e2 := e
		e2.cfg.Threshold = 1
		r, err := ops.NewRunner(e2.db.Ports(), e2.db.OpsBackend(),
			ops.WithConfig(e2.cfg),
			ops.WithExtractor(e2.ext))
		if err != nil {
			t.Fatalf("NewRunner: %v", err)
		}
		trig := ops.NewTriggers(r)
		fired, _, err := trig.Threshold(ctx, store.ScopeUser, ops.OpConsolidate, ops.Input{})
		if err != nil {
			t.Fatalf("threshold: %v", err)
		}
		if !fired {
			t.Fatalf("阈值触发未触发（未整理条目已达 Threshold）")
		}
	})
	idle := run(t, setup, func(e *env, ctx context.Context) {
		if _, err := e.trig.Idle(ctx, store.ScopeUser, ops.OpConsolidate, ops.Input{}, ops.TriggerIdle); err != nil {
			t.Fatalf("idle: %v", err)
		}
	})
	agent := run(t, setup, func(e *env, ctx context.Context) {
		if _, err := e.maint.Consolidate(ctx, ops.ConsolidateRequest{Scope: store.ScopeUser}); err != nil {
			t.Fatalf("agent consolidate: %v", err)
		}
	})

	for name, got := range map[string]outcome{
		"after_write": afterWrite, "threshold": threshold, "idle": idle, "agent": agent,
	} {
		if len(got.statuses) != 2 {
			t.Errorf("%s: 状态行数=%d, want 2（两条记忆都要有条目）", name, len(got.statuses))
		}
		if got.statuses["m-a"] != string(ops.StatusActive) {
			t.Errorf("%s: m-a 状态=%q, want active", name, got.statuses["m-a"])
		}
		if got.statuses["m-short"] != string(ops.StatusRejected) {
			t.Errorf("%s: m-short 状态=%q, want rejected", name, got.statuses["m-short"])
		}
	}

	// 四种触发源在审计里的 trigger_kind 各自可辨（"谁触发的"必须可回答）。
	for name, want := range map[string]string{
		"after_write": string(ops.TriggerAfterWrite), "threshold": string(ops.TriggerThreshold),
		"idle": string(ops.TriggerIdle), "agent": string(ops.TriggerAgent),
	} {
		var got outcome
		switch name {
		case "after_write":
			got = afterWrite
		case "threshold":
			got = threshold
		case "idle":
			got = idle
		case "agent":
			got = agent
		}
		if got.kinds[want] == 0 {
			t.Errorf("%s 的审计里没有 trigger_kind=%q 的记录（%v）", name, want, got.kinds)
		}
	}
}

// ===========================================================================
// 7) audit_log：能重建"谁在何时因何触发了什么变更"
// ===========================================================================

// TestAuditLogReconstructsWhoWhenWhy 断言审计证据链完整：
// op_id ↔ op_runs、trigger/actor/reason、时间单调、before/after diff 可读。
func TestAuditLogReconstructsWhoWhenWhy(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.db.Store().PutEpisode(ctx, store.Episode{
		ID: "ep-1", Scope: store.ScopeUser, Content: "原始语料正文", At: 100,
	}); err != nil {
		t.Fatalf("put episode: %v", err)
	}
	m := memory("m-1", store.ScopeUser, "")
	m.TValid, m.TCreated = 100, 100
	e.putMemory(m)

	res, err := e.runner.Run(ctx, ops.Request{
		Op: ops.OpInvalidate, Scope: store.ScopeUser,
		Trigger: ops.Trigger{Kind: ops.TriggerAgent, Actor: "agent-42", Reason: "用户说这条已作废"},
		Input:   ops.Input{Invalidate: &ops.InvalidateInput{MemoryID: "m-1", TInvalid: 500}},
	})
	if err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	if res.Key == "" {
		t.Fatal("运行没有幂等 key")
	}

	// op_runs 与 audit_log 必须能按 op_id 对上（"这一次运行做了什么"）。
	var (
		runOp, runStatus, runActor, runReason, runTrigger string
	)
	if err := e.raw.QueryRow(`SELECT op, status, actor, reason, trigger_kind FROM op_runs WHERE key = ?`, res.Key).
		Scan(&runOp, &runStatus, &runActor, &runReason, &runTrigger); err != nil {
		t.Fatalf("读 op_runs(%s): %v", res.Key, err)
	}
	if runOp != string(ops.OpInvalidate) || runStatus != string(ops.RunApplied) {
		t.Errorf("op_runs: op=%q status=%q", runOp, runStatus)
	}
	if runActor != "agent-42" {
		t.Errorf("op_runs.actor=%q, want agent-42（谁）", runActor)
	}
	if runReason != "用户说这条已作废" {
		t.Errorf("op_runs.reason=%q（为什么）", runReason)
	}
	if runTrigger != string(ops.TriggerAgent) {
		t.Errorf("op_runs.trigger_kind=%q（怎么触发的）", runTrigger)
	}

	entries, err := e.maint.Audit(ctx, ops.AuditQuery{OpID: res.Key})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("审计条数=%d, want 1", len(entries))
	}
	a := entries[0]
	if a.Actor != "agent-42" || a.Reason != "用户说这条已作废" || a.Trigger != ops.TriggerAgent {
		t.Errorf("审计元数据不完整：%+v", a)
	}
	if a.At <= 0 {
		t.Errorf("审计时间=%d（必须落库，否则「何时」无从回答）", a.At)
	}
	if !a.Changed {
		t.Errorf("审计 changed=false，但这次确实改了 t_invalid")
	}
	if !strings.Contains(a.BeforeJSON, `"t_invalid":0`) {
		t.Errorf("before 快照=%q，应记录变更前状态", a.BeforeJSON)
	}
	if !strings.Contains(a.AfterJSON, `"t_invalid":500`) {
		t.Errorf("after 快照=%q，应记录变更后状态", a.AfterJSON)
	}
	if a.Subject != "m-1" {
		t.Errorf("审计 subject=%q, want m-1（改了什么）", a.Subject)
	}

	// 按 subject 检索同样能重建这条记忆的完整变更史。
	bySubject, err := e.maint.Audit(ctx, ops.AuditQuery{Subject: "m-1"})
	if err != nil {
		t.Fatalf("Audit(subject): %v", err)
	}
	if len(bySubject) != 1 {
		t.Errorf("按 subject 检索到 %d 条, want 1", len(bySubject))
	}

	// op 台账也能按 scope/op 查（P4 的 History API 会用到）。
	runs, err := e.maint.Runs(ctx, ops.RunQuery{Scope: store.ScopeUser, Op: ops.OpInvalidate})
	if err != nil {
		t.Fatalf("Runs: %v", err)
	}
	if len(runs) != 1 || runs[0].Key != res.Key {
		t.Errorf("op 台账检索结果=%v", runs)
	}
}

// TestAuditRecordsNoopWithZeroDiff 断言空转也被审计（否则"为什么没变化"无法回答）。
func TestAuditRecordsNoopWithZeroDiff(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// 世界时间取"刚刚"：否则按 30 天半衰期会立刻衰减到 Stale，
	// 第二次整理就会因为合法的状态推进而产生 diff（那就不是空转用例了）。
	fresh := memory("m-1", store.ScopeUser, "")
	fresh.TValid = e.clk.Now().Unix()
	fresh.TCreated = e.clk.Now().Unix()
	e.putMemory(fresh)

	// 先跑一次推进到 active，再跑一次（第二次必然全是空转）。
	if _, err := e.runner.Run(ctx, ops.Request{Op: ops.OpConsolidate, Scope: store.ScopeUser}); err != nil {
		t.Fatalf("consolidate #1: %v", err)
	}
	before := e.countRows(`SELECT COUNT(1) FROM audit_log`)
	e.clk.Advance(time.Second)
	if _, err := e.runner.Run(ctx, ops.Request{Op: ops.OpConsolidate, Scope: store.ScopeUser}); err != nil {
		t.Fatalf("consolidate #2: %v", err)
	}
	after := e.countRows(`SELECT COUNT(1) FROM audit_log`)
	if after != before {
		t.Errorf("第二次整理本应全是空转，却新增了 %d 条审计（伪造 diff）", after-before)
	}
}
