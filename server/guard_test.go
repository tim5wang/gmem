package server_test

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tim5wang/gmem/server"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// 守卫：分层方向 / 只读优先 / 常量一致
//
// 这三条都是"违反了不会报错、只会悄悄变错"的类型，因此必须用结构性手段钉死：
//   - 分层：server import sqlite → SQL 泄漏到接口层，D6 双后端作废；
//   - 只读：多一条写路由 → "只读优先"的承诺失效，且没人会注意到；
//   - 常量：possiblySame 字符串漂移 → 裁决永远找不到待审边（表现为"没有待审项"）。
// ===========================================================================

// TestServerDoesNotImportSQLite 是本卡的**依赖方向守卫**。
//
// 判据：server 包的生产源码（不含 _test.go）不得 import 任何后端/驱动包。
// 一旦 import，接口层就与 schema 绑定，"换后端只换注入"不再成立。
func TestServerDoesNotImportSQLite(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	// 这些 import 路径一律禁止出现在 server 的生产源码里。
	forbidden := []string{
		"github.com/tim5wang/gmem/sqlite",
		"database/sql",
		"modernc.org/sqlite",
	}
	var scanned, imports int
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		file, err := parser.ParseFile(token.NewFileSet(), f, src, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		scanned++
		for _, imp := range file.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			imports++
			for _, bad := range forbidden {
				if path == bad {
					t.Errorf("%s: import 了 %q —— server 只允许依赖 store/recall/ops，"+
						"SQL 与 schema 必须留在后端包内（由 cmd/gmem 在装配时注入）", f, path)
				}
			}
		}
	}
	// 自定义错误：扫描空转必须失败，避免"守卫永远绿"。
	if scanned == 0 {
		t.Fatal("没有扫描到任何 server 生产源码（守卫失效）")
	}
	if imports == 0 {
		t.Fatal("没有看到任何 import（AST 解析空转，守卫已失效）")
	}
}

// TestOnlyReviewRouteWrites 扫路由表，断言**写路由集合恰好是 {POST /api/review}**。
//
// 这条用例的存在意义：新增一条写路由必须同时改这里，想悄悄加写入口会直接变红。
func TestOnlyReviewRouteWrites(t *testing.T) {
	h := newHarness(t)
	routes := h.srv.Routes()
	if len(routes) == 0 {
		t.Fatal("Routes() 为空（守卫失效）")
	}
	var writes []server.Route
	for _, r := range routes {
		switch r.Method {
		case http.MethodGet:
			// 读路由：允许。
		case http.MethodPost:
			writes = append(writes, r)
		default:
			t.Errorf("出现了未申报的方法 %s %s（只读优先：除裁决外不得有写路径）", r.Method, r.Pattern)
		}
	}
	if len(writes) != 1 {
		t.Fatalf("写路由必须恰好 1 条（POST /api/review），实际 %d 条：%+v", len(writes), writes)
	}
	if writes[0].Method != http.MethodPost || writes[0].Pattern != "/api/review" {
		t.Fatalf("唯一的写路由必须是 POST /api/review，实际 %s %s", writes[0].Method, writes[0].Pattern)
	}
}

// TestRoutesMatchRegisteredHandlers 路由表与实际注册必须一致（声明了却没人接 = 假路由）。
func TestRoutesMatchRegisteredHandlers(t *testing.T) {
	h := newHarness(t)
	h.node("alice", "Alice", store.ScopeUser, "")
	for _, r := range h.srv.Routes() {
		// 用对应方法打一次：GET 路由不该回 405，POST 路由不该回 405。
		var status int
		path := r.Pattern
		if strings.HasSuffix(path, "/") {
			path += "alice"
		}
		if r.Method == http.MethodGet {
			status, _ = h.get(path)
		} else {
			status, _ = h.postJSON(path, `{}`)
		}
		if status == http.StatusMethodNotAllowed || status == http.StatusNotFound {
			t.Errorf("路由 %s %s 声明了但没有正确注册（status=%d）", r.Method, r.Pattern, status)
		}
	}
}

// directWriteViolations 扫描一份源码，返回"直写记忆"的违规描述。
//
// 抽成纯函数（而不是写在 Test 里）是为了让**扫描器本身**可被测试：
// 变异用例 TestGuardDetectsBypassingWriter 拿一段故意违规的源码喂它，
// 断言它真的会报错。否则"守卫永远绿"与"守卫真的在工作"在外观上无法区分。
func directWriteViolations(filename string, src []byte) ([]string, error) {
	forbiddenMethods := map[string]bool{
		"PutMemory": true, "PutMemories": true, "InsertMemory": true, "UpsertMemory": true,
	}
	file, err := parser.ParseFile(token.NewFileSet(), filename, src, 0)
	if err != nil {
		return nil, err
	}
	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if forbiddenMethods[sel.Sel.Name] {
			out = append(out, filename+": 引用了 "+sel.Sel.Name+
				" —— server 不得直写记忆（必须经 recall.Writer，否则 FTS 会静默漏索引）")
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == "recall" && sel.Sel.Name == "NewWriter" {
			out = append(out, filename+": 引用了 recall.NewWriter —— 写入编排属于写入侧，"+
				"接口层不该自己造一个写入口")
		}
		return true
	})
	return out, nil
}

