package ops_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tim5wang/gmem/ops"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// 1) op 层骨架：8 个原子 op 实现同一接口，且两条触发路径走同一套代码
// ===========================================================================

// TestAllOpsShareOneInterface 钉死 §8 的 op 表：8 个 op 全部注册，且 Runner 只认识这一张表。
func TestAllOpsShareOneInterface(t *testing.T) {
	e := newEnv(t)

	want := map[ops.OpName]bool{
		ops.OpExtract: true, ops.OpDisambiguate: true, ops.OpMerge: true,
		ops.OpLinkUpdate: true, ops.OpInvalidate: true, ops.OpConsolidate: true,
		ops.OpDecay: true, ops.OpPrune: true,
	}
	got := e.runner.Ops()
	if len(got) < len(want) {
		t.Fatalf("注册的 op 数=%d, want >= %d（%v）", len(got), len(want), got)
	}
	have := map[ops.OpName]bool{}
	for _, n := range got {
		have[n] = true
	}
	for n := range want {
		if !have[n] {
			t.Errorf("缺少 op %q（§8 的 8 个原子操作必须齐全）", n)
		}
	}

	// 每个 op 都必须能 Name/Budget/Plan/Apply，否则它不可能被同一套编排调用。
	for _, name := range got {
		res, err := e.runner.Run(context.Background(), ops.Request{
			Op: name, Scope: store.ScopeUser,
			Trigger: ops.Trigger{Kind: ops.TriggerAgent, Actor: "test"},
		})
		if err != nil {
			var missingInput = strings.Contains(err.Error(), "需要输入")
			if !missingInput {
				t.Errorf("op %q 无法被统一编排调用: %v", name, err)
			}
			continue
		}
		if res.Key == "" {
			t.Errorf("op %q 未产生幂等 key", name)
		}
	}
}

