package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/tim5wang/gmem/llm"
	"github.com/tim5wang/gmem/memory"
	"github.com/tim5wang/gmem/ops"
	"github.com/tim5wang/gmem/recall"
	"github.com/tim5wang/gmem/server"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// 工具面（docs/TD.md §10 的 MCP 工具表）
//
// 三层，全部是**薄壳**：
//
//	主面（11）  gmem_remember / recall / get / list / forget / history /
//	            consolidate / review_similar / merge / link / supersede
//	图专用（2） gmem_graph / gmem_review
//	别名（6）   create_entities / create_relations / add_observations /
//	            search_nodes / open_nodes / read_graph
//
// 主面与图专用直接调 memory.Service 与 server.Server；别名层映射到同一批方法。
// 没有任何一个工具自己拼 SQL 或重算图——那会让"HTTP 是对的、MCP 是另一套"。
// ===========================================================================

// aliasDisclaimer 是**必须**出现在工具响应与 doctor 输出里的一句话。
//
// 为什么写死在这里而不是只放在 README：使用者读 README 的概率远低于
// 读工具返回值。别名层最容易被误解成"与官方 server 数据互通"，
// 而那个误解会直接导致数据丢失（以为重新指过去就能读到旧数据）。
const aliasDisclaimer = "兼容别名层只做**能力映射**，不承诺与 MCP 官方 memory server 的 JSON 存储格式互通" +
	"（同名概念在两边落在不同表形态里，不能靠「读那边写的文件」迁移数据）"

// toolSpec 是一个工具的声明。
type toolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// handler 是工具的执行函数：入参是原始 params.arguments，返回结果或**参数错误**。
type handler func(ctx context.Context, args json.RawMessage) (any, *RPCError)

