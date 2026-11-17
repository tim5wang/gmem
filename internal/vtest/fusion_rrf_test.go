package vtest

import (
	"math"
	"testing"

	"github.com/tim5wang/gmem/fusion"
)

// ---------------------------------------------------------------------------
// RRF 融合顺序：独立手算期望值，不复用 dev 的 rrf_test.go
//
// score(id) = Σ_c weight_c / (k + rank_c)，rank 从 1 开始。
// 设计文档 §5：lexical / vector / graph 三路交出排名列表，RRF 只看 rank。
// ---------------------------------------------------------------------------

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

// TestRRFHandComputedOrder 用一条完整的"三路召回"场景手算融合分。
//
// lex (k=60, w=1): a r1   b r2   c r3   d r4
// vec            : b r1   c r2
// grp            : c r1   d r2   e r3
//
//	a = 1/61                                  = 0.0163934
//	b = 1/62 + 1/61                           = 0.0325225
//	c = 1/63 + 1/62 + 1/61                    = 0.0483953
//	d = 1/64 + 1/62                           = 0.0317540
//	e = 1/63                                  = 0.0158730
//
// 期望顺序：c > b > d > a > e（全部互不相等，避免用平局掩盖排序逻辑）。
func TestRRFHandComputedOrder(t *testing.T) {
	const k = 60
	lex := fusion.Channel{Name: "lexical", Items: []fusion.Item{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}}}
	vec := fusion.Channel{Name: "vector", Items: []fusion.Item{{ID: "b"}, {ID: "c"}}}
	grp := fusion.Channel{Name: "graph", Items: []fusion.Item{{ID: "c"}, {ID: "d"}, {ID: "e"}}}

	fused := fusion.RRF(k, lex, vec, grp)
	wantOrder := []string{"c", "b", "d", "a", "e"}
	if got := fusion.IDs(fused); !sameStrings(got, wantOrder) {
		t.Fatalf("融合顺序 = %v, want %v", got, wantOrder)
	}

	want := map[string]float64{
		"a": 1.0 / 61.0,
		"b": 1.0/62.0 + 1.0/61.0,
		"c": 1.0/63.0 + 1.0/62.0 + 1.0/61.0,
		"d": 1.0/64.0 + 1.0/62.0,
		"e": 1.0 / 63.0,
	}
	for _, f := range fused {
		if !approx(f.Score, want[f.ID]) {
			t.Errorf("%s 融合分 = %.10f, want %.10f", f.ID, f.Score, want[f.ID])
		}
		if len(f.Contributions) == 0 {
			t.Errorf("%s 没有 Contributions，无法 explain", f.ID)
		}
	}

	// Contributions 必须能复现 score（§13 "任意一次召回能复现 plan"）。
	for _, f := range fused {
		var sum float64
		for name, c := range f.Contributions {
			sum += c.Value
			if !approx(c.Value, c.Weight/float64(k+c.Rank)) {
				t.Errorf("%s/%s: Value=%.10f 与 Weight/(k+Rank)=%.10f 不符", f.ID, name, c.Value, c.Weight/float64(k+c.Rank))
			}
		}
		if !approx(sum, f.Score) {
			t.Errorf("%s: ΣContributions=%.10f != Score=%.10f", f.ID, sum, f.Score)
		}
	}
}

