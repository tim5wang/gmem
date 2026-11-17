package sqlite

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
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

// TestVectorSnapshot：快照必须能**逐字段解析回**写入的向量（行格式有契约意义）。
//
// 旧版只断言 len(snap) != 0，对另一 scope 取快照也不报错——即使实现把行格式写错
// （字段顺序错、长度前缀错、量化数据错）也照样 PASS。
// 现在按行格式 idLen(4) id dim(4) scale(8) dataLen(4) data 逐字段解码，
// 断言 id/dim/scale/data 与写入值一致，且 data 反量化后确实还原出原向量。
func TestVectorSnapshot(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	vals := []float32{0.5, -0.25, 1}
	if err := d.Vector().Upsert(ctx, []store.Vector{
		{ID: "v1", Scope: store.ScopeUser, Values: vals},
		{ID: "v0", Scope: store.ScopeUser, Values: []float32{-1, 0, 0}}, // id 升序在前
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	snap, err := d.Vector().Snapshot(ctx, store.ScopeUser)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	type row struct {
		id    string
		dim   int
		scale float64
		data  []byte
	}
	var rows []row
	buf := bytes.NewReader(snap)
	for buf.Len() > 0 {
		var idLen int32
		if err := binary.Read(buf, binary.LittleEndian, &idLen); err != nil {
			t.Fatalf("解析 idLen 失败（行格式不符）: %v", err)
		}
		if idLen <= 0 {
			t.Fatalf("idLen = %d, want > 0", idLen)
		}
		id := make([]byte, idLen)
		if _, err := io.ReadFull(buf, id); err != nil {
			t.Fatalf("解析 id 失败: %v", err)
		}
		var dim int32
		if err := binary.Read(buf, binary.LittleEndian, &dim); err != nil {
			t.Fatalf("解析 dim 失败: %v", err)
		}
		var scale float64
		if err := binary.Read(buf, binary.LittleEndian, &scale); err != nil {
			t.Fatalf("解析 scale 失败: %v", err)
		}
		var dataLen int32
		if err := binary.Read(buf, binary.LittleEndian, &dataLen); err != nil {
			t.Fatalf("解析 dataLen 失败: %v", err)
		}
		if dataLen <= 0 || int(dataLen) != int(dim) {
			t.Fatalf("dataLen = %d, dim = %d（逐分量 int8 存储时两者必须相等）", dataLen, dim)
		}
		data := make([]byte, dataLen)
		if _, err := io.ReadFull(buf, data); err != nil {
			t.Fatalf("解析 data 失败: %v", err)
		}
		rows = append(rows, row{id: string(id), dim: int(dim), scale: scale, data: data})
	}

	// 行数、顺序（ORDER BY id ASC）、字段值。
	if len(rows) != 2 {
		t.Fatalf("快照行数 = %d, want 2", len(rows))
	}
	if rows[0].id != "v0" || rows[1].id != "v1" {
		t.Fatalf("快照顺序 = [%s %s], want [v0 v1]（ORDER BY id ASC）", rows[0].id, rows[1].id)
	}

	got := dequantize(rows[1].scale, rows[1].data)
	if rows[1].dim != len(vals) {
		t.Errorf("v1 dim = %d, want %d", rows[1].dim, len(vals))
	}
	if len(got) != len(vals) {
		t.Fatalf("v1 反量化长度 = %d, want %d", len(got), len(vals))
	}
	// int8 量化误差：要求误差 <= scale/2，而不是"大概对"。
	for i := range vals {
		if diff := math.Abs(float64(got[i] - vals[i])); diff > rows[1].scale/2+1e-6 {
			t.Errorf("v1[%d] 反量化 = %v, want %v（误差 %v 超过 scale/2 = %v）",
				i, got[i], vals[i], diff, rows[1].scale/2)
		}
	}

	// 其它 scope 无向量：必须返回**空**快照，而不是非空垃圾。
	other, err := d.Vector().Snapshot(ctx, store.ScopeAgent)
	if err != nil {
		t.Fatalf("Snapshot other scope: %v", err)
	}
	if len(other) != 0 {
		t.Errorf("其它 scope 快照应为空，得到 %d 字节（跨 scope 泄漏）", len(other))
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
		NodeID: "a", Scope: store.ScopeUser, Relations: []store.Relation{"part_of"}, Hops: 1,
	})
	if err != nil {
		t.Fatalf("Neighbors: %v", err)
	}
	if len(hops) != 1 || hops[0].Edge.ID != "e1" || hops[0].Depth != 1 {
		t.Fatalf("1 hop = %+v, want [e1@1]", hops)
	}

	// 2 跳 + part_of：到 b 与 c。
	hops, err = g.Neighbors(ctx, store.NeighborQuery{
		NodeID: "a", Scope: store.ScopeUser, Relations: []store.Relation{"part_of"}, Hops: 2,
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
	hops, err = g.Neighbors(ctx, store.NeighborQuery{NodeID: "a", Scope: store.ScopeUser, Hops: 1})
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
	hops, err := g.Neighbors(ctx, store.NeighborQuery{
		NodeID: "a", Scope: store.ScopeUser, Hops: 1, AsOf: &at1500,
	})
	if err != nil {
		t.Fatalf("Neighbors as_of 1500: %v", err)
	}
	if len(hops) != 1 || hops[0].Edge.ID != "old" {
		t.Fatalf("as_of 1500 = %+v, want [old]", hops)
	}

	at2500 := int64(2500)
	hops, err = g.Neighbors(ctx, store.NeighborQuery{
		NodeID: "a", Scope: store.ScopeUser, Hops: 1, AsOf: &at2500,
	})
	if err != nil {
		t.Fatalf("Neighbors as_of 2500: %v", err)
	}
	if len(hops) != 1 || hops[0].Edge.ID != "new" {
		t.Fatalf("as_of 2500 = %+v, want [new]", hops)
	}
}

// TestGraphHopsClamped：Hops<=0 取 1；Hops 超上限时必须**真被裁到 maxHops**。
//
// 旧版只断言"不报错"，把 99 和 0 都跑一遍就算过——即使实现完全不裁剪也能通过。
// 现在用一条 10 跳直链：请求 Hops=99 只能走 5 跳（maxHops），Hops=0 只能走 1 跳。
func TestGraphHopsClamped(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	g := d.Graph()

	// 建 n0 -r-> n1 -r-> ... -r-> n10，共 10 条边。
	const chain = 10
	for i := 0; i <= chain; i++ {
		id := fmt.Sprintf("n%d", i)
		if err := g.PutNode(ctx, store.Node{ID: id, Scope: store.ScopeUser, CanonicalName: id}); err != nil {
			t.Fatalf("PutNode %s: %v", id, err)
		}
	}
	for i := 0; i < chain; i++ {
		if err := g.PutEdge(ctx, store.Edge{
			ID: fmt.Sprintf("e%d", i), FromID: fmt.Sprintf("n%d", i), ToID: fmt.Sprintf("n%d", i+1),
			Relation: "r", Weight: 1, TValid: 1, TCreated: 1,
		}); err != nil {
			t.Fatalf("PutEdge e%d: %v", i, err)
		}
	}

	maxDepth := func(hops []store.EdgeHop) int {
		m := 0
		for _, h := range hops {
			if h.Depth > m {
				m = h.Depth
			}
		}
		return m
	}

	// Hops<=0 视为 1：只能看到 1 条边。
	hops, err := g.Neighbors(ctx, store.NeighborQuery{NodeID: "n0", Scope: store.ScopeUser, Hops: 0})
	if err != nil {
		t.Fatalf("Hops=0: %v", err)
	}
	if len(hops) != 1 || maxDepth(hops) != 1 {
		t.Fatalf("Hops=0 应视为 1：得到 %d 条边 maxDepth=%d, want 1/1", len(hops), maxDepth(hops))
	}

	// Hops=99 必须被裁到 maxHops：恰好 maxHops 条边，最深深度 == maxHops。
	hops, err = g.Neighbors(ctx, store.NeighborQuery{NodeID: "n0", Scope: store.ScopeUser, Hops: 99})
	if err != nil {
		t.Fatalf("Hops=99: %v", err)
	}
	if got := maxDepth(hops); got != maxHops {
		t.Fatalf("Hops=99 未被裁到上限：maxDepth = %d, want %d（链长 %d，若未裁剪会是 %d）",
			got, maxHops, chain, chain)
	}
	if len(hops) != maxHops {
		t.Errorf("Hops=99 边数 = %d, want %d（直链每跳恰好 1 条）", len(hops), maxHops)
	}

	if _, err := g.Neighbors(ctx, store.NeighborQuery{NodeID: "", Scope: store.ScopeUser, Hops: 1}); err == nil {
		t.Fatal("empty node id accepted")
	}
}
