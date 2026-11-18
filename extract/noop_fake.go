package extract

import (
	"context"
	"fmt"
	"strings"
)

// ===========================================================================
// NoopExtractor：不注入 LLM 时的默认实现（docs/TD.md §3.1 第三种接线方式）
//
// 「完全离线 / 单元测试 → NoopExtractor{}，退化到 L0+L1」。
//
// 两条硬约束：
//  1. 它必须**跑通全量测试**（CI 门禁：不注入 LLM 时 L0+L1 可跑、功能不崩溃）；
//  2. 降级必须**在 Degraded 里显式上报**，绝不静默（§13 风险表：
//     "宿主不实现 LLMClient → 抽取降级、体验变差 → 降级必须可见"）。
//
// 它刻意返回**空**的 Entities/Relations：L0 的确定性结果由 Pipeline 直接从
// L0Result 落库，不经由 Extractor。NoopExtractor 只负责"L2 这一层没有可用模型"
// 这个事实与它的显式报告。
// ===========================================================================

// NoopExtractor 是可用的空抽取器：不调用任何模型，L0+L1 仍然完整工作。
type NoopExtractor struct{}

var _ Extractor = NoopExtractor{}

// Extract 返回空结果 + 一条显式降级说明。
//
// 注意它**不报错**：报错会让调用方误以为整条管线失败；实际语义是
// "L2 缺席，L0+L1 的产物依然有效"。可见性由 Degraded 承担。
func (NoopExtractor) Extract(ctx context.Context, req ExtractRequest) (ExtractResult, error) {
	if err := ctx.Err(); err != nil {
		return ExtractResult{}, err
	}
	return ExtractResult{
		Degraded: []string{
			"L2 抽取已降级：使用 NoopExtractor（未注入 LLMClient）；" +
				"仅 L0 确定性提取 + L1 词法聚类生效，实体/关系抽取质量下降",
		},
	}, nil
}

// ===========================================================================
// FakeLLM：随库发布的测试替身（docs/TD.md §3.1「测试替身」）
//
// 「`FakeLLM{Responses []string}` 必须随库发布，让使用者的 CI 不依赖网络与密钥。
// 它同时要能模拟带 tool-call 的多轮序列（第 N 轮返回 tool call、第 N+1 轮返回终稿），
// 否则 D10 的收敛逻辑无法被测。」
//
// 因此它不是 internal 测试包里的私有类型，而是**公开产物**：宿主实现了自己的
// LLMClient 之后，仍可用它跑自己的 CI。
// ===========================================================================

// FakeStep 是 FakeLLM 剧本里的一步。
//
// 三种用法：
//   - 只设 Text：这一轮直接给终稿；
//   - 设 ToolCalls：这一轮返回工具调用（多轮序列）；
//   - 设 Err：这一轮报错（测失败路径）。
type FakeStep struct {
	Text      string
	ToolCalls []ToolCall
	Usage     LLMUsage
	Model     string
	Truncated bool
	Err       error
}

// FakeLLM 是按剧本应答的 LLMClient 实现。
//
// # 剧本耗尽的语义
//
// RepeatLast 为 true 时，最后一轮会被无限重复——这正是 D10 那条验收用例需要的行为：
// 「永远返回 tool call」的模型只能这样表达，然后断言恰好 MaxTurns 轮工具 +
// 1 次强制终稿 = MaxTurns + 1 次调用。
//
// RepeatLast 为 false 时，剧本耗尽后返回一个空 Text 的响应（即"无工具调用的终稿"），
// 让忘记准备终稿的测试快速失败而不是死循环。
type FakeLLM struct {
	Steps      []FakeStep
	RepeatLast bool
	// Calls 记录每次收到的请求（只读观察用；按调用顺序追加）。
	Calls []LLMRequest
	// OnCall 是可选钩子，便于断言"第几轮给了工具、第几轮没给"。
	OnCall func(n int, req LLMRequest)
}

var _ LLMClient = (*FakeLLM)(nil)

// NewFakeLLM 用若干步构造一个 FakeLLM（默认不重复最后一步）。
func NewFakeLLM(steps ...FakeStep) *FakeLLM {
	return &FakeLLM{Steps: steps}
}

