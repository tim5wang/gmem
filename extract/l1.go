package extract

import (
	"context"
	"math"
	"sort"
	"strings"

	"github.com/tim5wang/gmem/bigram"
	"github.com/tim5wang/gmem/recall"
)

// Embedder 复用 recall 的注入点（宿主只需要实现一次）。
//
// 用类型别名而不是新接口：同一份向量能力要同时服务召回侧（vector lane）与
// 整理侧（L1 近邻 / 消歧 cosine），复制两个接口只会让宿主写两次适配器。
type Embedder = recall.Embedder

// EmbedderFunc 同理复用 recall 的函数适配器，便于宿主与测试注入。
type EmbedderFunc = recall.EmbedderFunc

// ===========================================================================
// L1 词法聚类（0 次 LLM，docs/TD.md §3）
//
// 同 scope 内未消歧名称两两做 bigram/BM25 + 向量近邻，取 top-K，
// 用并查集产出「疑似同一实体」簇，交给 §4 消歧。
//
// **只产生候选，不做决定**：本文件不写任何 store，也不合并任何节点。
// 这一条是 §4「灰区绝不合并」在 L1 侧的对应约束——L1 的相似度是**候选信号**，
// 不是消歧结论；把它当结论就会绕过 θ_hi/θ_lo 的三区判定。
// ===========================================================================

// L1 是词法聚类器。零值不可用，用 NewL1 构造。
type L1 struct {
	cfg Config
	emb Embedder // 可为 nil：无向量通道时只用 bigram/BM25
}

// NewL1 构造聚类器。emb 为 nil 时只在 Lexical 通道上工作，
// 并在结果的 Degraded 里显式报告"vector lane 缺席"（不静默）。
func NewL1(cfg Config, emb Embedder) *L1 {
	return &L1{cfg: cfg.withDefaults(), emb: emb}
}

// Config 返回生效配置。
func (l *L1) Config() Config { return l.cfg }

// bm25Index 是一个极小的 BM25 语料索引（bigram token 空间）。
//
// 不引入第三方检索库：P0/P1 已经用 FTS5 做词法，这里只需要**成对相似度**，
// 而 BM25 的成对形式只有几十行，自己写比接一个库更容易解释与校准。
type bm25Index struct {
	docs  []map[string]int // 词频
	len   []int            // 每个 doc 的 token 数
	df    map[string]int
	n     int
	avgdl float64
	k1    float64
	b     float64
}

func newBM25Index(tokenSets [][]string, k1, b float64) *bm25Index {
	idx := &bm25Index{
		docs: make([]map[string]int, len(tokenSets)),
		len:  make([]int, len(tokenSets)),
		df:   make(map[string]int, 64),
		n:    len(tokenSets),
		k1:   k1,
		b:    b,
	}
	total := 0
	for i, toks := range tokenSets {
		tf := make(map[string]int, len(toks))
		for _, t := range toks {
			tf[t]++
		}
		idx.docs[i] = tf
		idx.len[i] = len(toks)
		total += len(toks)
		for t := range tf {
			idx.df[t]++
		}
	}
	if idx.n > 0 {
		idx.avgdl = float64(total) / float64(idx.n)
	}
	return idx
}

// idf 用 BM25 的平滑形式，并对 n=1 的语料给出正数（否则 ln(0.5) < 0 会把
// 相似度算成负数，后续归一化就失真了）。
func (x *bm25Index) idf(term string) float64 {
	df := float64(x.df[term])
	n := float64(x.n)
	return math.Log(1 + (n-df+0.5)/(df+0.5))
}

// score 返回 query 对 doc 的 BM25 分。
func (x *bm25Index) score(query []string, doc int) float64 {
	if doc < 0 || doc >= len(x.docs) {
		return 0
	}
	tf := x.docs[doc]
	dl := float64(x.len[doc])
	var s float64
	for _, term := range query {
		f, ok := tf[term]
		if !ok {
			continue
		}
		fd := float64(f)
		denom := fd + x.k1*(1-x.b+x.b*dl/max64(x.avgdl, 1e-9))
		if denom == 0 {
			continue
		}
		s += x.idf(term) * fd * (x.k1 + 1) / denom
	}
	return s
}

