package p2verify

import (
	"context"
	"testing"

	"github.com/tim5wang/gmem/extract"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// B-3 的正式回归：D10 的等式必须在**所有** MaxTurns 上成立
//
// leader 实测（修前）：FakeLLM 永远返回同一 tool call 时
//   MaxTurns=1/2/3 → Calls=2/3/4（符合）
//   MaxTurns=4/5/6 → Calls 恒为 4（崩溃）
// 根因：G3 命中后 continue，不计入 executed，于是整轮被 G4 误判为无进展。
// 仓库里原有的那条测试取 MaxTurns=3，恰好是 G1 与 G3+G4 的交叉点，属巧合通过。
//
// 因此这一组用例刻意做两件事：
//  1. 六档穷举（1..6）——把交叉点两侧都覆盖；
//  2. 用**同一 tool call**（最恶劣的对抗序列），因为那正是崩溃发生的形态。
// ===========================================================================

// TestB3EqualityHoldsAcrossAllMaxTurns 是 B-3 的六档回归。
//
// 对抗序列：模型每轮都请求同一条 lookup_entity（G3 必然命中）。
// 正确行为：G3 只拒"这一跳"，不把整轮判成无进展；循环由 G1 收口。
// 因此总调用次数必须恒等于 MaxTurns+1。
//
// 变异验证：把 G4 的判定改回 `executed == 0 || newIDs == 0`（不给 G3 拒跳开例外），
// MaxTurns=4/5/6 三档会退回 4，本用例必须变红。
func TestB3EqualityHoldsAcrossAllMaxTurns(t *testing.T) {
	for _, maxTurns := range []int{1, 2, 3, 4, 5, 6} {
		maxTurns := maxTurns
		t.Run(queryName("MaxTurns", maxTurns), func(t *testing.T) {
			// 每轮同一 tool call：这是 leader 实测崩溃的那个序列。
			llm := extract.NewRepeatingFakeLLM(extract.FakeStep{
				ToolCalls: []extract.ToolCall{
					extract.ToolCallJSON(extract.ToolLookupEntity, `{"name":"vim"}`),
				},
				Usage: extract.LLMUsage{TotalTokens: 5},
			})
			x, ctx := newExtractor(t, extract.Config{MaxTurns: maxTurns}, llm)
			if _, err := x.Extract(ctx, extract.ExtractRequest{Episode: probeEpisode()}); err != nil {
				t.Fatalf("Extract: %v", err)
			}
			traj := x.LastTrajectory()

			if got, want := llm.CallCount(), maxTurns+1; got != want {
				t.Fatalf("D10 硬等式失败：MaxTurns=%d 时应恰好 %d 次调用，实际 %d（gates=%v dup=%d rounds=%d）",
					maxTurns, want, got, traj.Gates, traj.DuplicateCalls, traj.ToolRounds)
			}
			if traj.Calls != llm.CallCount() {
				t.Errorf("轨迹 Calls=%d 与客户端调用数 %d 不一致", traj.Calls, llm.CallCount())
			}
			if traj.ToolRounds != maxTurns {
				t.Errorf("工具轮应为 %d，实际 %d", maxTurns, traj.ToolRounds)
			}
			if !traj.ForcedFinal {
				t.Error("终稿必须被强制")
			}
			// 收口者必须是 G1：本序列里 G3 会持续命中（这是它的职责），
			// 但绝不能出现 G4——那说明 G3 的拒跳被误计成无进展（B-3 的根因）。
			if !hasGate(traj.Gates, extract.GateMaxTurns) {
				t.Errorf("应由 G1 收口，实际 gates=%v", traj.Gates)
			}
			if hasGate(traj.Gates, extract.GateNoProgress) {
				t.Errorf("G3 拒跳不得计入 G4 无进展（B-3 根因），实际 gates=%v", traj.Gates)
			}
			// G3 确实在工作：第 2 轮起每一跳都是重复。
			if maxTurns >= 2 && traj.DuplicateCalls == 0 {
				t.Errorf("MaxTurns=%d 时应有被 G3 拒掉的跳，实际 0", maxTurns)
			}
		})
	}
}

// TestB3NoProgressStillWorksWhenNothingIsRejected 确保 B-3 的修法没有把 G4 废掉。
//
// 每轮查询名**不同**且都查不到：既有新跳（G3 不触发），又没有新增结果，
// 因此 G4 必须照常触发并在 2 轮后收口。
//
// 变异验证：把 G4 的 `newIDs == 0` 改成恒 false，本用例必须变红。
func TestB3NoProgressStillWorksWhenNothingIsRejected(t *testing.T) {
	miss := []string{"zzz-1", "zzz-2", "zzz-3", "zzz-4"}
	llm := &missLLM{names: miss}
	x, ctx := newExtractor(t, extract.Config{MaxTurns: 100}, llm)
	if _, err := x.Extract(ctx, extract.ExtractRequest{Episode: probeEpisode()}); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	traj := x.LastTrajectory()
	if traj.DuplicateCalls != 0 {
		t.Errorf("每轮查询名不同，不应有 G3 拒跳，实际 %d", traj.DuplicateCalls)
	}
	if !hasGate(traj.Gates, extract.GateNoProgress) {
		t.Fatalf("连续 2 轮无新增结果必须触发 G4，实际 gates=%v", traj.Gates)
	}
	if traj.ToolRounds != 2 {
		t.Errorf("G4 应在连续 2 轮后收口（工具轮=2），实际 %d", traj.ToolRounds)
	}
	if hasGate(traj.Gates, extract.GateMaxTurns) {
		t.Errorf("MaxTurns=100，不应由 G1 收口：gates=%v", traj.Gates)
	}
}

// missLLM 每轮发一个不同的、查不到任何结果的查询。
type missLLM struct {
	names []string
	calls int
}

func (m *missLLM) Complete(ctx context.Context, req extract.LLMRequest) (extract.LLMResponse, error) {
	if err := ctx.Err(); err != nil {
		return extract.LLMResponse{}, err
	}
	idx := m.calls % len(m.names)
	m.calls++
	if len(req.Tools) == 0 {
		return extract.LLMResponse{
			Text:  extract.BuildFinalJSON([]extract.EntityJSON{{Name: "vim"}}, nil),
			Usage: extract.LLMUsage{TotalTokens: 5}, Model: "miss",
		}, nil
	}
	return extract.LLMResponse{
		ToolCalls: []extract.ToolCall{
			extract.ToolCallJSON(extract.ToolLookupEntity,
				`{"name":"`+m.names[idx]+`"}`),
		},
		Usage: extract.LLMUsage{TotalTokens: 5}, Model: "miss",
	}, nil
}

// 确保 scope 常量被使用（本文件只用到 User scope）。
var _ = store.ScopeUser
