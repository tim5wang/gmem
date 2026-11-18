package sqlite

import (
	"context"
	"testing"

	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// D13 缺口 1：FTS 必须按 (id, version) 索引
//
// 本文件是 leader 探针的**永久回归**：探针曾实测出
//   - 同 id 写 v1("请求超时设为 30s") 再写 v2("请求超时设为 60s") 后，
//     检索 "30s" 返回 0 命中（旧版被 Index 按 id 整体覆盖掉了）；
//   - as_of 回溯因此静默丢失词法通道，与 D12 的承诺相反。
//
// 变异验证方法见文件末尾注释：把 Index 改回 `DELETE ... WHERE id = ?` 后，
// TestLexicalVersionedAsOfProbeRegression 与 TestLexicalIndexKeepsOtherVersions 必须变红。
// ===========================================================================

// indexMemoryVersion 把某条记忆的**当前版本**写进词法索引。
//
// 生产路径上"PutMemory + Lexical.Index"由调用方在同一事务里完成；
// 这里按同样的方式取版本号，避免测试自己去猜 version。
func indexMemoryVersion(t *testing.T, d *DB, id string) store.Memory {
	t.Helper()
	ctx := context.Background()
	m, err := d.Store().GetMemory(ctx, id)
	if err != nil {
		t.Fatalf("GetMemory(%s): %v", id, err)
	}
	if err := d.Lexical().Index(ctx, []store.Document{
		{ID: m.ID, Version: m.Version, Scope: m.Scope, Text: m.Content},
	}); err != nil {
		t.Fatalf("Lexical.Index(%s v%d): %v", id, m.Version, err)
	}
	return m
}

// TestLexicalVersionedAsOfProbeRegression 是 D13 探针的逐字回归。
//
// 断言矩阵（"30s" 只存在于 v1，"60s" 只存在于 v2）：
//   - 默认检索：只有 v2 可见 → "60s" 命中 v2、"30s" 零命中；
//   - as_of 落在 v1 有效期：只有 v1 可见 → "30s" 命中 v1、"60s" 零命中；
//   - as_of 落在 v2 生效后：只有 v2 可见 → "60s" 命中 v2、"30s" 零命中。
func TestLexicalVersionedAsOfProbeRegression(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	s := d.Store()

	const id = "m-probe"
	// v1：t_created=1100；v2：t_created=1200 并把 v1 置为过期。
	if err := s.PutMemory(ctx, store.Memory{
		ID: id, Scope: store.ScopeUser, Kind: store.KindSemantic,
		Content: "请求超时设为 30s", TValid: 1000, TCreated: 1100,
	}); err != nil {
		t.Fatalf("PutMemory v1: %v", err)
	}
	v1 := indexMemoryVersion(t, d, id)

	if err := s.PutMemory(ctx, store.Memory{
		ID: id, Scope: store.ScopeUser, Kind: store.KindSemantic,
		Content: "请求超时设为 60s", TValid: 1000, TCreated: 1200,
	}); err != nil {
		t.Fatalf("PutMemory v2: %v", err)
	}
	v2 := indexMemoryVersion(t, d, id)

	if v1.Version != 1 || v2.Version != 2 {
		t.Fatalf("版本号 = v%d / v%d, want v1 / v2", v1.Version, v2.Version)
	}

	search := func(query string, asOf *int64) []store.Hit {
		t.Helper()
		hits, err := d.Lexical().Search(ctx, store.LexicalQuery{
			Query: query, Scope: store.ScopeUser, AsOf: asOf, Limit: 10,
		})
		if err != nil {
			t.Fatalf("Search(%q, asOf=%v): %v", query, asOf, err)
		}
		return hits
	}

	// 默认：只召回当前版本（v1 已被 t_expired）。
	if hits := search("30s", nil); len(hits) != 0 {
		t.Errorf("默认检索 \"30s\" 命中 %v, want 0（v1 已过期，不应召回旧版内容）", hits)
	}
	hits := search("60s", nil)
	if len(hits) != 1 || hits[0].ID != id || hits[0].Version != 2 {
		t.Errorf("默认检索 \"60s\" = %v, want [{id:%s v2}]", hits, id)
	}

	// as_of=1150：v1 仍在有效期内。
	asOfV1 := int64(1150)
	hits = search("30s", &asOfV1)
	if len(hits) != 1 || hits[0].ID != id || hits[0].Version != 1 {
		t.Errorf("as_of=1150 检索 \"30s\" = %v, want [{id:%s v1}]（旧版词法索引丢失）", hits, id)
	}
	if hits := search("60s", &asOfV1); len(hits) != 0 {
		t.Errorf("as_of=1150 检索 \"60s\" 命中 %v, want 0（该时刻 v2 尚未写入）", hits)
	}

	// as_of=1300：v2 已生效、v1 已过期，方向必须反过来。
	asOfV2 := int64(1300)
	if hits := search("30s", &asOfV2); len(hits) != 0 {
		t.Errorf("as_of=1300 检索 \"30s\" 命中 %v, want 0（v1 已过期）", hits)
	}
	hits = search("60s", &asOfV2)
	if len(hits) != 1 || hits[0].ID != id || hits[0].Version != 2 {
		t.Errorf("as_of=1300 检索 \"60s\" = %v, want [{id:%s v2}]", hits, id)
	}
}

// TestLexicalIndexKeepsOtherVersions 直接盯住 Index/Delete 的覆盖粒度。
//
// 旧实现 `DELETE FROM memories_fts WHERE id = ?` 会在写 v2 时删掉 v1，
// 本用例断言 v1 依然在索引里（这是 probe 回归的最小单元）。
func TestLexicalIndexKeepsOtherVersions(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	if err := d.Lexical().Index(ctx, []store.Document{
		{ID: "m-keep", Version: 1, Scope: store.ScopeUser, Text: "旧版本内容 30s"},
	}); err != nil {
		t.Fatalf("Index v1: %v", err)
	}
	if err := d.Lexical().Index(ctx, []store.Document{
		{ID: "m-keep", Version: 2, Scope: store.ScopeUser, Text: "新版本内容 60s"},
	}); err != nil {
		t.Fatalf("Index v2: %v", err)
	}

	var rows int
	if err := d.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM memories_fts WHERE id = 'm-keep'`).Scan(&rows); err != nil {
		t.Fatalf("count fts rows: %v", err)
	}
	if rows != 2 {
		t.Errorf("memories_fts 中 m-keep 行数 = %d, want 2（写 v2 不得删掉 v1）", rows)
	}

	// 同一 (id, version) 重写必须覆盖，而不是插出重复行。
	if err := d.Lexical().Index(ctx, []store.Document{
		{ID: "m-keep", Version: 2, Scope: store.ScopeUser, Text: "新版本内容 90s"},
	}); err != nil {
		t.Fatalf("重新 Index v2: %v", err)
	}
	if err := d.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM memories_fts WHERE id = 'm-keep'`).Scan(&rows); err != nil {
		t.Fatalf("count fts rows: %v", err)
	}
	if rows != 2 {
		t.Errorf("重写 v2 后行数 = %d, want 2（(id,version) 覆盖不产生重复）", rows)
	}

	// Delete 也必须按 (id, version) 精确删除。
	if err := d.Lexical().Delete(ctx, []store.DocumentKey{{ID: "m-keep", Version: 2}}); err != nil {
		t.Fatalf("Delete v2: %v", err)
	}
	if err := d.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM memories_fts WHERE id = 'm-keep'`).Scan(&rows); err != nil {
		t.Fatalf("count fts rows: %v", err)
	}
	if rows != 1 {
		t.Errorf("删 v2 后行数 = %d, want 1（不得连带删掉 v1）", rows)
	}
}

// TestLexicalHitCarriesVersion 固定 Hit.Version 的语义：词法命中能表达"命中的是哪个版本"。
func TestLexicalHitCarriesVersion(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	if err := d.Lexical().Index(ctx, []store.Document{
		{ID: "m-hv", Version: 3, Scope: store.ScopeUser, Text: "版本标记检索"},
	}); err != nil {
		t.Fatalf("Index: %v", err)
	}
	hits, err := d.Lexical().Search(ctx, store.LexicalQuery{
		Query: "版本", Scope: store.ScopeUser, Limit: 10,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("命中数 = %d, want 1（hits=%v）", len(hits), hits)
	}
	if hits[0].Version != 3 {
		t.Errorf("Hit.Version = %d, want 3（词法命中必须带版本）", hits[0].Version)
	}
}

// ===========================================================================
// D13 缺口 2：recall_count 不得催生版本
//
// 探针曾实测：仅 recall_count 从 0 变到 3、内容完全相同，却得到 v4。
// 根因是 memorySame 把 RecallCount 纳入等价比较。
//
// 变异验证：把 `a.RecallCount == b.RecallCount` 加回 memorySame 后，
// TestRecallCountDoesNotCreateVersion 与 TestTouchRecallKeepsVersion 必须变红。
// ===========================================================================

// TestRecallCountDoesNotCreateVersion：连续 4 次仅 recall_count 变化的写入，版本恒为 1。
func TestRecallCountDoesNotCreateVersion(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	s := d.Store()

	m := store.Memory{
		ID: "m-rc", Scope: store.ScopeUser, Kind: store.KindSemantic,
		Content: "召回计数不得产生版本", TValid: 100, TCreated: 100,
	}
	if err := s.PutMemory(ctx, m); err != nil {
		t.Fatalf("PutMemory 首次: %v", err)
	}

	for i := 1; i <= 4; i++ {
		m.RecallCount = i
		if err := s.PutMemory(ctx, m); err != nil {
			t.Fatalf("PutMemory 第 %d 次(recall_count=%d): %v", i, i, err)
		}
		cur, err := s.GetMemory(ctx, "m-rc")
		if err != nil {
			t.Fatalf("GetMemory 第 %d 次: %v", i, err)
		}
		if cur.Version != 1 {
			t.Fatalf("第 %d 次仅 recall_count 变化后 Version = %d, want 1（运营字段不得版本化）",
				i, cur.Version)
		}
	}

	var rows int
	if err := d.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM memories WHERE id = 'm-rc'`).Scan(&rows); err != nil {
		t.Fatalf("count memories: %v", err)
	}
	if rows != 1 {
		t.Errorf("memories 行数 = %d, want 1（recall_count 变化不得插新版本）", rows)
	}
}

