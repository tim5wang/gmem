package p2verify

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/tim5wang/gmem/extract"
	store2 "github.com/tim5wang/gmem/store"
)

// ===========================================================================
// 攻击面 1：D10 收敛（docs/TD.md §3.2）
//
// "总调用次数 == MaxTurns + 1" 是 D10 唯一的硬验收依据。要把它验成**硬保证**
// 而不是巧合，必须同时做到三件事，本文件逐个做：
//
//  1. 模型**永远**请求工具（对抗性序列），不给它"自觉收口"的机会；
//  2. 换不同 MaxTurns 值，断言调用次数**随之改变**（1/3/5 三点成线）；
//  3. 断言 **G1 是唯一触发的门禁**（隔离）：若 G3/G4 抢先收口，
//     那测到的就不是 MaxTurns 的约束力。
//
// 第 3 点是本包与实现者测试的主要区别：仓库里那条用例每轮发**同一个**
// lookup_entity，于是 G3（重复）+ G4（无进展）会在 MaxTurns 之前收口。
// 它在 MaxTurns=3 时恰好也是 4 次调用，因此"看起来"通过了；
// 但把 MaxTurns 提到 5，同一条用例的调用次数不再等于 6 —— +1 来自 G4 而非 G1。
// 所以本包每轮发**不同**的查询（且都能查到结果），把 G3/G4 排除在外。
// ===========================================================================

// seedLookupOntology 说明：lookup_entity 的命中的确靠 alias 词典，
// 本包在 newExtractor 里种 "vim" 这一条，因此任何含 "vim" 的查询都会命中。

// newExtractor 构造一个直接可用的 LLMExtractor（不经 Pipeline，避免 L0/L1 噪声）。
func newExtractor(t *testing.T, cfg extract.Config, client extract.LLMClient) (*extract.LLMExtractor, context.Context) {
	t.Helper()
	d := openMemoryDB(t)
	seedNode(t, d, "n-vim", "vim")
	seedAlias(t, d, "vim", "n-vim", 0.99)
	// 为 D10 的“每轮不同查询”夹具再种一批**互不相同**的行。
	//
	// 为什么必须多行（M1 之后）：G4 的正确语义是“连续 2 轮无**新增**结果”，
	// 所以“换一个措辞反复命中同一行”本来就该被 G4 收口。要隔离出 G1，
	// 夹具必须让每轮真的拿到一条**新**结果，而不只是换一个查询字符串。
	// 旧夹具只有 “vim” 一条，所以它测到的“每轮不同”是自欺的。
	for i := 0; i < 12; i++ {
		name := gatRowName(i)
		sid := "n-" + name
		seedNode(t, d, sid, name)
		seedAlias(t, d, name, sid, 0.99)
	}
	return extract.NewLLMExtractor(client, d.Ports(), cfg), context.Background()
}

// gatRowName 返回第 i 条专用行的名字（0-based，两位数保证互不为子串）。
func gatRowName(i int) string {
	return "gtrow" + string(rune('a'+i/10)) + string(rune('a'+i%10))
}

// probeEpisode 是给 L2 的输入（内容只需合法，L2 的行为由 client 决定）。
func probeEpisode() extract.RawEpisode {
	return extract.RawEpisode{
		ID: "e-gate", Scope: store2.ScopeUser,
		Content: "Alice 使用 vim 写代码，认证服务依赖 Redis 做缓存。",
	}
}

// distinctHittingNames 生成“每轮不同、且每轮命中**不同一行**”的查询名。
//
// 为什么不能用 `vim` + 递增的 `!`：lookup_entity 是**双向包含**匹配，
// 任何含 "vim" 的串都命中**同一条** alias。在 M1 修正后，G4 的判据是
// “本轮无**新增**结果”，所以那样写会先被 G4 收口（MaxTurns>=4 时调用恒为 4）。
// 那正是 B-3 现象的另一面：夹具自己把 G1 的隔离条件破坏了。
// 这里每轮用一个独立行名（gtrowaa / gtrowab / …），既不重复（G3 不触发），
// 又每轮都有新结果（G4 不触发），于是只剩 G1 能收口。
func distinctHittingNames(n int) string {
	return gatRowName(n)
}