// toolList 返回全部工具（顺序固定，便于 tools/list 的回归比对）。
func (s *Server) toolList() []toolSpec {
	return []toolSpec{
		{
			Name: "gmem_remember",
			Description: "记住一段原文（对齐 Mem0 的 add）。不给 kind 时走整理管线（L0 规则 + 消歧 + 成图）；" +
				"给出 kind 时按结构化写入落单条记忆。返回实际落库的记忆与全部降级/告警。",
			InputSchema: objSchema(map[string]any{
				"content":   strProp("原文（必填）"),
				"scope":     enumProp("归属边界", "user", "agent", "project", "global"),
				"kind":      enumProp("记忆类型（省略则走抽取管线）", "semantic", "episodic", "procedural", "decision"),
				"namespace": strProp("命名空间（可选）"),
				"role":      strProp("说话人角色（可选）"),
				"at":        intProp("世界时间 unix 秒（可选，默认现在）"),
			}, "content"),
		},
		{
			Name: "gmem_recall",
			Description: "召回相关记忆（对齐 Mem0 的 search）。始终返回可解释的 plan（R5）；" +
				"覆盖率不足时返回 status=insufficient 与缺失槽位，而不是三条低分硬答。",
			InputSchema: objSchema(map[string]any{
				"query":   strProp("查询串（必填）"),
				"scope":   enumProp("限定边界", "user", "agent", "project", "global"),
				"k":       intProp("返回条数"),
				"kinds":   arrProp("限定记忆类型", "semantic", "episodic", "procedural", "decision"),
				"as_of":   intProp("双时态回溯（unix 秒）"),
				"explain": boolProp("是否返回完整 plan（默认 true）"),
			}, "query"),
		},
		{
			Name:        "gmem_get",
			Description: "按 ID 取一条记忆的当前版本。",
			InputSchema: objSchema(map[string]any{"id": strProp("记忆 ID（必填）")}, "id"),
		},
		{
			Name:        "gmem_list",
			Description: "枚举记忆（按世界时间倒序）。",
			InputSchema: objSchema(map[string]any{
				"scope":           enumProp("限定边界", "user", "agent", "project", "global"),
				"kind":            enumProp("限定单类记忆类型", "semantic", "episodic", "procedural", "decision"),
				"limit":           intProp("返回上限"),
				"include_invalid": boolProp("是否连已失效的事实一起返回"),
			}),
		},
		{
			Name: "gmem_forget",
			Description: "让记忆失效（对齐 Mem0 的 delete）。语义是**只置 t_invalid、不物理删除**，" +
				"因此 as_of 仍能回溯到它。必须显式传 confirm=true。",
			InputSchema: objSchema(map[string]any{
				"id":      strProp("记忆 ID（与 scope 二选一）"),
				"scope":   enumProp("按边界批量失效（与 id 二选一）", "user", "agent", "project", "global"),
				"reason":  strProp("原因（写入审计）"),
				"confirm": boolProp("必须为 true（二次确认）"),
			}),
		},
		{
			Name:        "gmem_history",
			Description: "取一条记忆的版本链（before/after）。",
			InputSchema: objSchema(map[string]any{"id": strProp("记忆 ID（必填）")}, "id"),
		},
		{
			Name:        "gmem_consolidate",
			Description: "整理一个 scope：推进生命周期状态机、报告孤立节点。与自动触发共用同一套 op。",
			InputSchema: objSchema(map[string]any{
				"scope": enumProp("边界（必填）", "user", "agent", "project", "global"),
				"limit": intProp("本次处理的条数上限"),
			}, "scope"),
		},
		{
			Name:        "gmem_review_similar",
			Description: "列疑似重复的实体对（只读，不写库）。灰区**绝不自动合并**。",
			InputSchema: objSchema(map[string]any{
				"scope": enumProp("限定边界", "user", "agent", "project", "global"),
				"limit": intProp("上限"),
			}),
		},
		{
			Name:        "gmem_merge",
			Description: "把 from 并入 to（旧 ID 保留为重定向，可回溯）。误合并最难恢复，调用前请先用 gmem_review 核对。",
			InputSchema: objSchema(map[string]any{
				"from":   strProp("被并入的节点 ID（必填）"),
				"to":     strProp("保留的节点 ID（必填）"),
				"scope":  enumProp("边界", "user", "agent", "project", "global"),
				"reason": strProp("原因"),
			}, "from", "to"),
		},
		{
			Name:        "gmem_link",
			Description: "写入或失效一条边（remove=true 等价于 unlink）。",
			InputSchema: objSchema(map[string]any{
				"from":     strProp("起点节点 ID（必填）"),
				"to":       strProp("终点节点 ID（必填）"),
				"relation": strProp("关系（必填，例：works_at / part_of / uses）"),
				"weight":   numProp("权重"),
				"remove":   boolProp("true = 失效这条边"),
				"scope":    enumProp("边界", "user", "agent", "project", "global"),
				"reason":   strProp("原因"),
			}, "from", "to", "relation"),
		},
		{
			Name:        "gmem_supersede",
			Description: "宣布一条记忆作废（只置 t_invalid，不删除）。",
			InputSchema: objSchema(map[string]any{
				"id":        strProp("记忆 ID（必填）"),
				"scope":     enumProp("边界", "user", "agent", "project", "global"),
				"t_invalid": intProp("失效的世界时间（unix 秒，默认现在）"),
				"reason":    strProp("原因"),
			}, "id"),
		},
		{
			Name: "gmem_graph",
			Description: "取邻域子图（与 GET /api/graph 同一套聚合：跳数裁剪 / 预算折叠 / 已失效边可见）。" +
				"focus 与 q 二选一，都不给则返回概览。",
			InputSchema: objSchema(map[string]any{
				"focus":   strProp("中心节点 ID"),
				"q":       strProp("关键词（走 Recall 取命中实体作种子）"),
				"concept": strProp("只保留该概念下的节点"),
				"hops":    intProp("跳数（默认 1，受关系传递性表裁剪）"),
				"as_of":   intProp("双时态视角（unix 秒）"),
				"scope":   enumProp("边界", "user", "agent", "project", "global"),
				"budget":  intProp("节点预算"),
				"k":       intProp("q 模式的种子条数"),
			}),
		},
		{
			Name: "gmem_review",
			Description: "待审池。不带 edge_id 时列出全部待审 possiblySame 边；" +
				"带 edge_id + verdict 时执行裁决（same→Merge，diff→写 negative_SameAs），必须 confirmed=true。" +
				"已失效的边不可再裁（守卫在服务端，MCP 无法绕过）。",
			InputSchema: objSchema(map[string]any{
				"scope":     enumProp("限定边界", "user", "agent", "project", "global"),
				"edge_id":   strProp("待审边 ID（给出则执行裁决）"),
				"verdict":   enumProp("裁决（same / diff）", "same", "diff"),
				"confirmed": boolProp("必须为 true（二次确认）"),
				"note":      strProp("裁决备注"),
			}),
		},

		// ---- 兼容别名层（能力映射，格式不互通） ----
		{
			Name: "create_entities",
			Description: "【兼容别名】创建/更新实体（name + entityType + observations）。" +
				"映射为：节点 + 别名 + draft 概念 + semantic 记忆。" + aliasDisclaimer,
			InputSchema: objSchema(map[string]any{
				"entities": map[string]any{
					"type": "array", "description": "实体列表",
					"items": objSchema(map[string]any{
						"name":         strProp("实体名"),
						"entityType":   strProp("实体类型（映射为 draft 概念）"),
						"observations": arrProp("观测片段（每条落一条 semantic 记忆）"),
					}, "name"),
				},
				"scope": enumProp("边界", "user", "agent", "project", "global"),
			}, "entities"),
		},
		{
			Name:        "create_relations",
			Description: "【兼容别名】创建实体关系（from / to / relationType）。映射为 gmem_link。" + aliasDisclaimer,
			InputSchema: objSchema(map[string]any{
				"relations": map[string]any{
					"type": "array", "description": "关系列表",
					"items": objSchema(map[string]any{
						"from":         strProp("起点实体名"),
						"to":           strProp("终点实体名"),
						"relationType": strProp("关系类型"),
					}, "from", "to", "relationType"),
				},
				"scope": enumProp("边界", "user", "agent", "project", "global"),
			}, "relations"),
		},
		{
			Name:        "add_observations",
			Description: "【兼容别名】给既有实体追加观测。映射为「按名解析节点 → 写 semantic 记忆」。" + aliasDisclaimer,
			InputSchema: objSchema(map[string]any{
				"observations": map[string]any{
					"type": "array", "description": "观测列表",
					"items": objSchema(map[string]any{
						"entityName": strProp("实体名"),
						"contents":   arrProp("新增观测片段"),
					}, "entityName", "contents"),
				},
				"scope": enumProp("边界", "user", "agent", "project", "global"),
			}, "observations"),
		},
		{
			Name:        "search_nodes",
			Description: "【兼容别名】按关键词找实体。映射为 Recall 的实体锚点 + 实体卡。" + aliasDisclaimer,
			InputSchema: objSchema(map[string]any{
				"query": strProp("查询串（必填）"),
				"scope": enumProp("边界", "user", "agent", "project", "global"),
				"k":     intProp("命中实体数上限"),
			}, "query"),
		},
		{
			Name:        "open_nodes",
			Description: "【兼容别名】按名字打开实体。映射为「别名词典解析 → 实体卡」。" + aliasDisclaimer,
			InputSchema: objSchema(map[string]any{
				"names": arrProp("实体名列表（必填）"),
				"scope": enumProp("边界", "user", "agent", "project", "global"),
			}, "names"),
		},
		{
			Name:        "read_graph",
			Description: "【兼容别名】读取整张图。映射为 gmem_graph 的概览模式（受同一套预算折叠）。" + aliasDisclaimer,
			InputSchema: objSchema(map[string]any{
				"scope":  enumProp("边界", "user", "agent", "project", "global"),
				"budget": intProp("节点预算"),
			}),
		},
	}
}

