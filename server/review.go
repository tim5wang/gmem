package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"

	"github.com/tim5wang/gmem/ops"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// POST /api/review：**唯一的写入口**（TD §9「待审裁决」）
//
// 契约：body {edge_id, verdict: "same"|"diff", confirmed: true}
//
//   - same → ops.Maintainer.Merge（把 from 并入 to，可回溯的重定向）
//   - diff → 写 negative_SameAs（此后这一对不再进灰区）
//
// 三条硬约束：
//  1. **二次确认**：confirmed 必须显式为 true，否则 400。误合并是记忆系统
//     最难恢复的错误（§4），不给"顺手点一下"留路径。
//  2. **只裁决 possiblySame 边**：其它关系一律拒绝——否则这里就成了
//     "任意改写图"的后门，与"只读优先"直接冲突。
//  3. 除本路由外**没有写路径**（由 Routes() 的方法集合守卫钉死）。
// ===========================================================================

// relationPossiblySame / relationNegativeSameAs 是待审关系常量。
//
// 与 ops/ops_write.go 的两个私有常量同名同值。为什么不从 ops 导出复用：
// 那两个常量属于 op 实现细节（Unlink/Merge 的记账边），把它提升为公开契约
// 会把"哪些关系是记账边"变成 API 的一部分。这里重复一份字符串，
// 由 server/guard_test.go 的用例断言两边取值一致——一旦漂移就变红。
const (
	relationPossiblySame   store.Relation = "possiblySame"
	relationNegativeSameAs store.Relation = "negative_SameAs"
)

// maxReviewBody 限制请求体大小（本地服务，但不接受无上限读取）。
const maxReviewBody = 1 << 16