// TestServerNeverWritesMemoriesDirectly 扫 server 生产源码，禁止任何"直写记忆"的 API。
//
// 为什么 server 也不该自己写记忆：绕过 recall.Writer 会让该条记忆在词法通道上
// 永远查不到（D13 的遗留缺陷，表现为"答案变差"而不是"程序出错"）。
// 变异验证见 TestGuardDetectsBypassingWriter（证明扫描器真的会红）。
func TestServerNeverWritesMemoriesDirectly(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	var scanned, selectors int
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		// 统计 SelectorExpr 以证明解析不是空转（directWriteViolations 内部已 Inspect）。
		file, err := parser.ParseFile(token.NewFileSet(), f, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		selectors += strings.Count(string(src), ".")
		scanned++
		violations, err := directWriteViolations(f, src)
		if err != nil {
			t.Fatalf("scan %s: %v", f, err)
		}
		for _, v := range violations {
			t.Error(v)
		}
		_ = file
	}
	if scanned == 0 {
		t.Fatal("没有扫描到任何 server 生产源码（守卫失效）")
	}
	if selectors == 0 {
		t.Fatal("扫描没有看到任何选择子（解析器空转，守卫已失效）")
	}
}

// TestGuardDetectsBypassingWriter 是"绕过应变红"的变异验证。
//
// 它直接喂违规源码给扫描器，断言扫描器**确实报错**。
// 这是上一条用例能成立的前提：如果扫描器对违规代码也保持沉默，
// 那 TestServerNeverWritesMemoriesDirectly 的绿就毫无意义。
func TestGuardDetectsBypassingWriter(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "直写 Store.PutMemory",
			src: `package server
import "context"
func bad(ctx context.Context, s interface{ PutMemory() error }) { _ = s.PutMemory() }
`,
			want: "PutMemory",
		},
		{
			name: "自己造写入编排",
			src: `package server
import "github.com/tim5wang/gmem/recall"
func bad() { _ = recall.NewWriter }
`,
			want: "recall.NewWriter",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := directWriteViolations("synthetic.go", []byte(tc.src))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(got) == 0 {
				t.Fatalf("变异验证失败：扫描器对违规源码保持沉默（漏报 %s）——"+
					"守卫已失效，TestServerNeverWritesMemoriesDirectly 的绿不可信", tc.want)
			}
			joined := strings.Join(got, "\n")
			if !strings.Contains(joined, tc.want) {
				t.Fatalf("报错信息未指出 %q：%s", tc.want, joined)
			}
		})
	}

	// 反向：合规源码不得被误报（避免守卫变成"永远红"而被人关掉）。
	clean := `package server
import "github.com/tim5wang/gmem/store"
func ok(s store.Store) { _ = s.GetMemory }
`
	got, err := directWriteViolations("clean.go", []byte(clean))
	if err != nil {
		t.Fatalf("parse clean: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("合规源码被误报（守卫过严会被人关掉）：%v", got)
	}
}

// TestRelationConstantsMatchOps 断言 server 与 ops 对两条记账关系的取值一致。
//
// server 刻意没有 import ops 的私有常量（见 review.go 注释），因此这里有漂移风险。
// 用**行为**而不是字面量验证：让 ops 写出待审边，再看 review 是否认得它。
// 这是比字符串比较更强的断言——它直接测"裁决能不能找到待审项"。
func TestRelationConstantsMatchOps(t *testing.T) {
	h := newHarness(t)
	h.node("n1", "N1", store.ScopeUser, "")
	h.node("n2", "N2", store.ScopeUser, "")
	// 用与 ops 相同的计数器关系值造边（取值漂移时这条用例必须变红）。
	h.edge("e-ps", "n1", "n2", store.Relation("possiblySame"), 1, 100, 0)

	status, body := h.postJSON("/api/review", `{"edge_id":"e-ps","verdict":"diff","confirmed":true}`)
	if status != http.StatusOK {
		t.Fatalf("server 未认出 ops 的 possiblySame 取值（status=%d body=%s）："+
			"review.go 里的 relationPossiblySame 与 ops/ops_write.go 漂移了", status, body)
	}
	edges, err := h.db.OpsBackend().ListEdges(context.Background(), store.ScopeUser, 0)
	if err != nil {
		t.Fatalf("ListEdges: %v", err)
	}
	var found bool
	for _, e := range edges {
		if string(e.Relation) == "negative_SameAs" {
			found = true
		}
	}
	if !found {
		t.Fatal("裁决 diff 后未出现 negative_SameAs：server 的 relationNegativeSameAs 与实际写入值漂移了")
	}
}
