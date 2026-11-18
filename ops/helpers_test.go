package ops_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/tim5wang/gmem/ops"
	"github.com/tim5wang/gmem/recall"
	"github.com/tim5wang/gmem/sqlite"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// P3 测试基座
//
// 全部用例走**公开 API**（ops / recall / sqlite / store），并且：
//   - 时钟可注入（不 sleep、不依赖真实时间）；
//   - 调度器可注入（去抖回归不靠 sleep）；
//   - 每个用例一个独立内存库（互不干扰）。
// ===========================================================================

// clock 是可推进的假时钟。
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock {
	return &clock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeTimer 是假调度器里的一个定时器。
type fakeTimer struct {
	due    time.Time
	fn     func()
	fired  bool
	cancel bool
}

// fakeScheduler 让"after=N 后执行 + 同 key 取消重排"可被确定性断言。
type fakeScheduler struct {
	clk    *clock
	mu     sync.Mutex
	timers []*fakeTimer
}

func newFakeScheduler(clk *clock) *fakeScheduler { return &fakeScheduler{clk: clk} }

func (f *fakeScheduler) After(d time.Duration, fn func()) func() {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := &fakeTimer{due: f.clk.Now().Add(d), fn: fn}
	f.timers = append(f.timers, t)
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		t.cancel = true
	}
}

// Advance 推进时钟并触发到期的定时器（按到期顺序）。
func (f *fakeScheduler) Advance(d time.Duration) {
	f.clk.Advance(d)
	f.Fire()
}

// Fire 触发所有已到期、未取消、未触发的定时器。
func (f *fakeScheduler) Fire() {
	f.mu.Lock()
	var ready []*fakeTimer
	for _, t := range f.timers {
		if !t.fired && !t.cancel && !t.due.After(f.clk.Now()) {
			ready = append(ready, t)
		}
	}
	for _, t := range ready {
		t.fired = true
	}
	f.mu.Unlock()
	sort.Slice(ready, func(i, j int) bool { return ready[i].due.Before(ready[j].due) })
	for _, t := range ready {
		t.fn()
	}
}

// Pending 返回尚未触发且未取消的定时器数。
func (f *fakeScheduler) Pending() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, t := range f.timers {
		if !t.fired && !t.cancel {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// 库与 Runner
// ---------------------------------------------------------------------------

// newDB 打开文件库，并给一条**直连**用于行级断言
// （memories_fts / audit_log / op_runs 这些只有直接查才知道对不对）。
func newDB(t *testing.T) (*sqlite.DB, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "p3ops.db")
	d, err := sqlite.Open(context.Background(), sqlite.Config{Path: path})
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("close db: %v", err)
		}
	})
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open(raw): %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return d, raw
}

// env 是一次测试的运行环境。
type env struct {
	t      *testing.T
	db     *sqlite.DB
	raw    *sql.DB
	clk    *clock
	sched  *fakeScheduler
	cfg    ops.Config
	runner *ops.Runner
	maint  *ops.Maintainer
	trig   *ops.Triggers
	ext    *fakeExtractor
}

func newEnv(t *testing.T, mutators ...func(*ops.Config)) *env {
	t.Helper()
	db, raw := newDB(t)
	clk := newClock()
	sched := newFakeScheduler(clk)
	cfg := ops.DefaultConfig()
	cfg.Now = clk.Now
	cfg.Scheduler = sched
	for _, m := range mutators {
		m(&cfg)
	}
	ext := &fakeExtractor{ports: db.Ports()}
	r, err := ops.NewRunner(db.Ports(), db.OpsBackend(),
		ops.WithConfig(cfg),
		ops.WithExtractor(ext),
	)
	if err != nil {
		t.Fatalf("ops.NewRunner: %v", err)
	}
	return &env{
		t: t, db: db, raw: raw, clk: clk, sched: sched, cfg: cfg,
		runner: r, maint: ops.NewMaintainer(r), trig: ops.NewTriggers(r), ext: ext,
	}
}

