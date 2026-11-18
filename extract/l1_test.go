package extract

import (
	"context"
	"testing"
)

// ===========================================================================
// L1 词法聚类（零 LLM）：只产候选，不做决定
// ===========================================================================

func TestL1ClustersSimilarSurfaces(t *testing.T) {
	l1 := NewL1(Config{}, nil)
	items := []L1Item{
		{ID: "a", Surface: "身份认证服务"},
		{ID: "b", Surface: "身份认证服务模块"},
		{ID: "c", Surface: "支付网关"},
		{ID: "d", Surface: "退款流程"},
	}
	res, err := l1.Cluster(context.Background(), items)
	if err != nil {
		t.Fatalf("Cluster: %v", err)
	}
	if len(res.Clusters) == 0 {
		t.Fatalf("应当产出至少一个疑似同簇（a/b 高度相似），实际 0；pairs=%v", res.Pairs)
	}
	// 断言 a/b 在同一簇内。
	found := false
	for _, c := range res.Clusters {
		hasA, hasB, hasC := false, false, false
		for _, s := range c.Surfaces {
			switch s {
			case "身份认证服务":
				hasA = true
			case "身份认证服务模块":
				hasB = true
			case "支付网关":
				hasC = true
			}
		}
		if hasA && hasB {
			found = true
			if hasC {
				t.Errorf("簇 %v 不应把无关名称'支付网关'卷进来", c.Surfaces)
			}
		}
	}
	if !found {
		t.Errorf("a/b 没有落在同一簇：%+v", res.Clusters)
	}
	if len(res.Degraded) == 0 {
		t.Error("未注入 Embedder 时必须在 Degraded 显式报告 vector 通道缺席")
	}
}

// TestL1ZeroDecision 断言 L1 不产生任何决定：它不返回"合并/命中节点"之类的结论，
// 只返回候选簇与候选对。这条是"L1 不得直接合并节点"的结构性证据。
func TestL1ProducesOnlyCandidates(t *testing.T) {
	l1 := NewL1(Config{}, nil)
	res, err := l1.Cluster(context.Background(), []L1Item{
		{ID: "n-1", Surface: "PostgreSQL 数据库", It: true},
		{ID: "n-2", Surface: "PostgreSQL 数据库集群", It: true},
	})
	if err != nil {
		t.Fatalf("Cluster: %v", err)
	}
	if len(res.Clusters) == 0 {
		t.Fatalf("应当产出候选簇")
	}
	// 簇内必须只含**表面形式**（候选），不含任何"选中哪个节点"的字段。
	for _, c := range res.Clusters {
		if c.ID == "" {
			t.Error("簇必须有稳定 ID（便于审计与测试）")
		}
		for _, s := range c.Surfaces {
			if s == "" {
				t.Error("簇成员不得为空")
			}
		}
	}
	// 相似度必须是"最弱一环"，不能伪造为 1。
	for _, c := range res.Clusters {
		if c.Similarity <= 0 || c.Similarity > 1 {
			t.Errorf("簇相似度应在 (0,1]，实际 %.3f", c.Similarity)
		}
	}
}

func TestL1TopKLimit(t *testing.T) {
	l1 := NewL1(Config{L1TopK: 2, L1MinSimilarity: 0.01}, nil)
	items := make([]L1Item, 0, 6)
	for _, s := range []string{"服务集群", "服务集群化", "服务集合", "服务聚合", "服务群体", "服务队列"} {
		items = append(items, L1Item{ID: s, Surface: s})
	}
	res, err := l1.Cluster(context.Background(), items)
	if err != nil {
		t.Fatalf("Cluster: %v", err)
	}
	degree := make(map[string]int, len(items))
	for _, p := range res.Pairs {
		degree[p.A]++
		degree[p.B]++
	}
	for id, d := range degree {
		if d > 2*2 { // 每侧 top-K=2，无序对最坏 2K
			t.Errorf("节点 %q 的候选边数 %d 超出 top-K 剪枝上界 4", id, d)
		}
	}
}

