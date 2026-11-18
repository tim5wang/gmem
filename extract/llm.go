package extract

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// L2：LLMExtractor（默认 Extractor 实现，建在 LLMClient 之上）
//
// **循环归 Gmem**（D10）。宿主实现 LLMClient 时只管"给我这一轮输出"，
// 多轮循环、工具执行、预算记账、五条收敛门禁全部在这里，
// 因此"确保收敛"是可验证的硬保证，而不是对模型的期望。
// ===========================================================================

// LLMExtractor 用注入的 LLMClient 做 L2 抽取。
type LLMExtractor struct {
	client LLMClient
	ports  store.Ports
	cfg    Config
	// SystemPrompt / ResponseSchema 允许宿主整段覆盖（§3.1 责任边界第 1 条）。
	SystemPrompt   string
	ResponseSchema json.RawMessage
	// last 是最近一次 Extract 的轨迹（由 LastTrajectory 暴露）。
	//
	// 它只被 LoopOwned 的调用点写一次，且测试是串行跑的；
	// 若同一 Extractor 被并发使用，语义是"最后一次"，不影响抽取结果。
	last Trajectory
}

// NewLLMExtractor 构造 L2 抽取器。
//
// ports 用于 tool-calling 的三个**只读**工具；client 为 nil 时退化为 NoopExtractor
// 的语义（但显式返回错误比静默降级更安全——调用方应自己选 NoopExtractor）。
func NewLLMExtractor(client LLMClient, ports store.Ports, cfg Config) *LLMExtractor {
	x := &LLMExtractor{client: client, ports: ports, cfg: cfg.withDefaults()}
	x.SystemPrompt = defaultSystemPrompt
	x.ResponseSchema = json.RawMessage(defaultResponseSchema)
	return x
}

// Config 返回生效配置。
func (x *LLMExtractor) Config() Config { return x.cfg }

const defaultSystemPrompt = `你是记忆整理管线中的实体/关系抽取器。输入是一段原文、
已知实体候选，以及（可能多轮的）只读查询结果。

要求：
1. 只输出一个 JSON 对象，不要任何解释、Markdown 或代码围栏。
2. 实体名必须是原文里出现过的表面形式，不要臆造、不要翻译。
3. 关系的两端必须出现在 entities 里，或出现在"已知实体"清单里。
4. relation 只能取白名单：subtype_of, part_of, depends_on, caused_by, works_at, owns, uses。
5. 需要更多信息时优先使用提供的只读工具；工具结果被标记 truncated 时不要假设"没有更多"。
6. 不确定的实体给低 confidence，不要为了凑数而拆分同一实体。`

const defaultResponseSchema = `{
  "type": "object",
  "properties": {
    "entities": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "name":       {"type": "string"},
          "aliases":    {"type": "array", "items": {"type": "string"}},
          "concept":    {"type": "string"},
          "definition": {"type": "string"},
          "confidence": {"type": "number", "minimum": 0, "maximum": 1}
        },
        "required": ["name"]
      }
    },
    "relations": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "from":       {"type": "string"},
          "to":         {"type": "string"},
          "relation":   {"type": "string"},
          "confidence": {"type": "number"}
        },
        "required": ["from", "to", "relation"]
      }
    },
    "concepts": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "id":         {"type": "string"},
          "name":       {"type": "string"},
          "definition": {"type": "string"}
        },
        "required": ["name"]
      }
    }
  },
  "required": ["entities"]
}`

// llmPayload 是模型终稿的 JSON 形状。
type llmPayload struct {
	Entities []struct {
		Name       string   `json:"name"`
		Aliases    []string `json:"aliases"`
		Concept    string   `json:"concept"`
		Definition string   `json:"definition"`
		Confidence float64  `json:"confidence"`
	} `json:"entities"`
	Relations []struct {
		From       string  `json:"from"`
		To         string  `json:"to"`
		Relation   string  `json:"relation"`
		Confidence float64 `json:"confidence"`
	} `json:"relations"`
	Concepts []struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		Definition string `json:"definition"`
	} `json:"concepts"`
}

