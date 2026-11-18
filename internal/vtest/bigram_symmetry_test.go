package vtest

import (
	"context"
	"strings"
	"testing"
	"unicode"

	"github.com/tim5wang/gmem/bigram"
	"github.com/tim5wang/gmem/store"
)

// ---------------------------------------------------------------------------
// 攻击面 1（最高优先级）：FTS5 中文 bigram 的**双侧对称**
//
// 设计文档与实现注释都点名："索引侧与查询侧必须走同一套归一化，否则中文查询静默返回空"。
// 这里不重复 dev 的 TestBigramBothSidesSymmetric（那只比对两个函数的输出），
// 而是**分别把"索引侧没做 bigram"和"查询侧没做 bigram"各造一个真实故障**，
// 并用 FTS5 原始表做对照，证明：
//   - 索引侧少了 bigram：2 字中文查询静默返回空；
//   - 查询侧少了 bigram：3 字以上中文查询静默返回空（2 字恰好因为索引里存在同形 token 而侥幸命中）；
//   - 显式对 2 字查询断言"双侧一致"，防止将来只改一侧。
// ---------------------------------------------------------------------------

// 一份足够"暴露分词差异"的语料：中文长串、纯 2 字、中英混排、繁体、标点分隔。
var bigramDocs = []store.Document{
	{ID: "zh-long", Scope: store.ScopeUser, Text: "用户身份认证失败，需要重新登录"},
	{ID: "zh-2char", Scope: store.ScopeUser, Text: "认证"},
	{ID: "zh-middle", Scope: store.ScopeUser, Text: "数据库迁移幂等性验证"},
	{ID: "mixed", Scope: store.ScopeUser, Text: "G1 服务身份认证超时 G1-timeout"},
	{ID: "zh-tw", Scope: store.ScopeUser, Text: "認證服務暫時不可用"},
}