// TestRRFWeightsChangeOrder 验证权重真的改变分值与顺序（否则权重参数形同虚设）。
//
// lex (w=1) : lex-top r1, shared r2
// vec (w=3) : vec-top r1, shared r2
//
//	shared  = 1/62 + 3/62 = 0.0645161
//	vec-top = 3/61        = 0.0491803
//	lex-top = 1/61        = 0.0163934
//
// 顺序：shared > vec-top > lex-top。
func TestRRFWeightsChangeOrder(t *testing.T) {
	lex := fusion.Channel{Name: "lexical", Weight: 1, Items: []fusion.Item{{ID: "lex-top"}, {ID: "shared"}}}
	vec := fusion.Channel{Name: "vector", Weight: 3, Items: []fusion.Item{{ID: "vec-top"}, {ID: "shared"}}}

	fused := fusion.RRF(60, lex, vec)
	if got := fusion.IDs(fused); !sameStrings(got, []string{"shared", "vec-top", "lex-top"}) {
		t.Fatalf("融合顺序 = %v, want [shared vec-top lex-top]", got)
	}
	if !approx(scoreOf(fused, "shared"), 1.0/62.0+3.0/62.0) {
		t.Errorf("shared 融合分 = %.10f, want %.10f", scoreOf(fused, "shared"), 1.0/62.0+3.0/62.0)
	}
	if !approx(scoreOf(fused, "vec-top"), 3.0/61.0) {
		t.Errorf("vec-top 融合分 = %.10f, want %.10f", scoreOf(fused, "vec-top"), 3.0/61.0)
	}

	// 把 lexical 权重抬到 W：lex-top 要反超 shared，需要 W/61 > (W+1)/62，
	// 即 W > 61——因为 shared 在两路都拿到 r2，单路 r1 必须靠足够大的权重才能压过。
	// 取 W=100 留出余量，同时把这个不等式固化进注释，避免以后被误当成"权重无效"。
	const heavyW = 100.0
	heavy := fusion.RRF(60,
		fusion.Channel{Name: "lexical", Weight: heavyW, Items: lex.Items},
		fusion.Channel{Name: "vector", Weight: 1, Items: vec.Items},
	)
	if heavy[0].ID != "lex-top" {
		t.Errorf("lexical 权重抬到 %v 后 lex-top 应第一，得到 %v (lex-top=%.6f shared=%.6f)",
			heavyW, fusion.IDs(heavy), scoreOf(heavy, "lex-top"), scoreOf(heavy, "shared"))
	}
	if !approx(scoreOf(heavy, "lex-top"), heavyW/61.0) {
		t.Errorf("lex-top 融合分 = %.10f, want %.10f", scoreOf(heavy, "lex-top"), heavyW/61.0)
	}

	// Weight<=0 视为 1（实现约定），不能变成"零贡献"或负分。
	zero := fusion.RRF(60,
		fusion.Channel{Name: "lexical", Weight: 0, Items: []fusion.Item{{ID: "x"}}},
		fusion.Channel{Name: "vector", Weight: -5, Items: []fusion.Item{{ID: "y"}}},
	)
	if !approx(scoreOf(zero, "x"), 1.0/61.0) || !approx(scoreOf(zero, "y"), 1.0/61.0) {
		t.Errorf("Weight<=0 应视为 1，得到 x=%.10f y=%.10f", scoreOf(zero, "x"), scoreOf(zero, "y"))
	}
}

// TestRRFMissingChannelDoesNotDilute 验证 lane 缺席时其余通道不被稀释（设计意图）。
//
// vector lane 无 embedder 时整路缺席：lexical 的 rank 不应因为"少了一路"而重排。
func TestRRFMissingChannelDoesNotDilute(t *testing.T) {
	lex := fusion.Channel{Name: "lexical", Items: []fusion.Item{{ID: "x"}, {ID: "y"}}}
	emptyVec := fusion.Channel{Name: "vector"}

	withEmpty := fusion.RRF(60, lex, emptyVec)
	if !sameStrings(fusion.IDs(withEmpty), []string{"x", "y"}) {
		t.Errorf("空 vector lane 改变了顺序: %v", fusion.IDs(withEmpty))
	}
	withoutVec := fusion.RRF(60, lex)
	if !sameStrings(fusion.IDs(withoutVec), []string{"x", "y"}) {
		t.Errorf("单 lane 顺序异常: %v", fusion.IDs(withoutVec))
	}
	if scoreOf(withEmpty, "x") != scoreOf(withoutVec, "x") {
		t.Errorf("空通道稀释了 lexical 分值: %.10f != %.10f",
			scoreOf(withEmpty, "x"), scoreOf(withoutVec, "x"))
	}
}

