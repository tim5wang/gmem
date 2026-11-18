package p3verify

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/tim5wang/gmem/ops"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// 攻击面 2：自动触发与 Agent 显式调用是否**真的同一条代码路径**（D3）
//
// TD §8 的原话："这个设计的全部价值在于：自动触发和 Agent 调用，走的是同一套代码。
// 两条路径分开写，会分别长出 bug，而且行为不可比。"
//
// 因此本文件不比"看起来一样"，而是逐项比对**外部可观测物**：
//
//	1. 产生的业务结果（memory_state 的状态迁移序列）
//	2. audit_log 的行结构（step_kind 序列、changed 标记、before/after 有无）
//	3. 幂等 key 的生成规则（同一个 hash 函数，只有 window 参与差异）
//
// 三条都比对通过，才能说"同一条路径"；任何一条分叉都说明存在两套实现。
// ===========================================================================

// auditShape 是审计行的"形状"（去掉时间与触发源这类**应当**不同的字段）。
type auditShape struct {
	Op       string
	StepKind string
	Subject  string
	Changed  bool
	HasBefore bool
	HasAfter  bool
}

// shapesOf 读该 op_id 下的审计形状（按 seq 升序）。
func (e *env) shapesOf(opID string) []auditShape {
	e.t.Helper()
	rows, err := e.raw.Query(`SELECT op, step_kind, subject, changed, before_json, after_json
		FROM audit_log WHERE op_id = ? ORDER BY seq`, opID)
	if err != nil {
		e.t.Fatalf("读 audit_log(%s): %v", opID, err)
	}
	defer rows.Close()
	var out []auditShape
	for rows.Next() {
		var s auditShape
		var changed int
		var before, after string
		if err := rows.Scan(&s.Op, &s.StepKind, &s.Subject, &changed, &before, &after); err != nil {
			e.t.Fatalf("scan audit: %v", err)
		}
		s.Changed = changed != 0
		s.HasBefore = strings.TrimSpace(before) != "" && before != "null"
		s.HasAfter = strings.TrimSpace(after) != "" && after != "null"
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		e.t.Fatalf("iterate audit: %v", err)
	}
	return out
}

