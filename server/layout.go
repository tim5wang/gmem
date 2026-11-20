package server

import (
	"math"
	"sort"
)

// ===========================================================================
// 预布局坐标：纯函数，不落库（TD §9「返回预布局坐标缓存，避免前端抖动」）
//
// # 为什么不建表缓存
//
// 坐标是 (nodeID, conceptID, 节点在团内按 ID 排序的序号, 团内节点数) 的确定性
// 函数：同一输入必得同一输出。为它加一张缓存表（migration v10）只会引入
// "缓存过期/多实例不一致"这类新故障面，换来的仅仅是省掉一次 O(n) 的纯计算
// （n 已被 budget 约束在 2000 以内）。
// 因此本卡**不新增任何 DB 表**：确定性由排序序号 + 黄金角常量保证，
// 不依赖时间、随机数与进程状态。
//
// # 布局形状（v2：团间距与团内散布都改成格点）
//
//  1. 团心 = 黄金角螺旋 r_i = clusterBaseRadius * sqrt(i+0.5)（i 是概念键排序后的序号）。
//     任意两团心的最小距离是 d(0,1) = 0.9*sqrt(1.5) ≈ 1.1023（数值验证见 layout_test.go）。
//  2. 团内 = 离心叶序格点：第 j 个节点 r_j = leafStep*sqrt(j+0.5)（封顶 clusterRadiusMax），
//     theta_j = j*goldenAngle。相邻节点间距近似相等，不会出现"随机极坐标"必然产生的
//     局部聚簇与空洞。离心项 +0.5 保证最内圈也离团心 leafStep*sqrt(0.5) > 0（旧的 j/n
//     让 j=0 落在团心，正好被前端画在团心的团标签压住）；r_j 只依赖 j，不依赖 n。
//  3. 团半径 R 随节点数增长（R = 0.10 + 0.055*sqrt(n)），但**封顶**在 clusterRadiusMax：
//     封顶是与团心间距联动的必要条件 —— 团心只依赖序号 i（新增概念不位移已有团心），
//     所以不封顶就无法保证 "dist(ci,cj) > Ri + Rj + 间隙"。
//
// 为什么团内地址要用叶序而不是哈希随机角：
// 随机（角度、半径）落在圆盘上的分布密度是 r² 相关的，30 个点里必然出现
// 一堆点挤在一起 + 一块空地（实测最近邻距离最大/最小比 5~10 倍）。叶序把点
// 按面积均匀铺开（同口径比值 ≈ 1.8），这才是"松散均匀"的来源。
// ===========================================================================

// goldenAngle 是黄金角（弧度）。按序号 × 黄金角取点，任何前缀都是均匀散布，
// 因此团心与团内叶序都不依赖总数——新增概念不会让已有团心位移。
const goldenAngle = 2.399963229728653

// unclusteredKey 是"节点没有 concept"时的聚簇键。
// 用控制字符开头，保证它排在所有真实 concept ID 之前（确定性排序）。
const unclusteredKey = "\x00unclustered"

// unclusteredLabel 是该簇的展示名。
const unclusteredLabel = "(未归属概念)"

const (
	// clusterBaseRadius 是团心螺旋的基准半径（不能低于 0.87：见 clusterGap 的推导）。
	clusterBaseRadius = 0.9
	// clusterRadiusBase / clusterRadiusPerNode 决定团内叶序的**名义**半径 R(n)。
	// 注意：节点落点已不再乘 R(n)（见 nodePosition：落点必须与 count 无关），
	// R(n) 现在只作为“团容量/间距校准”的参考量，被布局不变量用例钉死。
	clusterRadiusBase    = 0.10
	clusterRadiusPerNode = 0.055
	// clusterRadiusMax 是团外沿的上限。约束来自团心最小间距：
	//   d_min ≈ 1.1023  >=  2*clusterRadiusMax + clusterGap = 0.88 + 0.12 = 1.00 ✓
	clusterRadiusMax = 0.44
	// leafStep 是叶序半径的步长（世界坐标）。第 j 个节点的半径是
	//   r_j = leafStep * sqrt(j+0.5)，**只依赖团内序号 j，不依赖团内节点数**。
	// 为什么必须与 count 无关：否则团里新增一个实体，已有节点全部跟着位移
	// （TestCoordsDependOnlyOnNodeID 守的正是这条：不能“加一条记忆就整图重排”）。
	// 取值 0.08：r_0 = 0.0566 > 0（不落团心）；封顶前 n=30 时 r_max = 0.4345 < 0.44。
	leafStep = 0.08
	// clusterGap 是任意两团之间必须保留的最低间隙（世界坐标）。
	clusterGap = 0.12
)

// roundCoord 把坐标固定到 1e-6。
//
// 为什么要舍入：JSON 序列化 float64 已经是确定性的，但中间计算若引入
// 平台相关的 FMA 差异，最后一位可能不同——那会让"逐字节一致"的回归用例变脆。
// 舍入到 1e-6 之后，坐标是稳定可比的字面量。
func roundCoord(v float64) float64 {
	return math.Round(v*1e6) / 1e6
}

