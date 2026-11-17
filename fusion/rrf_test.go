package fusion

import (
	"math"
	"testing"
)

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

// TestRRFTwoChannels：两路通道（lexical + vector）按 rank 融合。
func TestRRFTwoChannels(t *testing.T) {
	got := RRF(60,
		Channel{Name: "lexical", Weight: 1, Items: []Item{{ID: "a"}, {ID: "b"}, {ID: "c"}}},
		Channel{Name: "vector", Weight: 1, Items: []Item{{ID: "c"}, {ID: "b"}, {ID: "d"}}},
	)
	want := []string{"c", "b", "a", "d"}
	ids := IDs(got)
	if len(ids) != len(want) {
		t.Fatalf("got %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("got %v, want %v", ids, want)
		}
	}
	// b = 1/62 + 1/62；c = 1/63 + 1/61（c 略高，这正是 RRF 的按 rank 计分结果）
	if !approx(got[0].Score, 1.0/63.0+1.0/61.0) {
		t.Errorf("c score = %v, want %v", got[0].Score, 1.0/63.0+1.0/61.0)
	}
	if !approx(got[1].Score, 2.0/62.0) {
		t.Errorf("b score = %v, want %v", got[1].Score, 2.0/62.0)
	}
}

// TestRRFThreeChannels：lexical + vector + graph，证明 ≥2 路可扩展且逐路可解释。
func TestRRFThreeChannels(t *testing.T) {
	got := RRF(60,
		Channel{Name: "lexical", Items: []Item{{ID: "a"}, {ID: "b"}}},
		Channel{Name: "vector", Items: []Item{{ID: "b"}, {ID: "c"}}},
		Channel{Name: "graph", Weight: 0.5, Items: []Item{{ID: "c"}, {ID: "a"}}},
	)
	if len(got) != 3 {
		t.Fatalf("got %d candidates, want 3", len(got))
	}
	// 每个候选都应带三路里出现过的那几路贡献，未出现的通道没有条目。
	for _, f := range got {
		if len(f.Contributions) == 0 {
			t.Errorf("%s: empty contributions", f.ID)
		}
		for name, c := range f.Contributions {
			if !approx(c.Value, c.Weight/float64(60+c.Rank)) {
				t.Errorf("%s/%s: value=%v", f.ID, name, c.Value)
			}
		}
	}
	// graph 权重 0.5：a = 1/61 + 0.5/62, c = 1/62 + 0.5/61, b = 1/62 + 1/61
	// b 最高（两路都靠前），因此首位是 b。
	if got[0].ID != "b" {
		t.Errorf("top = %s, want b (%v)", got[0].ID, IDs(got))
	}
}

// TestRRFWeightAndMissingChannel：一路缺席时不做惩罚（降级可见但不塌陷）。
func TestRRFWeightAndMissingChannel(t *testing.T) {
	got := RRF(60,
		Channel{Name: "lexical", Weight: 1, Items: []Item{{ID: "a"}}},
		Channel{Name: "vector", Weight: 1, Items: nil}, // 无 embedder：空通道
	)
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("got %v, want [a]", IDs(got))
	}
	if !approx(got[0].Score, 1.0/61.0) {
		t.Errorf("score = %v, want %v", got[0].Score, 1.0/61.0)
	}
}

// TestRRFDedupAndEmpty：同通道重复 ID 只算首次；空 ID 忽略。
func TestRRFDedupAndEmpty(t *testing.T) {
	got := RRF(60,
		Channel{Name: "lexical", Items: []Item{{ID: "a"}, {ID: "a"}, {ID: ""}}},
		Channel{Name: "vector", Items: []Item{{ID: "b"}}},
	)
	if len(got) != 2 {
		t.Fatalf("got %v, want 2 candidates", IDs(got))
	}
	if !approx(got[0].Score, 1.0/61.0) {
		t.Errorf("dup not deduped: %v", got[0].Score)
	}
}

// TestRRFStable：同分结果按 ID 稳定排序，多次调用输出一致。
func TestRRFStable(t *testing.T) {
	first := IDs(RRF(60,
		Channel{Name: "lexical", Items: []Item{{ID: "y"}}},
		Channel{Name: "vector", Items: []Item{{ID: "x"}}},
	))
	want := []string{"x", "y"}
	for i := range want {
		if first[i] != want[i] {
			t.Fatalf("got %v, want %v", first, want)
		}
	}
	for i := 0; i < 20; i++ {
		again := IDs(RRF(60,
			Channel{Name: "lexical", Items: []Item{{ID: "y"}}},
			Channel{Name: "vector", Items: []Item{{ID: "x"}}},
		))
		for j := range again {
			if again[j] != first[j] {
				t.Fatalf("unstable output: %v vs %v", again, first)
			}
		}
	}
}

// TestRRFDefaultK：k<=0 时**分值与** DefaultK 完全等价（而不是"名次相同"）。
//
// 旧版只比 IDs(a)[0] == IDs(b)[0]。单通道单候选时两边都只有一个元素且 ID 相同，
// 即使 k=0 走了与 DefaultK 完全不同的常数（比如 k=1）也照样 PASS——这个断言没有判别力。
// 现在直接断言 Score 的数值：
//   score = w / (k + rank) = 1 / (60 + 1)
// 因此 k=0 必须得到 1/61；若实现把 k=0 当成 0（或任何其它值），分母会变、断言必红。
func TestRRFDefaultK(t *testing.T) {
	items := []Item{{ID: "a"}, {ID: "b"}}
	ch := func(k int) []Fused {
		return RRF(k, Channel{Name: "l", Items: items})
	}

	zero := ch(0)
	deflt := ch(DefaultK)

	if len(zero) != len(deflt) {
		t.Fatalf("len(k=0) = %d, len(k=DefaultK) = %d", len(zero), len(deflt))
	}
	for i := range deflt {
		if zero[i].ID != deflt[i].ID {
			t.Fatalf("第 %d 名 ID: k=0 得到 %q, want %q", i, zero[i].ID, deflt[i].ID)
		}
		if !approx(zero[i].Score, deflt[i].Score) {
			t.Errorf("第 %d 名 Score: k=0 得到 %v, want %v（必须与 DefaultK 等价）",
				i, zero[i].Score, deflt[i].Score)
		}
	}

	// 硬编码的绝对值：锁住"k=0 确实用了 60"这个事实，而非两路同错同过。
	const wantFirst = 1.0 / (DefaultK + 1) // 1/61
	if !approx(deflt[0].Score, wantFirst) {
		t.Fatalf("DefaultK 首个候选 Score = %v, want %v", deflt[0].Score, wantFirst)
	}
	if !approx(zero[0].Score, wantFirst) {
		t.Errorf("k=0 首个候选 Score = %v, want %v", zero[0].Score, wantFirst)
	}
	if !approx(deflt[1].Score, 1.0/(DefaultK+2)) {
		t.Errorf("第二个候选 Score = %v, want %v", deflt[1].Score, 1.0/(DefaultK+2))
	}

	// 反例：k=1 必须得到与 k=0 不同的分值，证明上面的断言真能区分 k 的取值。
	if approx(ch(1)[0].Score, wantFirst) {
		t.Fatalf("k=1 与 k=60 分值相同（%v），断言对 k 无判别力", ch(1)[0].Score)
	}
}
