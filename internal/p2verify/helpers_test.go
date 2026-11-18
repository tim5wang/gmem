package p2verify

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tim5wang/gmem/extract"
	"github.com/tim5wang/gmem/sqlite"
	"github.com/tim5wang/gmem/store"

	_ "modernc.org/sqlite"
)

// ===========================================================================
// P2 的**独立验证**包：提取与消歧（docs/TD.md §3.2 D10 / §4）
//
// 与 internal/p1verify、internal/vtest 同一手法：
//   - 只 import 公开 API（extract / store / sqlite / recall），不 import 任何
//     extract 包的内部符号，也**不复用 extract/*_test.go 的任何代码**；
//   - 目标是找出实现者自己的测试测不到的问题，因此所有断言都从"外部可见结果"
//     出发：LLMClient 收到的请求/调用次数、落库后的 SQL 行、Degraded 文本。
//
// 这里刻意用 `p2verify`（而不是 extract 包内白盒）：
// 白盒测试能直接读内部字段，而内部字段"看起来对"不代表外部行为对。
// ===========================================================================

// openMemoryDB 打开一个内存库。
func openMemoryDB(t *testing.T) *sqlite.DB {
	t.Helper()
	d, err := sqlite.Open(context.Background(), sqlite.Config{})
	if err != nil {
		t.Fatalf("sqlite.Open(memory): %v", err)
	}
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("close db: %v", err)
		}
	})
	return d
}

// openFileDB 打开文件库并额外给一条直连连接（用于行级断言 nodes/edges/aliases）。
func openFileDB(t *testing.T) (*sqlite.DB, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "p2verify.db")
	d, err := sqlite.Open(context.Background(), sqlite.Config{Path: path})
	if err != nil {
		t.Fatalf("sqlite.Open(file): %v", err)
	}
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("close db: %v", err)
		}
	})
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw conn: %v", err)
	}
	t.Cleanup(func() {
		if err := raw.Close(); err != nil {
			t.Errorf("close raw conn: %v", err)
		}
	})
	return d, raw
}

