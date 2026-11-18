package p3verify

import (
	"context"
	"testing"

	"github.com/tim5wang/gmem/ops"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// D15 的第二个缺口：Promote(All) 的"在用"判据只由 Extract 一侧填充
// ===========================================================================

// TestPromoteAllMissesStructuredWrites 是【可后续】的最小可复现用例。
//
// # 现象
//
// `Promote{All:true}` 用 `GetConceptStat(...).UseCount >= MinUse`（默认 1）判断
// "这个 draft 在被使用"。但 `concept_usage` 只有**一个**填充点：
// `extractOp.Apply` 里的 `TouchConceptUsage`（ops/ops_scan.go）。
//
// 于是任何**不经 Extract** 的写入路径——例如 TD §10 的结构化写入
// （`recall.Writer.PutMemory` / `WriteBundle`，宿主直接给已抽好的记忆）——
// 隐式长出来的 draft 永远没有统计行，`GetConceptStat` 返回 ok=false，
// 于是 `if ok && stat.UseCount >= minUse` 恒假：**这些草稿永远不被 All 认证**。
//
// # 生产形状
//
// 宿主用结构化写入（省一次 LLM，TD §10 明确鼓励）+ 定时 `Promote(All)` 做 D15 清理。
// 结果是草稿表持续增长而清理永远空转，且 `RunNoop` 是"成功"返回值，没有任何告警。
//
// # 与 Prune 的不对称
//
// Prune 一侧有兜底：`ListStaleDrafts` 会给缺失统计行的 draft 补 (use_count=0,
// last_used_at=0)，所以孤儿草稿照样能被清掉。Promote 一侧没有对应的兜底。
//
// # 判定
//
// 期望：draft 的"在用"判据应能覆盖所有写入路径（例如 memories 是否有引用，
// 与 CountConceptRefs 同口径），而不是依赖单一 op 的副作用写统计。
// 默认不红，GMEM_VERIFY_DEFERRED=1 时红。
func TestPromoteAllMissesStructuredWrites(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	// 结构化写入：直接经 recall.Writer 落一条记忆，带一个未声明的 concept_id。
	// 写入路径会按 D15 隐式创建 draft（sqlite 的 ensureConcept）——这一点已被
	// TestD15HundredDraftsStayBounded 正面验证。
	m := mem("m-structured", store.ScopeUser, "c-structured")
	m.TValid, m.TCreated = e.clk.Now().Unix(), e.clk.Now().Unix()
	e.put(m)

	if got := e.count(`SELECT COUNT(1) FROM concepts WHERE status='draft'`); got != 1 {
		t.Fatalf("前置不成立：隐式 draft 数=%d, want 1", got)
	}
	usageRows := e.count(`SELECT COUNT(1) FROM concept_usage WHERE concept_id='c-structured'`)
	if usageRows != 0 {
		t.Skipf("前置变了：结构化写入已登记概念使用（usage 行=%d），本用例的攻击面不再存在", usageRows)
	}

	// 该 draft 确实被一条活记忆引用（= 确实"被用过"）。
	if refs := e.count(`SELECT COUNT(1) FROM memories WHERE concept_id='c-structured'`); refs != 1 {
		t.Fatalf("前置不成立：引用数=%d, want 1", refs)
	}

	res, err := e.maint.PromoteConcept(ctx, ops.PromoteRequest{
		Scope: store.ScopeUser, All: true, Actor: "janitor",
	})
	if err != nil {
		t.Fatalf("Promote(All): %v", err)
	}

	certified := e.count(`SELECT COUNT(1) FROM concepts WHERE status='certified'`)
	if certified == 1 {
		return // 期望行为已成立
	}

	msg := "Promote(All) 漏掉了结构化写入产生的 draft（status=" + string(res.Status) +
		"，certified=" + itoa(certified) + "）：concept_usage 只由 extractOp.Apply 填充，" +
		"不经 Extract 的写入路径（TD §10 结构化写入）产生的草稿永远拿不到 UseCount，" +
		"因此永远不会被 All 认证；而 Prune 一侧有 ListStaleDrafts 兜底、Promote 一侧没有。"
	if deferredGate() {
		t.Fatal(msg)
	}
	t.Log("【可后续 / GMEM_VERIFY_DEFERRED=1 时红】" + msg)
}
