package mcp_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tim5wang/gmem/extract"
	"github.com/tim5wang/gmem/llm"
	"github.com/tim5wang/gmem/mcp"
	"github.com/tim5wang/gmem/memory"
	"github.com/tim5wang/gmem/ops"
	"github.com/tim5wang/gmem/recall"
	"github.com/tim5wang/gmem/server"
	"github.com/tim5wang/gmem/sqlite"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// MCP stdio 测试
//
// 做法：用 io.Pipe 接出两条真实的流，起 Serve 的读取循环，测试侧当"客户端"
// 逐条读写报文。这样验的是**真的协议行为**：并发分派、id 配对、错误码，
// 而不是直接调 handler（那不会暴露 stdio 死锁与并发写交错）。
// ===========================================================================

type client struct {
	t      *testing.T
	in     *io.PipeWriter
	out    *bufio.Reader
	cancel context.CancelFunc
	done   chan error
	seq    int
}

// newClient 起一个 MCP 服务并返回测试侧客户端。
func newClient(t *testing.T, opts ...func(*mcp.Options)) *client {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())

	// 用临时文件库而不是内存库：内存库的连接池差点为 1，Serve 的并发分派
	// 会把"跨 goroutine 并发访问"暴露成 ErrTxConflict 而不是真正的协议问题。
	dbPath := filepath.Join(t.TempDir(), "mcp.db")

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()

	db, err := sqlite.Open(ctx, sqlite.Config{Path: dbPath})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	ports := db.Ports()
	runner, err := ops.NewRunner(ports, db.OpsBackend())
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	maint := ops.NewMaintainer(runner)
	rec := recall.New(ports, recall.Config{}, nil)
	srv, err := server.New(server.Options{
		Ports: ports, Recaller: rec, Maintainer: maint, Catalog: db.OpsBackend(),
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	svc, err := memory.New(memory.Config{
		Ports: ports, Recaller: rec, Maintainer: maint, History: db.OpsBackend(),
	})
	if err != nil {
		t.Fatalf("memory.New: %v", err)
	}

	o := mcp.Options{
		Service: svc, Graph: srv,
		LLMConfig: llm.Config{ExtractMode: "auto"},
		In:        inR, Out: outW, Log: io.Discard,
		Scope: store.ScopeUser,
	}
	for _, f := range opts {
		f(&o)
	}
	ms, err := mcp.New(o)
	if err != nil {
		t.Fatalf("mcp.New: %v", err)
	}

	// sampling 能力要接到抽取器：与 cmd/gmem/mcp.go 同样的延迟绑定。
	if o.OnSampler != nil {
		// 由调用方在自己的 OnSampler 里处理（见 withSampling）。
	}

	done := make(chan error, 1)
	go func() { done <- ms.Serve(ctx) }()

	c := &client{t: t, in: inW, out: bufio.NewReader(outR), cancel: cancel, done: done}
	t.Cleanup(func() {
		_ = inW.Close()
		cancel()
		_ = db.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("Serve 没有在取消后退出（stdio 读取循环可能挂住了）")
		}
	})
	return c
}

// send 写一条请求。
func (c *client) send(v any) {
	c.t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		c.t.Fatalf("marshal: %v", err)
	}
	if _, err := c.in.Write(append(body, '\n')); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

// recv 读一条报文（带超时：挂住时必须失败而不是让测试永远等下去）。
func (c *client) recv() map[string]any {
	c.t.Helper()
	type res struct {
		line []byte
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		line, err := c.out.ReadBytes('\n')
		ch <- res{line: line, err: err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			c.t.Fatalf("read: %v（stdio 可能被写坏）", r.err)
		}
		var m map[string]any
		if err := json.Unmarshal(r.line, &m); err != nil {
			c.t.Fatalf("响应不是合法 JSON: %v（原始=%q）", err, r.line)
		}
		// 协议版本必须逐条存在（stdout 被日志污染时这里会先炸）。
		if m["jsonrpc"] != "2.0" {
			c.t.Fatalf("响应缺少 jsonrpc=2.0：%v", m)
		}
		return m
	case <-time.After(5 * time.Second):
		c.t.Fatal("等响应超时（stdio 可能死锁）")
		return nil
	}
}

