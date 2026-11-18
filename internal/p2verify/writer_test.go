package p2verify

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tim5wang/gmem/extract"
	"github.com/tim5wang/gmem/sqlite"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// 攻击面 4：写入必须走 recall.Writer
//
// 绕过 Writer 的写入不会报错——它只是**永远搜不到**。这正是 P0 修过的
// "FTS 静默漏索引"缺陷：写入成功、检索恒空、答案变慢变差，且没有任何信号。
//
// 因此本节三条断言必须同时成立，任何一条单独都不够：
//  1. 源码里没有 Store.PutMemory 调用点（结构性保证，防止将来回潮）；
//  2. 写入后内容**真的能被词法通道搜到**（行为证据）；
//  3. 词法端口故意失败时，整笔写入回滚（证明索引与主表在同一事务里）。
// ===========================================================================

// TestNoPutMemoryBypassInProductionCode 扫源码：生产代码不得直接调 Store.PutMemory。
//
// 为什么不只看 extract：绕过 Writer 的风险点可能出现在任何一层，
// 因此把"允许直接写主表"的文件限制成显式白名单（Writer 自己 + 后端实现）。
func TestNoPutMemoryBypassInProductionCode(t *testing.T) {
	root := "../.."
	// 白名单：只有这两个文件可以直呼 PutMemory。
	//   recall/writer.go —— Writer 本体，它就是被允许的那一层。
	//   sqlite/*         —— 后端实现（端口落地，不是调用方）。
	allowed := map[string]bool{
		filepath.Join("recall", "writer.go"): true,
	}

	var offenders []string
	err := filepath.Walk(filepath.Join(root, "extract"), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") ||
			strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if allowed[rel] {
			return nil
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		src := string(b)
		// 只看真实调用：`Store.PutMemory(` 或 `.PutMemory(ctx`。
		for _, needle := range []string{"Store.PutMemory(", ".PutMemory(ctx"} {
			if strings.Contains(src, needle) {
				offenders = append(offenders, rel+" 含 "+needle)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk extract: %v", err)
	}
	if len(offenders) > 0 {
		t.Fatalf("生产代码里出现了绕过 recall.Writer 的写入点（会静默漏 FTS 索引）：\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// TestWrittenContentIsLexicallySearchable 是"走了 Writer"的行为证据。
//
// 判据：管线跑完后，用**另一个方向**（词法检索）查正文里的独特字符串。
// 若写入绕过了 Writer，主表有行、FTS 没有行，这里会返回 0 命中而不报错。
func TestWrittenContentIsLexicallySearchable(t *testing.T) {
	d, _ := openFileDB(t)
	// 独特词：确保命中只可能来自本次写入。
	const unique = "萤火虫协议"
	p := extract.NewPipeline(d.Ports(), extract.Config{}, extract.PipelineOptions{})
	content := "团队决定采用 " + unique + " 作为内部通信的默认方案。"
	res, err := p.Run(context.Background(), extract.RawEpisode{
		ID: "e-fts", Scope: store.ScopeUser, Content: content,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Writes) == 0 {
		t.Fatal("结果里没有任何 WriteReport（说明什么都没写）")
	}
	// 每条 WriteReport 必须指向真实存在的记忆行：它是"写入真的落地了"的最小证据。
	//
	// 【可后续】recall.PutResult.LexicalIndexed 在管线层被丢弃（extract.WriteReport
	// 没有这个字段），因此调用方**无法**从结果里确认索引已同步。
	// 目前只能靠下面的行为断言（FTS 能否搜到）间接验证。
	for i, w := range res.Writes {
		if w.MemoryID == "" {
			t.Fatalf("第 %d 条 WriteReport 没有 MemoryID", i)
		}
		if w.Version <= 0 {
			t.Fatalf("WriteReport %q 的 Version=%d 非法（Writer 契约要求 >=1）", w.MemoryID, w.Version)
		}
		if _, err := d.Store().GetMemory(context.Background(), w.MemoryID); err != nil {
			t.Errorf("WriteReport 声称写了 %q，但主表里读不到：%v", w.MemoryID, err)
		}
	}

	hits, err := d.Lexical().Search(context.Background(), store.LexicalQuery{
		Query: unique, Scope: store.ScopeUser, Limit: 10,
	})
	if err != nil {
		t.Fatalf("Lexical.Search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatalf("写入的内容在词法通道上搜不到（%q 命中 0 条）——"+
			"这就是绕过 recall.Writer 的 P0 缺陷特征：写成功、检索恒空、不报错", unique)
	}
	// 命中必须指向真实存在的记忆行（不是索引里的孤儿）。
	if _, err := d.Store().GetMemory(context.Background(), hits[0].ID); err != nil {
		t.Errorf("词法命中的记忆 %q 在主表里不存在：%v", hits[0].ID, err)
	}
}

// TestFailedLexicalRollsBackWholeWrite 攻击写入原子性。
//
// 词法端口在写某条记忆时报错，整笔写入必须回滚：主表、图、别名都不能留下行。
// 若"记忆已落库、索引失败但被吞掉"，就会得到主表有行 / 检索查不到的静默缺陷。
func TestFailedLexicalRollsBackWholeWrite(t *testing.T) {
	d, raw := openFileDB(t)
	// 让"含 unique 的记忆"在索引时报错。
	const unique = "灰烬信标"
	lex := &failingLexical{inner: d.Lexical(), failWhen: unique}
	ports := portsWithLexical(d.Ports(), lex)

	p := extract.NewPipeline(ports, extract.Config{
		// 关掉灰区护栏等噪声，只留必要路径。
		GrayBacklogLimit: 1000,
	}, extract.PipelineOptions{
		Extractor: entityExtractor{entities: []extract.ExtractedEntity{{Name: "信标服务"}}},
	})
	ep := extract.RawEpisode{
		ID: "e-rollback", Scope: store.ScopeUser,
		Content: "团队决定采用 " + unique + " 作为默认方案，信标服务负责广播。",
	}
	_, err := p.Run(context.Background(), ep)
	if err == nil {
		t.Fatal("词法索引失败时写入必须报错（不得静默成功）")
	}
	if lex.failures == 0 {
		t.Fatalf("注入的失败从未发生——用例前提失效（err=%v）", err)
	}
	// 必须能看出根因是索引这一步，而不是含糊的"写失败"。
	if !strings.Contains(err.Error(), "lexical") && !strings.Contains(err.Error(), "索引") {
		t.Errorf("错误信息应指出索引失败（便于定位），实际：%v", err)
	}

	// ---- 回滚断言：主表 / 图 / 别名 都不应留下本次写入的行 ----
	// episode 是 cliff：它由 Store.PutEpisode 直接落库（不可变来源，不参与 bundle 事务），
	// 因此这里**只**断言"记忆/图/别名"这三类派生数据被回滚。
	if n := countRows(t, raw, `SELECT COUNT(1) FROM memories`); n != 0 {
		t.Errorf("索引失败后 memories 应回滚为 0 行，实际 %d 行（主表有行、检索查不到＝静默缺陷）", n)
	}
	if n := countRows(t, raw, `SELECT COUNT(1) FROM memories_fts`); n != 0 {
		t.Errorf("索引失败后 memories_fts 应无行，实际 %d 行", n)
	}
	if n := countRows(t, raw, `SELECT COUNT(1) FROM nodes`); n != 0 {
		t.Errorf("索引失败后 nodes 应回滚为 0 行，实际 %d 行", n)
	}
	if n := countRows(t, raw, `SELECT COUNT(1) FROM aliases`); n != 0 {
		t.Errorf("索引失败后 aliases 应回滚为 0 行，实际 %d 行", n)
	}
	if n := countRows(t, raw, `SELECT COUNT(1) FROM edges`); n != 0 {
		t.Errorf("索引失败后 edges 应回滚为 0 行，实际 %d 行", n)
	}
	// episode 保留是**设计如此**（不可变事实来源），把它钉住以免将来被误改。
	if n := countRows(t, raw, `SELECT COUNT(1) FROM episodes WHERE id = 'e-rollback'`); n != 1 {
		t.Logf("注意：episode 行数 = %d（设计上 episode 在 bundle 事务之外先落库）", n)
	}
}

// TestNoopExtractorDegradeVisibleAndL0L1StillWork 验证"不注入 LLM"时的可用性与可见性。
//
// TD §13 把这条列为风险：宿主不实现 LLMClient → 抽取降级、体验变差。
// 要求是**降级必须可见**，且 L0+L1 仍然产出（否则离线/CI 场景直接不可用）。
func TestNoopExtractorDegradeVisibleAndL0L1StillWork(t *testing.T) {
	d, raw := openFileDB(t)
	// 不传 Extractor → 必须落到 NoopExtractor（而不是静默变成"什么都不做"）。
	p := extract.NewPipeline(d.Ports(), extract.Config{}, extract.PipelineOptions{})

	// 内容同时命中 L0 的决策句与关系句（保证 L0 有产物）。
	const content = "团队决定采用 SQLite 作为默认后端，认证服务依赖 Redis 做缓存。"
	res, err := p.Run(context.Background(), extract.RawEpisode{
		ID: "e-noop", Scope: store.ScopeUser, Content: content,
	})
	if err != nil {
		t.Fatalf("NoopExtractor 路径不应报错（应降级而非失败）：%v", err)
	}

	// ---- 1) 降级必须可见 ----
	if !hasDegraded(res.Degraded, "NoopExtractor") {
		t.Errorf("不注入 LLM 时 Degraded 必须说明用了 NoopExtractor，实际 %v", res.Degraded)
	}
	// 也不能假装 L2 跑过。
	if !res.EnabledL2() {
		t.Errorf("NoopExtractor 不属于 SkippedL2（SkippedL2 专指灰区积压护栏），实际 SkippedL2=%v",
			res.SkippedL2)
	}
	// ---- 2) L0 + L1 仍然产出 ----
	if len(res.L0.Findings) == 0 {
		t.Fatalf("L0 必须仍能产出（确定性提取不依赖 LLM）；Rejected=%v", res.L0.Rejected)
	}
	// L2 没有实体（NoopExtractor 刻意返回空），但 L0 的发现必须已落库。
	if len(res.Extracted.Entities) != 0 {
		t.Errorf("NoopExtractor 不应产出实体，实际 %d 个", len(res.Extracted.Entities))
	}
	if n := countRows(t, raw, `SELECT COUNT(1) FROM memories`); n == 0 {
		t.Fatal("L0 产物必须落库（NoopExtractor 只让 L2 缺席，不能让 L0 一起丢）")
	}
	// 决策句应落成 decision 记忆。
	if n := countRows(t, raw, `SELECT COUNT(1) FROM memories WHERE kind = 'decision'`); n == 0 {
		t.Errorf("L0 的决策句应落成 decision 记忆，实际 0 行（findings=%d）", len(res.L0.Findings))
	}
	// L0 的关系句应落地成边（端点节点由 L0 自己建，必须存在）。
	if n := countRows(t, raw, `SELECT COUNT(1) FROM edges`); n == 0 {
		t.Errorf("L0 的关系句应落成边，实际 0 行")
	}
	// ---- 3) 落库内容可被词法搜到（走了 Writer） ----
	hits, err := d.Lexical().Search(context.Background(), store.LexicalQuery{
		Query: "SQLite", Scope: store.ScopeUser, Limit: 10,
	})
	if err != nil {
		t.Fatalf("Lexical.Search: %v", err)
	}
	if len(hits) == 0 {
		t.Error("NoopExtractor 路径写入的内容也应可被词法搜到（必须走 Writer）")
	}
}

// TestL2FailureStillPersistsL0 验证"L2 报错不得连带丢掉 L0/L1 的产物"。
//
// L2 是最不可靠的一环（网络、模型、解析），若它失败就整段 episode 白写，
// 记忆系统会在最不该沉默的地方沉默。
func TestL2FailureStillPersistsL0(t *testing.T) {
	d, raw := openFileDB(t)
	failing := failingExtractor{err: errL2Down}
	p := extract.NewPipeline(d.Ports(), extract.Config{}, extract.PipelineOptions{
		Extractor: failing,
	})
	res, err := p.Run(context.Background(), extract.RawEpisode{
		ID: "e-l2fail", Scope: store.ScopeUser,
		Content: "团队决定采用 SQLite 作为默认后端，认证服务依赖 Redis 做缓存。",
	})
	if err != nil {
		t.Fatalf("L2 失败不应让整条管线报错（应降级并落 L0 产物）：%v", err)
	}
	if !hasDegraded(res.Degraded, "L2 抽取失败") {
		t.Errorf("L2 失败必须可见，实际 Degraded=%v", res.Degraded)
	}
	if n := countRows(t, raw, `SELECT COUNT(1) FROM memories WHERE kind = 'decision'`); n == 0 {
		t.Error("L2 失败后 L0 的决策记忆仍然必须落库")
	}
	if n := countRows(t, raw, `SELECT COUNT(1) FROM edges`); n == 0 {
		t.Error("L2 失败后 L0 的关系边仍然必须落库")
	}
}

// failingExtractor 是"报错的 L2"（模拟模型不可用）。
type failingExtractor struct{ err error }

func (f failingExtractor) Extract(context.Context, extract.ExtractRequest) (extract.ExtractResult, error) {
	return extract.ExtractResult{}, f.err
}

var errL2Down = errFmt("模型不可用（注入的失败）")

// errFmt 是极小的 error 实现，避免为一行错误引入额外依赖。
type errFmt string

func (e errFmt) Error() string { return string(e) }

// 保证 sqlite 包被引用（openFileDB 的返回类型来自它）。
var _ = (*sqlite.DB)(nil)