// TestRRFTieBreakStable 验证**真正同分**时顺序稳定可复现（map 迭代不能泄漏到输出）。
//
// 构造真实平局：两路交叉，a 与 b 各拿一次 r1 + 一次 r2，融合分完全相等。
// 实现注释写明"同分按 ID，保证输出稳定可复现"，这里把该约定固化成契约。
func TestRRFTieBreakStable(t *testing.T) {
	ch1 := fusion.Channel{Name: "lexical", Items: []fusion.Item{{ID: "b"}, {ID: "a"}}}
	ch2 := fusion.Channel{Name: "vector", Items: []fusion.Item{{ID: "a"}, {ID: "b"}}}

	first := fusion.IDs(fusion.RRF(60, ch1, ch2))
	if scoreOf(fusion.RRF(60, ch1, ch2), "a") != scoreOf(fusion.RRF(60, ch1, ch2), "b") {
		t.Fatalf("用例前提失效：a 与 b 未同分")
	}
	// 同分 -> 按 ID 升序。
	if !sameStrings(first, []string{"a", "b"}) {
		t.Errorf("同分排序 = %v, want [a b]", first)
	}
	for i := 0; i < 100; i++ {
		if got := fusion.IDs(fusion.RRF(60, ch1, ch2)); !sameStrings(got, first) {
			t.Fatalf("第 %d 次运行输出顺序变化: %v != %v（map 迭代顺序泄漏）", i, got, first)
		}
	}
}

// TestRRFInputNotMutated 验证融合不修改调用方的入参（通道列表可能被上层复用）。
func TestRRFInputNotMutated(t *testing.T) {
	items := []fusion.Item{{ID: "b"}, {ID: "a"}}
	ch := fusion.Channel{Name: "lexical", Items: items}
	out := fusion.RRF(60, ch)
	if items[0].ID != "b" || items[1].ID != "a" {
		t.Errorf("RRF 重排了调用方的切片: %v", items)
	}
	if ch.Name != "lexical" {
		t.Errorf("RRF 改写了通道名: %q", ch.Name)
	}
	if out[0].ID != "b" {
		t.Errorf("输出首项 = %q, want b", out[0].ID)
	}
}

// TestRRFDedupWithinChannel 验证同一通道内重复 ID 只按首次（最好）rank 计分。
//
// 这条在生产中会发生：FTS 与 KNN 都可能因数据重复把同一 ID 吐两次。
// 若实现按累加处理，重复项会凭空获得双倍分值并污染顺序。
func TestRRFDedupWithinChannel(t *testing.T) {
	ch := fusion.Channel{Name: "lexical", Items: []fusion.Item{
		{ID: "dup"}, {ID: "other"}, {ID: "dup"},
	}}
	fused := fusion.RRF(60, ch)
	if !approx(scoreOf(fused, "dup"), 1.0/61.0) {
		t.Errorf("通道内重复 ID 分值 = %.10f, want %.10f（只算首次）", scoreOf(fused, "dup"), 1.0/61.0)
	}
	if !approx(scoreOf(fused, "other"), 1.0/62.0) {
		t.Errorf("other 分值 = %.10f, want %.10f", scoreOf(fused, "other"), 1.0/62.0)
	}
}

// TestRRFkSemantics 验证 k 的三种边界：<=0 用默认 60、显式 k 生效、增大 k 会压平差异。
func TestRRFkSemantics(t *testing.T) {
	ch := fusion.Channel{Name: "lexical", Items: []fusion.Item{{ID: "a"}}}
	if !approx(scoreOf(fusion.RRF(0, ch), "a"), 1.0/61.0) {
		t.Errorf("k=0 应使用 DefaultK=%d，得到 %.10f", fusion.DefaultK, scoreOf(fusion.RRF(0, ch), "a"))
	}
	if !approx(scoreOf(fusion.RRF(-3, ch), "a"), 1.0/61.0) {
		t.Errorf("k<0 应使用 DefaultK，得到 %.10f", scoreOf(fusion.RRF(-3, ch), "a"))
	}
	if !approx(scoreOf(fusion.RRF(1, ch), "a"), 1.0/2.0) {
		t.Errorf("k=1 时 rank1 分值应为 0.5，得到 %.10f", scoreOf(fusion.RRF(1, ch), "a"))
	}
}

func scoreOf(fused []fusion.Fused, id string) float64 {
	for _, f := range fused {
		if f.ID == id {
			return f.Score
		}
	}
	return math.NaN()
}
