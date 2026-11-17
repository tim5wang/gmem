package vtest

import (
	"context"
	"errors"
	"testing"

	"github.com/tim5wang/gmem/store"
)

// ---------------------------------------------------------------------------
// 攻击面 2：双时态正确性
//
// §2 第 2 条原文："矛盾只置 t_invalid，永不物理删除"。
// 因此这里断言的不是"查得到最新的"，而是三件更容易被实现者漏掉的事：
//   - 旧记录**仍然物理存在**（不是被 DELETE / 被 content 覆盖）；
//   - as_of 能回到旧状态（世界时间 ∩ 系统时间）；
//   - 边界是**半开区间**：t_valid <= t < t_invalid，t_created <= t < t_expired。
// ---------------------------------------------------------------------------

const (
	contentOld = "用户的代码缩进偏好是 tabs"
	contentNew = "用户的代码缩进偏好是 spaces"

	memOld = "m-indent-tabs"
	memNew = "m-indent-spaces"

	// 旧事实：世界时间 100 成立，500 失效；系统时间 100 写入。
	oldTValid   = int64(100)
	oldTInvalid = int64(500)
	oldTCreated = int64(100)

	// 新事实：世界时间 500 成立；系统时间 2000 才写入。
	// t_created 远晚于 t_valid 是真实情况（事后补录），也是区分两条时间轴的关键。
	newTValid   = int64(500)
	newTCreated = int64(2000)
)

// TestBitemporalContradictionKeepsRow 是"永不物理删除"的核心断言。
func TestBitemporalContradictionKeepsRow(t *testing.T) {
	d, raw := openFile(t)
	ctx := context.Background()
	s := d.Store()

	if err := s.PutMemory(ctx, store.Memory{
		ID: memOld, Scope: store.ScopeUser, Kind: store.KindSemantic,
		Content: contentOld, TValid: oldTValid, TCreated: oldTCreated,
	}); err != nil {
		t.Fatalf("PutMemory(old): %v", err)
	}
	if err := s.PutMemory(ctx, store.Memory{
		ID: memNew, Scope: store.ScopeUser, Kind: store.KindSemantic,
		Content: contentNew, TValid: newTValid, TCreated: newTCreated,
	}); err != nil {
		t.Fatalf("PutMemory(new): %v", err)
	}
	if err := s.InvalidateMemory(ctx, memOld, oldTInvalid); err != nil {
		t.Fatalf("InvalidateMemory(old): %v", err)
	}

	// 1) 物理行仍在，t_invalid 被置上，content 未被改写。
	got, err := s.GetMemory(ctx, memOld)
	if err != nil {
		t.Fatalf("GetMemory(old) 失败，说明旧记录被物理删除了: %v", err)
	}
	if got.TInvalid != oldTInvalid {
		t.Errorf("旧记录 TInvalid = %d, want %d", got.TInvalid, oldTInvalid)
	}
	if got.Content != contentOld {
		t.Errorf("旧记录 content 被改写为 %q，应保持 %q", got.Content, contentOld)
	}
	if n := countInt(t, raw, `SELECT COUNT(1) FROM memories`); n != 2 {
		t.Errorf("memories 行数 = %d, want 2（矛盾不得物理删除）", n)
	}

	// 2) 默认查询只给当前有效事实。
	cur, err := s.ListMemories(ctx, store.MemoryQuery{})
	if err != nil {
		t.Fatalf("ListMemories(default): %v", err)
	}
	if ids := memIDs(cur); !sameStrings(ids, []string{memNew}) {
		t.Errorf("默认查询 = %v, want [%s]", ids, memNew)
	}

	// 3) IncludeInvalid 能同时看到两条（history/审计路径）。
	all, err := s.ListMemories(ctx, store.MemoryQuery{IncludeInvalid: true})
	if err != nil {
		t.Fatalf("ListMemories(IncludeInvalid): %v", err)
	}
	if ids := memIDs(all); len(ids) != 2 {
		t.Errorf("IncludeInvalid 查询 = %v, want 两条", ids)
	}

	// 4) 重复失效幂等：不报错，且不得把 t_invalid 往后推（否则 as_of 历史会被篡改）。
	if err := s.InvalidateMemory(ctx, memOld, oldTInvalid+999); err != nil {
		t.Errorf("重复 InvalidateMemory 应为幂等成功，得到 %v", err)
	}
	got2, err := s.GetMemory(ctx, memOld)
	if err != nil {
		t.Fatalf("GetMemory(old) 二次: %v", err)
	}
	if got2.TInvalid != oldTInvalid {
		t.Errorf("重复失效改写了 TInvalid = %d, want 保持 %d", got2.TInvalid, oldTInvalid)
	}

	// 5) 对不存在的 ID 失效必须报 ErrNotFound（不能静默成功）。
	if err := s.InvalidateMemory(ctx, "no-such-memory", oldTInvalid); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("失效不存在的 ID: err = %v, want store.ErrNotFound", err)
	}
}