// NewRepeatingFakeLLM 构造一个"永远重复最后一步"的 FakeLLM。
func NewRepeatingFakeLLM(steps ...FakeStep) *FakeLLM {
	return &FakeLLM{Steps: steps, RepeatLast: true}
}

// Complete 实现 LLMClient。
func (f *FakeLLM) Complete(ctx context.Context, req LLMRequest) (LLMResponse, error) {
	if err := ctx.Err(); err != nil {
		return LLMResponse{}, err
	}
	n := len(f.Calls)
	f.Calls = append(f.Calls, req)
	if f.OnCall != nil {
		f.OnCall(n, req)
	}
	if len(f.Steps) == 0 {
		return LLMResponse{}, nil
	}
	idx := n
	if idx >= len(f.Steps) {
		if !f.RepeatLast {
			return LLMResponse{}, nil
		}
		idx = len(f.Steps) - 1
	}
	step := f.Steps[idx]
	if step.Err != nil {
		return LLMResponse{}, step.Err
	}
	model := step.Model
	if model == "" {
		model = "fake-llm"
	}
	return LLMResponse{
		Text:      step.Text,
		ToolCalls: step.ToolCalls,
		Usage:     step.Usage,
		Model:     model,
		Truncated: step.Truncated,
	}, nil
}

// CallCount 返回已收到的调用次数。
func (f *FakeLLM) CallCount() int { return len(f.Calls) }

// CallsWithTools 返回"给了工具声明"的调用次数（D10 断言用）。
func (f *FakeLLM) CallsWithTools() int {
	n := 0
	for _, c := range f.Calls {
		if len(c.Tools) > 0 {
			n++
		}
	}
	return n
}

// CallsWithoutTools 返回"收走工具面"的调用次数（强制终稿轮）。
func (f *FakeLLM) CallsWithoutTools() int {
	n := 0
	for _, c := range f.Calls {
		if len(c.Tools) == 0 {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// 剧本构造函数（让使用者的测试读起来是"轨迹"而不是裸 JSON）
// ---------------------------------------------------------------------------

// ToolCallJSON 构造一个 tool call，arguments 是 JSON 文本。
func ToolCallJSON(name, argsJSON string) ToolCall {
	return ToolCall{ID: "call-" + name, Name: name, Arguments: []byte(argsJSON)}
}

// FinalJSONStep 构造一个"直接给终稿"的步骤。
func FinalJSONStep(json string) FakeStep {
	return FakeStep{Text: json}
}

// EntityJSON 是 FakeLLM 常用的终稿 JSON 片段（便于测试拼装）。
//
// 只覆盖测试最常用的形状：实体 + 关系。
type EntityJSON struct {
	Name    string   `json:"name"`
	Aliases []string `json:"aliases,omitempty"`
	Concept string   `json:"concept,omitempty"`
	Def     string   `json:"definition,omitempty"`
}

// BuildFinalJSON 用实体与关系拼出一个合法终稿 JSON。
//
// 刻意手写序列化而不依赖结构体 tag 顺序：测试里读 JSON 文本比读结构体更直接，
// 且不引入额外类型到公开面。
func BuildFinalJSON(entities []EntityJSON, relations [][3]string) string {
	var b strings.Builder
	b.WriteString(`{"entities":[`)
	for i, e := range entities {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(fmt.Sprintf(`{"name":%q`, e.Name))
		if len(e.Aliases) > 0 {
			b.WriteString(`,"aliases":[`)
			for j, a := range e.Aliases {
				if j > 0 {
					b.WriteString(",")
				}
				b.WriteString(fmt.Sprintf("%q", a))
			}
			b.WriteString("]")
		}
		if e.Concept != "" {
			b.WriteString(fmt.Sprintf(`,"concept":%q`, e.Concept))
		}
		if e.Def != "" {
			b.WriteString(fmt.Sprintf(`,"definition":%q`, e.Def))
		}
		b.WriteString(`,"confidence":0.9}`)
	}
	b.WriteString(`],"relations":[`)
	for i, r := range relations {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(fmt.Sprintf(`{"from":%q,"to":%q,"relation":%q,"confidence":0.8}`, r[0], r[1], r[2]))
	}
	b.WriteString(`],"concepts":[]}`)
	return b.String()
}
