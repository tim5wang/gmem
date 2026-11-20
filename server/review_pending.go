package server

import (
	"net/http"
	"strings"

	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// GET /api/review/pending：待审池**全貌**（TD §9 六页 ⑤ Review Queue）
//
// # 这一层为什么这么薄
//
// 待审池的定义（只看 possiblySame / 已失效的不进池 / scope 按节点归属过滤）
// 已经在 Server.PendingReviews 里实现一次，并且与 applyReview 的拒裁守卫同口径
// （server/query.go）。如果这里再筛一遍，就会出现"列表里还有、点裁决说已失效"
// 或反过来的矛盾——而且这种矛盾不会报错，只会慢慢漂移。
//
// 因此本端点**只做三件事**：
//  1. 解析 ?scope= 并交给 PendingReviews（非法 scope 由它返回 400，措辞与 CLI/MCP 一致）；
//  2. 逐字段透传它的结果（不重算权重、不重新排序、不做二次过滤）；
//  3. 补上页面**无法自己推导**的两类信息：两端节点名、可裁状态位。
//
// # 为什么节点名由后端补，而不是让页面自己拼
//
// 页面要显示"两端节点名"，但它只有 edge 的 from/to **ID**。让前端为每行再发两个
// /api/entity/{id} 请求，会把一次列表变成 1+2N 次请求（N 可达几十），并且合并
// 之后旧 ID 会重定向——那是另一套语义。名称是纯展示字段，由已经持有图端口的
// 服务端一次补齐最省事。
//
// # 行语义不得靠猜（调研 §6 ⑤(3)）
//
// 调研的结论是"用一个 selector 决定行语义，而不是让页面从 grade 字符串反猜"。
// 这里的做法是把结论**显式写进响应**：reviewable / invalidated 两个布尔位，
// 由后端按待审池口径给出。页面只需要读它，不需要解析 reason 的自然语言。
// ===========================================================================

// MaxReviewBatch 是一次批量裁决允许的最大边数（TD §9 ⑤「批量裁决」）。
//
// 为什么要有上限：批量裁决是对**记忆本体**的不可逆写操作，一次误操作影响的
// 实体数必须有界。50 是"一次能看完、也够用"的量级（待审池本身就是灰区，
// 真实库通常个位数到几十条）。
//
// 这个值同时出现在两处，且必须一致：
//   - 本端点响应里的 meta.max_batch（接口约束，前端据此校验并显示在 UI 上）；
//   - web/src/js/reviewflow.js 的 MAX_BATCH（UI 侧拦截）。
//
// 两边都有用例钉死（server/review_pending_test.go 与 web/src/test/review.test.mjs）。
const MaxReviewBatch = 50

// PendingReviewRow 是待审池里的一行。
//
// PendingReview 内嵌（而不是逐字段复制）：那样"透传"就是**语法层面**的事实，
// 谁想重算/改名字都必须先改 Server.PendingReviews 的类型，而不是在这里悄悄加工。
type PendingReviewRow struct {
	PendingReview
	// FromName / ToName 是两端节点的 canonical_name（缺失时回退成 ID，并在
	// meta.degraded 里说明——"名字没读到"不得表现为空字符串）。
	FromName string `json:"from_name"`
	ToName   string `json:"to_name"`
	// Invalidated 是**后端权威**的失效状态。
	//
	// 恒为 false：已失效的边不会进池（PendingReviews 已过滤）。仍然显式输出，
	// 是因为页面必须能区分"这条边不在池里是因为裁过了"与"因为它不是
	// possiblySame"。没有这个字段，前端只能从字符串反推，那正是调研反对的做法。
	Invalidated bool `json:"invalidated"`
	// Reviewable 表示本行现在可以裁决（能被 POST /api/review 接受）。
	//
	// 由后端计算，不由页面从 reason 文本里猜。当前口径 = 出现在池里即 true；
	// 未来若加入"端点已合并""边版本过期"等情形，只需在这里收紧，页面不用改。
	Reviewable bool `json:"reviewable"`
}

// ReviewPendingMeta 是待审池响应的元信息（口径与上限都要能看见）。
type ReviewPendingMeta struct {
	// Scope 回显请求的 scope（空字符串 = 所有 scope 的并集）。
	Scope string `json:"scope"`
	// Scopes 是本次实际枚举的 scope 列表（空 scope 时是四个枚举值的并集）。
	//
	// 为什么显式列出：页面显示"全库 3 条"时，读者能立刻看出这个"全库"到底
	// 覆盖了哪几个 scope，而不是相信一个数字。
	Scopes []string `json:"scopes"`
	// MaxBatch 是批量裁决上限（见 MaxReviewBatch）。
	MaxBatch int `json:"max_batch"`
	// Degraded 是本端点自己的降级清单（例如某个节点名读不到）。
	//
	// 与其它端点的 degraded 同语义：**不能读 ≠ 没有数据**。
	Degraded []string `json:"degraded,omitempty"`
}

// ReviewPendingResponse 是 GET /api/review/pending 的响应体。
type ReviewPendingResponse struct {
	Pending []PendingReviewRow `json:"pending"`
	// Count 等于 len(Pending)，冗余但方便调用方（与 CLI/MCP 的输出形状一致）。
	Count int               `json:"count"`
	Meta  ReviewPendingMeta `json:"meta"`
}

// reviewScopes 返回一次请求实际枚举的 scope 列表（与 PendingReviews 同口径）。
//
// 与 PendingReviews 里的同一份字面量重复，是因为要在响应里回显"覆盖了哪几个"。
// 两者由用例断言一致（TestReviewPendingScopeUnionMatchesPerScopeSum）。
func reviewScopes(scope store.Scope) []string {
	if scope != "" {
		return []string{string(scope)}
	}
	return []string{
		string(store.ScopeUser), string(store.ScopeAgent),
		string(store.ScopeProject), string(store.ScopeGlobal),
	}
}

// handleReviewPending 是 GET /api/review/pending 的处理器。
func (s *Server) handleReviewPending(w http.ResponseWriter, r *http.Request) {
	// scope 留空 = 所有 scope 的并集（PendingReviews 的既有语义）。
	raw := strings.TrimSpace(r.URL.Query().Get("scope"))
	scope := store.Scope(raw)
	if raw != "" && !scope.Valid() {
		// 自己先拦一道，是为了让错误**不依赖** PendingReviews 的实现细节
		// （它是导出面，未来可能改成别的措辞；HTTP 的 400 契约不能跟着漂）。
		s.writeError(w, badRequest("scope",
			"scope 必须是 user / agent / project / global 之一（留空 = 全部），收到 %q", raw))
		return
	}

	items, err := s.PendingReviews(r.Context(), scope)
	if err != nil {
		s.writeError(w, err)
		return
	}

	resp := ReviewPendingResponse{
		Pending: make([]PendingReviewRow, 0, len(items)),
		Count:   len(items),
		Meta: ReviewPendingMeta{
			Scope: raw, Scopes: reviewScopes(scope), MaxBatch: MaxReviewBatch,
		},
	}
	degraded := map[string]bool{}
	for _, it := range items {
		row := PendingReviewRow{
			PendingReview: it,
			// 口子在这里显式表态：池里的一律 not invalidated 且可裁。
			Invalidated: false,
			Reviewable:  true,
		}
		row.FromName = s.nodeDisplayName(r, it.From, &resp.Meta, degraded)
		row.ToName = s.nodeDisplayName(r, it.To, &resp.Meta, degraded)
		resp.Pending = append(resp.Pending, row)
	}
	// 顺序保持 PendingReviews 的（edge_id 升序）：透传层不重新排序。
	// 页面要的是 t_valid 降序，那是**展示**需求，由前端纯函数做（前端用例覆盖）。
	writeJSON(w, http.StatusOK, resp)
}

// nodeDisplayName 取节点的 canonical_name，读不到就回退成 ID 并显式上报。
//
// 回退而不是让整行消失：待审池的**成员资格**由 PendingReviews 决定，
// 名称只是展示字段。因为名字读不到就把一条可裁决的边藏起来，等于
// "cannot read ≠ nothing there" 的反面。
func (s *Server) nodeDisplayName(r *http.Request, id string, meta *ReviewPendingMeta, degraded map[string]bool) string {
	if id == "" || s.ports.Graph == nil {
		return id
	}
	node, err := s.ports.Graph.GetNode(r.Context(), id)
	if err != nil {
		msg := "节点 " + id + " 的名称不可读，已回退显示 ID：" + err.Error()
		if !degraded[msg] {
			degraded[msg] = true
			meta.Degraded = append(meta.Degraded, msg)
		}
		return id
	}
	if node.CanonicalName == "" {
		return id
	}
	return node.CanonicalName
}