// TestD10ExactlyMaxTurnsPlusOne 是 D10 的**主判据**。
//
// 三个 MaxTurns 值都断言：总调用次数 == MaxTurns+1、给工具的轮数 == MaxTurns、
// 收走工具的轮数 == 1，且只触发 G1。
func TestD10ExactlyMaxTurnsPlusOne(t *testing.T) {
	for _, maxTurns := range []int{1, 3, 5} {
		maxTurns := maxTurns
		t.Run(queryName("MaxTurns", maxTurns), func(t *testing.T) {
			llm := &recordingLLM{
				nameOf:    distinctHittingNames,
				finalJSON: extract.BuildFinalJSON([]extract.EntityJSON{{Name: "vim"}}, nil),
				usage:     extract.LLMUsage{TotalTokens: 7},
			}
			x, ctx := newExtractor(t, extract.Config{MaxTurns: maxTurns}, llm)

			res, err := x.Extract(ctx, extract.ExtractRequest{Episode: probeEpisode()})
			if err != nil {
				t.Fatalf("Extract 不应报错：%v", err)
			}
			traj := x.LastTrajectory()

			// ---- 判据 1：总调用次数 == MaxTurns + 1 ----
			if got, want := llm.calls(), maxTurns+1; got != want {
				t.Fatalf("总调用次数 == MaxTurns+1（%d）失败：实际 %d 次（gates=%v）",
					want, got, traj.Gates)
			}
			// 轨迹里的计数必须与客户端侧一致：两处独立记账，任一走偏都是缺陷。
			if traj.Calls != llm.calls() {
				t.Errorf("轨迹 Calls=%d 与客户端实际调用数 %d 不一致（记账缺陷）",
					traj.Calls, llm.calls())
			}
			// ---- 判据 2：恰好 MaxTurns 轮给工具，1 轮收走 ----
			if got := llm.callsWithTools(); got != maxTurns {
				t.Errorf("给工具的轮数应为 MaxTurns=%d，实际 %d", maxTurns, got)
			}
			if got := llm.callsWithoutTools(); got != 1 {
				t.Errorf("收走工具的轮数应为 1，实际 %d", got)
			}
			if traj.ToolRounds != maxTurns {
				t.Errorf("ToolRounds 应为 %d，实际 %d", maxTurns, traj.ToolRounds)
			}
			if !traj.ForcedFinal {
				t.Error("终稿必须是强制的（ForcedFinal=true），否则不等于门禁收口")
			}
			// ---- 判据 3：只触发 G1（隔离，证明 +1 来自 MaxTurns 而非其它门禁） ----
			if !hasGate(traj.Gates, extract.GateMaxTurns) {
				t.Errorf("应触发 G1 MaxTurns，实际 gates=%v", traj.Gates)
			}
			for _, other := range []extract.GateReason{
				extract.GateDuplicate, extract.GateNoProgress,
				extract.GateTokenBudget, extract.GateWallClock,
			} {
				if hasGate(traj.Gates, other) {
					t.Errorf("用例隔离失败：%v 抢先触发，G1 未被真正验证（gates=%v）",
						other, traj.Gates)
				}
			}
			// 最后一轮的请求里工具面必须为空（"不再给工具"要能被宿主观测到）。
			last := llm.reqs[len(llm.reqs)-1]
			if len(last.Tools) != 0 {
				t.Errorf("强制终稿轮仍给了 %d 个工具声明（应为 0）", len(last.Tools))
			}
			// 强制终稿的指令必须真的写进了 prompt，否则"听话的模型"无从知道要收口。
			mustContain(t, "强制终稿轮的 User", last.User, "必须立即输出终稿")
			// 未触发门禁的轮次必须带齐三个只读工具。
			first := llm.reqs[0]
			if len(first.Tools) != 3 {
				t.Errorf("工具轮应声明 3 个只读工具，实际 %d 个", len(first.Tools))
			}
			if len(res.Entities) == 0 {
				t.Error("终稿的实体应被解析出来（收口不等于丢弃结果）")
			}
		})
	}
}

