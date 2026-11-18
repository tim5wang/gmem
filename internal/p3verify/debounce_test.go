package p3verify

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/tim5wang/gmem/ops"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// 攻击面 1：去抖是否**真的只触发一次**（TD §8 · D3 的全部价值）
//
// 实现者的 ops/triggers_test.go 用的是本包不共享的基座，本文件独立重写：
// 断言不看 Debouncer 的自报计数就完事，而是同时看**库里的 op_runs 行**
// 与**执行时刻**——因为错误实现的两种形状都很具体：
//
//	每轮都跑 → op_runs 出现 N 行（LLM/IO 成本 × 对话轮数）
//	永不执行 → op_runs 0 行，且定时器仍挂着（"整理永远不发生"）
//
// 特别注意：本文件的两个用例分别覆盖**假调度器**（确定性时刻断言）
// 与**真实调度器**（time.AfterFunc 的生产路径）。只测假调度器会漏掉
// RealScheduler 的接线错误，只测真实调度器则无法断言"执行时刻"。
// ===========================================================================

// TestDebounceFiresOnceAtLastPlusAfter 是去抖的核心回归。
//
// 时间线（after=400ms，每次提交间隔 100ms）：
//
//	t0        submit#1   due=t0+400
//	t0+100    submit#2   due=t0+500  （#1 被取消）
//	t0+200    submit#3   due=t0+600
//	t0+300    submit#4   due=t0+700
//	t0+400    submit#5   due=t0+800  ← 只有它该活下来
//	t0+799    仍未执行（距最后一次提交 399ms < after）
//	t0+800    执行，且只执行这一次
func TestDebounceFiresOnceAtLastPlusAfter(t *testing.T) {
	const after = 400 * time.Millisecond
	e := newEnv(t, func(c *ops.Config) { c.AfterWrite = after })
	ctx := context.Background()
	key := string(store.ScopeUser)

	start := e.clk.Now()
	var lastSubmit time.Time
	for i := 0; i < 5; i++ {
		e.trig.AfterWrite(ctx, store.ScopeUser, ops.OpConsolidate, ops.Input{})
		lastSubmit = e.clk.Now()
		// 每次提交后推进 100ms，但**不跨过**下一次提交后的到期时刻。
		e.sched.Advance(100 * time.Millisecond)
		if fired, _ := e.trig.Debouncer().Stats(key); fired != 0 {
			t.Fatalf("第 %d 次提交后就执行了 %d 次（去抖失效 = 每轮都跑）", i+1, fired)
		}
		if n := e.countRows("op_runs"); n != 0 {
			t.Fatalf("第 %d 次提交后 op_runs 已有 %d 行（不得早于 after 执行）", i+1, n)
		}
	}
	// 此刻时钟 = t0+500ms（最后一次提交在 t0+400，due=t0+800）。
	lastDue := lastSubmit.Add(after)

	// 距最后一次提交 after-1ms：必须仍未执行。
	e.clk.Advance(lastDue.Sub(e.clk.Now()) - time.Millisecond)
	e.sched.Fire()
	if fired, _ := e.trig.Debouncer().Stats(key); fired != 0 {
		t.Fatalf("距最后一次提交 %v 就执行了（早于 after=%v）", after-time.Millisecond, after)
	}
	if n := e.countRows("op_runs"); n != 0 {
		t.Fatalf("早于 after 就有 %d 次运行", n)
	}

	// 到达 due：执行，且只执行一次。
	e.clk.Advance(time.Millisecond)
	e.sched.Fire()

	fired, canceled := e.trig.Debouncer().Stats(key)
	if fired != 1 {
		t.Fatalf("5 次提交共执行 %d 次，want 1（去抖必须把突发合并成一次）", fired)
	}
	if canceled != 4 {
		t.Errorf("被取消重排 %d 次，want 4（前 4 次都该被后续提交取消）", canceled)
	}
	if n := e.countRows("op_runs"); n != 1 {
		t.Fatalf("op_runs 行数=%d, want 1", n)
	}

	// 执行时刻必须**晚于最后一次提交 + after**（相等可以：这是下界）。
	startedAt := e.countExec(t, `SELECT started_at FROM op_runs`)
	wantAtLeast := lastSubmit.UnixMilli() + after.Milliseconds()
	if startedAt < wantAtLeast {
		t.Errorf("执行时刻=%d（相对起点 +%dms），want >= 最后一次提交 + after = +%dms",
			startedAt, startedAt-start.UnixMilli(), wantAtLeast-start.UnixMilli())
	}
	if total := e.sched.Total(); total != 5 {
		t.Errorf("调度器共登记 %d 个定时器，want 5（每次提交一次）", total)
	}
	if pending := e.sched.Pending(); pending != 0 {
		t.Errorf("执行后仍有 %d 个待触发定时器（会变成第二次执行）", pending)
	}

	// 安静下来之后不得再触发第二次（"永不执行"的反面同样不允许重复）。
	e.sched.Advance(10 * time.Second)
	if firedAfter, _ := e.trig.Debouncer().Stats(key); firedAfter != 1 {
		t.Errorf("安静后又触发了 %d 次", firedAfter-1)
	}
	if n := e.countRows("op_runs"); n != 1 {
		t.Errorf("安静后 op_runs 变成 %d 行，want 1", n)
	}
}

