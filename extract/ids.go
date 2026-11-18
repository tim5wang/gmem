package extract

import (
	"hash/fnv"
	"strings"
	"unicode"

	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// 稳定的标识符生成
//
// 全部用 **确定性哈希**（FNV-1a + 归一化名称/内容），不引入 uuid 依赖，
// 也不依赖随机数：同一输入重复跑必须得到同一 ID，否则"同一对反复写入"这类
// 用例（negative_SameAs 去重）无法成立——每次都会新建一条边/一个新节点。
// ===========================================================================

// slug 把任意名称归一成可读片段：小写、只留字母数字与汉字。
func slug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '_' || r == '.' || r == '/':
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	if out == "" {
		out = "unnamed"
	}
	// 限制长度，避免超长名称把主键撑爆（保留可读前缀 + 哈希后缀）。
	if len([]rune(out)) > 48 {
		out = string([]rune(out)[:48])
	}
	return out
}

// hash8 返回 8 位十六进制的 FNV-1a 摘要。
func hash8(parts ...string) string {
	h := fnv.New32a()
	for i, p := range parts {
		if i > 0 {
			_, _ = h.Write([]byte{0})
		}
		_, _ = h.Write([]byte(strings.ToLower(strings.TrimSpace(p))))
	}
	const hex = "0123456789abcdef"
	v := h.Sum32()
	buf := make([]byte, 8)
	for i := 0; i < 8; i++ {
		buf[7-i] = hex[v&0xf]
		v >>= 4
	}
	return string(buf)
}

// conceptIDFromName 由概念名派生稳定 concept id（D15 的隐式 draft 也会用它）。
func conceptIDFromName(name string) string {
	return "c-" + slug(name) + "-" + hash8("concept", name)
}

// NodeID 由 scope + 名称派生稳定节点 id。
//
// scope 参与哈希：同一名称在 user 与 agent 两个 scope 必须是**不同节点**，
// 否则跨 scope 的实体被静默合并（§2 范围隔离）。
func NodeID(scope store.Scope, name string) string {
	return "n-" + slug(name) + "-" + hash8("node", string(scope), name)
}

// MemoryID 由 scope + 内容派生稳定记忆 id（同内容重写 = 同一版本，幂等）。
func MemoryID(scope store.Scope, content string) string {
	return "m-" + hash8("memory", string(scope), content)
}

// EdgeID 由两端与关系派生稳定边 id。
func EdgeID(from, to string, rel store.Relation) string {
	return "e-" + hash8("edge", from, string(rel), to)
}

// AliasID 由 surface 与目标节点派生稳定 alias 键。
func AliasID(surface, nodeID string) string {
	return slug(surface) + "@" + hash8("alias", surface, nodeID)
}

// pairKey 是两个节点 id 的无序对键，用于 negative_SameAs / possiblySame 去重。
func pairKey(a, b string) string {
	// 排序保证 (a,b) 与 (b,a) 得到同一个键（重复写入不得重复进待审池）。
	if a > b {
		a, b = b, a
	}
	return a + "\x00" + b
}
