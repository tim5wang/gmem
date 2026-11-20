package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/tim5wang/gmem/server"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// GET /api/review/pending：待审池全貌（TD §9 六页 ⑤ Review Queue）
//
// 这些用例钉死三件不能靠肉眼的事：
//  1. **透传**：响应与 Server.PendingReviews 逐字段相同（不重算、不二次过滤）；
//  2. **同口径**：已失效的 possiblySame 边既不在池里，也不能被 POST /api/review 裁决；
//  3. **scope 语义**：单 scope 是并集的子集，并集条数 == 各 scope 条数之和。
// ===========================================================================

// pendingFixture 造两条 user 边 + 一条 agent 边（都是 possiblySame）。
//
// 三条边的 t_valid 刻意不同，便于断言"响应顺序不被打乱"（透传的是
// PendingReviews 的 edge_id 升序，不是 t_valid 降序——后者是前端展示层的事）。
func pendingFixture(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.node("alice", "Alice", store.ScopeUser, "person")
	h.node("alice-dup", "Alice (dup)", store.ScopeUser, "person")
	h.node("bob", "Bob", store.ScopeUser, "person")
	h.node("bob-dup", "Bob (dup)", store.ScopeUser, "person")
	// agent scope 的两个节点：用来证明 ?scope= 真的在过滤。
	h.node("run-1", "Run 1", store.ScopeAgent, "run")
	h.node("run-1-dup", "Run 1 (dup)", store.ScopeAgent, "run")

	h.edge("e-a", "alice", "alice-dup", store.Relation("possiblySame"), 0.9, 300, 0)
	h.edge("e-b", "bob", "bob-dup", store.Relation("possiblySame"), 0.8, 200, 0)
	h.edge("e-c", "run-1", "run-1-dup", store.Relation("possiblySame"), 0.7, 100, 0)
	return h
}

// getPending 请求 /api/review/pending 并解析。
func getPending(t *testing.T, h *harness, query string) (int, server.ReviewPendingResponse, []byte) {
	t.Helper()
	status, body := h.get("/api/review/pending" + query)
	var resp server.ReviewPendingResponse
	if status == http.StatusOK {
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("unmarshal pending: %v (body=%s)", err, body)
		}
	}
	return status, resp, body
}

// TestReviewPendingPassesFieldsThrough 钉死"逐字段透传"。
//
// 条件是双重的：既断言 HTTP 响应里的字段值，**又**与 Server.PendingReviews 的
// 导出结果逐条比对。后者是"没重算"的证据——如果有人在这一层偷改 weight 或
// 重新排序，两条路径就会分叉。
func TestReviewPendingPassesFieldsThrough(t *testing.T) {
	h := pendingFixture(t)
	status, resp, body := getPending(t, h, "")
	if status != http.StatusOK {
		t.Fatalf("GET /api/review/pending status=%d, want 200 (body=%s)", status, body)
	}

	want, err := h.srv.PendingReviews(context.Background(), "")
	if err != nil {
		t.Fatalf("PendingReviews: %v", err)
	}
	if len(resp.Pending) != len(want) {
		t.Fatalf("响应 %d 条，PendingReviews %d 条（必须逐条透传）", len(resp.Pending), len(want))
	}
	if resp.Count != len(want) {
		t.Errorf("count=%d, want %d", resp.Count, len(want))
	}
	for i, w := range want {
		got := resp.Pending[i]
		if got.EdgeID != w.EdgeID || got.From != w.From || got.To != w.To ||
			got.Weight != w.Weight || got.TValid != w.TValid ||
			got.Scope != w.Scope || got.Reason != w.Reason {
			t.Errorf("第 %d 行的透传字段与 PendingReviews 不一致：\n got=%+v\nwant=%+v", i, got.PendingReview, w)
		}
		if !got.Reviewable {
			t.Errorf("池里的边 %s 必须 reviewable=true", got.EdgeID)
		}
		if got.Invalidated {
			t.Errorf("池里的边 %s 不得报 invalidated=true（已失效边不进池）", got.EdgeID)
		}
		if got.FromName == "" || got.ToName == "" {
			t.Errorf("边 %s 的名称字段不得为空（缺名称应回退成 ID）", got.EdgeID)
		}
	}
	// 顺序也必须是 PendingReviews 的顺序（edge_id 升序），透传层不得重排。
	for i := 1; i < len(resp.Pending); i++ {
		if resp.Pending[i-1].EdgeID >= resp.Pending[i].EdgeID {
			t.Errorf("透传层打乱了 PendingReviews 的顺序：%s 在 %s 之前",
				resp.Pending[i-1].EdgeID, resp.Pending[i].EdgeID)
		}
	}
	// 名称来自节点表（不是把 ID 再抄一遍）。
	byID := map[string]server.PendingReviewRow{}
	for _, row := range resp.Pending {
		byID[row.EdgeID] = row
	}
	if got := byID["e-a"]; got.FromName != "Alice" || got.ToName != "Alice (dup)" {
		t.Errorf("e-a 的名称应为 Alice / Alice (dup)，实际 %q / %q", got.FromName, got.ToName)
	}
}

