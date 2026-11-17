package vtest

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// ===========================================================================
// 零网络依赖 / 无 LLM SDK：静态证明
//
// 验收要求两条：
//   1. 断网或 GONOSUMDB 环境下测试仍全部通过；
//   2. grep 确认无任何 LLM SDK import。
//
// 第 1 条只能由"实际在无网环境跑一遍"来证明（见报告里的 GOFLAGS=-mod=mod GOPROXY=off 运行记录）；
// 本文件做的是可重复的**静态**代理证明：扫全部生产源码与 go.mod，
// 断言不存在任何 LLM SDK / HTTP 客户端依赖，且第三方依赖只有 modernc 系列。
// ===========================================================================

// TestNoLLMSDKImports 扫描全部生产 .go 文件，禁止出现 LLM SDK / HTTP 客户端 import。
//
// 这是 docs/TD.md §13 证据边界里 "never imports an LLM SDK" 的机器化检查。
func TestNoLLMSDKImports(t *testing.T) {
	root := repoRoot(t)

	// LLM SDK / 常见 HTTP 客户端的 import 路径片段（小写匹配）。
	banned := []string{
		"openai", "anthropic", "claude", "gemini", "google.golang.org/genai",
		"github.com/sashabaranov/go-openai", "cohere", "ollama", "langchain",
		"mistral", "groq", "huggingface", "azure/ai", "aws/aws-sdk-go",
		"net/http", // 库本体不应自己发请求：LLM 由宿主注入
	}

	var scanned int
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// 跳过版本控制与测试/工具目录
			switch info.Name() {
			case ".git", "testdata", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// 只看生产代码：跳过本验证包自身与仓库内既有 _test.go
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		scanned++

		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return nil // 解析失败不在此用例负责范围
		}
		for _, spec := range f.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				continue
			}
			lower := strings.ToLower(importPath)
			for _, b := range banned {
				if strings.Contains(lower, b) {
					rel, _ := filepath.Rel(root, path)
					t.Errorf("%s imports %q（命中禁用片段 %q）：库本体不得依赖 LLM SDK 或自建 HTTP 客户端",
						rel, importPath, b)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("扫描源码树: %v", err)
	}
	if scanned == 0 {
		t.Fatalf("未扫描到任何生产 .go 文件，用例失效（repoRoot 可能算错）")
	}
	t.Logf("已扫描 %d 个生产 .go 文件，未发现 LLM SDK / net/http import", scanned)
}

// TestDependencyAllowlist 断言第三方依赖只有现代化的 SQLite 栈。
//
// 这条用例的意义：一旦有人为了"顺手"引入某个 SDK，go.mod 会变，
// 这里立刻失败，从而把"零 SDK 依赖"从口头约定变成 CI 可执行的断言。
func TestDependencyAllowlist(t *testing.T) {
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("读 go.mod: %v", err)
	}

	// 允许的第三方模块前缀（modernc 的 SQLite 实现及其传递依赖）。
	allowed := []string{
		"module github.com/tim5wang/gmem",
		"go ",
		"require (",
		"require modernc.org/sqlite ", // 单行 require 形态
		")",
		"modernc.org/sqlite",
		"modernc.org/libc",
		"modernc.org/mathutil",
		"modernc.org/memory",
		"github.com/dustin/go-humanize",
		"github.com/google/uuid",
		"github.com/mattn/go-isatty",
		"github.com/ncruces/go-strftime",
		"github.com/remyoudompheng/bigfft",
		"golang.org/x/exp",
		"golang.org/x/sys",
	}

	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") {
			continue
		}
		ok := false
		for _, a := range allowed {
			if strings.HasPrefix(trimmed, a) {
				ok = true
				break
			}
		}
		if !ok {
			t.Errorf("go.mod 出现未列入白名单的依赖行: %q（若确需新增，请显式更新本用例并说明理由）", trimmed)
		}
	}
}

// TestNoCGOImport 断言没有 CGO 依赖（CGO_ENABLED=0 是交付前提）。
func TestNoCGOImport(t *testing.T) {
	root := repoRoot(t)
	var cgoFiles []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if strings.Contains(string(data), "import \"C\"") {
			rel, _ := filepath.Rel(root, path)
			cgoFiles = append(cgoFiles, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("扫描源码树: %v", err)
	}
	if len(cgoFiles) > 0 {
		t.Errorf("发现 CGO 依赖（CGO_ENABLED=0 下会编译失败）: %v", cgoFiles)
	}
}

// repoRoot 从测试文件位置向上找到含 go.mod 的目录。
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("向上未找到 go.mod（起点 %s）", dir)
		}
		dir = parent
	}
}
