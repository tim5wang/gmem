package web_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tim5wang/gmem/web"
)

// ===========================================================================
// 目录边界守卫（docs/TD.md §9「工程约束」）
//
// 这条边界不违反也能"跑得挺好"：把 web/src 也 embed 进去、或者让 Go 代码
// 直接读 web/src 下的文件，日常完全看不出问题——直到有人在没装 Node 的机器上
// `go build` 失败，或者二进制里塞进几万个 node_modules 文件。
// 因此必须用结构性手段钉死，而不是写在文档里靠自觉。
// ===========================================================================

// repoRoot 从本包目录回到仓库根（web/ → ..）。
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	return root
}

// TestGoSourceNeverReferencesWebSrc 是全仓 Go 源码的守卫：
// **任何 .go 文件都不得出现 web/src 路径**。
//
// 判据是"路径出现"，不是"import 了包"：Go 无法 import npm 包，真正的风险是
// 有人图省事用 //go:embed ../web/src 或 os.ReadFile("web/src/...")，
// 那会让构建重新依赖前端目录（并可能把 node_modules 打进二进制）。
func TestGoSourceNeverReferencesWebSrc(t *testing.T) {
	root := repoRoot(t)
	var scanned, hits int
	var violations []string

	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// 跳过与"Go 源码"无关的重目录：.git / .godex（agent scratch）/ node_modules。
			switch d.Name() {
			case ".git", ".godex", "node_modules":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		scanned++
		rel, _ := filepath.Rel(root, p)
		for _, line := range strings.Split(string(src), "\n") {
			trimmed := strings.TrimSpace(line)
			// 注释里提到 web/src 是**说明**，不是依赖：只查代码行。
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") {
				continue
			}
			// 本文件自身必须提到这个字符串（就是判据本身）。
			if rel == filepath.Join("web", "embed_test.go") {
				continue
			}
			if strings.Contains(line, "web/src") {
				hits++
				violations = append(violations, rel+": "+trimmed)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if scanned == 0 {
		t.Fatal("没有扫描到任何 .go 文件（守卫失效）")
	}
	for _, v := range violations {
		t.Errorf("Go 源码里出现了 web/src 路径（构建不得依赖前端源码目录）: %s", v)
	}
	if hits > 0 {
		t.Fatalf("共 %d 处引用 web/src", hits)
	}
}

// TestEmbedContainsProduct 断言 embed 的东西确实是产物（不是空目录、不是源码）。
func TestEmbedContainsProduct(t *testing.T) {
	index, err := web.IndexHTML()
	if err != nil {
		t.Fatalf("IndexHTML: %v", err)
	}
	if len(index) == 0 {
		t.Fatal("index.html 为空")
	}
	html := string(index)
	for _, want := range []string{"<html", "assets/app.js", "assets/styles.css"} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html 缺少 %q（产物不完整？）", want)
		}
	}

	for _, name := range []string{"app.js", "app.js.map", "styles.css"} {
		body, ok := web.Asset(name)
		if !ok {
			t.Errorf("assets/%s 未被 embed（web/dist 未提交或未构建）", name)
			continue
		}
		if len(body) == 0 {
			t.Errorf("assets/%s 为空", name)
		}
	}
}

// TestAssetRejectsTraversal 资源读取只接受 assets/ 下的合法相对路径。
func TestAssetRejectsTraversal(t *testing.T) {
	for _, name := range []string{"", ".", "../embed.go", "..", "sub/../../x", `a\b`} {
		if _, ok := web.Asset(name); ok {
			t.Errorf("web.Asset(%q) 应当被拒绝，实际返回了内容", name)
		}
	}
}

// TestDistHasNoSrcResidue 断言产物里没有源码目录残留（构建脚本写错时的兜底）。
func TestDistHasNoSrcResidue(t *testing.T) {
	entries, err := fs.ReadDir(web.FS(), ".")
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	for _, n := range names {
		switch n {
		case "index.html", "assets":
			// 期望的两个入口。
		default:
			t.Errorf("web/dist 出现了意外的顶层条目 %q（dist 只应有 index.html 与 assets/）", n)
		}
	}
}