func TestL1EmptyAndSingle(t *testing.T) {
	l1 := NewL1(Config{}, nil)
	for _, items := range [][]L1Item{nil, {{ID: "x", Surface: "只有一个"}}} {
		res, err := l1.Cluster(context.Background(), items)
		if err != nil {
			t.Fatalf("Cluster: %v", err)
		}
		if len(res.Clusters) != 0 || len(res.Pairs) != 0 {
			t.Errorf("少于 2 项时不得产出任何候选，实际 clusters=%d pairs=%d",
				len(res.Clusters), len(res.Pairs))
		}
	}
}

// TestL1VectorChannel 验证注入 embedder 时向量通道参与相似度（且降级消失）。
//
// # 为什么断言要具体到"哪个对、多少分"（原来只断"非空"）
//
// 旧版只断 `len(res.Pairs) != 0` 与 `Pairs[0].Vector > 0`：
// 把相似度公式改错、或把 vector 权重摊掉，这两条仍然会绿。
// 这里把三块都钉死：
//   - 只有 a/b 一对（不凭空多出别的对）；
//   - Lexical 与 Vector 两个通道都 > 0（两者真的都被算进去了）；
//   - Similarity == (lex+vec)/2（融合公式本身，而不是"大于 0"）；
//   - 簇真的把 a/b 放在一起，且簇相似度落在 (0,1]。
func TestL1VectorChannel(t *testing.T) {
	emb := EmbedderFunc(func(ctx context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i, s := range texts {
			// 极简确定性向量：长度 + 首字符码，足以区分近似串。
			out[i] = []float32{float32(len([]rune(s))) / 10, 1}
		}
		return out, nil
	})
	l1 := NewL1(Config{L1MinSimilarity: 0.3}, emb)
	res, err := l1.Cluster(context.Background(), []L1Item{
		{ID: "a", Surface: "缓存服务"},
		{ID: "b", Surface: "缓存服务层"},
	})
	if err != nil {
		t.Fatalf("Cluster: %v", err)
	}
	if len(res.Degraded) != 0 {
		t.Errorf("注入 embedder 后不应再有 vector 缺席降级，实际 %v", res.Degraded)
	}
	if len(res.Pairs) != 1 {
		t.Fatalf("两项输入应产出恰好 1 个候选对，实际 %d：%+v", len(res.Pairs), res.Pairs)
	}
	p := res.Pairs[0]
	if !((p.A == "a" && p.B == "b") || (p.A == "b" && p.B == "a")) {
		t.Errorf("候选对应是 a/b，实际 %q/%q", p.A, p.B)
	}
	if p.Lexical <= 0 {
		t.Errorf("词法通道应参与打分（近似串的 BM25 相似度 > 0），实际 %.4f", p.Lexical)
	}
	if p.Vector <= 0 {
		t.Errorf("向量通道应参与打分，实际 Vector=%.4f", p.Vector)
	}
	if want := (p.Lexical + p.Vector) / 2; p.Similarity != want {
		t.Errorf("融合相似度应为 (lex+vec)/2=%.6f，实际 %.6f（公式被改坏或直接拷贝了单一通道）",
			want, p.Similarity)
	}
	if p.Similarity <= 0 || p.Similarity > 1 {
		t.Errorf("相似度应在 (0,1]，实际 %.4f", p.Similarity)
	}
	if len(res.Clusters) != 1 {
		t.Fatalf("应产出恰好 1 个簇，实际 %d：%+v", len(res.Clusters), res.Clusters)
	}
	got := map[string]bool{}
	for _, s := range res.Clusters[0].Surfaces {
		got[s] = true
	}
	if !got["缓存服务"] || !got["缓存服务层"] {
		t.Errorf("簇应同时包含两个表面形式，实际 %v", res.Clusters[0].Surfaces)
	}
	if c := res.Clusters[0].Similarity; c <= 0 || c > 1 {
		t.Errorf("簇相似度应在 (0,1]，实际 %.4f", c)
	}
}
