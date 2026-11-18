package p3verify

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// ===========================================================================
// 发现清单的门禁与公共小工具
//
// 约定沿用 internal/vtest 与 internal/p2verify：**已知缺陷不得让 `go test ./...`
// 永久变红**（那会掩盖新回归）。因此：
//   - GMEM_VERIFY_BLOCKER=1  → 阻塞合并项所指的用例以红色失败；
//   - GMEM_VERIFY_DEFERRED=1 → 可后续项所指的用例以红色失败；
//   - 未置位时用例仍会**实测**并把事实打印出来（不让缺陷被静默遗忘）。
//
// 这样"默认绿"不会变成"假装没问题"：t.Log 里始终有实测到的数字。
// ===========================================================================

func blockerGate() bool  { return os.Getenv("GMEM_VERIFY_BLOCKER") == "1" }
func deferredGate() bool { return os.Getenv("GMEM_VERIFY_DEFERRED") == "1" }

// itoa 是测试日志里的小工具（避免为了拼一行日志引入 fmt 的噪音）。
func itoa(n int) string { return strconv.Itoa(n) }

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

// grepCount 数 dir 下**生产代码**（非 _test.go）里 needle 出现的次数。
//
// 为什么不用 grep 命令：源码扫描必须是测试的一部分（否则守卫会退化成"人工检查"），
// 而且路径相对于测试文件所在包，用 Go 自己读目录最稳。
func grepCount(t *testing.T, dir, needle string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取目录 %s: %v", dir, err)
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("读取 %s: %v", name, err)
		}
		n += strings.Count(string(b), needle)
	}
	return n
}