// runnerWithout 构造一个缺组件的 Runner（用于显式降级/缺写入口的断言）。
func (e *env) runnerWithout(opts ...ops.Option) *ops.Runner {
	e.t.Helper()
	base := []ops.Option{ops.WithConfig(e.cfg)}
	r, err := ops.NewRunner(e.db.Ports(), e.db.OpsBackend(), append(base, opts...)...)
	if err != nil {
		e.t.Fatalf("ops.NewRunner(without): %v", err)
	}
	return r
}

// putMemory 经 recall.Writer 写入一条记忆（**唯一允许的写入方式**）。
func (e *env) putMemory(m store.Memory) recall.PutResult {
	e.t.Helper()
	if m.TCreated == 0 {
		m.TCreated = e.clk.Now().Unix()
	}
	res, err := recall.NewWriter(e.db.Ports()).PutMemory(context.Background(), m)
	if err != nil {
		e.t.Fatalf("writer.PutMemory(%q): %v", m.ID, err)
	}
	return res
}

// memory 构造一条内容足够（>= MinContentLength）的语义记忆。
func memory(id string, scope store.Scope, concept string) store.Memory {
	return store.Memory{
		ID: id, Scope: scope, ConceptID: concept, Kind: store.KindSemantic,
		Content:  "这是一条用于 P3 生命周期测试的完整记忆正文（超过十个字符）",
		TValid:   1000,
		Importance: 0.5,
	}
}

// countRows 直接查库计数（行级断言）。
func (e *env) countRows(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.raw.QueryRow(query, args...).Scan(&n); err != nil {
		e.t.Fatalf("query %q: %v", query, err)
	}
	return n
}

// exec 直接改库（用于造"历史数据"这类测试前置状态）。
func (e *env) exec(query string, args ...any) {
	e.t.Helper()
	if _, err := e.raw.Exec(query, args...); err != nil {
		e.t.Fatalf("exec %q: %v", query, err)
	}
}

// scalar 取一行一列（用于读 version 等）。
func (e *env) scalar(query string, args ...any) string {
	e.t.Helper()
	var v sql.NullString
	if err := e.raw.QueryRow(query, args...).Scan(&v); err != nil {
		e.t.Fatalf("scalar %q: %v", query, err)
	}
	return v.String
}

// ---------------------------------------------------------------------------
// 假 Extractor
// ---------------------------------------------------------------------------

// fakeExtractor 把"提取"简化成"经 recall.Writer 写一条记忆"，用来验证
// ops 侧的生命周期编排（P2 的抽取质量由 p2verify 负责，不在这里重复测）。
type fakeExtractor struct {
	ports store.Ports
	// onCall 可注入副作用（例如取消 ctx、断言 tx 可见性）。
	onCall func(n int, ep ops.EpisodeInput)
	calls  []ops.EpisodeInput
}

func (f *fakeExtractor) ExtractEpisode(ctx context.Context, ep ops.EpisodeInput) (ops.ExtractOutcome, error) {
	f.calls = append(f.calls, ep)
	if f.onCall != nil {
		f.onCall(len(f.calls), ep)
	}
	if err := ctx.Err(); err != nil {
		return ops.ExtractOutcome{}, err
	}
	id := "mem-" + ep.ID
	res, err := recall.NewWriter(f.ports).PutMemory(ctx, store.Memory{
		ID: id, Scope: ep.Scope, Kind: store.KindSemantic,
		Content: "从 episode " + ep.ID + " 提取出来的记忆正文（足够长）",
		TValid: ep.At, TCreated: ep.At,
	})
	if err != nil {
		return ops.ExtractOutcome{}, err
	}
	return ops.ExtractOutcome{Writes: []ops.WriteOutcome{{
		MemoryID: res.ID, Version: res.Version, LexicalIndexed: res.LexicalIndexed,
	}}}, nil
}

// recalledWriterPut 经 P1 的写入编排（recall.Writer）写一条记忆。
//
// 它是测试里**唯一**允许的写入方式：绕过它会让 FTS 静默漏索引，
// 而 ops/guard_test.go 的变异用例正是用这一点来证明守卫的必要性。
func recalledWriterPut(e *env, m store.Memory) (recall.PutResult, error) {
	e.t.Helper()
	return recall.NewWriter(e.db.Ports()).PutMemory(context.Background(), m)
}
