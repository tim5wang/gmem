package ops

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tim5wang/gmem/store"
)

// ErrBudgetExceeded 表示本次运行触到预算上限（正常停手点，不是缺陷）。
var ErrBudgetExceeded = errors.New("ops: budget exceeded")

// NewRunner 构造 op 执行器。它是**自动触发与 Agent 调用的唯一出口**（D3）。
//
// 必填：ports.Store（事务与 CRUD）、backend（生命周期/审计/幂等台账）。
// 可选：Extractor（P2 管线）——缺失时 Extract 显式降级，绝不静默跳过
// （"少写一条"在生产里最难查）。
func NewRunner(ports store.Ports, backend Backend, opts ...Option) (*Runner, error) {
	if ports.Store == nil {
		return nil, fmt.Errorf("ops: nil store port")
	}
	if backend == nil {
		return nil, fmt.Errorf("ops: nil backend（op_runs/audit_log 是 D3 的硬要求）")
	}
	r := &Runner{ports: ports, backend: backend}
	for _, o := range opts {
		o(r)
	}
	r.cfg = r.cfg.withDefaults()
	r.ops = r.buildOps()
	return r, nil
}

// Runner 执行原子 op：幂等 key → 一个事务 → Plan/Apply → audit_log。
type Runner struct {
	ports     store.Ports
	backend   Backend
	extractor Extractor
	cfg       Config
	ops       map[OpName]Op
}

// Config 返回生效配置。
func (r *Runner) Config() Config { return r.cfg }

// Backend 返回注入的存储端口（供维护面做只读查询）。
func (r *Runner) Backend() Backend { return r.backend }

// Ops 返回已注册的 op 清单（顺序固定）。
func (r *Runner) Ops() []OpName { return AllOps() }

// scanOps 是"按时间窗口扫描"的 op：它们的幂等 key 必须含窗口。
//
// 只看输入参数就足够的 op（Merge/LinkUpdate/Invalidate/Promote）**不在此列**：
// 对它们而言"同输入 = 同一件事"，重跑必须安全跳过（这才是幂等的本意）。
var scanOps = map[OpName]bool{
	OpExtract: true, OpDisambiguate: true, OpConsolidate: true,
	OpDecay: true, OpPrune: true,
}

// withWindow 给扫描型 op 补上扫描窗口（默认 = 本次运行的当前时刻）。
func (r *Runner) withWindow(op Op, in Input) Input {
	if !scanOps[op.Name()] || in.Window != 0 {
		return in
	}
	in.Window = r.cfg.Now().UnixMilli()
	return in
}

// budgetOf 取 op 自报预算与触发器预算的更严者，再与 op 层默认预算取更严者。
func (r *Runner) budgetOf(op Op, t Trigger) Budget {
	return op.Budget().stricter(t.Budget).stricter(r.cfg.Budget)
}

