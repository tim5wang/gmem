package p2verify

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/tim5wang/gmem/extract"
	"github.com/tim5wang/gmem/recall"
	"github.com/tim5wang/gmem/sqlite"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// 攻击面 2：工具面必须极小（docs/TD.md §3.2 "工具面刻意极小"）
//
// 三条隐性约束全是防循环的，本文件逐条从**外部可见结果**验：
//  1. hops 硬上限 1 —— 模型不能自己发起多跳；
//  2. 无写工具 —— 工具面里没有 create/update/delete；
//  3. 返回带截断标记 —— 超上限时必须告诉模型"这不是全部"。
//
// 为什么必须验"模型实际看到了什么"：工具实现返回 Truncated=true 但回注时
// 丢掉这个标记，等于没做——模型照样以为查完了。因此断言打在 LLMClient
// 收到的 User 文本上（那是模型的唯一视野）。
// ===========================================================================

// finalStep 是一个合法终稿（工具轮之后收口用）。
func finalStep() extract.FakeStep {
	return extract.FakeStep{Text: extract.BuildFinalJSON(
		[]extract.EntityJSON{{Name: "vim"}}, nil)}
}

// runOneTool 让模型发一次指定工具调用，然后交终稿；返回模型第二轮看到的 User。
func runOneTool(t *testing.T, cfg extract.Config, ports store.Ports, call extract.ToolCall) string {
	t.Helper()
	llm := &scriptedLLM{steps: []extract.LLMResponse{
		{ToolCalls: []extract.ToolCall{call}},
		{Text: extract.BuildFinalJSON([]extract.EntityJSON{{Name: "vim"}}, nil)},
	}}
	x := extract.NewLLMExtractor(llm, ports, cfg)
	res, err := x.Extract(context.Background(), extract.ExtractRequest{Episode: probeEpisode()})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	_ = res
	if len(llm.reqs) < 2 {
		t.Fatalf("应有至少 2 轮请求（工具轮 + 终稿轮），实际 %d 轮", len(llm.reqs))
	}
	return llm.reqs[1].User
}

// TestToolSurfaceIsThreeReadOnlyTools 断言工具面只有三个只读工具。
//
// 判据不只是"数量是 3"：工具名里出现任何写动词都直接判失败。
// 写工具会让模型在半途改动库，从而影响自己的后续查询——那正是
// "抽取一次 = 最多一次多读循环"这个成本上界失效的方式。
func TestToolSurfaceIsThreeReadOnlyTools(t *testing.T) {
	specs := extract.ToolSpecs()
	if len(specs) != 3 {
		t.Fatalf("工具面应恰好 3 个只读工具（§3.2 表），实际 %d 个", len(specs))
	}

	want := map[string]bool{
		extract.ToolLookupEntity:   false,
		extract.ToolNeighbors:      false,
		extract.ToolSearchMemories: false,
	}
	mutating := []string{
		"create", "update", "delete", "put", "insert", "write", "set", "remove",
		"merge", "upsert", "invalidate", "add", "patch", "drop", "clear",
	}
	for _, s := range specs {
		if _, ok := want[s.Name]; !ok {
			t.Errorf("工具面出现了未登记的工具 %q（白名单只有 lookup_entity/neighbors/search_memories）", s.Name)
		}
		want[s.Name] = true
		lower := strings.ToLower(s.Name)
		for _, verb := range mutating {
			if strings.Contains(lower, verb) {
				t.Errorf("工具 %q 的名字含写动词 %q：工具面必须完全只读", s.Name, verb)
			}
		}
		if len(s.Parameters) == 0 {
			t.Errorf("工具 %q 缺少参数 schema", s.Name)
		}
		// 参数 schema 里也不允许出现写动词（防止"只读名字 + 写参数"的绕过）。
		params := strings.ToLower(string(s.Parameters))
		for _, verb := range []string{"create", "update", "delete", "upsert", "insert"} {
			if strings.Contains(params, `"`+verb) {
				t.Errorf("工具 %q 的参数 schema 含写语义参数 %q", s.Name, verb)
			}
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("缺少工具 %q", name)
		}
	}

	// 宿主实际收到的声明必须与 ToolSpecs 一致（防"文档三个、实际四个"）。
	llm := &recordingLLM{nameOf: func(int) string { return "" }}
	x, ctx := newExtractor(t, extract.Config{MaxTurns: 3}, llm)
	if _, err := x.Extract(ctx, extract.ExtractRequest{Episode: probeEpisode()}); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(llm.reqs) == 0 || len(llm.reqs[0].Tools) != 3 {
		t.Fatalf("宿主收到的工具声明应为 3 个，实际 %d 个",
			len(llm.reqs[0].Tools))
	}
	for _, s := range llm.reqs[0].Tools {
		if _, ok := want[s.Name]; !ok {
			t.Errorf("宿主收到了未登记的工具 %q", s.Name)
		}
	}
}