// TestAutoAndAgentShareSameCodePath 是 D3 的核心断言：
// 同一个 op，自动触发与 Agent 显式调用**结果必须一致**（不是"都能跑"，是"结果相同"）。
func TestAutoAndAgentShareSameCodePath(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(e *env) ops.Request
	}{
		{"consolidate", func(e *env) ops.Request {
			return ops.Request{Op: ops.OpConsolidate, Scope: store.ScopeUser}
		}},
		{"decay", func(e *env) ops.Request {
			return ops.Request{Op: ops.OpDecay, Scope: store.ScopeUser}
		}},
		{"prune", func(e *env) ops.Request {
			return ops.Request{Op: ops.OpPrune, Scope: store.ScopeUser}
		}},
		{"promote", func(e *env) ops.Request {
			return ops.Request{Op: ops.OpPromote, Scope: store.ScopeUser,
				Input: ops.Input{Promote: &ops.PromoteInput{ConceptIDs: []string{"c-shared"}}}}
		}},
		{"merge", func(e *env) ops.Request {
			return ops.Request{Op: ops.OpMerge, Scope: store.ScopeUser,
				Input: ops.Input{Merge: &ops.MergeInput{FromNode: "n-a", ToNode: "n-b"}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 两条独立环境做同一件事：一条走触发源，一条走 Agent 入口。
			autoEnv := newEnv(t)
			agentEnv := newEnv(t)
			for _, e := range []*env{autoEnv, agentEnv} {
				e.seedSharedFixture()
			}

			autoReq := tc.build(autoEnv)
			autoReq.Trigger = ops.Trigger{Kind: ops.TriggerIdle, Actor: "auto", Reason: "idle"}
			autoRes, err := autoEnv.runner.Run(context.Background(), autoReq)
			if err != nil {
				t.Fatalf("自动触发失败: %v", err)
			}

			agentReq := tc.build(agentEnv)
			agentReq.Trigger = ops.Trigger{Kind: ops.TriggerAgent, Actor: "auto", Reason: "idle"}
			agentRes, err := agentEnv.maint.Runner().Run(context.Background(), agentReq)
			if err != nil {
				t.Fatalf("Agent 调用失败: %v", err)
			}

			if autoRes.Status != agentRes.Status {
				t.Errorf("两条路径的 Status 不同：auto=%s agent=%s（D3 要求行为一致）",
					autoRes.Status, agentRes.Status)
			}
			if len(autoRes.Steps) != len(agentRes.Steps) {
				t.Fatalf("两条路径的步骤数不同：auto=%d agent=%d", len(autoRes.Steps), len(agentRes.Steps))
			}
			for i := range autoRes.Steps {
				if autoRes.Steps[i].Step.Kind != agentRes.Steps[i].Step.Kind {
					t.Errorf("步骤 %d 的 Kind 不同：%q vs %q",
						i, autoRes.Steps[i].Step.Kind, agentRes.Steps[i].Step.Kind)
				}
				if autoRes.Steps[i].Diff.Changed != agentRes.Steps[i].Diff.Changed {
					t.Errorf("步骤 %d 的 Changed 不同", i)
				}
			}
			// 状态收敛到同一处：两条路径的 difference 只能来自审计元数据，不能来自业务结果。
			if got, want := autoEnv.countRows(`SELECT COUNT(1) FROM memory_redirects`),
				agentEnv.countRows(`SELECT COUNT(1) FROM memory_redirects`); got != want {
				t.Errorf("重定向数不同：auto=%d agent=%d", got, want)
			}
		})
	}
}

// seedSharedFixture 造一份两条路径共用的前置状态（节点/边/草稿概念/候选记忆）。
func (e *env) seedSharedFixture() {
	e.t.Helper()
	ctx := context.Background()
	for _, n := range []store.Node{
		{ID: "n-a", Scope: store.ScopeUser, CanonicalName: "张三(A)"},
		{ID: "n-b", Scope: store.ScopeUser, CanonicalName: "张三"},
	} {
		if err := e.db.Graph().PutNode(ctx, n); err != nil {
			e.t.Fatalf("put node: %v", err)
		}
	}
	if err := e.db.Graph().PutEdge(ctx, store.Edge{
		ID: "e-ab", FromID: "n-a", ToID: "n-b", Relation: store.RelationWorksAt,
		Weight: 1, TValid: 100, TCreated: 100,
	}); err != nil {
		e.t.Fatalf("put edge: %v", err)
	}
	if err := e.db.Store().PutConcept(ctx, store.Concept{
		ID: "c-shared", Name: "认证服务", Status: store.ConceptDraft,
		Origin: store.ConceptOriginImplicit,
	}); err != nil {
		e.t.Fatalf("put concept: %v", err)
	}
	e.putMemory(memory("m-1", store.ScopeUser, "c-shared"))
}

// ===========================================================================
// 2) 幂等：同一 key 重跑安全（不重复应用、不双写）
// ===========================================================================

// TestIdempotentRerunIsSafe 断言同一幂等 key 的第二次运行是 duplicate，且业务状态只变了一次。
func TestIdempotentRerunIsSafe(t *testing.T) {
	e := newEnv(t)
	e.seedSharedFixture()

	req := ops.Request{
		Op: ops.OpMerge, Scope: store.ScopeUser,
		Trigger: ops.Trigger{Kind: ops.TriggerAgent, Actor: "agent", Reason: "确认同一实体"},
		Input:   ops.Input{Merge: &ops.MergeInput{FromNode: "n-a", ToNode: "n-b", Reason: "同一个人"}},
	}

	first, err := e.runner.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("第一次运行: %v", err)
	}
	if first.Status != ops.RunApplied {
		t.Fatalf("第一次运行 Status=%s, want applied", first.Status)
	}
	redirects := e.countRows(`SELECT COUNT(1) FROM memory_redirects`)
	auditFirst := e.countRows(`SELECT COUNT(1) FROM audit_log`)

	second, err := e.runner.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("第二次运行（同 key）: %v", err)
	}
	if second.Status != ops.RunDuplicate {
		t.Fatalf("第二次运行 Status=%s, want duplicate（重跑必须安全跳过）", second.Status)
	}
	if len(second.Steps) != 0 {
		t.Errorf("重复运行应用了 %d 个步骤（不得重复应用）", len(second.Steps))
	}
	if got := e.countRows(`SELECT COUNT(1) FROM memory_redirects`); got != redirects {
		t.Errorf("重定向数 %d → %d（重复应用）", redirects, got)
	}
	if got := e.countRows(`SELECT COUNT(1) FROM audit_log`); got != auditFirst {
		t.Errorf("审计行数 %d → %d（重复运行不得伪造变更）", auditFirst, got)
	}

	// key 的形状必须是 hash(scope, op, 输入摘要)：同 scope 同 op 同输入 → 同 key；
	// 换 scope 或换输入 → 不同 key。
	key1, err := ops.IdempotencyKey(store.ScopeUser, ops.OpMerge, req.Input)
	if err != nil {
		t.Fatalf("IdempotencyKey: %v", err)
	}
	key2, _ := ops.IdempotencyKey(store.ScopeAgent, ops.OpMerge, req.Input)
	key3, _ := ops.IdempotencyKey(store.ScopeUser, ops.OpInvalidate, req.Input)
	key4, _ := ops.IdempotencyKey(store.ScopeUser, ops.OpMerge, ops.Input{})
	if key1 == key2 || key1 == key3 || key1 == key4 {
		t.Errorf("幂等 key 未随 (scope, op, 输入) 区分：%q %q %q %q", key1, key2, key3, key4)
	}
	if !strings.HasPrefix(key1, "merge-") {
		t.Errorf("幂等 key 前缀=%q, want merge-（可读性）", key1)
	}
}