// handleToolCall 执行一次 tools/call。
func (s *Server) handleToolCall(ctx context.Context, req Request) Response {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if len(req.Params) == 0 {
		return newError(req.ID, invalidParams("tools/call 需要 params.name"))
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return newError(req.ID, invalidParams("tools/call 参数非法: %v", err))
	}
	if params.Name == "" {
		return newError(req.ID, invalidParams("tools/call 缺少 name"))
	}
	h, ok := s.handlers()[params.Name]
	if !ok {
		return newError(req.ID, methodNotFound("工具 "+params.Name))
	}
	// arguments 允许缺省（等价于 {}）：部分客户端对无参工具直接省略。
	args := params.Arguments
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage(`{}`)
	}
	if !json.Valid(args) {
		return newError(req.ID, invalidParams("arguments 不是合法 JSON"))
	}
	result, rpcErr := h(ctx, args)
	if rpcErr != nil {
		// 参数校验失败 → JSON-RPC error（不是 panic，也不是 isError 结果）。
		return newError(req.ID, rpcErr)
	}
	return newResult(req.ID, result)
}

// handlers 把工具名映射到实现。
func (s *Server) handlers() map[string]handler {
	return map[string]handler{
		"gmem_remember":       s.toolRemember,
		"gmem_recall":         s.toolRecall,
		"gmem_get":            s.toolGet,
		"gmem_list":           s.toolListMemories,
		"gmem_forget":         s.toolForget,
		"gmem_history":        s.toolHistory,
		"gmem_consolidate":    s.toolConsolidate,
		"gmem_review_similar": s.toolReviewSimilar,
		"gmem_merge":          s.toolMerge,
		"gmem_link":           s.toolLink,
		"gmem_supersede":      s.toolSupersede,
		"gmem_graph":          s.toolGraph,
		"gmem_review":         s.toolReview,

		"create_entities":  s.toolCreateEntities,
		"create_relations": s.toolCreateRelations,
		"add_observations": s.toolAddObservations,
		"search_nodes":     s.toolSearchNodes,
		"open_nodes":       s.toolOpenNodes,
		"read_graph":       s.toolReadGraph,
	}
}

// ---------------------------------------------------------------------------
// 主面
// ---------------------------------------------------------------------------

