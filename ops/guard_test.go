package ops_test

import (
	"context"
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
func TestOpsNeverWritesMemoriesDirectly(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	// 这些是"写 memories **内容**"的 API。任何一处出现在 ops 生产代码里都是缺陷。
	//
	// 为什么 InvalidateMemory / TouchRecall 不在此列（这是刻意的，不是漏掉）：
	//   - InvalidateMemory 只置 t_invalid（**世界时间**），不改 content/kind/concept_id，
	//     因此按 D13 不产生新版本、FTS 也无需重同步——Port 契约就是这么定义的；
	//   - TouchRecall 只改运营计数 recall_count，D13 明确它不得催生版本。
	// 两者都不经过 FTS，因此不构成"绕过写入编排"。真正危险的只有内容写入 PutMemory：
	// 它写主表却不会顺手写 FTS，漏了也不报错。
	forbidden := []string{
		"PutMemory(",
	}
	var scanned int
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		scanned++
		body := string(src)
		for _, bad := range forbidden {
			if strings.Contains(body, bad) {
				t.Errorf("%s 里出现了 %q：ops 不得自行写 memories（必须经 recall.Writer，"+
					"否则 FTS 会静默漏索引）", f, bad)
			}
		}
		// 唯一允许的写入者：P2 的 Extractor（内部必经 recall.Writer）。
		if strings.Contains(body, "recall.NewWriter") {
			t.Errorf("%s 里出现了 recall.NewWriter：写入编排应由 Extractor/调用方注入，"+
				"ops 不应自己造一个写入入口", f)
		}
	}
	if scanned == 0 {
		t.Fatal("没有扫描到任何 ops 生产源码（守卫失效）")
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