// TestD10CallCountScalesWithMaxTurns 单独证明"不是巧合"：调用次数随 MaxTurns 线性增长。
//
// 单点相等可能碰巧成立（例如 MaxTurns=3 时 G4 也在第 4 次收口）；
// 三点成线才排除了"固定 4 次"之类的巧合。
func TestD10CallCountScalesWithMaxTurns(t *testing.T) {
	var got []int
	for _, maxTurns := range []int{1, 2, 3, 5, 8} {
		llm := &recordingLLM{
			nameOf:    distinctHittingNames,
			finalJSON: extract.BuildFinalJSON([]extract.EntityJSON{{Name: "vim"}}, nil),
		}
		x, ctx := newExtractor(t, extract.Config{MaxTurns: maxTurns}, llm)
		if _, err := x.Extract(ctx, extract.ExtractRequest{Episode: probeEpisode()}); err != nil {
			t.Fatalf("MaxTurns=%d: Extract: %v", maxTurns, err)
		}
		got = append(got, llm.calls())
	}
	want := []int{2, 3, 4, 6, 9} // = MaxTurns + 1
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("调用次数序列应为 %v（MaxTurns=1/2/3/5/8 各自 +1），实际 %v", want, got)
		}
	}
}

// TestD10ShippedFakeLLMAlwaysToolCall 用随库发布的 FakeLLM 复现 TD 的原话序列。
//
// 这一条同时是对"发布物能不能表达对抗序列"的验证：RepeatLast=true 的 FakeLLM
// 必须足以表达"永远返回 tool call"的模型，否则宿主无法在自己的 CI 里验 D10。
func TestD10ShippedFakeLLMAlwaysToolCall(t *testing.T) {
	for _, maxTurns := range []int{1, 3, 5, 8} {
		maxTurns := maxTurns
		t.Run(queryName("MaxTurns", maxTurns), func(t *testing.T) {
			// 每轮都请求同一工具、同一参数：这是 TD §3.2 原文的对抗序列。
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
			calls := llm.CallCount()

			// 硬上界：任何序列都不得超过 MaxTurns+1。
			if calls > maxTurns+1 {
				t.Fatalf("总调用次数 %d 超过硬上界 MaxTurns+1=%d", calls, maxTurns+1)
			}
			// 等式只在"没有其它门禁抢先收口"时成立；同 query 反复发会先命中 G3/G4。
			// 这里把两种情况都钉死，避免将来有人把"≤"悄悄写成"=="或反之。
			if maxTurns <= 3 {
				if calls != maxTurns+1 {
					t.Errorf("MaxTurns=%d 时总调用次数应为 %d，实际 %d（gates=%v）",
						maxTurns, maxTurns+1, calls, traj.Gates)
				}
			} else if !hasGate(traj.Gates, extract.GateDuplicate) &&
				!hasGate(traj.Gates, extract.GateNoProgress) {
				// MaxTurns>3 且没有 G3/G4 收口时，等式依然必须成立。
				if calls != maxTurns+1 {
					t.Errorf("MaxTurns=%d 无 G3/G4 时总调用次数应为 %d，实际 %d",
						maxTurns, maxTurns+1, calls)
				}
			}
			if !traj.ForcedFinal {
				t.Errorf("MaxTurns=%d: 终稿必须被强制", maxTurns)
			}
			if calls != traj.Calls {
				t.Errorf("FakeLLM 记录 %d 次调用，轨迹记录 %d 次（记账不一致）", calls, traj.Calls)
			}
			t.Logf("MaxTurns=%d → 调用 %d 次，gates=%v（上界 %d）",
				maxTurns, calls, traj.Gates, maxTurns+1)
		})
	}
}

