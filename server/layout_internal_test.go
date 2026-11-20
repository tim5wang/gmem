package server

import (
	"fmt"
	"math"
	"sort"
	"testing"
)

// ===========================================================================
// 布局纯函数的不变量（包内测试：直接吃未导出的 clusterCenter / nodePosition）
//
// 为什么这些断言必须存在：布局的目标是"松散均匀"，而"看起来还行"是可以用
// 一张截图骗过去的。这里把三条几何结论写成数字：
//   A) 任意两团的中心距 > 两团叶序半径之和 + 间隙（团不重叠）
//   B) 团内最近邻距离的"最大/最小"比 ≤ 3（叶序均匀；旧随机极坐标是 5~10）
//   C) 坐标对同一 (index,count,center) 逐字节一致（确定性）
// ===========================================================================

// TestClusterCentersNeverOverlap 造 8 个团（每团 3~30 个节点），断言团心间距。
//
// 断言用**每团自己的 R(n)**，不是常量：R 随节点数增长，如果只看一个大 R
// 就发现不了"小团配大间距、大团配小间距"这类配对错误。
func TestClusterCentersNeverOverlap(t *testing.T) {
	const clusters = 8
	// 每团节点数在 3~30 之间变化，覆盖 R(n) 的整段（含封顶前的最大处）。
	counts := make([]int, clusters)
	for i := range counts {
		counts[i] = 3 + i*27/(clusters-1)
	}

	type placed struct {
		cx, cy, r float64
	}
	pos := make([]placed, clusters)
	for i := 0; i < clusters; i++ {
		cx, cy := clusterCenter(i)
		pos[i] = placed{cx: cx, cy: cy, r: clusterRadius(counts[i])}
	}
	for i := 0; i < clusters; i++ {
		for j := i + 1; j < clusters; j++ {
			d := math.Hypot(pos[i].cx-pos[j].cx, pos[i].cy-pos[j].cy)
			need := pos[i].r + pos[j].r + clusterGap
			if d <= need {
				t.Errorf("团 %d 与团 %d 重叠：中心距 %.4f ≤ R%d(%.3f)+R%d(%.3f)+间隙 %.2f = %.4f",
					i, j, d, i, pos[i].r, j, pos[j].r, clusterGap, need)
			}
		}
	}

	// 每团内部的节点两两距离必须 > 0.02（点不叠在一起）。
	for i := 0; i < clusters; i++ {
		cx, cy := clusterCenter(i)
		n := counts[i]
		pts := make([][2]float64, n)
		for j := 0; j < n; j++ {
			x, y := nodePosition(j, n, cx, cy)
			pts[j] = [2]float64{x, y}
		}
		for a := 0; a < n; a++ {
			for b := a + 1; b < n; b++ {
				d := math.Hypot(pts[a][0]-pts[b][0], pts[a][1]-pts[b][1])
				if d <= 0.02 {
					t.Errorf("团 %d 内节点 %d/%d 距离 %.5f ≤ 0.02（扎堆）", i, a, b, d)
				}
			}
		}
	}
}

// TestClusterCenterSpacingCalibrated 把"基准半径必须 ≥ 0.87"这条校准钉死。
//
// 历史缺陷：radius = 0.55*sqrt(i+0.5) 时 d(0,1) = 0.674，而两团最长散布半径
// 之和 = 0.68 —— 最内两团必然重叠（截图里 Atlas / Beta Labs / Borealis 堆在一起）。
// 这里直接断言最坏情形：最小团心距 > 2*clusterRadiusMax + clusterGap。
func TestClusterCenterSpacingCalibrated(t *testing.T) {
	// 团心最小间距出现在 i=0/1 之间（螺旋半径的增幅在 i 小的时候最大）。
	// 但为稳妥起见，扫前 64 个团取真正的最小值。
	const n = 64
	minD := math.Inf(1)
	for i := 0; i < n; i++ {
		xi, yi := clusterCenter(i)
		for j := i + 1; j < n; j++ {
			xj, yj := clusterCenter(j)
			if d := math.Hypot(xi-xj, yi-yj); d < minD {
				minD = d
			}
		}
	}
	need := 2*clusterRadiusMax + clusterGap
	if minD <= need {
		t.Fatalf("团心最小间距 %.4f ≤ 2*Rmax(%.2f)+间隙(%.2f)=%.4f：最大团仍会与他人重叠",
			minD, clusterRadiusMax, clusterGap, need)
	}
	// 旧公式的值必须已经被甩开，否则说明系数没被真正上调。
	if minD <= 0.68 {
		t.Fatalf("团心最小间距 %.4f 没有超过旧公式的 0.674（调参未生效）", minD)
	}
}

