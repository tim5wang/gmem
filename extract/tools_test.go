package extract

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// tool-calling 的三个只读工具（docs/TD.md §3.2）
//
// 三个必须固定的不变量：
//  1. **无任何写工具**——工具面只有三个名字（结构性排除"模型改数据"）；
//  2. 每个工具都有行数上限，超上限返回 truncated 标记；
//  3. neighbors 的 hops **硬上限 1**，模型要更多也只能拿到 1 跳。
// ===========================================================================

func TestToolSpecsAreReadOnlyOnly(t *testing.T) {
	specs := ToolSpecs()
	if len(specs) != 3 {
		t.Fatalf("工具面必须恰好三个只读工具，实际 %d：%v", len(specs), specNames(specs))
	}
	want := map[string]bool{ToolLookupEntity: true, ToolNeighbors: true, ToolSearchMemories: true}
	for _, s := range specs {
		if !want[s.Name] {
			t.Errorf("出现了非预期工具 %q（工具面必须固定为三个只读查询）", s.Name)
		}
		if s.Description == "" || len(s.Parameters) == 0 {
			t.Errorf("工具 %q 必须带描述与参数 schema", s.Name)
		}
		// schema 必须能解析成 JSON 对象（宿主会直接转给自家模型）。
		var m map[string]any
		if err := json.Unmarshal(s.Parameters, &m); err != nil {
			t.Errorf("工具 %q 的参数 schema 不是合法 JSON: %v", s.Name, err)
		}
	}
	// 任何写语义的工具名都不得出现。
	for _, s := range specs {
		for _, banned := range []string{"put", "write", "create", "delete", "merge", "update", "set"} {
			if strings.Contains(strings.ToLower(s.Name), banned) {
				t.Errorf("工具 %q 名字带写语义（%q）：工具面必须全部只读", s.Name, banned)
			}
		}
		// neighbors 的 schema 必须把 hops 上限写成 1。
		if s.Name == ToolNeighbors && !strings.Contains(string(s.Parameters), `"maximum": 1`) {
			t.Errorf("neighbors 的 schema 必须声明 hops 硬上限 1，实际 %s", s.Parameters)
		}
	}
}

func specNames(specs []ToolSpec) []string {
	out := make([]string, 0, len(specs))
	for _, s := range specs {
		out = append(out, s.Name)
	}
	return out
}

func TestToolRunnerRejectsUnknownTool(t *testing.T) {
	d := openMemoryDB(t)
	tr := newToolRunner(d.Ports(), Config{}, store.ScopeUser)
	if _, err := tr.run(context.Background(),
		ToolCallJSON("write_memory", `{"content":"x"}`)); err == nil {
		t.Fatal("未知工具名必须报错（静默空结果会让模型以为'确实没有'）")
	}
}

// TestLookupEntityLimit 验证 lookup_entity 的上限 10 条与 truncated 标记。
func TestLookupEntityLimit(t *testing.T) {
	d := openMemoryDB(t)
	seedOntology(t, d)
	ctx := context.Background()
	// 再补 14 个都含 "vim" 的实体 → 总候选 > 10。
	for i := 0; i < 14; i++ {
		id := "n-vim-" + itoa(i)
		if err := d.Graph().PutNode(ctx, store.Node{
			ID: id, Scope: store.ScopeUser, CanonicalName: "vim 插件 " + itoa(i),
		}); err != nil {
			t.Fatalf("PutNode: %v", err)
		}
		if err := d.Store().PutAlias(ctx, store.Alias{
			Surface: "vim 插件 " + itoa(i), NodeID: id, Source: "seed", Confidence: 0.5,
		}); err != nil {
			t.Fatalf("PutAlias: %v", err)
		}
	}
	tr := newToolRunner(d.Ports(), Config{}, store.ScopeUser)
	out, err := tr.run(ctx, ToolCallJSON(ToolLookupEntity, `{"name":"vim"}`))
	if err != nil {
		t.Fatalf("lookup_entity: %v", err)
	}
	if len(out.Items) > 10 {
		t.Errorf("lookup_entity 最多返回 10 条，实际 %d", len(out.Items))
	}
	if !out.Truncated {
		t.Errorf("超过上限必须带 truncated 标记（total=%d, returned=%d）",
			out.Total, len(out.Items))
	}
}