// TestBigramSymmetryIndexSide 造"索引侧没做 bigram"的旧实现。
//
// 手法：用直连连接把同一段中文以**原始文本**写进 memories_fts.bigrams 列
// （旧实现会把 body 直接塞进索引列，不做 pre-tokenize）。
// 断言：bigram 索引的行能被 2 字查询命中，旧实现的行静默漏召回。
func TestBigramSymmetryIndexSide(t *testing.T) {
	d, raw := openFile(t)
	ctx := context.Background()

	if err := d.Lexical().Index(ctx, bigramDocs); err != nil {
		t.Fatalf("Index: %v", err)
	}

	// 旧实现行：bigrams 列直接写原文，不做任何 bigram 预转换。
	const legacyText = "用户身份认证失败，需要重新登录"
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO memories_fts (id, scope, body, bigrams) VALUES (?, ?, ?, ?)`,
		"legacy-raw", string(store.ScopeUser), legacyText, legacyText,
	); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}

	for _, q := range []string{"认证", "身份", "登录"} {
		hits, err := d.Lexical().Search(ctx, store.LexicalQuery{Query: q, Scope: store.ScopeUser, Limit: 10})
		if err != nil {
			t.Fatalf("Search(%q): %v", q, err)
		}
		ids := hitIDs(hits)
		if !contains(ids, "zh-long") {
			t.Errorf("索引侧必须走 bigram：查询 %q 未命中 zh-long，hits=%v", q, ids)
		}
		if contains(ids, "legacy-raw") {
			t.Errorf("旧实现（索引侧未 bigram）的行本应漏召回，却命中 q=%q hits=%v", q, ids)
		}
	}

	// 直接确认落库形态：bigrams 列必须是空格分隔的 bigram，而不是原文。
	var stored string
	if err := raw.QueryRowContext(ctx,
		`SELECT bigrams FROM memories_fts WHERE id = ?`, "zh-long").Scan(&stored); err != nil {
		t.Fatalf("read bigrams: %v", err)
	}
	want := bigram.IndexText("用户身份认证失败，需要重新登录")
	if stored != want {
		t.Fatalf("索引侧落库值 %q != bigram.IndexText 输出 %q", stored, want)
	}
	// 原文不得以整段形式出现（否则说明没切分）。
	if strings.Contains(stored, "用户身份认证失败") {
		t.Errorf("索引列仍含未切分的中文长串: %q", stored)
	}
}

// TestBigramSymmetryQuerySide 造"查询侧没做 bigram"的旧实现。
//
// 关键判别：**3 字查询**。索引里只有 bigram token（身份/份认/认证），
// 不存在整词 token "身份认证"，所以查询侧若不切分会零命中；
// 而 2 字查询（"认证"）在两侧都不切分时恰好也能撞上同名 token，
// 因此只用 2 字查询做验收是测不出查询侧缺陷的——这正是本用例存在的理由。
func TestBigramSymmetryQuerySide(t *testing.T) {
	d, raw := openFile(t)
	ctx := context.Background()

	if err := d.Lexical().Index(ctx, bigramDocs); err != nil {
		t.Fatalf("Index: %v", err)
	}

	const query = "身份认证"

	// 查询侧正确实现：命中。
	hits, err := d.Lexical().Search(ctx, store.LexicalQuery{Query: query, Scope: store.ScopeUser, Limit: 10})
	if err != nil {
		t.Fatalf("Search(%q): %v", query, err)
	}
	if ids := hitIDs(hits); !contains(ids, "zh-long") {
		t.Fatalf("查询侧必须走 bigram：%q 未命中 zh-long，hits=%v", query, ids)
	}

	// 查询侧旧实现：把原文直接交给 MATCH。必须零命中，否则这个用例失去判别力。
	var legacyHits int
	if err := raw.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM memories_fts WHERE memories_fts MATCH ?`, query).Scan(&legacyHits); err != nil {
		t.Fatalf("legacy match: %v", err)
	}
	if legacyHits != 0 {
		t.Fatalf("判别力失效：查询侧旧实现（原文 MATCH）竟然命中 %d 行，"+
			"说明索引列里存在整词 token %q", legacyHits, query)
	}

	// 同一张表、同一条查询，经 bigram.QueryText 后必须命中——
	// 唯一差别就是"查询侧有没有做归一化"。
	expr := bigram.QueryText(query)
	if expr == "" {
		t.Fatalf("QueryText(%q) 为空，查询侧无法构造 MATCH 表达式", query)
	}
	var bigramHits int
	if err := raw.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM memories_fts WHERE memories_fts MATCH ?`, expr).Scan(&bigramHits); err != nil {
		t.Fatalf("bigram match: %v", err)
	}
	if bigramHits == 0 {
		t.Fatalf("QueryText 结果 %q 在索引上零命中，双侧不对称", expr)
	}
}

// TestBigramBothSidesSameTokens 是最小对称性断言：两侧必须调用同一个 Tokens。
//
// 断言三点：
//  1. IndexText 的 token 多重集 == Tokens；
//  2. QueryText 里的每个 token（去引号后）都能在 IndexText 里找到；
//  3. 对语料里**每一个 2 字中文子串**做查询，都必须命中该文档（这是用户真正的期望）。
func TestBigramBothSidesSameTokens(t *testing.T) {
	d := openMemory(t)
	ctx := context.Background()

	if err := d.Lexical().Index(ctx, bigramDocs); err != nil {
		t.Fatalf("Index: %v", err)
	}

	for _, doc := range bigramDocs {
		indexToks := strings.Fields(bigram.IndexText(doc.Text))
		wantToks := bigram.Tokens(doc.Text)
		if strings.Join(indexToks, "\x00") != strings.Join(wantToks, "\x00") {
			t.Fatalf("doc %s：IndexText 的 token 序列与 Tokens 不一致\n got=%v\nwant=%v", doc.ID, indexToks, wantToks)
		}

		for _, sub := range cjkBigrams(doc.Text) {
			// 查询侧 token 必须在索引侧 token 集合里，否则必然漏召回。
			for _, qt := range bigram.Tokens(sub) {
				if !hasToken(indexToks, qt) {
					t.Fatalf("doc %s：查询子串 %q 的 token %q 不在索引 token 里，双侧不对称\nindex=%v",
						doc.ID, sub, qt, indexToks)
				}
			}
			hits, err := d.Lexical().Search(ctx, store.LexicalQuery{Query: sub, Scope: doc.Scope, Limit: 10})
			if err != nil {
				t.Fatalf("Search(%q): %v", sub, err)
			}
			if !contains(hitIDs(hits), doc.ID) {
				t.Errorf("doc %s：2 字中文查询 %q 未命中，hits=%v", doc.ID, sub, hitIDs(hits))
			}
		}
	}
}

// TestBigramSingleCharQueryDocumentedGap 记录一个已知且被实现注释承认的边界：
// 单字中文查询无法命中 2 字以上的 bigram 索引（索引侧只存 bigram，不存单字）。
//
// 这不是"测出来的新 bug"，而是需要显式固化下来的行为边界：
// 若 P1 召回侧要支持 1 字查询，必须另加单字列或前缀匹配，不能指望现结构。
func TestBigramSingleCharQueryDocumentedGap(t *testing.T) {
	d := openMemory(t)
	ctx := context.Background()
	if err := d.Lexical().Index(ctx, bigramDocs); err != nil {
		t.Fatalf("Index: %v", err)
	}
	for _, q := range []string{"认", "证", "数"} {
		hits, err := d.Lexical().Search(ctx, store.LexicalQuery{Query: q, Scope: store.ScopeUser, Limit: 10})
		if err != nil {
			t.Fatalf("Search(%q): %v", q, err)
		}
		if len(hits) != 0 {
			t.Errorf("单字查询 %q 出现了 %v，说明分词规则已变，需同步更新本边界说明", q, hitIDs(hits))
		}
	}
	// 2 字查询必须命中，作为同一语料下的对照。
	hits, err := d.Lexical().Search(ctx, store.LexicalQuery{Query: "数据库", Scope: store.ScopeUser, Limit: 10})
	if err != nil {
		t.Fatalf("Search(数据库): %v", err)
	}
	if !contains(hitIDs(hits), "zh-middle") {
		t.Errorf("对照失败：2 字查询「数据库」未命中 zh-middle，hits=%v", hitIDs(hits))
	}
}

// cjkBigrams 返回 s 中所有连续汉字段的 2 字子串（去重、保序）。
func cjkBigrams(s string) []string {
	rs := []rune(s)
	var out []string
	seen := map[string]bool{}
	i := 0
	for i < len(rs) {
		if !unicode.Is(unicode.Han, rs[i]) {
			i++
			continue
		}
		j := i
		for j < len(rs) && unicode.Is(unicode.Han, rs[j]) {
			j++
		}
		for k := i; k+1 < j; k++ {
			w := string(rs[k : k+2])
			if !seen[w] {
				seen[w] = true
				out = append(out, w)
			}
		}
		i = j
	}
	return out
}

func hasToken(toks []string, want string) bool {
	for _, t := range toks {
		if t == want {
			return true
		}
	}
	return false
}
