package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tim5wang/gmem/server"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// P5 CLI 测试（docs/TD.md §10 的命令表）
//
// 覆盖口径（任务卡要求）：
//   - **每个子命令至少 1 条成功 + 1 条错误路径**；
//   - --json 可被解析（严格 JSON，日志不混入）；
//   - forget 不确认时不删。
//
// 全部用**进程内**调用 runWithStdin（而不是像 main_test.go 那样起子进程）：
// 这些用例要断言的是"命令做了什么"，不是"退出码如何映射"。退出码由
// main() 的既有用例覆盖，这里只通过返回 error 判定非零。
// ===========================================================================

// cliEnv 是一个临时库环境。
type cliEnv struct {
	t    *testing.T
	path string
}

func newCLIEnv(t *testing.T) *cliEnv {
	t.Helper()
	return &cliEnv{t: t, path: filepath.Join(t.TempDir(), "cli.db")}
}

// run 跑一次命令，返回 stdout / stderr / error。
func (e *cliEnv) run(stdin string, args ...string) (string, string, error) {
	e.t.Helper()
	var out, errBuf bytes.Buffer
	err := runWithStdin(args, strings.NewReader(stdin), &out, &errBuf)
	return out.String(), errBuf.String(), err
}

// mustRun 要求命令成功。
func (e *cliEnv) mustRun(stdin string, args ...string) (string, string) {
	e.t.Helper()
	out, errOut, err := e.run(stdin, args...)
	if err != nil {
		e.t.Fatalf("命令失败 %v: %v（stderr=%s）", args, err, errOut)
	}
	return out, errOut
}

// TestCLIAllSubcommandsAcceptDB 每条子命令都接受 --db 并能跑通成功路径。
//
// 这是一条"覆盖面"用例：它保证新增子命令时不会漏掉 --db 这类公共选项，
// 也让"所有命令都走同一套装配（openApp）"这件事被真实执行一次。
func TestCLIAllSubcommandsAcceptDB(t *testing.T) {
	e := newCLIEnv(t)

	// 1) remember（成功：结构化写入）
	out, _ := e.mustRun("我偏好简洁的回复，不要客套话。", "remember", "--db", e.path, "--stdin",
		"--scope", "user", "--kind", "semantic")
	_ = out
	// 2) remember（走抽取管线的成功路径）
	e.mustRun("Alice 是 Acme 的工程师，负责 Atlas 项目。", "remember", "--db", e.path, "--stdin")

	// 3) list / get / recall / history / graph / review / consolidate / doctor
	listOut, _ := e.mustRun("", "list", "--db", e.path)
	if !strings.Contains(listOut, "共 ") {
		t.Errorf("list 输出缺少汇总行：%q", listOut)
	}
	// 取第一条记忆的 ID（list 的第一列）。
	id := firstListID(t, listOut)
	e.mustRun("", "get", "--db", e.path, id)
	e.mustRun("", "history", "--db", e.path, id)
	e.mustRun("", "recall", "--db", e.path, "Atlas")
	e.mustRun("", "graph", "--db", e.path)
	e.mustRun("", "review", "--db", e.path)
	e.mustRun("", "consolidate", "--db", e.path, "--scope", "user")
	e.mustRun("", "doctor", "--db", e.path)
}

// firstListID 从 list 的人类可读输出里取第一列 ID。
func firstListID(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if strings.HasPrefix(fields[0], "m-") {
			return fields[0]
		}
	}
	t.Fatalf("list 输出里找不到记忆 ID：%q", out)
	return ""
}

