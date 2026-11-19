// Package llm 实现 docs/TD.md §3.3 / D7 / D8 的**LLM 注入点解析**。
//
// # 三条不可退让的边界
//
//  1. **不 import 任何 LLM SDK**（D7）：本包的 import 只有标准库的
//     encoding/json / os / strconv / strings / time 与 gmem 自己的 extract。
//  2. **不自己发请求**：仓库的依赖方向守卫（internal/vtest/no_network_test.go）
//     禁止库本体 import net/http，也禁止服务入口出现出站客户端用法。因此
//     "直连 OpenAI 兼容端点"这一条在这里表达为**可注入的 Client**：
//     真正持有 HTTP 客户端的是宿主（Go package 嵌入方），本包只负责
//     "配置优先 → sampling 兜底 → 显式降级"这个**决策**。
//  3. **降级必须可见**（§13 风险清单最容易出的事故）：本包的每一项决策都带
//     Degraded 文案，doctor 与工具响应都会原样透出，绝不静默。
//
// # 优先级（D8）
//
//	GMEM_LLM_* 已配置  → mode=config   （使用者已明确选择模型/成本/合规）
//	未配置 + 可 sampling → mode=sampling（零密钥；LLM 在客户端那侧）
//	两者都无           → mode=degraded（L0+L1 仍可跑，抽取能力降级）
//
// 为什么"配置优先"：sampling 依赖客户端能力与用户弹窗，行为不可预测；
// 只在"没交代过怎么办"时才去问客户端要模型。
package llm

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tim5wang/gmem/extract"
	"github.com/tim5wang/gmem/store"
)

// Mode 是 LLM 的来源（D8 的三条分支）。
type Mode string

const (
	// ModeConfig：读到了 GMEM_LLM_* 配置。
	ModeConfig Mode = "config"
	// ModeSampling：没有配置，但可以通过 MCP sampling 向客户端要模型。
	ModeSampling Mode = "sampling"
	// ModeDegraded：两者都不可用，抽取降级为 L0+L1。
	ModeDegraded Mode = "degraded"
)

// Config 是从 GMEM_LLM_* 环境变量解析出的配置（TD §3.3 的表）。
type Config struct {
	BaseURL     string
	APIKey      string
	Model       string
	MaxTokens   int
	Timeout     time.Duration
	ExtractMode string // auto | l0_only | off
}

// Configured 报告配置是否"够用"：端点与模型都给了才算配置完成。
//
// 只给了 BASE_URL 而没给 MODEL 时不视为已配置——那种情况发出去的请求必然失败，
// 把它当成"已配置"只会让用户看到一堆 4xx 而不是一句"你少配了 MODEL"。
func (c Config) Configured() bool {
	return strings.TrimSpace(c.BaseURL) != "" && strings.TrimSpace(c.Model) != ""
}

// Partial 报告"给了配置但不够用"（用于显式告警，而不是静默降级）。
func (c Config) Partial() bool {
	return (strings.TrimSpace(c.BaseURL) != "" || strings.TrimSpace(c.Model) != "") && !c.Configured()
}

// FromEnv 从环境变量解析配置。缺省值：MaxTokens=1024，Timeout=60s，ExtractMode=auto。
func FromEnv(getenv func(string) string) Config {
	if getenv == nil {
		getenv = os.Getenv
	}
	c := Config{
		BaseURL:     strings.TrimSpace(getenv("GMEM_LLM_BASE_URL")),
		APIKey:      strings.TrimSpace(getenv("GMEM_LLM_API_KEY")),
		Model:       strings.TrimSpace(getenv("GMEM_LLM_MODEL")),
		MaxTokens:   1024,
		Timeout:     60 * time.Second,
		ExtractMode: strings.TrimSpace(getenv("GMEM_EXTRACT_MODE")),
	}
	if v := strings.TrimSpace(getenv("GMEM_LLM_MAX_TOKENS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.MaxTokens = n
		}
	}
	if v := strings.TrimSpace(getenv("GMEM_LLM_TIMEOUT")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			c.Timeout = d
		}
	}
	if c.ExtractMode == "" {
		c.ExtractMode = "auto"
	}
	return c
}

// ValidExtractMode 校验 GMEM_EXTRACT_MODE 的取值。
func ValidExtractMode(m string) bool {
	switch m {
	case "auto", "l0_only", "off":
		return true
	}
	return false
}