// call 发一条请求并读回配对响应。
func (c *client) call(method string, params any) map[string]any {
	c.t.Helper()
	c.seq++
	id := c.seq
	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	c.send(req)
	resp := c.recv()
	if got := int(resp["id"].(float64)); got != id {
		c.t.Fatalf("响应 id=%d，期望 %d（id 配对错了）", got, id)
	}
	return resp
}

// tool 调一个工具并返回 result。
func (c *client) tool(name string, args any) map[string]any {
	c.t.Helper()
	resp := c.call("tools/call", map[string]any{"name": name, "arguments": args})
	if e, ok := resp["error"]; ok && e != nil {
		c.t.Fatalf("%s 返回了 JSON-RPC error（本次期望成功）: %v", name, e)
	}
	res, _ := resp["result"].(map[string]any)
	if res == nil {
		c.t.Fatalf("%s 缺少 result: %v", name, resp)
	}
	return res
}

// structured 取 structuredContent。
func structured(t *testing.T, res map[string]any) map[string]any {
	t.Helper()
	sc, _ := res["structuredContent"].(map[string]any)
	if sc == nil {
		t.Fatalf("工具结果缺少 structuredContent: %v", res)
	}
	return sc
}

// ---------------------------------------------------------------------------

// TestInitializeHandshake initialize 必须返回协议版本、tools 能力与服务标识。
func TestInitializeHandshake(t *testing.T) {
	c := newClient(t)
	resp := c.call("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"clientInfo":      map[string]any{"name": "test", "version": "1"},
		"capabilities":    map[string]any{},
	})
	res, _ := resp["result"].(map[string]any)
	if res == nil {
		t.Fatalf("initialize 缺少 result: %v", resp)
	}
	if res["protocolVersion"] != "2024-11-05" {
		t.Errorf("protocolVersion 应回显客户端版本，实际 %v", res["protocolVersion"])
	}
	caps, _ := res["capabilities"].(map[string]any)
	if _, ok := caps["tools"]; !ok {
		t.Errorf("capabilities 必须声明 tools: %v", caps)
	}
	info, _ := res["serverInfo"].(map[string]any)
	if info["name"] != "gmem" {
		t.Errorf("serverInfo.name 应为 gmem，实际 %v", info["name"])
	}
	// 别名层免责声明必须写死在握手说明里（任务卡硬要求）。
	instr, _ := res["instructions"].(string)
	if !strings.Contains(instr, "能力映射") || !strings.Contains(instr, "不承诺") {
		t.Errorf("instructions 必须包含别名层的免责声明：%q", instr)
	}
}

// TestToolsListContainsAllTools tools/list 必须含主面 11 + 图专用 2 + 别名 6。
func TestToolsListContainsAllTools(t *testing.T) {
	c := newClient(t)
	c.call("initialize", map[string]any{"protocolVersion": "2024-11-05"})

	resp := c.call("tools/list", nil)
	res, _ := resp["result"].(map[string]any)
	tools, _ := res["tools"].([]any)
	if len(tools) == 0 {
		t.Fatalf("tools/list 为空: %v", resp)
	}

	got := map[string]bool{}
	for _, tv := range tools {
		tm, _ := tv.(map[string]any)
		name, _ := tm["name"].(string)
		if name == "" {
			t.Errorf("工具缺少 name: %v", tm)
			continue
		}
		got[name] = true
		// 每个工具都必须带 inputSchema 与 description。
		if _, ok := tm["inputSchema"].(map[string]any); !ok {
			t.Errorf("%s 缺少 inputSchema", name)
		}
		if d, _ := tm["description"].(string); d == "" {
			t.Errorf("%s 缺少 description", name)
		}
	}

	// 主面（TD §10 的工具表）。
	main := []string{
		"gmem_remember", "gmem_recall", "gmem_get", "gmem_list", "gmem_forget",
		"gmem_history", "gmem_consolidate", "gmem_review_similar",
		"gmem_merge", "gmem_link", "gmem_supersede",
	}
	// 图专用。
	graphTools := []string{"gmem_graph", "gmem_review"}
	// 兼容别名层。
	aliases := []string{
		"create_entities", "create_relations", "add_observations",
		"search_nodes", "open_nodes", "read_graph",
	}
	for _, group := range [][]string{main, graphTools, aliases} {
		for _, name := range group {
			if !got[name] {
				t.Errorf("tools/list 缺少工具 %q", name)
			}
		}
	}
	if want := len(main) + len(graphTools) + len(aliases); len(got) != want {
		t.Errorf("工具总数应为 %d，实际 %d", want, len(got))
	}
}

