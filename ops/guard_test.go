package ops_test

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tim5wang/gmem/bigram"
	"github.com/tim5wang/gmem/ops"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// 10) 守卫：ops 不得绕过 recall.Writer 写 memories
//
// 这条缺陷很特殊：绕过写入编排**不会报错**，它只是让这条记忆在词法通道上永远查不到，
// 表现为"答案变差"而不是"程序出错"（D13 遗留的 P1 必做项）。
// 因此用两个层次钉死：
//  1. **静态守卫**：ops 的生产源码里不得出现任何 memories 写入 API（Store.PutMemory 等）；
//  2. **变异验证**：证明"绕过确实会坏"——直写 Store 的记忆在 FTS 里查不到，而经 Writer
//     写入的能查到。这条用例存在的意义是：如果哪天有人把守卫删了，它仍然说明为什么该守卫存在。
// ===========================================================================

// TestOpsNeverWritesMemoriesDirectly 扫描 ops 包的生产源码（不含 _test.go）。
//
// # 为什么用 AST 而不是字面量匹配（T-7）
//
// 旧实现只查字面量 `"PutMemory("`：`tx.PutMemory (`（空格）、`st["Put"+"Memory"]`
// 或把接收者换成别的变量名都能绕过。这类守卫一旦能被绕过，就只是“看起来有守卫”。
// 现在用 go/ast 判断**调用的方法名**与**构造的字符串常量**，与接收者写法无关。
func TestOpsNeverWritesMemoriesDirectly(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	// 这些是"写 memories **内容**"的方法名。任何一处调用都是缺陷。
	//
	// 为什么 InvalidateMemory / TouchRecall 不在此列（刻意的，不是漏掉）：
	//   - InvalidateMemory 只置 t_invalid（**世界时间**），不改 content/kind/concept_id，
	//     因此按 D13 不产生新版本、FTS 也无需重同步；
	//   - TouchRecall 只改运营计数 recall_count，D13 明确它不得催生版本。
	// 真正危险的只有内容写入 PutMemory：它写主表却不写 FTS，漏了也不报错。
	forbiddenMethods := map[string]bool{
		"PutMemory": true, "PutMemories": true, "InsertMemory": true, "UpsertMemory": true,
	}
	// 自定义错误：^检查器自己找不到东西时必须失败（避免“扫描空转但永远绿”）。
	var scanned, calls int
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		scanned++
		file, err := parser.ParseFile(token.NewFileSet(), f, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CallExpr:
				calls++
				if sel, ok := node.Fun.(*ast.SelectorExpr); ok && forbiddenMethods[sel.Sel.Name] {
					t.Errorf("%s: 调用了 %s：ops 不得自行写 memories（必须经 recall.Writer，"+
						"否则 FTS 会静默漏索引）", f, sel.Sel.Name)
				}
			case *ast.SelectorExpr:
				// 捕获 `x.PutMemory` 这类**引用**（不仅调用），以及 recall.NewWriter。
				if forbiddenMethods[node.Sel.Name] {
					t.Errorf("%s: 引用了 %s：ops 不得自行写 memories（必须经 recall.Writer）", f, node.Sel.Name)
				}
				if id, ok := node.X.(*ast.Ident); ok && id.Name == "recall" && node.Sel.Name == "NewWriter" {
					t.Errorf("%s: 引用了 recall.NewWriter：写入编排应由 Extractor/调用方注入，"+
						"ops 不应自己造一个写入入口", f)
				}
			}
			return true
		})
	}
	if scanned == 0 {
		t.Fatal("没有扫描到任何 ops 生产源码（守卫失效）")
	}
	if calls == 0 {
		t.Fatal("AST 扫描没有看到任何 CallExpr（解析器空转，守卫已失效）")
	}
}

