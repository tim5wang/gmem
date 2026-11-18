// Package extract 是 Gmem 的整理层：确定性提取（L0）、词法聚类（L1）、
// LLM 抽取（L2，LLM 由主调方注入）与三区消歧（docs/TD.md §3 与 §4）。
//
// # 三条不可退让的边界
//
//  1. **Gmem 不直连任何 LLM、不 import 任何 LLM SDK、不保存密钥**（D7）。
//     本包的 import 只有标准库 + gmem 自己的包，`internal/vtest/no_network_test.go`
//     式的网络禁令在 internal/p1verify 与 internal/vtest 里继续生效。
//  2. **循环归 Gmem**（D10）：宿主实现 LLMClient 时只提供"这一轮的输出"，
//     多轮循环、工具执行、预算与收敛全部由本包的 LLMExtractor 拥有（§3.2）。
//     宿主实现 Extractor 时循环归宿主，Gmem 只用 context 强制中断。
//  3. **灰区绝不自动合并**（§4）：只有 s ≥ θ_hi 才写 alias；θ_lo ≤ s < θ_hi
//     只写 possiblySame 边并进待审池。误合并是记忆系统最难恢复的错误。
//
// # 写入
//
// 一切 memories/nodes/edges 的落库都走 recall.Writer（P1）：绕过它就是 P0 修过的
// "FTS 静默漏索引"缺陷。本包不直接调 Store.PutMemory。
package extract

import (
	"time"

	"github.com/tim5wang/gmem/store"
)

// 两个消歧专用关系名（docs/TD.md §4）。
//
// 它们**不在** store 的关系白名单里：白名单是"实体语义关系"（uses/part_of/…），
// 而这两个是消歧管线的记账边。刻意用独立常量，避免和 L2 抽取出的关系混淆。
const (
	// RelationPossiblySame 是灰区候选边：**不是合并**，只是"待人工/Agent 裁决"。
	RelationPossiblySame store.Relation = "possiblySame"
	// RelationNegativeSameAs 是"已裁决为不同实体"的记账边（§4 第 3 条）。
	//
	// 没有它，"张三(A)"和"张三(B)"会反复进入灰区，每次都要人重新判一遍。
	RelationNegativeSameAs store.Relation = "negative_SameAs"
)

// Zone 是三区判定结果（docs/TD.md §4）。
type Zone string

const (
	// ZoneAutoMerge：s ≥ θ_hi。写 alias，confidence=high。
	ZoneAutoMerge Zone = "auto_merge"
	// ZoneGray：θ_lo ≤ s < θ_hi。写 possiblySame 边入待审池，**绝不合并**。
	ZoneGray Zone = "gray"
	// ZoneNew：s < θ_lo。新建节点。
	ZoneNew Zone = "new"
	// ZoneNegative：该 (surface, 已有节点) 已被裁决为不同实体（negative_SameAs），
	// 因此不再进灰区，直接按新实体处理。
	ZoneNegative Zone = "negative"
)

