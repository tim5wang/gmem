package ops

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/tim5wang/gmem/store"
)

// buildOps 是唯一的 op 注册点。
//
// **自动触发与 Agent 调用取的是同一张表里的同一个实例**：这条是 D3 的落地点，
// 任何"给 Agent 另建一个 op"的写法都会让两条路径的行为漂移。
func (r *Runner) buildOps() map[OpName]Op {
	list := []Op{
		extractOp{}, disambiguateOp{}, mergeOp{}, linkOp{},
		invalidateOp{}, consolidateOp{}, decayOp{}, pruneOp{}, promoteOp{},
	}
	out := make(map[OpName]Op, len(list))
	for _, op := range list {
		out[op.Name()] = op
	}
	return out
}

// ---------------------------------------------------------------------------
// 调度器与去抖（TD §8：照搬 LangMem 的 ReflectionExecutor）
// ---------------------------------------------------------------------------

// Scheduler 是可注入的定时器（真实实现用 time.AfterFunc，测试用假时钟）。
//
// 把它抽成接口的唯一理由是**可测**："连续 N 次写入只触发 1 次、且在最后一次写入后
// after 才执行"这条回归断言不能靠 sleep 来证明。
type Scheduler interface {
	// After 在 d 之后调用 fn，返回取消函数。
	After(d time.Duration, fn func()) (cancel func())
}

// RealScheduler 是生产用的定时器。
type RealScheduler struct{}

// After 实现 Scheduler。
func (RealScheduler) After(d time.Duration, fn func()) func() {
	t := time.AfterFunc(d, fn)
	return func() { t.Stop() }
}

// Debouncer 落实"等对话安静再固化，而不是每轮都跑"（TD §8 去抖语义）。
//
// 语义与 LangMem 的 ReflectionExecutor 一致：
//
//	submit(ctx, payload, after=N, key="scope")
//
// 同 key 的新事件**取消并重排**：前一次的定时器被取消，计时从最后一次写入重新开始。
// 因此连续 N 次写入只产生 1 次整理，且执行时刻是"最后一次写入 + after"。
//
// 为什么这是成本量级的关键：每轮都跑等于把 LLM/IO 成本乘上对话轮数；
// 永不执行则整理永远不发生。两者都是缺陷。
type Debouncer struct {
	runner *Runner
	mu     sync.Mutex
	timers map[string]func() // key → 取消函数
	// pending 记录每个 key 最近一次 payload（合并语义：只看最后一次）。
	pending map[string]debounceEntry
	// fired 是每个 key 的实际触发次数（测试与 /doctor 用）。
	fired map[string]int
	// canceled 是被后续事件取消的次数（证明"取消并重排"真的发生，而不是没安排）。
	canceled map[string]int
}

type debounceEntry struct {
	req Request
	at  time.Time
}

// NewDebouncer 构造去抖器。
func NewDebouncer(r *Runner) *Debouncer {
	return &Debouncer{
		runner:   r,
		timers:   map[string]func(){},
		pending:  map[string]debounceEntry{},
		fired:    map[string]int{},
		canceled: map[string]int{},
	}
}

// Submit 登记一次写入后触发。key 相同则取消前一次并重排（不执行前一次）。
//
// 返回的 cancel 可撤销本次登记（Agent 打断时不留半截状态：定时器也不会再触发）。
func (d *Debouncer) Submit(ctx context.Context, req Request, after time.Duration, key string) func() {
	if key == "" {
		key = string(req.Scope)
	}
	if after <= 0 {
		after = d.runner.cfg.AfterWrite
	}
	// 去抖键参与幂等 key 的主体：同 key 重排在库里是**新的一次运行**
	// （输入摘要含 Window，所以不会被上一次的 key 挡掉）。
	req.Key = ""

	d.mu.Lock()
	if cancel, ok := d.timers[key]; ok {
		cancel()
		d.canceled[key]++
	}
	d.pending[key] = debounceEntry{req: req, at: d.runner.cfg.Now()}
	sched := d.runner.cfg.Scheduler
	cancel := sched.After(after, func() { d.fire(ctx, key) })
	d.timers[key] = cancel
	d.mu.Unlock()
	return func() { d.cancel(key) }
}

// fire 执行一次（**在锁外**调用 runner：op 会跑很久，持锁会把整条写入路径卡住）。
func (d *Debouncer) fire(ctx context.Context, key string) {
	d.mu.Lock()
	entry, ok := d.pending[key]
	if ok {
		delete(d.pending, key)
	}
	delete(d.timers, key)
	d.mu.Unlock()
	if !ok {
		return
	}
	d.mu.Lock()
	d.fired[key]++
	d.mu.Unlock()
	runCtx := ctx
	if runCtx == nil {
		runCtx = context.Background()
	}
	// 错误在这里无处上报（异步）：失败已由 Runner 写进 op_runs（status=failed）。
	_, _ = d.runner.Run(runCtx, entry.req)
}

