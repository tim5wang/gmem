package p1verify

import (
	"context"
	"fmt"
	"testing"

	"github.com/tim5wang/gmem/recall"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// 攻击面 5：写入编排是否**真的**原子 + 是否传真实版本号
//
// 作者的测试只验证"能写入"。这里验的是反面：**失败时两边都不写**。
// 三档失败点，覆盖不同的"写了一半"形态：
//   - Lexical.Index 直接失败（索引侧根本没写）；
//   - Lexical.Index 先写成功再报错（索引侧写了，但整体必须回滚）；
//   - Graph.PutNode 第 N 个节点失败（主表+索引已写、图写了一半）。
// 每档都必须：主表不可见、FTS 不可见、图不可见 —— 且用**独立连接**核对行级事实。
// ===========================================================================

// TestWriterAtomicWhenLexicalFails 让 Lexical.Index 直接失败：
// 主表也不得落库（双向回滚，不是"只写主表不写索引"）。
func TestWriterAtomicWhenLexicalFails(t *testing.T) {
	d, raw := openFile(t)
	ctx := context.Background()
	ports := d.Ports()

	w := recall.NewWriter(store.Ports{
		Store:   ports.Store,
		Graph:   ports.Graph,
		Lexical: &failLexical{inner: ports.Lexical}, // 不委托：根本没写
	})
	_, err := w.PutMemory(ctx, store.Memory{
		ID: "m-atomic-1", Scope: store.ScopeUser, Kind: store.KindSemantic,
		Content: "这条不该活下来", TValid: 1000, TCreated: 1000,
	})
	if err == nil {
		t.Fatalf("Lexical.Index 失败时 Writer 必须返回错误")
	}

	// 主表：不可见。
	if _, gerr := d.Store().GetMemory(ctx, "m-atomic-1"); gerr == nil {
		t.Fatalf("索引失败但主表落了库（单向写入）：GetMemory 竟然成功")
	}
	// 独立连接核行级事实：memories 里不能有该行。
	var n int
	if err := raw.QueryRow(`SELECT COUNT(1) FROM memories WHERE id = ?`, "m-atomic-1").Scan(&n); err != nil {
		t.Fatalf("count memories: %v", err)
	}
	if n != 0 {
		t.Fatalf("memories 表里有 %d 行 m-atomic-1（事务未回滚）", n)
	}
	// FTS：不可见。
	if rows := ftsRows(t, raw); len(rows) != 0 {
		t.Fatalf("memories_fts 留下了行：%+v", rows)
	}
	// 检索侧：查不到。
	hits, err := d.Lexical().Search(ctx, store.LexicalQuery{Query: "不该活下来", Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("索引失败后仍能检索到：%+v", hits)
	}
}

// TestWriterAtomicWhenLexicalFailsAfterWriting 是最严格的一档：
// Lexical.Index **先真实写入再报错**，因此"回滚"必须把已写的 FTS 行也撤掉。
//
// 这一档能区分两种实现：
//   - 真事务（正确）：FTS 行随事务回滚；
//   - 伪事务（缺陷）：FTS 行留下，主表也留下或只留下一边。
func TestWriterAtomicWhenLexicalFailsAfterWriting(t *testing.T) {
	d, raw := openFile(t)
	ctx := context.Background()
	ports := d.Ports()

	w := recall.NewWriter(store.Ports{
		Store:   ports.Store,
		Graph:   ports.Graph,
		Lexical: &failLexical{inner: ports.Lexical, afterDelegate: true},
	})
	_, err := w.PutMemory(ctx, store.Memory{
		ID: "m-atomic-2", Scope: store.ScopeUser, Kind: store.KindSemantic,
		Content: "先写后报错", TValid: 1000, TCreated: 1000,
	})
	if err == nil {
		t.Fatalf("Lexical.Index 报错时 Writer 必须返回错误")
	}

	if _, gerr := d.Store().GetMemory(ctx, "m-atomic-2"); gerr == nil {
		t.Fatalf("主表落了库（应随事务回滚）")
	}
	if rows := ftsRows(t, raw); len(rows) != 0 {
		t.Fatalf("已写入的 FTS 行未被回滚（伪事务）：%+v", rows)
	}
	var n int
	if err := raw.QueryRow(`SELECT COUNT(1) FROM memories WHERE id = ?`, "m-atomic-2").Scan(&n); err != nil {
		t.Fatalf("count memories: %v", err)
	}
	if n != 0 {
		t.Fatalf("memories 表里有 %d 行（事务未回滚）", n)
	}
}

// TestWriterAtomicWhenGraphNodeFails 让第 2 个节点写入失败：
// 第 1 个节点、主表、FTS 都必须一起回滚（不得部分提交）。
func TestWriterAtomicWhenGraphNodeFails(t *testing.T) {
	d, raw := openFile(t)
	ctx := context.Background()
	ports := d.Ports()

	w := recall.NewWriter(store.Ports{
		Store:   ports.Store,
		Lexical: ports.Lexical,
		Graph:   &failGraphNode{inner: ports.Graph, failAt: 2},
	})
	_, err := w.Put(ctx, recall.WriteBundle{
		Memory: store.Memory{
			ID: "m-atomic-3", Scope: store.ScopeUser, Kind: store.KindSemantic,
			Content: "图写到一半", TValid: 1000, TCreated: 1000,
		},
		Nodes: []store.Node{
			{ID: "n-first", Scope: store.ScopeUser, CanonicalName: "first"},
			{ID: "n-second", Scope: store.ScopeUser, CanonicalName: "second"},
		},
	})
	if err == nil {
		t.Fatalf("第 2 个 PutNode 失败时 Writer 必须返回错误")
	}

	// 第 1 个节点写成功了，但必须随事务回滚。
	if _, gerr := d.Graph().GetNode(ctx, "n-first"); gerr == nil {
		t.Fatalf("第一个节点留下了（部分提交）")
	}
	if _, gerr := d.Graph().GetNode(ctx, "n-second"); gerr == nil {
		t.Fatalf("第二个节点留下了（虽然它是失败的那个）")
	}
	var nodes int
	if err := raw.QueryRow(`SELECT COUNT(1) FROM nodes`).Scan(&nodes); err != nil {
		t.Fatalf("count nodes: %v", err)
	}
	if nodes != 0 {
		t.Fatalf("nodes 表里有 %d 行（图部分提交）", nodes)
	}
	// 主表与索引同样不得留下。
	if _, gerr := d.Store().GetMemory(ctx, "m-atomic-3"); gerr == nil {
		t.Fatalf("图失败但主表落库")
	}
	if rows := ftsRows(t, raw); len(rows) != 0 {
		t.Fatalf("图失败但 FTS 留下行：%+v", rows)
	}
}

// TestWriterRejectsUnknownGraphPort 覆盖"静默丢图"的一条边界：带节点但 Graph 为 nil 必须报错。
// （作者测过带 Nodes 的情形；这里补带 **Edges** 的情形，见 BLOCKER B3。）
func TestWriterRejectsUnknownGraphPort(t *testing.T) {
	d := openMemory(t)
	ctx := context.Background()
	w := recall.NewWriter(store.Ports{Store: d.Store(), Lexical: d.Lexical()})

	_, err := w.Put(ctx, recall.WriteBundle{
		Memory: store.Memory{
			ID: "m-nograph", Scope: store.ScopeUser, Kind: store.KindSemantic,
			Content: "带节点", TValid: 1000, TCreated: 1000,
		},
		Nodes: []store.Node{{ID: "n-x", Scope: store.ScopeUser, CanonicalName: "x"}},
	})
	if err == nil {
		t.Fatalf("带节点但 graph 为 nil，必须报错")
	}
	if _, gerr := d.Store().GetMemory(ctx, "m-nograph"); gerr == nil {
		t.Fatalf("失败写入留下了记忆")
	}
}

// TestBlockerWriterNilGraphPanicsOnEdges 是 BLOCKER 3：只带 Edges（不带 Nodes）且 Graph 为 nil 时** panic**。
//
// 缺陷：writer.go 的前置检查只有 `len(b.Nodes) > 0 && w.ports.Graph == nil`，
// 漏了 Edges。于是纯边写入（调用方只想写关系、节点早已存在——这是最常见的增量写法）
// 会在 `w.ports.Graph.PutEdge` 处 nil 解引用 panic，而不是返回可读错误。
//
// 为什么是阻塞项：库代码 panic 会穿过宿主进程（CLI/MCP server），
// 而"只想补一条边"是完全合法且文档鼓励的用法（P1-NOTES §2.2 的 InlineGraph）。
func TestBlockerWriterNilGraphPanicsOnEdges(t *testing.T) {
	d := openMemory(t)
	ctx := context.Background()
	ports := d.Ports()
	w := recall.NewWriter(store.Ports{Store: ports.Store, Lexical: ports.Lexical})

	var putErr error
	panicMsg := mustPanic(func() {
		_, putErr = w.Put(ctx, recall.WriteBundle{
			Memory: store.Memory{
				ID: "m-edges", Scope: store.ScopeUser, Kind: store.KindSemantic,
				Content: "只有边没有节点", TValid: 1000, TCreated: 1000,
			},
			Edges: []store.Edge{{
				ID: "e-1", FromID: "n-a", ToID: "n-b", Relation: store.RelationUses,
			}},
		})
	})
	// 无论哪条路径，主表都不得留下（事务/lint 任一环节）。
	_, getErr := d.Store().GetMemory(ctx, "m-edges")

	pin(t, pinBlocker, "B3", "Writer 只带 Edges 且 Graph=nil 时 nil 解引用 panic",
		func() string {
			if panicMsg == "" {
				return fmt.Sprintf("预期 panic，实际未 panic（err=%v）——缺陷形态已变", putErr)
			}
			return ""
		},
		func() string {
			if panicMsg != "" {
				return fmt.Sprintf("仍然 panic（panic=%q）；应返回可读错误", panicMsg)
			}
			if putErr == nil {
				return "带 Edges 但 Graph=nil 时既没 panic 也没报错（静默丢边）"
			}
			return ""
		})

	// 附加断言（两种口径都必须成立）：库代码 panic 后必须留下一致状态，不能留下半截记忆。
	if getErr == nil {
		t.Fatalf("panic 后主表留下了记录（状态不一致）")
	}
}

// ---------------------------------------------------------------------------
// 真实版本号（D13 核心）
// ---------------------------------------------------------------------------

// TestWriterPassesRealVersionToFTS 是本卡点名的 D13 核心断言：
// 同一 id 重写产生 v2 后，FTS 里 **v1 与 v2 都必须存在**，且 as_of 各能命中各自版本。
//
// 除了作者测过的"搜索命中"，这里额外用**独立连接**核对行级事实：
// memories_fts 的 (id, version) 必须是 {1,2}，且 body 与各版本内容一一对应。
// 这样"两个版本都还能查"就不依赖 Search 的实现口径。
func TestWriterPassesRealVersionToFTS(t *testing.T) {
	d, raw := openFile(t)
	ctx := context.Background()
	w := recall.NewWriter(d.Ports())

	put := func(content string, tValid int64) recall.PutResult {
		t.Helper()
		res, err := w.PutMemory(ctx, store.Memory{
			ID: "m-scheme", Scope: store.ScopeUser, Kind: store.KindDecision,
			Content: content, TValid: tValid, TCreated: tValid,
		})
		if err != nil {
			t.Fatalf("PutMemory(%q): %v", content, err)
		}
		return res
	}
	r1 := put("认证用 session", 1000)
	r2 := put("认证用 JWT", 2000)
	// 等价重写：不得产生新版本（D13：RecallCount 等运营计数不参与比较）。
	r3 := put("认证用 JWT", 2000)

	if r1.Version != 1 {
		t.Fatalf("首次写入 Version=%d, want 1", r1.Version)
	}
	if r2.Version != 2 {
		t.Fatalf("内容变化后 Version=%d, want 2", r2.Version)
	}
	if r3.Version != 2 {
		t.Fatalf("等价重写 Version=%d, want 2（幂等，不得产生新版本）", r3.Version)
	}
	if !r2.LexicalIndexed || !r3.LexicalIndexed {
		t.Fatalf("LexicalIndexed 必须恒为 true：%+v %+v", r2, r3)
	}
	if r2.ID != "m-scheme" || r2.Version != 2 {
		t.Fatalf("PutResult=%+v, want m-scheme/v2", r2)
	}

	// 行级事实：FTS 必须同时有 v1 与 v2，且 body 与各版本内容对应。
	rows := ftsRows(t, raw)
	versions, ok := rows["m-scheme"]
	if !ok {
		t.Fatalf("memories_fts 里没有 m-scheme：%+v", rows)
	}
	if len(versions) != 2 {
		t.Fatalf("FTS 版本数=%d, want 2（v1 必须保留，否则 as_of 回溯在词法通道静默丢数据）：%+v",
			len(versions), versions)
	}
	if got := versions[1]; got != "认证用 session" {
		t.Fatalf("FTS v1 body=%q, want 认证用 session（版本与内容错位）", got)
	}
	if got := versions[2]; got != "认证用 JWT" {
		t.Fatalf("FTS v2 body=%q, want 认证用 JWT", got)
	}

	// as_of 双向：1500 命中 v1，2500 命中 v2。
	for _, tc := range []struct {
		asOf        int64
		wantVersion int
		wantQuery   string
		badQuery    string
	}{
		{1500, 1, "session", "JWT"},
		{2500, 2, "JWT", ""},
	} {
		asOf := tc.asOf
		hits, err := d.Lexical().Search(ctx, store.LexicalQuery{
			Query: tc.wantQuery, Scope: store.ScopeUser, AsOf: &asOf,
		})
		if err != nil {
			t.Fatalf("Search(as_of=%d): %v", tc.asOf, err)
		}
		if len(hits) != 1 || hits[0].Version != tc.wantVersion {
			t.Fatalf("as_of=%d 查 %q：hits=%+v, want 单条 v%d",
				tc.asOf, tc.wantQuery, hits, tc.wantVersion)
		}
		if tc.badQuery != "" {
			// 未来版本的内容不得泄漏到过去。
			bad, err := d.Lexical().Search(ctx, store.LexicalQuery{
				Query: tc.badQuery, Scope: store.ScopeUser, AsOf: &asOf,
			})
			if err != nil {
				t.Fatalf("Search(bad, as_of=%d): %v", tc.asOf, err)
			}
			if len(bad) != 0 {
				t.Fatalf("as_of=%d 查 %q 命中 %+v：v2 内容泄漏到过去", tc.asOf, tc.badQuery, bad)
			}
		}
	}

	// Recall 侧同样必须取当时有效的版本，而不是最新版。
	r := recall.New(d.Ports(), recall.Config{}, nil)
	asOf1500 := int64(1500)
	res, err := r.Recall(ctx, recall.RecallRequest{
		Query: "认证", Scope: store.ScopeUser, AsOf: &asOf1500,
	})
	if err != nil {
		t.Fatalf("Recall(as_of=1500): %v", err)
	}
	if len(res.Items) == 0 {
		t.Fatalf("as_of=1500 无结果：plan=%+v", res.Plan)
	}
	if res.Items[0].Memory.Version != 1 || res.Items[0].Memory.Content != "认证用 session" {
		t.Fatalf("as_of=1500 返回了 v%d %q，want v1/Session（用最新版冒充历史版）",
			res.Items[0].Memory.Version, res.Items[0].Memory.Content)
	}

	// 当前（as_of=nil）必须是 v2。
	resCur, err := r.Recall(ctx, recall.RecallRequest{Query: "认证", Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("Recall(current): %v", err)
	}
	if len(resCur.Items) == 0 {
		t.Fatalf("当前无结果")
	}
	if resCur.Items[0].Memory.Version != 2 {
		t.Fatalf("当前返回 v%d, want v2", resCur.Items[0].Memory.Version)
	}
}

// TestWriterNeverIndexesVersionZero 是 D13 的守门断言：
// 任何走 Writer 的写入都不得在 FTS 里留下 version=0 的行
// （0 在 DocumentKey 里被解释为 1，但在 FTS 行里是"版本未知"，会让 as_of 对不上）。
func TestWriterNeverIndexesVersionZero(t *testing.T) {
	d, raw := openFile(t)
	ctx := context.Background()
	w := recall.NewWriter(d.Ports())

	for i, content := range []string{"第一版内容", "第二版内容", "第三版内容"} {
		if _, err := w.PutMemory(ctx, store.Memory{
			ID: "m-multi", Scope: store.ScopeUser, Kind: store.KindSemantic,
			Content: content, TValid: int64(1000 * (i + 1)), TCreated: int64(1000 * (i + 1)),
		}); err != nil {
			t.Fatalf("PutMemory(%d): %v", i, err)
		}
	}
	rows := ftsRows(t, raw)
	versions := rows["m-multi"]
	if len(versions) != 3 {
		t.Fatalf("FTS 版本数=%d, want 3：%+v", len(versions), versions)
	}
	if _, bad := versions[0]; bad {
		t.Fatalf("FTS 里出现了 version=0 的行：%+v", versions)
	}
	for v := 1; v <= 3; v++ {
		if _, ok := versions[v]; !ok {
			t.Fatalf("FTS 缺 version=%d：%+v", v, versions)
		}
	}
}