// Config 是提取与消歧的全部可调参数。
//
// 零值可直接用（withDefaults 会补齐）。**所有阈值与上限都是待校准起点**，
// 不是实测结论（docs/TD.md §13「证据边界」）——改这里即可换策略，不必改调用点。
type Config struct {
	// ---------------------------------------------------------------
	// L0 质量门
	// ---------------------------------------------------------------

	// MinContentLength 是最小内容长度（rune 数）。低于它直接拒写
	// （docs/TD.md §3 L0「同样设长度下限（<10 字符拒写）」）。
	// 默认 10。
	MinContentLength int

	// ---------------------------------------------------------------
	// L1 词法聚类
	// ---------------------------------------------------------------

	// L1TopK 是每个名称保留的近邻数量（top-K）。
	// ⚠️ 默认 5 是待校准起点。
	L1TopK int
	// L1MinSimilarity 是"疑似同一实体"的入边门槛（bigram+BM25 融合分）。
	// ⚠️ 默认 0.60 是待校准起点：它只影响**候选簇**，不做任何合并决定，
	// 因此宁松勿紧（漏掉的候选要靠 L2 的 LLM 抽补回，误合并则无法回滚）。
	L1MinSimilarity float64
	// L1BM25K1 / L1BM25B 是 BM25 参数（标准取值，未校准但通常足够）。
	L1BM25K1 float64
	L1BM25B  float64

	// ---------------------------------------------------------------
	// 三区消歧（§4）
	// ---------------------------------------------------------------

	// W1Cosine / W2BM25 / W3Structural 是融合分权重：
	//
	//	s = w1·cosine(emb(name+definition)) + w2·BM25(name, aliases) + w3·结构一致性
	//
	// 权重按和归一（除以三者之和），因此 s 恒在 [0,1]。
	// ⚠️ 0.5/0.3/0.2 是待校准起点（TD §4 只给了公式，没给数值）。
	W1Cosine     float64
	W2BM25       float64
	W3Structural float64

	// ThetaHi / ThetaLo 是三区阈值。
	//
	// ⚠️ **必须是配置项，不是硬编码**，且 **0.92/0.72 必须用真实数据校准后
	// 才能定死**（TD §4 第 1 条）。它们是 A-MEM 灰区设计的起点，没有评测集支撑。
	ThetaHi float64
	ThetaLo float64

	// CandidateLimit 是消歧候选池上限（按 alias 枚举到的同 scope 节点）。
	// 候选池过大时按 alias 置信度截断，并在 Degraded 里报告。
	CandidateLimit int

	// ---------------------------------------------------------------
	// L2 收敛门禁（§3.2 D10）
	// ---------------------------------------------------------------

	// MaxTurns 是 G1：最多允许**执行工具**的轮数。默认 3。
	// ⚠️ MaxTurns=3 尤其需要根据真实抽取质量与成本重新校准（TD §13）。
	MaxTurns int
	// TokenBudget 是 G2：累计 token 预算（prompt+completion 之和）。
	// ⚠️ 默认 12000 是待校准起点。
	TokenBudget int
	// WallClock 是 G5：墙钟超时。默认 30s。
	WallClock time.Duration
	// FinalGrace 是"强制终稿"那一次调用可用的时长：
	// G5 已经超时时仍给一次极短的收尾机会，拿不到就降级返回。
	FinalGrace time.Duration
	// MaxTokens / Temperature 是每轮请求的默认值（抽取任务不需要随机性）。
	MaxTokens   int
	Temperature float32

	// ---------------------------------------------------------------
	// 工具行数上限（§3.2 表）
	// ---------------------------------------------------------------

	LookupLimit    int // lookup_entity：10
	NeighborsLimit int // neighbors：30
	SearchLimit    int // search_memories：5

	// ---------------------------------------------------------------
	// 歧义爆炸护栏（§4 表）
	// ---------------------------------------------------------------

	// NodeEpisodeRatio：同 scope 节点数 / episode 数 > 该比例 → 告警"抽取过碎"。
	// ⚠️ 默认 20 是待校准起点。
	NodeEpisodeRatio float64
	// GrayBacklogLimit：灰区待审积压 > N 条 → 降级：暂停 L2，只跑 L0/L1。
	// ⚠️ 默认 200 是待校准起点。
	GrayBacklogLimit int
	// MaxAliasesPerNode：单节点 alias 数 > M → 强制人工审，不再自动合并。
	// ⚠️ 默认 8 是待校准起点，防"万能实体"。
	MaxAliasesPerNode int

	// Now 是可注入时钟（测试用）。nil = time.Now。
	Now func() time.Time
}

// withDefaults 补齐零值。**不做范围夹取**（除明显非法值）：
// 调用方显式设成 0 表示"关掉这条门禁"时应当被尊重，只有 <0 才视为未设置。
func (c Config) withDefaults() Config {
	if c.MinContentLength <= 0 {
		c.MinContentLength = 10
	}
	if c.L1TopK <= 0 {
		c.L1TopK = 5
	}
	if c.L1MinSimilarity <= 0 {
		c.L1MinSimilarity = 0.60
	}
	if c.L1BM25K1 <= 0 {
		c.L1BM25K1 = 1.2
	}
	if c.L1BM25B < 0 {
		c.L1BM25B = 0.75
	}
	if c.L1BM25B == 0 {
		c.L1BM25B = 0.75
	}
	if c.W1Cosine <= 0 {
		c.W1Cosine = 0.5
	}
	if c.W2BM25 <= 0 {
		c.W2BM25 = 0.3
	}
	if c.W3Structural <= 0 {
		c.W3Structural = 0.2
	}
	if c.ThetaHi <= 0 {
		c.ThetaHi = 0.92
	}
	if c.ThetaLo <= 0 {
		c.ThetaLo = 0.72
	}
	if c.ThetaLo > c.ThetaHi {
		// 区间反了就后退到默认起点：灰区不能是空集，
		// 否则"灰区不合并"这条保证会静默变成"要么合并要么新建"。
		c.ThetaHi, c.ThetaLo = 0.92, 0.72
	}
	if c.CandidateLimit <= 0 {
		c.CandidateLimit = 64
	}
	if c.MaxTurns <= 0 {
		c.MaxTurns = 3
	}
	if c.TokenBudget <= 0 {
		c.TokenBudget = 12000
	}
	if c.WallClock <= 0 {
		c.WallClock = 30 * time.Second
	}
	if c.FinalGrace <= 0 {
		c.FinalGrace = 5 * time.Second
	}
	if c.MaxTokens <= 0 {
		c.MaxTokens = 1024
	}
	if c.Temperature < 0 {
		c.Temperature = 0
	}
	if c.LookupLimit <= 0 {
		c.LookupLimit = 10
	}
	if c.NeighborsLimit <= 0 {
		c.NeighborsLimit = 30
	}
	if c.SearchLimit <= 0 {
		c.SearchLimit = 5
	}
	if c.NodeEpisodeRatio <= 0 {
		c.NodeEpisodeRatio = 20
	}
	if c.GrayBacklogLimit <= 0 {
		c.GrayBacklogLimit = 200
	}
	if c.MaxAliasesPerNode <= 0 {
		c.MaxAliasesPerNode = 8
	}
	return c
}

// now 返回可注入时钟的结果。
func (c Config) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}