// countRows 是行级断言的最小工具：不受端口实现影响，直接看表。
func countRows(t *testing.T, raw *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := raw.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

// seedNode 直接落一个节点（模拟"已有实体"）。
func seedNode(t *testing.T, d *sqlite.DB, id, canonical string) {
	t.Helper()
	scope := scopeOf(id)
	if err := d.Graph().PutNode(context.Background(), store.Node{
		ID: id, Scope: scope, CanonicalName: canonical,
	}); err != nil {
		t.Fatalf("PutNode(%q): %v", id, err)
	}
}

// seedAlias 直接落一条词典项（L0 词典规则与消歧候选池的唯一枚举入口）。
func seedAlias(t *testing.T, d *sqlite.DB, surface, nodeID string, conf float64) {
	t.Helper()
	if err := d.Store().PutAlias(context.Background(), store.Alias{
		Surface: surface, NodeID: nodeID, Source: "verify", Confidence: conf,
	}); err != nil {
		t.Fatalf("PutAlias(%q → %q): %v", surface, nodeID, err)
	}
}

// seedEdge 直接落一条边。
func seedEdge(t *testing.T, d *sqlite.DB, id, from, to string, rel store.Relation) {
	t.Helper()
	if err := d.Graph().PutEdge(context.Background(), store.Edge{
		ID: id, FromID: from, ToID: to, Relation: rel, Weight: 1, TCreated: 1,
	}); err != nil {
		t.Fatalf("PutEdge(%q): %v", id, err)
	}
}

// scopeOf 从测试用的节点 id 约定里取 scope（本包所有 seed 都用 user scope）。
func scopeOf(string) store.Scope { return store.ScopeUser }

// ---------------------------------------------------------------------------
// 从 LLMClient 收到的请求里读工具结果
//
// 为什么只能这样读：toolRunner 是 extract 包内部类型，外部包拿不到。
// 但"模型实际看到了什么"恰好是最该验的东西——截断标记、hops 夹紧、
// 只读拒绝，全都必须在**回注上下文**里可见才真正生效。
// ---------------------------------------------------------------------------

// toolResultLine 取出回注上下文里某个工具的那一行结果。
func toolResultLine(t *testing.T, prompt, toolName string) string {
	t.Helper()
	prefix := "【工具 " + toolName
	for _, line := range strings.Split(prompt, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	t.Fatalf("回注上下文里没有工具 %q 的结果；prompt=\n%s", toolName, prompt)
	return ""
}

// toolItems 解析工具结果行里的 JSON 数组。
func toolItems(t *testing.T, line string) []map[string]any {
	t.Helper()
	i := strings.LastIndex(line, "：[")
	if i < 0 {
		t.Fatalf("工具结果行里找不到 JSON 部分：%q", line)
	}
	var items []map[string]any
	if err := json.Unmarshal([]byte(line[i+len("："):]), &items); err != nil {
		t.Fatalf("解析工具结果 JSON 失败：%v（行=%q）", err, line)
	}
	return items
}

// mustContain 断言包含，并给出上下文。
func mustContain(t *testing.T, where, hay, needle string) {
	t.Helper()
	if !strings.Contains(hay, needle) {
		t.Fatalf("%s 应包含 %q，实际：\n%s", where, needle, hay)
	}
}

func mustNotContain(t *testing.T, where, hay, needle string) {
	t.Helper()
	if strings.Contains(hay, needle) {
		t.Fatalf("%s 不应包含 %q，实际：\n%s", where, needle, hay)
	}
}

// ---------------------------------------------------------------------------
// 测试替身（本包自带，不复用 extract.FakeLLM 的实现细节）
// ---------------------------------------------------------------------------

// recordingLLM 记录每次请求，并按 mode 决定返回什么。
//
// 与 extract.FakeLLM 的差别：它能按"第几轮"生成**逐轮不同**的工具调用。
// 这不是重复造轮子，而是 D10 断言的必要条件：
// 同一条 query 反复发会先触发 G3/G4，导致"+1 来自 G1"这件事无法被隔离验证。
type recordingLLM struct {
	reqs []extract.LLMRequest
	// nameOf 生成第 n 轮（0-based）的工具查询名；返回空串表示"这一轮给终稿"。
	nameOf func(n int) string
	// tool 是工具名，默认为 lookup_entity。
	tool string
	// finalJSON 是"不再请求工具"时给出的终稿。
	finalJSON string
	// onFinal 是可选的"强制终稿轮"钩子（用于 G5 的阻塞实验）。
	onFinal func(ctx context.Context) (extract.LLMResponse, error)
	// usage 是每轮上报的 token。
	usage extract.LLMUsage
}

func (r *recordingLLM) Complete(ctx context.Context, req extract.LLMRequest) (extract.LLMResponse, error) {
	n := len(r.reqs)
	r.reqs = append(r.reqs, req)
	if len(req.Tools) == 0 && r.onFinal != nil {
		return r.onFinal(ctx)
	}
	// 工具面被收走 = 强制终稿轮：模型必须给终稿（这里模拟一个"听话"的模型）。
	if len(req.Tools) == 0 {
		return extract.LLMResponse{Text: r.finalJSON, Usage: r.usage, Model: "recording"}, nil
	}
	tool := r.tool
	if tool == "" {
		tool = extract.ToolLookupEntity
	}
	name := ""
	if r.nameOf != nil {
		name = r.nameOf(n)
	}
	if name == "" {
		return extract.LLMResponse{Text: r.finalJSON, Usage: r.usage, Model: "recording"}, nil
	}
	return extract.LLMResponse{
		ToolCalls: []extract.ToolCall{
			extract.ToolCallJSON(tool, fmt.Sprintf(`{"name":%q}`, name)),
		},
		Usage: r.usage, Model: "recording",
	}, nil
}

// calls 返回实际调用次数。
func (r *recordingLLM) calls() int { return len(r.reqs) }

// callsWithTools 返回"给了工具声明"的调用次数。
func (r *recordingLLM) callsWithTools() int {
	n := 0
	for _, q := range r.reqs {
		if len(q.Tools) > 0 {
			n++
		}
	}
	return n
}

// callsWithoutTools 返回"收走工具面"的调用次数。
func (r *recordingLLM) callsWithoutTools() int { return r.calls() - r.callsWithTools() }

// lastUser 返回最后一轮请求的 User（含全部工具回注）。
func (r *recordingLLM) lastUser() string {
	if len(r.reqs) == 0 {
		return ""
	}
	return r.reqs[len(r.reqs)-1].User
}

// constEmbedder 返回固定向量，便于把 cosine 钉成 1 或 0。
type constEmbedder struct{ vec []float32 }

func (c constEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = append([]float32(nil), c.vec...)
	}
	return out, nil
}

// entityExtractor 是自定义的 L2：只回固定实体，不做任何 LLM 调用。
//
// 用途：把"消歧三区"的输入钉死，从而让 s 落在指定区间，不受 L0/L2 的随机性影响。
type entityExtractor struct {
	entities  []extract.ExtractedEntity
	relations []extract.ExtractedRelation
	degraded  []string
}

func (e entityExtractor) Extract(context.Context, extract.ExtractRequest) (extract.ExtractResult, error) {
	return extract.ExtractResult{
		Entities: e.entities, Relations: e.relations, Degraded: e.degraded,
	}, nil
}

// failingLexical 是"故意失败的词法端口"：命中 failWhen 的文档时就报错，
// 其余全部委托给真实后端。
//
// 这是验证"写入整体回滚"的唯一手段：绕过 Writer 的实现不会碰 Index，
// 因此这个端口失败时它照样能写成功——那就说明写入没走 Writer。
type failingLexical struct {
	inner    store.LexicalIndex
	failWhen string
	// failures 记录被拦下的次数（断言至少发生一次，避免测试假绿）。
	failures int
}

func (f *failingLexical) Index(ctx context.Context, docs []store.Document) error {
	for _, d := range docs {
		if strings.Contains(d.Text, f.failWhen) {
			f.failures++
			return fmt.Errorf("injected lexical failure on %q", d.ID)
		}
	}
	return f.inner.Index(ctx, docs)
}

func (f *failingLexical) Delete(ctx context.Context, keys []store.DocumentKey) error {
	return f.inner.Delete(ctx, keys)
}

func (f *failingLexical) Search(ctx context.Context, q store.LexicalQuery) ([]store.Hit, error) {
	return f.inner.Search(ctx, q)
}

// gradedLexical 是"只读观察词法索引"的包装：直接委托，不做修改。
func portsWithLexical(p store.Ports, l store.LexicalIndex) store.Ports {
	p.Lexical = l
	return p
}

// queryName 生成子测试名（避免用 fmt 造成测试名里带空格）。
func queryName(prefix string, n int) string {
	return fmt.Sprintf("%s=%d", prefix, n)
}

// hasGate 判断门禁是否触发过。
func hasGate(gates []extract.GateReason, want extract.GateReason) bool {
	for _, g := range gates {
		if g == want {
			return true
		}
	}
	return false
}

// hasDegraded 判断 Degraded 里是否有一条包含 needle 的说明。
func hasDegraded(list []string, needle string) bool {
	for _, s := range list {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

// readSource 读一份仓库源码（用于断言"文档承诺"，如"待校准"注释）。
//
// 为什么需要它：注释不是运行时行为，无法从 API 观察；但"默认阈值是待校准起点"
// 这条承诺恰恰写在注释里，而它决定了后来者敢不敢依赖 0.92 这个数字。
func readSource(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(rel)
	if err != nil {
		t.Fatalf("读取 %s: %v", rel, err)
	}
	return string(b)
}

// countRelation 数某个关系的边（含已失效行，用 raw SQL 避免端口过滤干扰）。
func countRelation(t *testing.T, raw *sql.DB, rel string) int {
	t.Helper()
	return countRows(t, raw, `SELECT COUNT(1) FROM edges WHERE relation = ?`, rel)
}
