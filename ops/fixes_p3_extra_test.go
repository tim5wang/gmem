package ops_test

import (
	"context"
	"testing"

	"github.com/tim5wang/gmem/ops"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// 补充回归：F-8（Merge 必须处理旧节点）+ F-7（Plan 失败要落审计）
// ===========================================================================

// TestMergeRegistersOldNameAsAliasToNewNode 是 F-8 的回归。
//
// # 缺陷形状
//
// Merge 迁移了边、迁移了 alias、写了 redirect，但旧节点**本身没动**：它仍以
// CanonicalName 参与召回对齐（Aligner 只按 concept name / alias 匹配，不看 redirect），
// 于是"合并"在召回侧看起来根本没发生。
//
// 变异点：删掉 mergeNodes 里的 PutAlias(oldName → to) → 按旧名对齐仍得到旧节点，
// 第一个断言必红。
func TestMergeRegistersOldNameAsAliasToNewNode(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, n := range []store.Node{
		{ID: "n-old", Scope: store.ScopeUser, CanonicalName: "张三(A)"},
		{ID: "n-new", Scope: store.ScopeUser, CanonicalName: "张三"},
	} {
		if err := e.db.Graph().PutNode(ctx, n); err != nil {
			t.Fatalf("PutNode(%s): %v", n.ID, err)
		}
	}

	res, err := e.maint.Merge(ctx, ops.MergeRequest{
		Scope: store.ScopeUser, FromNode: "n-old", ToNode: "n-new",
		Reason: "同一人", Actor: "agent",
	})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if res.Status != ops.RunApplied {
		t.Fatalf("Merge 状态=%s, want applied", res.Status)
	}

	// 旧名必须已指向新节点：按旧名对齐得到的 NodeID 是新节点。
	alias, err := e.db.Store().GetAlias(ctx, "张三(A)")
	if err != nil {
		t.Fatalf("旧名未被登记为 alias（合并后旧名仍以旧身份参与召回）：%v", err)
	}
	if alias.NodeID != "n-new" {
		t.Errorf("旧名指向=%q, want n-new（旧节点仍会被召回带回来）", alias.NodeID)
	}

	// 旧 ID 仍必须保留为重定向（TD §4：合并可回溯，不是抹掉）。
	redirected, err := e.db.OpsBackend().ResolveRedirect(ctx, "n-old")
	if err != nil {
		t.Fatalf("ResolveRedirect: %v", err)
	}
	if redirected != "n-new" {
		t.Errorf("n-old 重定向到 %q, want n-new", redirected)
	}
}

// TestPlanFailureIsAudited 是 F-7 的回归（唯一无冲突的分支）。
//
// # 为什么只做 Plan 失败这一条
//
// duplicate / cancelled 两条路径写 audit_log 会直接违反受保护用例的现有断言
// （ops/ops_test.go:197、internal/p3verify/idempotent_test.go:76、
// internal/p3verify/cancel_d15scale_test.go:68），因此留待裁决。
// Plan 失败那条没有冲突：它本来就在独立事务里补 op_runs，顺手补一条审计不违反任何既有断言。
//
// 变异点：删掉 recordFailed 里的 appendOutcomeAudit → plan_failed 审计行为 0，必红。
func TestPlanFailureIsAudited(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// 库里一条记忆都没有，但显式点名一个不存在的 episode：Plan 读不到就必须失败。
	res, err := e.runner.Run(ctx, ops.Request{
		Op: ops.OpExtract, Scope: store.ScopeUser,
		Trigger: ops.Trigger{Kind: ops.TriggerAgent, Actor: "agent-9", Reason: "点名抽取"},
		Input:   ops.Input{Episodes: []ops.ExtractTarget{{EpisodeID: "ep-missing"}}},
	})
	if err == nil {
		t.Fatalf("Plan 应该失败（episode 不存在），实际 status=%s", res.Status)
	}
	if res.Status != ops.RunFailed {
		t.Fatalf("状态=%s, want failed", res.Status)
	}

	var n int
	if err := e.raw.QueryRow(
		`SELECT COUNT(1) FROM audit_log WHERE step_kind='run_plan_failed' AND op_id = ?`, res.Key).
		Scan(&n); err != nil {
		t.Fatalf("查审计: %v", err)
	}
	if n != 1 {
		t.Errorf("Plan 失败的审计行=%d, want 1（失败路径零审计 → 无法重建因果链）", n)
	}

	// 审计里必须能读到“为什么失败”，而不只是“失败过”。
	entries, err := e.maint.Audit(ctx, ops.AuditQuery{OpID: res.Key})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	var found bool
	for _, a := range entries {
		if a.StepKind == "run_plan_failed" && a.AfterJSON != "" {
			found = true
		}
	}
	if !found {
		t.Errorf("plan_failed 审计缺少 after_json（原因不可读）：%+v", entries)
	}
	// 本次运行什么都没改：不得伪造 changed=true。
	var changed int
	if err := e.raw.QueryRow(`SELECT COUNT(1) FROM audit_log WHERE changed=1`).Scan(&changed); err != nil {
		t.Fatalf("查 changed: %v", err)
	}
	if changed != 0 {
		t.Errorf("失败运行产生了 %d 条 changed=1 的审计（伪造变更）", changed)
	}
}