func (s *Server) toolRemember(ctx context.Context, args json.RawMessage) (any, *RPCError) {
	var a struct {
		Content   string `json:"content"`
		Scope     string `json:"scope"`
		Kind      string `json:"kind"`
		Namespace string `json:"namespace"`
		Role      string `json:"role"`
		At        int64  `json:"at"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, invalidParams("gmem_remember: %v", err)
	}
	if strings.TrimSpace(a.Content) == "" {
		return nil, invalidParams("gmem_remember: content 不能为空")
	}
	scope, rpcErr := s.scopeOr(a.Scope)
	if rpcErr != nil {
		return nil, rpcErr
	}
	kind := store.MemoryKind(a.Kind)
	if a.Kind != "" && !kind.Valid() {
		return nil, invalidParams("gmem_remember: kind 必须是 semantic / episodic / procedural / decision 之一，收到 %q", a.Kind)
	}
	res, err := s.svc.Remember(ctx, memory.RememberRequest{
		Content: a.Content, Scope: scope, Kind: kind,
		Namespace: a.Namespace, Role: a.Role, At: a.At, Actor: "mcp",
	})
	if err != nil {
		return errorResult(err), nil
	}
	// 降级必须在**工具响应**里可见（D8 的"不得静默"）。
	degraded := append([]string{}, res.Degraded...)
	degraded = append(degraded, s.strategy().Degraded...)
	return textResult(map[string]any{
		"episode_id": res.EpisodeID,
		"writes":     res.Writes,
		"reviewable": res.Reviewable,
		"degraded":   degraded,
		"warnings":   res.Warnings,
		"rejected":   res.Rejected,
		"llm_mode":   string(s.strategy().Mode),
	}), nil
}

func (s *Server) toolRecall(ctx context.Context, args json.RawMessage) (any, *RPCError) {
	var a struct {
		Query   string   `json:"query"`
		Scope   string   `json:"scope"`
		K       int      `json:"k"`
		Kinds   []string `json:"kinds"`
		AsOf    *int64   `json:"as_of"`
		Explain *bool    `json:"explain"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, invalidParams("gmem_recall: %v", err)
	}
	if strings.TrimSpace(a.Query) == "" {
		return nil, invalidParams("gmem_recall: query 不能为空")
	}
	scope, rpcErr := s.scopeOptional(a.Scope)
	if rpcErr != nil {
		return nil, rpcErr
	}
	kinds, rpcErr := parseKinds(a.Kinds)
	if rpcErr != nil {
		return nil, rpcErr
	}
	res, err := s.svc.Recall(ctx, memory.RecallRequest{
		Query: a.Query, Scope: scope, Kinds: kinds, AsOf: a.AsOf, K: a.K, Actor: "mcp",
	})
	if err != nil {
		return errorResult(err), nil
	}
	out := map[string]any{
		"status":   string(res.Status),
		"coverage": res.Coverage,
		"items":    recallItems(res),
		"missing":  res.Missing,
		"degraded": append(append([]string{}, res.Degraded...), s.strategy().Degraded...),
	}
	// explain 默认 true：R5 要求每次召回可解释，把完整 plan 交出去才谈得上"可复现"。
	if a.Explain == nil || *a.Explain {
		out["plan"] = res.Plan
	}
	return textResult(out), nil
}

func (s *Server) toolGet(ctx context.Context, args json.RawMessage) (any, *RPCError) {
	var a struct {
		ID string `json:"id"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, invalidParams("gmem_get: %v", err)
	}
	if strings.TrimSpace(a.ID) == "" {
		return nil, invalidParams("gmem_get: id 不能为空")
	}
	m, err := s.svc.Get(ctx, a.ID)
	if err != nil {
		return errorResult(err), nil
	}
	return textResult(memoryView(m)), nil
}

func (s *Server) toolListMemories(ctx context.Context, args json.RawMessage) (any, *RPCError) {
	var a struct {
		Scope          string `json:"scope"`
		Kind           string `json:"kind"`
		Limit          int    `json:"limit"`
		IncludeInvalid bool   `json:"include_invalid"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, invalidParams("gmem_list: %v", err)
	}
	scope, rpcErr := s.scopeOptional(a.Scope)
	if rpcErr != nil {
		return nil, rpcErr
	}
	var kinds []store.MemoryKind
	if a.Kind != "" {
		k := store.MemoryKind(a.Kind)
		if !k.Valid() {
			return nil, invalidParams("gmem_list: kind 非法 %q", a.Kind)
		}
		kinds = []store.MemoryKind{k}
	}
	mems, err := s.svc.List(ctx, memory.ListRequest{
		Scope: scope, Kinds: kinds, Limit: a.Limit, IncludeInvalid: a.IncludeInvalid,
	})
	if err != nil {
		return errorResult(err), nil
	}
	views := make([]any, 0, len(mems))
	for _, m := range mems {
		views = append(views, memoryView(m))
	}
	return textResult(map[string]any{"count": len(views), "memories": views}), nil
}

func (s *Server) toolForget(ctx context.Context, args json.RawMessage) (any, *RPCError) {
	var a struct {
		ID      string `json:"id"`
		Scope   string `json:"scope"`
		Reason  string `json:"reason"`
		Confirm bool   `json:"confirm"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, invalidParams("gmem_forget: %v", err)
	}
	if !a.Confirm {
		// 二次确认属于**参数层面**的错误：请求不被执行，调用方必须改参数重发。
		return nil, invalidParams("gmem_forget: 必须显式传 confirm=true（删除是记忆系统里最难恢复的操作）")
	}
	var scope store.Scope
	if a.Scope != "" {
		sc, rpcErr := s.scopeOptional(a.Scope)
		if rpcErr != nil {
			return nil, rpcErr
		}
		scope = sc
	}
	res, err := s.svc.Forget(ctx, memory.ForgetRequest{
		ID: a.ID, Scope: scope, Reason: a.Reason, Actor: "mcp", Confirmed: true,
	})
	if err != nil {
		return errorResult(err), nil
	}
	return textResult(map[string]any{
		"invalidated": res.Invalidated, "count": len(res.Invalidated),
		"degraded": res.Degraded, "warnings": res.Warnings,
	}), nil
}

func (s *Server) toolHistory(ctx context.Context, args json.RawMessage) (any, *RPCError) {
	var a struct {
		ID string `json:"id"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, invalidParams("gmem_history: %v", err)
	}
	if strings.TrimSpace(a.ID) == "" {
		return nil, invalidParams("gmem_history: id 不能为空")
	}
	res, err := s.svc.History(ctx, a.ID)
	if err != nil {
		return errorResult(err), nil
	}
	return textResult(res), nil
}

