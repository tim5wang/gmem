package p1verify

import (
	"context"
	"testing"

	"github.com/tim5wang/gmem/recall"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// 攻击面 6：两条降级路径（无 embedder / embedder 失败）都不得静默
// ===========================================================================

// TestVectorLaneAbsentWithoutEmbedder：embedder 为 nil 时 vector lane 必须**缺席**，
// 且缺席要同时体现在三处：LanePlan.Executed=false、SkipReason 非空、Degraded 有文本。
//
// 只要有一处缺失，宿主就无法区分"这条结果没用向量"与"向量通道坏了"。
func TestVectorLaneAbsentWithoutEmbedder(t *testing.T) {
	d := openMemory(t)
	ctx := context.Background()
	w := recall.NewWriter(d.Ports())
	if _, err := w.PutMemory(ctx, store.Memory{
		ID: "m-x", Scope: store.ScopeUser, Kind: store.KindSemantic,
		Content: "认证方式 JWT", TValid: 1000, TCreated: 1000,
	}); err != nil {
		t.Fatalf("PutMemory: %v", err)
	}
	// 即使库里有向量，没有 embedder 也不该走 vector。
	if err := d.Vector().Upsert(ctx, []store.Vector{
		{ID: "m-x", Scope: store.ScopeUser, Values: []float32{1, 0, 0}},
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	r := recall.New(d.Ports(), recall.Config{}, nil)
	res, err := r.Recall(ctx, recall.RecallRequest{Query: "JWT 认证", Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}

	lp, ok := laneOf(res.Plan, recall.LaneVector)
	if !ok {
		t.Fatalf("Plan.Lanes 里没有 vector 行：无法解释它为何缺席：%+v", res.Plan.Lanes)
	}
	if lp.Selected != true {
		t.Fatalf("vector 应被规则表选中（Selected=true），只是不能执行：%+v", lp)
	}
	if lp.Executed {
		t.Fatalf("无 embedder 时 vector lane 执行了：%+v", lp)
	}
	if lp.SkipReason == "" {
		t.Fatalf("vector 缺席但 SkipReason 为空（静默）：%+v", lp)
	}
	if !containsSubstr(res.Degraded, "vector lane skipped") {
		t.Fatalf("Degraded=%v 未报告 vector 缺席", res.Degraded)
	}
	if !containsSubstr(res.Degraded, "embedder") {
		t.Fatalf("Degraded=%v 未说明是'无 embedder'这一原因", res.Degraded)
	}
	// 不得静默：有降级就必须有 degraded 状态。
	if res.Status != recall.StatusDegraded {
		t.Fatalf("status=%s, want degraded（有通道缺席）", res.Status)
	}
	// 词法通道必须照常工作（降级不等于不可用）。
	if len(res.Items) == 0 {
		t.Fatalf("词法通道应仍可用，却 0 结果")
	}
}

// TestVectorLaneRecordsEmbedderError：embedder 返回错误时，
// 错误必须**出现在 Degraded 里**（且带原因），整次召回不得失败。
func TestVectorLaneRecordsEmbedderError(t *testing.T) {
	d := openMemory(t)
	ctx := context.Background()
	w := recall.NewWriter(d.Ports())
	if _, err := w.PutMemory(ctx, store.Memory{
		ID: "m-x", Scope: store.ScopeUser, Kind: store.KindSemantic,
		Content: "认证方式 JWT", TValid: 1000, TCreated: 1000,
	}); err != nil {
		t.Fatalf("PutMemory: %v", err)
	}

	emb := recall.EmbedderFunc(func(context.Context, []string) ([][]float32, error) {
		return nil, errInjected
	})
	r := recall.New(d.Ports(), recall.Config{}, emb)
	res, err := r.Recall(ctx, recall.RecallRequest{Query: "JWT 认证", Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("embedder 失败不得让整次召回失败：%v", err)
	}
	if !containsSubstr(res.Degraded, "embedder error") {
		t.Fatalf("Degraded=%v 未记录 embedder 错误", res.Degraded)
	}
	// 注入的错误文本必须被带出来（否则无法定位是哪一类失败）。
	if !containsSubstr(res.Degraded, errInjected.Error()) {
		t.Fatalf("Degraded=%v 未带出 embedder 的原始错误 %q", res.Degraded, errInjected)
	}
	lp, ok := laneOf(res.Plan, recall.LaneVector)
	if !ok || lp.Executed {
		t.Fatalf("embedder 失败后 vector lane 不该执行：%+v", res.Plan.Lanes)
	}
	if lp.SkipReason == "" {
		t.Fatalf("vector 跳过但 SkipReason 为空：%+v", lp)
	}
	if res.Status != recall.StatusDegraded {
		t.Fatalf("status=%s, want degraded", res.Status)
	}
	// 剩余通道仍产出结果。
	if len(res.Items) == 0 {
		t.Fatalf("embedder 失败后词法通道应仍可用")
	}
}

// TestVectorLaneEmptyEmbeddingDegrades：embedder 返回空向量也必须降级上报，
// 不能以"没有查询向量"为由静默跳过（那是另一类静默失败）。
func TestVectorLaneEmptyEmbeddingDegrades(t *testing.T) {
	d := openMemory(t)
	ctx := context.Background()
	w := recall.NewWriter(d.Ports())
	if _, err := w.PutMemory(ctx, store.Memory{
		ID: "m-x", Scope: store.ScopeUser, Kind: store.KindSemantic,
		Content: "认证方式 JWT", TValid: 1000, TCreated: 1000,
	}); err != nil {
		t.Fatalf("PutMemory: %v", err)
	}
	emb := recall.EmbedderFunc(func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		// 每条都是空向量。
		for i := range out {
			out[i] = []float32{}
		}
		return out, nil
	})
	r := recall.New(d.Ports(), recall.Config{}, emb)
	res, err := r.Recall(ctx, recall.RecallRequest{Query: "JWT 认证", Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("空向量不得让召回失败：%v", err)
	}
	if !containsSubstr(res.Degraded, "空向量") {
		t.Fatalf("Degraded=%v 未上报 embedder 返回空向量", res.Degraded)
	}
	lp, _ := laneOf(res.Plan, recall.LaneVector)
	if lp.Executed {
		t.Fatalf("空向量时 vector lane 不该执行：%+v", lp)
	}
	if res.Status != recall.StatusDegraded {
		t.Fatalf("status=%s, want degraded", res.Status)
	}
}

// TestForcedVectorWithoutEmbedderIsVisible：调用方显式指定 vector lane 但没有 embedder 时，
// 必须出现在 Degraded 里（而不是悄悄退到 lexical）。
func TestForcedVectorWithoutEmbedderIsVisible(t *testing.T) {
	d := openMemory(t)
	ctx := context.Background()
	w := recall.NewWriter(d.Ports())
	if _, err := w.PutMemory(ctx, store.Memory{
		ID: "m-x", Scope: store.ScopeUser, Kind: store.KindSemantic,
		Content: "认证方式 JWT", TValid: 1000, TCreated: 1000,
	}); err != nil {
		t.Fatalf("PutMemory: %v", err)
	}
	r := recall.New(d.Ports(), recall.Config{}, nil)
	res, err := r.Recall(ctx, recall.RecallRequest{
		Query: "JWT", Scope: store.ScopeUser,
		Lanes: []recall.LaneName{recall.LaneVector},
	})
	if err != nil {
		t.Fatalf("Recall(forced vector): %v", err)
	}
	if !containsSubstr(res.Degraded, "vector lane skipped") {
		t.Fatalf("Degraded=%v 未报告强制指定的 vector lane 缺席", res.Degraded)
	}
	lp, ok := laneOf(res.Plan, recall.LaneVector)
	if !ok {
		t.Fatalf("调用方指定的 vector lane 未出现在 Plan.Lanes：%+v", res.Plan.Lanes)
	}
	if lp.Executed {
		t.Fatalf("无 embedder 却执行了 vector：%+v", lp)
	}
}

// ===========================================================================
// 攻击面 7：scope / kind 隔离
// ===========================================================================

// TestScopeIsolation：跨 scope 的记忆不得被召回，且不得以任何形式泄漏内容。
//
// 除了"结果里没有"，还断言：
//   - 目标 scope 的答案仍在（隔离不等于全空）；
//   - Plan.Dropped 里若记录了跨 scope 候选，其 Reason 不得带出被隔离记忆的内容。
func TestScopeIsolation(t *testing.T) {
	d := openMemory(t)
	ctx := context.Background()
	w := recall.NewWriter(d.Ports())
	for _, m := range []struct {
		id      string
		scope   store.Scope
		content string
	}{
		{"m-user", store.ScopeUser, "认证方式 JWT 用户级机密"},
		{"m-agent", store.ScopeAgent, "认证方式 JWT 代理级机密"},
		{"m-project", store.ScopeProject, "认证方式 JWT 项目级机密"},
	} {
		if _, err := w.PutMemory(ctx, store.Memory{
			ID: m.id, Scope: m.scope, Kind: store.KindSemantic,
			Content: m.content, TValid: 1000, TCreated: 1000,
		}); err != nil {
			t.Fatalf("PutMemory(%s): %v", m.id, err)
		}
	}

	r := recall.New(d.Ports(), recall.Config{}, nil)
	res, err := r.Recall(ctx, recall.RecallRequest{Query: "认证方式 JWT", Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	ids := itemIDs(res.Items)
	if !containsID(ids, "m-user") {
		t.Fatalf("目标 scope 的 m-user 未召回：%v", ids)
	}
	for _, banned := range []string{"m-agent", "m-project"} {
		if containsID(ids, banned) {
			t.Fatalf("跨 scope 泄漏：%s 出现在 scope=user 的结果里：%v", banned, ids)
		}
	}
	// 任何返回条目的 Scope 都必须等于请求 scope。
	for _, it := range res.Items {
		if it.Memory.Scope != store.ScopeUser {
			t.Fatalf("%s 的 scope=%s，泄漏进 scope=user 的结果", it.Memory.ID, it.Memory.Scope)
		}
	}
	// 另外两侧也必须隔离（反向）。
	for _, tc := range []struct {
		scope store.Scope
		want  string
	}{
		{store.ScopeAgent, "m-agent"},
		{store.ScopeProject, "m-project"},
	} {
		got, err := r.Recall(ctx, recall.RecallRequest{Query: "认证方式 JWT", Scope: tc.scope})
		if err != nil {
			t.Fatalf("Recall(%s): %v", tc.scope, err)
		}
		gotIDs := itemIDs(got.Items)
		if !containsID(gotIDs, tc.want) {
			t.Fatalf("scope=%s 未召回 %s：%v", tc.scope, tc.want, gotIDs)
		}
		for _, id := range gotIDs {
			if id != tc.want {
				t.Fatalf("scope=%s 的结果里混入了 %s：%v", tc.scope, id, gotIDs)
			}
		}
	}
}

// TestKindFilterVisibleInDropped：kind 被过滤时必须在 Plan.Dropped 里可见，
// 且 Reason 必须点明是 kind 过滤（不能只给一句"被拦截"）。
func TestKindFilterVisibleInDropped(t *testing.T) {
	d := openMemory(t)
	ctx := context.Background()
	w := recall.NewWriter(d.Ports())
	for _, m := range []struct {
		id   string
		kind store.MemoryKind
	}{
		{"m-dec", store.KindDecision},
		{"m-epi", store.KindEpisodic},
	} {
		if _, err := w.PutMemory(ctx, store.Memory{
			ID: m.id, Scope: store.ScopeUser, Kind: m.kind,
			Content: "认证用 JWT", TValid: 1000, TCreated: 1000,
		}); err != nil {
			t.Fatalf("PutMemory(%s): %v", m.id, err)
		}
	}

	r := recall.New(d.Ports(), recall.Config{}, nil)
	res, err := r.Recall(ctx, recall.RecallRequest{
		Query: "JWT 认证", Scope: store.ScopeUser,
		Kinds: []store.MemoryKind{store.KindEpisodic},
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	ids := itemIDs(res.Items)
	if containsID(ids, "m-dec") {
		t.Fatalf("kind 过滤未生效：decision 记忆出现在只要 episodic 的结果里：%v", ids)
	}
	if !containsID(ids, "m-epi") {
		t.Fatalf("目标 kind 的 m-epi 未召回：%v", ids)
	}
	var decDropped bool
	for _, dr := range res.Plan.Dropped {
		if dr.ID != "m-dec" {
			continue
		}
		decDropped = true
		if dr.Reason == "" {
			t.Fatalf("Dropped 缺 Reason：%+v", dr)
		}
		if !containsSubstr([]string{dr.Reason}, "kind") {
			t.Fatalf("Dropped 的 Reason 未点明 kind 过滤：%q", dr.Reason)
		}
	}
	if !decDropped {
		t.Fatalf("m-dec 被 kind 过滤拦截，但 Plan.Dropped 未记录（静默丢弃）：%+v", res.Plan.Dropped)
	}
}
