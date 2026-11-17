package vtest

import (
	"context"
	"errors"
	"testing"

	"github.com/tim5wang/gmem/fusion"
	"github.com/tim5wang/gmem/sqlite"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// Port 抽象一致性：**一个测试体，两个后端，同一答案**
//
// 这是 docs/TD.md §11 第 2 条的原话：
//   "104 opt-in PostgreSQL tests, mostly parity: one test body, both databases,
//    same answer required. 这是保证两个后端不偏移的唯一有效手段。"
//
// 这里用内存后端代替 PostgreSQL 担任第二个后端。它的价值有两层：
//  1. 能写出内存后端 ⇒ Port 接口没有把 SQL 泄漏到上层（否则必然写不出来）；
//  2. 同一组断言在两个后端上都过 ⇒ 契约是后端无关的，不是"SQLite 行为说明书"。
//
// 断言口径（刻意保守）：
//   - 只断言**契约**（集合、顺序、错误值），不断言通道内分值（BM25 vs 命中计数本就不同）；
//   - 涉及排序的用例让"正确顺序"由契约唯一决定（token 命中数差异明显 / 向量明显分离）。
// ===========================================================================

// backendFactory 为每个子测试产出**全新**的后端。
//
// 必须每次新建而不是共享：这些用例都在往库里写数据，共享实例会让
// "前一个用例的数据"污染后一个用例的断言（首轮跑就踩到了这个坑）。
type backendFactory struct {
	name string
	make func(t *testing.T) store.Ports
}

// TestPortsConformance 用同一组断言分别跑 SQLite 与内存后端。
func TestPortsConformance(t *testing.T) {
	backends := []backendFactory{
		{name: "sqlite", make: func(t *testing.T) store.Ports {
			return openMemory(t).Ports()
		}},
		{name: "memory", make: func(t *testing.T) store.Ports {
			return newMemoryPorts()
		}},
	}
	cases := []struct {
		name string
		run  func(t *testing.T, p store.Ports)
	}{
		{"StoreMemoryCRUD", runStoreMemoryCRUD},
		{"StoreBitemporal", runStoreBitemporal},
		{"StoreEpisodeConceptAlias", runStoreEpisodeConceptAlias},
		{"StoreTxRollback", runStoreTxRollback},
		{"LexicalChineseTwoChar", runLexicalChineseTwoChar},
		{"LexicalScopeAndOverwrite", runLexicalScopeAndOverwrite},
		{"LexicalBigramSymmetry", runLexicalBigramSymmetry},
		{"VectorKNN", runVectorKNN},
		{"GraphNeighbors", runGraphNeighbors},
		{"RRFFusionOrder", runRRFFusionOrder},
	}
	for _, b := range backends {
		b := b
		t.Run(b.name, func(t *testing.T) {
			for _, c := range cases {
				c := c
				t.Run(c.name, func(t *testing.T) {
					c.run(t, b.make(t))
				})
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Store
// ---------------------------------------------------------------------------

func runStoreMemoryCRUD(t *testing.T, p store.Ports) {
	ctx := context.Background()

	if _, err := p.Store.GetMemory(ctx, "absent"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetMemory(absent) err = %v, want ErrNotFound", err)
	}

	m := store.Memory{
		ID: "m1", Scope: store.ScopeProject, Kind: store.KindDecision,
		Content: "选择 SQLite 单文件作为 P0 存储", TValid: 10, TCreated: 10, Importance: 0.9,
	}
	if err := p.Store.PutMemory(ctx, m); err != nil {
		t.Fatalf("PutMemory: %v", err)
	}
	got, err := p.Store.GetMemory(ctx, "m1")
	if err != nil {
		t.Fatalf("GetMemory: %v", err)
	}
	if got.ID != m.ID || got.Scope != m.Scope || got.Kind != m.Kind || got.Content != m.Content {
		t.Errorf("字段往返不一致: got %+v want %+v", got, m)
	}
	if got.TValid != m.TValid || got.TCreated != m.TCreated {
		t.Errorf("时间戳往返不一致: got t_valid=%d t_created=%d want %d/%d",
			got.TValid, got.TCreated, m.TValid, m.TCreated)
	}

	// 无效输入必须被 Validate 挡住（两个后端一致）。
	bad := []store.Memory{
		{ID: "", Scope: store.ScopeUser, Kind: store.KindSemantic, Content: "x"},
		{ID: "b1", Scope: store.Scope("bogus"), Kind: store.KindSemantic, Content: "x"},
		{ID: "b2", Scope: store.ScopeUser, Kind: store.MemoryKind("bogus"), Content: "x"},
		{ID: "b3", Scope: store.ScopeUser, Kind: store.KindSemantic, Content: ""},
	}
	for i, b := range bad {
		if err := p.Store.PutMemory(ctx, b); err == nil {
			t.Errorf("第 %d 条非法 Memory 未被拒绝: %+v", i, b)
		}
	}
}

func runStoreBitemporal(t *testing.T, p store.Ports) {
	ctx := context.Background()

	// 冲突事实：旧的在世界时间 100..500 成立，新的从 500 起成立，系统时间 2000 写入。
	if err := p.Store.PutMemory(ctx, store.Memory{
		ID: "old", Scope: store.ScopeUser, Kind: store.KindSemantic,
		Content: "缩进用 tabs", TValid: 100, TCreated: 100,
	}); err != nil {
		t.Fatalf("PutMemory(old): %v", err)
	}
	if err := p.Store.PutMemory(ctx, store.Memory{
		ID: "new", Scope: store.ScopeUser, Kind: store.KindSemantic,
		Content: "缩进用 spaces", TValid: 500, TCreated: 2000,
	}); err != nil {
		t.Fatalf("PutMemory(new): %v", err)
	}
	if err := p.Store.InvalidateMemory(ctx, "old", 500); err != nil {
		t.Fatalf("InvalidateMemory: %v", err)
	}

	// 旧记录必须仍在（只置 t_invalid）。
	old, err := p.Store.GetMemory(ctx, "old")
	if err != nil {
		t.Fatalf("旧记录被物理删除: %v", err)
	}
	if old.TInvalid != 500 {
		t.Errorf("旧记录 TInvalid = %d, want 500", old.TInvalid)
	}
	if old.Content != "缩进用 tabs" {
		t.Errorf("旧记录内容被改写: %q", old.Content)
	}

	now, err := p.Store.ListMemories(ctx, store.MemoryQuery{})
	if err != nil {
		t.Fatalf("ListMemories: %v", err)
	}
	if ids := memIDs(now); !sameStrings(ids, []string{"new"}) {
		t.Errorf("当前有效事实 = %v, want [new]", ids)
	}

	for _, c := range []struct {
		asOf int64
		want []string
	}{
		{300, []string{"old"}},  // 旧事实区间内
		{500, nil},              // 旧事实失效瞬间；新事实尚未入库
		{2000, []string{"new"}}, // 新事实入库瞬间
		{9999, []string{"new"}}, // 现状
	} {
		asOf := c.asOf
		got, err := p.Store.ListMemories(ctx, store.MemoryQuery{AsOf: &asOf})
		if err != nil {
			t.Fatalf("as_of=%d: %v", c.asOf, err)
		}
		if ids := memIDs(got); !sameStrings(ids, c.want) {
			t.Errorf("as_of=%d: got %v, want %v", c.asOf, ids, c.want)
		}
	}
}

func runStoreEpisodeConceptAlias(t *testing.T, p store.Ports) {
	ctx := context.Background()

	if err := p.Store.PutEpisode(ctx, store.Episode{
		ID: "ep1", Scope: store.ScopeUser, Role: "user", Content: "我叫王五", At: 100,
	}); err != nil {
		t.Fatalf("PutEpisode: %v", err)
	}
	ep, err := p.Store.GetEpisode(ctx, "ep1")
	if err != nil {
		t.Fatalf("GetEpisode: %v", err)
	}
	if ep.Content != "我叫王五" || ep.Scope != store.ScopeUser {
		t.Errorf("Episode 往返不一致: %+v", ep)
	}

	if err := p.Store.PutConcept(ctx, store.Concept{
		ID: "c1", Name: "认证服务", Status: store.ConceptDraft, Definition: "身份校验",
	}); err != nil {
		t.Fatalf("PutConcept: %v", err)
	}
	c, err := p.Store.GetConcept(ctx, "c1")
	if err != nil {
		t.Fatalf("GetConcept: %v", err)
	}
	if c.Name != "认证服务" || c.Status != store.ConceptDraft {
		t.Errorf("Concept 往返不一致: %+v", c)
	}

	// alias 的 node_id 在 SQLite 侧有 FOREIGN KEY 约束，因此先建节点。
	// 这说明"引用完整性由后端 schema 承担"，Port 契约本身不表达它——
	// 已记入报告「可后续」项：跨后端一致性测试需知道这条隐含约束。
	for _, n := range []store.Node{
		{ID: "n-low", Scope: store.ScopeUser, CanonicalName: "认证（低置信）"},
		{ID: "n-high", Scope: store.ScopeUser, CanonicalName: "认证（词典）"},
		{ID: "n-other", Scope: store.ScopeUser, CanonicalName: "鉴权"},
	} {
		if err := p.Graph.PutNode(ctx, n); err != nil {
			t.Fatalf("PutNode(%s): %v", n.ID, err)
		}
	}

	// alias：同一 surface 多条映射时，GetAlias 必须给置信度最高的那条。
	for _, a := range []store.Alias{
		{Surface: "认证", NodeID: "n-low", Source: "llm", Confidence: 0.3},
		{Surface: "认证", NodeID: "n-high", Source: "dict", Confidence: 0.95},
		{Surface: "鉴权", NodeID: "n-other", Source: "dict", Confidence: 0.8},
	} {
		if err := p.Store.PutAlias(ctx, a); err != nil {
			t.Fatalf("PutAlias(%s): %v", a.Surface, err)
		}
	}
	best, err := p.Store.GetAlias(ctx, "认证")
	if err != nil {
		t.Fatalf("GetAlias: %v", err)
	}
	if best.NodeID != "n-high" {
		t.Errorf("GetAlias(认证).NodeID = %q, want n-high（应取最高置信度）", best.NodeID)
	}
	if _, err := p.Store.GetAlias(ctx, "不存在"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetAlias(不存在) err = %v, want ErrNotFound", err)
	}
}

func runStoreTxRollback(t *testing.T, p store.Ports) {
	ctx := context.Background()
	sentinel := errors.New("故意失败")

	err := p.Store.Tx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.PutMemory(ctx, store.Memory{
			ID: "tx-m1", Scope: store.ScopeUser, Kind: store.KindSemantic,
			Content: "应被回滚", TValid: 1, TCreated: 1,
		}); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Tx 应透传回调错误，得到 %v", err)
	}
	if _, err := p.Store.GetMemory(ctx, "tx-m1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Tx 回滚失败：tx-m1 仍存在 (err=%v)", err)
	}

	// 成功的 Tx 必须提交。
	if err := p.Store.Tx(ctx, func(ctx context.Context, tx store.Store) error {
		return tx.PutMemory(ctx, store.Memory{
			ID: "tx-m2", Scope: store.ScopeUser, Kind: store.KindSemantic,
			Content: "应提交", TValid: 1, TCreated: 1,
		})
	}); err != nil {
		t.Fatalf("Tx(成功) 返回错误: %v", err)
	}
	if _, err := p.Store.GetMemory(ctx, "tx-m2"); err != nil {
		t.Errorf("成功的 Tx 未提交: %v", err)
	}
}

// ---------------------------------------------------------------------------
// LexicalIndex
// ---------------------------------------------------------------------------

func runLexicalChineseTwoChar(t *testing.T, p store.Ports) {
	ctx := context.Background()

	if err := p.Lexical.Index(ctx, []store.Document{
		{ID: "d-auth", Scope: store.ScopeUser, Text: "用户身份认证失败，需要重新登录"},
		{ID: "d-migrate", Scope: store.ScopeUser, Text: "数据库迁移幂等性验证"},
	}); err != nil {
		t.Fatalf("Index: %v", err)
	}

	// 中文 2 字词必须命中（bigram 预分词的核心价值）。
	for _, c := range []struct {
		query string
		want  string
	}{
		{"认证", "d-auth"},
		{"身份", "d-auth"},
		{"登录", "d-auth"},
		{"数据", "d-migrate"},
		{"迁移", "d-migrate"},
		{"幂等", "d-migrate"},
	} {
		hits, err := p.Lexical.Search(ctx, c.query, store.ScopeUser, 10)
		if err != nil {
			t.Fatalf("Search(%q): %v", c.query, err)
		}
		if !contains(hitIDs(hits), c.want) {
			t.Errorf("中文 2 字查询 %q 未命中 %s，hits=%v", c.query, c.want, hitIDs(hits))
		}
	}

	// 3 字以上同样命中（查询侧必须整体切分，不能只处理 2 字）。
	hits, err := p.Lexical.Search(ctx, "身份认证", store.ScopeUser, 10)
	if err != nil {
		t.Fatalf("Search(身份认证): %v", err)
	}
	if !contains(hitIDs(hits), "d-auth") {
		t.Errorf("中文 4 字查询未命中 d-auth，hits=%v", hitIDs(hits))
	}

	// 无关查询不得误召回。
	hits, err = p.Lexical.Search(ctx, "量子纠缠", store.ScopeUser, 10)
	if err != nil {
		t.Fatalf("Search(量子纠缠): %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("无关查询出现误召回: %v", hitIDs(hits))
	}

	// 空查询/纯标点返回空且不报错。
	for _, q := range []string{"", "  ", "，。"} {
		hits, err := p.Lexical.Search(ctx, q, store.ScopeUser, 10)
		if err != nil {
			t.Errorf("Search(%q) 报错: %v", q, err)
		}
		if len(hits) != 0 {
			t.Errorf("Search(%q) = %v, want 空", q, hitIDs(hits))
		}
	}

	// Rank 必须严格从 1 递增且 ID 不重复（融合侧完全依赖这个契约）。
	for _, q := range []string{"认证", "数据库", "身份认证"} {
		hits, err := p.Lexical.Search(ctx, q, store.ScopeUser, 10)
		if err != nil {
			t.Fatalf("Search(%q): %v", q, err)
		}
		seen := map[string]bool{}
		for i, h := range hits {
			if h.Rank != i+1 {
				t.Errorf("query=%q 第 %d 项 Rank = %d, want %d", q, i, h.Rank, i+1)
			}
			if seen[h.ID] {
				t.Errorf("query=%q 出现重复 ID %q", q, h.ID)
			}
			seen[h.ID] = true
		}
	}
}

func runLexicalScopeAndOverwrite(t *testing.T, p store.Ports) {
	ctx := context.Background()

	if err := p.Lexical.Index(ctx, []store.Document{
		{ID: "u1", Scope: store.ScopeUser, Text: "用户身份认证失败"},
		{ID: "a1", Scope: store.ScopeAgent, Text: "Agent 身份认证流程"},
	}); err != nil {
		t.Fatalf("Index: %v", err)
	}

	// scope 隔离：查询带 scope 时不得跨 scope 返回。
	hits, err := p.Lexical.Search(ctx, "认证", store.ScopeUser, 10)
	if err != nil {
		t.Fatalf("Search(user): %v", err)
	}
	if ids := hitIDs(hits); !sameStrings(ids, []string{"u1"}) {
		t.Errorf("scope=user 查询 = %v, want [u1]", ids)
	}
	hits, err = p.Lexical.Search(ctx, "认证", store.ScopeAgent, 10)
	if err != nil {
		t.Fatalf("Search(agent): %v", err)
	}
	if ids := hitIDs(hits); !sameStrings(ids, []string{"a1"}) {
		t.Errorf("scope=agent 查询 = %v, want [a1]", ids)
	}
	// 空 scope = 不限，两 scope 都能出。
	hits, err = p.Lexical.Search(ctx, "认证", "", 10)
	if err != nil {
		t.Fatalf("Search(all): %v", err)
	}
	if len(hits) != 2 {
		t.Errorf("空 scope 查询应返回 2 条，得到 %v", hitIDs(hits))
	}

	// 覆盖写：同 ID 重写后旧文本不得残留（FTS5 无主键，实现必须先删后插）。
	if err := p.Lexical.Index(ctx, []store.Document{
		{ID: "u1", Scope: store.ScopeUser, Text: "数据库迁移幂等性验证"},
	}); err != nil {
		t.Fatalf("覆盖 Index: %v", err)
	}
	hits, err = p.Lexical.Search(ctx, "认证", store.ScopeUser, 10)
	if err != nil {
		t.Fatalf("Search after overwrite: %v", err)
	}
	if contains(hitIDs(hits), "u1") {
		t.Errorf("覆盖写后旧文本仍可检索（旧 token 未清理）: %v", hitIDs(hits))
	}
	hits, err = p.Lexical.Search(ctx, "迁移", store.ScopeUser, 10)
	if err != nil {
		t.Fatalf("Search(迁移): %v", err)
	}
	if !contains(hitIDs(hits), "u1") {
		t.Errorf("覆盖写后新文本不可检索: %v", hitIDs(hits))
	}

	// 删除后不可检索。
	if err := p.Lexical.Delete(ctx, []string{"u1"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	hits, err = p.Lexical.Search(ctx, "迁移", store.ScopeUser, 10)
	if err != nil {
		t.Fatalf("Search after delete: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("Delete 后仍可检索: %v", hitIDs(hits))
	}

	// 空文档 ID 必须报错（不能静默写入一条无法定位的文档）。
	if err := p.Lexical.Index(ctx, []store.Document{{ID: "", Scope: store.ScopeUser, Text: "x"}}); err == nil {
		t.Errorf("空文档 ID 未被拒绝")
	}
	// 空批次必须是无操作且不报错。
	if err := p.Lexical.Index(ctx, nil); err != nil {
		t.Errorf("空批次 Index 报错: %v", err)
	}
}

func runLexicalBigramSymmetry(t *testing.T, p store.Ports) {
	ctx := context.Background()
	if err := p.Lexical.Index(ctx, bigramDocs); err != nil {
		t.Fatalf("Index: %v", err)
	}

	// 语料里每个 2 字中文子串都必须命中它所在的文档（对称性的最终判据）。
	for _, doc := range bigramDocs {
		for _, sub := range cjkBigrams(doc.Text) {
			hits, err := p.Lexical.Search(ctx, sub, doc.Scope, 10)
			if err != nil {
				t.Fatalf("Search(%q): %v", sub, err)
			}
			if !contains(hitIDs(hits), doc.ID) {
				t.Errorf("doc %s：2 字查询 %q 未命中，hits=%v（索引侧/查询侧归一化不对称）",
					doc.ID, sub, hitIDs(hits))
			}
		}
	}
}

// ---------------------------------------------------------------------------
// VectorIndex
// ---------------------------------------------------------------------------

func runVectorKNN(t *testing.T, p store.Ports) {
	ctx := context.Background()

	// 三个**明显分离**的方向，避免 int8 量化误差把顺序翻掉。
	if err := p.Vector.Upsert(ctx, []store.Vector{
		{ID: "v-x", Scope: store.ScopeUser, Values: []float32{1, 0, 0}},
		{ID: "v-y", Scope: store.ScopeUser, Values: []float32{0, 1, 0}},
		{ID: "v-z", Scope: store.ScopeUser, Values: []float32{0, 0, 1}},
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	hits, err := p.Vector.Search(ctx, []float32{0.95, 0.05, 0}, store.ScopeUser, 3)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("Search 返回 %d 条, want 3", len(hits))
	}
	if hits[0].ID != "v-x" {
		t.Errorf("最相似应为 v-x，得到 %s（顺序 %v）", hits[0].ID, hitIDs(hits))
	}
	for i, h := range hits {
		if h.Rank != i+1 {
			t.Errorf("第 %d 项 Rank = %d, want %d", i, h.Rank, i+1)
		}
	}

	// k 生效。
	top1, err := p.Vector.Search(ctx, []float32{0, 1, 0}, store.ScopeUser, 1)
	if err != nil {
		t.Fatalf("Search(k=1): %v", err)
	}
	if len(top1) != 1 || top1[0].ID != "v-y" {
		t.Errorf("k=1 结果 = %v, want [v-y]", hitIDs(top1))
	}

	// 维度不一致的向量跳过而不报错（重建中的部分索引不该让召回整体失败）。
	if err := p.Vector.Upsert(ctx, []store.Vector{
		{ID: "v-2d", Scope: store.ScopeUser, Values: []float32{1, 1}},
	}); err != nil {
		t.Fatalf("Upsert(2d): %v", err)
	}
	hits, err = p.Vector.Search(ctx, []float32{1, 0, 0}, store.ScopeUser, 10)
	if err != nil {
		t.Fatalf("Search(混合维度): %v", err)
	}
	if contains(hitIDs(hits), "v-2d") {
		t.Errorf("维度不一致的向量被纳入结果: %v", hitIDs(hits))
	}

	// Delete 后不再返回。
	if err := p.Vector.Delete(ctx, []string{"v-z"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	hits, err = p.Vector.Search(ctx, []float32{0, 0, 1}, store.ScopeUser, 10)
	if err != nil {
		t.Fatalf("Search after delete: %v", err)
	}
	if contains(hitIDs(hits), "v-z") {
		t.Errorf("Delete 后仍返回 v-z: %v", hitIDs(hits))
	}
}

// ---------------------------------------------------------------------------
// GraphStore
// ---------------------------------------------------------------------------

func runGraphNeighbors(t *testing.T, p store.Ports) {
	ctx := context.Background()

	for _, n := range []store.Node{
		{ID: "n-a", Scope: store.ScopeUser, CanonicalName: "认证服务"},
		{ID: "n-b", Scope: store.ScopeUser, CanonicalName: "令牌服务"},
		{ID: "n-c", Scope: store.ScopeUser, CanonicalName: "用户表"},
		{ID: "n-d", Scope: store.ScopeUser, CanonicalName: "审计日志"},
	} {
		if err := p.Graph.PutNode(ctx, n); err != nil {
			t.Fatalf("PutNode(%s): %v", n.ID, err)
		}
	}
	for _, e := range []store.Edge{
		{ID: "e-ab", FromID: "n-a", ToID: "n-b", Relation: "depends_on", Weight: 1, TValid: 10, TCreated: 10},
		{ID: "e-bc", FromID: "n-b", ToID: "n-c", Relation: "depends_on", Weight: 1, TValid: 10, TCreated: 10},
		{ID: "e-ad", FromID: "n-a", ToID: "n-d", Relation: "logs_to", Weight: 1, TValid: 10, TCreated: 10},
	} {
		if err := p.Graph.PutEdge(ctx, e); err != nil {
			t.Fatalf("PutEdge(%s): %v", e.ID, err)
		}
	}

	// 1 跳：只出直接相邻的边。
	hops, err := p.Graph.Neighbors(ctx, store.NeighborQuery{NodeID: "n-a", Scope: store.ScopeUser, Hops: 1})
	if err != nil {
		t.Fatalf("Neighbors(1 hop): %v", err)
	}
	if got := hopIDs(hops); !sameStrings(got, []string{"e-ab", "e-ad"}) {
		t.Errorf("1 跳 = %v, want [e-ab e-ad]", got)
	}

	// 2 跳：多出 e-bc，且 depth 正确。
	hops, err = p.Graph.Neighbors(ctx, store.NeighborQuery{NodeID: "n-a", Scope: store.ScopeUser, Hops: 2})
	if err != nil {
		t.Fatalf("Neighbors(2 hops): %v", err)
	}
	if got := hopIDs(hops); !sameStrings(got, []string{"e-ab", "e-ad", "e-bc"}) {
		t.Errorf("2 跳 = %v, want [e-ab e-ad e-bc]", got)
	}
	for _, h := range hops {
		want := 1
		if h.Edge.ID == "e-bc" {
			want = 2
		}
		if h.Depth != want {
			t.Errorf("边 %s Depth = %d, want %d", h.Edge.ID, h.Depth, want)
		}
	}

	// 关系白名单：只看 depends_on。
	hops, err = p.Graph.Neighbors(ctx, store.NeighborQuery{
		NodeID: "n-a", Scope: store.ScopeUser, Hops: 2, Relations: []store.Relation{"depends_on"},
	})
	if err != nil {
		t.Fatalf("Neighbors(relation filter): %v", err)
	}
	if got := hopIDs(hops); !sameStrings(got, []string{"e-ab", "e-bc"}) {
		t.Errorf("关系过滤 = %v, want [e-ab e-bc]（logs_to 应被排除）", got)
	}

	// 定向：从 n-c 出发没有出边（图是 from→to 单向的）。
	hops, err = p.Graph.Neighbors(ctx, store.NeighborQuery{NodeID: "n-c", Scope: store.ScopeUser, Hops: 3})
	if err != nil {
		t.Fatalf("Neighbors(n-c): %v", err)
	}
	if len(hops) != 0 {
		t.Errorf("n-c 无出边，应返回空，得到 %v", hopIDs(hops))
	}

	// 空 NodeID 报错。
	if _, err := p.Graph.Neighbors(ctx, store.NeighborQuery{Scope: store.ScopeUser}); err == nil {
		t.Errorf("空 NodeID 未报错")
	}
	// 空 Scope 必须报错：它是隔离边界，不是"不限"（I-2）。
	if _, err := p.Graph.Neighbors(ctx, store.NeighborQuery{NodeID: "n-a"}); err == nil {
		t.Errorf("空 Scope 未报错，跨 scope 隔离失去强制力")
	}
}

// ---------------------------------------------------------------------------
// 端到端：两路召回 → RRF → 顺序由契约决定
// ---------------------------------------------------------------------------

func runRRFFusionOrder(t *testing.T, p store.Ports) {
	ctx := context.Background()

	// 语料：只有 d-auth 命中「认证」与「令牌」两个词，d-migrate 只命中「迁移」。
	if err := p.Lexical.Index(ctx, []store.Document{
		{ID: "d-auth", Scope: store.ScopeUser, Text: "认证令牌过期的处理流程"},
		{ID: "d-migrate", Scope: store.ScopeUser, Text: "数据库迁移步骤"},
	}); err != nil {
		t.Fatalf("Index: %v", err)
	}
	// 向量路：v-auth 最接近查询向量。
	if err := p.Vector.Upsert(ctx, []store.Vector{
		{ID: "v-auth", Scope: store.ScopeUser, Values: []float32{1, 0, 0}},
		{ID: "v-other", Scope: store.ScopeUser, Values: []float32{0, 1, 0}},
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	lexHits, err := p.Lexical.Search(ctx, "认证令牌", store.ScopeUser, 10)
	if err != nil {
		t.Fatalf("lexical search: %v", err)
	}
	vecHits, err := p.Vector.Search(ctx, []float32{1, 0, 0}, store.ScopeUser, 10)
	if err != nil {
		t.Fatalf("vector search: %v", err)
	}
	if len(lexHits) == 0 || len(vecHits) == 0 {
		t.Fatalf("前提失效：lexHits=%v vecHits=%v", hitIDs(lexHits), hitIDs(vecHits))
	}

	fused := fusion.RRF(fusion.DefaultK,
		fusion.Channel{Name: "lexical", Items: toItems(lexHits)},
		fusion.Channel{Name: "vector", Items: toItems(vecHits)},
	)
	if len(fused) == 0 {
		t.Fatalf("RRF 结果为空")
	}
	// 融合分必须单调不增（排序契约）。
	for i := 1; i < len(fused); i++ {
		if fused[i].Score > fused[i-1].Score {
			t.Errorf("融合结果未按分降序：第 %d 项 %.10f > 第 %d 项 %.10f",
				i, fused[i].Score, i-1, fused[i-1].Score)
		}
	}
	// 单路出现过的候选都必须保留（RRF 不做阈值剪枝）。
	merged := map[string]bool{}
	for _, f := range fused {
		merged[f.ID] = true
	}
	for _, h := range append(append([]store.Hit{}, lexHits...), vecHits...) {
		if !merged[h.ID] {
			t.Errorf("候选 %s 在融合中丢失", h.ID)
		}
	}
}

func toItems(hits []store.Hit) []fusion.Item {
	out := make([]fusion.Item, len(hits))
	for i, h := range hits {
		out[i] = fusion.Item{ID: h.ID, Score: h.Score}
	}
	return out
}

func hopIDs(hops []store.EdgeHop) []string {
	out := make([]string, len(hops))
	for i, h := range hops {
		out[i] = h.Edge.ID
	}
	return out
}

// 保证 sqlite 包在只跑本文件时也被引用（一致性测试需要它的 Ports）。
var _ = (*sqlite.DB)(nil)