// TestNeighborsHopsHardCap 验证 hops 硬上限 1：要 3 跳也只拿到 1 跳。
func TestNeighborsHopsHardCap(t *testing.T) {
	d := openMemoryDB(t)
	ctx := context.Background()
	// 链 A → B → C → D：若允许 3 跳就能看到 D。
	for _, id := range []string{"n-a", "n-b", "n-c", "n-d"} {
		if err := d.Graph().PutNode(ctx, store.Node{
			ID: id, Scope: store.ScopeUser, CanonicalName: id,
		}); err != nil {
			t.Fatalf("PutNode: %v", err)
		}
	}
	chain := [][2]string{{"n-a", "n-b"}, {"n-b", "n-c"}, {"n-c", "n-d"}}
	for _, pair := range chain {
		if err := d.Graph().PutEdge(ctx, store.Edge{
			ID: "e-" + pair[0] + "-" + pair[1], FromID: pair[0], ToID: pair[1],
			Relation: store.RelationUses, Weight: 1, TCreated: 1,
		}); err != nil {
			t.Fatalf("PutEdge: %v", err)
		}
	}
	tr := newToolRunner(d.Ports(), Config{}, store.ScopeUser)
	out, err := tr.run(ctx, ToolCallJSON(ToolNeighbors, `{"node_id":"n-a","hops":3}`))
	if err != nil {
		t.Fatalf("neighbors: %v", err)
	}
	for _, item := range out.Items {
		if depth, ok := item["depth"].(int); ok && depth > 1 {
			t.Errorf("hops 硬上限 1 被突破：出现 depth=%d 的边 %v", depth, item)
		}
	}
	if len(out.Items) != 1 {
		t.Errorf("从 n-a 出发只应有 1 条 1 跳边，实际 %d 条：%v", len(out.Items), out.Items)
	}
	if !out.Truncated {
		t.Error("hops 被裁剪时必须带 truncated 标记（让模型知道被限制过）")
	}
	if HardMaxHops != 1 {
		t.Errorf("HardMaxHops 必须是常量 1（不得随配置放大），实际 %d", HardMaxHops)
	}
}

// TestNeighborsLimit30 验证 neighbors 的行数上限 30。
func TestNeighborsLimit30(t *testing.T) {
	d := openMemoryDB(t)
	ctx := context.Background()
	if err := d.Graph().PutNode(ctx, store.Node{
		ID: "n-hub", Scope: store.ScopeUser, CanonicalName: "hub",
	}); err != nil {
		t.Fatalf("PutNode: %v", err)
	}
	for i := 0; i < 40; i++ {
		id := "n-spoke-" + itoa(i)
		if err := d.Graph().PutNode(ctx, store.Node{
			ID: id, Scope: store.ScopeUser, CanonicalName: id,
		}); err != nil {
			t.Fatalf("PutNode: %v", err)
		}
		if err := d.Graph().PutEdge(ctx, store.Edge{
			ID: "e-hub-" + itoa(i), FromID: "n-hub", ToID: id,
			Relation: store.RelationUses, Weight: 1, TCreated: 1,
		}); err != nil {
			t.Fatalf("PutEdge: %v", err)
		}
	}
	tr := newToolRunner(d.Ports(), Config{}, store.ScopeUser)
	out, err := tr.run(ctx, ToolCallJSON(ToolNeighbors, `{"node_id":"n-hub"}`))
	if err != nil {
		t.Fatalf("neighbors: %v", err)
	}
	if len(out.Items) > 30 {
		t.Errorf("neighbors 最多 30 条，实际 %d", len(out.Items))
	}
	if !out.Truncated {
		t.Errorf("40 条边超过上限时必须 truncated（total=%d returned=%d）",
			out.Total, len(out.Items))
	}
}