// TestCLIErrorPaths 每个子命令都至少有一条错误路径非零退出（且走 error 而不是 panic）。
func TestCLIErrorPaths(t *testing.T) {
	e := newCLIEnv(t)
	e.mustRun("我是 Alice，在 Acme 工作。", "remember", "--db", e.path, "--stdin", "--kind", "semantic")

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"remember 缺正文来源", []string{"remember", "--db", e.path}, "-f <file> 或 --stdin"},
		{"remember 无 --db", []string{"remember", "--stdin"}, "--db"},
		{"remember 非法 kind", []string{"remember", "--db", e.path, "--stdin", "--kind", "bogus"}, "--kind"},
		{"recall 缺查询", []string{"recall", "--db", e.path}, "查询串"},
		{"recall 非法 scope", []string{"recall", "--db", e.path, "--scope", "nope", "x"}, "--scope"},
		{"get 缺 id", []string{"get", "--db", e.path}, "记忆 ID"},
		{"get 不存在的 id", []string{"get", "--db", e.path, "m-nope"}, "不存在"},
		{"list 非法 kind", []string{"list", "--db", e.path, "--kind", "bogus"}, "--kind"},
		{"forget 缺 id 与 scope", []string{"forget", "--db", e.path}, "<id> 或 --scope"},
		{"forget 两者同时给", []string{"forget", "--db", e.path, "m-x", "--scope", "user"}, "二选一"},
		{"history 缺 id", []string{"history", "--db", e.path}, "记忆 ID"},
		{"consolidate 非法 scope", []string{"consolidate", "--db", e.path, "--scope", "nope"}, "--scope"},
		{"review 非法 scope", []string{"review", "--db", e.path, "--scope", "nope"}, "--scope"},
		{"graph focus 与 q 同时给", []string{"graph", "--db", e.path, "--focus", "a", "--q", "b"}, "二选一"},
		{"graph 非法 format", []string{"graph", "--db", e.path, "--format", "xml"}, "--format"},
		{"doctor 缺 --db", []string{"doctor"}, "--db"},
		{"demo 缺 --db", []string{"demo"}, "--db"},
		{"mcp 缺 --db", []string{"mcp"}, "--db"},
		{"mcp 拒绝 --json", []string{"mcp", "--db", e.path, "--json"}, "不适用"},
		{"未知子命令", []string{"nosuchverb"}, "未知子命令"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := e.run("", tc.args...)
			if err == nil {
				t.Fatalf("本应失败但成功了：%v", tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误信息应包含 %q，实际 %q", tc.want, err.Error())
			}
		})
	}
}

// TestCLIJSONIsStrictJSON 断言 --json 的 stdout 是**可被解析的单个 JSON 文档**。
//
// 这是"日志不得混入 stdout"的机器化判据：多一行日志，json.Unmarshal 立刻失败。
func TestCLIJSONIsStrictJSON(t *testing.T) {
	e := newCLIEnv(t)
	e.mustRun("我偏好简洁的回复，不要客套话。", "remember", "--db", e.path, "--stdin", "--kind", "semantic")
	e.mustRun("Alice 是 Acme 的工程师，负责 Atlas 项目。", "remember", "--db", e.path, "--stdin")

	listOut, _ := e.mustRun("", "list", "--db", e.path)
	id := firstListID(t, listOut)

	cmds := map[string][]string{
		"recall":      {"recall", "--db", e.path, "--json", "--explain", "Atlas"},
		"get":         {"get", "--db", e.path, "--json", id},
		"list":        {"list", "--db", e.path, "--json"},
		"history":     {"history", "--db", e.path, "--json", id},
		"graph":       {"graph", "--db", e.path, "--json"},
		"review":      {"review", "--db", e.path, "--json"},
		"consolidate": {"consolidate", "--db", e.path, "--json"},
		"doctor":      {"doctor", "--db", e.path, "--json"},
		"remember":    {"remember", "--db", e.path, "--json", "--stdin", "--kind", "decision"},
		"forget":      {"forget", "--db", e.path, "--json", "-y", id},
	}
	for name, args := range cmds {
		t.Run(name, func(t *testing.T) {
			out, errOut, err := e.run("要记住的一条决策：继续用 Postgres。", args...)
			if err != nil {
				t.Fatalf("%v 失败: %v（stderr=%s）", args, err, errOut)
			}
			var v any
			if err := json.Unmarshal([]byte(out), &v); err != nil {
				t.Fatalf("--json 输出不是合法 JSON（日志可能混入了 stdout）: %v\n输出=%q", err, out)
			}
			// 缩进 JSON 一定以 '{' 或 '[' 开头：前面有任何横幅都会破坏这一点。
			if trimmed := strings.TrimSpace(out); trimmed == "" ||
				(trimmed[0] != '{' && trimmed[0] != '[') {
				t.Fatalf("--json 输出必须以 JSON 开头，实际 %q", out)
			}
		})
	}
}