// TestReviewPendingOmitsInvalidatedEdge 【同口径硬证据】。
//
// 池里有一条 t_invalid != 0 的 possiblySame 边时，它不得出现在响应里
// （server/query.go 的过滤）；并且它与 POST /api/review 的拒裁守卫**同口径**：
// 同一条边直接裁决必须 400。
//
// 两条断言必须成对出现：只测前者，无法证明"列表拦住的边其实仍可被裁决"这种
// 危险组合不存在。
func TestReviewPendingOmitsInvalidatedEdge(t *testing.T) {
	h := pendingFixture(t)
	// 一条**已失效**的 possiblySame 边（模拟"已经裁过，但行仍留在库里"）。
	h.node("carol", "Carol", store.ScopeUser, "person")
	h.node("carol-dup", "Carol (dup)", store.ScopeUser, "person")
	h.edge("e-invalid", "carol", "carol-dup", store.Relation("possiblySame"), 0.95, 400, 500)

	status, resp, body := getPending(t, h, "")
	if status != http.StatusOK {
		t.Fatalf("status=%d, want 200 (body=%s)", status, body)
	}
	for _, row := range resp.Pending {
		if row.EdgeID == "e-invalid" {
			t.Fatalf("已失效边 e-invalid 不得出现在待审池响应里（body=%s）", body)
		}
	}
	// 对照：它确实在库里的边表里（不是"根本没写进去"导致的假绿）。
	if e, ok := findEdgeByID(t, h, "e-invalid"); !ok {
		t.Fatal("e-invalid 应仍在边表里（保留历史，不过滤），用例前提不成立")
	} else if e.TInvalid == 0 {
		t.Fatalf("e-invalid 的 t_invalid 必须非零，实际 0")
	}

	// 同口径：对这条边直接裁决必须 400（与列表的过滤理由完全一致）。
	for _, verdict := range []string{"same", "diff"} {
		st, b := h.postJSON("/api/review",
			`{"edge_id":"e-invalid","verdict":"`+verdict+`","confirmed":true}`)
		if st != http.StatusBadRequest {
			t.Fatalf("已失效边裁决 %s 必须 400（与待审池过滤同口径），实际 %d (body=%s)", verdict, st, b)
		}
	}
}

