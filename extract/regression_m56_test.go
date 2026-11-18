package extract

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/tim5wang/gmem/store"
)

// ---------------------------------------------------------------------------
// M5：L0 定义句主语收紧（长句片段不得成为实体）
// ---------------------------------------------------------------------------

// TestM5DefinitionSubjectRejectsSentenceFragment 是 M5 的回归用例。
//
// 缺陷：定义句正则允许任意 1-40 字符作主语（含空格与逗号），于是
// "vim 使用 插件管理器 来扩展功能，这 是 …" 产出一个 19 字的垃圾实体，
// 每写入一段含"是"的长句就凭空多一个节点 + alias，污染消歧候选池。
// 修法：主语长度上限 + 不得含句读标点。
//
// 变异验证：去掉长度/标点这两道闸，本用例必须变红。
func TestM5DefinitionSubjectRejectsSentenceFragment(t *testing.T) {
	l := l0WithDict(t)
	cases := []struct {
		name    string
		content string
		bad     string // 不得作为定义句主语出现的片段前缀
	}{
		{
			name:    "长句片段（含逗号）",
			content: "vim 使用 插件管理器 来扩展功能，这是一条足够长的句子。",
			bad:     "vim 使用",
		},
		{
			name:    "含句号的长主语",
			content: "今天讨论了很多事情。缓存服务是这一层的核心组件，需要重点优化。",
			bad:     "今天讨论",
		},
		{
			// 冒号规则（X：Y）也受同一道闸约束：主语过长时不得成概念。
			// 注意不能用“回忆昨天的会议内容：…”这类：6-12 字的短语本身就是可命名主语，
			// 那是合法定义句（冒号规则的本意），不是句子片段。
			name:    "冒号规则下的过长主语",
			content: "今天开会讨论了缓存层的实现细节：缓存服务是这一层的核心组件。",
			bad:     "今天开会讨论",
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			res, err := l.Extract(context.Background(), RawEpisode{
				ID: "e-m5", Scope: store.ScopeUser, Content: c.content,
			})
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}
			for _, f := range findingsOf(res, CatDefinition) {
				if strings.HasPrefix(f.Text, c.bad) {
					t.Errorf("句子片段被当成概念：%q（来自 %q）", f.Text, c.content)
				}
				if len([]rune(f.Text)) > MaxDefinitionSubjectRunes {
					t.Errorf("定义句主语 %q 长 %d 字，超过上限 %d",
						f.Text, len([]rune(f.Text)), MaxDefinitionSubjectRunes)
				}
				if strings.ContainsAny(f.Text, "，,。；;：:！!？?") {
					t.Errorf("定义句主语含句读标点（句子片段特征）：%q", f.Text)
				}
			}
		})
	}
}

// TestM5LegitDefinitionStillExtracted 确保收紧没有过度到"合法定义句全部干掉"。
// 变异验证：把主语长度上限改到 1，本用例必须变红。
func TestM5LegitDefinitionStillExtracted(t *testing.T) {
	l := l0WithDict(t)
	res, err := l.Extract(context.Background(), RawEpisode{
		ID: "e-m5b", Scope: store.ScopeUser,
		Content: "身份认证服务是一种校验调用方身份的中间件。灰度发布指的是按比例放量的上线方式。",
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	fs := findingsOf(res, CatDefinition)
	got := map[string]bool{}
	for _, f := range fs {
		got[f.Text] = true
	}
	for _, want := range []string{"身份认证服务", "灰度发布"} {
		if !got[want] {
			t.Errorf("合法定义句主语 %q 不应被收紧规则干掉，实际 %v", want, textsOf(fs))
		}
	}
}

// ---------------------------------------------------------------------------
// M6：LLMExtractor 的轨迹必须并发安全
// ---------------------------------------------------------------------------

// TestM6ConcurrentExtractRaceFree 是 M6 的回归用例。
//
// 缺陷：最近一次轨迹字段被 Extract 的 defer 无条件写、被 LastTrajectory 读，
// 旧注释用"测试是串行跑的"当理由——那不是库的契约。生产上"一个 scope 一个
// goroutine 并发跑 Pipeline.Run"是常见形态，这是对切片的并发写。
// 修法：mutex 保护。
//
// # 为什么需要一个自带锁的 client
//
// 本用例的靶子是 **LLMExtractor 的共享字段 last**。随库发布的测试替身 FakeLLM
// 自己也有一把**没上锁**的共享调用记录（见卡评论里的额外发现），那是另一个
// 缺陷、不在 M6 范围内。用这个最小 client 才能把靶子隔离开，
// 否则 -race 会把替身的问醒目算到 Extractor 头上。
//
// 变异验证：去掉 LastTrajectory 的 mu.Lock/Unlock 或 defer 里的 mu.Lock/Unlock，
// `go test -race ./extract/ -run TestM6` 必须报 data race（靶点就是 last）。
func TestM6ConcurrentExtractRaceFree(t *testing.T) {
	d := openMemoryDB(t)
	seedOntology(t, d)
	// 共享 x（含 last）；client 自己持锁，ports 的并发安全由 sqlite 保证。
	x := NewLLMExtractor(&lockedToolLLM{}, d.Ports(), Config{MaxTurns: 2})

	const workers = 8
	done := make(chan struct{}, workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			_, _ = x.Extract(context.Background(), ExtractRequest{
				Episode: RawEpisode{ID: "e-m6", Scope: store.ScopeUser,
					Content: "Alice 使用 vim 写代码，认证服务依赖 Redis 做缓存。"},
			})
			_ = x.LastTrajectory()
		}()
	}
	for i := 0; i < workers; i++ {
		<-done
	}
	// 语义上也必须成立：并发后轨迹仍是"某一次完整运行"的结果（不是零值/半写）。
	if traj := x.LastTrajectory(); traj.Calls == 0 {
		t.Error("并发后 LastTrajectory 不应是零值（轨迹被写入）")
	}
}

// lockedToolLLM 是一个**自带锁**的最小 client：永远请求同一工具。
//
// 它存在的唯一理由是：让 M6 的 -race 断言只可能命中 LLMExtractor.last，
// 而不是测试替身自己的共享状态。
type lockedToolLLM struct {
	mu    sync.Mutex
	calls int
}

func (l *lockedToolLLM) Complete(ctx context.Context, req LLMRequest) (LLMResponse, error) {
	if err := ctx.Err(); err != nil {
		return LLMResponse{}, err
	}
	l.mu.Lock()
	l.calls++
	l.mu.Unlock()
	if len(req.Tools) == 0 {
		return LLMResponse{Text: BuildFinalJSON([]EntityJSON{{Name: "vim"}}, nil),
			Usage: LLMUsage{TotalTokens: 5}, Model: "locked"}, nil
	}
	return LLMResponse{
		ToolCalls: []ToolCall{ToolCallJSON(ToolLookupEntity, `{"name":"vim"}`)},
		Usage:     LLMUsage{TotalTokens: 5}, Model: "locked",
	}, nil
}
