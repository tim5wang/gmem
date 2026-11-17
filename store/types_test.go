package store

import (
	"strings"
	"testing"
)

func TestEnumValid(t *testing.T) {
	for _, s := range []Scope{ScopeUser, ScopeAgent, ScopeProject, ScopeGlobal} {
		if !s.Valid() {
			t.Errorf("scope %q should be valid", s)
		}
	}
	if Scope("bogus").Valid() {
		t.Error("bogus scope accepted")
	}

	for _, k := range []MemoryKind{KindSemantic, KindEpisodic, KindProcedural, KindDecision} {
		if !k.Valid() {
			t.Errorf("kind %q should be valid", k)
		}
	}
	if MemoryKind("bogus").Valid() {
		t.Error("bogus kind accepted")
	}

	for _, c := range []ConceptStatus{ConceptDraft, ConceptCertified, ConceptDeprecated} {
		if !c.Valid() {
			t.Errorf("status %q should be valid", c)
		}
	}
	if ConceptStatus("bogus").Valid() {
		t.Error("bogus concept status accepted")
	}
}

func TestValidate(t *testing.T) {
	good := struct {
		ep   Episode
		mem  Memory
		conc Concept
		node Node
		edge Edge
		al   Alias
	}{
		ep:   Episode{ID: "e", Scope: ScopeUser, Content: "c"},
		mem:  Memory{ID: "m", Scope: ScopeUser, Kind: KindSemantic, Content: "c"},
		conc: Concept{ID: "c", Name: "n", Status: ConceptDraft},
		node: Node{ID: "n", Scope: ScopeUser, CanonicalName: "name"},
		edge: Edge{ID: "e", FromID: "a", ToID: "b", Relation: Relation("part_of")},
		al:   Alias{Surface: "s", NodeID: "n"},
	}
	for name, err := range map[string]error{
		"episode": good.ep.Validate(),
		"memory":  good.mem.Validate(),
		"concept": good.conc.Validate(),
		"node":    good.node.Validate(),
		"edge":    good.edge.Validate(),
		"alias":   good.al.Validate(),
	} {
		if err != nil {
			t.Errorf("%s: valid input rejected: %v", name, err)
		}
	}

	bad := map[string]error{
		"episode no id":      Episode{Scope: ScopeUser, Content: "c"}.Validate(),
		"episode bad scope":  Episode{ID: "e", Scope: "bogus", Content: "c"}.Validate(),
		"episode no content": Episode{ID: "e", Scope: ScopeUser}.Validate(),
		"memory bad kind":    Memory{ID: "m", Scope: ScopeUser, Kind: "bogus", Content: "c"}.Validate(),
		"concept no name":    Concept{ID: "c", Status: ConceptDraft}.Validate(),
		"concept bad status": Concept{ID: "c", Name: "n", Status: "bogus"}.Validate(),
		"node no name":       Node{ID: "n", Scope: ScopeUser}.Validate(),
		"edge no relation":   Edge{ID: "e", FromID: "a", ToID: "b"}.Validate(),
		"edge no endpoint":   Edge{ID: "e", FromID: "a", Relation: Relation("r")}.Validate(),
		"alias no surface":   Alias{NodeID: "n"}.Validate(),
	}
	for name, err := range bad {
		if err == nil {
			t.Errorf("%s: invalid input accepted", name)
		}
	}
}

// TestValidateErrorsMentionField：错误信息要能定位到字段，便于上层降级上报。
func TestValidateErrorsMentionField(t *testing.T) {
	err := Memory{ID: "m", Scope: ScopeUser, Kind: "bogus", Content: "c"}.Validate()
	if err == nil || !strings.Contains(err.Error(), "kind") {
		t.Fatalf("err = %v, want it to mention kind", err)
	}
}

// TestRanked：Ranked 生成 1-based rank，score 缺失时补 0，供 RRF 直接消费。
func TestRanked(t *testing.T) {
	hits := Ranked([]string{"a", "b"}, []float64{0.9})
	if len(hits) != 2 {
		t.Fatalf("hits = %d, want 2", len(hits))
	}
	if hits[0].Rank != 1 || hits[1].Rank != 2 {
		t.Fatalf("ranks = %d,%d, want 1,2", hits[0].Rank, hits[1].Rank)
	}
	if hits[1].Score != 0 {
		t.Fatalf("missing score = %v, want 0", hits[1].Score)
	}
	if len(Ranked(nil, nil)) != 0 {
		t.Fatal("nil input should yield empty hits")
	}
}