// TestFailedRunCanBeRetried 断言 failed 的 key 允许重跑：一次瞬时故障不该把窗口永久卡死。
func TestFailedRunCanBeRetried(t *testing.T) {
	e := newEnv(t)
	// 缺 Writer 时 Extract 会显式降级（不是失败），因此这里用 Merge 指向不存在的节点制造失败。
	bad := ops.Request{
		Op: ops.OpMerge, Scope: store.ScopeUser,
		Input:   ops.Input{Merge: &ops.MergeInput{FromNode: "n-x", ToNode: "n-y"}},
		Trigger: ops.Trigger{Kind: ops.TriggerAgent},
	}
	if _, err := e.runner.Run(context.Background(), bad); err == nil {
		t.Fatal("指向不存在节点的 merge 应当报错")
	}
	var status string
	if err := e.raw.QueryRow(`SELECT status FROM op_runs LIMIT 1`).Scan(&status); err != nil {
		t.Fatalf("读 op_runs: %v", err)
	}
	if status != string(ops.RunFailed) {
		t.Fatalf("失败运行的台账状态=%q, want failed", status)
	}

	// 修正输入后必须能跑（key 不同，但同一 op 的后续运行不受影响）。
	e.seedSharedFixture()
	good := bad
	good.Input = ops.Input{Merge: &ops.MergeInput{FromNode: "n-a", ToNode: "n-b"}}
	res, err := e.runner.Run(context.Background(), good)
	if err != nil {
		t.Fatalf("修正后重跑: %v", err)
	}
	if res.Status != ops.RunApplied {
		t.Errorf("修正后 Status=%s, want applied", res.Status)
	}
}

// ===========================================================================
// 3) 可取消：ctx 取消不留半截状态
// ===========================================================================