// TestBitemporalAsOfRewinds 验证 as_of 能回到旧状态，且两条时间轴必须同时成立。
//
// 时间轴：
//
//	old: t_valid=100  t_invalid=500   t_created=100
//	new: t_valid=500  t_invalid=∞     t_created=2000
//
// 关键的两个"看似应当可见、实则不可见"的时刻：
//   - as_of=500：新事实在世界时间已成立，但系统时间还没写入 → 不可见（历史不可预知）。
//   - as_of=1999：同上，写库前一秒。
func TestBitemporalAsOfRewinds(t *testing.T) {
	d := openMemory(t)
	ctx := context.Background()
	s := d.Store()

	if err := s.PutMemory(ctx, store.Memory{
		ID: memOld, Scope: store.ScopeUser, Kind: store.KindSemantic,
		Content: contentOld, TValid: oldTValid, TCreated: oldTCreated,
	}); err != nil {
		t.Fatalf("PutMemory(old): %v", err)
	}
	if err := s.PutMemory(ctx, store.Memory{
		ID: memNew, Scope: store.ScopeUser, Kind: store.KindSemantic,
		Content: contentNew, TValid: newTValid, TCreated: newTCreated,
	}); err != nil {
		t.Fatalf("PutMemory(new): %v", err)
	}
	if err := s.InvalidateMemory(ctx, memOld, oldTInvalid); err != nil {
		t.Fatalf("InvalidateMemory(old): %v", err)
	}

	cases := []struct {
		asOf int64
		want []string
		why  string
	}{
		{50, nil, "早于旧事实生效（t_valid=100 未到）"},
		{99, nil, "旧事实生效前一刻"},
		{100, []string{memOld}, "旧事实生效瞬间（t_valid <= t 含端点）"},
		{300, []string{memOld}, "旧事实区间内：世界时间与系统时间均成立"},
		{499, []string{memOld}, "失效前一刻"},
		{500, nil, "旧事实在 500 失效（t < t_invalid 不含端点）；新事实 t_created=2000 尚未写入"},
		{1999, nil, "新事实世界时间已成立但尚未入库（系统时间闸门）"},
		{2000, []string{memNew}, "新事实入库瞬间（t_created <= t 含端点）"},
		{2500, []string{memNew}, "现状：旧失效、新生效"},
	}

	for _, c := range cases {
		asOf := c.asOf
		got, err := s.ListMemories(ctx, store.MemoryQuery{AsOf: &asOf})
		if err != nil {
			t.Fatalf("as_of=%d: ListMemories: %v", c.asOf, err)
		}
		if ids := memIDs(got); !sameStrings(ids, c.want) {
			t.Errorf("as_of=%d: got %v, want %v — %s", c.asOf, ids, c.want, c.why)
		}
	}

	// 回溯到旧状态时，内容必须是旧的（证明不是"新内容覆盖旧行"）。
	asOf := int64(300)
	got, err := s.ListMemories(ctx, store.MemoryQuery{AsOf: &asOf})
	if err != nil {
		t.Fatalf("ListMemories(as_of=300): %v", err)
	}
	if len(got) != 1 || got[0].Content != contentOld {
		t.Errorf("as_of=300 内容 = %+v, want 只有 %q", got, contentOld)
	}
}

