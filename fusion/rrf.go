// Package fusion 实现多路召回结果的 RRF（Reciprocal Rank Fusion）融合。
//
// 设计参照 docs/TD.md §5 的 Fuser：lexical / vector / graph 各 lane 交出**排名列表**，
// RRF 只按 rank 融合，不比较不同通道的原始分值——这是 RRF 无需跨通道校准的前提。
package fusion

import "sort"

// DefaultK 是 RRF 的平滑常数（Cormack et al. 常用取值）。
const DefaultK = 60

// Item 是某一路通道返回的一个候选。
type Item struct {
	ID    string
	Score float64 // 通道内原始分值，仅用于 explain，不参与融合
}

// Channel 是一路召回通道的排名列表。Items 必须已按 rank 从好到差排列。
type Channel struct {
	Name   string
	Weight float64 // <=0 视为 1
	Items  []Item
}

// Contribution 是一个通道对融合结果的贡献明细，用于 explain。
type Contribution struct {
	Rank   int
	Weight float64
	Value  float64 // Weight / (K + Rank)
}

// Fused 是一个融合后的候选。
type Fused struct {
	ID            string
	Score         float64
	Contributions map[string]Contribution
}

// RRF 融合至少两路通道的排名列表，按融合分降序返回。
//
// 计分：score(id) = Σ_channels weight_c / (K + rank_c)，rank 从 1 开始。
// 只在部分通道出现的候选按出现过的通道累计——这正是 RRF 对缺项稳健的来源
// （vector lane 因无 embedder 缺席时，lexical 结果不会被整体稀释）。
// k<=0 时使用 DefaultK。同一通道内重复的 ID 只取首次（最好）出现。
func RRF(k int, channels ...Channel) []Fused {
	if k <= 0 {
		k = DefaultK
	}
	acc := make(map[string]*Fused, len(channels)*8)
	for _, ch := range channels {
		w := ch.Weight
		if w <= 0 {
			w = 1
		}
		seen := make(map[string]bool, len(ch.Items))
		for i, it := range ch.Items {
			if it.ID == "" || seen[it.ID] {
				continue
			}
			seen[it.ID] = true
			rank := i + 1
			val := w / float64(k+rank)
			f, ok := acc[it.ID]
			if !ok {
				f = &Fused{ID: it.ID, Contributions: make(map[string]Contribution, len(channels))}
				acc[it.ID] = f
			}
			f.Score += val
			f.Contributions[ch.Name] = Contribution{Rank: rank, Weight: w, Value: val}
		}
	}
	out := make([]Fused, 0, len(acc))
	for _, f := range acc {
		out = append(out, *f)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].ID < out[j].ID // 同分按 ID，保证输出稳定可复现
	})
	return out
}

// IDs 只取融合结果的 ID 序列，便于直接喂给上层。
func IDs(fused []Fused) []string {
	out := make([]string, len(fused))
	for i, f := range fused {
		out[i] = f.ID
	}
	return out
}
