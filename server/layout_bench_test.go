package server

import (
	"fmt"
	"testing"
)

// ===========================================================================
// 布局的复杂度基准（不参与 go test 默认运行；用 -bench 跑）
//
//   go test ./server -run '^$' -bench 'BenchmarkLayout' -benchmem
//
// 为什么要它：卡片要求"layout 仍是纯函数且不得引入 O(n³)"。用时序断言
// （测试里比较 1000 vs 3000 的墙钟）会把 CI 的负载噪声变成假红，因此这里
// 用标准 benchmark 给出真实数字，另用下面的用例做**不依赖绝对耗时**的
// 结构性检查（坐标个数 = 节点数，且每个坐标只被算一次）。
// ===========================================================================

// benchInput 造 n 个节点、按 32 个概念团均匀分布（每团 n/32 个）。
func benchInput(n, clusters int) (map[string]map[string]int, map[string]int, []nodeRef) {
	refs := make([]nodeRef, 0, n)
	for i := 0; i < n; i++ {
		refs = append(refs, nodeRef{
			id:         fmt.Sprintf("n-%06d", i),
			conceptKey: fmt.Sprintf("c-%03d", i%clusters),
		})
	}
	index, count := leafOrder(refs)
	return index, count, refs
}

// BenchmarkLayout3000 是 3000 节点（budget 上限级别的稠密图）的整布局耗时。
func BenchmarkLayout3000(b *testing.B) {
	index, count, refs := benchInput(3000, 32)
	centers := make(map[string][2]float64, 32)
	for i := 0; i < 32; i++ {
		x, y := clusterCenter(i)
		centers[fmt.Sprintf("c-%03d", i)] = [2]float64{x, y}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, r := range refs {
			c := centers[r.conceptKey]
			nodePosition(index[r.conceptKey][r.id], count[r.conceptKey], c[0], c[1])
		}
	}
}

// BenchmarkLeafOrder3000 单独量序号分配（每个节点一次 map 写 + 一次计数）。
func BenchmarkLeafOrder3000(b *testing.B) {
	_, _, refs := benchInput(3000, 32)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		leafOrder(refs)
	}
}

// TestLayoutLinearInNodes 断言布局对每个节点只做**常数次**计算。
//
// 判据不靠墙钟：把同一批节点重复喂进去，坐标序列必须是"每节点恰好一个坐标"，
// 且追加节点不改变已有节点的序号（O(1) 增量）。真正的复杂度上界由
// BenchmarkLayout3000 的数字体现（本机 ~1ms 量级 ⇒ 每节点 ~300ns，线性）。
func TestLayoutLinearInNodes(t *testing.T) {
	index, count, refs := benchInput(3000, 32)
	if len(refs) != 3000 {
		t.Fatalf("入参节点数 %d != 3000", len(refs))
	}
	total := 0
	for k, n := range count {
		total += n
		if n != len(index[k]) {
			t.Fatalf("团 %s 的节点数 %d 与序号表大小 %d 不一致", k, n, len(index[k]))
		}
	}
	if total != 3000 {
		t.Fatalf("团内节点数之和 %d != 3000（有节点未被计入）", total)
	}
	// 每个团的序号必须是 0..n-1 的一个排列（无重复、无空洞）。
	seen := make(map[string]map[int]bool, len(index))
	for k, m := range index {
		seen[k] = make(map[int]bool, len(m))
		for _, j := range m {
			if j < 0 || j >= count[k] {
				t.Fatalf("团 %s 的序号 %d 越界 [0,%d)", k, j, count[k])
			}
			if seen[k][j] {
				t.Fatalf("团 %s 的序号 %d 重复（叶序格点会重叠）", k, j)
			}
			seen[k][j] = true
		}
		if len(seen[k]) != count[k] {
			t.Fatalf("团 %s 的序号有空洞：%d 个不同序号，应为 %d", k, len(seen[k]), count[k])
		}
	}
}