// GateReason 是一条收敛门禁被触发的原因（进 Degraded，绝不静默）。
type GateReason string

// 五条收敛门禁（docs/TD.md §3.2）。
const (
	// GateMaxTurns 是 G1：工具轮数达到 MaxTurns（默认 3）。
	GateMaxTurns GateReason = "G1 MaxTurns"
	// GateTokenBudget 是 G2：累计 token 超过 Budget。
	GateTokenBudget GateReason = "G2 token 预算"
	// GateDuplicate 是 G3：同一 query（归一化后）重复，该跳被直接拒。
	GateDuplicate GateReason = "G3 同 query 重复"
	// GateNoProgress 是 G4：连续 2 轮工具结果为空/无新增节点。
	GateNoProgress GateReason = "G4 无进展"
	// GateWallClock 是 G5：墙钟超时（默认 30s）。
	GateWallClock GateReason = "G5 墙钟超时"
)

// trajectory 记录一次 L2 的运行轨迹，用于断言与 explain（§3.2 要求可审计）。
// Trajectory 记录一次 L2 的运行轨迹，用于断言与 explain（§3.2 要求可审计）。
type Trajectory struct {
	// Calls 是**实际调用 LLMClient 的次数**。
	Calls int
	// ToolRounds 是**实际执行工具的轮数**。
	ToolRounds int
	// Tokens 是累计 token。
	Tokens int
	// ForcedFinal 表示终稿是强制产生的（工具面已被收走）。
	ForcedFinal bool
	// Gates 是触发过的门禁（去重后按首次触发顺序）。
	Gates []GateReason
	// Models 是每一轮报告的模型名。
	Models []string
	// Duration 是墙钟耗时。
	Duration time.Duration
	// DuplicateCalls 是被 G3 拒掉的工具调用数。
	DuplicateCalls int
	// NoProgressRounds 是连续无进展的轮数（用于 G4 判定）。
	NoProgressRounds int
}

// LastTrajectory 返回最近一次 Extract 的轨迹（供测试与 doctor 观测）。
//
// 刻意做成方法而不是返回值字段：ExtractResult 是两条 LLM 路径共用的形状（§3.3），
// 不应掺入 Gmem 内部的收敛细节。
func (x *LLMExtractor) LastTrajectory() Trajectory { return x.last }

