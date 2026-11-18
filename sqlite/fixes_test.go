package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// P0 阻塞项 / 重要项的回归测试
//
// 每条用例都对应任务卡上的一个编号，断言**行为**而不是实现细节。
// 每条都做过变异验证（把修复改回缺陷形态，确认用例确实变红）。
// ===========================================================================

// TestFixB2UpsertKeepsTInvalid：已失效事实不得因再次写入而复活。
func TestFixB2UpsertKeepsTInvalid(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	s := d.Store()

	m := store.Memory{
		ID: "m-b2", Scope: store.ScopeUser, Kind: store.KindSemantic,
		Content: "部署走蓝绿", TValid: 100, TCreated: 100,
	}
	if err := s.PutMemory(ctx, m); err != nil {
		t.Fatalf("PutMemory: %v", err)
	}
	if err := s.InvalidateMemory(ctx, "m-b2", 500); err != nil {
		t.Fatalf("InvalidateMemory: %v", err)
	}

	// 再次写入同一 id（TInvalid 留空，模拟"整理层又抽到同一条事实"）。
	if err := s.PutMemory(ctx, m); err != nil {
		t.Fatalf("再次 PutMemory: %v", err)
	}

	got, err := s.GetMemory(ctx, "m-b2")
	if err != nil {
		t.Fatalf("GetMemory: %v", err)
	}
	if got.TInvalid != 500 {
		t.Errorf("再次写入后 TInvalid = %d, want 500（已失效事实复活）", got.TInvalid)
	}

	cur, err := s.ListMemories(ctx, store.MemoryQuery{})
	if err != nil {
		t.Fatalf("ListMemories: %v", err)
	}
	for _, x := range cur {
		if x.ID == "m-b2" {
			t.Errorf("已失效事实出现在默认查询里: %+v", x)
		}
	}

	// 走 ON CONFLICT 的"同版本重写"路径也不得复活：直接再写一次同版本。
	if err := s.PutMemory(ctx, m); err != nil {
		t.Fatalf("第三次 PutMemory: %v", err)
	}
	var invalid sql.NullInt64
	if err := d.db.QueryRowContext(ctx,
		`SELECT t_invalid FROM memories WHERE id = 'm-b2' AND t_expired IS NULL`).
		Scan(&invalid); err != nil {
		t.Fatalf("查 t_invalid: %v", err)
	}
	if !invalid.Valid || invalid.Int64 != 500 {
		t.Errorf("当前版本 t_invalid = %+v, want 500（NULL 或其它值说明被抹掉）", invalid)
	}
}

// TestFixB3InvalidateZeroLandsNull：InvalidateMemory(id, 0) 必须落成 SQL NULL。
func TestFixB3InvalidateZeroLandsNull(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	s := d.Store()

	if err := s.PutMemory(ctx, store.Memory{
		ID: "m-b3", Scope: store.ScopeUser, Kind: store.KindSemantic,
		Content: "灰度发布先放 1%", TValid: 100, TCreated: 100,
	}); err != nil {
		t.Fatalf("PutMemory: %v", err)
	}
	if err := s.InvalidateMemory(ctx, "m-b3", 0); err != nil {
		t.Fatalf("InvalidateMemory(0): %v", err)
	}

	var inv sql.NullInt64
	if err := d.db.QueryRowContext(ctx,
		`SELECT t_invalid FROM memories WHERE id = 'm-b3'`).Scan(&inv); err != nil {
		t.Fatalf("查 t_invalid: %v", err)
	}
	if inv.Valid {
		t.Errorf("t_invalid = {Valid:true Int64:%d}, want NULL（0 必须走 nullableInt）", inv.Int64)
	}

	// t_invalid=0 语义上是"未失效"，因此记录仍应在默认查询里。
	got, err := s.ListMemories(ctx, store.MemoryQuery{})
	if err != nil {
		t.Fatalf("ListMemories: %v", err)
	}
	if len(got) != 1 || got[0].ID != "m-b3" {
		t.Errorf("t_invalid=NULL 的记录应可见，得到 %+v", got)
	}
}

