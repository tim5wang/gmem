package extract

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	// mu 保护 last：同一个 LLMExtractor 被并发使用（例如一个 scope 一个 goroutine
	// 并发跑 Pipeline.Run）是正常用法，「测试是串行跑的」不是库的契约。
	mu sync.Mutex
	// last 是最近一次 Extract 的轨迹（由 LastTrajectory 暴露）。语义是「最后一次」。
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
	// RepairRetries 是修复式重试次数（它计入轮配额，见 M4）。
	RepairRetries int
}

// LastTrajectory 返回最近一次 Extract 的轨迹（供测试与 doctor 观测）。
//
// 刻意做成方法而不是返回值字段：ExtractResult 是两条 LLM 路径共用的形状（§3.3），
// 不应掺入 Gmem 内部的收敛细节。
func (x *LLMExtractor) LastTrajectory() Trajectory {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.last
}

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
	// seenResultIDs 是已见过的结果 id（node/edge/memory）。
	// G4 的"无进展"必须是"无**新增**结果"：只数 len(Items) 的话，模型换个说法
	// 反复命中同一行就永远不会被 G4 收口（M1）。
	seenResultIDs := make(map[string]struct{}, 16)
	turns := 0
	// spent 是已用掉的**轮配额**：工具轮与修复式重试都计入。
	// 它保证"总调用次数 <= MaxTurns+1"是真硬上界（M4），而不是"工具轮 + 1 + 重试"。
	spent := 0
	noProgress := 0
	budgetExceeded := false
	wallClockExceeded := false

	defer func() {
		traj.Duration = cfg.now().Sub(start)
		traj.NoProgressRounds = noProgress
		x.mu.Lock()
		x.last = traj
		x.mu.Unlock()
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
		case turns >= cfg.MaxTurns || spent >= cfg.MaxTurns:
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
		//
		// **工具轮也必须带 deadline**（B-2）：如果只在强制终稿轮设超时，
		// 宿主客户端卡在工具轮时 Gmem 没有任何中断手段，WallClock 对在飞调用
		// 完全无效（实测 20ms 配置跑了 610ms）。
		//
		// 强制终稿轮用 now+FinalGrace（而不是 deadline+FinalGrace）：那时的语义是
		// "已经决定收口，再给一次极短机会"，不应再等满整个 WallClock。
		var callCtx context.Context
		var cancel context.CancelFunc
		if allowTools {
			callCtx, cancel = context.WithDeadline(ctx, deadline)
		} else {
			callCtx, cancel = context.WithTimeout(ctx, cfg.FinalGrace)
		}
		resp, err := x.client.Complete(callCtx, llmReq)
		cancel()
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
			// 且重试**占用轮配额**（spent++）：否则总调用次数会变成 MaxTurns+2，
			// D10 的"总调用 == MaxTurns+1"就不再是硬上界（M4）。
			if !traj.ForcedFinal && spent < cfg.MaxTurns {
				transcript = append(transcript,
					"【系统】上一轮输出无法解析："+perr.Error()+"。请只输出修正后的 JSON 对象。")
				spent++
				traj.RepairRetries++
				fixCtx, fixCancel := context.WithDeadline(ctx, deadline)
				fixResp, fixErr := x.client.Complete(fixCtx, LLMRequest{
					System: x.SystemPrompt, User: x.buildUserPrompt(req, transcript),
					JSONSchema: x.ResponseSchema, MaxTokens: cfg.MaxTokens, Temperature: 0,
				})
				fixCancel()
				traj.Calls++
				traj.Tokens += fixResp.Usage.Tokens()
				if fixResp.Model != "" {
					traj.Models = append(traj.Models, fixResp.Model)
				}
				if fixErr != nil {
					if errors.Is(fixErr, context.DeadlineExceeded) || errors.Is(fixErr, context.Canceled) {
						res.Degraded = append(res.Degraded, fmt.Sprintf(
							"L2 中断：修复式重试超时/取消（%v）", fixErr))
						traj.Gates = appendUnique(traj.Gates, GateWallClock)
						return res, nil
					}
				} else if fixed, ferr := x.parsePayload(fixResp.Text); ferr == nil {
					res.Degraded = append(res.Degraded, "L2 终稿首次解析失败，修复式重试成功："+perr.Error())
					x.applyPayload(fixed, req, &res)
					return res, nil
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
		// rejected 是本轮被 G3 拒掉的跳数；newIDs 是本轮首次出现的**结果 id**数。
		rejected := 0
		newIDs := 0
		for _, call := range resp.ToolCalls {
			key := normalizeCall(call)
			if _, dup := seenCalls[key]; dup {
				// G3：直接拒这一跳，不再回注（也不重复花钱）。
				traj.DuplicateCalls++
				rejected++
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
			for _, id := range out.resultIDs() {
				if _, ok := seenResultIDs[id]; ok {
					continue
				}
				seenResultIDs[id] = struct{}{}
				newIDs++
			}
			transcript = append(transcript, formatToolResult(out))
		}
		turns++
		spent++
		traj.ToolRounds = turns

		// G4：连续 2 轮"无进展" → 下一轮强制终稿。
		//
		// "无进展"的口径是**本轮没有带来首次出现的 node/edge/memory id**（M1）。
		// 旧口径只数 len(Items)，于是模型把同一条查询换个措辞（vim / vi / v）
		// 反复命中同一行时 noProgress 永远归零、G4 永不触发。
		//
		// 但**全部跳都被 G3 拒掉的一轮不算无进展**（B-3）：那一轮模型根本没拿到
		// 新证据，原因是我们自己拒绝执行，不是模型停滞。若把它计入，MaxTurns>=4 时
		// 每一跳都被拒 → G4 在第 3 轮抢先收口 → D10 的 Calls==MaxTurns+1 在 4/5/6
		// 上变成 4（这就是本次要修的阻塞）。
		switch {
		case executed == 0 && rejected > 0:
			// G3 拒跳不叠加到 G4 上（也不重置：它既不是进展也不是停滞）。
		case newIDs == 0:
			noProgress++
		default:
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

// normalizeCall 是 G3 的归一化键：工具名 + **该工具声明过的有效参数**（去空白、小写）。
//
// 归一化必须**不依赖 JSON 文本的字段顺序**：宿主模型每次序列化顺序可能不同，
// 用原始 JSON 字符串比较会让"同一条查询"漏检。
//
// 同时不能把**空值参数**算进键（M2）：模型只要每轮多塞一个 `{"concept":""}`
// 就能把同一条语义查询变成"新查询"，G3 就形同虚设。因此按工具声明逐个规约：
// 空/缺省参数直接剔除，数值参数先夹到**真正生效**的值（hops<=1、k<=SearchLimit）。
func normalizeCall(c ToolCall) string {
	name := strings.ToLower(strings.TrimSpace(c.Name))
	if len(c.Arguments) == 0 {
		return name + "?"
	}
	var m map[string]any
	if err := json.Unmarshal(c.Arguments, &m); err != nil {
		return name + "?" + normalizeValue(string(c.Arguments))
	}

	// effective 是该工具真正生效的参数集合；nil 表示未知工具（回退到全参数）。
	var effective map[string]string
	switch name {
	case ToolLookupEntity:
		effective = map[string]string{"name": normalizeValue(m["name"])}
		if v := normalizeValue(m["concept"]); v != "" {
			effective["concept"] = v
		}
	case ToolNeighbors:
		effective = map[string]string{"node_id": normalizeValue(m["node_id"])}
		if v := normalizeValue(m["relation"]); v != "" {
			effective["relation"] = v
		}
		// 无论模型给 0/1/3，工具侧都会夹到 HardMaxHops，语义等价。
		effective["hops"] = strconv.Itoa(HardMaxHops)
	case ToolSearchMemories:
		effective = map[string]string{"query": normalizeValue(m["query"])}
		// k 夹取后比较：k 缺省、k=0、k>上限 在工具侧得到同一个生效值。
		k := defaultSearchLimit
		if f, ok := m["k"].(float64); ok {
			if n := int(f); n > 0 && n < k {
				k = n
			}
		}
		effective["k"] = strconv.Itoa(k)
	}

	keys := make([]string, 0, len(m))
	parts := make([]string, 0, len(m))
	if effective != nil {
		for k, v := range effective {
			if strings.TrimSpace(v) == "" {
				continue // 空参不进键（M2）
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			parts = append(parts, k+"="+effective[k])
		}
		return name + "?" + strings.Join(parts, "&")
	}

	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := normalizeValue(m[k])
		if v == "" {
			continue // 未知工具也剔除空参：空值不可能改变查询语义
		}
		parts = append(parts, strings.ToLower(k)+"="+v)
	}
	return name + "?" + strings.Join(parts, "&")
}

func normalizeValue(v any) string {
	switch t := v.(type) {
	case string:
		return strings.Join(strings.Fields(strings.ToLower(t)), " ")
	case float64:
		// -1 精度直接输出最短表示：旧写法只剥一个尾零，1.10 与 1.1 会被判成不同。
		return strconv.FormatFloat(t, 'f', -1, 64)
	case nil:
		return ""
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