// Sampler 是 MCP `sampling/createMessage` 的能力接口（零密钥路径）。
//
// 由 MCP 服务端实现（发一条 JSON-RPC 请求给客户端并等结果）；
// CLI 没有客户端通道，因此 CLI 下它恒为 nil → 走降级。
type Sampler interface {
	// SamplingAvailable 报告客户端是否声明了 sampling 能力。
	SamplingAvailable() bool
	// CreateMessage 发一次 sampling 请求，返回模型给出的文本。
	CreateMessage(ctx context.Context, req SamplingRequest) (string, error)
}

// SamplingRequest 是 sampling/createMessage 的参数（按 MCP spec 的常用子集）。
type SamplingRequest struct {
	System      string
	Prompt      string
	MaxTokens   int
	Temperature float64
	// 模型偏好用优先级表达（服务端不知道该客户端有什么模型）。
	IntelligencePriority float64
	SpeedPriority        float64
	CostPriority         float64
}

// Strategy 是一次解析的结论。
type Strategy struct {
	Mode   Mode
	Config Config
	// Client 是生效的 LLM 客户端（Mode=config 且注入了实现时非 nil）。
	Client extract.LLMClient
	// Degraded 是**必须可见**的降级说明（doctor 与工具响应都原样透出）。
	Degraded []string
	// Warnings 是非致命的配置问题（例如只配了一半）。
	Warnings []string
}

// ExtractEnabled 报告是否应尝试 L2 抽取。
//
// GMEM_EXTRACT_MODE=off → 不抽取（只落原文 + L0 规则）；
// l0_only → 只跑确定性规则；auto → 按可用性决定。
func (s Strategy) ExtractEnabled() bool { return s.Config.ExtractMode != "off" }

// Resolve 按 D8 的优先级决定 LLM 来源。
//
// 参数 client 是宿主注入的实现（Go package 嵌入方自己持有 HTTP 客户端）；
// CLI / MCP 都没有宿主，因此传 nil。传 nil 时：
//   - 配置齐备 → 仍报 Degraded（"配置了但本二进制未内置出站客户端"），并尝试
//     sampling 兜底；这与"静默当作没配置"有本质区别：*用户能看到为什么*。
//   - 配置未给 → sampling（若可用），否则 degraded。
func Resolve(cfg Config, client extract.LLMClient, sampler Sampler) Strategy {
	s := Strategy{Config: cfg}
	if !ValidExtractMode(cfg.ExtractMode) {
		s.Warnings = append(s.Warnings, fmt.Sprintf(
			"GMEM_EXTRACT_MODE=%q 非法（只接受 auto / l0_only / off），按 auto 处理", cfg.ExtractMode))
		s.Config.ExtractMode = "auto"
	}
	if cfg.Partial() {
		s.Warnings = append(s.Warnings, fmt.Sprintf(
			"GMEM_LLM_* 配置不完整（需要同时给 GMEM_LLM_BASE_URL 与 GMEM_LLM_MODEL；当前 base_url=%q model=%q），按未配置处理",
			cfg.BaseURL, cfg.Model))
	}

	switch {
	case cfg.Configured() && client != nil:
		s.Mode = ModeConfig
		s.Client = client
		return s

	case cfg.Configured() && client == nil:
		// 配置齐备但没有出站客户端：先说清楚，再看 sampling 能不能兜。
		s.Degraded = append(s.Degraded, fmt.Sprintf(
			"GMEM_LLM_* 已配置（base_url=%s model=%s），但当前入口未内置出站 HTTP 客户端"+
				"（仓库依赖守卫禁止库本体自建 HTTP 客户端，D7）；抽取降级，改走 sampling 或 L0+L1",
			cfg.BaseURL, cfg.Model))
		if sampler != nil && sampler.SamplingAvailable() {
			s.Mode = ModeSampling
			return s
		}
		s.Mode = ModeDegraded
		s.Degraded = append(s.Degraded,
			"sampling 不可用（客户端未声明 sampling 能力）：抽取降级为 L0+L1，vector lane 亦缺席")
		return s

	case sampler != nil && sampler.SamplingAvailable():
		s.Mode = ModeSampling
		return s

	default:
		s.Mode = ModeDegraded
		s.Degraded = append(s.Degraded,
			"未配置 GMEM_LLM_*，且客户端未声明 sampling 能力：抽取降级为 L0+L1（确定性规则），"+
				"实体/关系抽取需要宿主注入 LLMClient（D7）")
		return s
	}
}