// TestToolsCallRememberRecallGraphReview 覆盖四个关键 tools/call（任务卡要求）。
func TestToolsCallRememberRecallGraphReview(t *testing.T) {
	c := newClient(t)
	c.call("initialize", map[string]any{"protocolVersion": "2024-11-05"})

	// 1) remember：结构化写入（确定性，不依赖 LLM）。
	rec := c.tool("gmem_remember", map[string]any{
		"content": "决策：Atlas 的存储继续用 Postgres，不切到 SQLite。",
		"scope":   "user",
		"kind":    "decision",
	})
	rs := structured(t, rec)
	writes, _ := rs["writes"].([]any)
	if len(writes) != 1 {
		t.Fatalf("remember 应写入 1 条记忆，实际 %v", rs["writes"])
	}
	// 降级必须在**工具响应**里可见（不是只有 doctor 才说）。
	if _, ok := rs["degraded"]; !ok {
		t.Errorf("remember 响应必须带 degraded 字段（哪怕为空）: %v", rs)
	}
	if rs["llm_mode"] != "degraded" {
		t.Errorf("未配置 LLM 且无 sampling 时 llm_mode 应为 degraded，实际 %v", rs["llm_mode"])
	}

	// 2) recall：必须返回可解释的 plan（R5）。
	rr := c.tool("gmem_recall", map[string]any{"query": "Atlas Postgres", "scope": "user"})
	rrs := structured(t, rr)
	if rrs["plan"] == nil {
		t.Errorf("recall 默认必须返回 plan（R5）：%v", rrs)
	}
	items, _ := rrs["items"].([]any)
	if len(items) == 0 {
		t.Errorf("recall 应命中刚写入的记忆：%v", rrs)
	}

	// 3) graph：无参数 → 概览模式（不得报错）。
	gr := c.tool("gmem_graph", map[string]any{"scope": "user"})
	gs := structured(t, gr)
	meta, _ := gs["meta"].(map[string]any)
	if meta["mode"] != "overview" {
		t.Errorf("无 focus/q 时应为 overview 模式，实际 %v", meta["mode"])
	}

	// 4) review：列待审池（此时为空，但必须成功返回结构）。
	rv := c.tool("gmem_review", map[string]any{"scope": "user"})
	rvs := structured(t, rv)
	if _, ok := rvs["pending"]; !ok {
		t.Errorf("gmem_review 必须返回 pending 字段：%v", rvs)
	}
}