func (s *Server) toolConsolidate(ctx context.Context, args json.RawMessage) (any, *RPCError) {
	var a struct {
		Scope string `json:"scope"`
		Limit int    `json:"limit"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, invalidParams("gmem_consolidate: %v", err)
	}
	scope, rpcErr := s.scopeOr(a.Scope)
	if rpcErr != nil {
		return nil, rpcErr
	}
	res, err := s.svc.Consolidate(ctx, scope, a.Limit, "mcp")
	if err != nil {
		return errorResult(err), nil
	}
	return textResult(opsView(res)), nil
}

func (s *Server) toolReviewSimilar(ctx context.Context, args json.RawMessage) (any, *RPCError) {
	var a struct {
		Scope string `json:"scope"`
		Limit int    `json:"limit"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, invalidParams("gmem_review_similar: %v", err)
	}
	scope, rpcErr := s.scopeOptional(a.Scope)
	if rpcErr != nil {
		return nil, rpcErr
	}
	res, err := s.svc.ReviewSimilar(ctx, scope, a.Limit, "mcp")
	if err != nil {
		return errorResult(err), nil
	}
	return textResult(map[string]any{
		"pairs": res.Similar, "status": string(res.Result.Status),
		"steps": stepSummaries(res.Result), "degraded": res.Result.Degraded,
		"warnings": res.Result.Warnings,
	}), nil
}

func (s *Server) toolMerge(ctx context.Context, args json.RawMessage) (any, *RPCError) {
	var a struct {
		From   string `json:"from"`
		To     string `json:"to"`
		Scope  string `json:"scope"`
		Reason string `json:"reason"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, invalidParams("gmem_merge: %v", err)
	}
	if a.From == "" || a.To == "" {
		return nil, invalidParams("gmem_merge: from 与 to 都必填")
	}
	scope, rpcErr := s.scopeOptional(a.Scope)
	if rpcErr != nil {
		return nil, rpcErr
	}
	res, err := s.svc.Merge(ctx, scope, a.From, a.To, a.Reason, "mcp")
	if err != nil {
		return errorResult(err), nil
	}
	return textResult(opsView(res)), nil
}

func (s *Server) toolLink(ctx context.Context, args json.RawMessage) (any, *RPCError) {
	var a struct {
		From     string  `json:"from"`
		To       string  `json:"to"`
		Relation string  `json:"relation"`
		Weight   float64 `json:"weight"`
		Remove   bool    `json:"remove"`
		Scope    string  `json:"scope"`
		Reason   string  `json:"reason"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, invalidParams("gmem_link: %v", err)
	}
	if a.From == "" || a.To == "" || a.Relation == "" {
		return nil, invalidParams("gmem_link: from / to / relation 都必填")
	}
	scope, rpcErr := s.scopeOptional(a.Scope)
	if rpcErr != nil {
		return nil, rpcErr
	}
	res, err := s.svc.Link(ctx, scope, a.From, a.To, store.Relation(a.Relation),
		a.Weight, a.Remove, a.Reason, "mcp")
	if err != nil {
		return errorResult(err), nil
	}
	return textResult(opsView(res)), nil
}

func (s *Server) toolSupersede(ctx context.Context, args json.RawMessage) (any, *RPCError) {
	var a struct {
		ID       string `json:"id"`
		Scope    string `json:"scope"`
		TInvalid int64  `json:"t_invalid"`
		Reason   string `json:"reason"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, invalidParams("gmem_supersede: %v", err)
	}
	if a.ID == "" {
		return nil, invalidParams("gmem_supersede: id 必填")
	}
	scope, rpcErr := s.scopeOptional(a.Scope)
	if rpcErr != nil {
		return nil, rpcErr
	}
	// scope 为空时从记忆自身推导（与 forget 同口径，避免"记不清 scope 就不能作废"）。
	if scope == "" {
		m, err := s.svc.Get(ctx, a.ID)
		if err != nil {
			return errorResult(err), nil
		}
		scope = m.Scope
	}
	res, err := s.svc.Supersede(ctx, scope, a.ID, a.TInvalid, a.Reason, "mcp")
	if err != nil {
		return errorResult(err), nil
	}
	return textResult(opsView(res)), nil
}

// ---------------------------------------------------------------------------
// 图专用
// ---------------------------------------------------------------------------

func (s *Server) toolGraph(ctx context.Context, args json.RawMessage) (any, *RPCError) {
	var a struct {
		Focus   string `json:"focus"`
		Q       string `json:"q"`
		Concept string `json:"concept"`
		Hops    int    `json:"hops"`
		AsOf    *int64 `json:"as_of"`
		Scope   string `json:"scope"`
		Budget  int    `json:"budget"`
		K       int    `json:"k"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, invalidParams("gmem_graph: %v", err)
	}
	if a.Focus != "" && a.Q != "" {
		return nil, invalidParams("gmem_graph: focus 与 q 只能二选一")
	}
	scope, rpcErr := s.scopeOptional(a.Scope)
	if rpcErr != nil {
		return nil, rpcErr
	}
	resp, err := s.graph.GraphView(ctx, server.GraphQuery{
		Focus: a.Focus, Query: a.Q, Concept: a.Concept, Scope: scope,
		Hops: a.Hops, AsOf: a.AsOf, Budget: a.Budget, K: a.K,
	})
	if err != nil {
		return apiErrorToResult("gmem_graph", err), nil
	}
	return textResult(resp), nil
}

