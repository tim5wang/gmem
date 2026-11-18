package p3verify

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tim5wang/gmem/ops"
	"github.com/tim5wang/gmem/recall"
	"github.com/tim5wang/gmem/sqlite"
	"github.com/tim5wang/gmem/store"

	_ "modernc.org/sqlite"
)

// ===========================================================================
// P3 的**独立验证**包：生命周期与 op 层（docs/TD.md §7 / §8）
//
// 与 internal/vtest、internal/p1verify、internal/p2verify 同一手法：
//   - 只 import 公开 API（ops / recall / sqlite / store），**不复用 ops/*_test.go
//     的任何代码**（那是实现者自己的测试：它的基座可能恰好绕开了缺陷）；
//   - 断言一律从外部可见结果出发：库里的行、op_runs/audit_log 的内容、
//     Aligner 的 StatusFactor，而不是内部字段"看起来对"；
//   - 时钟与调度器都由本包自己注入，回归不靠 sleep。
//
// 本包的定位：**找出实现者测不到的问题**，因此每个用例都先问一句
// "这条断言如果写错，生产里会变成什么形状"。
// ===========================================================================

// testClock 是可推进的假时钟（本包独立实现）。
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock {
	return &testClock{t: time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// manualTimer 是手动调度器里的一个定时器。
type manualTimer struct {
	due      time.Time
	fn       func()
	canceled bool
	fired    bool
	firedAt  time.Time
}

// manualScheduler 让"after=N 之后才执行 + 同 key 取消重排"可被确定性断言。
//
// 为什么必须可注入：用 sleep 证明去抖只能证明"某段时间内没执行"，
// 证明不了"执行时刻 = 最后一次写入 + after"——而后者才是 D3 的成本契约。
type manualScheduler struct {
	clk *testClock
	mu  sync.Mutex
	all []*manualTimer
}

func newManualScheduler(clk *testClock) *manualScheduler { return &manualScheduler{clk: clk} }

func (m *manualScheduler) After(d time.Duration, fn func()) func() {
	m.mu.Lock()
	defer m.mu.Unlock()
	tm := &manualTimer{due: m.clk.Now().Add(d), fn: fn}
	m.all = append(m.all, tm)
	return func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		tm.canceled = true
	}
}

// Advance 推进时钟并触发所有已到期、未取消的定时器（按到期时间排序）。
func (m *manualScheduler) Advance(d time.Duration) {
	m.clk.Advance(d)
	m.Fire()
}

// Fire 触发已到期的定时器。
func (m *manualScheduler) Fire() {
	m.mu.Lock()
	now := m.clk.Now()
	var ready []*manualTimer
	for _, tm := range m.all {
		if !tm.fired && !tm.canceled && !tm.due.After(now) {
			ready = append(ready, tm)
		}
	}
	for _, tm := range ready {
		tm.fired = true
		tm.firedAt = now
	}
	m.mu.Unlock()
	// 按到期时间升序执行（同 due 时保持登记顺序）
	for i := 0; i < len(ready); i++ {
		for j := i + 1; j < len(ready); j++ {
			if ready[j].due.Before(ready[i].due) {
				ready[i], ready[j] = ready[j], ready[i]
			}
		}
	}
	for _, tm := range ready {
		tm.fn()
	}
}

// Pending 返回尚未触发且未取消的定时器数。
func (m *manualScheduler) Pending() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, tm := range m.all {
		if !tm.fired && !tm.canceled {
			n++
		}
	}
	return n
}

// Canceled 返回被取消的定时器数。
func (m *manualScheduler) Canceled() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, tm := range m.all {
		if tm.canceled {
			n++
		}
	}
	return n
}

func (m *manualScheduler) Total() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.all)
}

// ---------------------------------------------------------------------------
// 库与环境
// ---------------------------------------------------------------------------

// newDB 打开文件库，并给一条直连用于行级断言。
func newDB(t *testing.T) (*sqlite.DB, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "p3.db")
	d, err := sqlite.Open(context.Background(), sqlite.Config{Path: path})
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open(raw): %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return d, raw
}

// env 是一次验证的运行环境。
type env struct {
	t      *testing.T
	db     *sqlite.DB
	raw    *sql.DB
	clk    *testClock
	sched  *manualScheduler
	cfg    ops.Config
	runner *ops.Runner
	maint  *ops.Maintainer
	trig   *ops.Triggers
	ext    *probeExtractor
}