// Extract 执行一次 L2 抽取，内含多轮 tool-calling 循环。
//
// 收敛由五条门禁硬保证（G1–G5），任一条不过即**强制终稿**：
// 收走工具面，要求模型"现在必须产出"，该次调用的次数计入 Calls，
// 因此"永远返回 tool call"的模型的总调用次数恰好是 MaxTurns + 1。
func (x *LLMExtractor) Extract(ctx context.Context, req ExtractRequest) (ExtractResult, error) {
	var res ExtractResult
	if ctx == nil {
		ctx = context.Background()
	}
	if x.client == nil {
		res.Degraded = append(res.Degraded,
			"L2 skipped: 未注入 LLMClient（等价 NoopExtractor）；仅 L0+L1 可用")
		return res, nil
	}
	cfg := x.cfg
	if req.Budget.MaxTurns > 0 {
		cfg.MaxTurns = req.Budget.MaxTurns
	}
	if req.Budget.TokenBudget > 0 {
		cfg.TokenBudget = req.Budget.TokenBudget
	}
	if req.Budget.WallClock > 0 {
		cfg.WallClock = req.Budget.WallClock
	}
	if cfg.MaxTurns < 1 {
		cfg.MaxTurns = 1
	}

	start := cfg.now()
	deadline := start.Add(cfg.WallClock)
	tools := newToolRunner(x.ports, cfg, req.Episode.Scope)

	var traj Trajectory
	// transcript 累积每轮的工具结果，回注到下一轮的 User 里。
	// LLMClient 是**单轮 primitive**（没有 message 历史），因此上下文只能由 Gmem 拼装。
	var transcript []string
	seenCalls := make(map[string]struct{}, 8)
	turns := 0
	noProgress := 0
	budgetExceeded := false
	wallClockExceeded := false

	defer func() {
		traj.Duration = cfg.now().Sub(start)
		traj.NoProgressRounds = noProgress
		x.last = traj
	}()

	for {
		if err := ctx.Err(); err != nil {
			// G5 的另一半：宿主/调用方 context 已死，不再发请求，直接降级。
			res.Degraded = append(res.Degraded, fmt.Sprintf(
				"L2 中断：context 已取消（%s 后通过 %s）", traj.Duration.Round(time.Millisecond), err))
			traj.Gates = appendUnique(traj.Gates, GateWallClock)
			return res, nil
		}

		// ---- 收敛门禁（在**发请求前**判定，决定这一轮还给不给工具） ----
		var reason GateReason
		switch {
		case wallClockExceeded || cfg.now().After(deadline):
			reason = GateWallClock
		case budgetExceeded || (cfg.TokenBudget > 0 && traj.Tokens >= cfg.TokenBudget):
			reason = GateTokenBudget
		case turns >= cfg.MaxTurns:
			reason = GateMaxTurns
		case noProgress >= 2:
			reason = GateNoProgress
		}
		allowTools := reason == ""

		// 强制终稿时收走工具面：这就是"不再给工具，以'必须现在产出'重问一次"。
		callTools := []ToolSpec(nil)
		if allowTools {
			callTools = ToolSpecs()
		}
		if reason != "" {
			traj.Gates = appendUnique(traj.Gates, reason)
			traj.ForcedFinal = true
			transcript = append(transcript, "【系统】"+string(reason)+
				" 已触发：本轮工具已全部收回，你必须立即输出终稿 JSON，不要再请求任何工具。")
		}

		llmReq := LLMRequest{
			System:      x.SystemPrompt,
			User:        x.buildUserPrompt(req, transcript),
			JSONSchema:  x.ResponseSchema,
			Tools:       callTools,
			MaxTokens:   cfg.MaxTokens,
			Temperature: cfg.Temperature,
		}

		// G5：墙钟到点后只给一次极短的收尾窗口（FinalGrace），拿不到就降级返回。
		callCtx := ctx
		var cancel context.CancelFunc
		if !allowTools {
			callCtx, cancel = context.WithTimeout(ctx, cfg.FinalGrace)
		}
		resp, err := x.client.Complete(callCtx, llmReq)
		if cancel != nil {
			cancel()
		}
		traj.Calls++
		if resp.Model != "" {
			traj.Models = append(traj.Models, resp.Model)
		}
		traj.Tokens += resp.Usage.Tokens()

		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				res.Degraded = append(res.Degraded, fmt.Sprintf(
					"L2 中断：第 %d 次调用超时/取消（%v）；已产出 %d 个实体",
					traj.Calls, err, len(res.Entities)))
				traj.Gates = appendUnique(traj.Gates, GateWallClock)
				return res, nil
			}
			return res, fmt.Errorf("extract: LLM Complete 失败（第 %d 次调用）: %w", traj.Calls, err)
		}
		if cfg.TokenBudget > 0 && traj.Tokens > cfg.TokenBudget {
			budgetExceeded = true
		}

		// ---- 终稿判定 ----
		if len(resp.ToolCalls) == 0 {
			parsed, perr := x.parsePayload(resp.Text)
			if perr == nil {
				x.applyPayload(parsed, req, &res)
				return res, nil
			}
			// 解析失败：**不静默丢弃**（§3.1）。只在门禁未触发时重试 1 次，
			// 因为强制终稿那一轮不允许再花一次调用（否则总调用次数会超出 MaxTurns+1，
			// D10 的断言就不再是硬保证）。
			if !traj.ForcedFinal && turns < cfg.MaxTurns {
				transcript = append(transcript,
					"【系统】上一轮输出无法解析："+perr.Error()+"。请只输出修正后的 JSON 对象。")
				// 用一次独立的修复式重试，不计入工具轮；仍失败则降级。
				fixResp, fixErr := x.client.Complete(ctx, LLMRequest{
					System: x.SystemPrompt, User: x.buildUserPrompt(req, transcript),
					JSONSchema: x.ResponseSchema, MaxTokens: cfg.MaxTokens, Temperature: 0,
				})
				traj.Calls++
				traj.Tokens += fixResp.Usage.Tokens()
				if fixResp.Model != "" {
					traj.Models = append(traj.Models, fixResp.Model)
				}
				if fixErr == nil {
					if fixed, ferr := x.parsePayload(fixResp.Text); ferr == nil {
						res.Degraded = append(res.Degraded, "L2 终稿首次解析失败，修复式重试成功："+perr.Error())
						x.applyPayload(fixed, req, &res)
						return res, nil
					}
				}
				res.Degraded = append(res.Degraded, "L2 终稿解析失败（含修复式重试）："+perr.Error())
				return res, nil
			}
			res.Degraded = append(res.Degraded, "L2 终稿解析失败（强制终稿轮，不再重试）："+perr.Error())
			return res, nil
		}

		// ---- 强制终稿轮却仍返回工具调用：不再执行，降级 ----
		if !allowTools {
			res.Degraded = append(res.Degraded, fmt.Sprintf(
				"L2 强制终稿后模型仍返回 %d 个工具调用（循环已由 Gmem 终止，未执行）；原文交 L0/L1",
				len(resp.ToolCalls)))
			return res, nil
		}

		// ---- 执行工具（只读、有行数上限、hops 硬上限 1） ----
		executed := 0
		roundItems := 0
		for _, call := range resp.ToolCalls {
			key := normalizeCall(call)
			if _, dup := seenCalls[key]; dup {
				// G3：直接拒这一跳，不再回注（也不重复花钱）。
				traj.DuplicateCalls++
				traj.Gates = appendUnique(traj.Gates, GateDuplicate)
				transcript = append(transcript, fmt.Sprintf(
					"【工具 %s】该查询与之前完全相同，已被去重拒绝；请基于已有结果产出终稿。", call.Name))
				continue
			}
			seenCalls[key] = struct{}{}
			out, toolErr := tools.run(ctx, call)
			executed++
			if toolErr != nil {
				transcript = append(transcript, fmt.Sprintf("【工具 %s 失败】%v", call.Name, toolErr))
				continue
			}
			roundItems += len(out.Items)
			transcript = append(transcript, formatToolResult(out))
		}
		turns++
		traj.ToolRounds = turns

		// G4：连续 2 轮工具结果为空/无新增节点 → 下一轮强制终稿。
		if executed == 0 || roundItems == 0 {
			noProgress++
		} else {
			noProgress = 0
		}
		if cfg.now().After(deadline) {
			wallClockExceeded = true
		}
	}
}