// TestD10ModelIgnoringWithdrawalDegrades 验证"收走工具后模型仍请求工具"的收尾。
//
// 这条分支最容易被写成死循环：模型不听话时若继续执行工具，收敛就失效。
func TestD10ModelIgnoringWithdrawalDegrades(t *testing.T) {
	llm := &recordingLLM{
		nameOf: distinctHittingNames,
		// 强制终稿轮仍然返回工具调用（模拟完全不听指令的模型）。
		onFinal: func(context.Context) (extract.LLMResponse, error) {
			return extract.LLMResponse{
				ToolCalls: []extract.ToolCall{
					extract.ToolCallJSON(extract.ToolLookupEntity, `{"name":"vim"}`),
				},
			}, nil
		},
	}
	x, ctx := newExtractor(t, extract.Config{MaxTurns: 2}, llm)
	res, err := x.Extract(ctx, extract.ExtractRequest{Episode: probeEpisode()})
	if err != nil {
		t.Fatalf("模型不听话也不应报错（应降级）：%v", err)
	}
	if calls := llm.calls(); calls != 3 {
		t.Fatalf("MaxTurns=2 时应恰好 3 次调用后终止，实际 %d 次", calls)
	}
	if !hasDegraded(res.Degraded, "强制终稿后模型仍返回") {
		t.Errorf("模型无视收口必须在 Degraded 里可见，实际 %v", res.Degraded)
	}
}

// ---------------------------------------------------------------------------
// G3 / G4：两道"防死循环"的闸
// ---------------------------------------------------------------------------

// TestG3RepeatedIdenticalQueryIsRejected 攻击 G3：同一条 query 反复发。
//
// 归一化必须对 JSON 字段顺序与空白不敏感——宿主模型的序列化顺序不保证稳定，
// 若按原始文本比较，"同一条查询"会漏检并变成真正的一轮新查询。
func TestG3RepeatedIdenticalQueryIsRejected(t *testing.T) {
	// 三种写法归一化后是同一条查询：字段顺序/空白不同、参数值大小写不同。
	// 刻意**不带** concept 过滤：本用例的前提是“该查询能查到东西”，
	// 否则第一轮就 0 命中、G4 会先于 G3 收口（那是另一条用例的靶子）。
	args := []string{
		`{"name":"vim"}`,
		`{ "name" : "vim" }`,
		`{"name":"VIM"}`,
	}
	same := &scriptedLLM{steps: []extract.LLMResponse{
		{ToolCalls: []extract.ToolCall{{ID: "c1", Name: extract.ToolLookupEntity, Arguments: []byte(args[0])}}},
		{ToolCalls: []extract.ToolCall{{ID: "c2", Name: extract.ToolLookupEntity, Arguments: []byte(args[1])}}},
		{ToolCalls: []extract.ToolCall{{ID: "c3", Name: extract.ToolLookupEntity, Arguments: []byte(args[2])}}},
		{Text: extract.BuildFinalJSON([]extract.EntityJSON{{Name: "vim"}}, nil)},
	}}
	x, ctx := newExtractor(t, extract.Config{MaxTurns: 100}, same)
	if _, err := x.Extract(ctx, extract.ExtractRequest{Episode: probeEpisode()}); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	traj := x.LastTrajectory()
	if traj.DuplicateCalls != 2 {
		t.Fatalf("三种写法应被识别为 2 次重复（归一化后同一条查询），实际 %d 次（gates=%v）",
			traj.DuplicateCalls, traj.Gates)
	}
	if !hasGate(traj.Gates, extract.GateDuplicate) {
		t.Errorf("应记录 G3 触发，实际 gates=%v", traj.Gates)
	}
	if same.calls != 4 {
		t.Errorf("模型每轮都被调用（拒的是这一跳、不是这一轮），应为 4 次，实际 %d", same.calls)
	}
	// 被拒的跳必须让模型知道（否则它以为查询成功、结果就是空）。
	mustContain(t, "回注上下文", same.lastUser(t), "已被去重拒绝")
}