// TestCancelLeavesNoPartialState 是"可取消"的硬断言：
// 提取 3 个 episode，中途取消 → **一条记忆都不该留下**（整体回滚）。
func TestCancelLeavesNoPartialState(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, id := range []string{"ep-1", "ep-2", "ep-3"} {
		if err := e.db.Store().PutEpisode(ctx, store.Episode{
			ID: id, Scope: store.ScopeUser, Content: "第 " + id + " 段语料的正文", At: 100,
		}); err != nil {
			t.Fatalf("put episode: %v", err)
		}
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	e.ext.onCall = func(n int, _ ops.EpisodeInput) {
		if n == 2 {
			cancel() // 第二个 episode 处理中被打断
		}
	}

	res, err := e.runner.Run(cancelCtx, ops.Request{
		Op: ops.OpExtract, Scope: store.ScopeUser,
		Trigger: ops.Trigger{Kind: ops.TriggerAgent},
	})
	if err == nil {
		t.Fatal("取消后必须返回错误（不得伪装成功）")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("返回的错误=%v, want context.Canceled（必须可判定）", err)
	}
	if res.Status != ops.RunCancelled {
		t.Errorf("Status=%s, want cancelled", res.Status)
	}
	if got := e.countRows(`SELECT COUNT(1) FROM memories`); got != 0 {
		t.Errorf("取消后残留 %d 条记忆（半截状态）；必须整体回滚", got)
	}
	if got := e.countRows(`SELECT COUNT(1) FROM memories_fts`); got != 0 {
		t.Errorf("取消后残留 %d 条 FTS 行（主表与索引不一致）", got)
	}
	// 台账必须留下 cancelled（用独立事务记），否则"为什么没有变化"无从回答。
	var status, note string
	if err := e.raw.QueryRow(`SELECT status, note FROM op_runs LIMIT 1`).Scan(&status, &note); err != nil {
		t.Fatalf("读 op_runs: %v", err)
	}
	if status != string(ops.RunCancelled) {
		t.Errorf("台账状态=%q, want cancelled", status)
	}
	if !strings.Contains(note, "cancel") {
		t.Errorf("台账备注=%q，应说明取消原因", note)
	}
}

// TestCanceledRunCanRerun 断言取消后重跑能正常完成（cancelled 不得永久锁死 key）。
func TestCanceledRunCanRerun(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.db.Store().PutEpisode(ctx, store.Episode{
		ID: "ep-1", Scope: store.ScopeUser, Content: "一段需要提取的语料正文", At: 100,
	}); err != nil {
		t.Fatalf("put episode: %v", err)
	}
	req := ops.Request{Op: ops.OpExtract, Scope: store.ScopeUser}

	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := e.runner.Run(cancelCtx, req); err == nil {
		t.Fatal("已取消的 ctx 应当报错")
	}
	res, err := e.runner.Run(ctx, req)
	if err != nil {
		t.Fatalf("取消后重跑: %v", err)
	}
	if res.Status != ops.RunApplied {
		t.Errorf("重跑 Status=%s, want applied", res.Status)
	}
	if got := e.countRows(`SELECT COUNT(1) FROM memories`); got != 1 {
		t.Errorf("重跑后记忆数=%d, want 1", got)
	}
}

// ===========================================================================
// 4) 生命周期状态机（TD §7 的四条纪律）
// ===========================================================================

// TestStateMachineDisciplines 覆盖四条纪律里的 (a)(b)(d) 与合法/非法迁移。
func TestStateMachineDisciplines(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	// Rejected 是合法状态，且是终态（拒写就是拒写，不复活）。
	if err := ops.ValidateTransition(ops.StatusCandidate, ops.StatusRejected); err != nil {
		t.Errorf("Candidate → Rejected 应合法: %v", err)
	}
	if err := ops.ValidateTransition(ops.StatusRejected, ops.StatusActive); err == nil {
		t.Error("Rejected → Active 应非法（状态机里没有这条边）")
	}
	if err := ops.ValidateTransition(ops.StatusActive, ops.StatusCandidate); err == nil {
		t.Error("Active → Candidate 应非法")
	}
	if err := ops.ValidateTransition(ops.StatusArchived, ops.StatusActive); err == nil {
		t.Error("Archived → Active 应非法（只能被 Prune 删除）")
	}
	for _, ok := range [][2]ops.MemoryStatus{
		{ops.StatusCandidate, ops.StatusActive},
		{ops.StatusActive, ops.StatusInvalidated},
		{ops.StatusActive, ops.StatusStale},
		{ops.StatusInvalidated, ops.StatusActive},
		{ops.StatusInvalidated, ops.StatusArchived},
		{ops.StatusStale, ops.StatusArchived},
	} {
		if err := ops.ValidateTransition(ok[0], ok[1]); err != nil {
			t.Errorf("%s → %s 应合法: %v", ok[0], ok[1], err)
		}
	}

	// (d) Rejected 也落库：内容过短的记忆必须留下 Rejected 状态行。
	e.putMemory(store.Memory{
		ID: "m-short", Scope: store.ScopeUser, Kind: store.KindSemantic,
		Content: "太短", TValid: 100, Importance: 0.5,
	})
	e.putMemory(memory("m-ok", store.ScopeUser, ""))
	res, err := e.runner.Run(ctx, ops.Request{Op: ops.OpConsolidate, Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("consolidate: %v", err)
	}
	if res.Status != ops.RunApplied {
		t.Fatalf("consolidate Status=%s, want applied", res.Status)
	}
	if st := e.stateStatus("m-short"); st != string(ops.StatusRejected) {
		t.Errorf("过短记忆状态=%q, want rejected（Rejected 必须落库）", st)
	}
	if st := e.stateStatus("m-ok"); st != string(ops.StatusActive) {
		t.Errorf("正常记忆状态=%q, want active", st)
	}

	// (b) importance 四个分项各一列，且总分 = 四项乘积。
	base := e.scalarFloat(`SELECT importance_base FROM memory_state WHERE memory_id='m-ok'`)
	rec := e.scalarFloat(`SELECT importance_recency FROM memory_state WHERE memory_id='m-ok'`)
	acc := e.scalarFloat(`SELECT importance_access_boost FROM memory_state WHERE memory_id='m-ok'`)
	grade := e.scalarFloat(`SELECT importance_grade FROM memory_state WHERE memory_id='m-ok'`)
	total := e.scalarFloat(`SELECT importance FROM memory_state WHERE memory_id='m-ok'`)
	if base <= 0 || rec <= 0 || acc <= 0 || grade <= 0 {
		t.Fatalf("四个分项都必须 > 0：base=%v rec=%v access=%v grade=%v", base, rec, acc, grade)
	}
	if diff := (base*rec*acc*grade - total); diff > 1e-9 || diff < -1e-9 {
		t.Errorf("importance=%v 不等于 base×recency×access_boost×grade=%v",
			total, base*rec*acc*grade)
	}
}

// TestInvalidatedIsNotDeletion 是纪律 (a)：Invalidated 不等于删除，as_of 仍可回溯。
func TestInvalidatedIsNotDeletion(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.db.Store().PutEpisode(ctx, store.Episode{
		ID: "ep-1", Scope: store.ScopeUser, Content: "原始语料正文", At: 100,
	}); err != nil {
		t.Fatalf("put episode: %v", err)
	}
	m := memory("m-1", store.ScopeUser, "")
	// 世界时间：100 起成立；系统时间：100 起已被 Gmem 知晓。
	// 两者都要早于下面 as_of=300，否则"回溯不到"是 as_of 语义正确的结果，不是缺陷。
	m.TValid = 100
	m.TCreated = 100
	e.putMemory(m)

	res, err := e.runner.Run(ctx, ops.Request{
		Op: ops.OpInvalidate, Scope: store.ScopeUser,
		Trigger: ops.Trigger{Kind: ops.TriggerAgent, Actor: "agent", Reason: "旧事实作废"},
		Input:   ops.Input{Invalidate: &ops.InvalidateInput{MemoryID: "m-1", TInvalid: 500}},
	})
	if err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	if res.Status != ops.RunApplied {
		t.Fatalf("Status=%s, want applied", res.Status)
	}

	// 主表行仍在（不物理删除），t_invalid 已置。
	if got := e.countRows(`SELECT COUNT(1) FROM memories WHERE id='m-1'`); got != 1 {
		t.Fatalf("失效后主表行数=%d, want 1（Invalidated 不等于删除）", got)
	}
	if got := e.scalar(`SELECT t_invalid FROM memories WHERE id='m-1'`); got != "500" {
		t.Errorf("t_invalid=%q, want 500", got)
	}

	// 默认读不到（当前有效事实里没有它）。
	cur, err := e.db.Store().ListMemories(ctx, store.MemoryQuery{Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("ListMemories: %v", err)
	}
	if len(cur) != 0 {
		t.Errorf("默认查询返回 %d 条已失效记忆（应只返回当前有效事实）", len(cur))
	}

	// as_of 能回溯到失效之前（这是 Zep 语义，也是 Invalidated ≠ 删除的判据）。
	asOf := int64(300) // 100 <= 300 < 500：失效之前的时刻
	old, err := e.db.Store().ListMemories(ctx, store.MemoryQuery{Scope: store.ScopeUser, AsOf: &asOf})
	if err != nil {
		t.Fatalf("ListMemories(as_of): %v", err)
	}
	if len(old) != 1 || old[0].ID != "m-1" {
		t.Errorf("as_of=300 回溯结果=%v, want [m-1]（失效不代表历史消失）", old)
	}

	// 失效幂等：再跑一次不产生第二次 diff。
	again, err := e.runner.Run(ctx, ops.Request{
		Op: ops.OpInvalidate, Scope: store.ScopeUser,
		Input: ops.Input{Invalidate: &ops.InvalidateInput{MemoryID: "m-1", TInvalid: 900}},
	})
	if err != nil {
		t.Fatalf("重复 invalidate: %v", err)
	}
	if again.Status != ops.RunNoop {
		t.Errorf("重复 invalidate Status=%s, want noop", again.Status)
	}
	if got := e.scalar(`SELECT t_invalid FROM memories WHERE id='m-1'`); got != "500" {
		t.Errorf("重复失效改写了 t_invalid=%q（幂等被破坏）", got)
	}
}

// TestArchivedThenPruneIsOnlyPhysicalDelete 是纪律 (c)：
// 只有 Archived 会被物理删除，且必须显式触发。
func TestArchivedThenPruneIsOnlyPhysicalDelete(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.db.Store().PutEpisode(ctx, store.Episode{
		ID: "ep-1", Scope: store.ScopeUser, Content: "原始语料正文", At: 100,
	}); err != nil {
		t.Fatalf("put episode: %v", err)
	}
	e.putMemory(memory("m-arch", store.ScopeUser, ""))
	e.putMemory(memory("m-live", store.ScopeUser, ""))

	// m-arch 走真实路径：Candidate → Active → Invalidated →（保留期满）Archived。
	if _, err := e.runner.Run(ctx, ops.Request{Op: ops.OpConsolidate, Scope: store.ScopeUser}); err != nil {
		t.Fatalf("consolidate #1: %v", err)
	}
	if _, err := e.runner.Run(ctx, ops.Request{
		Op: ops.OpInvalidate, Scope: store.ScopeUser,
		Input: ops.Input{Invalidate: &ops.InvalidateInput{MemoryID: "m-arch"}},
	}); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	// 保留期满：把时钟推过 ArchiveAfterDays。
	e.clk.Advance(time.Duration(e.cfg.ArchiveAfterDays+1) * 24 * time.Hour)
	if _, err := e.runner.Run(ctx, ops.Request{Op: ops.OpConsolidate, Scope: store.ScopeUser}); err != nil {
		t.Fatalf("consolidate #2: %v", err)
	}
	if st := e.stateStatus("m-arch"); st != string(ops.StatusArchived) {
		t.Fatalf("m-arch 状态=%q, want archived（保留期满应归档）", st)
	}

	// Prune：只删 Archived；Active 的 m-live 必须活着。
	res, err := e.runner.Run(ctx, ops.Request{
		Op: ops.OpPrune, Scope: store.ScopeUser,
		Trigger: ops.Trigger{Kind: ops.TriggerAgent, Actor: "maintenance", Reason: "维护窗口"},
		Input:   ops.Input{IncludeDrafts: new(bool)},
	})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if res.Status != ops.RunApplied {
		t.Fatalf("prune Status=%s, want applied", res.Status)
	}
	if got := e.countRows(`SELECT COUNT(1) FROM memories WHERE id='m-arch'`); got != 0 {
		t.Errorf("Archived 记忆未被物理删除（rows=%d）", got)
	}
	if got := e.countRows(`SELECT COUNT(1) FROM memories WHERE id='m-live'`); got != 1 {
		t.Errorf("Active 记忆被 Prune 删掉了（rows=%d）——默认只作用于 Archived", got)
	}
	if got := e.countRows(`SELECT COUNT(1) FROM memories_fts WHERE id='m-arch'`); got != 0 {
		t.Errorf("物理删除后 FTS 仍残留 %d 行（索引与主表不一致）", got)
	}
	if got := e.countRows(`SELECT COUNT(1) FROM memory_state WHERE memory_id='m-arch'`); got != 0 {
		t.Errorf("物理删除后残留 %d 条孤儿状态行", got)
	}
}

// TestPruneNeverTouchesInvalidatedByDefault 断言 Prune 的默认范围：
// Invalidated **不在**默认清理范围内（它还要支撑 as_of 回溯）。
func TestPruneNeverTouchesInvalidatedByDefault(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.db.Store().PutEpisode(ctx, store.Episode{
		ID: "ep-1", Scope: store.ScopeUser, Content: "原始语料正文", At: 100,
	}); err != nil {
		t.Fatalf("put episode: %v", err)
	}
	e.putMemory(memory("m-inv", store.ScopeUser, ""))
	if _, err := e.runner.Run(ctx, ops.Request{Op: ops.OpConsolidate, Scope: store.ScopeUser}); err != nil {
		t.Fatalf("consolidate: %v", err)
	}
	if _, err := e.runner.Run(ctx, ops.Request{
		Op: ops.OpInvalidate, Scope: store.ScopeUser,
		Input: ops.Input{Invalidate: &ops.InvalidateInput{MemoryID: "m-inv"}},
	}); err != nil {
		t.Fatalf("invalidate: %v", err)
	}

	if _, err := e.runner.Run(ctx, ops.Request{
		Op: ops.OpPrune, Scope: store.ScopeUser, Input: ops.Input{IncludeDrafts: new(bool)},
	}); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if got := e.countRows(`SELECT COUNT(1) FROM memories WHERE id='m-inv'`); got != 1 {
		t.Errorf("默认 Prune 删掉了 Invalidated 记忆（rows=%d）——TD §7 明确默认只作用于 Archived", got)
	}
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func (e *env) stateStatus(memoryID string) string {
	e.t.Helper()
	return e.scalar(`SELECT status FROM memory_state WHERE memory_id = ?`, memoryID)
}

func (e *env) scalarFloat(query string, args ...any) float64 {
	e.t.Helper()
	var v float64
	if err := e.raw.QueryRow(query, args...).Scan(&v); err != nil {
		e.t.Fatalf("scalar float %q: %v", query, err)
	}
	return v
}