// TestDebounceRealSchedulerFiresOnce 走**生产调度器**（time.AfterFunc）。
//
// 为什么必须单独测：假调度器绕开了 RealScheduler，接线错误（例如把 AfterFunc
// 的返回值当"是否触发"、或忘记 Stop）只会在真实路径上暴露。
func TestDebounceRealSchedulerFiresOnce(t *testing.T) {
	const after = 60 * time.Millisecond
	// 显式切回生产调度器与真实时钟（newEnv 默认注入手动调度器 + 假时钟）。
	// 两者必须同时切换：只换调度器会让 started_at 记成假时钟的时间戳，
	// 那就成了拿假时间比真时间（用例自身失效，而不是产品缺陷）。
	e := newEnv(t, func(c *ops.Config) {
		c.AfterWrite = after
		c.Scheduler = ops.RealScheduler{}
		c.Now = time.Now
	})
	ctx := context.Background()
	key := string(store.ScopeUser)

	var lastSubmit time.Time
	for i := 0; i < 5; i++ {
		e.trig.AfterWrite(ctx, store.ScopeUser, ops.OpConsolidate, ops.Input{})
		lastSubmit = time.Now()
		time.Sleep(after / 6) // 间隔 < after：每次都在窗口内"再等等"
	}

	// 到一个 after 时不该已经执行（距最后一次提交不足 after）。
	if n := e.countRows("op_runs"); n != 0 {
		t.Fatalf("距最后一次提交不足 after 就已产生 %d 次运行", n)
	}

	waitFor(t, 2*time.Second, "去抖在 after 之后执行一次", func() bool {
		return e.countRows("op_runs") >= 1
	})
	// 给潜在的第二次执行留出足够窗口再确认（3×after）。
	time.Sleep(3 * after)

	if fired, _ := e.trig.Debouncer().Stats(key); fired != 1 {
		t.Errorf("5 次提交在真实调度器上执行 %d 次，want 1", fired)
	}
	if n := e.countRows("op_runs"); n != 1 {
		t.Fatalf("op_runs 行数=%d, want 1（真实调度器路径上出现了重复执行）", n)
	}
	startedAt := e.countExec(t, `SELECT started_at FROM op_runs`)
	if min := lastSubmit.UnixMilli() + after.Milliseconds(); startedAt < min {
		t.Errorf("执行时刻 %d 早于 最后一次提交+after=%d", startedAt, min)
	}
}

// TestDebounceConcurrentSameKeyFiresOnce 断言并发提交同一 key 仍只执行一次且无竞争。
//
// 形状：32 个 goroutine 同时 Submit 同一 key。正确语义是"互相取消重排，最终 1 次"；
// 错误实现会让每个 goroutine 各留一个定时器（N 次执行 = 成本爆炸）。
// 本用例如需配合 `go test -race` 才有意义（竞态由 -race 检测，不由此处的断言检测）。
func TestDebounceConcurrentSameKeyFiresOnce(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	key := string(store.ScopeUser)

	const goroutines = 32
	var (
		wg    sync.WaitGroup
		start = make(chan struct{})
	)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			<-start
			e.trig.AfterWrite(ctx, store.ScopeUser, ops.OpConsolidate, ops.Input{})
		}()
	}
	close(start)
	wg.Wait()

	if pending := e.sched.Pending(); pending != 1 {
		t.Fatalf("32 个并发提交后待触发定时器=%d, want 1（去抖未合并）", pending)
	}
	e.sched.Advance(10 * time.Second)

	fired, _ := e.trig.Debouncer().Stats(key)
	if fired != 1 {
		t.Fatalf("并发提交后执行 %d 次，want 1", fired)
	}
	if n := e.countRows("op_runs"); n != 1 {
		t.Fatalf("op_runs 行数=%d, want 1（并发下出现了重复执行/双写）", n)
	}
	if n := e.countRows("op_runs WHERE status IN ('running','failed','cancelled')"); n != 0 {
		t.Errorf("有 %d 次运行没有正常收尾（留下卡住的台账）", n)
	}
}