// TestToolUnknownNameIsError 断言"编造工具名"不会被静默当成空结果。
//
// 静默空结果会让模型以为"确实没有"，而真实原因是它写错了工具名——
// 这条是 §3.2 里"未知工具名是错误（不是静默空结果）"的落地。
func TestToolUnknownNameIsError(t *testing.T) {
	d := openMemoryDB(t)
	user := runOneTool(t, extract.Config{MaxTurns: 3}, d.Ports(),
		extract.ToolCall{Name: "delete_entity",
			Arguments: json.RawMessage(`{"name":"vim"}`)})
	mustContain(t, "回注上下文", user, "未知工具")
	mustContain(t, "回注上下文", user, "delete_entity")
	// 必须明确说明"没有执行"，否则模型可能以为自己删掉了东西。
	mustContain(t, "回注上下文", user, "失败")
}

// TestHopsClampedToHardMaxOne 攻击 hops 夹紧：模型要 2 或 99，只能拿到 1 跳。
//
// 造一条 A→B→C 的链：若 hops 被放行到 2，B→C 会出现在结果里（depth=2）。
// 因此断言"结果里最大 depth == 1"就能证明夹紧真的生效，而不是参数被忽略。
func TestHopsClampedToHardMaxOne(t *testing.T) {
	for _, reqHops := range []string{"2", "99", "1000000"} {
		reqHops := reqHops
		t.Run("hops="+reqHops, func(t *testing.T) {
			d := openMemoryDB(t)
			seedNode(t, d, "n-a", "alpha")
			seedNode(t, d, "n-b", "beta")
			seedNode(t, d, "n-c", "gamma")
			seedEdge(t, d, "e-ab", "n-a", "n-b", store.RelationUses)
			seedEdge(t, d, "e-bc", "n-b", "n-c", store.RelationUses)

			user := runOneTool(t, extract.Config{MaxTurns: 3}, d.Ports(),
				extract.ToolCall{Name: extract.ToolNeighbors,
					Arguments: json.RawMessage(`{"node_id":"n-a","hops":` + reqHops + `}`)})

			line := toolResultLine(t, user, extract.ToolNeighbors)
			items := toolItems(t, line)
			// 多跳泄漏的判定：B→C 出现在结果里。
			for _, it := range items {
				if it["edge_id"] == "e-bc" {
					t.Fatalf("hops=%s 时拿到了 2 跳边 e-bc（B→C）：hops 未被夹到 %d，多跳遍历会突破 G1 的轮数控制",
						reqHops, extract.HardMaxHops)
				}
				if depth, ok := it["depth"].(float64); ok && depth > float64(extract.HardMaxHops) {
					t.Fatalf("hops=%s 时结果含 depth=%v 的边（硬上限 %d）", reqHops, depth, extract.HardMaxHops)
				}
			}
			if len(items) != 1 || items[0]["edge_id"] != "e-ab" {
				t.Fatalf("hops=%s 应只返回 1 跳边 e-ab，实际 %v", reqHops, items)
			}
			// 被裁剪必须让模型知道（"truncated"是契约的一部分）。
			mustContain(t, "hops="+reqHops+" 的回注", line, "截断")
		})
	}
}