// TestBitemporalSystemTimeExpiry 隔离验证系统时间轴（t_created / t_expired）。
//
// t_expired 表达的是"Gmem 不再使用这条事实"，与"事实在现实中是否成立"无关。
// 若实现把两条轴混为一谈，这里会立刻发现。
func TestBitemporalSystemTimeExpiry(t *testing.T) {
	d := openMemory(t)
	ctx := context.Background()
	s := d.Store()

	const expired = "m-expired"
	if err := s.PutMemory(ctx, store.Memory{
		ID: expired, Scope: store.ScopeAgent, Kind: store.KindProcedural,
		Content: "发布流程需要两台审批",
		TValid:  100, TCreated: 1000, TExpired: 3000,
	}); err != nil {
		t.Fatalf("PutMemory: %v", err)
	}

	cur, err := s.ListMemories(ctx, store.MemoryQuery{})
	if err != nil {
		t.Fatalf("ListMemories(default): %v", err)
	}
	if len(cur) != 0 {
		t.Errorf("默认查询应排除 t_expired 记录，得到 %v", memIDs(cur))
	}

	all, err := s.ListMemories(ctx, store.MemoryQuery{IncludeExpired: true})
	if err != nil {
		t.Fatalf("ListMemories(IncludeExpired): %v", err)
	}
	if ids := memIDs(all); !sameStrings(ids, []string{expired}) {
		t.Errorf("IncludeExpired 应返回 [%s]，得到 %v", expired, ids)
	}

	asOf := int64(2000)
	got, err := s.ListMemories(ctx, store.MemoryQuery{AsOf: &asOf})
	if err != nil {
		t.Fatalf("ListMemories(as_of=2000): %v", err)
	}
	if ids := memIDs(got); !sameStrings(ids, []string{expired}) {
		t.Errorf("as_of=2000 应看到未过期记录，得到 %v", ids)
	}

	// as_of 恰好等于 t_expired：半开区间，不可见。
	asOf = 3000
	got, err = s.ListMemories(ctx, store.MemoryQuery{AsOf: &asOf})
	if err != nil {
		t.Fatalf("ListMemories(as_of=3000): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("as_of=3000（t_expired 时刻）应不可见，得到 %v", memIDs(got))
	}

	// 早于 t_created：世界时间成立但系统尚未写入，不可见。
	asOf = 500
	got, err = s.ListMemories(ctx, store.MemoryQuery{AsOf: &asOf})
	if err != nil {
		t.Fatalf("ListMemories(as_of=500): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("as_of=500 早于 t_created=1000，应不可见，得到 %v", memIDs(got))
	}
}

// TestBitemporalFiltersCombine 验证过滤条件与双时态条件是可组合的（AND，不是互相覆盖）。
func TestBitemporalFiltersCombine(t *testing.T) {
	d := openMemory(t)
	ctx := context.Background()
	s := d.Store()

	rows := []store.Memory{
		{ID: "u-sema", Scope: store.ScopeUser, Kind: store.KindSemantic, Content: "用户偏好缩进 tabs", TValid: 100, TCreated: 100},
		{ID: "u-epis", Scope: store.ScopeUser, Kind: store.KindEpisodic, Content: "昨天调试了认证失败", TValid: 100, TCreated: 100},
		{ID: "a-sema", Scope: store.ScopeAgent, Kind: store.KindSemantic, Content: "Agent 会先查认证日志", TValid: 100, TCreated: 100},
	}
	for _, m := range rows {
		if err := s.PutMemory(ctx, m); err != nil {
			t.Fatalf("PutMemory(%s): %v", m.ID, err)
		}
	}
	if err := s.InvalidateMemory(ctx, "a-sema", 900); err != nil {
		t.Fatalf("InvalidateMemory(a-sema): %v", err)
	}

	asOf := int64(500)
	got, err := s.ListMemories(ctx, store.MemoryQuery{
		Scope: store.ScopeUser,
		Kinds: []store.MemoryKind{store.KindSemantic},
		AsOf:  &asOf,
	})
	if err != nil {
		t.Fatalf("ListMemories: %v", err)
	}
	if ids := memIDs(got); !sameStrings(ids, []string{"u-sema"}) {
		t.Errorf("组合过滤（scope=user + kind=semantic + as_of=500）= %v, want [u-sema]", ids)
	}

	// 失效的那条在 as_of 回溯里应可见（它当时有效），但默认查询不可见。
	asOf = 500
	agentRows, err := s.ListMemories(ctx, store.MemoryQuery{Scope: store.ScopeAgent, AsOf: &asOf})
	if err != nil {
		t.Fatalf("ListMemories(agent, as_of=500): %v", err)
	}
	if ids := memIDs(agentRows); !sameStrings(ids, []string{"a-sema"}) {
		t.Errorf("a-sema 在 as_of=500 应可见，得到 %v", ids)
	}
	agentNow, err := s.ListMemories(ctx, store.MemoryQuery{Scope: store.ScopeAgent})
	if err != nil {
		t.Fatalf("ListMemories(agent, now): %v", err)
	}
	if len(agentNow) != 0 {
		t.Errorf("a-sema 已失效，默认查询应为空，得到 %v", memIDs(agentNow))
	}

	// Limit 必须作用在过滤之后。
	limited, err := s.ListMemories(ctx, store.MemoryQuery{Limit: 1})
	if err != nil {
		t.Fatalf("ListMemories(Limit=1): %v", err)
	}
	if len(limited) != 1 {
		t.Errorf("Limit=1 应返回 1 条，得到 %d 条", len(limited))
	}
}

func memIDs(ms []store.Memory) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return out
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
