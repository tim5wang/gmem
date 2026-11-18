package p1verify

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tim5wang/gmem/recall"
	"github.com/tim5wang/gmem/sqlite"
	"github.com/tim5wang/gmem/store"

	_ "modernc.org/sqlite"
)

// 本包是 P1 召回落（commit 47cf9bf）的**独立验证**包。
//
// 与 recall/integration_test.go 的关系：零复用。作者测试断言"功能可用"，
// 本包攻击"失败路径"：写入失败时两边是否都不落库、insufficient 的**反向**误报、
// 跳数白名单在"节点→记忆"映射层是否真的还成立、Explain 是否逐项复算得出来。
//
// 约束：只新增 _test.go，不改生产代码，不改 docs/TD.md。
// 只通过公开 API（store 四个 Port + recall 公开类型）观察行为；唯一例外是
// D13 的 (id,version) 需要行级证据时，直连同一个库文件做只读查询。

// ---------------------------------------------------------------------------
// 开库
// ---------------------------------------------------------------------------

// openMemory 打开内存库（不落盘）。
func openMemory(t *testing.T) *sqlite.DB {
	t.Helper()
	d, err := sqlite.Open(context.Background(), sqlite.Config{})
	if err != nil {
		t.Fatalf("sqlite.Open(memory): %v", err)
	}
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return d
}

// openFile 打开文件库，并返回一条**直连只读**用于行级观察（memories_fts 的 (id,version)）。
func openFile(t *testing.T) (*sqlite.DB, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "p1verify.db")
	d, err := sqlite.Open(context.Background(), sqlite.Config{Path: path})
	if err != nil {
		t.Fatalf("sqlite.Open(file): %v", err)
	}
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	raw.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if err := raw.Close(); err != nil {
			t.Errorf("raw close: %v", err)
		}
	})
	return d, raw
}

// ---------------------------------------------------------------------------
// 种子
// ---------------------------------------------------------------------------

func putConcept(t *testing.T, s store.Store, id, name string, status store.ConceptStatus, definition string) {
	t.Helper()
	if err := s.PutConcept(context.Background(), store.Concept{
		ID: id, Name: name, Status: status, Definition: definition,
	}); err != nil {
		t.Fatalf("PutConcept(%s): %v", id, err)
	}
}

func putNode(t *testing.T, g store.GraphStore, id, name string, scope store.Scope, conceptID, episode string) {
	t.Helper()
	if err := g.PutNode(context.Background(), store.Node{
		ID: id, Scope: scope, ConceptID: conceptID, CanonicalName: name, SourceEpisode: episode,
	}); err != nil {
		t.Fatalf("PutNode(%s): %v", id, err)
	}
}

func putEdge(t *testing.T, g store.GraphStore, id, from, to string, rel store.Relation, tValid, tCreated int64) {
	t.Helper()
	if err := g.PutEdge(context.Background(), store.Edge{
		ID: id, FromID: from, ToID: to, Relation: rel, Weight: 1,
		TValid: tValid, TCreated: tCreated,
	}); err != nil {
		t.Fatalf("PutEdge(%s): %v", id, err)
	}
}

func putAlias(t *testing.T, s store.Store, surface, nodeID string, conf float64) {
	t.Helper()
	if err := s.PutAlias(context.Background(), store.Alias{
		Surface: surface, NodeID: nodeID, Source: "dict", Confidence: conf,
	}); err != nil {
		t.Fatalf("PutAlias(%s): %v", surface, err)
	}
}

// seedEditorOntology 写"Alice 用 vim"的最小本体 + 图 + 词典项。
//
// 记忆本身由各用例自己写（不同用例要不同 concept_id 与内容）。
func seedEditorOntology(t *testing.T, d *sqlite.DB) {
	t.Helper()
	putConcept(t, d.Store(), "c-editor", "编辑器", store.ConceptCertified, "用于编写代码的文本编辑程序")
	putNode(t, d.Graph(), "n-alice", "Alice", store.ScopeUser, "", "")
	putNode(t, d.Graph(), "n-vim", "vim", store.ScopeUser, "c-editor", "")
	putEdge(t, d.Graph(), "e-alice-vim", "n-alice", "n-vim", store.RelationUses, 1000, 1000)
	putAlias(t, d.Store(), "Alice", "n-alice", 1.0)
}

// ---------------------------------------------------------------------------
// 断言辅助
// ---------------------------------------------------------------------------

func laneOf(plan *recall.RecallPlan, lane recall.LaneName) (recall.LanePlan, bool) {
	if plan == nil {
		return recall.LanePlan{}, false
	}
	for _, lp := range plan.Lanes {
		if lp.Lane == lane {
			return lp, true
		}
	}
	return recall.LanePlan{}, false
}

func executedLanes(plan *recall.RecallPlan) []recall.LaneName {
	var out []recall.LaneName
	if plan == nil {
		return nil
	}
	for _, lp := range plan.Lanes {
		if lp.Executed {
			out = append(out, lp.Lane)
		}
	}
	return out
}