// TestTouchRecallKeepsVersion：召回计数走独立路径，只更新当前版本、不产生新版本。
func TestTouchRecallKeepsVersion(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	s := d.Store()

	if err := s.PutMemory(ctx, store.Memory{
		ID: "m-touch", Scope: store.ScopeUser, Kind: store.KindSemantic,
		Content: "召回一次计数加一", TValid: 100, TCreated: 100,
	}); err != nil {
		t.Fatalf("PutMemory: %v", err)
	}

	for i := 1; i <= 3; i++ {
		if err := s.TouchRecall(ctx, "m-touch"); err != nil {
			t.Fatalf("TouchRecall 第 %d 次: %v", i, err)
		}
		cur, err := s.GetMemory(ctx, "m-touch")
		if err != nil {
			t.Fatalf("GetMemory 第 %d 次: %v", i, err)
		}
		if cur.Version != 1 {
			t.Fatalf("第 %d 次 TouchRecall 后 Version = %d, want 1", i, cur.Version)
		}
		if cur.RecallCount != i {
			t.Fatalf("第 %d 次 TouchRecall 后 RecallCount = %d, want %d", i, cur.RecallCount, i)
		}
	}

	var rows int
	if err := d.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM memories WHERE id = 'm-touch'`).Scan(&rows); err != nil {
		t.Fatalf("count memories: %v", err)
	}
	if rows != 1 {
		t.Errorf("memories 行数 = %d, want 1（TouchRecall 不得插新版本）", rows)
	}

	// 不存在的 id 必须报 ErrNotFound（不能静默当成"计数成功"）。
	if err := s.TouchRecall(ctx, "no-such"); err == nil {
		t.Errorf("TouchRecall(不存在) 未返回错误")
	}
}
