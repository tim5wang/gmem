package sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/tim5wang/gmem/store"
)

// seedEpisode 写一条 episode 作为溯源目标。
func seedEpisode(t *testing.T, d *DB, id string) store.Episode {
	t.Helper()
	e := store.Episode{
		ID: id, Scope: store.ScopeUser, Namespace: "chat", Role: "user",
		Content: "我把认证服务从 session 换成 JWT 了", ContentHash: "h-" + id, At: 1700000000,
	}
	if err := d.Store().PutEpisode(context.Background(), e); err != nil {
		t.Fatalf("PutEpisode: %v", err)
	}
	return e
}

func putMemory(t *testing.T, d *DB, m store.Memory) {
	t.Helper()
	if err := d.Store().PutMemory(context.Background(), m); err != nil {
		t.Fatalf("PutMemory(%s): %v", m.ID, err)
	}
}

// TestMemoryBitemporal：双时态——矛盾只置 t_invalid，历史 as_of 仍可查回。
func TestMemoryBitemporal(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	ep := seedEpisode(t, d, "ep1")

	const (
		tValid   = int64(1000) // 世界时间：事实从 1000 开始成立
		tCreated = int64(1100) // 系统时间：Gmem 在 1100 写入
		tInvalid = int64(2000) // 世界时间：事实在 2000 被推翻
	)
	putMemory(t, d, store.Memory{
		ID: "m1", Scope: store.ScopeUser, Kind: store.KindDecision,
		Content: "认证用 JWT", TValid: tValid, TCreated: tCreated,
		SourceEpisode: ep.ID, Importance: 0.9,
	})

	// 失效前：默认查询能看到。
	got, err := d.Store().ListMemories(ctx, store.MemoryQuery{Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("ListMemories: %v", err)
	}
	if len(got) != 1 || got[0].ID != "m1" {
		t.Fatalf("before invalidate: %+v", got)
	}

	if err := d.Store().InvalidateMemory(ctx, "m1", tInvalid); err != nil {
		t.Fatalf("InvalidateMemory: %v", err)
	}

	// 默认查询：当前有效事实集合里已经没有它。
	got, err = d.Store().ListMemories(ctx, store.MemoryQuery{Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("ListMemories: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("after invalidate default query returned %+v, want empty", got)
	}

	// 物理删除的反面：记录仍在，t_invalid 已置。
	m, err := d.Store().GetMemory(ctx, "m1")
	if err != nil {
		t.Fatalf("GetMemory: %v", err)
	}
	if m.TInvalid != tInvalid {
		t.Fatalf("t_invalid = %d, want %d", m.TInvalid, tInvalid)
	}

	// as_of 回溯：1500 时刻这条事实仍成立（世界时间与系统时间都满足）。
	asOf := int64(1500)
	got, err = d.Store().ListMemories(ctx, store.MemoryQuery{AsOf: &asOf})
	if err != nil {
		t.Fatalf("ListMemories as_of: %v", err)
	}
	if len(got) != 1 || got[0].ID != "m1" {
		t.Fatalf("as_of 1500 = %+v, want m1", got)
	}

	// as_of 2500：既过了 t_invalid，事实已不成立。
	later := int64(2500)
	got, err = d.Store().ListMemories(ctx, store.MemoryQuery{AsOf: &later})
	if err != nil {
		t.Fatalf("ListMemories as_of later: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("as_of 2500 = %+v, want empty", got)
	}

	// as_of 900：早于 t_created，系统当时还没有这条记忆。
	earlier := int64(900)
	got, err = d.Store().ListMemories(ctx, store.MemoryQuery{AsOf: &earlier})
	if err != nil {
		t.Fatalf("ListMemories as_of earlier: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("as_of 900 = %+v, want empty", got)
	}

	// IncludeInvalid 可以看到历史（审计路径）。
	got, err = d.Store().ListMemories(ctx, store.MemoryQuery{IncludeInvalid: true})
	if err != nil {
		t.Fatalf("ListMemories IncludeInvalid: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("IncludeInvalid = %+v, want 1", got)
	}

	// 重复失效是幂等的，但不存在 ID 要报 ErrNotFound。
	if err := d.Store().InvalidateMemory(ctx, "m1", tInvalid+1); err != nil {
		t.Fatalf("re-invalidate should be idempotent: %v", err)
	}
	if err := d.Store().InvalidateMemory(ctx, "nope", 1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("invalidate missing = %v, want ErrNotFound", err)
	}
}

// TestMemoryQueryFilters：kind 多选、concept 过滤、limit。
func TestMemoryQueryFilters(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	putMemory(t, d, store.Memory{ID: "m1", Scope: store.ScopeUser, Kind: store.KindSemantic, Content: "认证服务"})
	putMemory(t, d, store.Memory{ID: "m2", Scope: store.ScopeUser, Kind: store.KindDecision, Content: "改用 JWT"})
	putMemory(t, d, store.Memory{ID: "m3", Scope: store.ScopeAgent, Kind: store.KindSemantic, Content: "另一个 scope"})

	got, err := d.Store().ListMemories(ctx, store.MemoryQuery{
		Scope: store.ScopeUser, Kinds: []store.MemoryKind{store.KindSemantic},
	})
	if err != nil {
		t.Fatalf("ListMemories: %v", err)
	}
	if len(got) != 1 || got[0].ID != "m1" {
		t.Fatalf("kind filter = %+v, want m1", got)
	}

	got, err = d.Store().ListMemories(ctx, store.MemoryQuery{Limit: 2})
	if err != nil {
		t.Fatalf("ListMemories limit: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("limit = %d, want 2", len(got))
	}
}

// TestValidateRejectsBadInput：写入前的最小校验生效（不依赖数据库约束）。
func TestValidateRejectsBadInput(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	if err := d.Store().PutMemory(ctx, store.Memory{ID: "x", Scope: "bogus", Kind: store.KindSemantic, Content: "c"}); err == nil {
		t.Fatal("invalid scope accepted")
	}
	if err := d.Store().PutMemory(ctx, store.Memory{ID: "x", Scope: store.ScopeUser, Kind: "bogus", Content: "c"}); err == nil {
		t.Fatal("invalid kind accepted")
	}
	if err := d.Store().PutConcept(ctx, store.Concept{ID: "c", Name: "n", Status: "bogus"}); err == nil {
		t.Fatal("invalid concept status accepted")
	}
	if _, err := d.Store().GetMemory(ctx, "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetMemory missing = %v, want ErrNotFound", err)
	}
}

// TestTxRollback：事务内出错整体回滚，不漏写。
func TestTxRollback(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	s := d.Store()

	boom := errors.New("boom")
	err := s.Tx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.PutMemory(ctx, store.Memory{ID: "tx1", Scope: store.ScopeUser, Kind: store.KindSemantic, Content: "a"}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Tx err = %v, want boom", err)
	}
	if _, err := s.GetMemory(ctx, "tx1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("rollback failed, tx1 still visible: %v", err)
	}

	// 正常提交。
	if err := s.Tx(ctx, func(ctx context.Context, tx store.Store) error {
		return tx.PutMemory(ctx, store.Memory{ID: "tx2", Scope: store.ScopeUser, Kind: store.KindSemantic, Content: "b"})
	}); err != nil {
		t.Fatalf("Tx commit: %v", err)
	}
	if _, err := s.GetMemory(ctx, "tx2"); err != nil {
		t.Fatalf("committed row missing: %v", err)
	}
}

// TestConceptsAndAliases：concepts 往返 + aliases 取最高置信。
func TestConceptsAndAliases(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	s := d.Store()

	c := store.Concept{ID: "c1", Name: "认证", Status: store.ConceptDraft, Definition: "验证身份的过程"}
	if err := s.PutConcept(ctx, c); err != nil {
		t.Fatalf("PutConcept: %v", err)
	}
	c.Status = store.ConceptCertified
	if err := s.PutConcept(ctx, c); err != nil {
		t.Fatalf("PutConcept update: %v", err)
	}
	got, err := s.GetConcept(ctx, "c1")
	if err != nil {
		t.Fatalf("GetConcept: %v", err)
	}
	if got.Status != store.ConceptCertified || got.Definition != c.Definition {
		t.Fatalf("concept = %+v", got)
	}

	node := store.Node{ID: "n1", Scope: store.ScopeUser, CanonicalName: "认证服务"}
	if err := d.Graph().PutNode(ctx, node); err != nil {
		t.Fatalf("PutNode: %v", err)
	}
	if err := s.PutAlias(ctx, store.Alias{Surface: "登录鉴权", NodeID: "n1", Source: "dict", Confidence: 0.5}); err != nil {
		t.Fatalf("PutAlias: %v", err)
	}
	if err := s.PutAlias(ctx, store.Alias{Surface: "登录鉴权", NodeID: "n1", Source: "dict", Confidence: 0.9}); err != nil {
		t.Fatalf("PutAlias upsert: %v", err)
	}
	a, err := s.GetAlias(ctx, "登录鉴权")
	if err != nil {
		t.Fatalf("GetAlias: %v", err)
	}
	if a.Confidence != 0.9 || a.NodeID != "n1" {
		t.Fatalf("alias = %+v", a)
	}
	if _, err := s.GetAlias(ctx, "不存在"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetAlias missing = %v", err)
	}
}

// TestEpisodeRoundTrip：episodes 可溯源读取。
func TestEpisodeRoundTrip(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	ep := seedEpisode(t, d, "ep1")
	got, err := d.Store().GetEpisode(ctx, ep.ID)
	if err != nil {
		t.Fatalf("GetEpisode: %v", err)
	}
	if got.Content != ep.Content || got.ContentHash != ep.ContentHash || got.Scope != ep.Scope {
		t.Fatalf("episode = %+v, want %+v", got, ep)
	}
}