func (s *Server) toolReview(ctx context.Context, args json.RawMessage) (any, *RPCError) {
	var a struct {
		Scope     string `json:"scope"`
		EdgeID    string `json:"edge_id"`
		Verdict   string `json:"verdict"`
		Confirmed bool   `json:"confirmed"`
		Note      string `json:"note"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, invalidParams("gmem_review: %v", err)
	}
	scope, rpcErr := s.scopeOptional(a.Scope)
	if rpcErr != nil {
		return nil, rpcErr
	}
	// 不带 edge_id：列待审池。
	if a.EdgeID == "" {
		pending, err := s.graph.PendingReviews(ctx, scope)
		if err != nil {
			return apiErrorToResult("gmem_review", err), nil
		}
		return textResult(map[string]any{
			"pending": pending, "count": len(pending),
			"hint": "用 gmem_review 带 edge_id + verdict(same|diff) + confirmed=true 裁决",
		}), nil
	}
	// 带 edge_id：执行裁决。确认与取值校验都在这里（与 HTTP 同一套口径）。
	if !a.Confirmed {
		return nil, invalidParams("gmem_review: 执行裁决必须传 confirmed=true（TD §9 二次确认）")
	}
	if a.Verdict != "same" && a.Verdict != "diff" {
		return nil, invalidParams(`gmem_review: verdict 只接受 "same" 或 "diff"，收到 %q`, a.Verdict)
	}
	res, err := s.graph.Review(ctx, server.ReviewRequest{
		EdgeID: a.EdgeID, Verdict: a.Verdict, Confirmed: true,
		Scope: string(scope), Note: a.Note,
	})
	if err != nil {
		// 关键：走 server.Review → applyReview，因此"已失效边拒裁"的守卫无法被绕过。
		return apiErrorToResult("gmem_review", err), nil
	}
	return textResult(res), nil
}

// ---------------------------------------------------------------------------
// 兼容别名层
// ---------------------------------------------------------------------------

func (s *Server) toolCreateEntities(ctx context.Context, args json.RawMessage) (any, *RPCError) {
	var a struct {
		Entities []struct {
			Name         string   `json:"name"`
			EntityType   string   `json:"entityType"`
			Observations []string `json:"observations"`
		} `json:"entities"`
		Scope string `json:"scope"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, invalidParams("create_entities: %v", err)
	}
	if len(a.Entities) == 0 {
		return nil, invalidParams("create_entities: entities 不能为空")
	}
	scope, rpcErr := s.scopeOr(a.Scope)
	if rpcErr != nil {
		return nil, rpcErr
	}
	refs := make([]memory.EntityRef, 0, len(a.Entities))
	for _, e := range a.Entities {
		refs = append(refs, memory.EntityRef{
			Name: e.Name, Type: e.EntityType, Observations: e.Observations,
		})
	}
	res, err := s.svc.UpsertEntities(ctx, scope, refs)
	if err != nil {
		return errorResult(err), nil
	}
	return textResult(map[string]any{
		"entities": res.Entities, "degraded": res.Degraded,
		"note": aliasDisclaimer,
	}), nil
}

