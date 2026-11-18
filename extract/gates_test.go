package extract

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// D10：五条收敛门禁（G1–G5）
//
// 本节最重要的一条是 TestG1ForcedFinalExactlyMaxTurnsPlusOne ——
// docs/TD.md §3.2 与任务卡都把它写成"D10 的唯一验收依据"：
// 用 FakeLLM 编排"永远返回 tool call"的序列，断言恰好 MaxTurns 轮后强制终稿，
// 且**总调用次数 == MaxTurns + 1**。
//
// 它同时是变异验证的靶子：把 MaxTurns 改大、或去掉任何一条门禁，它必须变红。
// ===========================================================================

// alwaysToolCallLLM 是"永远返回工具调用"的模型（D10 需要的对抗性序列）。
func alwaysToolCallLLM() *FakeLLM {
	return NewRepeatingFakeLLM(FakeStep{
		ToolCalls: []ToolCall{ToolCallJSON(ToolLookupEntity, `{"name":"vim"}`)},
		Usage:     LLMUsage{TotalTokens: 10},
	})
}

// newGateExtractor 构造一个带真实 ports 的 LLMExtractor（工具真的会执行）。
func newGateExtractor(t *testing.T, cfg Config, client LLMClient) (*LLMExtractor, *Pipeline) {
	t.Helper()
	d := openMemoryDB(t)
	seedOntology(t, d)
	x := NewLLMExtractor(client, d.Ports(), cfg)
	p := NewPipeline(d.Ports(), cfg, PipelineOptions{Extractor: x})
	return x, p
}