// TestCLIForgetRequiresConfirmation 未确认时**不得删除**（任务卡的硬性验收项）。
func TestCLIForgetRequiresConfirmation(t *testing.T) {
	e := newCLIEnv(t)
	e.mustRun("我偏好简洁的回复，不要客套话。", "remember", "--db", e.path, "--stdin", "--kind", "semantic")
	listOut, _ := e.mustRun("", "list", "--db", e.path)
	id := firstListID(t, listOut)

	// 1) 交互确认输入 "no" → 命令成功返回但**不做任何变更**。
	out, errOut, err := e.run("no\n", "forget", "--db", e.path, id)
	if err != nil {
		t.Fatalf("取消确认不应报错: %v", err)
	}
	if !strings.Contains(errOut, "已取消") {
		t.Errorf("stderr 应说明已取消：%q", errOut)
	}
	if strings.Contains(out, "已失效") {
		t.Errorf("取消后 stdout 不得出现\"已失效\"：%q", out)
	}

	// 2) 记忆仍然存在且**未失效**。
	memOut, _ := e.mustRun("", "get", "--db", e.path, "--json", id)
	var m map[string]any
	if err := json.Unmarshal([]byte(memOut), &m); err != nil {
		t.Fatalf("解析 get --json: %v", err)
	}
	if inv, ok := m["t_invalid"].(float64); ok && inv != 0 {
		t.Fatalf("未确认的 forget 竟然改了 t_invalid=%v", inv)
	}

	// 3) stdin 为空（EOF）也必须视为未确认。
	if _, _, err := e.run("", "forget", "--db", e.path, id); err != nil {
		t.Fatalf("EOF 取消不应报错: %v", err)
	}

	// 4) -y 才真的失效。
	e.mustRun("", "forget", "--db", e.path, "-y", id)
	memOut, _ = e.mustRun("", "get", "--db", e.path, "--json", id)
	if err := json.Unmarshal([]byte(memOut), &m); err != nil {
		t.Fatalf("解析 get --json: %v", err)
	}
	if inv, ok := m["t_invalid"].(float64); !ok || inv == 0 {
		t.Fatalf("-y 之后记忆应已失效，实际 t_invalid=%v", m["t_invalid"])
	}
}

// TestCLIForgetDryRun 只列不改。
func TestCLIForgetDryRun(t *testing.T) {
	e := newCLIEnv(t)
	e.mustRun("我偏好简洁的回复，不要客套话。", "remember", "--db", e.path, "--stdin", "--kind", "semantic")
	out, _ := e.mustRun("", "forget", "--db", e.path, "--scope", "user", "--dry-run")
	if !strings.Contains(out, "dry-run") {
		t.Errorf("dry-run 输出应声明未做变更：%q", out)
	}
	listOut, _ := e.mustRun("", "list", "--db", e.path)
	if !strings.Contains(listOut, "共 1 条") {
		t.Errorf("dry-run 之后记忆数应不变：%q", listOut)
	}
}

// TestCLIGraphDOTExportsInvalidatedAsDashed DOT 导出必须能看出"这条边已失效"。
func TestCLIGraphDOTExportsInvalidatedAsDashed(t *testing.T) {
	e := newCLIEnv(t)
	// 用 demo 造出"已失效边 + 待审边"这两个必须覆盖的形状。
	e.mustRun("", "demo", "--db", e.path)
	out, _ := e.mustRun("", "graph", "--db", e.path, "--format", "dot")
	if !strings.Contains(out, "digraph gmem {") {
		t.Fatalf("DOT 输出缺少 digraph 头：%q", out)
	}
	if !strings.Contains(out, "style=dashed") {
		t.Errorf("DOT 输出必须把已失效边画成 dashed（否则管道处理会把它当现行事实）")
	}
	if !strings.Contains(out, "color=blue") {
		t.Errorf("DOT 输出必须把 possiblySame 边标成蓝色（待审可见）")
	}
}