// opIDPatternOK 校验幂等 key 的生成规则：`<op>-<32 位十六进制>`。
//
// 为什么单独断言它：若某条触发源自己拼 key（例如加前缀 "auto:"），
// 两条路径的幂等语义立刻不同——自动触发能跑、Agent 调用被当重复，或反过来说不清。
func opIDPatternOK(op ops.OpName, key string) bool {
	prefix := string(op) + "-"
	if !strings.HasPrefix(key, prefix) {
		return false
	}
	hex := key[len(prefix):]
	if len(hex) != 32 {
		return false
	}
	for _, r := range hex {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

// workingSet 造同一份输入数据（两条路径必须面对同一世界）。
func workingSet(t *testing.T, e *env) {
	t.Helper()
	fresh := mem("m-live", store.ScopeUser, "")
	fresh.TValid, fresh.TCreated = e.clk.Now().Unix(), e.clk.Now().Unix()
	e.put(fresh)

	short := store.Memory{
		ID: "m-short", Scope: store.ScopeUser, Kind: store.KindSemantic,
		Content: "太短", TValid: 1000, TCreated: 1000,
	}
	e.put(short)
}

// TestAutoAndAgentShareIdenticalPath 比对自动（after_write）与 Agent（显式）两条路径。
func TestAutoAndAgentShareIdenticalPath(t *testing.T) {
	type run struct {
		key     string
		status  ops.RunStatus
		trigger ops.TriggerKind
		seq     []string // memory_id → status（排序后压平）
		shapes  []auditShape
		wantAt  int64 // 该次运行的扫描窗口（时钟毫秒）
	}

	exec := func(t *testing.T, via string) run {
		t.Helper()
		e := newEnv(t)
		ctx := context.Background()
		workingSet(t, e)

		// 两条路径都必须拿到"当前时刻"作为扫描窗口。
		window := e.clk.Now().UnixMilli()
		switch via {
		case "after_write":
			e.trig.AfterWrite(ctx, store.ScopeUser, ops.OpConsolidate, ops.Input{})
			e.sched.Advance(10 * time.Second) // 触发去抖
		case "agent":
			if _, err := e.maint.Consolidate(ctx, ops.ConsolidateRequest{Scope: store.ScopeUser}); err != nil {
				t.Fatalf("agent consolidate: %v", err)
			}
		}

		var key, status, trig string
		if err := e.raw.QueryRow(`SELECT key, status, trigger_kind FROM op_runs`).Scan(&key, &status, &trig); err != nil {
			t.Fatalf("%s: 读 op_runs: %v", via, err)
		}
		// after_write 的窗口是 fire 时刻（推进 10s 之后），Agent 是提交时刻。
		if via == "after_write" {
			window = e.clk.Now().UnixMilli()
		}

		rows, err := e.raw.Query(`SELECT memory_id, status FROM memory_state ORDER BY memory_id`)
		if err != nil {
			t.Fatalf("%s: 读 memory_state: %v", via, err)
		}
		defer rows.Close()
		var seq []string
		for rows.Next() {
			var id, st string
			if err := rows.Scan(&id, &st); err != nil {
				t.Fatalf("scan state: %v", err)
			}
			seq = append(seq, id+"="+st)
		}
		return run{key: key, status: ops.RunStatus(status), trigger: ops.TriggerKind(trig),
			seq: seq, shapes: e.shapesOf(key), wantAt: window}
	}

	auto := exec(t, "after_write")
	agent := exec(t, "agent")

	// 1) 幂等 key 生成规则一致（同一个 hash，只是 window 不同）。
	for name, r := range map[string]run{"after_write": auto, "agent": agent} {
		if !opIDPatternOK(ops.OpConsolidate, r.key) {
			t.Errorf("%s: 幂等 key=%q 不符合 <op>-<hex32> 规则", name, r.key)
		}
		want, err := ops.IdempotencyKey(store.ScopeUser, ops.OpConsolidate, ops.Input{Window: r.wantAt})
		if err != nil {
			t.Fatalf("IdempotencyKey: %v", err)
		}
		if r.key != want {
			t.Errorf("%s: 实际 key=%q, 按同一规则应为 %q（说明该路径自己拼了 key）", name, r.key, want)
		}
	}

	// 2) 业务结果一致。
	if strings.Join(auto.seq, ",") != strings.Join(agent.seq, ",") {
		t.Errorf("两条路径的状态迁移不同：\n after_write=%v\n agent      =%v", auto.seq, agent.seq)
	}
	if auto.status != agent.status {
		t.Errorf("两条路径的运行结论不同：after_write=%s, agent=%s", auto.status, agent.status)
	}

	// 3) audit_log 形状一致（step_kind 序列 + changed + before/after 有无）。
	if len(auto.shapes) != len(agent.shapes) {
		t.Fatalf("审计条数不同：after_write=%d, agent=%d", len(auto.shapes), len(agent.shapes))
	}
	for i := range auto.shapes {
		if auto.shapes[i] != agent.shapes[i] {
			t.Errorf("第 %d 条审计形状不同：\n after_write=%+v\n agent      =%+v",
				i, auto.shapes[i], agent.shapes[i])
		}
	}

	// 4) 只有 trigger_kind 该不同（其余全部一致，这正是设计允许的唯一差异）。
	if auto.trigger != ops.TriggerAfterWrite {
		t.Errorf("自动触发的 trigger_kind=%q, want %q", auto.trigger, ops.TriggerAfterWrite)
	}
	if agent.trigger != ops.TriggerAgent {
		t.Errorf("Agent 触发的 trigger_kind=%q, want %q", agent.trigger, ops.TriggerAgent)
	}
	if auto.trigger == agent.trigger {
		t.Error("两条路径的 trigger_kind 相同：那就无法在审计里区分是谁发起的")
	}
	// 而幂等 key 除 window 外必须由同一规则生成（同 op、同 scope → 同窗口时同 key）。
	// 这里断言的是：两条路径的 key 差异**只**来自 window。
	if auto.wantAt != agent.wantAt && auto.key == agent.key {
		t.Error("窗口不同却得到相同 key（window 未参与幂等摘要，重跑语义会错）")
	}
}

// TestTriggersAgentEntryMatchesAfterWrite 覆盖 **Triggers.Agent** 这个公开入口。
//
// 为什么必须单独覆盖：`Triggers` 有四个公开触发入口（AfterWrite / Threshold /
// Idle / Agent），而 Maintainer 走的是 runner.Run 直连。若只测 Maintainer，
// Triggers.Agent 就完全没被测到——变异验证证实了这一点：把它的 req.Key 改成
// 自拼前缀（破坏幂等 key 生成规则）时，原有用例 **全部存活**。
//
// 断言：同一 op、同 scope 下，Triggers.Agent 与 after_write 的
//   - 幂等 key 生成规则一致（<op>-<hex32>，且等于按同一窗口算出的值）
//   - 审计形状一致
//   - 只有 trigger_kind / actor 不同
func TestTriggersAgentEntryMatchesAfterWrite(t *testing.T) {
	run := func(t *testing.T, via string) (key string, status ops.RunStatus, trig ops.TriggerKind,
		window int64, shapes []auditShape, seq []string) {
		t.Helper()
		e := newEnv(t)
		ctx := context.Background()
		workingSet(t, e)

		switch via {
		case "after_write":
			e.trig.AfterWrite(ctx, store.ScopeUser, ops.OpConsolidate, ops.Input{})
			e.sched.Advance(10 * time.Second)
			window = e.clk.Now().UnixMilli() // 窗口 = fire 时刻
		case "triggers_agent":
			window = e.clk.Now().UnixMilli() // 窗口 = 提交时刻
			res, err := e.trig.Agent(ctx, ops.Request{
				Op: ops.OpConsolidate, Scope: store.ScopeUser,
				Trigger: ops.Trigger{Kind: ops.TriggerAgent, Actor: "agent-x", Reason: "Agent 显式整理"},
			})
			if err != nil {
				t.Fatalf("Triggers.Agent: %v", err)
			}
			status = res.Status
		}

		var st string
		if err := e.raw.QueryRow(`SELECT key, status, trigger_kind FROM op_runs`).
			Scan(&key, &st, (*string)(&trig)); err != nil {
			t.Fatalf("%s: 读 op_runs: %v", via, err)
		}
		if status == "" {
			status = ops.RunStatus(st)
		}

		rows, err := e.raw.Query(`SELECT memory_id, status FROM memory_state ORDER BY memory_id`)
		if err != nil {
			t.Fatalf("%s: 读 memory_state: %v", via, err)
		}
		defer rows.Close()
		for rows.Next() {
			var id, s string
			if err := rows.Scan(&id, &s); err != nil {
				t.Fatalf("scan: %v", err)
			}
			seq = append(seq, id+"="+s)
		}
		return key, status, trig, window, e.shapesOf(key), seq
	}

	aKey, aStatus, aTrig, aWin, aShapes, aSeq := run(t, "after_write")
	gKey, gStatus, gTrig, gWin, gShapes, gSeq := run(t, "triggers_agent")

	// 幂等 key 必须由同一规则生成（变异点：自拼前缀会在这里变红）。
	for name, r := range map[string]struct {
		key    string
		window int64
	}{"after_write": {aKey, aWin}, "triggers_agent": {gKey, gWin}} {
		if !opIDPatternOK(ops.OpConsolidate, r.key) {
			t.Errorf("%s: 幂等 key=%q 不符合 <op>-<hex32> 规则（该入口自己拼了 key）", name, r.key)
		}
		want, err := ops.IdempotencyKey(store.ScopeUser, ops.OpConsolidate, ops.Input{Window: r.window})
		if err != nil {
			t.Fatalf("IdempotencyKey: %v", err)
		}
		if r.key != want {
			t.Errorf("%s: key=%q, 按同一规则应为 %q", name, r.key, want)
		}
	}

	// 业务结果与审计形状一致。
	if aStatus != gStatus {
		t.Errorf("结论不同：after_write=%s, Triggers.Agent=%s", aStatus, gStatus)
	}
	if strings.Join(aSeq, ",") != strings.Join(gSeq, ",") {
		t.Errorf("状态迁移不同：\n after_write=%v\n Triggers.Agent=%v", aSeq, gSeq)
	}
	if len(aShapes) != len(gShapes) {
		t.Fatalf("审计条数不同：after_write=%d, Triggers.Agent=%d", len(aShapes), len(gShapes))
	}
	for i := range aShapes {
		if aShapes[i] != gShapes[i] {
			t.Errorf("第 %d 条审计形状不同：\n after_write=%+v\n Triggers.Agent=%+v",
				i, aShapes[i], gShapes[i])
		}
	}

	// 只有 trigger_kind 该不同。
	if aTrig != ops.TriggerAfterWrite {
		t.Errorf("after_write 的 trigger_kind=%q", aTrig)
	}
	if gTrig != ops.TriggerAgent {
		t.Errorf("Triggers.Agent 的 trigger_kind=%q, want agent", gTrig)
	}
}

// TestThresholdAndAgentShareIdenticalPath 用 threshold 触发源再做一次同样的比对。
//
// 为什么不止测 after_write：threshold 走的是**同步**的 Runner.Run（不经去抖），
// 它是"另一条"入口，最容易被写成顺手复制一份逻辑的地方。
func TestThresholdAndAgentShareIdenticalPath(t *testing.T) {
	exec := func(t *testing.T, via string) (status ops.RunStatus, seq []string, shapes []auditShape) {
		t.Helper()
		e := newEnv(t, func(c *ops.Config) { c.Threshold = 1 })
		ctx := context.Background()
		workingSet(t, e)

		switch via {
		case "threshold":
			fired, res, err := e.trig.Threshold(ctx, store.ScopeUser, ops.OpConsolidate, ops.Input{})
			if err != nil {
				t.Fatalf("threshold: %v", err)
			}
			if !fired {
				t.Fatalf("threshold 未触发（未整理条目已达 Threshold=1）")
			}
			status = res.Status
		case "agent":
			res, err := e.maint.Consolidate(ctx, ops.ConsolidateRequest{Scope: store.ScopeUser})
			if err != nil {
				t.Fatalf("agent: %v", err)
			}
			status = res.Status
		}

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
			seq = append(seq, id+"="+st)
		}
		var key string
		if err := e.raw.QueryRow(`SELECT key FROM op_runs`).Scan(&key); err != nil {
			t.Fatalf("读 op_runs: %v", err)
		}
		return status, seq, e.shapesOf(key)
	}

	thStatus, thSeq, thShapes := exec(t, "threshold")
	agStatus, agSeq, agShapes := exec(t, "agent")

	if thStatus != agStatus {
		t.Errorf("threshold=%s, agent=%s（同一 op 的结论应相同）", thStatus, agStatus)
	}
	if strings.Join(thSeq, ",") != strings.Join(agSeq, ",") {
		t.Errorf("状态迁移不同：\n threshold=%v\n agent    =%v", thSeq, agSeq)
	}
	if len(thShapes) != len(agShapes) {
		t.Fatalf("审计条数不同：threshold=%d, agent=%d", len(thShapes), len(agShapes))
	}
	for i := range thShapes {
		if thShapes[i] != agShapes[i] {
			t.Errorf("第 %d 条审计形状不同：\n threshold=%+v\n agent    =%+v", i, thShapes[i], agShapes[i])
		}
	}
}

// TestAuditRebuildsFullCausalChain 只凭 audit_log 重建"谁在何时因何触发了什么变更"。
//
// 断言的是**因果链可拼**，不是"有若干行"：
//
//	op_runs.key ←→ audit_log.op_id（同一次运行）
//	trigger_kind / actor / reason（谁、怎么触发的、为什么）
//	at（何时）
//	step_kind + subject + before_json/after_json（改了什么）
//
// 任何一环缺失都让"这条记忆为什么变成现在这样"无法回答。
func TestAuditRebuildsFullCausalChain(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	m := mem("m-chain", store.ScopeUser, "")
	m.TValid, m.TCreated = e.clk.Now().Unix(), e.clk.Now().Unix()
	e.put(m)

	// 三次运行：Consolidate（Active）→ Invalidate（矛盾出现）→ Consolidate（归档判定）
	if _, err := e.runner.Run(ctx, ops.Request{
		Op: ops.OpConsolidate, Scope: store.ScopeUser,
		Trigger: ops.Trigger{Kind: ops.TriggerIdle, Actor: "idle-loop", Reason: "空闲窗口推进状态机"},
	}); err != nil {
		t.Fatalf("consolidate#1: %v", err)
	}
	inv, err := e.maint.Supersede(ctx, ops.SupersedeRequest{
		Scope: store.ScopeUser, MemoryID: "m-chain", Actor: "agent-7",
		Reason: "用户说这个部署环境已变更",
	})
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	if inv.Status != ops.RunApplied {
		t.Fatalf("supersede 状态=%s, want applied", inv.Status)
	}

	// 按 subject 拉全history：必须能按时间顺序拼出因果链。
	entries, err := e.maint.Audit(ctx, ops.AuditQuery{Subject: "m-chain"})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if len(entries) < 2 {
		t.Fatalf("审计条数=%d, want >=2（状态推进 + 失效）", len(entries))
	}

	// 链的每一环都要能回答四个问题。
	for _, a := range entries {
		if a.OpID == "" {
			t.Errorf("seq=%d 缺 op_id（无法定位到哪一次运行）", a.Seq)
		}
		if !a.Trigger.Valid() {
			t.Errorf("seq=%d trigger=%q 非法（无法回答「怎么触发」）", a.Seq, a.Trigger)
		}
		if a.Actor == "" {
			t.Errorf("seq=%d 缺 actor（无法回答「谁」）", a.Seq)
		}
		if a.Reason == "" {
			t.Errorf("seq=%d 缺 reason（无法回答「为什么」）", a.Seq)
		}
		if a.At <= 0 {
			t.Errorf("seq=%d 缺时间（无法回答「何时」）", a.Seq)
		}
		if a.StepKind == "" || a.Subject == "" {
			t.Errorf("seq=%d 缺 step_kind/subject（无法回答「改了什么」）", a.Seq)
		}
		// before/after 至少一侧可解析为 JSON（变更可被重放/对比）。
		for name, js := range map[string]string{"before": a.BeforeJSON, "after": a.AfterJSON} {
			if strings.TrimSpace(js) == "" {
				continue
			}
			var v any
			if err := json.Unmarshal([]byte(js), &v); err != nil {
				t.Errorf("seq=%d %s_json 不是合法 JSON: %v", a.Seq, name, err)
			}
		}
	}

	// op_id → op_runs 必须对得上（同一次运行的全部步骤共用一个 key）。
	keys := map[string]struct{}{}
	for _, a := range entries {
		keys[a.OpID] = struct{}{}
	}
	for key := range keys {
		var op, trig, actor, reason string
		if err := e.raw.QueryRow(`SELECT op, trigger_kind, actor, reason FROM op_runs WHERE key = ?`, key).
			Scan(&op, &trig, &actor, &reason); err != nil {
			t.Errorf("审计 op_id=%s 在 op_runs 里找不到（因果链断裂）: %v", key, err)
			continue
		}
		if actor == "" || reason == "" || trig == "" || op == "" {
			t.Errorf("op_runs(%s) 元数据不完整: op=%q trig=%q actor=%q reason=%q",
				key, op, trig, actor, reason)
		}
	}

	// 时间必须单调（seq 升序 = 时间升序），否则"因果"只是并列。
	for i := 1; i < len(entries); i++ {
		if entries[i].At < entries[i-1].At {
			t.Errorf("审计时间倒退：seq=%d at=%d < seq=%d at=%d",
				entries[i].Seq, entries[i].At, entries[i-1].Seq, entries[i-1].At)
		}
	}

	// 最后一次失效必须能从审计里读出来（谁宣布作废、为什么）。
	var sawInvalidate bool
	for _, a := range entries {
		if a.Op != ops.OpInvalidate {
			continue
		}
		sawInvalidate = true
		if a.Actor != "agent-7" {
			t.Errorf("失效审计的 actor=%q, want agent-7", a.Actor)
		}
		if !strings.Contains(a.Reason, "部署环境已变更") {
			t.Errorf("失效审计的 reason=%q，读不出「为什么」", a.Reason)
		}
		if !strings.Contains(a.AfterJSON, "invalidated") {
			t.Errorf("失效审计的 after=%q 未记录目标状态", a.AfterJSON)
		}
	}
	if !sawInvalidate {
		t.Error("审计链里缺少 invalidate 这一步")
	}
}

// TestStepsAreOrderedDeterministically 断言同一次运行内步骤顺序稳定。
//
// 为什么重要：Plan 用 map 迭代时步骤顺序会随机，导致审计序列不可比、
// 也无法做 as_of 回放。这里连跑两次（不同窗口）比对 step 序列。
func TestStepsAreOrderedDeterministically(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 12; i++ {
		m := mem("m-det-"+itoa(i), store.ScopeUser, "c-"+itoa(i%3))
		m.TValid, m.TCreated = e.clk.Now().Unix(), e.clk.Now().Unix()
		e.put(m)
	}

	var seqs [][]string
	for round := 0; round < 2; round++ {
		res := e.mustRun(ops.Request{Op: ops.OpDecay, Scope: store.ScopeUser})
		var seq []string
		for _, s := range res.Steps {
			seq = append(seq, s.Step.ID)
		}
		seqs = append(seqs, seq)
		e.clk.Advance(time.Hour) // 新窗口
	}

	if len(seqs[0]) == 0 {
		t.Skip("本次用例没有产生步骤（时钟与数据不匹配），无顺序可比")
	}
	if strings.Join(seqs[0], ",") != strings.Join(seqs[1], ",") {
		a := append([]string(nil), seqs[0]...)
		b := append([]string(nil), seqs[1]...)
		sort.Strings(a)
		sort.Strings(b)
		if strings.Join(a, ",") == strings.Join(b, ",") {
			t.Errorf("步骤集合相同但顺序不同（map 迭代未排序）：\n round0=%v\n round1=%v", seqs[0], seqs[1])
		} else {
			t.Logf("两轮步骤集合不同（数据随时间变化，属预期）：\n round0=%v\n round1=%v", seqs[0], seqs[1])
		}
	}
}