// handleReview 是 POST /api/review 的处理器。
func (s *Server) handleReview(w http.ResponseWriter, r *http.Request) {
	if s.maint == nil {
		s.writeError(w, internalError("维护面未注入（ops.Maintainer 为 nil）：无法执行裁决"))
		return
	}
	req, err := decodeReview(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	resp, err := s.applyReview(r.Context(), req)
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// decodeReview 解析并校验请求体。
func decodeReview(r *http.Request) (ReviewRequest, error) {
	var req ReviewRequest
	body, err := io.ReadAll(io.LimitReader(r.Body, maxReviewBody))
	if err != nil {
		return req, badRequest("body", "读取请求体失败: %v", err)
	}
	if len(body) == 0 {
		return req, badRequest("body", "请求体不能为空：需要 {edge_id, verdict, confirmed}")
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return req, badRequest("body", "请求体不是合法 JSON: %v", err)
	}
	return validateReviewRequest(req)
}

// validateReviewRequest 是**唯一**的裁决请求校验（HTTP / CLI / MCP 共用）。
//
// 二次确认与"只接受 same/diff"是数据安全阀：这些规则不得只存在于 HTTP 路径上，
// 否则 MCP 的 gmem_review 就成了一扇后门（相同的 body 经 stdio 就能绕过）。
func validateReviewRequest(req ReviewRequest) (ReviewRequest, error) {
	if req.EdgeID == "" {
		return req, badRequest("edge_id", "edge_id 必填")
	}
	// 二次确认（§9）：缺省即拒绝，绝不把"没说"当成"同意"。
	if !req.Confirmed {
		return req, badRequest("confirmed", "必须显式传 confirmed=true 才能执行裁决（TD §9 要求二次确认）")
	}
	switch req.Verdict {
	case "same", "diff":
	default:
		return req, badRequest("verdict", `verdict 只接受 "same" 或 "diff"，收到 %q`, req.Verdict)
	}
	if v := req.Scope; v != "" {
		if sc := store.Scope(v); !sc.Valid() {
			return req, badRequest("scope", "scope 必须是 user / agent / project / global 之一，收到 %q", v)
		}
	}
	return req, nil
}

// applyReview 执行裁决。
func (s *Server) applyReview(ctx context.Context, req ReviewRequest) (ReviewResponse, error) {
	edge, scope, err := s.findReviewEdge(ctx, req.EdgeID, store.Scope(req.Scope))
	if err != nil {
		return ReviewResponse{}, err
	}
	// 只裁决待审边：其它关系一律拒绝（守住"只读优先"的边界）。
	if edge.Relation != relationPossiblySame {
		return ReviewResponse{}, badRequest("edge_id",
			"边 %q 的关系是 %q，只有 %q 边可以在这里裁决",
			edge.ID, edge.Relation, relationPossiblySame)
	}
	// 已裁决过的边**不能再裁决**：判过 diff 的边会被写成 negative_SameAs 并置
	// TInvalid，但那条 possiblySame 边本身仍然留在库里（保留历史，不过滤）。
	// 若不在这里拦住，同一 edge_id 还能再判一次 same → Merge 真的落地，把
	// 用户**已经判成不同**的两个实体合并掉（TD §4：误合并最难恢复）。
	// 门禁放在服务端：UI 的条件只是提示，不能当唯一防线。
	if edge.TInvalid != 0 {
		return ReviewResponse{}, badRequest("edge_id",
			"边 %q 已于 t_invalid=%d 失效（裁决已落地），不能重复裁决："+
				"如需改判请对本对实体新建待审边",
			edge.ID, edge.TInvalid)
	}
	from, to := edge.FromID, edge.ToID
	if from == "" || to == "" {
		return ReviewResponse{}, badRequest("edge_id", "边 %q 端点不完整，无法裁决", edge.ID)
	}
	note := req.Note
	if note == "" {
		note = fmt.Sprintf("图上裁决边 %s", edge.ID)
	}

	switch req.Verdict {
	case "same":
		res, err := s.maint.Merge(ctx, ops.MergeRequest{
			Scope: scope, FromNode: from, ToNode: to,
			Reason: note, Actor: "gmem-serve",
		})
		if err != nil {
			return ReviewResponse{}, reviewOpError("merge", err)
		}
		return toReviewResponse(edge.ID, "same", res), nil
	case "diff":
		res, err := s.maint.Judge(ctx, ops.JudgeRequest{
			Scope: scope,
			Verdicts: []ops.PairVerdict{{
				LeftNode: from, RightNode: to,
				Verdict: "different", Note: note,
			}},
			Actor: "gmem-serve", Reason: note,
		})
		if err != nil {
			return ReviewResponse{}, reviewOpError("disambiguate", err)
		}
		return toReviewResponse(edge.ID, "diff", res), nil
	}
	// decodeReview 已经拦掉其它取值；这里只是防御（不放任静默返回 200）。
	return ReviewResponse{}, badRequest("verdict", `verdict 只接受 "same" 或 "diff"，收到 %q`, req.Verdict)
}

// findReviewEdge 定位待审边并推导它所属的 scope。
//
// 边表没有 scope 列（edges 靠节点的 scope 归属过滤），因此未显式传 scope 时
// 按四个枚举值依次查。顺序固定 → 结果确定，不会因 map 迭代而漂移。
func (s *Server) findReviewEdge(ctx context.Context, edgeID string, want store.Scope) (store.Edge, store.Scope, error) {
	scopes := []store.Scope{want}
	if want == "" {
		scopes = []store.Scope{store.ScopeUser, store.ScopeAgent, store.ScopeProject, store.ScopeGlobal}
	}
	if s.catalog == nil {
		return store.Edge{}, "", internalError("后端未提供边表端口：无法定位边（findReviewEdge）")
	}
	for _, sc := range scopes {
		edges, err := s.catalog.ListEdges(ctx, sc, 0)
		if err != nil {
			continue
		}
		for _, e := range edges {
			if e.ID == edgeID {
				return e, sc, nil
			}
		}
	}
	return store.Edge{}, "", notFound(fmt.Sprintf("边 %q 不存在（或不在可裁决的 scope 内）", edgeID))
}

// reviewOpError 把 op 错误映射成可读的 HTTP 错误。
func reviewOpError(op string, err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return notFound(fmt.Sprintf("%s 失败：%v", op, err))
	}
	// op 的输入/状态校验失败按 400 处理：是调用方能修的（例如边端点已合并）。
	return &apiError{
		status: http.StatusBadRequest, code: "review_failed",
		message: fmt.Sprintf("%s 执行失败：%v", op, err),
	}
}

// toReviewResponse 把 ops.Result 投影成响应体。
func toReviewResponse(edgeID, verdict string, res ops.Result) ReviewResponse {
	out := ReviewResponse{
		EdgeID: edgeID, Verdict: verdict, Op: string(res.Op), Status: string(res.Status),
		Degraded: res.Degraded, Warnings: res.Warnings,
	}
	steps := make([]string, 0, len(res.Steps))
	for _, st := range res.Steps {
		if st.Diff.Summary != "" {
			steps = append(steps, st.Diff.Summary)
		}
	}
	sort.Strings(steps) // 确定性：同一输入的回答不该因步骤顺序而变
	out.Steps = steps
	if len(steps) > 0 {
		out.Result = steps[len(steps)-1]
	}
	return out
}