// TestG3G4BoundUnboundedModel 是“不得死循环”的正面证明。
//
// 模型永远返回同一条查询、MaxTurns 给到 100：G1 必须把它在 MaxTurns+1 次调用内
// 收口（D10 的硬等式）。
//
// # 为什么不再断言 “≤4 次”
//
// 旧断言成立的前提是 B-3 那个缺陷：G3 每轮拒跳都不计 executed，于是整轮被 G4
// 误判为无进展，第 3 轮就强制终稿。修好 B-3 后，“同一条查询反复发”的正确结局
// 是“每次都被 G3 拒、循环由 G1 在 MaxTurns 处收口”——它**不可能**在 4 次内收口，
// 除非 MaxTurns≤3。保留的原意图不变：不得死循环、必须有门禁拦住、必须强制终稿。
func TestG3G4BoundUnboundedModel(t *testing.T) {
	const maxTurns = 100
	llm := extract.NewRepeatingFakeLLM(extract.FakeStep{
		ToolCalls: []extract.ToolCall{
			extract.ToolCallJSON(extract.ToolLookupEntity, `{"name":"vim"}`),
		},
	})
	x, ctx := newExtractor(t, extract.Config{MaxTurns: maxTurns}, llm)
	if _, err := x.Extract(ctx, extract.ExtractRequest{Episode: probeEpisode()}); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	traj := x.LastTrajectory()
	if calls := llm.CallCount(); calls != maxTurns+1 {
		t.Fatalf("不得死循环，也不得提前放弃：应为 MaxTurns+1=%d 次调用，实际 %d 次（gates=%v）",
			maxTurns+1, calls, traj.Gates)
	}
	// 逐轮重复的跳必须被 G3 拒（不是被静默重复执行）。
	if !hasGate(traj.Gates, extract.GateDuplicate) {
		t.Fatalf("重跳必须被 G3 拒绝，实际 gates=%v", traj.Gates)
	}
	if traj.DuplicateCalls == 0 {
		t.Error("应记录被拒跳数（DuplicateCalls>0）")
	}
	if traj.ForcedFinal != true {
		t.Error("必须强制终稿")
	}
}

// TestG4ConsecutiveEmptyResultsForceFinal 攻击 G4：连续空结果。
//
// 每轮查询**不同**的名字（否则测到的是 G3），但一个都查不到。
// 连续 2 轮空 → 第 3 轮强制终稿。
func TestG4ConsecutiveEmptyResultsForceFinal(t *testing.T) {
	missing := []string{"zeta", "eta", "theta", "iota", "kappa"}
	llm := &recordingLLM{
		nameOf:    func(n int) string { return missing[n%len(missing)] },
		finalJSON: extract.BuildFinalJSON([]extract.EntityJSON{{Name: "vim"}}, nil),
	}
	x, ctx := newExtractor(t, extract.Config{MaxTurns: 100}, llm)
	if _, err := x.Extract(ctx, extract.ExtractRequest{Episode: probeEpisode()}); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	traj := x.LastTrajectory()
	if got := llm.calls(); got != 3 {
		t.Fatalf("连续 2 轮空结果后第 3 轮必须强制终稿，实际调用 %d 次（gates=%v）",
			got, traj.Gates)
	}
	if !hasGate(traj.Gates, extract.GateNoProgress) {
		t.Errorf("应触发 G4，实际 gates=%v", traj.Gates)
	}
	if hasGate(traj.Gates, extract.GateMaxTurns) {
		t.Errorf("G4 用例被 G1 抢先（隔离失败）：MaxTurns=100，gates=%v", traj.Gates)
	}
	if traj.NoProgressRounds != 2 || !traj.ForcedFinal {
		t.Errorf("应记录 2 轮无进展并强制终稿，实际 %d 轮 ForcedFinal=%v",
			traj.NoProgressRounds, traj.ForcedFinal)
	}
}

// ---------------------------------------------------------------------------
// G5：墙钟超时
// ---------------------------------------------------------------------------

// TestG5CallerDeadlineDegrades 是 G5 的**已覆盖**路径：调用方给了 deadline。
func TestG5CallerDeadlineDegrades(t *testing.T) {
	blocking := &blockingLLM{blockInToolRound: true}
	x, _ := newExtractor(t, extract.Config{MaxTurns: 100, WallClock: time.Second}, blocking)

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	start := time.Now()
	res, err := x.Extract(ctx, extract.ExtractRequest{Episode: probeEpisode()})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("超时必须降级，不得报错：%v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("应在 deadline 附近返回，实际耗时 %v", elapsed)
	}
	if len(res.Degraded) == 0 {
		t.Fatal("超时降级必须可见（Degraded 为空）")
	}
	if !hasGate(x.LastTrajectory().Gates, extract.GateWallClock) {
		t.Errorf("应记录 G5，实际 gates=%v", x.LastTrajectory().Gates)
	}
}