// buildUserPrompt 拼装本轮 User 内容：原文 + 同簇 + 已知实体 + 工具结果。
func (x *LLMExtractor) buildUserPrompt(req ExtractRequest, transcript []string) string {
	var b strings.Builder
	b.WriteString("## 原文\n")
	b.WriteString(strings.TrimSpace(req.Episode.Content))
	if len(req.Clusters) > 0 {
		b.WriteString("\n\n## L1 的疑似同簇（候选，不是结论；请自行判断是否同一实体）\n")
		for _, c := range req.Clusters {
			b.WriteString(fmt.Sprintf("- 相似度 %.2f：%s\n", c.Similarity, strings.Join(c.Surfaces, " / ")))
		}
	}
	if len(req.Known) > 0 {
		b.WriteString("\n## 已知实体（本 scope）\n")
		limit := 40
		for i, k := range req.Known {
			if i >= limit {
				b.WriteString(fmt.Sprintf("- ……（还有 %d 个已省略）\n", len(req.Known)-limit))
				break
			}
			line := "- " + k.Name + "（node=" + k.NodeID
			if k.ConceptID != "" {
				line += ", concept=" + k.ConceptID
			}
			if len(k.Aliases) > 0 {
				line += ", aliases=" + strings.Join(k.Aliases, "/")
			}
			b.WriteString(line + "）\n")
		}
	}
	if len(transcript) > 0 {
		b.WriteString("\n## 查询轨迹与工具结果\n")
		for _, t := range transcript {
			b.WriteString(t)
			b.WriteString("\n")
		}
	}
	b.WriteString("\n## 任务\n输出符合 schema 的 JSON 终稿。")
	return b.String()
}