// clusterCenter 返回第 i 个概念团的团心坐标（i 是概念键排序后的序号）。
//
// 只依赖 i：新增/删除概念不会让已有团心位移（排序序号对已有概念不变，
// 除非新概念插到它前面——那本来就是"概念键顺序变了"的语义变化）。
func clusterCenter(i int) (float64, float64) {
	if i == 0 {
		return 0, 0
	}
	angle := float64(i) * goldenAngle
	radius := clusterBaseRadius * math.Sqrt(float64(i)+0.5)
	return roundCoord(radius * math.Cos(angle)), roundCoord(radius * math.Sin(angle))
}

// clusterRadius 返回团内叶序的**名义**散布半径 R(n)，随节点数增长并封顶。
//
// 保留理由：它是“一个 n 个节点的团大约占多大”的校准量，布局不变量用例
// （团心间距 / 半径随 n 增长）与基准测试仍用它陈述约束。
// 但**节点落点不乘它**（见 nodePosition）：落点必须只依赖团内序号，
// 否则团内新增一个节点会让已有节点全部位移。
func clusterRadius(n int) float64 {
	if n <= 0 {
		return 0
	}
	r := clusterRadiusBase + clusterRadiusPerNode*math.Sqrt(float64(n))
	if r > clusterRadiusMax {
		r = clusterRadiusMax
	}
	return r
}

// nodePosition 返回团内第 index 个节点（共 count 个）的坐标。
//
// 纯函数：只吃 (index, count, center)。同一 nodeID 因为"团内按 ID 排序的序号"
// 确定 ⇒ 每次请求都落在同一格点上。**没有随机数、没有时间、没有包级可变状态。**
// r = min(leafStep*sqrt(index+0.5), clusterRadiusMax)、theta = index*goldenAngle
// 是离心叶序（向日葵）格点：任意前缀都是均匀散布，相邻点距近似相等。
//
// # 两个必须同时成立的性质
//
//  1. **不落团心**：旧式 r = R*sqrt(index/count) 在 index=0 时恒为 0 ⇒ 团内第一个
//     节点**必然**落在团心，而前端把团编圆盘也画在团心（render.js 的 translate），
//     于是那个节点永远被团徽盖住（demo 数据三个团全部命中）。离心项 +0.5 让
//     最近一环也离团心 leafStep*sqrt(0.5) > 0。
//  2. **与 count 无关**：r 只看 index，不看 count。旧式乘 sqrt(1/count) 使同一个
//     节点在团里多一个实体时就会位移（"加一条记忆就整图重排"）。
//
// 封顶用常量 clusterRadiusMax（而不是 clusterRadius(count)）：后者会重新引入
// count 依赖。代价是很难达到的、单团超过约 30 个节点时外圈会挤在R=0.44 的环上，
// 那部分残余重叠由前端的确定性碰撞松弛处理。
func nodePosition(index, count int, centerX, centerY float64) (float64, float64) {
	if count <= 0 {
		return centerX, centerY
	}
	if index < 0 {
		index = 0
	}
	if index >= count {
		index = count - 1
	}
	radius := leafStep * math.Sqrt(float64(index)+0.5)
	if radius > clusterRadiusMax {
		radius = clusterRadiusMax
	}
	angle := float64(index) * goldenAngle
	return roundCoord(centerX + radius*math.Cos(angle)),
		roundCoord(centerY + radius*math.Sin(angle))
}

// clusterKeys 返回排序后的概念键（确定性聚簇顺序的唯一来源）。
func clusterKeys(nodes []nodeRef) []string {
	seen := make(map[string]struct{}, 8)
	keys := make([]string, 0, 8)
	for _, n := range nodes {
		k := n.conceptKey
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// conceptKeyOf 把节点的 concept_id 归一成聚簇键（空 = 未归属）。
func conceptKeyOf(conceptID string) string {
	if conceptID == "" {
		return unclusteredKey
	}
	return conceptID
}

// leafOrder 给每个聚簇键分配"团内序号"与"团内节点数"。
//
// 输入 refs 已按节点 ID 排序（graph.go 的 ids 来自 sort.Strings），
// 同一个键的出现顺序因此就是"团内按 ID 排序"，序号即叶序参数 j。
// 返回的 index[k][id] 与 count[k] 都只由本次请求的节点集决定 ⇒ 不破坏确定性。
//
// 为什么序号用**全部**节点（含折叠掉的）而不是只数命中节点：
// 折叠（budget 超出）时命中节点会保留为真实节点，如果序号改按"命中集合"重排，
// 同一个实体在折叠前后的位置就会跳变——用全量序号让坐标与折叠无关。
func leafOrder(refs []nodeRef) (map[string]map[string]int, map[string]int) {
	index := make(map[string]map[string]int, 8)
	count := make(map[string]int, 8)
	for _, r := range refs {
		m, ok := index[r.conceptKey]
		if !ok {
			m = make(map[string]int, 4)
			index[r.conceptKey] = m
		}
		m[r.id] = count[r.conceptKey]
		count[r.conceptKey]++
	}
	return index, count
}