// TestFixB4VersionedAsOfReturnsPreviousVersion 是 D12 的核心断言。
//
// 同一 id 先后以 t_created=1100 / 1200 写入两个版本，
// as_of=1150 必须返回**前一版**（而不是 0 行，也不是新版）。
func TestFixB4VersionedAsOfReturnsPreviousVersion(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	s := d.Store()

	if err := s.PutMemory(ctx, store.Memory{
		ID: "m-b4", Scope: store.ScopeUser, Kind: store.KindSemantic,
		Content: "超时设为 30s", TValid: 1000, TCreated: 1100,
	}); err != nil {
		t.Fatalf("PutMemory v1: %v", err)
	}
	if err := s.PutMemory(ctx, store.Memory{
		ID: "m-b4", Scope: store.ScopeUser, Kind: store.KindSemantic,
		Content: "超时设为 60s", TValid: 1000, TCreated: 1200,
	}); err != nil {
		t.Fatalf("PutMemory v2: %v", err)
	}

	// 两版都在库里（版本化，而不是覆盖）。
	var versions int
	if err := d.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM memories WHERE id = 'm-b4'`).Scan(&versions); err != nil {
		t.Fatalf("count versions: %v", err)
	}
	if versions != 2 {
		t.Errorf("memories 版本数 = %d, want 2（Update 应插新版而非覆盖）", versions)
	}

	// 旧版必须被置 t_expired（t_expired 的真实写入路径）。
	var expired sql.NullInt64
	if err := d.db.QueryRowContext(ctx,
		`SELECT t_expired FROM memories WHERE id = 'm-b4' AND version = 1`).Scan(&expired); err != nil {
		t.Fatalf("查 v1 t_expired: %v", err)
	}
	if !expired.Valid {
		t.Fatalf("v1.t_expired 为 NULL，t_expired 没有写入路径")
	}

	// as_of=1150：只有前一版成立。
	asOf := int64(1150)
	got, err := s.ListMemories(ctx, store.MemoryQuery{AsOf: &asOf})
	if err != nil {
		t.Fatalf("ListMemories(as_of=1150): %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("as_of=1150 返回 %d 行, want 1（0 行说明系统时间轴未生效）", len(got))
	}
	if got[0].Content != "超时设为 30s" {
		t.Errorf("as_of=1150 内容 = %q, want 前一版 %q", got[0].Content, "超时设为 30s")
	}
	if got[0].Version != 1 {
		t.Errorf("as_of=1150 版本 = %d, want 1", got[0].Version)
	}

	// 默认查询只给当前版本（新版）。
	cur, err := s.GetMemory(ctx, "m-b4")
	if err != nil {
		t.Fatalf("GetMemory: %v", err)
	}
	if cur.Content != "超时设为 60s" || cur.Version != 2 {
		t.Errorf("当前版本 = {%q v%d}, want {超时设为 60s v2}", cur.Content, cur.Version)
	}

	// as_of 早于所有版本：0 行。
	early := int64(1000)
	if rows, err := s.ListMemories(ctx, store.MemoryQuery{AsOf: &early}); err != nil {
		t.Fatalf("ListMemories(as_of=1000): %v", err)
	} else if len(rows) != 0 {
		t.Errorf("as_of=1000 应不可见，得到 %d 行", len(rows))
	}
}

// TestFixB5ExpiredEdgeInvisible 验证边的系统时间轴：t_expired=150 的边默认不可见。
func TestFixB5ExpiredEdgeInvisible(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	g := d.Graph()

	for _, id := range []string{"a", "b"} {
		if err := g.PutNode(ctx, store.Node{ID: id, Scope: store.ScopeUser, CanonicalName: id}); err != nil {
			t.Fatalf("PutNode %s: %v", id, err)
		}
	}
	if err := g.PutEdge(ctx, store.Edge{
		ID: "e-exp", FromID: "a", ToID: "b", Relation: "works_at", Weight: 1,
		TValid: 10, TCreated: 10, TExpired: 150,
	}); err != nil {
		t.Fatalf("PutEdge: %v", err)
	}

	hops, err := g.Neighbors(ctx, store.NeighborQuery{
		NodeID: "a", Scope: store.ScopeUser, Hops: 1,
	})
	if err != nil {
		t.Fatalf("Neighbors default: %v", err)
	}
	if len(hops) != 0 {
		t.Errorf("t_expired 的边默认应不可见，得到 %v", hopIDsOf(hops))
	}

	// as_of=100：系统时间与世界时间都成立 → 可见。
	at100 := int64(100)
	hops, err = g.Neighbors(ctx, store.NeighborQuery{
		NodeID: "a", Scope: store.ScopeUser, Hops: 1, AsOf: &at100,
	})
	if err != nil {
		t.Fatalf("Neighbors as_of=100: %v", err)
	}
	if len(hops) != 1 || hops[0].Edge.ID != "e-exp" {
		t.Errorf("as_of=100 应可见 e-exp，得到 %v", hopIDsOf(hops))
	}

	// as_of=150（= t_expired）：半开区间，不可见。
	at150 := int64(150)
	hops, err = g.Neighbors(ctx, store.NeighborQuery{
		NodeID: "a", Scope: store.ScopeUser, Hops: 1, AsOf: &at150,
	})
	if err != nil {
		t.Fatalf("Neighbors as_of=150: %v", err)
	}
	if len(hops) != 0 {
		t.Errorf("as_of=t_expired 应不可见（半开区间），得到 %v", hopIDsOf(hops))
	}

	// 边的 Update 也必须置旧版 t_expired。
	//
	// 关键：这里用的 e-ver 第一条**不显式给 TExpired**，
	// 因此"旧版已过期"只可能来自 Update 的置位逻辑（而不是测试自己写进去的）。
	for _, id := range []string{"a", "b"} {
		if err := g.PutNode(ctx, store.Node{ID: id, Scope: store.ScopeUser, CanonicalName: id}); err != nil {
			t.Fatalf("PutNode: %v", err)
		}
	}
	if err := g.PutEdge(ctx, store.Edge{
		ID: "e-ver", FromID: "a", ToID: "b", Relation: "works_at", Weight: 1,
		TValid: 10, TCreated: 100,
	}); err != nil {
		t.Fatalf("PutEdge e-ver v1: %v", err)
	}
	if err := g.PutEdge(ctx, store.Edge{
		ID: "e-ver", FromID: "a", ToID: "b", Relation: "works_at", Weight: 2,
		TValid: 10, TCreated: 200,
	}); err != nil {
		t.Fatalf("PutEdge e-ver v2: %v", err)
	}

	var cur, old int
	if err := d.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM edges WHERE id = 'e-ver' AND t_expired IS NULL`).Scan(&cur); err != nil {
		t.Fatalf("count current edges: %v", err)
	}
	if err := d.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM edges WHERE id = 'e-ver' AND t_expired IS NOT NULL`).Scan(&old); err != nil {
		t.Fatalf("count expired edges: %v", err)
	}
	if cur != 1 || old != 1 {
		t.Errorf("边版本状态: current=%d expired=%d, want 1/1（旧版必须由 Update 置 t_expired）", cur, old)
	}

	// 旧版置位后就默认不可见，只有当前版本可见。
	hops, err = g.Neighbors(ctx, store.NeighborQuery{
		NodeID: "a", Scope: store.ScopeUser, Hops: 1,
	})
	if err != nil {
		t.Fatalf("Neighbors after update: %v", err)
	}
	if len(hops) != 1 || hops[0].Edge.Version != 2 {
		t.Errorf("Update 后邻域应只剩当前版本 v2，得到 %+v", hops)
	}
}

// TestFixI1PragmaPerConnection：PRAGMA 必须随每条新连接生效（DSN 形式）。
func TestFixI1PragmaPerConnection(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "pragma.db")

	d, err := Open(ctx, Config{Path: path, BusyTimeout: 3 * 1000 * 1000 * 1000}) // 3s
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = d.Close() }()

	// 用同一份 DSN 另开一个连接池：每条新连接都必须带上 pragma。
	// 这正是旧实现（Open 里 Exec 一次 + continue 吞失败）做不到的。
	dsn := buildDSN(path, 3e9)
	fresh, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open fresh conn pool: %v", err)
	}
	defer func() { _ = fresh.Close() }()
	fresh.SetMaxOpenConns(1)

	var fk, busy int
	if err := fresh.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatalf("PRAGMA foreign_keys: %v", err)
	}
	if err := fresh.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busy); err != nil {
		t.Fatalf("PRAGMA busy_timeout: %v", err)
	}
	if fk != 1 {
		t.Errorf("新连接 foreign_keys = %d, want 1（pragma 未随连接生效）", fk)
	}
	if busy != 3000 {
		t.Errorf("新连接 busy_timeout = %d, want 3000（配置未进入 DSN）", busy)
	}

	// 外键约束真的会拒绝越界写入（证明 foreign_keys 生效而非仅报告值）。
	if _, err := fresh.ExecContext(ctx,
		`INSERT INTO aliases (surface, node_id) VALUES ('x', 'no-such-node')`); err == nil {
		t.Error("外键约束未生效：越界 node_id 被接受")
	}
}

// TestFixI2NeighborsScopeIsolation：Neighbors 必须强制 scope 过滤，杜绝跨 scope 泄漏。
func TestFixI2NeighborsScopeIsolation(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	g := d.Graph()

	// user scope：a→b；agent scope：b→c（同一终点节点被两个 scope 共享 id 是不可能的，
	// 因此这里用"user 的 a 试图走向 agent 的 c"来构造跨 scope 边）。
	for _, n := range []store.Node{
		{ID: "u-a", Scope: store.ScopeUser, CanonicalName: "用户A"},
		{ID: "u-b", Scope: store.ScopeUser, CanonicalName: "用户B"},
		{ID: "ag-c", Scope: store.ScopeAgent, CanonicalName: "AgentC"},
	} {
		if err := g.PutNode(ctx, n); err != nil {
			t.Fatalf("PutNode %s: %v", n.ID, err)
		}
	}
	// 跨 scope 边：起点 user、终点 agent。这是 review 实测出现过的那种泄漏。
	if err := g.PutEdge(ctx, store.Edge{
		ID: "leak", FromID: "u-a", ToID: "ag-c", Relation: "depends_on",
		Weight: 1, TValid: 1, TCreated: 1,
	}); err != nil {
		t.Fatalf("PutEdge leak: %v", err)
	}
	if err := g.PutEdge(ctx, store.Edge{
		ID: "same", FromID: "u-a", ToID: "u-b", Relation: "depends_on",
		Weight: 1, TValid: 1, TCreated: 1,
	}); err != nil {
		t.Fatalf("PutEdge same: %v", err)
	}

	hops, err := g.Neighbors(ctx, store.NeighborQuery{
		NodeID: "u-a", Scope: store.ScopeUser, Hops: 1,
	})
	if err != nil {
		t.Fatalf("Neighbors: %v", err)
	}
	ids := hopIDsOf(hops)
	for _, id := range ids {
		if id == "leak" {
			t.Fatalf("跨 scope 边泄漏到 user scope 的邻域结果: %v", ids)
		}
	}
	if len(ids) != 1 || ids[0] != "same" {
		t.Errorf("user scope 1 跳 = %v, want [same]", ids)
	}

	// 空 Scope 必须报错（它是必填隔离边界，不是"不限"）。
	if _, err := g.Neighbors(ctx, store.NeighborQuery{NodeID: "u-a", Hops: 1}); err == nil {
		t.Error("空 Scope 未报错，隔离失去强制力")
	}
	// agent scope 从 u-a 出发看不到任何东西。
	if hops, err := g.Neighbors(ctx, store.NeighborQuery{
		NodeID: "u-a", Scope: store.ScopeAgent, Hops: 1,
	}); err != nil {
		t.Fatalf("Neighbors(agent): %v", err)
	} else if len(hops) != 0 {
		t.Errorf("agent scope 不应看到 user 的边，得到 %v", hopIDsOf(hops))
	}
}

// TestFixI3OffsetWithoutLimit：Offset 在无 Limit 时也必须生效。
func TestFixI3OffsetWithoutLimit(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	s := d.Store()

	for i := 0; i < 3; i++ {
		if err := s.PutMemory(ctx, store.Memory{
			ID: fmt.Sprintf("m-off-%d", i), Scope: store.ScopeUser, Kind: store.KindSemantic,
			Content: fmt.Sprintf("第 %d 条", i), TValid: int64(100 + i), TCreated: 100,
		}); err != nil {
			t.Fatalf("PutMemory %d: %v", i, err)
		}
	}

	all, err := s.ListMemories(ctx, store.MemoryQuery{})
	if err != nil {
		t.Fatalf("ListMemories: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("全量 = %d, want 3", len(all))
	}

	got, err := s.ListMemories(ctx, store.MemoryQuery{Offset: 1})
	if err != nil {
		t.Fatalf("ListMemories(Offset=1): %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Offset=1 无 Limit 返回 %d 行, want 2（Offset 被丢弃）", len(got))
	}
	if got[0].ID != all[1].ID || got[1].ID != all[2].ID {
		t.Errorf("Offset=1 结果 = %v, want %v", memIDsOf(got), memIDsOf(all[1:]))
	}

	// Limit 与 Offset 同时给：先跳后取。
	lim, err := s.ListMemories(ctx, store.MemoryQuery{Offset: 1, Limit: 1})
	if err != nil {
		t.Fatalf("ListMemories(Offset=1, Limit=1): %v", err)
	}
	if len(lim) != 1 || lim[0].ID != all[1].ID {
		t.Errorf("Offset=1 Limit=1 = %v, want [%s]", memIDsOf(lim), all[1].ID)
	}

	// Offset 超过总数：空结果，不报错。
	if rows, err := s.ListMemories(ctx, store.MemoryQuery{Offset: 99}); err != nil {
		t.Fatalf("ListMemories(Offset=99): %v", err)
	} else if len(rows) != 0 {
		t.Errorf("Offset=99 应返回空，得到 %d 行", len(rows))
	}
}

// TestFixI4NodesSourceEpisode：nodes 必须能溯源回 episode。
func TestFixI4NodesSourceEpisode(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	ep := seedEpisode(t, d, "ep-node")

	if err := d.Graph().PutNode(ctx, store.Node{
		ID: "n-src", Scope: store.ScopeUser, CanonicalName: "认证服务",
		SourceEpisode: ep.ID, PropsJSON: `{"env":"prod"}`,
	}); err != nil {
		t.Fatalf("PutNode: %v", err)
	}

	got, err := d.Graph().GetNode(ctx, "n-src")
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if got.SourceEpisode != ep.ID {
		t.Errorf("SourceEpisode = %q, want %q", got.SourceEpisode, ep.ID)
	}

	// 列必须真的存在于 schema 里（不只是结构体字段）。
	cols := columnNames(t, d, "nodes")
	if !cols["source_episode"] {
		t.Errorf("nodes 表缺 source_episode 列，现有列 %v", cols)
	}

	// 未提供溯源时不应伪造。
	if err := d.Graph().PutNode(ctx, store.Node{
		ID: "n-nosrc", Scope: store.ScopeUser, CanonicalName: "审计日志",
	}); err != nil {
		t.Fatalf("PutNode(no src): %v", err)
	}
	nosrc, err := d.Graph().GetNode(ctx, "n-nosrc")
	if err != nil {
		t.Fatalf("GetNode(no src): %v", err)
	}
	if nosrc.SourceEpisode != "" {
		t.Errorf("未提供 SourceEpisode 却得到 %q", nosrc.SourceEpisode)
	}
}

// TestFixI5InvalidKindsRejected：非法 Kinds 必须返回错误，不得静默空集。
func TestFixI5InvalidKindsRejected(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	s := d.Store()

	if err := s.PutMemory(ctx, store.Memory{
		ID: "m-k", Scope: store.ScopeUser, Kind: store.KindSemantic,
		Content: "合法记忆", TValid: 1, TCreated: 1,
	}); err != nil {
		t.Fatalf("PutMemory: %v", err)
	}

	// 单个非法值。
	if _, err := s.ListMemories(ctx, store.MemoryQuery{
		Kinds: []store.MemoryKind{store.MemoryKind("bogus")},
	}); err == nil {
		t.Error("非法 Kinds 未报错，静默返回空集会掩盖调用方缺陷")
	}

	// 合法与非法混在一起：也必须报错，不能"过滤掉非法的然后正常返回"。
	if _, err := s.ListMemories(ctx, store.MemoryQuery{
		Kinds: []store.MemoryKind{store.KindSemantic, store.MemoryKind("bogus")},
	}); err == nil {
		t.Error("混合 Kinds 未报错")
	}

	// 合法值仍然正常（不要过度收紧）。
	got, err := s.ListMemories(ctx, store.MemoryQuery{
		Kinds: []store.MemoryKind{store.KindSemantic, store.KindDecision},
	})
	if err != nil {
		t.Fatalf("合法 Kinds: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("合法 Kinds 返回 %d 行, want 1", len(got))
	}
}

// TestFixTxConflictInsteadOfBlocking：拿不到事务句柄的跨 Port 调用必须快速报错，绝不阻塞。
//
// 这是 B-1 的另一半：§11 要求"无法注入时必须快速返回 ErrTxConflict"。
// 这里在事务回调内用**外层 ctx**（没有注入事务）调其它 Port，断言：
//   - 立刻返回 store.ErrTxConflict；
//   - 不消耗 ctx 超时（用 1ms 级别的短超时也能立刻拿到 ErrTxConflict 而不是 deadline）。
func TestFixTxConflictInsteadOfBlocking(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	s := d.Store()

	err := s.Tx(ctx, func(txCtx context.Context, tx store.Store) error {
		if err := tx.PutMemory(txCtx, store.Memory{
			ID: "m-conf", Scope: store.ScopeUser, Kind: store.KindSemantic,
			Content: "事务内写入", TValid: 1, TCreated: 1,
		}); err != nil {
			return err
		}
		// 故意用外层 ctx（无事务句柄）。
		if err := d.Lexical().Index(ctx, []store.Document{
			{ID: "m-conf", Scope: store.ScopeUser, Text: "事务内写入"},
		}); !errors.Is(err, store.ErrTxConflict) {
			return fmt.Errorf("外层 ctx 跨 Port 调用应返回 ErrTxConflict，得到 %v", err)
		}
		// 用事务 ctx 则落到同一事务。
		return d.Lexical().Index(txCtx, []store.Document{
			{ID: "m-conf", Scope: store.ScopeUser, Text: "事务内写入"},
		})
	})
	if err != nil {
		t.Fatalf("Tx: %v", err)
	}

	// 提交后两条路径的数据都在。
	if _, err := s.GetMemory(ctx, "m-conf"); err != nil {
		t.Errorf("memory 未提交: %v", err)
	}
	hits, err := d.Lexical().Search(ctx, store.LexicalQuery{Query: "事务", Scope: store.ScopeUser, Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != "m-conf" {
		t.Errorf("事务内 FTS 写入未提交: %v", hits)
	}

	// 事务外的并发场景不受影响：没有事务在跑时应正常执行。
	if err := d.Lexical().Index(ctx, []store.Document{
		{ID: "m-conf", Scope: store.ScopeUser, Text: "事务外重写"},
	}); err != nil {
		t.Errorf("事务外 Index 失败: %v", err)
	}
}

// TestFixTxRollbackAcrossPorts：跨 Port 写入在同一事务内回滚，三者一起消失。
func TestFixTxRollbackAcrossPorts(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	boom := errors.New("boom")

	err := d.Store().Tx(ctx, func(txCtx context.Context, tx store.Store) error {
		if err := tx.PutMemory(txCtx, store.Memory{
			ID: "m-rb", Scope: store.ScopeUser, Kind: store.KindSemantic,
			Content: "应整体回滚", TValid: 1, TCreated: 1,
		}); err != nil {
			return err
		}
		if err := d.Graph().PutNode(txCtx, store.Node{
			ID: "n-rb", Scope: store.ScopeUser, CanonicalName: "应整体回滚",
		}); err != nil {
			return err
		}
		if err := d.Vector().Upsert(txCtx, []store.Vector{
			{ID: "m-rb", Scope: store.ScopeUser, Values: []float32{1, 0}},
		}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Tx err = %v, want boom", err)
	}

	if _, err := d.Store().GetMemory(ctx, "m-rb"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("memory 未回滚: %v", err)
	}
	if _, err := d.Graph().GetNode(ctx, "n-rb"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("node 未回滚: %v", err)
	}
	hits, err := d.Vector().Search(ctx, []float32{1, 0}, store.ScopeUser, 10)
	if err != nil {
		t.Fatalf("Vector.Search: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("vector 未回滚: %v", hits)
	}
}

func hopIDsOf(hops []store.EdgeHop) []string {
	out := make([]string, len(hops))
	for i, h := range hops {
		out[i] = h.Edge.ID
	}
	return out
}

func memIDsOf(ms []store.Memory) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return out
}
