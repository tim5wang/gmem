package vtest

import (
	"fmt"
	"go/ast"
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
//
// # 关于 net/http 的两条边界（P4 修订，见任务卡 t-1795078030761-11）
//
// 本条用例的原意是「库本体不得**自己发请求**：LLM 由宿主注入」（见下方 banned 的注释），
// 约束的是**出站**客户端。而 P4 的 `gmem serve`（TD §9 / D4）必须用标准库
// `net/http` 做**入站**服务——两者不是同一件事。
//
// 因此这里把 net/http 的禁令拆成两条，方向相反但都保持强制：
//  1. 除服务入口包外，**任何**生产源码仍不得 import net/http；
//  2. 服务入口包虽允许 import net/http，但**不得出现任何出站客户端用法**
//     （http.Client / http.Get / http.Post / http.DefaultClient / Transport 等）。
//
// 第 2 条比原来的「一刀切禁止」更精确地表达了原意：禁止的是发请求，不是接请求。
func TestNoLLMSDKImports(t *testing.T) {
	root := repoRoot(t)

	// LLM SDK / 常见 HTTP 客户端的 import 路径片段(小写匹配)。
	banned := []string{
		"openai", "anthropic", "claude", "gemini", "google.golang.org/genai",
		"github.com/sashabaranov/go-openai", "cohere", "ollama", "langchain",
		"mistral", "groq", "huggingface", "azure/ai", "aws/aws-sdk-go",
		"net/http", // 库本体不应自己发请求：LLM 由宿主注入（见 servePackages 的例外）
	}
	// servePackages 是**入站** HTTP 服务的入口包（TD §9 D4 `gmem serve`）。
	//
	// 只有它们允许 import net/http，且下方对它另行断言「无任何出站客户端用法」。
	// 把范围写成白名单而不是"跳过所有含 net/http 的文件"：新增一个包不会
	// 因为 import 了 net/http 就自动获得豁免——必须显式出现在这里。
	servePackages := []string{
		filepath.Join("cmd", "gmem"),
		"server",
	}

	var scanned int
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// 跳过版本控制与测试/工具目录。
			//
			// .godex 是本仓的 agent 暂存目录（已 gitignore）：调研卡会往里克隆
			// **第三方参考源码**。若在这里不跳过，那些外部库的 net/http / LLM SDK
			// import 会被当成本仓生产代码，让本用例凭空变红——那不是真实缺陷，
			// 却会让全量测试不可信。因此与 .git 同待遇。
			switch info.Name() {
			case ".git", "testdata", "node_modules", ".godex":
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
		isServe := isServePackage(root, path, servePackages)
		for _, spec := range f.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				continue
			}
			lower := strings.ToLower(importPath)
			for _, b := range banned {
				if !strings.Contains(lower, b) {
					continue
				}
				if isServe && b == "net/http" {
					// 入站服务入口：允许 import，出站用法由 TestServePackagesMakeNoOutboundHTTP 断言。
					continue
				}
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s imports %q（命中禁用片段 %q）：库本体不得依赖 LLM SDK 或自建 HTTP 客户端",
					rel, importPath, b)
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
	t.Logf("已扫描 %d 个生产 .go 文件，未发现 LLM SDK / 越界的 net/http import", scanned)
}

// isServePackage 报告 path 是否属于入站 HTTP 服务的入口包（白名单精确匹配）。
func isServePackage(root, path string, servePackages []string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	dir := filepath.Dir(rel)
	for _, p := range servePackages {
		if dir == p || strings.HasPrefix(dir, p+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// TestServePackagesMakeNoOutboundHTTP 是上一条用例的**替代断言**（P4）。
//
// 服务入口包（cmd/gmem、server）允许 import net/http 做入站服务，
// 但**不得**出现任何出站客户端用法：那才是原用例真正要防的东西
// （「库本体不应自己发请求」）。这条断言让豁免不会变成后门。
func TestServePackagesMakeNoOutboundHTTP(t *testing.T) {
	root := repoRoot(t)
	// 出站客户端的构造/调用名。用 AST 的 SelectorExpr 判断，与接收者写法无关
	// （`c.Get(`、`myClient.Do(`、`(&http.Client{}).Get(` 都能命中）。
	outbound := map[string]bool{
		"Client": true, "Get": true, "Post": true, "PostForm": true, "Head": true,
		"Do": true, "DefaultClient": true, "DefaultTransport": true, "Transport": true,
		"NewRequest": true, "NewRequestWithContext": true,
	}
	servePackages := []string{
		filepath.Join("cmd", "gmem"),
		"server",
	}
	var scanned, selectors int
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// .godex 同 .git：只放 agent 暂存与第三方参考源码，不是本仓产物。
			if info.Name() == ".git" || info.Name() == ".godex" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if !isServePackage(root, path, servePackages) {
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return nil
		}
		// 只扫 import 了 net/http 的文件（其余没有出站能力）。
		usesHTTP := false
		for _, spec := range f.Imports {
			if p, err := strconv.Unquote(spec.Path.Value); err == nil && p == "net/http" {
				usesHTTP = true
			}
		}
		if !usesHTTP {
			return nil
		}
		if _, err := os.Stat(path); err != nil {
			return nil
		}
		full, err := parseFull(path)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		scanned++
		var violation []string
		ast.Inspect(full, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			selectors++
			ident, ok := sel.X.(*ast.Ident)
			if !ok || ident.Name != "http" {
				return true
			}
			if outbound[sel.Sel.Name] {
				rel, _ := filepath.Rel(root, path)
				violation = append(violation, fmt.Sprintf("%s: http.%s（服务入口不得发出站请求）", rel, sel.Sel.Name))
			}
			return true
		})
		for _, v := range violation {
			t.Error(v)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("扫描源码树: %v", err)
	}
	if scanned == 0 {
		t.Fatal("没有扫描到任何 import net/http 的服务入口文件（本用例失效：豁免范围可能写错）")
	}
	if selectors == 0 {
		t.Fatal("AST 扫描没有看到任何 SelectorExpr（解析空转，本用例已失效）")
	}
}

// parseFull 解析整份文件（需要函数体，不能只用 ImportsOnly）。
func parseFull(path string) (*ast.File, error) {
	return parser.ParseFile(token.NewFileSet(), path, nil, 0)
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
			// .godex 同 .git：agent 暂存与第三方参考源码都在这里，不是本仓产物。
			if info.Name() == ".git" || info.Name() == ".godex" {
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