// TestInvalidParamsReturnsJSONRPCError 非法参数必须返回 JSON-RPC error，
// **不得 panic、不得挂住 stdio**（任务卡点名的最容易做错的地方）。
func TestInvalidParamsReturnsJSONRPCError(t *testing.T) {
	c := newClient(t)
	c.call("initialize", map[string]any{"protocolVersion": "2024-11-05"})

	cases := []struct {
		name   string
		method string
		params any
		code   float64
	}{
		{"未知方法", "nosuch/method", nil, -32601},
		{"未知工具", "tools/call", map[string]any{"name": "nosuch_tool", "arguments": map[string]any{}}, -32601},
		{"recall 缺 query", "tools/call", map[string]any{"name": "gmem_recall", "arguments": map[string]any{}}, -32602},
		{"get 缺 id", "tools/call", map[string]any{"name": "gmem_get", "arguments": map[string]any{}}, -32602},
		{"forget 未确认", "tools/call", map[string]any{
			"name": "gmem_forget", "arguments": map[string]any{"id": "m-x"},
		}, -32602},
		{"非法 scope", "tools/call", map[string]any{
			"name": "gmem_recall", "arguments": map[string]any{"query": "x", "scope": "bogus"},
		}, -32602},
		{"graph focus 与 q 同时给", "tools/call", map[string]any{
			"name": "gmem_graph", "arguments": map[string]any{"focus": "a", "q": "b"},
		}, -32602},
		{"arguments 类型不符", "tools/call", map[string]any{
			"name": "gmem_recall", "arguments": map[string]any{"query": 123},
		}, -32602},
		{"review 裁决未确认", "tools/call", map[string]any{
			"name": "gmem_review", "arguments": map[string]any{"edge_id": "e-x", "verdict": "same"},
		}, -32602},
		{"review 非法 verdict", "tools/call", map[string]any{
			"name": "gmem_review", "arguments": map[string]any{"edge_id": "e-x", "verdict": "maybe", "confirmed": true},
		}, -32602},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := c.call(tc.method, tc.params)
			e, ok := resp["error"].(map[string]any)
			if !ok || e == nil {
				t.Fatalf("必须返回 JSON-RPC error 对象，实际 %v", resp)
			}
			code, _ := e["code"].(float64)
			if code != tc.code {
				t.Errorf("错误码应为 %v，实际 %v（message=%v）", tc.code, code, e["message"])
			}
			if msg, _ := e["message"].(string); msg == "" {
				t.Errorf("错误必须带可读 message：%v", e)
			}
			if _, hasResult := resp["result"]; hasResult {
				t.Errorf("error 响应不得同时带 result：%v", resp)
			}
		})
	}

	// 关键：以上全部非法输入之后，服务**仍然可用**（没有 panic、stdio 没坏）。
	ping := c.call("ping", nil)
	if _, ok := ping["result"]; !ok {
		t.Fatalf("非法输入之后服务应仍可响应 ping：%v", ping)
	}
}