// TestG5ForcedFinalGraceDegrades 验证"强制终稿那一次调用"的超时兜底。
//
// TD §3.2 的 G5 写着"中断并降级"，实现里对应 FinalGrace：门禁已触发时
// 只给一次极短机会，拿不到就降级返回。这条必须真的生效，否则一个卡住的模型
// 会让"已经决定收口"的那一轮永久挂住。
func TestG5ForcedFinalGraceDegrades(t *testing.T) {
	// 工具轮快速返回，强制终稿轮永久阻塞（只认 ctx）。
	llm := &blockingLLM{blockInToolRound: false, blockInFinal: true}
	x, _ := newExtractor(t, extract.Config{
		MaxTurns: 1, WallClock: 5 * time.Second, FinalGrace: 30 * time.Millisecond,
	}, llm)

	start := time.Now()
	res, err := x.Extract(context.Background(), extract.ExtractRequest{Episode: probeEpisode()})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("强制终稿超时必须降级而不是报错：%v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("FinalGrace=30ms 应在数十毫秒内收口，实际 %v", elapsed)
	}
	if !hasDegraded(res.Degraded, "超时") && !hasDegraded(res.Degraded, "取消") {
		t.Fatalf("强制终稿超时必须可见，实际 Degraded=%v", res.Degraded)
	}
}

// TestG5WallClockBoundsBlockingToolRound 是 【阻塞合并】候选 #1 的复现用例。
//
// 判据来自 TD §3.2 的 G5 一行："墙钟超时（context deadline，默认 30s）→
// 中断并降级"。这里的"中断"必须对**工具轮**也成立：宿主注入的 LLMClient
// 一旦卡住（网络停滞、SDK 没有自己的超时），Gmem 是唯一还能收口的一方。
//
// 现状：门禁只在**两次调用之间**比较时间戳（`cfg.now().After(deadline)`），
// 从不把它变成 ctx 的 deadline；只有"强制终稿"那一轮套了 FinalGrace。
// 因此工具轮里 Complete 永久阻塞时，Gmem 永久阻塞——WallClock 完全不起作用。
//
// 默认套件必须绿（已知缺陷不应让 CI 永久变红），因此复现挂在
// GMEM_VERIFY_BLOCKER=1 之后；未置位时本用例仍会**实测**并打印结论。
func TestG5WallClockBoundsBlockingToolRound(t *testing.T) {
	llm := &blockingLLM{blockInToolRound: true, release: make(chan struct{})}
	// WallClock 极短，但调用方**不给** deadline：这是真实宿主最常见的形态。
	x, _ := newExtractor(t, extract.Config{MaxTurns: 100, WallClock: 20 * time.Millisecond}, llm)

	type outcome struct {
		res     extract.ExtractResult
		err     error
		elapsed time.Duration
	}
	done := make(chan outcome, 1)
	start := time.Now()
	go func() {
		res, err := x.Extract(context.Background(), extract.ExtractRequest{Episode: probeEpisode()})
		done <- outcome{res: res, err: err, elapsed: time.Since(start)}
	}()

	const watchdog = 600 * time.Millisecond
	select {
	case o := <-done:
		if o.err != nil {
			t.Fatalf("WallClock 超时不得报错（应降级）：%v", o.err)
		}
		if len(o.res.Degraded) == 0 {
			t.Fatal("WallClock 超时降级必须可见（Degraded 为空）")
		}
		t.Logf("G5 对工具轮生效：耗时 %v 后降级返回", o.elapsed)
	case <-time.After(watchdog):
		// 先解锁，避免 goroutine/数据库泄漏影响后续用例。
		llm.unblock()
		o := <-done
		if os.Getenv("GMEM_VERIFY_BLOCKER") == "1" {
			t.Fatalf(`【阻塞合并】G5 墙钟超时不约束工具轮，模型卡住即整条管线挂死：
  配置 WallClock   = 20ms
  实际阻塞时长     > %v（最终由测试看门狗解开阻塞）
  解阻后的结果      = err=%v degraded=%v

根因（extract/llm.go 多轮循环）：
  门禁只在**发请求前**用 cfg.now().After(deadline) 判一下时间戳，
  但从未把 deadline 变成 ctx 的 deadline：
      callCtx, cancel = context.WithTimeout(ctx, cfg.FinalGrace)   // 只在 !allowTools 时
  工具轮的 Complete 直接拿到调用方的 ctx。因此宿主客户端一旦阻塞，
  Gmem 无法中断它——TD §3.2 对 G5 的承诺（"中断并降级"）在工具轮上不成立。

最小修复方向（不由本测试实施）：
  每一轮都 callCtx = context.WithDeadline(ctx, deadline)，
  再把 DeadlineExceeded 归到 GateWallClock 的降级分支（已有该分支）。`,
				watchdog, o.err, o.res.Degraded)
		}
		t.Logf("【可复现缺陷】G5 墙钟超时不约束工具轮：WallClock=20ms，"+
			"但阻塞 >%v 才被看门狗解开（判据：整条管线挂死）", watchdog)
	}
}

