package sqlite

import (
	"context"
	"testing"

	"github.com/tim5wang/gmem/bigram"
	"github.com/tim5wang/gmem/store"
)

// indexDocs 是一个便捷封装：把 id→text 写成词法文档。
func indexDocs(t *testing.T, d *DB, scope store.Scope, docs map[string]string) {
	t.Helper()
	list := make([]store.Document, 0, len(docs))
	for id, text := range docs {
		list = append(list, store.Document{ID: id, Scope: scope, Text: text})
	}
	if err := d.Lexical().Index(context.Background(), list); err != nil {
		t.Fatalf("Index: %v", err)
	}
}

// TestLexicalSearchChineseTwoChar 是 P0 的硬验收：中文 2 字词必须能命中。
//
// 没有 bigram 预转换时，"用户身份认证失败"会被 FTS5 当成一个 token，
// 查询"认证"零命中——这个测试就会失败。
func TestLexicalSearchChineseTwoChar(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	indexDocs(t, d, store.ScopeUser, map[string]string{
		"m1": "用户身份认证失败，请重试",
		"m2": "令牌已过期，需要重新刷新",
		"m3": "部署环境是 k8s 集群",
	})

	for _, c := range []struct {
		query  string
		wantID string
	}{
		{"认证", "m1"},
		{"令牌", "m2"},
		{"过期", "m2"},
	} {
		hits, err := d.Lexical().Search(ctx, store.LexicalQuery{Query: c.query, Scope: store.ScopeUser, Limit: 10})
		if err != nil {
			t.Fatalf("Search(%q): %v", c.query, err)
		}
		found := false
		for _, h := range hits {
			if h.ID == c.wantID {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("查询 %q 未命中 %q；hits=%+v", c.query, c.wantID, hits)
		}
		if len(hits) > 0 && hits[0].Rank != 1 {
			t.Fatalf("首条 rank = %d, want 1", hits[0].Rank)
		}
	}
}

// TestBigramBothSidesSymmetric 是 §12 P0 验收要求的"专项断言测试"。
//
// 它直接对库里真实写入的索引文本与真实使用的查询表达式做比对：
// 从 memories_fts.bigrams 读回索引侧 token，从 bigram.QueryText 取查询侧 token，
// 两侧必须给出**同一个 token 序列**。任何一侧单独改规则都会在这里失败。
func TestBigramBothSidesSymmetric(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	corpus := []string{
		"认证",
		"令牌已过期，请重新认证",
		"用户身份认证服务在 2024 年上线",
		"SQLite FTS5 bigram 预分词",
		"我 更喜欢 用 Go 写 存储层",
		"认证、令牌、会话",
	}
	for i, text := range corpus {
		id := "doc" + string(rune('a'+i))
		indexDocs(t, d, store.ScopeUser, map[string]string{id: text})

		// 索引侧：从 FTS 表里读回落库的 bigrams 列。
		var indexed string
		if err := d.db.QueryRowContext(ctx,
			`SELECT bigrams FROM memories_fts WHERE id = ?`, id).Scan(&indexed); err != nil {
			t.Fatalf("read bigrams for %q: %v", text, err)
		}
		indexTokens := splitTokens(indexed)

		// 查询侧：同一段文本走查询表达式生成。
		queryTokens := splitQueryTokens(bigram.QueryText(text))

		want := bigram.Tokens(text)
		if !sameStrings(indexTokens, want) {
			t.Errorf("索引侧与 Tokens 不对称: %q → indexed %v, want %v", text, indexTokens, want)
		}
		if !sameStrings(queryTokens, want) {
			t.Errorf("查询侧与 Tokens 不对称: %q → query %v, want %v", text, queryTokens, want)
		}
		if !sameStrings(indexTokens, queryTokens) {
			t.Errorf("两侧不对称: %q → index %v, query %v", text, indexTokens, queryTokens)
		}
	}
}

// TestLexicalScopeIsolation：scope 过滤生效，串不到别的 scope。
func TestLexicalScopeIsolation(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	if err := d.Lexical().Index(ctx, []store.Document{
		{ID: "u1", Scope: store.ScopeUser, Text: "认证服务用 JWT"},
		{ID: "a1", Scope: store.ScopeAgent, Text: "认证服务用 session"},
	}); err != nil {
		t.Fatalf("Index: %v", err)
	}

	hits, err := d.Lexical().Search(ctx, store.LexicalQuery{Query: "认证", Scope: store.ScopeUser, Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != "u1" {
		t.Fatalf("scope filter = %+v, want [u1]", hits)
	}

	hits, err = d.Lexical().Search(ctx, store.LexicalQuery{Query: "认证", Scope: "", Limit: 10})
	if err != nil {
		t.Fatalf("Search all scopes: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("all scopes = %+v, want 2 hits", hits)
	}
}

// TestLexicalIndexOverwriteAndDelete：覆盖写不产生重复行；删除后不再命中。
func TestLexicalIndexOverwriteAndDelete(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	indexDocs(t, d, store.ScopeUser, map[string]string{"m1": "旧的认证实现"})
	indexDocs(t, d, store.ScopeUser, map[string]string{"m1": "新的令牌实现"})

	var n int
	if err := d.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM memories_fts WHERE id = 'm1'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("rows after overwrite = %d, want 1", n)
	}

	hits, err := d.Lexical().Search(ctx, store.LexicalQuery{Query: "认证", Scope: "", Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("stale bigram still matched: %+v", hits)
	}

	if err := d.Lexical().Delete(ctx, []store.DocumentKey{{ID: "m1"}}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	hits, err = d.Lexical().Search(ctx, store.LexicalQuery{Query: "令牌", Scope: "", Limit: 10})
	if err != nil {
		t.Fatalf("Search after delete: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("deleted doc still matched: %+v", hits)
	}
}

// TestLexicalSearchEmptyQuery：无 token 的查询返回空结果而非报错。
func TestLexicalSearchEmptyQuery(t *testing.T) {
	d := openTest(t)
	indexDocs(t, d, store.ScopeUser, map[string]string{"m1": "认证"})

	hits, err := d.Lexical().Search(context.Background(), store.LexicalQuery{Query: "！？", Scope: store.ScopeUser, Limit: 10})
	if err != nil {
		t.Fatalf("Search punctuation: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("punctuation query = %+v, want empty", hits)
	}
}

// TestLexicalMultiTokenQuery：多字查询不得因伪相邻 bigram 零命中。
//
// "认证令牌" 的 bigram 含 "证令"，真实文本里几乎不出现；用 AND 会零命中。
func TestLexicalMultiTokenQuery(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	indexDocs(t, d, store.ScopeUser, map[string]string{
		"m1": "认证失败，请重试",
		"m2": "令牌已过期",
	})

	hits, err := d.Lexical().Search(ctx, store.LexicalQuery{Query: "认证令牌", Scope: store.ScopeUser, Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	got := map[string]bool{}
	for _, h := range hits {
		got[h.ID] = true
	}
	if !got["m1"] || !got["m2"] {
		t.Fatalf("多字查询零命中或漏召：hits=%+v", hits)
	}
}

func splitTokens(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	cur := ""
	for _, r := range s {
		if r == ' ' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// splitQueryTokens 从 MATCH 表达式还原 token（去引号、按 OR 切分）。
//
// 必须按 rune 遍历：按 byte 遍历会把 UTF-8 多字节字符拆碎，
// 从而伪造出"两侧不对称"的假失败。
func splitQueryTokens(q string) []string {
	if q == "" {
		return nil
	}
	rs := []rune(q)
	const sep = " OR "
	sepRunes := []rune(sep)
	var out []string
	var cur []rune
	inQuote := false
	for i := 0; i < len(rs); i++ {
		if rs[i] == '"' {
			inQuote = !inQuote
			continue
		}
		if !inQuote && i+len(sepRunes) <= len(rs) && string(rs[i:i+len(sepRunes)]) == sep {
			if len(cur) > 0 {
				out = append(out, string(cur))
				cur = cur[:0]
			}
			i += len(sepRunes) - 1
			continue
		}
		cur = append(cur, rs[i])
	}
	if len(cur) > 0 {
		out = append(out, string(cur))
	}
	return out
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