// maxScore 是同一 query 的理论上界（每个 term 都取 tf=1 且 dl=avgdl 的最优情形），
// 用于把 BM25 分归一化到 [0,1]。没有归一化就无法与 cosine 混合，也无法定阈值。
func (x *bm25Index) maxScore(query []string) float64 {
	var s float64
	seen := make(map[string]struct{}, len(query))
	for _, term := range query {
		if _, ok := seen[term]; ok {
			continue
		}
		seen[term] = struct{}{}
		if x.df[term] == 0 {
			// query 里语料完全没有的 term：它对分母没有贡献（分子也必然是 0）。
			continue
		}
		denom := 1 + x.k1*(1-x.b+x.b*max64(x.avgdl, 1e-9)/max64(x.avgdl, 1e-9))
		s += x.idf(term) * 1 * (x.k1 + 1) / denom
	}
	if s == 0 {
		return 0
	}
	return s
}

func max64(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// Cluster 对一批名称做"疑似同一实体"聚类。
//
// 输出只有候选：Clusters 与 Pairs 都是**建议**，不做任何合并决定。
// 相似度 ≥ L1MinSimilarity 才连边；连边后取并查集的连通分量成簇（成员 ≥2）。
func (l *L1) Cluster(ctx context.Context, items []L1Item) (L1Result, error) {
	var res L1Result
	if err := ctx.Err(); err != nil {
		return res, err
	}
	if len(items) < 2 {
		return res, nil
	}

	surfaces := make([]string, len(items))
	tokenSets := make([][]string, len(items))
	for i, it := range items {
		s := strings.ToLower(strings.TrimSpace(it.Surface))
		surfaces[i] = s
		tokenSets[i] = bigram.Tokens(s)
	}
	bm := newBM25Index(tokenSets, l.cfg.L1BM25K1, l.cfg.L1BM25B)

	// 向量通道：宿主没注入 embedder 时**显式**降级，而不是悄悄只用词法。
	vectors := make([][]float32, len(items))
	haveVector := false
	if l.emb != nil {
		texts := make([]string, len(items))
		for i, it := range items {
			if len(it.Values) > 0 {
				vectors[i] = it.Values
				haveVector = true
				continue
			}
			texts[i] = it.Surface
		}
		if !haveVector {
			vecs, err := l.emb.Embed(ctx, texts)
			switch {
			case err != nil:
				res.Degraded = append(res.Degraded,
					"L1 vector lane skipped: embedder error: "+err.Error())
			case len(vecs) != len(texts):
				res.Degraded = append(res.Degraded, "L1 vector lane skipped: embedder 返回数量不匹配")
			default:
				for i := range vecs {
					if len(vecs[i]) > 0 {
						vectors[i] = vecs[i]
						haveVector = true
					}
				}
			}
		}
	} else {
		res.Degraded = append(res.Degraded,
			"L1 vector lane skipped: 未注入 Embedder（只用 bigram/BM25，属已知降级）")
	}

	// 成对打分（O(n²)，n 是同一 scope 的单批候选量级，见 §13 规模假设）。
	type pair struct {
		i, j int
		sim  float64
		lex  float64
		vec  float64
	}
	var pairs []pair
	for i := 0; i < len(items); i++ {
		for j := i + 1; j < len(items); j++ {
			lex := lexicalSimilarity(bm, tokenSets, i, j)
			var cos float64
			if haveVector && len(vectors[i]) > 0 && len(vectors[j]) > 0 {
				cos = cosine(vectors[i], vectors[j])
			}
			sim := lex
			if cos > 0 {
				sim = (lex + cos) / 2
			}
			if sim < l.cfg.L1MinSimilarity {
				continue
			}
			pairs = append(pairs, pair{i: i, j: j, sim: sim, lex: lex, vec: cos})
		}
	}

	// top-K 剪枝：按相似度降序，每个节点最多保留 L1TopK 条边。
	// 这既控成本，也避免"全连成一簇"的链式污染。
	sort.SliceStable(pairs, func(a, b int) bool {
		if pairs[a].sim != pairs[b].sim {
			return pairs[a].sim > pairs[b].sim
		}
		if pairs[a].i != pairs[b].i {
			return pairs[a].i < pairs[b].i
		}
		return pairs[a].j < pairs[b].j
	})
	degree := make([]int, len(items))
	uf := newUnionFind(len(items))
	for _, p := range pairs {
		if degree[p.i] >= l.cfg.L1TopK || degree[p.j] >= l.cfg.L1TopK {
			continue
		}
		degree[p.i]++
		degree[p.j]++
		uf.union(p.i, p.j)
		res.Pairs = append(res.Pairs, L1Pair{
			A: items[p.i].ID, B: items[p.j].ID,
			Lexical: p.lex, Vector: p.vec, Similarity: p.sim,
		})
	}

	// 并查集连通分量 → 簇（成员 ≥ 2）。
	groups := make(map[int][]int, len(items))
	for i := range items {
		r := uf.find(i)
		groups[r] = append(groups[r], i)
	}
	for _, members := range groups {
		if len(members) < 2 {
			continue
		}
		sort.Ints(members)
		c := Cluster{ID: clusterID(items, members), Similarity: 1}
		for _, m := range members {
			c.Surfaces = append(c.Surfaces, items[m].Surface)
		}
		// 簇的"最弱一环"：入边里最小的成对相似度。
		c.Similarity = minClusterSim(res.Pairs, items, members)
		res.Clusters = append(res.Clusters, c)
	}
	sort.SliceStable(res.Clusters, func(i, j int) bool {
		return res.Clusters[i].ID < res.Clusters[j].ID
	})
	sort.SliceStable(res.Pairs, func(i, j int) bool {
		if res.Pairs[i].Similarity != res.Pairs[j].Similarity {
			return res.Pairs[i].Similarity > res.Pairs[j].Similarity
		}
		if res.Pairs[i].A != res.Pairs[j].A {
			return res.Pairs[i].A < res.Pairs[j].A
		}
		return res.Pairs[i].B < res.Pairs[j].B
	})
	return res, nil
}

// lexicalSimilarity 用 BM25 的双向分归一化后取较大值，作为成对词法相似度。
//
// 取双向较大值而不是平均：中文短名的 bigram 覆盖差距大（"张伟" vs "张伟明"），
// 平均会把"短名完全被长名包含"这种强信号压掉一半。
func lexicalSimilarity(bm *bm25Index, tokenSets [][]string, i, j int) float64 {
	a, b := tokenSets[i], tokenSets[j]
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	norm := func(query []string, doc int) float64 {
		den := bm.maxScore(query)
		if den <= 0 {
			return 0
		}
		s := bm.score(query, doc) / den
		if s > 1 {
			return 1
		}
		if s < 0 {
			return 0
		}
		return s
	}
	return math.Max(norm(a, j), norm(b, i))
}

func cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	c := dot / (math.Sqrt(na) * math.Sqrt(nb))
	if c < 0 {
		return 0
	}
	if c > 1 {
		return 1
	}
	return c
}