// TestCLIDoctorShowsDegradation doctor 必须**显式**报告 LLM / vector 降级。
func TestCLIDoctorShowsDegradation(t *testing.T) {
	e := newCLIEnv(t)
	// 确保不继承宿主环境里的 LLM 配置（否则本用例在开发机上会假绿）。
	for _, k := range []string{
		"GMEM_LLM_BASE_URL", "GMEM_LLM_API_KEY", "GMEM_LLM_MODEL", "GMEM_EXTRACT_MODE",
	} {
		t.Setenv(k, "")
	}
	out, _ := e.mustRun("", "doctor", "--db", e.path)
	for _, want := range []string{"extraction", "vector_lane", "L0+L1"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor 输出应包含 %q（降级必须可见）：%s", want, out)
		}
	}
	// --json 里也必须有 degraded 明细（机器可读的降级声明）。
	jsonOut, _ := e.mustRun("", "doctor", "--db", e.path, "--json")
	var rep map[string]any
	if err := json.Unmarshal([]byte(jsonOut), &rep); err != nil {
		t.Fatalf("doctor --json: %v", err)
	}
	degraded, _ := rep["degraded"].([]any)
	if len(degraded) == 0 {
		t.Errorf("doctor --json 的 degraded 不得为空（缺 LLM 时必须显式降级）：%v", rep)
	}
	llmInfo, _ := rep["llm"].(map[string]any)
	if llmInfo["mode"] != "degraded" {
		t.Errorf("无 LLM 配置时 llm.mode 应为 degraded，实际 %v", llmInfo["mode"])
	}
}

// TestCLIDoctorFailsOnBrokenDB 存在 FAIL 项时退出码非零。
func TestCLIDoctorFailsOnBrokenDB(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "no-such-dir", "x.db")
	var out, errBuf bytes.Buffer
	err := runWithStdin([]string{"doctor", "--db", bad}, strings.NewReader(""), &out, &errBuf)
	if err == nil {
		t.Fatal("库打不开时 doctor 必须非零退出")
	}
}

// TestCLIDemoEmptyDBThenRejectThenForce 覆盖 demo 的三条验收：
// 空库写入后 graph 非空、非空库默认拒绝、--force 幂等。
func TestCLIDemoEmptyDBThenRejectThenForce(t *testing.T) {
	e := newCLIEnv(t)

	// 1) 空库写入成功。
	out, _ := e.mustRun("", "demo", "--db", e.path)
	if !strings.Contains(out, "演示数据已写入") {
		t.Fatalf("demo 应报告写入成功：%q", out)
	}

	// 2) graph 必须**真实**返回节点与边（不是空图）。
	ctx := context.Background()
	a, err := openApp(ctx, appOptions{DBPath: e.path})
	if err != nil {
		t.Fatalf("openApp: %v", err)
	}
	defer func() { _ = a.Close() }()

	resp, err := a.server.GraphView(ctx, server.GraphQuery{Scope: store.ScopeUser})
	if err != nil {
		t.Fatalf("GraphView: %v", err)
	}
	if len(resp.Nodes) < 15 || len(resp.Nodes) > 25 {
		t.Errorf("演示节点数应在 15..25，实际 %d", len(resp.Nodes))
	}
	if len(resp.Edges) < 20 || len(resp.Edges) > 35 {
		t.Errorf("演示边数应在 20..35，实际 %d", len(resp.Edges))
	}

	// 3) 必须覆盖四类形状：subtype_of 传递链、works_at、possiblySame、已失效。
	var hasSubtype, hasWorksAt, hasPossiblySame, hasInvalid bool
	for _, e := range resp.Edges {
		switch e.Relation {
		case "subtype_of":
			hasSubtype = true
		case "works_at":
			hasWorksAt = true
		case "possiblySame":
			hasPossiblySame = true
		}
		if e.Invalidated {
			hasInvalid = true
		}
	}
	for name, ok := range map[string]bool{
		"subtype_of 传递链": hasSubtype, "works_at 非传递": hasWorksAt,
		"possiblySame 待审边": hasPossiblySame, "t_invalid 非零的已失效边": hasInvalid,
	} {
		if !ok {
			t.Errorf("演示图缺少：%s", name)
		}
	}

	// 4) /api/entity 的等价物（EntityView）必须能真实返回实体卡。
	card, err := a.server.EntityView(ctx, "n-demo-alice", nil)
	if err != nil {
		t.Fatalf("EntityView: %v", err)
	}
	if card.CanonicalName == "" || len(card.Edges) == 0 {
		t.Errorf("实体卡应带名字与关系：%+v", card)
	}

	// 5) 非空库默认拒绝。
	if _, _, err := e.run("", "demo", "--db", e.path); err == nil {
		t.Fatal("非空库必须默认拒绝写入演示数据")
	}

	// 6) --force 幂等：行数不变。
	before := dataTotal(t, e.path)
	e.mustRun("", "demo", "--db", e.path, "--force")
	after := dataTotal(t, e.path)
	if before != after {
		t.Fatalf("--force 重跑必须幂等（行数不变），实际 %d → %d", before, after)
	}
}

