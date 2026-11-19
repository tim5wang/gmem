package server

import (
	"hash/fnv"
	"math"
	"sort"
)

// ===========================================================================
// 预布局坐标：纯函数，不落库（TD §9「返回预布局坐标缓存，避免前端抖动」）
//
// # 为什么不建表缓存
//
// 坐标是 (nodeID, conceptID) 的确定性函数：同一输入必得同一输出。为它加一张
// 缓存表（migration v10）只会引入"缓存过期/多实例不一致"这类新故障面，
// 换来的仅仅是省掉一次 O(n) 的纯计算（n 已被 budget 约束在 2000 以内）。
// 因此本卡**不新增任何 DB 表**：确定性由哈希保证，不依赖时间、随机数与进程状态。
//
// # 布局形状
//
// 概念团按黄金角螺旋排布（团心），团内节点绕团心做环形散布。
// 团心只依赖"概念键在排序后的序号"，节点位置只依赖 nodeID 的哈希，
// 因此删掉/新增一个节点不会让**其它**节点跳位（避免全图重排的视觉抖动）。
// ===========================================================================

// goldenAngle 是黄金角（弧度）。按序号 × 黄金角取点，任何前缀都是均匀散布，
// 因此团心分布不依赖总数——新增概念不会让已有团心位移。
const goldenAngle = 2.399963229728653

// unclusteredKey 是"节点没有 concept"时的聚簇键。
// 用控制字符开头，保证它排在所有真实 concept ID 之前（确定性排序）。
const unclusteredKey = "\x00unclustered"

// unclusteredLabel 是该簇的展示名。
const unclusteredLabel = "(未归属概念)"

// hashUnit 返回 [0,1) 的确定性伪随机数。
//
// 输入只有 (salt, key)：**没有时间、没有随机源、没有全局状态**。
// 因此同一 nodeID 在任何进程、任何时刻都得到同一坐标。
func hashUnit(key string, salt byte) float64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte{salt})
	_, _ = h.Write([]byte(key))
	sum := h.Sum64()
	// 取低 20 位映射到百万分之一，避免浮点在极端值上的平台差异。
	return float64(sum%1000003) / 1000003
}

// roundCoord 把坐标固定到 1e-6。
//
// 为什么要舍入：JSON 序列化 float64 已经是确定性的，但中间计算若引入
// 平台相关的 FMA 差异，最后一位可能不同——那会让"逐字节一致"的回归用例变脆。
// 舍入到 1e-6 之后，坐标是稳定可比的字面量。
func roundCoord(v float64) float64 {
	return math.Round(v*1e6) / 1e6
}

// clusterCenter 返回第 i 个概念团的团心坐标（i 是概念键排序后的序号）。
func clusterCenter(i int) (float64, float64) {
	if i == 0 {
		return 0, 0
	}
	angle := float64(i) * goldenAngle
	radius := 0.55 * math.Sqrt(float64(i)+0.5)
	return roundCoord(radius * math.Cos(angle)), roundCoord(radius * math.Sin(angle))
}

// nodePosition 返回节点在其概念团内的坐标。
func nodePosition(nodeID string, centerX, centerY float64) (float64, float64) {
	angle := 2 * math.Pi * hashUnit(nodeID, 0x11)
	radius := 0.12 + 0.22*math.Sqrt(hashUnit(nodeID, 0x22))
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
