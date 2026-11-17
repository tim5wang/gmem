package bigram

import (
	"strings"
	"testing"
)

func TestTokens(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"认证", []string{"认证"}},
		{"身份认证服务", []string{"身份", "份认", "认证", "证服", "服务"}},
		{"令牌已过期", []string{"令牌", "牌已", "已过", "过期"}},
		// 标点、空格、下划线作为分隔符
		{"认证、令牌", []string{"认证", "令牌"}},
		{"a_b c", []string{"a", "b", "c"}},
		// 拉丁字母/数字整段一个小写 token
		{"G1 SQLite", []string{"g1", "sqlite"}},
		// 单字 CJK 保留
		{"我 认证", []string{"我", "认证"}},
		// 无 token
		{"！？...", nil},
	}
	for _, c := range cases {
		got := Tokens(c.in)
		if !equal(got, c.want) {
			t.Errorf("Tokens(%q) = %#v, want %#v", c.in, got, c.want)
		}
	}
}

// TestBothSidesSymmetric 是 P0 的专项断言：索引侧（IndexText）与查询侧（QueryText）
// 必须产出**同一组 token**——只要有一侧单独改了规则，这里就会失败。
func TestBothSidesSymmetric(t *testing.T) {
	corpus := []string{
		"认证",
		"令牌已过期，请重新认证",
		"用户身份认证服务在 2024 年上线",
		"SQLite FTS5 bigram 预分词",
		"我 更喜欢 用 Go 写 存储层",
	}
	for _, s := range corpus {
		want := Tokens(s)
		if got := tokensOfIndexText(IndexText(s)); !equal(got, want) {
			t.Errorf("索引侧 token 不对称: IndexText(%q) → %#v, want %#v", s, got, want)
		}
		if got := tokensOfQueryText(QueryText(s)); !equal(got, want) {
			t.Errorf("查询侧 token 不对称: QueryText(%q) → %#v, want %#v", s, got, want)
		}
	}
}

// TestQuerySubstringHitsBigrams：查询"认证"必须能匹配"用户身份认证失败"的 bigram 集合。
func TestQuerySubstringHitsBigrams(t *testing.T) {
	indexed := IndexText("用户身份认证失败")
	want := Tokens("认证")[0]
	found := false
	for _, tok := range strings.Fields(indexed) {
		if tok == want {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("索引文本 %q 中未找到 bigram %q", indexed, want)
	}
}

// TestQueryTextEmpty 保证没有 token 的查询返回空表达式，调用方据此跳过检索。
func TestQueryTextEmpty(t *testing.T) {
	if got := QueryText("！？"); got != "" {
		t.Errorf("QueryText(标点) = %q, want empty", got)
	}
	if got := QueryText("  "); got != "" {
		t.Errorf("QueryText(空白) = %q, want empty", got)
	}
}

func tokensOfIndexText(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, " ")
}

// tokensOfQueryText 从 MATCH 表达式里还原 token 序列（去引号、去 OR）。
func tokensOfQueryText(q string) []string {
	if q == "" {
		return nil
	}
	parts := strings.Split(q, " OR ")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.Trim(p, `"`))
	}
	return out
}

func equal(a, b []string) bool {
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