func newEnv(t *testing.T, mutators ...func(*ops.Config)) *env {
	t.Helper()
	db, raw := newDB(t)
	clk := newTestClock()
	sched := newManualScheduler(clk)
	cfg := ops.DefaultConfig()
	cfg.Now = clk.Now
	cfg.Scheduler = sched
	for _, m := range mutators {
		m(&cfg)
	}
	ext := &probeExtractor{ports: db.Ports()}
	r, err := ops.NewRunner(db.Ports(), db.OpsBackend(),
		ops.WithConfig(cfg), ops.WithExtractor(ext))
	if err != nil {
		t.Fatalf("ops.NewRunner: %v", err)
	}
	return &env{
		t: t, db: db, raw: raw, clk: clk, sched: sched, cfg: cfg,
		runner: r, maint: ops.NewMaintainer(r), trig: ops.NewTriggers(r), ext: ext,
	}
}

// put 经 recall.Writer 写入一条记忆（生产里唯一允许的写入方式）。
func (e *env) put(m store.Memory) recall.PutResult {
	e.t.Helper()
	res, err := recall.NewWriter(e.db.Ports()).PutMemory(context.Background(), m)
	if err != nil {
		e.t.Fatalf("writer.PutMemory(%q): %v", m.ID, err)
	}
	return res
}

// mem 构造一条内容长度足够的语义记忆（否则会被质量门判 Rejected）。
func mem(id string, scope store.Scope, concept string) store.Memory {
	return store.Memory{
		ID: id, Scope: scope, ConceptID: concept, Kind: store.KindSemantic,
		Content: "这是一条用于 P3 独立验证的完整记忆正文（长度充足）",
		TValid:  1000, Importance: 0.5,
	}
}

func (e *env) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.raw.QueryRow(query, args...).Scan(&n); err != nil {
		e.t.Fatalf("count %q: %v", query, err)
	}
	return n
}

func (e *env) exec(query string, args ...any) {
	e.t.Helper()
	if _, err := e.raw.Exec(query, args...); err != nil {
		e.t.Fatalf("exec %q: %v", query, err)
	}
}

func (e *env) str(query string, args ...any) string {
	e.t.Helper()
	var v sql.NullString
	if err := e.raw.QueryRow(query, args...).Scan(&v); err != nil {
		e.t.Fatalf("str %q: %v", query, err)
	}
	return v.String
}

// seedState 把一条已写入记忆的状态行改成指定状态（造生命周期前置条件）。
func (e *env) seedState(id string, status ops.MemoryStatus, importance float64) {
	e.t.Helper()
	now := e.clk.Now().UnixMilli()
	e.exec(`INSERT INTO memory_state (memory_id, scope, status, importance_base,
			importance_recency, importance_access_boost, importance_grade, importance,
			status_changed_at, last_recalled_at, created_at, updated_at)
		VALUES (?, 'user', ?, ?, 1, 1, 1, ?, ?, 0, ?, ?)
		ON CONFLICT(memory_id) DO UPDATE SET status=excluded.status, importance=excluded.importance`,
		id, string(status), importance, importance, now, now, now)
}

// probeExtractor 把"提取"简化成"经 recall.Writer 写一条记忆"，
// 用于验证 ops 侧的生命周期编排（抽取质量由 p2verify 负责）。
type probeExtractor struct {
	ports  store.Ports
	onCall func(n int, ep ops.EpisodeInput) // 可注入副作用（例如取消 ctx）
	calls  []ops.EpisodeInput
}

func (p *probeExtractor) ExtractEpisode(ctx context.Context, ep ops.EpisodeInput) (ops.ExtractOutcome, error) {
	p.calls = append(p.calls, ep)
	if p.onCall != nil {
		p.onCall(len(p.calls), ep)
	}
	if err := ctx.Err(); err != nil {
		return ops.ExtractOutcome{}, err
	}
	res, err := recall.NewWriter(p.ports).PutMemory(ctx, store.Memory{
		ID: "mem-" + ep.ID, Scope: ep.Scope, Kind: store.KindSemantic,
		Content: "从 episode " + ep.ID + " 提取出的记忆正文（长度充足）",
		TValid:  ep.At, TCreated: ep.At,
	})
	if err != nil {
		return ops.ExtractOutcome{}, err
	}
	return ops.ExtractOutcome{Writes: []ops.WriteOutcome{{
		MemoryID: res.ID, Version: res.Version, LexicalIndexed: res.LexicalIndexed,
	}}}, nil
}

// waitFor 轮询直到 cond 成立或超时（真实时钟用例用）。
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("等待超时（%s）：%s", timeout, what)
}

// mustRun 跑一次 op 并要求无错误。
func (e *env) mustRun(req ops.Request) ops.Result {
	e.t.Helper()
	res, err := e.runner.Run(context.Background(), req)
	if err != nil {
		e.t.Fatalf("Run(%s): %v", req.Op, err)
	}
	return res
}

func (e *env) countRows(table string) int {
	e.t.Helper()
	return e.count(fmt.Sprintf("SELECT COUNT(1) FROM %s", table))
}
