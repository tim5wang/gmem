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

// TestRRFDefaultK：k<=0 时等价于 DefaultK。
func TestRRFDefaultK(t *testing.T) {
	a := IDs(RRF(0, Channel{Name: "l", Items: []Item{{ID: "a"}}}))
	b := IDs(RRF(DefaultK, Channel{Name: "l", Items: []Item{{ID: "a"}}}))
	if a[0] != b[0] {
		t.Fatalf("k=0 should equal DefaultK")
	}
}