// TestHopsZeroOrNegativeDefaultsToOne 断言缺省/非法 hops 落到 1，而不是 0 跳或无限。
func TestHopsZeroOrNegativeDefaultsToOne(t *testing.T) {
	for _, args := range []string{
		`{"node_id":"n-a"}`,
		`{"node_id":"n-a","hops":0}`,
		`{"node_id":"n-a","hops":-5}`,
	} {
		args := args
		t.Run(args, func(t *testing.T) {
			d := openMemoryDB(t)
			seedNode(t, d, "n-a", "alpha")
			seedNode(t, d, "n-b", "beta")
			seedEdge(t, d, "e-ab", "n-a", "n-b", store.RelationUses)

			user := runOneTool(t, extract.Config{MaxTurns: 3}, d.Ports(),
				extract.ToolCall{Name: extract.ToolNeighbors, Arguments: json.RawMessage(args)})
			items := toolItems(t, toolResultLine(t, user, extract.ToolNeighbors))
			if len(items) != 1 || items[0]["edge_id"] != "e-ab" {
				t.Fatalf("hops 缺省/非法时应等价于 1 跳，实际 %v", items)
			}
		})
	}
}

// TestLookupEntityTruncatesAtTen 攻击 lookup_entity 的行数上限与截断标记。
func TestLookupEntityTruncatesAtTen(t *testing.T) {
	d := openMemoryDB(t)
	// 15 个含 "target" 的词典项 → 上限 10，必须截断且明确告知。
	const n = 15
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("n-target-%02d", i)
		seedNode(t, d, id, fmt.Sprintf("target-%02d", i))
		seedAlias(t, d, fmt.Sprintf("target-%02d", i), id, 0.9)
	}
	user := runOneTool(t, extract.Config{}, d.Ports(),
		extract.ToolCall{Name: extract.ToolLookupEntity,
			Arguments: json.RawMessage(`{"name":"target"}`)})

	line := toolResultLine(t, user, extract.ToolLookupEntity)
	items := toolItems(t, line)
	if len(items) != 10 {
		t.Fatalf("lookup_entity 行数上限应为 10，实际返回 %d 条", len(items))
	}
	mustContain(t, "lookup_entity 的回注", line, "截断")
	// Total 必须报告真实命中数（15），否则模型无法判断"还有多少"。
	mustContain(t, "lookup_entity 的回注", line, "共 15 条")
}

// TestNeighborsTruncatesAtThirty 攻击 neighbors 的行数上限与截断标记。
func TestNeighborsTruncatesAtThirty(t *testing.T) {
	d := openMemoryDB(t)
	seedNode(t, d, "n-hub", "hub")
	const n = 40
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("n-leaf-%02d", i)
		seedNode(t, d, id, fmt.Sprintf("leaf-%02d", i))
		seedEdge(t, d, fmt.Sprintf("e-hub-%02d", i), "n-hub", id, store.RelationUses)
	}
	user := runOneTool(t, extract.Config{}, d.Ports(),
		extract.ToolCall{Name: extract.ToolNeighbors,
			Arguments: json.RawMessage(`{"node_id":"n-hub"}`)})

	line := toolResultLine(t, user, extract.ToolNeighbors)
	items := toolItems(t, line)
	if len(items) != 30 {
		t.Fatalf("neighbors 行数上限应为 30，实际返回 %d 条", len(items))
	}
	mustContain(t, "neighbors 的回注", line, "截断")
	mustContain(t, "neighbors 的回注", line, "共 40 条")
}

// seedSearchableMemories 用 recall.Writer 落 n 条可被 "缓存" 搜到的记忆。
//
// 刻意走 Writer 而不是 Store.PutMemory：本用例要的是"词法通道真的有内容"，
// 直接写主表不会进 FTS，测试会假绿。
func seedSearchableMemories(t *testing.T, d *sqlite.DB, n int) {
	t.Helper()
	w := recall.NewWriter(d.Ports())
	for i := 0; i < n; i++ {
		if _, err := w.PutMemory(context.Background(), store.Memory{
			ID: fmt.Sprintf("m-cache-%02d", i), Scope: store.ScopeUser,
			Kind: store.KindSemantic, Content: fmt.Sprintf("缓存策略第 %02d 条：缓存失效与缓存穿透", i),
			TCreated: 1,
		}); err != nil {
			t.Fatalf("Writer.PutMemory(%d): %v", i, err)
		}
	}
}