// TestReviewPendingScopeSemantics ?scope= 只含该 scope；不带 scope = 四种并集。
//
// 并集条数必须**等于各 scope 条数之和**（逐 scope 对比），而不是"看起来差不多"。
func TestReviewPendingScopeSemantics(t *testing.T) {
	h := pendingFixture(t)
	scopes := []string{"user", "agent", "project", "global"}

	perScope := map[string]int{}
	for _, sc := range scopes {
		status, resp, body := getPending(t, h, "?scope="+sc)
		if status != http.StatusOK {
			t.Fatalf("?scope=%s status=%d, want 200 (body=%s)", sc, status, body)
		}
		if resp.Meta.Scope != sc {
			t.Errorf("?scope=%s 回显 %q", sc, resp.Meta.Scope)
		}
		if len(resp.Meta.Scopes) != 1 || resp.Meta.Scopes[0] != sc {
			t.Errorf("?scope=%s 的 meta.scopes=%v，应只含该 scope", sc, resp.Meta.Scopes)
		}
		// 每条都必须属于该 scope（不是"包含"而是"只含"）。
		for _, row := range resp.Pending {
			if row.Scope != sc {
				t.Errorf("?scope=%s 的响应里出现了 scope=%s 的边 %s", sc, row.Scope, row.EdgeID)
			}
		}
		perScope[sc] = len(resp.Pending)
	}
	if perScope["user"] != 2 {
		t.Errorf("user scope 应有 2 条待审边，实际 %d", perScope["user"])
	}
	if perScope["agent"] != 1 {
		t.Errorf("agent scope 应有 1 条待审边，实际 %d", perScope["agent"])
	}

	status, all, body := getPending(t, h, "")
	if status != http.StatusOK {
		t.Fatalf("无 scope status=%d, want 200 (body=%s)", status, body)
	}
	sum := 0
	for _, sc := range scopes {
		sum += perScope[sc]
	}
	if len(all.Pending) != sum {
		t.Fatalf("并集 %d 条，各 scope 之和 %d 条（必须相等）", len(all.Pending), sum)
	}
	if all.Meta.Scope != "" {
		t.Errorf("无 scope 时 meta.scope 应为空字符串，实际 %q", all.Meta.Scope)
	}
	if len(all.Meta.Scopes) != len(scopes) {
		t.Errorf("无 scope 时 meta.scopes 应列出全部 %d 个 scope，实际 %v", len(scopes), all.Meta.Scopes)
	}
	// 并集里两个 scope 的边都要出现（否则"相等"可能来自"两边都空"）。
	seen := map[string]bool{}
	for _, row := range all.Pending {
		seen[row.Scope] = true
	}
	if !seen["user"] || !seen["agent"] {
		t.Errorf("并集里必须同时出现 user 与 agent 的边，实际覆盖 %v", seen)
	}
}

// TestReviewPendingInvalidScope400 非法 scope → 400 + 可读错误（且不发请求给后端）。
func TestReviewPendingInvalidScope400(t *testing.T) {
	h := pendingFixture(t)
	for _, bad := range []string{"bogus", "USER", "user,agent", "all"} {
		status, body := h.get("/api/review/pending?scope=" + bad)
		if status != http.StatusBadRequest {
			t.Fatalf("scope=%q 应 400，实际 %d (body=%s)", bad, status, body)
		}
		var e server.ErrorResponse
		if err := json.Unmarshal(body, &e); err != nil {
			t.Fatalf("错误体不是 JSON: %v (body=%s)", err, body)
		}
		if e.Field != "scope" {
			t.Errorf("scope=%q 的错误字段应为 scope，实际 %q", bad, e.Field)
		}
		if e.Message == "" {
			t.Errorf("scope=%q 的错误信息必须可读", bad)
		}
	}
}

// TestReviewPendingNotShadowedByReviewMethodGuard 新端点不得被 /api/review 的
// method guard 误伤（这是本卡最容易做错的地方之一）。
func TestReviewPendingNotShadowedByReviewMethodGuard(t *testing.T) {
	h := pendingFixture(t)

	// 1) GET /api/review/pending 必须是 200（不是 405）。
	if status, body := h.get("/api/review/pending"); status != http.StatusOK {
		t.Fatalf("GET /api/review/pending status=%d, want 200 (body=%s)", status, body)
	}
	// 2) GET /api/review 仍必须 405（既有行为不变）。
	if status, body := h.get("/api/review"); status != http.StatusMethodNotAllowed {
		t.Errorf("GET /api/review status=%d, want 405 (body=%s)", status, body)
	}
	// 3) POST /api/review/pending 必须 405（新端点只读）。
	if status, body := h.postJSON("/api/review/pending", `{}`); status != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/review/pending status=%d, want 405 (body=%s)", status, body)
	}
	// 4) POST /api/review 仍正常（既有写路径未被新路由挤掉）。
	if status, body := h.postJSON("/api/review",
		`{"edge_id":"e-a","verdict":"diff","confirmed":true}`); status != http.StatusOK {
		t.Errorf("POST /api/review status=%d, want 200 (body=%s)", status, body)
	}
	// 5) 裁决后该边必须从池里消失（端点是活数据，不是缓存）。
	_, resp, _ := getPending(t, h, "")
	for _, row := range resp.Pending {
		if row.EdgeID == "e-a" {
			t.Error("裁决为 diff 后 e-a 仍在池里（端点必须读实时数据）")
		}
	}
}

