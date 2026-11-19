package server_test

import (
	"net/http"
	"testing"

	"github.com/tim5wang/gmem/server"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// GET /api/entity/{id}：实体卡
// ===========================================================================

// TestEntityCard 覆盖实体卡的每一项交付要求：
// canonical name、concept、全部关系（含方向与对端）、来源 episode、
// 以及 as_of 视角下"为何生效 / 它替换了什么"。
func TestEntityCard(t *testing.T) {
	h := newHarness(t)
	h.concept("person", "人", store.ConceptDraft, store.ScopeUser, store.ConceptOriginImplicit)
	h.node("acme", "Acme", store.ScopeUser, "org")
	h.node("newco", "NewCo", store.ScopeUser, "org")
	h.node("employee", "Employee", store.ScopeUser, "person")
	h.episode("ep-1", 100, "Alice 换了工作：从 Acme 到 NewCo。")
	h.nodeWithSource("alice", "Alice", store.ScopeUser, "person", "ep-1")

	// 旧关系已失效（被替换），新关系有效 —— 这正是"它替换了什么"的场景。
	h.edge("e-old", "alice", "acme", store.RelationWorksAt, 1, 100, 300)
	h.edge("e-new", "alice", "newco", store.RelationWorksAt, 1, 300, 0)
	// 入边：一个 subtype_of 指向 alice，验证方向字段。
	h.edge("e-in", "employee", "alice", store.RelationSubtypeOf, 1, 100, 0)

	status, card, body := h.getEntity("alice", "")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if card.CanonicalName != "Alice" {
		t.Errorf("canonical_name=%q, want Alice", card.CanonicalName)
	}
	if card.ID != "alice" {
		t.Errorf("id=%q", card.ID)
	}

	// 概念（含 draft + implicit，前端判斜纹）。
	if card.Concept == nil {
		t.Fatal("实体卡必须返回 concept")
	}
	if card.Concept.ID != "person" || card.Concept.Status != string(store.ConceptDraft) {
		t.Errorf("concept=%+v", card.Concept)
	}
	if card.Concept.Origin != string(store.ConceptOriginImplicit) {
		t.Errorf("concept origin=%q, want implicit", card.Concept.Origin)
	}

	// 来源 episode（溯源）。
	if card.Source == nil {
		t.Fatal("实体卡必须返回来源 episode")
	}
	if card.Source.ID != "ep-1" || card.Source.At != 100 {
		t.Errorf("source=%+v", card.Source)
	}
	if card.Source.Content == "" {
		t.Error("来源 episode 应带可读正文（侧栏要展示来源）")
	}

	// 全部关系：3 条（2 出 1 入），含方向与对端。
	if len(card.Edges) != 3 {
		t.Fatalf("应返回 3 条关系，实际 %d: %+v", len(card.Edges), card.Edges)
	}
	var sawOutOld, sawOutNew, sawIn bool
	for _, e := range card.Edges {
		switch e.ID {
		case "e-old":
			sawOutOld = true
			if e.Direction != "out" || e.Counterpart != "acme" {
				t.Errorf("e-old 方向/对端错误：%+v", e)
			}
			if !e.Invalidated {
				t.Error("e-old 已失效，invalidated 必须为 true（画虚线）")
			}
			if e.CounterpartName != "Acme" {
				t.Errorf("e-old 对端名=%q, want Acme", e.CounterpartName)
			}
		case "e-new":
			sawOutNew = true
			if e.Direction != "out" || e.Counterpart != "newco" {
				t.Errorf("e-new 方向/对端错误：%+v", e)
			}
			if e.Invalidated {
				t.Error("e-new 有效，invalidated 应为 false")
			}
		case "e-in":
			sawIn = true
			if e.Direction != "in" || e.Counterpart != "employee" {
				t.Errorf("入边方向应为 in、对端应为 employee：%+v", e)
			}
		}
	}
	if !sawOutOld || !sawOutNew || !sawIn {
		t.Fatalf("关系不全：old=%v new=%v in=%v", sawOutOld, sawOutNew, sawIn)
	}

	// 归组视图：works_at 组里"有效 1 条 + 被替换 1 条"。
	var worksAt *server.RelationGroup
	for i := range card.Relations {
		if card.Relations[i].Relation == string(store.RelationWorksAt) {
			worksAt = &card.Relations[i]
		}
	}
	if worksAt == nil {
		t.Fatalf("relations 缺少 works_at 组：%+v", card.Relations)
	}
	if worksAt.Effective == nil || worksAt.Effective.ID != "e-new" {
		t.Errorf("works_at 的生效边应为 e-new：%+v", worksAt.Effective)
	}
	if len(worksAt.Replaced) != 1 || worksAt.Replaced[0].ID != "e-old" {
		t.Errorf("works_at 被替换的旧边应为 [e-old]：%+v", worksAt.Replaced)
	}
	if worksAt.Reason == "" {
		t.Error("relation group 必须给出 reason（为何生效）")
	}

	// Why：人话解释"为何生效 / 它替换了什么"。
	if len(card.Why) == 0 {
		t.Fatal("实体卡必须返回 why 解释")
	}
	var mentionsReplaced, mentionsSource bool
	for _, w := range card.Why {
		if contains(w, "替换") {
			mentionsReplaced = true
		}
		if contains(w, "ep-1") {
			mentionsSource = true
		}
	}
	if !mentionsReplaced {
		t.Errorf("why 里必须说明它替换了什么：%v", card.Why)
	}
	if !mentionsSource {
		t.Errorf("why 里必须提到来源 episode：%v", card.Why)
	}
}

// TestEntityAsOfPerspective as_of 视角下"为何生效"必须跟着变。
func TestEntityAsOfPerspective(t *testing.T) {
	h := newHarness(t)
	h.node("alice", "Alice", store.ScopeUser, "")
	h.node("acme", "Acme", store.ScopeUser, "")
	h.node("newco", "NewCo", store.ScopeUser, "")
	h.edge("e-old", "alice", "acme", store.RelationWorksAt, 1, 100, 300)
	h.edge("e-new", "alice", "newco", store.RelationWorksAt, 1, 300, 0)

	// as_of=200：当时 e-old 有效、e-new 尚未生效。
	status, card, body := h.getEntity("alice", "?as_of=200")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if card.Meta.AsOf == nil || *card.Meta.AsOf != 200 {
		t.Errorf("meta.as_of 未回显：%+v", card.Meta.AsOf)
	}
	var group *server.RelationGroup
	for i := range card.Relations {
		if card.Relations[i].Relation == string(store.RelationWorksAt) {
			group = &card.Relations[i]
		}
	}
	if group == nil {
		t.Fatal("缺少 works_at 组")
	}
	if group.Effective == nil || group.Effective.ID != "e-old" {
		t.Errorf("as_of=200 时生效边应为 e-old（当时它还成立），实际 %+v", group.Effective)
	}

	// as_of 非法 → 400。
	if status, body := h.get("/api/entity/alice?as_of=soon"); status != http.StatusBadRequest {
		t.Errorf("as_of 非法 status=%d, want 400 (body=%s)", status, body)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
