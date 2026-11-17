// Package bigram 提供 Gmem 的中文 bigram 预分词。
//
// 为什么需要预分词：FTS5 的 unicode61 分词器把一整段连续的 CJK 字符当作**一个**
// token，于是"身份认证服务"只能被整段匹配，"认证"永远召回不到它。
//
// 解决办法是索引侧和查询侧做**同一套** bigram 切分：
//
//	索引侧：IndexText(doc) → 写入 memories_fts.bigrams 列
//	查询侧：QueryText(q)   → 生成 MATCH 表达式
//
// 两侧都必须调用本包的 Tokens，任何一侧单独改规则都会破坏对称性（P0 的核心验收）。
package bigram

import (
	"strings"
	"unicode"
)

// Tokens 返回 s 的 FTS token 序列。
//
// 规则（索引侧与查询侧共用，不得分叉）：
//   - 连续 CJK（Han）串：切成重叠 bigram，如"认证服务" → ["认证","证服","服务"]；
//     单字串保留该单字，否则会被完全丢弃。
//   - 连续拉丁字母/数字串：整段作为一个小写 token，如"G1" → ["g1"]。
//   - 其他字符（空白、标点、下划线、连字符……）作为分隔符丢弃。
func Tokens(s string) []string {
	rs := []rune(s)
	toks := make([]string, 0, len(rs))
	for i := 0; i < len(rs); {
		switch {
		case isHan(rs[i]):
			j := i
			for j < len(rs) && isHan(rs[j]) {
				j++
			}
			run := rs[i:j]
			if len(run) == 1 {
				toks = append(toks, string(run))
			} else {
				for k := 0; k+1 < len(run); k++ {
					toks = append(toks, string(run[k:k+2]))
				}
			}
			i = j
		case isWord(rs[i]):
			j := i
			for j < len(rs) && isWord(rs[j]) {
				j++
			}
			toks = append(toks, strings.ToLower(string(rs[i:j])))
			i = j
		default:
			i++
		}
	}
	return toks
}

// IndexText 返回写入 FTS5 列的值：token 以空格连接。
//
// 空格连接的意义是让 FTS5 把每个 bigram 当成独立 token，
// 而不是被 unicode61 再合并回一整段 CJK。
func IndexText(s string) string {
	return strings.Join(Tokens(s), " ")
}

// QueryText 返回 FTS5 的 MATCH 表达式：token 逐个加引号后以 OR 连接。
//
// 为什么是 OR 而不是 AND：bigram 切分会产出大量伪相邻二元组（"认证令牌" →
// 认证 / 证令 / 令牌），其中"证令"在真实文本里几乎不出现。若用 AND，
// 多字查询会直接零命中。OR 把"命中哪些 token"交给 bm25 排序，
// 而融合侧只看 rank，因此不影响 RRF 的正确性。
//
// 查询无任何 token 时返回空串，调用方应跳过检索。
func QueryText(s string) string {
	toks := Tokens(s)
	if len(toks) == 0 {
		return ""
	}
	parts := make([]string, 0, len(toks))
	for _, t := range toks {
		parts = append(parts, `"`+strings.ReplaceAll(t, `"`, `""`)+`"`)
	}
	return strings.Join(parts, " OR ")
}

func isHan(r rune) bool { return unicode.Is(unicode.Han, r) }

func isWord(r rune) bool {
	if isHan(r) {
		return false
	}
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}
