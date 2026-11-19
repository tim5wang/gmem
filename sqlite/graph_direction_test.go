package sqlite

import (
	"context"
	"fmt"
	"testing"

	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// F-8 回归：邻域遍历必须支持显式方向（outbound / inbound / both）
//
// 缺陷形状：递归 CTE 只走 from→to，于是 focus 一个"扇入型枢纽"
// （入边有、出边没有，例如 demo 里的 n-demo-acme）得到的是空邻域——
// 而可视化侧的 degree 是双向统计的，界面上就出现"圆圈很大却一根边都没有"。
//
// 修法：方向变成 NeighborQuery 的显式参数，零值 = outbound（召回侧行为不变）。
// 下面按任务卡的 A/B/C/性能四条各自断言**行为**，不碰实现细节。
// ===========================================================================

// seedWorksAt 造 A works_at B 的单向边（正是"扇入型枢纽"的最小形状）。
func seedWorksAt(t *testing.T, g store.GraphStore) {
	t.Helper()
	ctx := context.Background()
	for _, n := range []store.Node{
		{ID: "n-a", Scope: store.ScopeUser, CanonicalName: "Alice"},
		{ID: "n-b", Scope: store.ScopeUser, CanonicalName: "Acme"},
	} {
		if err := g.PutNode(ctx, n); err != nil {
			t.Fatalf("PutNode(%s): %v", n.ID, err)
		}
	}
	if err := g.PutEdge(ctx, store.Edge{
		ID: "e-a-b", FromID: "n-a", ToID: "n-b",
		Relation: store.RelationWorksAt, Weight: 1, TValid: 100, TCreated: 100,
	}); err != nil {
		t.Fatalf("PutEdge: %v", err)
	}
}

// TestFixF8HubFanInReturnsNeighbor【回归 A / 必需的不变量】
//
// focus 一个**只有入边**的节点（枢纽）时，both 方向必须返回邻居与边。
// 修前这里是 0 条边（整张图只剩它自己）——本用例就是钉死那一点的不变量。
func TestFixF8HubFanInReturnsNeighbor(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	seedWorksAt(t, d.Graph())

	hops, err := d.Graph().Neighbors(ctx, store.NeighborQuery{
		NodeID: "n-b", Scope: store.ScopeUser, Hops: 1,
		Direction: store.DirectionBoth,
	})
	if err != nil {
		t.Fatalf("Neighbors: %v", err)
	}
	if len(hops) != 1 || hops[0].Edge.ID != "e-a-b" {
		t.Fatalf("枢纽节点 both@1 跳应返回 1 条边（e-a-b），实际 %+v", hops)
	}
	if hops[0].Depth != 1 {
		t.Errorf("Depth = %d, want 1", hops[0].Depth)
	}
	// 前进目标是 e.from_id（n-a），不是起点自己：写错时这里会是 n-b，
	// 后果是起点被反复入栈，双向遍历失去终止性。
	if hops[0].Reached != "n-a" {
		t.Fatalf("inbound 方向的前进目标应为 e.from_id（n-a），实际 %q", hops[0].Reached)
	}
}

// TestFixF8DirectionSemantics【回归 B】三个方向各自的语义，独立用例。
func TestFixF8DirectionSemantics(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	seedWorksAt(t, d.Graph())

	cases := []struct {
		name      string
		from      string
		direction store.NeighborDirection
		wantEdges []string
	}{
		// 零值必须是 outbound：召回侧（recall/lanes.go）依赖它，不得静默改语义。
		{"零值=outbound，从扇入端出发看不到入边", "n-b", "", nil},
		{"outbound 显式，从扇入端出发看不到入边", "n-b", store.DirectionOutbound, nil},
		{"outbound，从出边端出发看得到", "n-a", store.DirectionOutbound, []string{"e-a-b"}},
		{"inbound，从出边端出发看不到出边", "n-a", store.DirectionInbound, nil},
		{"inbound，从扇入端出发看得到", "n-b", store.DirectionInbound, []string{"e-a-b"}},
		{"both，从出边端出发看得到", "n-a", store.DirectionBoth, []string{"e-a-b"}},
		{"both，从扇入端出发看得到", "n-b", store.DirectionBoth, []string{"e-a-b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hops, err := d.Graph().Neighbors(ctx, store.NeighborQuery{
				NodeID: tc.from, Scope: store.ScopeUser, Hops: 1, Direction: tc.direction,
			})
			if err != nil {
				t.Fatalf("Neighbors: %v", err)
			}
			got := make([]string, 0, len(hops))
			for _, h := range hops {
				got = append(got, h.Edge.ID)
			}
			if len(got) != len(tc.wantEdges) {
				t.Fatalf("方向 %q 从 %s 出发得到 %v，want %v", tc.direction, tc.from, got, tc.wantEdges)
			}
			for i := range got {
				if got[i] != tc.wantEdges[i] {
					t.Fatalf("方向 %q 从 %s 出发得到 %v，want %v", tc.direction, tc.from, got, tc.wantEdges)
				}
			}
		})
	}
}

