package extract

import (
	"context"
	"testing"
	"time"

	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// P2 修复的回归用例（每一项都能通过变异验证变红）
//
// 这一组与实现者的原始用例分开：它们各自对准一个**已确认的缺陷**，
// 修好之前必然红、修好之后必须绿。因此它们同时是"陈旧的假测试"的反例。
// ===========================================================================

// ---------------------------------------------------------------------------
// M2：G3 归一化不得被"多带一个空参"绕过
// ---------------------------------------------------------------------------

// TestG3EmptyArgDoesNotChangeKey 是 M2 的单元级回归。
//
// 缺陷：G3 的键是"工具名 + 全部参数"。模型只要每轮多带一个 `"concept":""`
// 就把同一条语义查询变成"新查询"，G3 形同虚设，只能靠 G1/G2 兜住成本。
// 修法：键只取该工具**真正生效**的参数，空值直接从键里剔除。
//
// 变异验证：把 normalizeCall 改回"全参数排序拼接"，本用例必须变红。
func TestG3EmptyArgDoesNotChangeKey(t *testing.T) {
	first := normalizeCall(ToolCallJSON(ToolLookupEntity, `{"name":"vim"}`))
	same := []string{
		`{"name":"vim","concept":""}`,
		`{"name":"vim","concept":"   "}`,
		`{"concept":"","name":"vim"}`,
	}
	for _, args := range same {
		if got := normalizeCall(ToolCallJSON(ToolLookupEntity, args)); got != first {
			t.Errorf("空参不得改变 G3 归一化键：%s 得到 %q，基准 %q", args, got, first)
		}
	}
	// 反向：真实的概念过滤必须**改变**键（不能被归一化抹平）。
	if got := normalizeCall(ToolCallJSON(ToolLookupEntity,
		`{"name":"vim","concept":"c-editor"}`)); got == first {
		t.Errorf("带真实 concept 过滤应是不同查询，实际键相同 %q", got)
	}
	// search_memories：k 缺省 / 0 / 超上限 在工具侧都夹到 5，语义等价。
	base := normalizeCall(ToolCallJSON(ToolSearchMemories, `{"query":"缓存"}`))
	for _, args := range []string{
		`{"query":"缓存","k":0}`,
		`{"query":"缓存","k":9}`,
		`{"query":"缓存","k":5}`,
	} {
		if got := normalizeCall(ToolCallJSON(ToolSearchMemories, args)); got != base {
			t.Errorf("%s 应归一化为同一条查询（k 被夹到上限），实际 %q vs %q", args, got, base)
		}
	}
	// neighbors：hops 的任意值都会被夹到 HardMaxHops，语义等价。
	nbase := normalizeCall(ToolCallJSON(ToolNeighbors, `{"node_id":"n-vim"}`))
	for _, args := range []string{
		`{"node_id":"n-vim","hops":1}`,
		`{"node_id":"n-vim","hops":3}`,
		`{"node_id":"n-vim","hops":0}`,
	} {
		if got := normalizeCall(ToolCallJSON(ToolNeighbors, args)); got != nbase {
			t.Errorf("%s 应归一化为同一条查询（hops 被夹到 1），实际 %q vs %q", args, got, nbase)
		}
	}
}

// TestG3EmptyArgStillRejectedEndToEnd 端到端证明 M2：每轮"同一查询 + 空参"必须被 G3 拒。
func TestG3EmptyArgStillRejectedEndToEnd(t *testing.T) {
	llm := &emptyArgLLM{}
	x, _ := newGateExtractor(t, Config{MaxTurns: 4}, llm)
	if _, err := x.Extract(context.Background(), ExtractRequest{
		Episode: RawEpisode{ID: "e-m2", Scope: store.ScopeUser,
			Content: "Alice 使用 vim 写代码，认证服务依赖 Redis 做缓存。"},
	}); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	traj := x.LastTrajectory()
	if !hasGate(traj.Gates, GateDuplicate) {
		t.Fatalf("同一查询 + 空参必须被 G3 识别为重复，实际 gates=%v dup=%d",
			traj.Gates, traj.DuplicateCalls)
	}
	// MaxTurns=4：第 1 轮执行，后 3 轮全被拒。
	if traj.DuplicateCalls != 3 {
		t.Errorf("后 3 轮应全部被拒（重复），实际 %d 次", traj.DuplicateCalls)
	}
}

// emptyArgLLM 每轮发同一条查询，但交替塞入不同的空参。
type emptyArgLLM struct{ Calls []LLMRequest }

func (e *emptyArgLLM) Complete(ctx context.Context, req LLMRequest) (LLMResponse, error) {
	if err := ctx.Err(); err != nil {
		return LLMResponse{}, err
	}
	n := len(e.Calls)
	e.Calls = append(e.Calls, req)
	if len(req.Tools) == 0 {
		return LLMResponse{Text: BuildFinalJSON([]EntityJSON{{Name: "vim"}}, nil),
			Usage: LLMUsage{TotalTokens: 5}, Model: "fake"}, nil
	}
	args := []string{
		`{"name":"vim"}`,
		`{"name":"vim","concept":""}`,
		`{"name":"vim","concept":"   "}`,
		`{"name":"vim","concept":""}`,
	}
	return LLMResponse{
		ToolCalls: []ToolCall{ToolCallJSON(ToolLookupEntity, args[n%len(args)])},
		Usage:     LLMUsage{TotalTokens: 5}, Model: "fake",
	}, nil
}

// ---------------------------------------------------------------------------
// M4：修复式重试不得突破 MaxTurns+1
// ---------------------------------------------------------------------------

// TestM4RepairRetryWithinMaxTurns 是 M4 的回归用例。
//
// 缺陷：终稿解析失败时的"修复式重试"额外发一次 Complete 且 Calls++，
// 上界因此变成 MaxTurns+2，"总调用次数 == MaxTurns+1"不再是硬等式。
// 修法：重试占用轮配额（spent++），上界回到 MaxTurns+1。
//
// 变异验证：去掉重试处的 `spent++`，第二个子用例必须变红（会取到 4→5 次）。
func TestM4RepairRetryWithinMaxTurns(t *testing.T) {
	t.Run("配额用尽时不再重试", func(t *testing.T) {
		// MaxTurns=1：第 1 轮是工具轮；强制终稿轮给坏 JSON，配额已空。
		llm := NewFakeLLM(
			FakeStep{ToolCalls: []ToolCall{ToolCallJSON(ToolLookupEntity, `{"name":"vim"}`)}},
			FakeStep{Text: "这不是 JSON"},
			FakeStep{Text: BuildFinalJSON([]EntityJSON{{Name: "vim"}}, nil)},
		)
		x, _ := newGateExtractor(t, Config{MaxTurns: 1}, llm)
		res, err := x.Extract(context.Background(), ExtractRequest{
			Episode: RawEpisode{ID: "e-m4a", Scope: store.ScopeUser,
				Content: "Alice 使用 vim 写代码，认证服务依赖 Redis 做缓存。"},
		})
		if err != nil {
			t.Fatalf("Extract: %v", err)
		}
		traj := x.LastTrajectory()
		if traj.Calls != 2 {
			t.Errorf("强制终稿轮的坏 JSON 不得再重试：Calls 应为 2，实际 %d", traj.Calls)
		}
		if traj.RepairRetries != 0 {
			t.Errorf("配额用尽时不应发生修复式重试，实际 %d 次", traj.RepairRetries)
		}
		if len(res.Degraded) == 0 {
			t.Error("解析失败必须降级可见")
		}
	})

	t.Run("重试成功后总调用不超过 MaxTurns+1", func(t *testing.T) {
		// MaxTurns=3：第 1 轮工具、第 2 轮坏 JSON，配额还剩 1 轮给重试。
		llm := NewFakeLLM(
			FakeStep{ToolCalls: []ToolCall{ToolCallJSON(ToolLookupEntity, `{"name":"vim"}`)}},
			FakeStep{Text: "坏 JSON"},
			FakeStep{Text: BuildFinalJSON([]EntityJSON{{Name: "vim"}}, nil)},
		)
		x, _ := newGateExtractor(t, Config{MaxTurns: 3}, llm)
		res, err := x.Extract(context.Background(), ExtractRequest{
			Episode: RawEpisode{ID: "e-m4b", Scope: store.ScopeUser,
				Content: "Alice 使用 vim 写代码，认证服务依赖 Redis 做缓存。"},
		})
		if err != nil {
			t.Fatalf("Extract: %v", err)
		}
		traj := x.LastTrajectory()
		if traj.Calls > 4 {
			t.Errorf("修复式重试后总调用不得超过 MaxTurns+1=4，实际 %d", traj.Calls)
		}
		if traj.RepairRetries != 1 {
			t.Errorf("应恰好发生 1 次修复式重试，实际 %d", traj.RepairRetries)
		}
		if len(res.Entities) == 0 {
			t.Error("修复式重试成功后应拿到实体")
		}
	})
}

// ---------------------------------------------------------------------------
// B-2 / G5：墙钟对**在飞调用**生效
// ---------------------------------------------------------------------------

// TestB2WallClockBoundsInflightToolRound 是 B-2 的回归用例（无门禁、默认必跑）。
//
// 缺陷：只在"已触发门禁的强制终稿轮"设 WithTimeout，工具轮的 Complete 直接
// 拿到调用方 ctx（没有 deadline）。宿主客户端一卡住，Gmem 永久阻塞，
// WallClock 对在飞调用完全无效（修前实测 610ms+，判据是整条管线挂死）。
// 修法：每一轮都用 context.WithDeadline(ctx, deadline)。
//
// 变异验证：把工具轮的 WithDeadline 去掉（改回 callCtx = ctx），本用例必须变红。
func TestB2WallClockBoundsInflightToolRound(t *testing.T) {
	llm := &stubbornLLM{release: make(chan struct{})}
	defer llm.unblockCh()
	x, _ := newGateExtractor(t, Config{MaxTurns: 100, WallClock: 20 * 1000 * 1000}, llm)

	done := make(chan struct {
		res ExtractResult
		err error
	}, 1)
	go func() {
		res, err := x.Extract(context.Background(), ExtractRequest{
			Episode: RawEpisode{ID: "e-b2", Scope: store.ScopeUser,
				Content: "Alice 使用 vim 写代码，认证服务依赖 Redis 做缓存。"},
		})
		done <- struct {
			res ExtractResult
			err error
		}{res, err}
	}()

	select {
	case o := <-done:
		if o.err != nil {
			t.Fatalf("墙钟超时必须降级而不是报错：%v", o.err)
		}
		if len(o.res.Degraded) == 0 {
			t.Error("墙钟超时必须降级可见（Degraded 为空）")
		}
	case <-time.After(400 * time.Millisecond):
		llm.unblockCh()
		<-done
		t.Fatalf("WallClock=20ms 未中断在飞的工具轮（>400ms 才返回）：G5 对在飞调用无效")
	}
}

// stubbornLLM 的工具轮只认 ctx：没有 deadline 就永久阻塞。
// （刻意不监听 release，除非测试显式解开，用于避免 goroutine 泄漏。）
type stubbornLLM struct {
	release chan struct{}
	once    bool
}

func (s *stubbornLLM) unblockCh() {
	if s.release != nil && !s.once {
		s.once = true
		close(s.release)
	}
}

func (s *stubbornLLM) Complete(ctx context.Context, req LLMRequest) (LLMResponse, error) {
	if err := ctx.Err(); err != nil {
		return LLMResponse{}, err
	}
	if len(req.Tools) == 0 {
		return LLMResponse{Text: BuildFinalJSON([]EntityJSON{{Name: "vim"}}, nil),
			Usage: LLMUsage{TotalTokens: 5}, Model: "fake"}, nil
	}
	select {
	case <-ctx.Done():
		return LLMResponse{}, ctx.Err()
	case <-s.release:
		return LLMResponse{}, ctx.Err()
	}
}