// Status 是可序列化的状态快照（doctor 与 MCP 工具响应共用）。
type Status struct {
	Mode string `json:"mode"`
	// Ready 表示"现在真的能调 LLM"。
	Ready bool `json:"ready"`
	// ExtractMode 是生效的抽取策略（auto / l0_only / off）。
	ExtractMode string `json:"extract_mode"`
	Model       string `json:"model,omitempty"`
	BaseURL     string `json:"base_url,omitempty"`
	// Sampling 是"客户端是否声明 sampling 能力"。
	Sampling bool     `json:"sampling_available"`
	Degraded []string `json:"degraded,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// Status 投影成可序列化快照。
func (s Strategy) Status(sampling bool) Status {
	return Status{
		Mode: string(s.Mode), Ready: s.Client != nil,
		ExtractMode: s.Config.ExtractMode, Model: s.Config.Model, BaseURL: s.Config.BaseURL,
		Sampling: sampling, Degraded: s.Degraded, Warnings: s.Warnings,
	}
}

// Degraded 是给 doctor / 工具响应用的一句话摘要。
func (s Strategy) DegradedText() string {
	if len(s.Degraded) == 0 {
		return ""
	}
	return strings.Join(s.Degraded, "；")
}

// ===========================================================================
// sampling 适配：把 extract.LLMClient 落到"向客户端要模型"
// ===========================================================================

// SamplingClient 用 MCP sampling 实现 extract.LLMClient。
//
// # 为什么这能成立（D7）
//
// MCP 的 sampling 是"服务器请求**客户端**做一次生成"的标准能力：
// 密钥、模型选择、人在环路都在客户端那侧。Gmem 只是把 prompt 交上去、
// 把文本收回来——它仍然**不直连任何 LLM、不持有任何密钥**。
//
// 两条路径（config / sampling）的行为必须一致：产出的 LLMResponse 形状、
// 校验规则、降级语义完全相同。差异只在"模型从哪来"，不渗透到业务逻辑。
type SamplingClient struct {
	Sampler Sampler
	// MaxTokens 与 Temperature 透传到 sampling 请求。
	MaxTokens   int
	Temperature float64
}

// Complete 实现 extract.LLMClient。
func (c *SamplingClient) Complete(ctx context.Context, req extract.LLMRequest) (extract.LLMResponse, error) {
	if c.Sampler == nil || !c.Sampler.SamplingAvailable() {
		return extract.LLMResponse{}, fmt.Errorf("llm: sampling 不可用（客户端未声明能力）")
	}
	maxTokens := c.MaxTokens
	if maxTokens <= 0 {
		maxTokens = req.MaxTokens
	}
	if maxTokens <= 0 {
		maxTokens = 1024
	}
	prompt := req.User
	if len(req.JSONSchema) > 0 {
		// 把输出契约一并交上去：sampling 路径没有结构化输出的强约束，
		// 不显式给出 schema 就只能依赖模型自觉。
		prompt += "\n\n请严格按以下 JSON Schema 输出一个 JSON 对象（不要 Markdown 围栏）：\n" +
			string(req.JSONSchema)
	}
	text, err := c.Sampler.CreateMessage(ctx, SamplingRequest{
		System: req.System, Prompt: prompt, MaxTokens: maxTokens,
		Temperature:          c.Temperature,
		IntelligencePriority: 0.7, SpeedPriority: 0.3,
	})
	if err != nil {
		return extract.LLMResponse{}, fmt.Errorf("llm: sampling/createMessage: %w", err)
	}
	return extract.LLMResponse{Text: text, Model: "mcp-sampling"}, nil
}

// NewExtractor 是两条路径的**唯一**构造点：config 与 sampling 都落成同一个
// extract.LLMExtractor（循环、工具、收敛全部归 Gmem，D10）。
//
// 这样"同一片段走配置或走 sampling，产出的结果形状与降级语义完全相同"
// 就不再靠约定，而是**同一段代码**的自然结果（§3.3 第 3 条注意点）。
func NewExtractor(client extract.LLMClient, ports store.Ports, cfg extract.Config) *extract.LLMExtractor {
	return extract.NewLLMExtractor(client, ports, cfg)
}