// ---------------------------------------------------------------------------
// 测试替身
// ---------------------------------------------------------------------------

// scriptedLLM 按剧本逐轮应答，剧本用完后返回空终稿（让测试快速失败而非挂死）。
type scriptedLLM struct {
	steps []extract.LLMResponse
	reqs  []extract.LLMRequest
	calls int
}

func (s *scriptedLLM) Complete(ctx context.Context, req extract.LLMRequest) (extract.LLMResponse, error) {
	if err := ctx.Err(); err != nil {
		return extract.LLMResponse{}, err
	}
	n := s.calls
	s.calls++
	s.reqs = append(s.reqs, req)
	if n >= len(s.steps) {
		return extract.LLMResponse{}, nil
	}
	return s.steps[n], nil
}

func (s *scriptedLLM) lastUser(t *testing.T) string {
	t.Helper()
	if len(s.reqs) == 0 {
		return ""
	}
	return s.reqs[len(s.reqs)-1].User
}

// blockingLLM 模拟"卡住的宿主客户端"。
//
// release 非 nil 时，阻塞会一直持续到测试显式解开（用于"证明它会挂死"）；
// 否则只认 ctx（用于验证已经存在的 FinalGrace 兜底）。
type blockingLLM struct {
	blockInToolRound bool
	blockInFinal     bool
	release          chan struct{}
	// toolRounds 记录工具轮的序号，让每轮查询名不同（排除 G3/G4 混淆）。
	round int
}

func (b *blockingLLM) Complete(ctx context.Context, req extract.LLMRequest) (extract.LLMResponse, error) {
	block := (len(req.Tools) > 0 && b.blockInToolRound) ||
		(len(req.Tools) == 0 && b.blockInFinal)
	if block {
		select {
		case <-ctx.Done():
			return extract.LLMResponse{}, ctx.Err()
		case <-b.release:
			// 被测试解开：返回一个合法终稿，让 Extract 能正常收尾。
			return extract.LLMResponse{
				Text: extract.BuildFinalJSON([]extract.EntityJSON{{Name: "vim"}}, nil),
			}, nil
		}
	}
	b.round++
	return extract.LLMResponse{
		ToolCalls: []extract.ToolCall{
			extract.ToolCallJSON(extract.ToolLookupEntity,
				`{"name":`+quote(distinctHittingNames(b.round))+`}`),
		},
	}, nil
}

func (b *blockingLLM) unblock() {
	if b.release != nil {
		close(b.release)
		b.release = nil
	}
}

// quote 是极简的 JSON 字符串字面量（测试里只拼 ASCII 名字）。
func quote(s string) string { return `"` + s + `"` }