// TestFixF8HopCounting【回归 C】双向下的多跳深度必须正确，且不得无限递归。
//
// 链 a -part_of-> b -part_of-> c -part_of-> d：
//   - inbound 从 d 出发 2 跳 = {c@1, b@2}
//   - both  从 c 出发 2 跳 = {d@1, b@1, a@2}
//
// 若双向把路径长度算错（例如按 min(depth) 全局截断，或前进目标写错），
// 这里会立刻看出深度不对。
func TestFixF8HopCounting(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	for _, id := range []string{"n-a", "n-b", "n-c", "n-d"} {
		if err := d.Graph().PutNode(ctx, store.Node{ID: id, Scope: store.ScopeUser, CanonicalName: id}); err != nil {
			t.Fatalf("PutNode(%s): %v", id, err)
		}
	}
	for i, e := range []store.Edge{
		{ID: "e1", FromID: "n-a", ToID: "n-b", Relation: store.RelationPartOf, Weight: 1, TValid: 100, TCreated: 100},
		{ID: "e2", FromID: "n-b", ToID: "n-c", Relation: store.RelationPartOf, Weight: 1, TValid: 100, TCreated: 100},
		{ID: "e3", FromID: "n-c", ToID: "n-d", Relation: store.RelationPartOf, Weight: 1, TValid: 100, TCreated: 100},
	} {
		if err := d.Graph().PutEdge(ctx, e); err != nil {
			t.Fatalf("PutEdge(%d): %v", i, err)
		}
	}

	depths := func(t *testing.T, q store.NeighborQuery) map[string]int {
		t.Helper()
		hops, err := d.Graph().Neighbors(ctx, q)
		if err != nil {
			t.Fatalf("Neighbors(%+v): %v", q, err)
		}
		out := map[string]int{}
		for _, h := range hops {
			out[h.Edge.ID] = h.Depth
			// 每一条被走过的边，Reached 必须是它真实的一端。
			if h.Reached != h.Edge.FromID && h.Reached != h.Edge.ToID {
				t.Fatalf("边 %s 的 Reached=%q 不是它的任何一端", h.Edge.ID, h.Reached)
			}
		}
		return out
	}

	t.Run("inbound 多跳深度", func(t *testing.T) {
		got := depths(t, store.NeighborQuery{
			NodeID: "n-d", Scope: store.ScopeUser, Hops: 2, Direction: store.DirectionInbound,
		})
		// 2 跳内从 d 反向可达 c（1）与 b（2）；a 是第 3 跳，不得出现。
		if len(got) != 2 || got["e3"] != 1 || got["e2"] != 2 {
			t.Fatalf("inbound@2 = %v, want {e3:1 e2:2}", got)
		}
	})

	t.Run("both 多跳深度", func(t *testing.T) {
		got := depths(t, store.NeighborQuery{
			NodeID: "n-c", Scope: store.ScopeUser, Hops: 2, Direction: store.DirectionBoth,
		})
		want := map[string]int{"e3": 1, "e2": 1, "e1": 2}
		if len(got) != len(want) {
			t.Fatalf("both@2 = %v, want %v", got, want)
		}
		for id, w := range want {
			if got[id] != w {
				t.Fatalf("both@2 = %v, want %v", got, want)
			}
		}
	})

	t.Run("outbound 多跳深度不得因新参数漂移", func(t *testing.T) {
		got := depths(t, store.NeighborQuery{
			NodeID: "n-a", Scope: store.ScopeUser, Hops: 2,
		})
		if len(got) != 2 || got["e1"] != 1 || got["e2"] != 2 {
			t.Fatalf("outbound@2 = %v, want {e1:1 e2:2}", got)
		}
	})
}