// parsePayload 解析并**校验**模型终稿（§3.1 责任边界第 2 条）。
func (x *LLMExtractor) parsePayload(text string) (llmPayload, error) {
	var p llmPayload
	body := stripCodeFence(text)
	if strings.TrimSpace(body) == "" {
		return p, errors.New("空输出")
	}
	// 容忍模型在 JSON 前后加解说：截取第一个 { 到最后一个 }。
	if i, j := strings.Index(body, "{"), strings.LastIndex(body, "}"); i >= 0 && j > i {
		body = body[i : j+1]
	}
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		return p, fmt.Errorf("JSON 解析失败: %w", err)
	}
	if len(p.Entities) == 0 && len(p.Relations) == 0 && len(p.Concepts) == 0 {
		return p, errors.New("JSON 合法但没有任何实体/关系/概念")
	}
	return p, nil
}

// applyPayload 把解析结果转成 ExtractResult，并做字段级校验。
func (x *LLMExtractor) applyPayload(p llmPayload, req ExtractRequest, res *ExtractResult) {
	known := make(map[string]struct{}, len(req.Known)+len(p.Entities))
	for _, k := range req.Known {
		known[strings.ToLower(strings.TrimSpace(k.Name))] = struct{}{}
	}
	for _, e := range p.Entities {
		name := strings.TrimSpace(e.Name)
		if name == "" {
			res.Degraded = append(res.Degraded, "L2 校验：丢弃一个空名实体")
			continue
		}
		conf := e.Confidence
		if conf <= 0 || conf > 1 {
			conf = 0.6 // 模型没给或给了非法值时用保守默认，而不是 1
		}
		res.Entities = append(res.Entities, ExtractedEntity{
			Name: name, Aliases: cleanAliases(e.Aliases, name), ConceptID: e.Concept,
			Definition: strings.TrimSpace(e.Definition), Confidence: conf,
		})
		known[strings.ToLower(name)] = struct{}{}
	}
	for _, r := range p.Relations {
		from := strings.TrimSpace(r.From)
		to := strings.TrimSpace(r.To)
		rel := store.Relation(strings.TrimSpace(r.Relation))
		if from == "" || to == "" || rel == "" {
			res.Degraded = append(res.Degraded, "L2 校验：丢弃一个字段不全的关系")
			continue
		}
		if !relationWhitelisted(rel) {
			res.Degraded = append(res.Degraded, fmt.Sprintf("L2 校验：丢弃白名单外的关系 %q", rel))
			continue
		}
		// 两端必须能在结果或已知实体里找到（§3.1 第 2 条）。
		if _, ok := known[strings.ToLower(from)]; !ok {
			res.Degraded = append(res.Degraded, fmt.Sprintf(
				"L2 校验：丢弃关系 %s-%s->%s（起点不在结果或已知实体中）", from, rel, to))
			continue
		}
		if _, ok := known[strings.ToLower(to)]; !ok {
			res.Degraded = append(res.Degraded, fmt.Sprintf(
				"L2 校验：丢弃关系 %s-%s->%s（终点不在结果或已知实体中）", from, rel, to))
			continue
		}
		conf := r.Confidence
		if conf <= 0 || conf > 1 {
			conf = 0.6
		}
		res.Relations = append(res.Relations, ExtractedRelation{
			From: from, To: to, Relation: rel, Confidence: conf,
		})
	}
	for _, c := range p.Concepts {
		name := strings.TrimSpace(c.Name)
		if name == "" {
			continue
		}
		id := strings.TrimSpace(c.ID)
		if id == "" {
			id = conceptIDFromName(name)
		}
		res.Concepts = append(res.Concepts, ConceptHint{
			ID: id, Name: name, Definition: strings.TrimSpace(c.Definition),
		})
	}
}