// dataTotal 统计"用户数据"表的总行数。
func dataTotal(t *testing.T, dbPath string) int64 {
	t.Helper()
	ctx := context.Background()
	a, err := openApp(ctx, appOptions{DBPath: dbPath})
	if err != nil {
		t.Fatalf("openApp: %v", err)
	}
	defer func() { _ = a.Close() }()
	counts, err := a.db.DataCounts(ctx)
	if err != nil {
		t.Fatalf("DataCounts: %v", err)
	}
	var total int64
	for _, n := range counts {
		total += n
	}
	return total
}

// TestCLIRememberFileInput 覆盖 -f 文件路径（与 --stdin 是两条不同的输入分支）。
func TestCLIRememberFileInput(t *testing.T) {
	e := newCLIEnv(t)
	f := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(f, []byte("一条来自文件的偏好：我偏好简洁的回复。"), 0o600); err != nil {
		t.Fatalf("写入临时文件: %v", err)
	}
	out, _ := e.mustRun("", "remember", "--db", e.path, "-f", f, "--kind", "semantic", "--json")
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("-f 模式的 --json 输出: %v", err)
	}
	writes, _ := res["writes"].([]any)
	if len(writes) != 1 {
		t.Errorf("-f 写入应产生 1 条记忆，实际 %v", res["writes"])
	}

	// 空文件必须被拒（不得写一条空记忆）。
	empty := filepath.Join(t.TempDir(), "empty.txt")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatalf("写入空文件: %v", err)
	}
	if _, _, err := e.run("", "remember", "--db", e.path, "-f", empty); err == nil {
		t.Fatal("空文件必须被拒绝")
	}
}

// TestCLIRecallExplainPrintsPlan --explain 必须打印 lane 选择与分数分解（R5）。
func TestCLIRecallExplainPrintsPlan(t *testing.T) {
	e := newCLIEnv(t)
	e.mustRun("", "demo", "--db", e.path)
	out, _ := e.mustRun("", "recall", "--db", e.path, "--explain", "Atlas Postgres")
	for _, want := range []string{"plan", "lanes:", "分数分解"} {
		if !strings.Contains(out, want) {
			t.Errorf("--explain 输出应包含 %q：%s", want, out)
		}
	}
}

// TestCLIGraphExportsSubgraphOfFocus focus 模式只导出邻域（不是全图）。
func TestCLIGraphExportsSubgraphOfFocus(t *testing.T) {
	e := newCLIEnv(t)
	e.mustRun("", "demo", "--db", e.path)
	out, _ := e.mustRun("", "graph", "--db", e.path, "--focus", "n-demo-alice", "--hops", "1", "--json")
	var resp server.GraphResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("graph --json: %v", err)
	}
	if resp.Meta.Mode != "focus" {
		t.Errorf("mode 应为 focus，实际 %q", resp.Meta.Mode)
	}
	if resp.Meta.Hops != 1 {
		t.Errorf("hops 应回显 1，实际 %d", resp.Meta.Hops)
	}
	if len(resp.Nodes) == 0 || len(resp.Nodes) > 15 {
		t.Errorf("focus 模式应只导出邻域（1..15 个节点），实际 %d", len(resp.Nodes))
	}
}