func (s *Server) toolCreateRelations(ctx context.Context, args json.RawMessage) (any, *RPCError) {
	var a struct {
		Relations []struct {
			From         string `json:"from"`
			To           string `json:"to"`
			RelationType string `json:"relationType"`
		} `json:"relations"`
		Scope string `json:"scope"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, invalidParams("create_relations: %v", err)
	}
	if len(a.Relations) == 0 {
		return nil, invalidParams("create_relations: relations 不能为空")
	}
	scope, rpcErr := s.scopeOr(a.Scope)
	if rpcErr != nil {
		return nil, rpcErr
	}
	// 端点解析：官方 server 用名字，这里必须先过别名词典换成节点 ID。
	names := make([]string, 0, len(a.Relations)*2)
	for _, r := range a.Relations {
		names = append(names, r.From, r.To)
	}
	resolved, missing, degraded, err := s.svc.ResolveNames(ctx, names)
	if err != nil {
		return errorResult(err), nil
	}
	var created []map[string]any
	var failed []map[string]any
	for _, r := range a.Relations {
		fromID, okF := resolved[r.From]
		toID, okT := resolved[r.To]
		if !okF || !okT {
			failed = append(failed, map[string]any{
				"from": r.From, "to": r.To, "relation": r.RelationType,
				"error": "端点未解析到实体（先用 create_entities 创建，或检查名字）",
			})
			continue
		}
		res, err := s.svc.Link(ctx, scope, fromID, toID, store.Relation(r.RelationType), 1, false,
			"mcp:create_relations", "mcp")
		if err != nil {
			failed = append(failed, map[string]any{
				"from": r.From, "to": r.To, "error": err.Error(),
			})
			continue
		}
		created = append(created, map[string]any{
			"from": r.From, "to": r.To, "from_id": fromID, "to_id": toID,
			"relation": r.RelationType, "status": string(res.Status),
		})
	}
	return textResult(map[string]any{
		"created": created, "failed": failed, "unresolved_names": missing,
		"degraded": degraded, "note": aliasDisclaimer,
	}), nil
}

func (s *Server) toolAddObservations(ctx context.Context, args json.RawMessage) (any, *RPCError) {
	var a struct {
		Observations []struct {
			EntityName string   `json:"entityName"`
			Contents   []string `json:"contents"`
		} `json:"observations"`
		Scope string `json:"scope"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, invalidParams("add_observations: %v", err)
	}
	if len(a.Observations) == 0 {
		return nil, invalidParams("add_observations: observations 不能为空")
	}
	scope, rpcErr := s.scopeOr(a.Scope)
	if rpcErr != nil {
		return nil, rpcErr
	}
	names := make([]string, 0, len(a.Observations))
	for _, o := range a.Observations {
		names = append(names, o.EntityName)
	}
	resolved, missing, degraded, err := s.svc.ResolveNames(ctx, names)
	if err != nil {
		return errorResult(err), nil
	}
	var added, failed []map[string]any
	for _, o := range a.Observations {
		nodeID, ok := resolved[o.EntityName]
		if !ok {
			failed = append(failed, map[string]any{
				"entity": o.EntityName, "error": "未解析到实体（先用 create_entities 创建）",
			})
			continue
		}
		// 用 UpsertEntities 复用同一条写入路径（节点覆盖写 + 观测记忆）。
		res, err := s.svc.UpsertEntities(ctx, scope, []memory.EntityRef{{
			Name: o.EntityName, Observations: o.Contents,
		}})
		if err != nil {
			failed = append(failed, map[string]any{"entity": o.EntityName, "error": err.Error()})
			continue
		}
		for _, e := range res.Entities {
			added = append(added, map[string]any{
				"entity": o.EntityName, "node_id": nodeID, "memories": e.Memories,
			})
		}
	}
	return textResult(map[string]any{
		"added": added, "failed": failed, "unresolved_names": missing,
		"degraded": degraded, "note": aliasDisclaimer,
	}), nil
}

func (s *Server) toolSearchNodes(ctx context.Context, args json.RawMessage) (any, *RPCError) {
	var a struct {
		Query string `json:"query"`
		Scope string `json:"scope"`
		K     int    `json:"k"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, invalidParams("search_nodes: %v", err)
	}
	if strings.TrimSpace(a.Query) == "" {
		return nil, invalidParams("search_nodes: query 不能为空")
	}
	scope, rpcErr := s.scopeOptional(a.Scope)
	if rpcErr != nil {
		return nil, rpcErr
	}
	res, err := s.svc.Recall(ctx, memory.RecallRequest{
		Query: a.Query, Scope: scope, K: a.K, Actor: "mcp",
	})
	if err != nil {
		return errorResult(err), nil
	}
	anchors := memory.Anchors(res)
	return textResult(map[string]any{
		"entities": anchors, "count": len(anchors),
		"status": string(res.Status), "coverage": res.Coverage,
		"memories": recallItems(res), "missing": res.Missing,
		"degraded": append(append([]string{}, res.Degraded...), s.strategy().Degraded...),
		"note":     aliasDisclaimer,
	}), nil
}

func (s *Server) toolOpenNodes(ctx context.Context, args json.RawMessage) (any, *RPCError) {
	var a struct {
		Names []string `json:"names"`
		Scope string   `json:"scope"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, invalidParams("open_nodes: %v", err)
	}
	if len(a.Names) == 0 {
		return nil, invalidParams("open_nodes: names 不能为空")
	}
	resolved, missing, degraded, err := s.svc.ResolveNames(ctx, a.Names)
	if err != nil {
		return errorResult(err), nil
	}
	cards := make([]any, 0, len(resolved))
	// 确定性顺序：按输入名字的顺序输出，便于比对。
	for _, n := range a.Names {
		id, ok := resolved[n]
		if !ok {
			continue
		}
		card, err := s.graph.EntityView(ctx, id, nil)
		if err != nil {
			degraded = append(degraded, fmt.Sprintf("实体 %q（%s）读取失败: %v", n, id, err))
			continue
		}
		cards = append(cards, card)
	}
	return textResult(map[string]any{
		"entities": cards, "count": len(cards), "not_found": missing,
		"degraded": degraded, "note": aliasDisclaimer,
	}), nil
}