// TestSearchMemoriesCapsAtFive 攻击 search_memories 的 5 条上限。
func TestSearchMemoriesCapsAtFive(t *testing.T) {
	d := openMemoryDB(t)
	seedSearchableMemories(t, d, 8)

	t.Run("k 超上限被夹紧并标记截断", func(t *testing.T) {
		user := runOneTool(t, extract.Config{}, d.Ports(),
			extract.ToolCall{Name: extract.ToolSearchMemories,
				Arguments: json.RawMessage(`{"query":"缓存","k":99}`)})
		line := toolResultLine(t, user, extract.ToolSearchMemories)
		items := toolItems(t, line)
		if len(items) != 5 {
			t.Fatalf("search_memories 上限应为 5，实际返回 %d 条", len(items))
		}
		mustContain(t, "k=99 的回注", line, "截断")
	})

	t.Run("k 缺省也只给 5 条", func(t *testing.T) {
		user := runOneTool(t, extract.Config{}, d.Ports(),
			extract.ToolCall{Name: extract.ToolSearchMemories,
				Arguments: json.RawMessage(`{"query":"缓存"}`)})
		items := toolItems(t, toolResultLine(t, user, extract.ToolSearchMemories))
		if len(items) != 5 {
			t.Fatalf("k 缺省时应取上限 5，实际 %d 条", len(items))
		}
	})

	// 【可后续】k<=5 时后端只返回 k 条，工具无从知道"还有更多"，
	// 因此不可能给出截断标记。下面把现状钉死，避免将来被无意"修好又改坏"。
	t.Run("k=5 时无法感知还有更多（已知限制）", func(t *testing.T) {
		user := runOneTool(t, extract.Config{}, d.Ports(),
			extract.ToolCall{Name: extract.ToolSearchMemories,
				Arguments: json.RawMessage(`{"query":"缓存","k":5}`)})
		line := toolResultLine(t, user, extract.ToolSearchMemories)
		items := toolItems(t, line)
		if len(items) != 5 {
			t.Fatalf("应返回 5 条，实际 %d 条", len(items))
		}
		if strings.Contains(line, "截断") {
			t.Logf("search_memories 已能在 k<=limit 时报告截断（限制已修复）：%s", line)
			return
		}
		t.Logf(`【可后续】search_memories 在 k<=SearchLimit 时无法给出截断标记：
  实际命中 >= 8 条（种了 8 条），返回 5 条，但回注里没有"结果已截断"：
  %s
影响：模型会把"只给了 5 条"理解成"只有 5 条相关记忆"，从而漏掉已有的
上下文证据（表现为抽取质量下降，不报错）。
最小修法：内部按 limit+1 查询，多出 1 条即置 Truncated（不改公开契约）。`, line)
	})
}

// TestToolScopeIsolation 断言工具不返回跨 scope 的数据。
//
// 范围隔离是 §2 的硬边界：工具面一旦漏出跨 scope 的实体，
// 模型会把另一个 scope 的事实当成本 scope 的上下文写进记忆。
func TestToolScopeIsolation(t *testing.T) {
	d := openMemoryDB(t)
	seedNode(t, d, "n-user", "target-user")
	seedAlias(t, d, "target-user", "n-user", 0.9)
	if err := d.Graph().PutNode(context.Background(), store.Node{
		ID: "n-agent", Scope: store.ScopeAgent, CanonicalName: "target-agent",
	}); err != nil {
		t.Fatalf("PutNode(agent): %v", err)
	}
	seedAlias(t, d, "target-agent", "n-agent", 0.9)

	user := runOneTool(t, extract.Config{}, d.Ports(),
		extract.ToolCall{Name: extract.ToolLookupEntity,
			Arguments: json.RawMessage(`{"name":"target"}`)})
	line := toolResultLine(t, user, extract.ToolLookupEntity)
	mustNotContain(t, "lookup_entity 的回注", line, "n-agent")
	mustContain(t, "lookup_entity 的回注", line, "n-user")
}