// TestFixF8BothTerminatesOnDenseBidirectionalGraph【性能与安全】
//
// 稠密双向图 + 超上限跳数：必须能终止，且结果规模被 (nodes × hops) 与
// maxHops 双重封住。若 UNION 去重键写坏（或前进目标写错导致起点反复入栈），
// 这里会挂死或返回规模爆炸的结果。
func TestFixF8BothTerminatesOnDenseBidirectionalGraph(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	const nodes = 40
	const fanout = 4
	edgeCount := 0
	for i := 0; i < nodes; i++ {
		if err := d.Graph().PutNode(ctx, store.Node{
			ID: fmt.Sprintf("n%d", i), Scope: store.ScopeUser, CanonicalName: fmt.Sprintf("N%d", i),
		}); err != nil {
			t.Fatalf("PutNode(%d): %v", i, err)
		}
	}
	for i := 0; i < nodes; i++ {
		for k := 1; k <= fanout; k++ {
			j := (i + k*3) % nodes
			if j == i {
				continue
			}
			// 单向写入（反向边由其它 i 的取值自然产生），但 part_of 在 both 方向下
			// 会被双向遍历，于是同一对节点之间产生大量长度 2 的环——正是要压的东西。
			edgeCount++
			if err := d.Graph().PutEdge(ctx, store.Edge{
				ID: fmt.Sprintf("e%d", edgeCount), FromID: fmt.Sprintf("n%d", i), ToID: fmt.Sprintf("n%d", j),
				Relation: store.RelationPartOf, Weight: 1, TValid: 100, TCreated: 100,
			}); err != nil {
				t.Fatalf("PutEdge: %v", err)
			}
		}
	}

	// Hops=99 必须被裁到 maxHops；both 方向不得因此失控。
	hops, err := d.Graph().Neighbors(ctx, store.NeighborQuery{
		NodeID: "n0", Scope: store.ScopeUser, Hops: 99, Direction: store.DirectionBoth,
	})
	if err != nil {
		t.Fatalf("Neighbors(both, dense): %v", err)
	}
	if len(hops) == 0 {
		t.Fatal("稠密双向图 both 方向不应返回空")
	}
	// 每条边最多出现一次（按边 ID 去重），因此结果规模 ≤ 边总数。
	if len(hops) > edgeCount {
		t.Fatalf("both 结果 %d 条超过边总数 %d：去重失效，递归在翻倍", len(hops), edgeCount)
	}
	for _, h := range hops {
		if h.Depth < 1 || h.Depth > maxHops {
			t.Fatalf("深度 %d 越界（合法区间 1..%d）", h.Depth, maxHops)
		}
	}
}

// TestFixF8InvalidDirectionRejected 未知方向必须报错，不得静默当成 outbound。
func TestFixF8InvalidDirectionRejected(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	seedWorksAt(t, d.Graph())

	if _, err := d.Graph().Neighbors(ctx, store.NeighborQuery{
		NodeID: "n-a", Scope: store.ScopeUser, Hops: 1, Direction: store.NeighborDirection("sideways"),
	}); err == nil {
		t.Fatal("未知方向被接受了（静默按某个默认值走，调用方无从察觉）")
	}
}
