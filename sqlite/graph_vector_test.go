package sqlite

import (
	"context"
	"testing"

	"github.com/tim5wang/gmem/store"
)

// TestVectorSearchKNN：int8 量化后 KNN 仍能按余弦相似度排序。
func TestVectorSearchKNN(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	if err := d.Vector().Upsert(ctx, []store.Vector{
		{ID: "v1", Scope: store.ScopeUser, Values: []float32{1, 0, 0}},
		{ID: "v2", Scope: store.ScopeUser, Values: []float32{0.9, 0.1, 0}},
		{ID: "v3", Scope: store.ScopeUser, Values: []float32{0, 1, 0}},
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	hits, err := d.Vector().Search(ctx, []float32{1, 0, 0}, store.ScopeUser, 2)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits = %d, want 2", len(hits))
	}
	if hits[0].ID != "v1" {
		t.Fatalf("top = %s, want v1 (%+v)", hits[0].ID, hits)
	}
	if hits[0].Rank != 1 || hits[1].Rank != 2 {
		t.Fatalf("ranks = %d,%d, want 1,2", hits[0].Rank, hits[1].Rank)
	}
}

// TestVectorSnapshot：快照非空且可重复获取（可重建的加速物）。
func TestVectorSnapshot(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	if err := d.Vector().Upsert(ctx, []store.Vector{
		{ID: "v1", Scope: store.ScopeUser, Values: []float32{0.5, -0.25}},
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	snap, err := d.Vector().Snapshot(ctx, store.ScopeUser)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snap) == 0 {
		t.Fatal("empty snapshot")
	}
	if _, err := d.Vector().Snapshot(ctx, store.ScopeAgent); err != nil {
		t.Fatalf("Snapshot other scope: %v", err)
	}
}

// TestGraphNeighborsHops：递归 CTE 按跳数定向遍历，白名单过滤关系。
func TestGraphNeighborsHops(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	g := d.Graph()

	for _, id := range []string{"a", "b", "c", "d"} {
		if err := g.PutNode(ctx, store.Node{ID: id, Scope: store.ScopeUser, CanonicalName: id}); err != nil {
			t.Fatalf("PutNode %s: %v", id, err)
		}
	}
	// 链 a -part_of-> b -part_of-> c，旁支 a -works_at-> d
	edges := []store.Edge{
		{ID: "e1", FromID: "a", ToID: "b", Relation: "part_of", Weight: 1, TValid: 100},
		{ID: "e2", FromID: "b", ToID: "c", Relation: "part_of", Weight: 1, TValid: 100},
		{ID: "e3", FromID: "a", ToID: "d", Relation: "works_at", Weight: 1, TValid: 100},
	}
	for _, e := range edges {
		if err := g.PutEdge(ctx, e); err != nil {
			t.Fatalf("PutEdge %s: %v", e.ID, err)
		}
	}

	// 1 跳：只看 part_of → 只有 e1。
	hops, err := g.Neighbors(ctx, store.NeighborQuery{
		NodeID: "a", Relations: []store.Relation{"part_of"}, Hops: 1,
	})
	if err != nil {
		t.Fatalf("Neighbors: %v", err)
	}
	if len(hops) != 1 || hops[0].Edge.ID != "e1" || hops[0].Depth != 1 {
		t.Fatalf("1 hop = %+v, want [e1@1]", hops)
	}

	// 2 跳 + part_of：到 b 与 c。
	hops, err = g.Neighbors(ctx, store.NeighborQuery{
		NodeID: "a", Relations: []store.Relation{"part_of"}, Hops: 2,
	})
	if err != nil {
		t.Fatalf("Neighbors 2 hops: %v", err)
	}
	gotDepths := map[string]int{}
	for _, h := range hops {
		gotDepths[h.Edge.ID] = h.Depth
	}
	if gotDepths["e1"] != 1 || gotDepths["e2"] != 2 {
		t.Fatalf("2 hop depths = %+v, want e1@1 e2@2", gotDepths)
	}

	// 不限关系：1 跳也能拿到 works_at。
	hops, err = g.Neighbors(ctx, store.NeighborQuery{NodeID: "a", Hops: 1})
	if err != nil {
		t.Fatalf("Neighbors no filter: %v", err)
	}
	if len(hops) != 2 {
		t.Fatalf("no filter 1 hop = %d edges, want 2", len(hops))
	}
}

// TestGraphNeighborsBitemporal：as_of 只看该时刻有效的边。
func TestGraphNeighborsBitemporal(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	g := d.Graph()

	for _, id := range []string{"a", "b", "c"} {
		if err := g.PutNode(ctx, store.Node{ID: id, Scope: store.ScopeUser, CanonicalName: id}); err != nil {
			t.Fatalf("PutNode: %v", err)
		}
	}
	if err := g.PutEdge(ctx, store.Edge{
		ID: "old", FromID: "a", ToID: "b", Relation: "works_at",
		Weight: 1, TValid: 1000, TInvalid: 2000,
	}); err != nil {
		t.Fatalf("PutEdge old: %v", err)
	}
	if err := g.PutEdge(ctx, store.Edge{
		ID: "new", FromID: "a", ToID: "c", Relation: "works_at",
		Weight: 1, TValid: 2000,
	}); err != nil {
		t.Fatalf("PutEdge new: %v", err)
	}

	at1500 := int64(1500)
	hops, err := g.Neighbors(ctx, store.NeighborQuery{NodeID: "a", Hops: 1, AsOf: &at1500})
	if err != nil {
		t.Fatalf("Neighbors as_of 1500: %v", err)
	}
	if len(hops) != 1 || hops[0].Edge.ID != "old" {
		t.Fatalf("as_of 1500 = %+v, want [old]", hops)
	}

	at2500 := int64(2500)
	hops, err = g.Neighbors(ctx, store.NeighborQuery{NodeID: "a", Hops: 1, AsOf: &at2500})
	if err != nil {
		t.Fatalf("Neighbors as_of 2500: %v", err)
	}
	if len(hops) != 1 || hops[0].Edge.ID != "new" {
		t.Fatalf("as_of 2500 = %+v, want [new]", hops)
	}
}

// TestGraphHopsClamped：Hops<=0 取 1，超过上限被裁到 maxHops（不报错）。
func TestGraphHopsClamped(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	g := d.Graph()
	if err := g.PutNode(ctx, store.Node{ID: "a", Scope: store.ScopeUser, CanonicalName: "a"}); err != nil {
		t.Fatalf("PutNode: %v", err)
	}
	if _, err := g.Neighbors(ctx, store.NeighborQuery{NodeID: "a", Hops: 0}); err != nil {
		t.Fatalf("Hops=0: %v", err)
	}
	if _, err := g.Neighbors(ctx, store.NeighborQuery{NodeID: "a", Hops: 99}); err != nil {
		t.Fatalf("Hops=99: %v", err)
	}
	if _, err := g.Neighbors(ctx, store.NeighborQuery{NodeID: "", Hops: 1}); err == nil {
		t.Fatal("empty node id accepted")
	}
}