// clusterID 用成员名称的稳定序拼出簇标识（不哈希：可读性对调试更重要，
// 且这里不涉及长度约束）。
func clusterID(items []L1Item, members []int) string {
	names := make([]string, len(members))
	for i, m := range members {
		names[i] = strings.ToLower(strings.TrimSpace(items[m].Surface))
	}
	sort.Strings(names)
	return "cluster:" + strings.Join(names, "|")
}

func minClusterSim(pairs []L1Pair, items []L1Item, members []int) float64 {
	inCluster := make(map[string]struct{}, len(members))
	for _, m := range members {
		inCluster[items[m].ID] = struct{}{}
	}
	best := 1.0
	found := false
	for _, p := range pairs {
		_, okA := inCluster[p.A]
		_, okB := inCluster[p.B]
		if !okA || !okB {
			continue
		}
		if !found || p.Similarity < best {
			best = p.Similarity
			found = true
		}
	}
	if !found {
		return 0
	}
	return best
}

// unionFind 是并查集（带路径压缩 + 按秩合并），用于连通分量。
type unionFind struct {
	parent []int
	rank   []int
}

func newUnionFind(n int) *unionFind {
	u := &unionFind{parent: make([]int, n), rank: make([]int, n)}
	for i := range u.parent {
		u.parent[i] = i
	}
	return u
}

func (u *unionFind) find(x int) int {
	for u.parent[x] != x {
		u.parent[x] = u.parent[u.parent[x]]
		x = u.parent[x]
	}
	return x
}

func (u *unionFind) union(a, b int) {
	ra, rb := u.find(a), u.find(b)
	if ra == rb {
		return
	}
	if u.rank[ra] < u.rank[rb] {
		ra, rb = rb, ra
	}
	u.parent[rb] = ra
	if u.rank[ra] == u.rank[rb] {
		u.rank[ra]++
	}
}