func relationPlanOf(plan *recall.RecallPlan, rel store.Relation) (recall.RelationPlan, bool) {
	if plan == nil {
		return recall.RelationPlan{}, false
	}
	for _, rp := range plan.Relations {
		if rp.Relation == rel {
			return rp, true
		}
	}
	return recall.RelationPlan{}, false
}

func itemIDs(items []recall.RecallItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Memory.ID
	}
	return out
}

func containsSubstr(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func hasStep(steps []recall.PlanStep, name string) bool {
	for _, s := range steps {
		if s.Step == name {
			return true
		}
	}
	return false
}

func hasStepPrefix(steps []recall.PlanStep, prefix string) bool {
	for _, s := range steps {
		if strings.HasPrefix(s.Step, prefix) {
			return true
		}
	}
	return false
}

// stepDetail 取某个 step 的 Detail（不存在时 ok=false）。
func stepDetail(steps []recall.PlanStep, name string) (string, bool) {
	for _, s := range steps {
		if s.Step == name {
			return s.Detail, true
		}
	}
	return "", false
}

func droppedReasons(plan *recall.RecallPlan) []string {
	if plan == nil {
		return nil
	}
	out := make([]string, 0, len(plan.Dropped))
	for _, d := range plan.Dropped {
		out = append(out, d.Reason)
	}
	return out
}

// ftsRows 直连枚举 memories_fts 的 (id, version) → body，用于 D13 的行级断言。
func ftsRows(t *testing.T, raw *sql.DB) map[string]map[int]string {
	t.Helper()
	rows, err := raw.Query(`SELECT id, version, body FROM memories_fts`)
	if err != nil {
		t.Fatalf("query memories_fts: %v", err)
	}
	defer rows.Close()

	out := make(map[string]map[int]string)
	for rows.Next() {
		var id, body string
		var version int
		if err := rows.Scan(&id, &version, &body); err != nil {
			t.Fatalf("scan memories_fts: %v", err)
		}
		if out[id] == nil {
			out[id] = make(map[int]string)
		}
		out[id][version] = body
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate memories_fts: %v", err)
	}
	return out
}

// ---------------------------------------------------------------------------
// 故障注入 Port 包装器
// ---------------------------------------------------------------------------

var errInjected = errors.New("injected fault")

// failLexical 让 Index 失败。
//
// afterDelegate=true 时先真实写入再报错：模拟"写进去了但上层认为失败"，
// 用来区分"事务回滚"与"根本没写"——两种情形都必须让主表也不可见。
type failLexical struct {
	inner         store.LexicalIndex
	afterDelegate bool
}

func (f *failLexical) Index(ctx context.Context, docs []store.Document) error {
	if f.afterDelegate {
		if err := f.inner.Index(ctx, docs); err != nil {
			return err
		}
	}
	return errInjected
}

func (f *failLexical) Delete(ctx context.Context, keys []store.DocumentKey) error {
	return f.inner.Delete(ctx, keys)
}

func (f *failLexical) Search(ctx context.Context, q store.LexicalQuery) ([]store.Hit, error) {
	return f.inner.Search(ctx, q)
}

// failGraphNode 让第 failAt 个（1-based）PutNode 失败，之前的真实写入。
type failGraphNode struct {
	inner  store.GraphStore
	failAt int
	n      int
}

func (f *failGraphNode) PutNode(ctx context.Context, n store.Node) error {
	f.n++
	if f.n == f.failAt {
		return errInjected
	}
	return f.inner.PutNode(ctx, n)
}

func (f *failGraphNode) GetNode(ctx context.Context, id string) (store.Node, error) {
	return f.inner.GetNode(ctx, id)
}

func (f *failGraphNode) PutEdge(ctx context.Context, e store.Edge) error {
	return f.inner.PutEdge(ctx, e)
}

func (f *failGraphNode) Neighbors(ctx context.Context, q store.NeighborQuery) ([]store.EdgeHop, error) {
	return f.inner.Neighbors(ctx, q)
}

// failNeighborsFor 只对指定起点节点的邻域查询报错，其余委托真实实现
// （用于观察"同一 lane 内部分锚点失败"是否连累成功的锚点）。
type failNeighborsFor struct {
	inner  store.GraphStore
	nodeID string
}

func (f *failNeighborsFor) PutNode(ctx context.Context, n store.Node) error {
	return f.inner.PutNode(ctx, n)
}

func (f *failNeighborsFor) GetNode(ctx context.Context, id string) (store.Node, error) {
	return f.inner.GetNode(ctx, id)
}

func (f *failNeighborsFor) PutEdge(ctx context.Context, e store.Edge) error {
	return f.inner.PutEdge(ctx, e)
}

func (f *failNeighborsFor) Neighbors(ctx context.Context, q store.NeighborQuery) ([]store.EdgeHop, error) {
	if q.NodeID == f.nodeID {
		return nil, errInjected
	}
	return f.inner.Neighbors(ctx, q)
}
