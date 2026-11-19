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
// POST /api/review：唯一的写入口
// ===========================================================================

// reviewFixture 造一对待审实体 + 一条 possiblySame 边。
func reviewFixture(t *testing.T) (*harness, string) {
	t.Helper()
	h := newHarness(t)
	h.node("alice", "Alice", store.ScopeUser, "person")
	h.node("alice2", "Alice (dup)", store.ScopeUser, "person")
	h.edge("e-ps", "alice", "alice2", store.Relation("possiblySame"), 1, 100, 0)
	return h, "e-ps"
}

// TestReviewRequiresConfirmed **未传 confirmed=true 必须 400**（TD §9 二次确认）。
func TestReviewRequiresConfirmed(t *testing.T) {
	h, edgeID := reviewFixture(t)
	bodies := []string{
		`{"edge_id":"` + edgeID + `","verdict":"same"}`,                   // 缺 confirmed
		`{"edge_id":"` + edgeID + `","verdict":"same","confirmed":false}`, // 显式 false
	}
	for i, body := range bodies {
		status, resp := h.postJSON("/api/review", body)
		if status != http.StatusBadRequest {
			t.Fatalf("第 %d 个 body 未二次确认却返回 %d（body=%s），必须 400", i+1, status, resp)
		}
		var e server.ErrorResponse
		if err := json.Unmarshal(resp, &e); err != nil {
			t.Fatalf("错误体不是 JSON: %v (body=%s)", err, resp)
		}
		if e.Field != "confirmed" {
			t.Errorf("field=%q, want confirmed", e.Field)
		}
		if e.Message == "" {
			t.Error("错误信息必须可读")
		}
	}
	// 关键：400 之后**什么都没写**（边仍待审）。
	resp, ok := findEdgeByID(t, h, edgeID)
	if !ok {
		t.Fatal("被拒的裁决不应改动边，但边不见了")
	}
	if resp.Relation != "possiblySame" {
		t.Errorf("被拒后边的关系不应变化，实际 %q", resp.Relation)
	}
}

// TestReviewRejectsInvalidVerdict 非法 verdict → 400（且不写）。
func TestReviewRejectsInvalidVerdict(t *testing.T) {
	h, edgeID := reviewFixture(t)
	for _, v := range []string{"SAME", "different", "", "merge"} {
		body := `{"edge_id":"` + edgeID + `","verdict":"` + v + `","confirmed":true}`
		status, resp := h.postJSON("/api/review", body)
		if status != http.StatusBadRequest {
			t.Fatalf("verdict=%q 应 400，实际 %d (body=%s)", v, status, resp)
		}
	}
}

// TestReviewRejectsNonPendingEdge 非 possiblySame 边必须拒绝（否则就是改写图的后门）。
func TestReviewRejectsNonPendingEdge(t *testing.T) {
	h := newHarness(t)
	h.node("alice", "Alice", store.ScopeUser, "")
	h.node("acme", "Acme", store.ScopeUser, "")
	h.edge("e-works", "alice", "acme", store.RelationWorksAt, 1, 100, 0)

	status, body := h.postJSON("/api/review",
		`{"edge_id":"e-works","verdict":"same","confirmed":true}`)
	if status != http.StatusBadRequest {
		t.Fatalf("非 possiblySame 边应 400，实际 %d (body=%s)", status, body)
	}
	// 原边必须没被动过。
	if _, ok := findEdgeByID(t, h, "e-works"); !ok {
		t.Fatal("被拒的裁决不得删除/改动 works_at 边")
	}
}

// TestReviewUnknownEdge404 未知边 → 404。
func TestReviewUnknownEdge404(t *testing.T) {
	h, _ := reviewFixture(t)
	status, body := h.postJSON("/api/review",
		`{"edge_id":"e-does-not-exist","verdict":"same","confirmed":true}`)
	if status != http.StatusNotFound {
		t.Fatalf("未知边应 404，实际 %d (body=%s)", status, body)
	}
}

// TestReviewSameVerdictMerges same → 走 Merge：边迁到保留节点，旧 ID 有重定向。
func TestReviewSameVerdictMerges(t *testing.T) {
	h, edgeID := reviewFixture(t)
	status, body := h.postJSON("/api/review",
		`{"edge_id":"`+edgeID+`","verdict":"same","confirmed":true}`)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var resp server.ReviewResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, body)
	}
	if resp.Verdict != "same" {
		t.Errorf("verdict=%q, want same", resp.Verdict)
	}
	// 必须真的走了 merge op（而不是只回一句 200）。
	if resp.Op != "merge" {
		t.Errorf("op=%q, want merge（same 必须经 Merge 落地）", resp.Op)
	}
	if resp.Status == "" {
		t.Error("必须回报 op 的执行状态")
	}
	// 旧 ID 必须保留为重定向（TD §4：合并可回溯）。
	to, err := h.db.OpsBackend().ResolveRedirect(context.Background(), "alice")
	if err != nil {
		t.Fatalf("ResolveRedirect: %v", err)
	}
	if to != "alice2" {
		t.Errorf("alice 应被重定向到 alice2，实际 %q", to)
	}
}

// TestReviewDiffVerdictWritesNegative diff → 写 negative_SameAs，并清掉待审边。
func TestReviewDiffVerdictWritesNegative(t *testing.T) {
	h, edgeID := reviewFixture(t)
	status, body := h.postJSON("/api/review",
		`{"edge_id":"`+edgeID+`","verdict":"diff","confirmed":true}`)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var resp server.ReviewResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, body)
	}
	if resp.Verdict != "diff" {
		t.Errorf("verdict=%q, want diff", resp.Verdict)
	}
	if resp.Op != "disambiguate" {
		t.Errorf("op=%q, want disambiguate", resp.Op)
	}
	// negative_SameAs 必须真的落库。
	edges, err := h.db.OpsBackend().ListEdges(context.Background(), store.ScopeUser, 0)
	if err != nil {
		t.Fatalf("ListEdges: %v", err)
	}
	var neg, ps int
	for _, e := range edges {
		switch string(e.Relation) {
		case "negative_SameAs":
			neg++
		case "possiblySame":
			if e.TInvalid == 0 {
				ps++
			}
		}
	}
	if neg != 1 {
		t.Errorf("应写入 1 条 negative_SameAs，实际 %d", neg)
	}
	if ps != 0 {
		t.Errorf("裁决为 different 后不应仍有未失效的 possiblySame 边，实际 %d", ps)
	}
}

// TestReviewEmptyAndMalformed 空 body / 非法 JSON / 缺 edge_id → 400（不得 panic）。
func TestReviewEmptyAndMalformed(t *testing.T) {
	h, _ := reviewFixture(t)
	cases := []struct{ name, body string }{
		{"空 body", ""},
		{"非法 JSON", "{not json"},
		{"缺 edge_id", `{"verdict":"same","confirmed":true}`},
		{"类型不符", `{"edge_id":123,"verdict":"same","confirmed":true}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic: %v", r)
				}
			}()
			status, body := h.postJSON("/api/review", tc.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status=%d, want 400 (body=%s)", status, body)
			}
		})
	}
}

// findEdgeByID 从边表读一条边（用来断言"被拒的裁决没有写库"）。
func findEdgeByID(t *testing.T, h *harness, id string) (store.Edge, bool) {
	t.Helper()
	edges, err := h.db.OpsBackend().ListEdges(context.Background(), store.ScopeUser, 0)
	if err != nil {
		t.Fatalf("ListEdges: %v", err)
	}
	for _, e := range edges {
		if e.ID == id {
			return e, true
		}
	}
	return store.Edge{}, false
}