// Run 执行一次 op。**四种触发源都只走这一个入口**（D3）。
//
// 语义：
//   - 幂等：同 key 已成功（applied/noop）或正在跑 → 返回 RunDuplicate，不重复应用；
//     failed/cancelled 的 key 允许重跑（否则一次瞬时故障会让该窗口永久卡死）。
//   - 可取消：ctx 取消 → 事务回滚；随后用独立事务记一条 cancelled，说明"没有变化"。
//   - 预算：Duration 走 ctx 超时；Steps/IO 在循环里检查，触顶即停手（已应用的步骤保留）。
func (r *Runner) Run(ctx context.Context, req Request) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	op, ok := r.ops[req.Op]
	if !ok {
		return Result{}, fmt.Errorf("ops: unknown op %q", req.Op)
	}
	if !req.Scope.Valid() {
		return Result{}, fmt.Errorf("ops: invalid scope %q（Scope 是必填的隔离边界）", req.Scope)
	}
	trig := req.Trigger.normalize()
	// 扫描型 op 的"输入摘要"必须含扫描窗口，否则第二次整理会被当成重跑而永久跳过。
	//
	// 为什么放在 Runner 而不是各触发源里：四种触发源（after_write/threshold/idle/agent）
	// 都必须得到同一份 key 语义，否则同一个 op 在两条路径上的可重跑性会不同（违背 D3）。
	req.Input = r.withWindow(op, req.Input)
	key := req.Key
	if key == "" {
		var err error
		key, err = IdempotencyKey(req.Scope, req.Op, req.Input)
		if err != nil {
			return Result{}, err
		}
	}
	budget := r.budgetOf(op, trig)

	res := Result{Op: req.Op, Scope: req.Scope, Key: key, Budget: budget}

	runCtx := ctx
	if budget.Duration > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, budget.Duration)
		defer cancel()
	}

	var (
		steps    []StepResult
		dup      bool
		canceled bool
	)
	rec := RunRecord{
		Key: key, Op: req.Op, Scope: req.Scope, Trigger: trig.Kind,
		Actor: trig.Actor, Reason: trig.Reason, Status: RunRunning,
		StartedAt: r.cfg.Now().UnixMilli(),
	}

	txErr := r.ports.Store.Tx(runCtx, func(tctx context.Context, tx store.Store) error {
		existing, found, err := r.backend.GetRun(tctx, key)
		if err != nil {
			return err
		}
		if found {
			if existing.Status == RunApplied || existing.Status == RunNoop || existing.Status == RunRunning {
				dup = true
				return nil // 不应用任何步骤：重跑安全
			}
			// failed / cancelled：允许重跑，把台账改回 running。
			if err := r.backend.UpdateRun(tctx, rec); err != nil {
				return err
			}
		} else if err := r.backend.InsertRun(tctx, rec); err != nil {
			if IsDuplicateRun(err) {
				dup = true
				return nil
			}
			return err
		}

		txc := &txContext{
			ctx: tctx, scope: req.Scope, st: tx, ports: r.ports, be: r.backend,
			ex: r.extractor, in: req.Input, cfg: r.cfg, now: r.cfg.Now(), budget: budget,
		}

		planned, err := op.Plan(txc)
		if err != nil {
			if isCancel(err) {
				canceled = true
				return err
			}
			return err
		}
		applied := 0
		for _, s := range planned {
			if err := txc.Check(); err != nil {
				canceled = true
				return err
			}
			if budget.Steps > 0 && len(steps) >= budget.Steps {
				break
			}
			d, err := op.Apply(txc, s)
			if err != nil {
				if isCancel(err) || errors.Is(err, ErrBudgetExceeded) {
					canceled = isCancel(err)
					if !canceled {
						// 预算触顶不是取消：已应用的步骤照常提交，未做的留空。
						break
					}
					return err
				}
				return fmt.Errorf("ops: %s step %s: %w", req.Op, s.ID, err)
			}
			steps = append(steps, StepResult{Step: s, Diff: d})
			if d.Changed {
				applied++
			}
			if _, err := r.backend.AppendAudit(tctx, AuditEntry{
				OpID: key, Op: req.Op, Scope: req.Scope,
				Trigger: trig.Kind, Actor: trig.Actor, Reason: trig.Reason,
				StepID: s.ID, StepKind: s.Kind, Subject: s.Subject,
				BeforeJSON: d.Before, AfterJSON: d.After,
				Changed: d.Changed, At: r.cfg.Now().UnixMilli(),
			}); err != nil {
				return err
			}
			res.Degraded = append(res.Degraded, d.Degraded...)
			res.Warnings = append(res.Warnings, d.Warnings...)
		}

		status := RunNoop
		if applied > 0 {
			status = RunApplied
		}
		rec.Status = status
		rec.FinishedAt = r.cfg.Now().UnixMilli()
		rec.Note = fmt.Sprintf("steps=%d applied=%d", len(steps), applied)
		if err := r.backend.UpdateRun(tctx, rec); err != nil {
			return err
		}
		res.Status = status
		res.Steps = steps
		return nil
	})

	switch {
	case dup:
		res.Status = RunDuplicate
		res.Steps = nil
		return res, nil
	case canceled || isCancel(txErr):
		// 事务已回滚（没有半截状态）。用独立事务记一条 cancelled，
		// 让"这次为什么没有变化"在审计里可回答。
		r.recordCancelled(ctx, rec, txErr)
		res.Status = RunCancelled
		res.Steps = nil
		return res, ctx.Err()
	case txErr != nil:
		r.recordFailed(ctx, rec, txErr)
		res.Status = RunFailed
		return res, txErr
	}
	if res.Status == "" {
		res.Status = RunNoop
		res.Steps = steps
	}
	return res, nil
}