// TestClusterCenterStableOnNewConcept 新增一个概念不得让已有团心位移。
//
// 判据：按序号取点（黄金角螺旋），因此 i 不变 ⇒ 坐标不变。这里断言
// 概念键集合扩大（新键排在任何已有键之后）时，前 k 个团心逐字节一致。
func TestClusterCenterStableOnNewConcept(t *testing.T) {
	// 先算"只有 3 个概念"时的团心，再算"多一个排最后的新概念"时的团心。
	base := clusterKeys([]nodeRef{
		{id: "n1", conceptKey: "c-1"},
		{id: "n2", conceptKey: "c-2"},
		{id: "n3", conceptKey: "c-3"},
	})
	grown := clusterKeys([]nodeRef{
		{id: "n1", conceptKey: "c-1"},
		{id: "n2", conceptKey: "c-2"},
		{id: "n3", conceptKey: "c-3"},
		{id: "n4", conceptKey: "c-9"}, // 排序后落在最后，不影响前三个的序号
	})
	if len(base) != 3 || len(grown) != 4 {
		t.Fatalf("聚簇键数量不符：base=%v grown=%v", base, grown)
	}
	for i, k := range base {
		if grown[i] != k {
			t.Fatalf("新概念改变了已有概念的序号：base[%d]=%s grown[%d]=%s", i, k, i, grown[i])
		}
		x1, y1 := clusterCenter(i)
		x2, y2 := clusterCenter(i)
		if x1 != x2 || y1 != y2 {
			t.Fatalf("团 %d 的团心不稳定：%.6f,%.6f vs %.6f,%.6f", i, x1, y1, x2, y2)
		}
	}
	// 团心只依赖序号：同一序号必得同一坐标（这就是"新增概念不位移已有团心"的根）。
	x1, y1 := clusterCenter(2)
	x2, y2 := clusterCenter(2)
	if x1 != x2 || y1 != y2 {
		t.Fatal("团心不是纯函数")
	}
}

// TestClusterKeysSorted 顺带钉死聚簇键必须排序（团心序号的前提）。
func TestClusterKeysSorted(t *testing.T) {
	keys := clusterKeys([]nodeRef{
		{id: "n1", conceptKey: "c-3"},
		{id: "n2", conceptKey: unclusteredKey},
		{id: "n3", conceptKey: "c-1"},
	})
	if !sort.StringsAreSorted(keys) {
		t.Fatalf("聚簇键未排序：%v", keys)
	}
	if keys[0] != unclusteredKey {
		t.Fatalf("未归属概念必须排在最前（沿用控制字符前缀），实际 %q", keys[0])
	}
}