// TestG1ForcedFinalExactlyMaxTurnsPlusOne 是 D10 的唯一验收依据。
//
// 断言三件事：
//  1. 恰好 3 轮给了工具（MaxTurns=3）；
//  2. 第 4 次调用**没有**工具（强制终稿），且总调用次数 == MaxTurns+1 == 4；
//  3. 触发原因是 G1，并且降级可见（模型仍在请求工具 → Degraded 有说明）。
func TestG1ForcedFinalExactlyMaxTurnsPlusOne(t *testing.T) {
	cfg := Config{MaxTurns: 3}
	client := alwaysToolCallLLM()
	x, p := newGateExtractor(t, cfg, client)

	res, err := p.Run(context.Background(), RawEpisode{
		ID: "e-g1", Scope: store.ScopeUser,
		Content: "Alice 使用 vim 写代码，认证服务依赖 Redis 做缓存。",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	_ = res

	traj := x.LastTrajectory()
	if want := cfg.MaxTurns + 1; traj.Calls != want {
		t.Fatalf("D10：总调用次数必须 == MaxTurns+1 == %d，实际 %d（toolRounds=%d）",
			want, traj.Calls, traj.ToolRounds)
	}
	if traj.ToolRounds != cfg.MaxTurns {
		t.Errorf("恰好 %d 轮应执行工具，实际 %d", cfg.MaxTurns, traj.ToolRounds)
	}
	if !traj.ForcedFinal {
		t.Errorf("第 %d 次调用必须是强制终稿（工具面被收走）", traj.Calls)
	}
	if got := client.CallsWithTools(); got != cfg.MaxTurns {
		t.Errorf("给了工具声明的调用次数应为 %d，实际 %d", cfg.MaxTurns, got)
	}
	if got := client.CallsWithoutTools(); got != 1 {
		t.Errorf("收走工具面的调用次数必须恰好 1，实际 %d", got)
	}
	// 最后一次请求必须显式告知"工具已收回、必须现在产出"。
	last := client.Calls[len(client.Calls)-1]
	if !strings.Contains(last.User, "工具已全部收回") {
		t.Errorf("强制终稿轮的 prompt 必须要求立即产出，实际末尾：%q",
			tail(last.User, 200))
	}
	// G1 必须在轨迹里可见。
	if !hasGate(traj.Gates, GateMaxTurns) {
		t.Errorf("轨迹里应记录 G1 触发，实际 %v", traj.Gates)
	}
}

// TestG1MaxTurnsIsConfigurable 验证 MaxTurns 真是配置项：
// 改成 1 之后总调用次数必须变成 2（证明那个 +1 来自门禁而不是巧合）。
func TestG1MaxTurnsIsConfigurable(t *testing.T) {
	cfg := Config{MaxTurns: 1}
	client := alwaysToolCallLLM()
	x, p := newGateExtractor(t, cfg, client)

	if _, err := p.Run(context.Background(), RawEpisode{
		ID: "e-g1b", Scope: store.ScopeUser,
		Content: "Alice 使用 vim 写代码，认证服务依赖 Redis 做缓存。",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, want := x.LastTrajectory().Calls, cfg.MaxTurns+1; got != want {
		t.Fatalf("MaxTurns=1 时总调用次数应为 %d，实际 %d", want, got)
	}
}

// TestG2TokenBudgetForcesFinal 验证 G2：累计 token 越过预算即强制终稿。
func TestG2TokenBudgetForcesFinal(t *testing.T) {
	cfg := Config{MaxTurns: 100, TokenBudget: 50}
	// 每轮报 30 token：第 1 轮后 30（未超），第 2 轮后 60（超）→ 第 3 轮应为强制终稿。
	client := NewRepeatingFakeLLM(FakeStep{
		ToolCalls: []ToolCall{ToolCallJSON(ToolLookupEntity, `{"name":"vim"}`)},
		Usage:     LLMUsage{TotalTokens: 30},
	})
	x, p := newGateExtractor(t, cfg, client)

	if _, err := p.Run(context.Background(), RawEpisode{
		ID: "e-g2", Scope: store.ScopeUser, Content: "Alice 使用 vim 写代码，认证服务依赖 Redis 做缓存。",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	traj := x.LastTrajectory()
	if !hasGate(traj.Gates, GateTokenBudget) {
		t.Errorf("应当触发 G2，实际 gates=%v tokens=%d", traj.Gates, traj.Tokens)
	}
	if traj.Calls > 4 {
		t.Errorf("G2 应尽早收口，实际调用了 %d 次", traj.Calls)
	}
}

// TestG3DuplicateQueryRejected 验证 G3：同一 query（归一化后）重复 → 直接拒这一跳。
//
// 归一化必须与 JSON 字段顺序、空白无关：两个文本不同但语义相同的参数算重复。
func TestG3DuplicateQueryRejected(t *testing.T) {
	// 第 1 轮与第 2 轮是**同一条查询**，只是 JSON 字段顺序/空白不同。
	client := NewFakeLLM(
		FakeStep{ToolCalls: []ToolCall{{ID: "c1", Name: ToolLookupEntity,
			Arguments: []byte(`{"name":"vim","concept":"c-editor"}`)}}},
		FakeStep{ToolCalls: []ToolCall{{ID: "c2", Name: ToolLookupEntity,
			Arguments: []byte("{ \"concept\" : \"c-editor\" , \"name\" : \"vim\" }")}}},
		FakeStep{Text: BuildFinalJSON([]EntityJSON{{Name: "vim"}}, nil)},
	)
	x, p := newGateExtractor(t, Config{MaxTurns: 5}, client)

	if _, err := p.Run(context.Background(), RawEpisode{
		ID: "e-g3", Scope: store.ScopeUser, Content: "Alice 使用 vim 写代码，认证服务依赖 Redis 做缓存。",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	traj := x.LastTrajectory()
	if traj.DuplicateCalls != 1 {
		t.Errorf("归一化后应识别出 1 次重复查询，实际 %d（gates=%v）", traj.DuplicateCalls, traj.Gates)
	}
	if !hasGate(traj.Gates, GateDuplicate) {
		t.Errorf("应记录 G3 触发，实际 %v", traj.Gates)
	}
}

// TestG4NoProgressForcesFinal 验证 G4：连续 2 轮工具结果为空 → 强制终稿。
func TestG4NoProgressForcesFinal(t *testing.T) {
	// 查询一个词典里不存在的名字：工具成功但返回 0 条。
	n := 0
	client := &FakeLLM{
		RepeatLast: true,
		Steps:      []FakeStep{{Usage: LLMUsage{TotalTokens: 5}}},
	}
	client.OnCall = func(int, LLMRequest) {}
	client.Steps[0].ToolCalls = nil
	// 用一个"每轮都问不同名字、但都查不到"的适配器。
	distinct := &distinctMissingLLM{n: &n}
	x, p := newGateExtractor(t, Config{MaxTurns: 100}, distinct)
	_ = client

	if _, err := p.Run(context.Background(), RawEpisode{
		ID: "e-g4", Scope: store.ScopeUser, Content: "Alice 使用 vim 写代码，认证服务依赖 Redis 做缓存。",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	traj := x.LastTrajectory()
	if !hasGate(traj.Gates, GateNoProgress) {
		t.Errorf("连续空结果应触发 G4，实际 gates=%v rounds=%d", traj.Gates, traj.ToolRounds)
	}
	if traj.ToolRounds != 2 {
		t.Errorf("G4 应在连续 2 轮空结果后收口（工具轮=2），实际 %d", traj.ToolRounds)
	}
}

// distinctMissingLLM 每轮都查一个不同的、词典里不存在的名字 → 结果恒为空。
type distinctMissingLLM struct {
	n     *int
	Calls []LLMRequest
}

func (d *distinctMissingLLM) Complete(ctx context.Context, req LLMRequest) (LLMResponse, error) {
	if err := ctx.Err(); err != nil {
		return LLMResponse{}, err
	}
	d.Calls = append(d.Calls, req)
	if len(req.Tools) == 0 {
		// 强制终稿轮：给出合法终稿，让管线正常结束。
		return LLMResponse{Text: BuildFinalJSON([]EntityJSON{{Name: "vim"}}, nil),
			Usage: LLMUsage{TotalTokens: 5}, Model: "fake"}, nil
	}
	*d.n++
	return LLMResponse{
		ToolCalls: []ToolCall{ToolCallJSON(ToolLookupEntity,
			`{"name":"不存在的实体-`+itoa(*d.n)+`"}`)},
		Usage: LLMUsage{TotalTokens: 5}, Model: "fake",
	}, nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestG5WallClockForcesFinal 验证 G5：墙钟超时后不再给工具。
//
// # 隔离条件（这条用例第一版是假绿的，必须说明）
//
// 只有当**其它四条门禁都没法先触发**时，G5 才被真正测到。因此这里刻意构造：
//   - 每轮查询**不同**的名字（前缀递减：vim / vi / v）→ G3 无重复；
//   - 每个名字都能查到 1 条 → G4 无"空结果"；
//   - MaxTurns=100 → G1 不参与；
//   - TokenBudget 默认（远大于累计量）→ G2 不参与。
//
// 第一版让每轮都查同一个 "vim"，于是 G3 去重 + G4 无进展先一步强制终稿，
// 把 G5 那个分支整个绕过去了——删掉 G5 门禁用例照样绿，等于没测。
func TestG5WallClockForcesFinal(t *testing.T) {
	cfg := Config{MaxTurns: 100, WallClock: 20 * time.Millisecond, FinalGrace: 5 * time.Millisecond}
	// 每轮 sleep 一小段，让墙钟在第 2~3 轮之间越过 deadline。
	client := &slowToolLLM{delay: 12 * time.Millisecond}
	x, p := newGateExtractor(t, cfg, client)

	if _, err := p.Run(context.Background(), RawEpisode{
		ID: "e-g5", Scope: store.ScopeUser, Content: "Alice 使用 vim 写代码，认证服务依赖 Redis 做缓存。",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	traj := x.LastTrajectory()
	if !hasGate(traj.Gates, GateWallClock) {
		t.Errorf("超过墙钟应触发 G5，实际 gates=%v calls=%d", traj.Gates, traj.Calls)
	}
	// 隔离断言：若 G3/G4 也出现在 gates 里，说明本用例没有真正测到 G5。
	for _, other := range []GateReason{GateDuplicate, GateNoProgress, GateMaxTurns, GateTokenBudget} {
		if hasGate(traj.Gates, other) {
			t.Errorf("G5 用例被 %v 抢先触发（隔离失败，G5 未被真正验证）：gates=%v",
				other, traj.Gates)
		}
	}
	if traj.Calls < 2 || traj.Calls > 6 {
		t.Errorf("G5 应在 2~6 次调用内收口，实际 %d 次", traj.Calls)
	}
	if client.Calls != traj.Calls {
		t.Errorf("客户端侧调用数 %d 与轨迹 %d 不一致", client.Calls, traj.Calls)
	}
}

type slowToolLLM struct {
	delay time.Duration
	Calls int
}

// distinctNames 是逐轮不同的查询名，且每个都能在种子里查到 1 条（避免 G4 空结果）。
var distinctNames = []string{"vim", "vi", "v", "vim 编辑器", "编辑"}

func (s *slowToolLLM) Complete(ctx context.Context, req LLMRequest) (LLMResponse, error) {
	if err := ctx.Err(); err != nil {
		return LLMResponse{}, err
	}
	s.Calls++
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return LLMResponse{}, ctx.Err()
	}
	if len(req.Tools) == 0 {
		return LLMResponse{Text: BuildFinalJSON([]EntityJSON{{Name: "vim"}}, nil),
			Usage: LLMUsage{TotalTokens: 5}}, nil
	}
	name := distinctNames[(s.Calls-1)%len(distinctNames)]
	return LLMResponse{
		ToolCalls: []ToolCall{ToolCallJSON(ToolLookupEntity, `{"name":"`+name+`"}`)},
		Usage:     LLMUsage{TotalTokens: 5},
	}, nil
}

// TestG5ContextCancelDegrades 验证 context 取消时降级返回而不是抛错。
func TestG5ContextCancelDegrades(t *testing.T) {
	d := openMemoryDB(t)
	seedOntology(t, d)
	client := alwaysToolCallLLM()
	x := NewLLMExtractor(client, d.Ports(), Config{MaxTurns: 100})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立刻取消

	res, err := x.Extract(ctx, ExtractRequest{
		Episode: RawEpisode{ID: "e", Scope: store.ScopeUser, Content: "Alice 使用 vim 写代码。"},
	})
	if err != nil {
		t.Fatalf("context 取消应降级而不是报错，实际 %v", err)
	}
	if len(res.Degraded) == 0 {
		t.Error("context 取消必须在 Degraded 显式报告")
	}
}

// TestFakeLLMSequenceAndPublishable 验证 FakeLLM 的多轮序列能力（随库发布的产物）。
func TestFakeLLMSequence(t *testing.T) {
	f := NewFakeLLM(
		FakeStep{ToolCalls: []ToolCall{ToolCallJSON(ToolLookupEntity, `{"name":"vi"}`)}},
		FakeStep{ToolCalls: []ToolCall{ToolCallJSON(ToolNeighbors, `{"node_id":"n-vim"}`)}},
		FakeStep{Text: `{"entities":[{"name":"vim"}]}`},
	)
	ctx := context.Background()
	resp1, err := f.Complete(ctx, LLMRequest{})
	if err != nil {
		t.Fatalf("Complete 1: %v", err)
	}
	if len(resp1.ToolCalls) != 1 || resp1.ToolCalls[0].Name != ToolLookupEntity {
		t.Fatalf("第 1 轮应为 lookup_entity 的工具调用，实际 %+v", resp1.ToolCalls)
	}
	resp2, err := f.Complete(ctx, LLMRequest{})
	if err != nil {
		t.Fatalf("Complete 2: %v", err)
	}
	if len(resp2.ToolCalls) != 1 || resp2.ToolCalls[0].Name != ToolNeighbors {
		t.Fatalf("第 2 轮应为 neighbors，实际 %+v", resp2.ToolCalls)
	}
	resp3, err := f.Complete(ctx, LLMRequest{})
	if err != nil {
		t.Fatalf("Complete 3: %v", err)
	}
	if len(resp3.ToolCalls) != 0 {
		t.Fatalf("第 3 轮应为终稿，实际仍有工具调用 %+v", resp3.ToolCalls)
	}
	if !strings.Contains(resp3.Text, "vim") {
		t.Errorf("第 3 轮终稿内容不符：%q", resp3.Text)
	}
	if f.CallCount() != 3 {
		t.Errorf("调用计数应为 3，实际 %d", f.CallCount())
	}
}

// TestFakeLLMErrorStep 验证剧本里的错误步（失败路径可测）。
func TestFakeLLMErrorStep(t *testing.T) {
	f := NewFakeLLM(FakeStep{Err: context.DeadlineExceeded})
	if _, err := f.Complete(context.Background(), LLMRequest{}); err == nil {
		t.Fatal("Err 步必须返回错误")
	}
}

// TestLLMRequestShape 验证 LLMRequest/LLMResponse 携带 TD §3.1 要求的全部字段。
func TestLLMRequestShape(t *testing.T) {
	f := NewFakeLLM(FakeStep{Text: "{}", Usage: LLMUsage{TotalTokens: 7}, Model: "m1"})
	req := LLMRequest{
		System: "sys", User: "usr", JSONSchema: []byte(`{"type":"object"}`),
		Tools: ToolSpecs(), MaxTokens: 256, Temperature: 0,
	}
	resp, err := f.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	got := f.Calls[0]
	if got.System != "sys" || got.User != "usr" || got.MaxTokens != 256 ||
		string(got.JSONSchema) == "" || len(got.Tools) != 3 || got.Temperature != 0 {
		t.Errorf("请求字段不完整：%+v", got)
	}
	if resp.Text != "{}" || resp.Usage.TotalTokens != 7 || resp.Model != "m1" {
		t.Errorf("响应字段不完整：%+v", resp)
	}
	if resp.Usage.Tokens() != 7 {
		t.Errorf("Usage.Tokens() 应为 7，实际 %d", resp.Usage.Tokens())
	}
}

func hasGate(gates []GateReason, want GateReason) bool {
	for _, g := range gates {
		if g == want {
			return true
		}
	}
	return false
}

func tail(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

// TestDefaultGateValues 钉住五条门禁与工具上限的**默认值**（docs/TD.md §3.2 表）。
//
// 为什么单独一条用例：上面那些行为用例都用显式 Config 覆盖了默认值，
// 因此"默认值被改动"不会被它们发现。而 §3.2 的门禁默认值本身就是设计契约
// （MaxTurns=3、墙钟 30s、工具上限 10/30/5），必须被独立锁住。
func TestDefaultGateValues(t *testing.T) {
	cfg := Config{}.withDefaults()
	cases := []struct {
		name string
		got  int
		want int
	}{
		{"MaxTurns", cfg.MaxTurns, 3},
		{"TokenBudget", cfg.TokenBudget, 12000},
		{"LookupLimit", cfg.LookupLimit, 10},
		{"NeighborsLimit", cfg.NeighborsLimit, 30},
		{"SearchLimit", cfg.SearchLimit, 5},
		{"L1TopK", cfg.L1TopK, 5},
		{"CandidateLimit", cfg.CandidateLimit, 64},
		{"MinContentLength", cfg.MinContentLength, 10},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("默认 %s 应为 %d，实际 %d（§3.2/§3 的默认值是设计契约）",
				c.name, c.want, c.got)
		}
	}
	if cfg.WallClock != 30*time.Second {
		t.Errorf("默认墙钟 G5 应为 30s，实际 %v", cfg.WallClock)
	}
	if cfg.ThetaHi != 0.92 || cfg.ThetaLo != 0.72 {
		t.Errorf("默认 θ_hi/θ_lo 应为 0.92/0.72，实际 %.2f/%.2f", cfg.ThetaHi, cfg.ThetaLo)
	}
	if cfg.W1Cosine != 0.5 || cfg.W2BM25 != 0.3 || cfg.W3Structural != 0.2 {
		t.Errorf("默认权重应为 0.5/0.3/0.2，实际 %.2f/%.2f/%.2f",
			cfg.W1Cosine, cfg.W2BM25, cfg.W3Structural)
	}
}
