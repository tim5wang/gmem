package p1verify

import (
	"fmt"
	"os"
	"testing"
)

// ===========================================================================
// 缺陷钉桩（characterization / pin）机制
//
// 这些用例来自对 47cf9bf 的独立验证：3 个**阻塞合并**项与 3 个**可后续**项。
// 每个缺陷用例都是**双向口径**，同时保留两段断言：
//
//	current：断言"缺陷现状成立"（旧行为）；
//	fixed  ：断言"缺陷已修复"的正确行为。
//
// 极性已翻转到**修复后**（本卡完成修复）：
//
//	默认                           ：断言正确行为（修复验收口径，必须全绿）；
//	GMEM_P1_EXPECT_DEFECT=1        ：断言旧缺陷行为（变异验证口径）。
//
// 为什么保留 defect 分支：它是这些用例的**变异验证开关**——把生产代码改回旧行为后，
// 用 GMEM_P1_EXPECT_DEFECT=1 跑必须全绿，默认跑必须变红。这样既证明用例真的钉住了
// 缺陷（不是空断言），又不允许缺陷被悄悄改回去而用例继续"绿着"。
//
// 验收人两条命令：
//
//	go test ./internal/p1verify/                          # 修复后行为成立
//	GMEM_P1_EXPECT_DEFECT=1 go test ./internal/p1verify/   # 改回旧行为时缺陷仍可复现
// ===========================================================================

// pinKind 是缺陷等级（报告里的分类口径）。
type pinKind string

const (
	pinBlocker  pinKind = "阻塞合并"
	pinFollowUp pinKind = "可后续"
)

// pin 按等级与口径做断言。
//
// current 返回空串表示"缺陷现状符合预期"；fixed 返回空串表示"正确行为成立"。
func pin(t *testing.T, kind pinKind, id, title string, current, fixed func() string) {
	t.Helper()
	if os.Getenv("GMEM_P1_EXPECT_DEFECT") == "1" {
		// 变异验证口径：生产代码被改回旧行为时，缺陷必须仍可复现。
		if msg := current(); msg != "" {
			t.Errorf("%s %s [%s] 旧缺陷行为已不可复现（变异验证失效）：%s", kind, id, title, msg)
			return
		}
		t.Logf("%s %s [%s] 旧缺陷行为仍可复现（变异验证口径）", kind, id, title)
		return
	}
	// 默认口径：修复后的正确行为必须成立。
	if msg := fixed(); msg != "" {
		t.Errorf("%s %s [%s] 正确行为不成立：%s", kind, id, title, msg)
		return
	}
	t.Logf("%s %s [%s] 已修复：正确行为成立", kind, id, title)
}

// pinKnownLimitation 断言一个**本卡不修**的已知限制仍然存在。
//
// 与 pin 的区别：pin 的极性是“已修复”（本卡把它修了），而这一类是“明确不在本卡
// 范围内”的缺陷（例如 concept 扇出属 P2 抽取质量）。它们的用例价值在于**记录缺陷
// 现状**并防止被误以为已修，因此两种口径下都断言“缺陷仍可复现”，并额外提供
// GMEM_P1_EXPECT_DEFECT 的同名日志以免两套开关混淆。
func pinKnownLimitation(t *testing.T, kind pinKind, id, title, scope string, current func() string) {
	t.Helper()
	if msg := current(); msg != "" {
		t.Errorf("%s %s [%s] 已知限制的形态已变化（需重新评估）：%s", kind, id, title, msg)
		return
	}
	t.Logf("%s %s [%s] 已知限制仍存在（不在本卡范围：%s）", kind, id, title, scope)
}

// mustPanic 执行 fn 并把 panic 值转成字符串（无 panic 返回空串）。
func mustPanic(fn func()) (msg string) {
	defer func() {
		if r := recover(); r != nil {
			msg = fmt.Sprint(r)
		}
	}()
	fn()
	return ""
}

// sprintList 把列表渲染成一行（只在失败信息里用，避免各测试文件重复引入 fmt）。
func sprintList(v any) string { return fmt.Sprintf("%v", v) }