// TestSearchMemoriesLimit5 验证 search_memories 的 k ≤ 5。
func TestSearchMemoriesLimit5(t *testing.T) {
	d := openMemoryDB(t)
	p := NewPipeline(d.Ports(), Config{}, PipelineOptions{})
	ctx := context.Background()
	// 写 8 条都含"缓存"的记忆。
	for i := 0; i < 8; i++ {
		if _, err := p.Run(ctx, RawEpisode{
			ID: "e-s-" + itoa(i), Scope: store.ScopeUser, At: int64(1000 + i),
			Content: "缓存层设计说明第 " + itoa(i) + " 条，内容足够长以便通过长度门。",
		}); err != nil {
			t.Fatalf("Run: %v", err)
		}
	}
	tr := newToolRunner(d.Ports(), Config{}, store.ScopeUser)
	// 模型要 k=20，必须被夹到 5。
	out, err := tr.run(ctx, ToolCallJSON(ToolSearchMemories, `{"query":"缓存","k":20}`))
	if err != nil {
		t.Fatalf("search_memories: %v", err)
	}
	if len(out.Items) > 5 {
		t.Errorf("search_memories 最多 5 条（k≤5 是契约），实际 %d", len(out.Items))
	}
	if len(out.Items) == 0 {
		t.Fatal("应当能检索到含'缓存'的记忆")
	}
	if !out.Truncated {
		t.Errorf("k 被夹取时必须带 truncated 标记（total=%d）", out.Total)
	}
}

// TestToolScopeIsolation 验证工具严格按 scope 过滤（跨 scope 泄漏是隔离缺陷）。
func TestToolScopeIsolation(t *testing.T) {
	d := openMemoryDB(t)
	ctx := context.Background()
	if err := d.Graph().PutNode(ctx, store.Node{
		ID: "n-user-only", Scope: store.ScopeUser, CanonicalName: "用户私有实体",
	}); err != nil {
		t.Fatalf("PutNode: %v", err)
	}
	if err := d.Store().PutAlias(ctx, store.Alias{
		Surface: "用户私有实体", NodeID: "n-user-only", Source: "seed", Confidence: 0.9,
	}); err != nil {
		t.Fatalf("PutAlias: %v", err)
	}

	// agent scope 的工具调用不得看见 user scope 的实体。
	tr := newToolRunner(d.Ports(), Config{}, store.ScopeAgent)
	out, err := tr.run(ctx, ToolCallJSON(ToolLookupEntity, `{"name":"用户私有实体"}`))
	if err != nil {
		t.Fatalf("lookup_entity: %v", err)
	}
	if len(out.Items) != 0 {
		t.Errorf("跨 scope 泄漏：agent 视角看到了 user scope 的 %v", out.Items)
	}
}

// TestToolResultIsInjectedWithTruncationFlag 验证截断标记真的会回注给模型。
func TestToolResultIsInjectedWithTruncationFlag(t *testing.T) {
	msg := formatToolResult(toolResult{
		Tool: ToolNeighbors, Items: []map[string]any{}, Total: 40, Truncated: true,
	})
	if !strings.Contains(msg, "截断") {
		t.Errorf("回注文本必须包含截断说明，实际 %q", msg)
	}
	ok := formatToolResult(toolResult{Tool: ToolNeighbors, Items: []map[string]any{}, Total: 0})
	if strings.Contains(ok, "截断") {
		t.Errorf("未截断时不应误报截断：%q", ok)
	}
}

// TestNormalizeCallIgnoresJSONOrderForG3 验证 G3 的归一化不吃 JSON 字段顺序。
func TestNormalizeCallIgnoresJSONOrder(t *testing.T) {
	a := ToolCallJSON(ToolLookupEntity, `{"name":"Vim","concept":"c-editor"}`)
	b := ToolCallJSON(ToolLookupEntity, `{ "concept" : "c-editor" , "name" : "  vim  " }`)
	if normalizeCall(a) != normalizeCall(b) {
		t.Errorf("归一化后应视为同一查询：\n%q\n%q", normalizeCall(a), normalizeCall(b))
	}
	c := ToolCallJSON(ToolLookupEntity, `{"name":"emacs"}`)
	if normalizeCall(a) == normalizeCall(c) {
		t.Error("不同查询不得被归一化成同一个键")
	}
}