// TestDebounceDistinctKeysDoNotInterfere 断言去抖键隔离。
//
// 反例的形状很具体：所有 key 共用一个定时器 → 只有最后一个 scope 被整理，
// 其它 scope 的记忆**永远不进入生命周期**（静默漏整理）。
func TestDebounceDistinctKeysDoNotInterfere(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	scopes := []store.Scope{store.ScopeUser, store.ScopeAgent, store.ScopeProject, store.ScopeGlobal}

	for _, s := range scopes {
		e.trig.AfterWrite(ctx, s, ops.OpConsolidate, ops.Input{})
	}
	if pending := e.sched.Pending(); pending != len(scopes) {
		t.Fatalf("4 个不同 key 的待触发数=%d, want %d", pending, len(scopes))
	}
	if canceled := e.sched.Canceled(); canceled != 0 {
		t.Errorf("不同 key 之间发生了 %d 次取消（应互不干扰）", canceled)
	}
	e.sched.Advance(10 * time.Second)

	for _, s := range scopes {
		if fired, cancelled := e.trig.Debouncer().Stats(string(s)); fired != 1 {
			t.Errorf("scope %s 执行 %d 次，want 1（取消重排 %d 次）", s, fired, cancelled)
		}
	}
	if n := e.countRows("op_runs"); n != len(scopes) {
		t.Errorf("op_runs 行数=%d, want %d（每个 key 各一次）", n, len(scopes))
	}
}

// TestDebounceStillFiresWhenSubmitterCtxCanceled 是【阻塞项】的最小可复现用例。
//
// # 现象
//
// 写入路径的 ctx 通常是**请求/对话级**的（HTTP 响应结束、或一轮 agent 交互结束就
// cancel）。去抖把提交时的 ctx 捕获进定时器闭包，于是 after 之后真正执行时用的是
// 一个**已取消**的 ctx：
//
//	fire → runner.Run(canceledCtx) → Store.Tx 起事务即失败 → status=cancelled
//
// 结果：`fired` 计数是 1（看起来"触发了"），但库里**没有任何整理发生**，
// 而且不会自动重试——这就是 TD §8 说的"永不执行"那个极端。
//
// # 为什么实现者测不到
//
// ops/triggers_test.go 里所有 AfterWrite 都传 context.Background()，
// 这个 ctx 永不取消，因此缺陷不可见。生产里恰恰相反。
//
// # 判定
//
//   - 期望语义（本用例断言）：去抖一旦被登记，执行不应依赖提交者的 ctx 存活；
//     应使用"不继承取消"的 ctx（或显式要求宿主传长生命周期 ctx 并在文档里写明）。
//   - 默认不红（避免掩盖新回归），GMEM_VERIFY_BLOCKER=1 时红。
func TestDebounceStillFiresWhenSubmitterCtxCanceled(t *testing.T) {
	e := newEnv(t)
	key := string(store.ScopeUser)

	// 前置：库里必须有一条“值得整理”的记忆。
	//
	// 原用例的空库前置有一个自身缺陷：Consolidate 在无数据时必然返回 noop，
	// 于是 `applied == 1` 在任何实现下都不可能成立（无论 ctx 修没修）。
	// 补一条真实数据（不是放宽断言）后，这条用例才能分辨“ctx 被捕获/没被捕获”两​​种实现。
	m := mem("m-deb-ctx", store.ScopeUser, "")
	m.TValid, m.TCreated = e.clk.Now().Unix(), e.clk.Now().Unix()
	e.put(m)

	reqCtx, cancel := context.WithCancel(context.Background())
	e.trig.AfterWrite(reqCtx, store.ScopeUser, ops.OpConsolidate, ops.Input{})
	// 请求结束（响应已返回）：提交者的 ctx 被取消。
	cancel()

	e.sched.Advance(10 * time.Second)

	fired, _ := e.trig.Debouncer().Stats(key)
	runs := e.countRows("op_runs")
	applied := e.countRows("op_runs WHERE status = 'applied'")
	cancelled := e.countRows("op_runs WHERE status = 'cancelled'")

	if fired != 1 {
		t.Fatalf("去抖应触发 1 次，实际 %d 次（这里只关心执行次数）", fired)
	}
	if applied == 1 {
		// 期望行为已经成立（实现改用 WithoutCancel 之类），无需门禁。
		return
	}
	msg := "去抖触发后没有产生任何 applied 运行：提交者 ctx 取消会让整理**永不执行**" +
		"（fired=1 但 op_runs.applied=0，cancelled=" + itoa(cancelled) + "，总行数=" + itoa(runs) + "）"
	if blockerGate() {
		t.Fatal(msg)
	}
	t.Log("【阻塞合并 / GMEM_VERIFY_BLOCKER=1 时红】" + msg)
}

// countExec 读一个整数列。
func (e *env) countExec(t *testing.T, query string, args ...any) int64 {
	t.Helper()
	var v int64
	if err := e.raw.QueryRow(query, args...).Scan(&v); err != nil {
		t.Fatalf("scan %q: %v", query, err)
	}
	return v
}