// Plan 只出计划、不应用（ReviewSimilar 用它把"疑似重复"交给人/Agent）。
//
// 它与 Run 共用同一个 Plan 实现，因此**看到的就是会执行的**。
func (r *Runner) Plan(ctx context.Context, req Request) ([]Step, error) {
	op, ok := r.ops[req.Op]
	if !ok {
		return nil, fmt.Errorf("ops: unknown op %q", req.Op)
	}
	if !req.Scope.Valid() {
		return nil, fmt.Errorf("ops: invalid scope %q", req.Scope)
	}
	var out []Step
	err := r.ports.Store.Tx(ctx, func(tctx context.Context, tx store.Store) error {
		txc := &txContext{
			ctx: tctx, scope: req.Scope, st: tx, ports: r.ports, be: r.backend,
			ex: r.extractor, in: req.Input, cfg: r.cfg, now: r.cfg.Now(),
			budget: r.cfg.Budget,
		}
		steps, err := op.Plan(txc)
		if err != nil {
			return err
		}
		out = steps
		return nil
	})
	return out, err
}

// recordCancelled / recordFailed 用**独立事务**记账。
//
// 不用被打断的 ctx（它已经取消，再往里写必然失败），改用 Background + 短超时，
// 因此"取消"这件事本身不会因为取消而丢失审计。
func (r *Runner) recordCancelled(ctx context.Context, rec RunRecord, cause error) {
	base := context.Background()
	if ctx != nil {
		// 只继承值不继承取消：审计写入必须能落地。
		base = context.WithoutCancel(ctx)
	}
	note := "cancelled"
	if cause != nil {
		note += ": " + cause.Error()
	}
	r.markRun(base, rec, RunCancelled, note)
}

func (r *Runner) recordFailed(ctx context.Context, rec RunRecord, cause error) {
	base := context.WithoutCancel(ctx)
	note := "failed"
	if cause != nil {
		note += ": " + cause.Error()
	}
	r.markRun(base, rec, RunFailed, note)
}

func (r *Runner) markRun(ctx context.Context, rec RunRecord, status RunStatus, note string) {
	tctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	rec.Status = status
	rec.Note = note
	rec.FinishedAt = r.cfg.Now().UnixMilli()
	_ = r.ports.Store.Tx(tctx, func(inctx context.Context, _ store.Store) error {
		// 失败/取消的运行，它的 InsertRun 随主事务一起回滚了（这正是"不留半截状态"
		// 的副作用：台账本身也在那个事务里）。因此这里先尝试更新，找不到就补插一行。
		// 两件事都必须发生在**独立事务**里，否则取消会连带把审计一起取消掉。
		if err := r.backend.UpdateRun(inctx, rec); err != nil {
			return r.backend.InsertRun(inctx, rec)
		}
		return nil
	})
	// 记账失败不改变主结论；调用方已经从 Result.Status 得到结论。
}

func isCancel(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// ---------------------------------------------------------------------------
// TxContext 的实现
// ---------------------------------------------------------------------------

type txContext struct {
	ctx    context.Context
	scope  store.Scope
	st     store.Store
	ports  store.Ports
	be     Backend
	ex     Extractor
	in     Input
	cfg    Config
	now    time.Time
	budget Budget
	spent  int
}

func (t *txContext) Context() context.Context { return t.ctx }
func (t *txContext) Scope() store.Scope       { return t.scope }
func (t *txContext) Store() store.Store       { return t.st }
func (t *txContext) Ports() store.Ports       { return t.ports }
func (t *txContext) Input() Input             { return t.in }
func (t *txContext) Backend() Backend         { return t.be }
func (t *txContext) Extractor() Extractor     { return t.ex }
func (t *txContext) Cfg() Config              { return t.cfg }
func (t *txContext) Now() time.Time           { return t.now }

// Check 在 op 的循环里定期调用（取消点）。
func (t *txContext) Check() error { return t.ctx.Err() }

// Spend 记一次 IO 消耗；超预算返回 ErrBudgetExceeded 作为**正常停手点**。
func (t *txContext) Spend(io int) error {
	t.spent += io
	if t.budget.IO > 0 && t.spent > t.budget.IO {
		return ErrBudgetExceeded
	}
	return nil
}
