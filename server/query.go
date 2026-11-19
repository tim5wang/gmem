package server

import (
	"context"
	"errors"
	"net/http"
	"sort"

	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// 导出面：让 CLI / MCP 复用**同一套**图与实体聚合（TD §10 D5）
//
// # 为什么必须有这一层，而不是让 CLI/MCP 自己拼
//
// /api/graph 的三入口合流（focus / q / overview）、跳数裁剪、预算折叠、
// 已失效边补齐、确定性格局，全部在 buildGraph 里。CLI 的 `graph` 与 MCP 的
// `gmem_graph` 若各写一遍，就会出现"HTTP 是对的、CLI/MCP 是另一套语义"的分叉——
// 而这种分叉不会报错，只是慢慢漂移（TD §10 要的正是"同一接口面三层薄壳"）。
//
// 因此这里只做**参数整形 + 校验复用**，业务逻辑一行都不复制：解析完仍是
// graphParams，仍交给 buildGraph / buildEntity / applyReview。
//
// HTTP 处理器（handleGraph 等）与本层共用同样的校验函数，因此
// "HTTP 返回 400 的输入，CLI/MCP 也一定被拒"，错误码与文案也一致。
// ===========================================================================

// GraphQuery 是图视图的导出请求（CLI --focus/--hops/--as-of、MCP gmem_graph）。
//
// 零值字段按 HTTP 的默认值处理：Hops=0 → 1，Budget=0 → 200，K=0 → 5。
type GraphQuery struct {
	// Focus 是中心节点（focus 模式）；与 Query 二选一。
	Focus string
	// Query 走 Recall 取命中实体作种子（search 模式）。
	Query string
	// Concept 只保留该概念下的节点（P5 的图专用过滤，HTTP 侧同名参数）。
	Concept string
	// Scope 空 = 由节点自身推导（focus）或默认 user。
	Scope store.Scope
	Hops  int
	// AsOf 是双时态回溯（unix 秒），nil = 当前。
	AsOf   *int64
	Budget int
	K      int
}

// GraphView 产出一次图视图。与 GET /api/graph 完全同一条代码路径。
func (s *Server) GraphView(ctx context.Context, q GraphQuery) (GraphResponse, error) {
	p := graphParams{
		focus: q.Focus, query: q.Query, concept: q.Concept, scope: q.Scope,
		hops: q.Hops, asOf: q.AsOf, budget: q.Budget, k: q.K,
	}
	if p.hops == 0 {
		p.hops = defaultHops
	}
	if p.budget == 0 {
		p.budget = defaultBudget
	}
	if p.k == 0 {
		p.k = defaultSeedK
	}
	if err := p.validate(); err != nil {
		return GraphResponse{}, err
	}
	return s.buildGraph(ctx, p)
}

// EntityView 组装实体卡。与 GET /api/entity/{id} 完全同一条代码路径。
func (s *Server) EntityView(ctx context.Context, id string, asOf *int64) (EntityCard, error) {
	if id == "" {
		return EntityCard{}, badRequest("id", "缺少实体 ID")
	}
	return s.buildEntity(ctx, id, asOf)
}

// Review 执行一次待审裁决。与 POST /api/review 完全同一条代码路径。
//
// # 这条方法是 MCP 不得绕过守卫的**结构性保证**
//
// "已失效边拒裁"（t_invalid != 0 → 400）写死在 applyReview 里，而不是只在 UI 上。
// MCP 的 gmem_review 裁决走本方法 → 同一个 applyReview → 守卫必然生效。
// 若哪天有人让 MCP 直接调 ops.Maintainer.Merge，那条守卫就会被绕过，
// 于是"已判为不同"的两个实体可以被再次合并（TD §4：误合并最难恢复）。
func (s *Server) Review(ctx context.Context, req ReviewRequest) (ReviewResponse, error) {
	checked, err := validateReviewRequest(req)
	if err != nil {
		return ReviewResponse{}, err
	}
	return s.applyReview(ctx, checked)
}

// APIError 是 apiError 的导出投影，供 CLI / MCP 判断 HTTP 语义（400 / 404 / 500）。
type APIError struct {
	Status  int
	Code    string
	Message string
	Field   string
}

// Error 实现 error。
func (e APIError) Error() string { return e.Message }

// AsAPIError 把 server 的错误投影成 APIError。
//
// CLI 与 MCP 需要区分"调用方能修的错误"（400：参数/状态）与"服务端错误"（500），
// 否则 MCP 会把参数错误报成 -32603（内部错误），调用方以为重试就好了。
func AsAPIError(err error) (APIError, bool) {
	var ae *apiError
	if errors.As(err, &ae) {
		return APIError{Status: ae.status, Code: ae.code, Message: ae.message, Field: ae.field}, true
	}
	return APIError{}, false
}

// IsNotFound 报告错误是否为 404 语义（实体/边不存在）。
func IsNotFound(err error) bool {
	if ae, ok := AsAPIError(err); ok {
		return ae.Status == http.StatusNotFound
	}
	return errors.Is(err, store.ErrNotFound)
}

// PendingReview 是一条待审的 possiblySame 边。
type PendingReview struct {
	EdgeID string  `json:"edge_id"`
	From   string  `json:"from"`
	To     string  `json:"to"`
	Weight float64 `json:"weight"`
	TValid int64   `json:"t_valid"`
	Scope  string  `json:"scope"`
	// Reason 是给人看的一句话（为什么它在待审池里）。
	Reason string `json:"reason"`
}

// PendingReviews 枚举待审的 possiblySame 边（MCP 的 gmem_review / CLI 的 review）。
//
// # 为什么复用而不是新写一套
//
// 待审池的定义包含三条隐含规则：只看 possiblySame、已失效（t_invalid != 0）的
// 不再进池、scope 过滤按节点归属。它们与 applyReview 的守卫是**同一个口径**。
// 两者若各写一遍，就会出现"列表里还有、点裁决说已失效"的矛盾。
// 因此这里用同一个 relationPossiblySame 常量与同一份边枚举端口（s.catalog）。
func (s *Server) PendingReviews(ctx context.Context, scope store.Scope) ([]PendingReview, error) {
	if s.catalog == nil {
		return nil, internalError("后端未提供边表端口：无法枚举待审边")
	}
	scopes := []store.Scope{store.ScopeUser, store.ScopeAgent, store.ScopeProject, store.ScopeGlobal}
	if scope != "" {
		if !scope.Valid() {
			return nil, badRequest("scope", "scope 必须是 user / agent / project / global 之一，收到 %q", scope)
		}
		scopes = []store.Scope{scope}
	}
	out := make([]PendingReview, 0, 8)
	for _, sc := range scopes {
		edges, err := s.catalog.ListEdges(ctx, sc, 0)
		if err != nil {
			continue
		}
		for _, e := range edges {
			if e.Relation != relationPossiblySame {
				continue
			}
			// 已失效的边不再进待审池：它们已经被裁过（与 applyReview 的守卫同口径）。
			if e.TInvalid != 0 {
				continue
			}
			out = append(out, PendingReview{
				EdgeID: e.ID, From: e.FromID, To: e.ToID, Weight: e.Weight,
				TValid: e.TValid, Scope: string(sc),
				Reason: "疑似同一实体，待裁决（绝不自动合并）",
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EdgeID < out[j].EdgeID })
	return out, nil
}