// TestLeafOrderNearestNeighbourUniform 单团 30 个节点：最近邻距离最大/最小比 ≤ 3。
//
// 变异验证（变异 1：把角度换回 hash 随机角，即旧实现）会产生 5~10 的比值，
// 本断言立刻变红 —— 见 TestOldRandomPolarIsNotUniform（同一口径下的对照组）。
func TestLeafOrderNearestNeighbourUniform(t *testing.T) {
	const n = 30
	cx, cy := clusterCenter(0)
	pts := make([][2]float64, n)
	for j := 0; j < n; j++ {
		x, y := nodePosition(j, n, cx, cy)
		pts[j] = [2]float64{x, y}
	}
	minD, maxD := nnStats(pts)
	ratio := maxD / minD
	t.Logf("30 节点叶序：最近邻 min=%.4f max=%.4f 比值=%.3f", minD, maxD, ratio)
	if ratio > 3.0 {
		t.Fatalf("团内散布不均匀：最近邻最大/最小 = %.3f > 3.0（min=%.4f max=%.4f）", ratio, minD, maxD)
	}
	if minD <= 0.02 {
		t.Fatalf("最近邻距离 %.4f ≤ 0.02：节点重叠", minD)
	}
}

// TestOldRandomPolarIsNotUniform 是**对照组**：用旧实现的口径（hash 随机角度 +
// hash 随机半径）在同一断言下必须变红。
//
// 它的价值不是"测旧代码"，而是证明上面那条 <=3.0 的阈值确实有鉴别力：
// 若阈值松到连旧实现都过，那条断言就等于没写。
func TestOldRandomPolarIsNotUniform(t *testing.T) {
	const n = 30
	pts := make([][2]float64, n)
	for j := 0; j < n; j++ {
		// 旧实现的坐标公式（保留在此仅作对照，不参与生产路径）。
		id := fmt.Sprintf("n%02d", j)
		angle := 2 * math.Pi * legacyHashUnit(id, 0x11)
		radius := 0.12 + 0.22*math.Sqrt(legacyHashUnit(id, 0x22))
		pts[j] = [2]float64{radius * math.Cos(angle), radius * math.Sin(angle)}
	}
	minD, maxD := nnStats(pts)
	ratio := maxD / minD
	t.Logf("旧随机极坐标：最近邻 min=%.4f max=%.4f 比值=%.3f", minD, maxD, ratio)
	if ratio <= 3.0 {
		t.Fatalf("对照组失效：旧随机极坐标的比值 %.3f 也 ≤ 3.0，"+
			"说明 TestLeafOrderNearestNeighbourUniform 的阈值没有鉴别力", ratio)
	}
}

// TestClusterRadiusGrowsWithCount 团内半径必须随节点数增长（且封顶）。
func TestClusterRadiusGrowsWithCount(t *testing.T) {
	prev := 0.0
	for _, n := range []int{1, 2, 3, 5, 9, 20, 30, 38} {
		r := clusterRadius(n)
		if r < prev {
			t.Fatalf("R(%d)=%.4f 小于更小规模的 R=%.4f（半径未随 n 增长）", n, r, prev)
		}
		prev = r
	}
	// 20 个节点若用 10 个节点的半径，点距会腰斩 —— 直接对比两档的最近邻距离。
	d20 := minNeighbour(clusterCenter, 20)
	d10 := minNeighbour(clusterCenter, 10)
	if d20 <= d10*0.55 {
		t.Fatalf("20 节点最近邻 %.4f 相对 10 节点 %.4f 塌缩过多（R 增长不足）", d20, d10)
	}
	if clusterRadius(1000) != clusterRadiusMax {
		t.Fatalf("大团半径必须封顶在 %.2f（否则必然与邻团重叠），实际 %.4f", clusterRadiusMax, clusterRadius(1000))
	}
}

// TestNodePositionDeterministic 同一 (index,count,center) 必得同一坐标，且落在团内。
func TestNodePositionDeterministic(t *testing.T) {
	cx, cy := clusterCenter(3)
	for j := 0; j < 12; j++ {
		x1, y1 := nodePosition(j, 12, cx, cy)
		x2, y2 := nodePosition(j, 12, cx, cy)
		if x1 != x2 || y1 != y2 {
			t.Fatalf("节点 %d 坐标不稳定：%.6f,%.6f vs %.6f,%.6f", j, x1, y1, x2, y2)
		}
		if d := math.Hypot(x1-cx, y1-cy); d > clusterRadius(12)+1e-6 {
			t.Fatalf("节点 %d 跑出团半径：%.5f > %.5f", j, d, clusterRadius(12))
		}
		// 舍入到 1e-6（roundCoord 的契约）。
		if math.Round(x1*1e6)/1e6 != x1 || math.Round(y1*1e6)/1e6 != y1 {
			t.Fatalf("节点 %d 坐标未舍入到 1e-6：%.9f,%.9f", j, x1, y1)
		}
	}
}