// relationWhitelist 复用 store 的关系常量；内联表而不是查询 recall 的配置，
// 因为这里校验的是"这个关系名是否属于白名单"，与跳数策略无关。
func relationWhitelisted(r store.Relation) bool {
	switch r {
	case store.RelationSubtypeOf, store.RelationPartOf, store.RelationDependsOn,
		store.RelationCausedBy, store.RelationWorksAt, store.RelationOwns, store.RelationUses:
		return true
	}
	return false
}

func cleanAliases(in []string, self string) []string {
	out := make([]string, 0, len(in))
	selfLower := strings.ToLower(strings.TrimSpace(self))
	seen := make(map[string]struct{}, len(in))
	for _, a := range in {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		key := strings.ToLower(a)
		if key == selfLower {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, a)
	}
	return out
}

func stripCodeFence(s string) string {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "```") {
		return t
	}
	// 去掉首行围栏与结尾围栏。
	if i := strings.Index(t, "\n"); i >= 0 {
		t = t[i+1:]
	}
	if j := strings.LastIndex(t, "```"); j >= 0 {
		t = t[:j]
	}
	return strings.TrimSpace(t)
}

// normalizeCall 是 G3 的归一化键：工具名 + 排序后的参数（去空白、小写）。
//
// 归一化必须**不依赖 JSON 文本的字段顺序**：宿主模型每次序列化顺序可能不同，
// 用原始 JSON 字符串比较会让"同一条查询"漏检。
func normalizeCall(c ToolCall) string {
	var key string
	if len(c.Arguments) > 0 {
		var m map[string]any
		if err := json.Unmarshal(c.Arguments, &m); err == nil {
			keys := make([]string, 0, len(m))
			for k := range m {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			parts := make([]string, 0, len(keys))
			for _, k := range keys {
				parts = append(parts, strings.ToLower(k)+"="+normalizeValue(m[k]))
			}
			key = strings.Join(parts, "&")
		} else {
			key = normalizeValue(string(c.Arguments))
		}
	}
	return strings.ToLower(strings.TrimSpace(c.Name)) + "?" + key
}

func normalizeValue(v any) string {
	switch t := v.(type) {
	case string:
		return strings.Join(strings.Fields(strings.ToLower(t)), " ")
	case float64:
		return strings.TrimSuffix(strings.TrimSuffix(fmt.Sprintf("%f", t), "0"), ".")
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return strings.ToLower(string(b))
	}
}

// formatToolResult 把工具结果转成回注上下文的一段文本。
//
// **Truncated 必须出现在回注里**：模型需要知道"你看到的不是全部"。
func formatToolResult(r toolResult) string {
	b, err := json.Marshal(r.Items)
	if err != nil {
		b = []byte("[]")
	}
	flag := ""
	if r.Truncated {
		flag = "（结果已截断：以下不是全部）"
	}
	return fmt.Sprintf("【工具 %s】共 %d 条，返回 %d 条%s：%s",
		r.Tool, r.Total, len(r.Items), flag, string(b))
}

func appendUnique(gates []GateReason, g GateReason) []GateReason {
	for _, x := range gates {
		if x == g {
			return gates
		}
	}
	return append(gates, g)
}
