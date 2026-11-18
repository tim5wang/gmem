package p1verify

import (
	"fmt"
	"os"
	"testing"
)

// ===========================================================================
// 缺陷钉桩（characterization / pin）机制
//
// 本次独立验证在 47cf9bf 上找到 3 个**阻塞合并**项与 3 个**可后续**项。
// 它们客观存在于生产代码里；若直接断言"正确行为"，`go test ./...` 会永久变红，
// 反而掩盖后续新回归（P0 也踩过这个坑，见 internal/vtest/repro_findings_test.go 的历史注释）。
//
// 因此每个缺陷用例都是**双向口径**：
//
//	默认                ：断言"缺陷现状成立"——把它钉成可复现的最小用例，全绿；
//	GMEM_P1_EXPECT_FIXED=1：断言"缺陷已修复"的正确行为。
//
// 验收人两条命令：
//
//	go test ./internal/p1verify/                         # 缺陷现状可复现
//	GMEM_P1_EXPECT_FIXED=1 go test ./internal/p1verify/  # 修复后转绿，即为验收通过
//
// 修复者一旦改好实现，默认口径会**变红**并在错误信息里指名要翻转断言——
// 这是刻意的强制收敛点：不允许缺陷被悄悄修掉而用例继续"绿着"。
// ===========================================================================

// pinKind 是缺陷等级（报告里的分类口径）。
type pinKind string

const (
	pinBlocker    pinKind = "阻塞合并"
	pinFollowUp   pinKind = "可后续"
)

// pin 按等级与口径做断言。
//
// current 返回空串表示"缺陷现状符合预期"；fixed 返回空串表示"正确行为成立"。
func pin(t *testing.T, kind pinKind, id, title string, current, fixed func() string) {
	t.Helper()
	if os.Getenv("GMEM_P1_EXPECT_FIXED") == "1" {
		if msg := fixed(); msg != "" {
			t.Errorf("%s %s [%s] 修复后仍不成立：%s", kind, id, title, msg)
			return
		}
		t.Logf("%s %s [%s] 已修复：正确行为成立", kind, id, title)
		return
	}
	if msg := current(); msg != "" {
		t.Errorf("%s %s [%s] 现状已变化 → 请翻转本用例断言：%s", kind, id, title, msg)
		return
	}
	t.Logf("%s %s [%s] 缺陷可复现（默认口径）；修复后用 GMEM_P1_EXPECT_FIXED=1 验收", kind, id, title)
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