func (d *Debouncer) cancel(key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if cancel, ok := d.timers[key]; ok {
		cancel()
		delete(d.timers, key)
	}
	delete(d.pending, key)
}

// Pending 返回尚未触发的 key 数。
func (d *Debouncer) Pending() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.pending)
}

// Stats 返回 (实际触发次数, 被取消重排次数)，按 key。
func (d *Debouncer) Stats(key string) (fired, canceled int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.fired[key], d.canceled[key]
}

// CancelAll 撤销全部未触发的登记（关闭时调用）。
func (d *Debouncer) CancelAll() {
	d.mu.Lock()
	keys := make([]string, 0, len(d.timers))
	for k := range d.timers {
		keys = append(keys, k)
	}
	d.mu.Unlock()
	for _, k := range keys {
		d.cancel(k)
	}
}

// ---------------------------------------------------------------------------
// 四种触发源
// ---------------------------------------------------------------------------

// Triggers 把四种触发源收在一处：它们**都只调 Runner.Run**，
// 因此行为一致（D3），差别只在"什么时候、以什么预算"。
type Triggers struct {
	runner *Runner
	deb    *Debouncer
}

// NewTriggers 构造触发器集合。
func NewTriggers(r *Runner) *Triggers {
	return &Triggers{runner: r, deb: NewDebouncer(r)}
}

// Debouncer 暴露去抖器（测试与 doctor 用）。
func (t *Triggers) Debouncer() *Debouncer { return t.deb }

// AfterWrite 是触发源 1：写入后去抖（同 key 取消重排）。
//
// 每次写入都在延迟窗口内"再等等"：连续写入因此合并成一次整理。
func (t *Triggers) AfterWrite(ctx context.Context, scope store.Scope, op OpName, in Input) func() {
	req := Request{
		Op: op, Scope: scope, Input: in,
		Trigger: Trigger{Kind: TriggerAfterWrite, Key: string(scope), After: t.runner.cfg.AfterWrite,
			Reason: "写入后去抖：等对话安静再固化"},
	}
	return t.deb.Submit(ctx, req, t.runner.cfg.AfterWrite, string(scope))
}

// Threshold 是触发源 2：未整理条目达到 N。
//
// 它返回一个"是否已触发"的布尔值而不是定时器：阈值判定是写入路径上的一次廉价查询，
// 达到就立刻跑（**不再等待**）——这与去抖的语义正好互补（去抖等安静，阈值等量）。
func (t *Triggers) Threshold(ctx context.Context, scope store.Scope, op OpName, in Input) (bool, Result, error) {
	n, err := t.runner.backend.CountUnconsolidatedMemories(ctx, scope)
	if err != nil {
		return false, Result{}, err
	}
	if n < t.runner.cfg.Threshold {
		return false, Result{}, nil
	}
	req := Request{
		Op: op, Scope: scope, Input: in,
		Trigger: Trigger{Kind: TriggerThreshold, Key: string(scope),
			Reason: fmt.Sprintf("未整理条目 %d >= Threshold %d", n, t.runner.cfg.Threshold)},
	}
	res, err := t.runner.Run(ctx, req)
	return true, res, err
}

// Idle 是触发源 3：空闲/定时。
//
// idle 与 cron 共用同一路径：判定"是否真的空闲"由调用方（宿主）决定，
// ops 只负责"以 idle 的身份执行同一个 op"。这样 idle 与 cron 的行为不可能分叉。
func (t *Triggers) Idle(ctx context.Context, scope store.Scope, op OpName, in Input, kind TriggerKind) (Result, error) {
	if kind != TriggerIdle && kind != TriggerCron {
		kind = TriggerIdle
	}
	req := Request{
		Op: op, Scope: scope, Input: in,
		Trigger: Trigger{Kind: kind, Key: string(scope), After: t.runner.cfg.IdleAfter,
			Reason: "空闲/定时窗口：推进状态机与衰减"},
	}
	return t.runner.Run(ctx, req)
}

// Agent 是触发源 4：Agent 显式调用。
//
// 它与上面三条走**同一个** Runner.Run；差别只有 Trigger.Kind（审计口径）与预算
// （Agent 交互通常要更快返回）。
func (t *Triggers) Agent(ctx context.Context, req Request) (Result, error) {
	if req.Trigger.Kind == "" {
		req.Trigger.Kind = TriggerAgent
	}
	if req.Trigger.Actor == "" {
		req.Trigger.Actor = "agent"
	}
	return t.runner.Run(ctx, req)
}