func (s *Server) toolReadGraph(ctx context.Context, args json.RawMessage) (any, *RPCError) {
	var a struct {
		Scope  string `json:"scope"`
		Budget int    `json:"budget"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, invalidParams("read_graph: %v", err)
	}
	scope, rpcErr := s.scopeOptional(a.Scope)
	if rpcErr != nil {
		return nil, rpcErr
	}
	// 复用概览模式（预算折叠与聚簇一并生效）；绝不另写一套"全量导出"。
	resp, err := s.graph.GraphView(ctx, server.GraphQuery{Scope: scope, Budget: a.Budget})
	if err != nil {
		return apiErrorToResult("read_graph", err), nil
	}
	return textResult(map[string]any{
		"meta": resp.Meta, "nodes": resp.Nodes, "edges": resp.Edges,
		"clusters": resp.Clusters, "cluster_edges": resp.ClusterEdges,
		"note": aliasDisclaimer,
	}), nil
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// decodeArgs 把 arguments 解进结构体。
//
// 用 DisallowUnknownFields 之外的方式（允许未知字段）：MCP 客户端常把
// 官方 server 的额外参数原样透传过来，严守未知字段会让兼容别名层直接失效。
// 但类型不符必须报错——那才是"参数写错了"。
func decodeArgs(args json.RawMessage, v any) error {
	if len(args) == 0 {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(string(args)))
	if err := dec.Decode(v); err != nil {
		return err
	}
	return nil
}

// scopeOr 解析必需 scope（缺省用服务端默认）。
func (s *Server) scopeOr(v string) (store.Scope, *RPCError) {
	if strings.TrimSpace(v) == "" {
		return s.scope, nil
	}
	sc := store.Scope(v)
	if !sc.Valid() {
		return "", invalidParams("scope 必须是 user / agent / project / global 之一，收到 %q", v)
	}
	return sc, nil
}

// scopeOptional 解析可选 scope（空 = 不限）。
func (s *Server) scopeOptional(v string) (store.Scope, *RPCError) {
	if strings.TrimSpace(v) == "" {
		return "", nil
	}
	sc := store.Scope(v)
	if !sc.Valid() {
		return "", invalidParams("scope 必须是 user / agent / project / global 之一，收到 %q", v)
	}
	return sc, nil
}

// parseKinds 校验 kind 列表。
func parseKinds(in []string) ([]store.MemoryKind, *RPCError) {
	out := make([]store.MemoryKind, 0, len(in))
	for _, s := range in {
		k := store.MemoryKind(s)
		if !k.Valid() {
			return nil, invalidParams("kind 必须是 semantic / episodic / procedural / decision 之一，收到 %q", s)
		}
		out = append(out, k)
	}
	return out, nil
}

// memoryView 是记忆的可序列化投影（不直接吐 store.Memory：字段名要与
// 前端/CLI 的 snake_case 一致，避免同一份数据三种命名）。
func memoryView(m store.Memory) map[string]any {
	return map[string]any{
		"id": m.ID, "version": m.Version, "scope": string(m.Scope),
		"kind": string(m.Kind), "content": m.Content,
		"concept_id": m.ConceptID, "source_episode": m.SourceEpisode,
		"t_valid": m.TValid, "t_invalid": m.TInvalid,
		"t_created": m.TCreated, "t_expired": m.TExpired,
		"importance": m.Importance, "recall_count": m.RecallCount,
	}
}

// recallItems 把召回结果投影成可读列表（保留分数分解，R5）。
func recallItems(res recall.RecallResult) []map[string]any {
	out := make([]map[string]any, 0, len(res.Items))
	for _, it := range res.Items {
		out = append(out, map[string]any{
			"id": it.Memory.ID, "version": it.Memory.Version,
			"kind": string(it.Memory.Kind), "scope": string(it.Memory.Scope),
			"content": it.Memory.Content, "score": it.Score,
			"fusion_score": it.FusionScore, "concept_boost": it.ConceptBoost,
			"recency_factor": it.RecencyFactor,
			"contributions":  it.Contributions, "adjustments": it.Adjustments,
			"graph_evidence":     it.GraphEvidence,
			"concept_consistent": it.ConceptConsistent,
		})
	}
	return out
}

// opsView 把 op 结果投影成可序列化视图。
func opsView(res ops.Result) map[string]any {
	return map[string]any{
		"op": string(res.Op), "scope": string(res.Scope), "status": string(res.Status),
		"key": res.Key, "steps": stepSummaries(res),
		"degraded": res.Degraded, "warnings": res.Warnings,
	}
}

// stepSummaries 提取步骤摘要（只保留有内容的摘要，跳过空行）。
//
// 不排序：步骤顺序本身就是 Plan 的语义（先写 episode 再写记忆），
// 排序会把"为什么失败在第二步"这条信息抹掉。
func stepSummaries(res ops.Result) []string {
	steps := make([]string, 0, len(res.Steps))
	for _, st := range res.Steps {
		if st.Diff.Summary != "" {
			steps = append(steps, st.Diff.Summary)
		}
	}
	return steps
}

// apiErrorToResult 把 server 的错误转成工具结果。
//
// 400/404 是**业务结果**（模型需要看到"这个边不可裁决"才能自我修正），
// 因此走 isError=true 的 ToolResult 而不是 JSON-RPC error。
func apiErrorToResult(tool string, err error) ToolResult {
	if ae, ok := server.AsAPIError(err); ok {
		return errorResult(fmt.Errorf("%s: %s（%s, status=%d）", tool, ae.Message, ae.Code, ae.Status))
	}
	return errorResult(fmt.Errorf("%s: %w", tool, err))
}

// 编译期断言：Strategy 的降级说明可被本包直接读（避免误改字段名）。
var _ = llm.ModeDegraded
var _ = errors.Is