// TestReviewPendingEmptyPool 空池回 200 + 空数组（不是 null、不是 404）。
//
// 前端据此显示空态；回 null 会让"暂无待审"变成 JS 里的 undefined 崩溃点。
func TestReviewPendingEmptyPool(t *testing.T) {
	h := newHarness(t)
	h.node("alice", "Alice", store.ScopeUser, "")
	status, resp, body := getPending(t, h, "")
	if status != http.StatusOK {
		t.Fatalf("空池 status=%d, want 200 (body=%s)", status, body)
	}
	if len(resp.Pending) != 0 || resp.Count != 0 {
		t.Fatalf("空池应返回空数组与 count=0，实际 pending=%v count=%d", resp.Pending, resp.Count)
	}
	if resp.Meta.MaxBatch != server.MaxReviewBatch {
		t.Errorf("meta.max_batch=%d, want %d（批量上限必须由接口显式给出）",
			resp.Meta.MaxBatch, server.MaxReviewBatch)
	}
}

// TestReviewPendingVerdictVisibleOnGraph 【裁完在图上看到】（TD §9 的 Q --> G 箭头）。
//
// 裁决必须改变**图视图**里看得见的东西，否则"合并/判异"在 UI 上等于什么都没发生。
// 这里用真实的 Merge 落地验证：
//   - 待审池里那条 possiblySame 边消失（它已被裁掉）；
//   - 迁移后的普通边出现在 /api/graph?focus=<保留节点> 的边集里，且指向新实体。
func TestReviewPendingVerdictVisibleOnGraph(t *testing.T) {
	h := newHarness(t)
	h.node("alice", "Alice", store.ScopeUser, "person")
	h.node("alice2", "Alice (dup)", store.ScopeUser, "person")
	h.node("acme", "Acme", store.ScopeUser, "org")
	// alice 的一条普通边：合并后它必须迁到 alice2（这就是"图上看得见的差异"）。
	h.edge("e-works", "alice", "acme", store.RelationWorksAt, 0.9, 100, 0)
	// 待审的疑似重复边。
	h.edge("e-ps", "alice", "alice2", store.Relation("possiblySame"), 1, 100, 0)

	// 1) 裁决前：待审池里有这条边。
	_, before, beforeBody := getPending(t, h, "?scope=user")
	foundPending := false
	for _, row := range before.Pending {
		if row.EdgeID == "e-ps" {
			foundPending = true
		}
	}
	if !foundPending {
		t.Fatalf("裁决前 e-ps 应在待审池里（body=%s）", beforeBody)
	}

	// 2) 判为同一（Merge alice → alice2）。
	if status, body := h.postJSON("/api/review",
		`{"edge_id":"e-ps","verdict":"same","confirmed":true}`); status != http.StatusOK {
		t.Fatalf("裁决 status=%d body=%s", status, body)
	}

	// 3) 待审池里它必须消失（端点读实时数据）。
	_, after, body := getPending(t, h, "?scope=user")
	for _, row := range after.Pending {
		if row.EdgeID == "e-ps" {
			t.Fatalf("裁决后 e-ps 仍在待审池里（body=%s）", body)
		}
	}

	// 4) 图视图（页面 /review 每行跳转的目标）：以保留节点为焦点，
	// 迁移后的 works_at 边必须以 alice2 → acme 的形式出现。
	status, g, gb := h.getGraph("?focus=alice2&hops=1")
	if status != http.StatusOK {
		t.Fatalf("graph focus=alice2 status=%d body=%s", status, gb)
	}
	if _, ok := findNode(g, "alice2"); !ok {
		t.Fatalf("保留节点 alice2 必须出现在图上（body=%s）", gb)
	}
	if _, ok := findNode(g, "acme"); !ok {
		t.Fatalf("合并后与 acme 的关系必须仍可见（否则合并把信息弄丢了，body=%s）", gb)
	}
	var migrated *server.Edge
	for i := range g.Edges {
		e := g.Edges[i]
		if e.Relation != string(store.RelationWorksAt) {
			continue
		}
		migrated = &e
	}
	if migrated == nil {
		t.Fatalf("合并后图上必须仍有一条 works_at 边（body=%s）", gb)
	}
	if migrated.From != "alice2" {
		t.Errorf("合并后 works_at 的起点应为 alice2（旧 ID 已重定向），实际 %q", migrated.From)
	}
	if migrated.To != "acme" {
		t.Errorf("works_at 的终点应保持 acme，实际 %q", migrated.To)
	}
	if migrated.Invalidated {
		t.Error("迁移后的边必须是当前有效边（不得是虚线已失效状态）")
	}
}