// TestLeafOrderIsByNodeID 团内序号必须等于"按节点 ID 排序后的下标"。
func TestLeafOrderIsByNodeID(t *testing.T) {
	refs := []nodeRef{
		{id: "a", conceptKey: "k1"},
		{id: "b", conceptKey: "k2"},
		{id: "c", conceptKey: "k1"},
		{id: "d", conceptKey: "k1"},
	}
	index, count := leafOrder(refs)
	if count["k1"] != 3 || count["k2"] != 1 {
		t.Fatalf("团内节点数统计错误：%+v", count)
	}
	if index["k1"]["a"] != 0 || index["k1"]["c"] != 1 || index["k1"]["d"] != 2 {
		t.Fatalf("团内序号必须按 ID 升序：%+v", index["k1"])
	}
	if index["k2"]["b"] != 0 {
		t.Fatalf("单节点团的序号必须是 0：%+v", index["k2"])
	}
}

// ---- 测试辅助 -------------------------------------------------------------

// nnStats 返回最近邻距离的 (最小, 最大)。
func nnStats(pts [][2]float64) (float64, float64) {
	minD := math.Inf(1)
	maxD := 0.0
	for i, p := range pts {
		best := math.Inf(1)
		for j, q := range pts {
			if i == j {
				continue
			}
			if d := math.Hypot(p[0]-q[0], p[1]-q[1]); d < best {
				best = d
			}
		}
		if best < minD {
			minD = best
		}
		if best > maxD {
			maxD = best
		}
	}
	return minD, maxD
}

// minNeighbour 返回 n 个叶序节点的最小最近邻距离。
func minNeighbour(center func(int) (float64, float64), n int) float64 {
	cx, cy := center(0)
	pts := make([][2]float64, n)
	for j := 0; j < n; j++ {
		x, y := nodePosition(j, n, cx, cy)
		pts[j] = [2]float64{x, y}
	}
	minD, _ := nnStats(pts)
	return minD
}

// legacyHashUnit 是旧实现（被替换前）的 hash 伪随机数，仅供对照组使用。
func legacyHashUnit(key string, salt byte) float64 {
	// FNV-1a 64，与替换前 server/layout.go 的实现逐字节等价。
	var h uint64 = 0xcbf29ce484222325
	h ^= uint64(salt)
	h *= 0x100000001b3
	for i := 0; i < len(key); i++ {
		h ^= uint64(key[i])
		h *= 0x100000001b3
	}
	return float64(h%1000003) / 1000003
}

// TestLayoutIsPureNoPackageState 断言 layout 的入口不带包级可变状态。
//
// 做法：同一组入参连调两次，中间插入一次"别的团"的计算，结果必须一致。
func TestLayoutIsPureNoPackageState(t *testing.T) {
	cx, cy := clusterCenter(2)
	first := make([][2]float64, 9)
	for j := range first {
		x, y := nodePosition(j, len(first), cx, cy)
		first[j] = [2]float64{x, y}
	}
	// 中间做一堆无关计算（如果哪天真引入了包级缓存/游标，这里会露馅）。
	for i := 0; i < 5; i++ {
		clusterCenter(i)
		nodePosition(i, 7, 0, 0)
		clusterRadius(i + 3)
	}
	for j := range first {
		x, y := nodePosition(j, len(first), cx, cy)
		if x != first[j][0] || y != first[j][1] {
			t.Fatalf("nodePosition 受包级状态影响：节点 %d %.6f,%.6f vs %.6f,%.6f",
				j, x, y, first[j][0], first[j][1])
		}
	}
}
