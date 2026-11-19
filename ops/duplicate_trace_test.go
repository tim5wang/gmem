package ops_test

import (
	"context"
	"testing"
	"time"

	"github.com/tim5wang/gmem/ops"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// F-7 收口：duplicate 路径必须可追溯，且**不新增任何行**
//
// 旧实现的形状：命中幂等 key 时 `dup = true; return nil`，随后 `case dup:` 直接返回。
// 它既不走 UpdateRun 也不走 InsertRun，因此 audit_log 与 op_runs **两边都看不到**——
// 「为什么这一次什么都没发生」在生产里完全无从回答。
//
// 约束是硬的双向：
//
//	internal/p3verify/idempotent_test.go:76  duplicate 后 audit_log 行数不变
//	internal/p3verify/idempotent_test.go:79  duplicate 后 op_runs 行数不变
//
// 所以痕迹只能写回**它自己那一行**：skip_count+1、last_skipped_at=now（只 UPDATE）。
// ===========================================================================

// TestDuplicateLeavesTraceWithoutNewRows 是 F-7 的收官回归：同一 key 连跑 3 次。
//
// 断言四件事（缺一条这个洞就会以另一种形状回来）：
//  1. 行数不动：audit_log 与 op_runs 都不新增（受保护用例的同一口径）；
//  2. skip_count 累加到 2（1 次 applied + 2 次 duplicate）——去掉累加就会红；
//  3. last_skipped_at 严格递增（第二次跳过必须晚于第一次）；
//  4. last_skipped_at 晚于 started_at（否则读出来是"跳过发生在运行之前"，无法解释因果）。
func TestDuplicateLeavesTraceWithoutNewRows(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	e.putMemory(memory("m-dup-trace", store.ScopeUser, ""))
	req := ops.Request{
		Op: ops.OpInvalidate, Scope: store.ScopeUser,
		Trigger: ops.Trigger{Kind: ops.TriggerAgent, Actor: "a1", Reason: "r1"},
		Input:   ops.Input{Invalidate: &ops.InvalidateInput{MemoryID: "m-dup-trace", TInvalid: 5000}},
	}

	first, err := e.runner.Run(ctx, req)
	if err != nil {
		t.Fatalf("Run#1: %v", err)
	}
	if first.Status != ops.RunApplied {
		t.Fatalf("Run#1 状态=%s, want applied", first.Status)
	}

	// 快照：这两条行数在两次 duplicate 之后必须一字不动。
	auditBefore := e.countRows(`SELECT COUNT(1) FROM audit_log`)
	runsBefore := e.countRows(`SELECT COUNT(1) FROM op_runs`)

	// 第二次：duplicate。推进时钟，让 last_skipped_at 可以与 started_at 区分开。
	e.clk.Advance(2 * time.Second)
	second, err := e.runner.Run(ctx, req)
	if err != nil {
		t.Fatalf("Run#2: %v", err)
	}
	if second.Status != ops.RunDuplicate {
		t.Fatalf("Run#2 状态=%s, want duplicate", second.Status)
	}
	skipAfterSecond := e.runSkipCount(second.Key)
	if skipAfterSecond != 1 {
		t.Fatalf("第一次跳过之后 skip_count=%d, want 1（duplicate 必须留下痕迹）", skipAfterSecond)
	}
	lastSkippedAfterSecond := e.runLastSkippedAt(second.Key)

	// 第三次：同一 key，再跳过一次。
	e.clk.Advance(2 * time.Second)
	third, err := e.runner.Run(ctx, req)
	if err != nil {
		t.Fatalf("Run#3: %v", err)
	}
	if third.Status != ops.RunDuplicate {
		t.Fatalf("Run#3 状态=%s, want duplicate", third.Status)
	}
	if third.Key != first.Key {
		t.Fatalf("三次运行的 key 不一致：%q / %q / %q（幂等摘要不稳定）",
			first.Key, second.Key, third.Key)
	}

	// 1) 行数不变：duplicate 不得插 audit 行、不得插台账行。
	if got := e.countRows(`SELECT COUNT(1) FROM audit_log`); got != auditBefore {
		t.Errorf("两次 duplicate 之后 audit_log=%d, want %d（不得插审计行）", got, auditBefore)
	}
	if got := e.countRows(`SELECT COUNT(1) FROM op_runs`); got != runsBefore {
		t.Errorf("两次 duplicate 之后 op_runs=%d, want %d（不得插台账行）", got, runsBefore)
	}

	// 2) skip_count 累加到 2。
	skipCount := e.runSkipCount(first.Key)
	if skipCount != 2 {
		t.Errorf("skip_count=%d, want 2（1 次 applied + 2 次 duplicate）", skipCount)
	}

	// 3) last_skipped_at 递增。
	lastSkipped := e.runLastSkippedAt(first.Key)
	if lastSkipped <= lastSkippedAfterSecond {
		t.Errorf("last_skipped_at 没有递增：%d → %d（第二次跳过必须晚于第一次）",
			lastSkippedAfterSecond, lastSkipped)
	}

	// 4) last_skipped_at 晚于 started_at（因果顺序：先跑过，后来才被跳过）。
	startedAt := e.runStartedAt(first.Key)
	if lastSkipped <= startedAt {
		t.Errorf("last_skipped_at=%d 不晚于 started_at=%d（跳过发生在运行之前，无法解释因果）",
			lastSkipped, startedAt)
	}

	// applied 那次运行的结论不得被跳过痕迹改写。
	var status string
	if err := e.raw.QueryRow(`SELECT status FROM op_runs WHERE key = ?`, first.Key).Scan(&status); err != nil {
		t.Fatalf("读 op_runs.status: %v", err)
	}
	if status != string(ops.RunApplied) {
		t.Errorf("原运行的 status=%q, want applied（跳过不得改写主结论）", status)
	}
}

// TestDuplicateTraceIsPerKey 断言跳过计数只落在被跳过的那一个 key 上。
//
// 为什么要单独一条：把 skip_count 写成"全表 +1"或写错 WHERE 也会让主用例变绿
// （它只看一个 key），但那等于把台账变成噪声。
func TestDuplicateTraceIsPerKey(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	for _, id := range []string{"m-dup-a", "m-dup-b"} {
		e.putMemory(memory(id, store.ScopeUser, ""))
	}
	reqA := ops.Request{
		Op: ops.OpInvalidate, Scope: store.ScopeUser,
		Trigger: ops.Trigger{Kind: ops.TriggerAgent, Actor: "a", Reason: "r"},
		Input:   ops.Input{Invalidate: &ops.InvalidateInput{MemoryID: "m-dup-a", TInvalid: 5000}},
	}
	reqB := reqA
	reqB.Input = ops.Input{Invalidate: &ops.InvalidateInput{MemoryID: "m-dup-b", TInvalid: 5000}}

	firstA, err := e.runner.Run(ctx, reqA)
	if err != nil {
		t.Fatalf("Run A: %v", err)
	}
	firstB, err := e.runner.Run(ctx, reqB)
	if err != nil {
		t.Fatalf("Run B: %v", err)
	}

	e.clk.Advance(2 * time.Second)
	if res, err := e.runner.Run(ctx, reqA); err != nil || res.Status != ops.RunDuplicate {
		t.Fatalf("重跑 A: status=%s err=%v, want duplicate", res.Status, err)
	}

	if got := e.runSkipCount(firstA.Key); got != 1 {
		t.Errorf("被跳过的 key 的 skip_count=%d, want 1", got)
	}
	// B 从未被跳过：它的计数必须仍是 0（写错 WHERE 会在这里暴露）。
	var skipB, lastB int64
	if err := e.raw.QueryRow(`SELECT skip_count, last_skipped_at FROM op_runs WHERE key = ?`,
		firstB.Key).Scan(&skipB, &lastB); err != nil {
		t.Fatalf("读 B 的台账: %v", err)
	}
	if skipB != 0 || lastB != 0 {
		t.Errorf("未被跳过的 key 的计数=(%d, %d), want (0, 0)（跳过计数必须按 key 隔离）", skipB, lastB)
	}
}

// ---------------------------------------------------------------------------
// 台账列读取（直接从库里读，不走 ops 的封装：断言的是存下来的事实）
// ---------------------------------------------------------------------------

// runSkipCount 读该 key 的 skip_count。
func (e *env) runSkipCount(key string) int64 {
	e.t.Helper()
	var n int64
	if err := e.raw.QueryRow(`SELECT skip_count FROM op_runs WHERE key = ?`, key).Scan(&n); err != nil {
		e.t.Fatalf("读 skip_count（key=%s）: %v", key, err)
	}
	return n
}

// runLastSkippedAt 读该 key 的 last_skipped_at。
func (e *env) runLastSkippedAt(key string) int64 {
	e.t.Helper()
	var n int64
	if err := e.raw.QueryRow(`SELECT last_skipped_at FROM op_runs WHERE key = ?`, key).Scan(&n); err != nil {
		e.t.Fatalf("读 last_skipped_at（key=%s）: %v", key, err)
	}
	return n
}

// runStartedAt 读该 key 的 started_at。
func (e *env) runStartedAt(key string) int64 {
	e.t.Helper()
	var n int64
	if err := e.raw.QueryRow(`SELECT started_at FROM op_runs WHERE key = ?`, key).Scan(&n); err != nil {
		e.t.Fatalf("读 started_at（key=%s）: %v", key, err)
	}
	return n
}