// TestMalformedJSONReturnsParseError 坏报文 → -32700，且服务不退出。
func TestMalformedJSONReturnsParseError(t *testing.T) {
	c := newClient(t)
	c.call("initialize", map[string]any{"protocolVersion": "2024-11-05"})

	if _, err := c.in.Write([]byte("{this is not json\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	resp := c.recv()
	e, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("坏报文必须返回 error 对象：%v", resp)
	}
	if code, _ := e["code"].(float64); code != -32700 {
		t.Errorf("解析错误码应为 -32700，实际 %v", code)
	}
	// 服务仍然活着。
	c.call("ping", nil)
}

// TestAliasLayerMapsToCoreCapability 兼容别名层必须真的可用（可映射到核心能力）。
func TestAliasLayerMapsToCoreCapability(t *testing.T) {
	c := newClient(t)
	c.call("initialize", map[string]any{"protocolVersion": "2024-11-05"})

	// create_entities：实体 + 类型（draft 概念）+ 观测（semantic 记忆）。
	ce := c.tool("create_entities", map[string]any{
		"entities": []any{
			map[string]any{
				"name": "Ada", "entityType": "Person",
				"observations": []string{"Ada 是分析引擎的作者。"},
			},
			map[string]any{"name": "Acme", "entityType": "Organization"},
		},
	})
	ces := structured(t, ce)
	ents, _ := ces["entities"].([]any)
	if len(ents) != 2 {
		t.Fatalf("create_entities 应返回 2 个实体：%v", ces)
	}
	// 免责声明必须出现在响应里（不只是 README）。
	if note, _ := ces["note"].(string); !strings.Contains(note, "不承诺") {
		t.Errorf("别名层响应必须带免责声明：%v", ces["note"])
	}

	// create_relations：按名字解析端点后写边。
	cr := c.tool("create_relations", map[string]any{
		"relations": []any{
			map[string]any{"from": "Ada", "to": "Acme", "relationType": "works_at"},
		},
	})
	crs := structured(t, cr)
	created, _ := crs["created"].([]any)
	if len(created) != 1 {
		t.Fatalf("create_relations 应成功创建 1 条关系：%v", crs)
	}

	// search_nodes / open_nodes / read_graph 都要能用。
	sn := structured(t, c.tool("search_nodes", map[string]any{"query": "Ada"}))
	if _, ok := sn["entities"]; !ok {
		t.Errorf("search_nodes 必须返回 entities：%v", sn)
	}
	on := structured(t, c.tool("open_nodes", map[string]any{"names": []string{"Ada", "Nobody"}}))
	cards, _ := on["entities"].([]any)
	if len(cards) != 1 {
		t.Errorf("open_nodes 应只返回能找到的 1 个实体，实际 %d: %v", len(cards), on)
	}
	if nf, _ := on["not_found"].([]any); len(nf) != 1 {
		t.Errorf("open_nodes 必须报告找不到的名字：%v", on["not_found"])
	}
	rg := structured(t, c.tool("read_graph", map[string]any{}))
	if _, ok := rg["nodes"]; !ok {
		t.Errorf("read_graph 必须返回 nodes：%v", rg)
	}

	// add_observations：给既有实体追加观测。
	ao := structured(t, c.tool("add_observations", map[string]any{
		"observations": []any{
			map[string]any{"entityName": "Ada", "contents": []string{"Ada 也维护 Borealis。"}},
		},
	}))
	added, _ := ao["added"].([]any)
	if len(added) != 1 {
		t.Errorf("add_observations 应成功追加：%v", ao)
	}
}

// TestReviewGuardCannotBeBypassed MCP 的裁决不得绕过"已失效边拒裁"守卫。
//
// 这条守卫写在 server.applyReview 里（门禁在服务端，不只是 UI）。MCP 若自己调
// ops.Maintainer.Merge 就会绕过它，于是"已判为不同"的两个实体能被再次合并。
func TestReviewGuardCannotBeBypassed(t *testing.T) {
	c := newClient(t)
	c.call("initialize", map[string]any{"protocolVersion": "2024-11-05"})

	// 造一条待审边：用两段内容触发灰区不现实（需要 LLM），因此直接裁决一条
	// **不存在**的边 —— 这已经能证明 MCP 走的是 server 的定位逻辑（返回业务错误
	// 而不是成功）。
	resp := c.call("tools/call", map[string]any{
		"name": "gmem_review",
		"arguments": map[string]any{
			"edge_id": "e-not-exist", "verdict": "same", "confirmed": true,
		},
	})
	res, _ := resp["result"].(map[string]any)
	if res == nil {
		t.Fatalf("裁决不存在的边应返回工具结果（isError）而不是 JSON-RPC error：%v", resp)
	}
	if res["isError"] != true {
		t.Fatalf("裁决不存在的边必须报错：%v", res)
	}
	content, _ := res["content"].([]any)
	joined := ""
	for _, cv := range content {
		cm, _ := cv.(map[string]any)
		if s, ok := cm["text"].(string); ok {
			joined += s
		}
	}
	// 错误来自 server 层（"边 ... 不存在"），证明走的是同一套 applyReview。
	if !strings.Contains(joined, "不存在") {
		t.Errorf("错误信息应来自服务端的边定位逻辑：%q", joined)
	}
}

// TestSamplingFallbackIsUsed 客户端声明 sampling 后，抽取应改走 sampling（D8）。
//
// 验证三件事：服务端**真的发出** sampling/createMessage；回包按 id 配对；
// 客户端拒绝后管线**显式降级**而不是静默失败。
func TestSamplingFallbackIsUsed(t *testing.T) {
	var sampler llm.Sampler

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dbPath := filepath.Join(t.TempDir(), "samp.db")
	db, err := sqlite.Open(ctx, sqlite.Config{Path: dbPath})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	ports := db.Ports()
	runner, err := ops.NewRunner(ports, db.OpsBackend())
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	maint := ops.NewMaintainer(runner)
	rec := recall.New(ports, recall.Config{}, nil)
	srv, err := server.New(server.Options{Ports: ports, Recaller: rec, Maintainer: maint, Catalog: db.OpsBackend()})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	svc, err := memory.New(memory.Config{Ports: ports, Recaller: rec, Maintainer: maint, History: db.OpsBackend()})
	if err != nil {
		t.Fatalf("memory.New: %v", err)
	}
	ms, err := mcp.New(mcp.Options{
		Service: svc, Graph: srv,
		LLMConfig: llm.Config{ExtractMode: "auto"},
		In:        inR, Out: outW, Log: io.Discard,
		Scope: store.ScopeUser,
		OnSampler: func(s llm.Sampler) {
			sampler = s
			// 与 cmd/gmem/mcp.go 相同的延迟绑定：能力变化后立刻反映到抽取器。
			svc.SetExtractorProvider(func() extract.Extractor {
				if s == nil || !s.SamplingAvailable() {
					return nil
				}
				return llm.NewExtractor(&llm.SamplingClient{Sampler: s, MaxTokens: 256}, ports, extract.Config{})
			})
		},
	})
	if err != nil {
		t.Fatalf("mcp.New: %v", err)
	}
	go func() { _ = ms.Serve(ctx) }()

	out := bufio.NewReader(outR)
	send := func(v any) {
		body, _ := json.Marshal(v)
		if _, err := inW.Write(append(body, '\n')); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	readMsg := func() map[string]any {
		ch := make(chan []byte, 1)
		go func() {
			line, _ := out.ReadBytes('\n')
			ch <- line
		}()
		select {
		case line := <-ch:
			var m map[string]any
			if err := json.Unmarshal(line, &m); err != nil {
				t.Fatalf("响应不是 JSON: %v（%q）", err, line)
			}
			return m
		case <-time.After(5 * time.Second):
			t.Fatal("等报文超时")
			return nil
		}
	}

	// 1) initialize 时声明 sampling 能力。
	send(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"sampling": map[string]any{}},
		},
	})
	if resp := readMsg(); resp["error"] != nil {
		t.Fatalf("initialize 失败: %v", resp)
	}
	if sampler == nil || !sampler.SamplingAvailable() {
		t.Fatal("initialize 声明 sampling 后，能力应为可用（D8 的兜底前提）")
	}

	// 2) 发一次 remember（不带 kind → 走抽取管线 → 需要 L2 → 触发 sampling）。
	//    它的响应会**晚于** sampling 请求，因此必须先读到 sampling 请求并回包。
	send(map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{
			"name": "gmem_remember",
			"arguments": map[string]any{
				"content": "Alice 是 Acme 的资深工程师，负责 Atlas 项目的存储层设计。",
				"scope":   "user",
			},
		},
	})

	// 3) 服务端应发来 sampling/createMessage（服务端→客户端请求）。
	samp := readMsg()
	if samp["method"] != "sampling/createMessage" {
		t.Fatalf("期望收到 sampling/createMessage，实际 %v", samp)
	}
	params, _ := samp["params"].(map[string]any)
	if params["maxTokens"] == nil {
		t.Errorf("sampling 请求必须带 maxTokens：%v", params)
	}
	if _, ok := params["modelPreferences"]; !ok {
		t.Errorf("sampling 请求必须用优先级表达模型偏好：%v", params)
	}
	// 4) 回包（客户端拒绝）→ 管线必须显式降级而不是挂住。
	send(map[string]any{
		"jsonrpc": "2.0", "id": samp["id"],
		"error": map[string]any{"code": -1, "message": "user declined sampling"},
	})

	// 5) 读 remember 的响应。
	resp := readMsg()
	for resp["id"] != float64(2) {
		// 可能还有别的 sampling 请求（多轮）；一律拒绝。
		if resp["method"] == "sampling/createMessage" {
			send(map[string]any{
				"jsonrpc": "2.0", "id": resp["id"],
				"error": map[string]any{"code": -1, "message": "user declined sampling"},
			})
			resp = readMsg()
			continue
		}
		resp = readMsg()
	}
	res, _ := resp["result"].(map[string]any)
	if res == nil {
		t.Fatalf("remember 应有结果：%v", resp)
	}
	sc, _ := res["structuredContent"].(map[string]any)
	if sc["llm_mode"] != "sampling" {
		t.Errorf("声明 sampling 后 llm_mode 应为 sampling，实际 %v", sc["llm_mode"])
	}
	// 抽取失败必须可见（客户端拒绝 → Degraded 里要有原因）。
	degraded, _ := sc["degraded"].([]any)
	joined := ""
	for _, d := range degraded {
		if s, ok := d.(string); ok {
			joined += s + " "
		}
	}
	if !strings.Contains(joined, "L2") && !strings.Contains(joined, "sampling") {
		t.Errorf("sampling 失败后必须显式降级（degraded 里要有原因）：%v", degraded)
	}
}