// TestBypassingWriterLosesLexicalIndex 是"绕过应变红"的变异验证。
//
// 两个语义相同的记忆：一个经 recall.Writer 写入，一个直写 Store.PutMemory。
// 断言后者在词法通道上**查不到**——这正是守卫要防的静默缺陷。
func TestBypassingWriterLosesLexicalIndex(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	const query = "认证服务"
	content := "认证服务的部署环境是预发布集群"

	// 路径 A：经写入编排（正确做法）。
	if _, err := recalledWriterPut(e, store.Memory{
		ID: "m-via-writer", Scope: store.ScopeUser, Kind: store.KindSemantic,
		Content: content, TValid: 100, TCreated: 100,
	}); err != nil {
		t.Fatalf("经 Writer 写入: %v", err)
	}
	// 路径 B：绕过写入编排（缺陷做法）。
	if err := e.db.Store().PutMemory(ctx, store.Memory{
		ID: "m-bypass", Scope: store.ScopeUser, Kind: store.KindSemantic,
		Content: content, TValid: 100, TCreated: 100,
	}); err != nil {
		t.Fatalf("直写 Store: %v", err)
	}

	hits, err := e.db.Lexical().Search(ctx, store.LexicalQuery{
		Query: query, Scope: store.ScopeUser, Limit: 10,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	var sawWriter, sawBypass bool
	for _, h := range hits {
		switch h.ID {
		case "m-via-writer":
			sawWriter = true
		case "m-bypass":
			sawBypass = true
		}
	}
	if !sawWriter {
		t.Fatalf("经 Writer 写入的记忆在词法通道查不到（写入编排本身有问题）：hits=%v", hits)
	}
	if sawBypass {
		t.Fatalf("直写 Store 的记忆也被索引到了——说明这个后端不靠写入编排同步 FTS，" +
			"本守卫的前提不成立，需要重新设计断言")
	}
	// 结论必须可复述：绕过 = 主表有、索引无。这正是"答案变差而不报错"的形状。
	if got := e.countRows(`SELECT COUNT(1) FROM memories WHERE id='m-bypass'`); got != 1 {
		t.Errorf("直写记忆在主表里=%d 行, want 1（主表有、索引无才是问题所在）", got)
	}
	if got := e.countRows(`SELECT COUNT(1) FROM memories_fts WHERE id='m-bypass'`); got != 0 {
		t.Errorf("直写记忆在 FTS 里=%d 行, want 0", got)
	}
	// bigram 双侧对称是词法可检索的前提；这里顺带断言索引侧确实用了它。
	if !strings.Contains(bigram.IndexText(content), bigram.Tokens("认证")[0]) {
		t.Error("索引侧未使用 bigram 预转换（词法通道对中文两字查询会失效）")
	}
}

// TestExtractVerifiesLexicalIndex 断言 Extract 会核验写入结果里的 LexicalIndexed，
// 缺失时显式降级而不是假装成功。
func TestExtractVerifiesLexicalIndex(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.db.Store().PutEpisode(ctx, store.Episode{
		ID: "ep-1", Scope: store.ScopeUser, Content: "一段需要提取的语料正文", At: 100,
	}); err != nil {
		t.Fatalf("put episode: %v", err)
	}

	// 伪造一个"写入但索引没同步"的 Extractor：这正是 D13 缺陷的形状。
	bad := ops.ExtractFunc(func(_ context.Context, ep ops.EpisodeInput) (ops.ExtractOutcome, error) {
		return ops.ExtractOutcome{
			Writes: []ops.WriteOutcome{{MemoryID: "m-x", Version: 1, LexicalIndexed: false}},
		}, nil
	})
	r, err := ops.NewRunner(e.db.Ports(), e.db.OpsBackend(),
		ops.WithConfig(e.cfg), ops.WithExtractor(bad))
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	res, err := r.Run(ctx, ops.Request{Op: ops.OpExtract, Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Degraded) == 0 {
		t.Fatal("词法索引未同步时必须显式降级（不得静默）")
	}
	var found bool
	for _, d := range res.Degraded {
		if strings.Contains(d, "词法索引未同步") {
			found = true
		}
	}
	if !found {
		t.Errorf("降级信息未点明词法索引：%v", res.Degraded)
	}
}

// TestSingleOpInstanceAndSingleEntryPoint 是 D3 的**结构性**实证（T-5）。
//
// # 为什么行为断言不够
//
// TestAutoAndAgentShareSameCodePath 与 TestFourTriggerSourcesProduceSameBehavior 都是
// “两条路径都调 e.runner.Run，只换 Trigger.Kind”。真出现两套实现时（例如给 Agent
// 另建一个 runner、或某个触发器直接调 op.Apply），这些用例仍是同一份 Run，照样绿。
//
// 因此这里从**源码结构**上钉死两件事：
//  1. `op.Plan(...)` / `op.Apply(...)` 只允许出现在 runner.go（唯一编排点）；
//  2. 触发器与维护面只能调 `runner.Run`，不得自己调 Plan/Apply。
func TestSingleOpInstanceAndSingleEntryPoint(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	// 允许直接调 Plan/Apply 的文件（唯一编排点 + 它自己的契约文档）。
	allowed := map[string]bool{"runner.go": true}
	// 这两个标识符在测试里用来校验“只能出现在 runner.go”。
	var violations int
	var runCalls int
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if sel.Sel.Name == "Run" {
				runCalls++
			}
			if sel.Sel.Name == "Plan" || sel.Sel.Name == "Apply" {
				if !allowed[f] {
					violations++
					t.Errorf("%s: 直接调用了 %s——op 只能经 Runner 编排（D3：两条触发路径不得分叉）",
						f, sel.Sel.Name)
				}
			}
			return true
		})
	}
	if violations != 0 {
		t.Fatalf("发现 %d 处绕过 Runner 的 Plan/Apply 调用", violations)
	}
	// 反向守卫：触发器/维护面必须**真的**调了 Run（否则“只调 Run”可能只是因为它们什么都不做）。
	if runCalls == 0 {
		t.Fatal("ops 生产代码里没有任何 .Run( 调用（触发器入口已失效）")
	}
}
