package server_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tim5wang/gmem/ops"
	"github.com/tim5wang/gmem/recall"
	"github.com/tim5wang/gmem/server"
	"github.com/tim5wang/gmem/sqlite"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// P4-a 测试基座
//
// 为什么用真实 sqlite 后端而不是内存假实现：本卡交付的是**数据接口**，
// 它要复用的关键语义（Neighbors 的递归 CTE、已失效边的可见性、OntologyReader）
// 全部在后端里。用假后端测出来的"绿"不能证明线上那条路径能跑。
//
// server 包本身**不** import sqlite（server/guard_test.go 钉死）；
// 测试代码 import 它属于装配，与 cmd/gmem 同性质。
// ===========================================================================

type harness struct {
	t     *testing.T
	db    *sqlite.DB
	ports store.Ports
	srv   *server.Server
	http  *httptest.Server
	rec   *recall.Recaller
	maint *ops.Maintainer
}

// newHarness 起一个内存库 + 完整装配的 HTTP 服务。
func newHarness(t *testing.T, cfgs ...recall.Config) *harness {
	t.Helper()
	db, err := sqlite.Open(context.Background(), sqlite.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ports := db.Ports()
	runner, err := ops.NewRunner(ports, db.OpsBackend())
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	cfg := recall.Config{}
	if len(cfgs) > 0 {
		cfg = cfgs[0]
	}
	rec := recall.New(ports, cfg, nil)
	maint := ops.NewMaintainer(runner)
	srv, err := server.New(server.Options{
		Ports: ports, Recaller: rec, Maintainer: maint, Catalog: db.OpsBackend(),
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	hs := &harness{t: t, db: db, ports: ports, srv: srv, rec: rec, maint: maint}
	hs.http = httptest.NewServer(srv.Handler())
	t.Cleanup(hs.http.Close)
	return hs
}

// raw 发一个任意方法的请求并返回状态码与原始字节。
func (h *harness) raw(method, path, body string) (int, []byte) {
	h.t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, h.http.URL+path, rdr)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, buf
}

func (h *harness) get(path string) (int, []byte) { return h.raw(http.MethodGet, path, "") }

// getGraph 请求 /api/graph 并解析成 GraphResponse（同时返回原始字节）。
func (h *harness) getGraph(query string) (int, server.GraphResponse, []byte) {
	h.t.Helper()
	status, body := h.get("/api/graph" + query)
	var resp server.GraphResponse
	if status == http.StatusOK {
		if err := json.Unmarshal(body, &resp); err != nil {
			h.t.Fatalf("unmarshal graph: %v (body=%s)", err, body)
		}
	}
	return status, resp, body
}

func (h *harness) postJSON(path, body string) (int, []byte) {
	h.t.Helper()
	return h.raw(http.MethodPost, path, body)
}

// getEntity 请求 /api/entity/{id}。
func (h *harness) getEntity(id, query string) (int, server.EntityCard, []byte) {
	h.t.Helper()
	status, body := h.get("/api/entity/" + id + query)
	var card server.EntityCard
	if status == http.StatusOK {
		if err := json.Unmarshal(body, &card); err != nil {
			h.t.Fatalf("unmarshal entity: %v (body=%s)", err, body)
		}
	}
	return status, card, body
}

// ---------------------------------------------------------------------------
// 造数据
// ---------------------------------------------------------------------------

func (h *harness) node(id, name string, scope store.Scope, concept string) {
	h.t.Helper()
	err := h.ports.Graph.PutNode(context.Background(), store.Node{
		ID: id, Scope: scope, CanonicalName: name, ConceptID: concept, PropsJSON: "{}",
	})
	if err != nil {
		h.t.Fatalf("PutNode(%s): %v", id, err)
	}
}

// nodeWithSource 造一个带来源 episode 的节点（实体卡的溯源头）。
func (h *harness) nodeWithSource(id, name string, scope store.Scope, concept, episodeID string) {
	h.t.Helper()
	err := h.ports.Graph.PutNode(context.Background(), store.Node{
		ID: id, Scope: scope, CanonicalName: name, ConceptID: concept,
		SourceEpisode: episodeID, PropsJSON: "{}",
	})
	if err != nil {
		h.t.Fatalf("PutNode(%s): %v", id, err)
	}
}

func (h *harness) edge(id, from, to string, rel store.Relation, weight float64, tValid, tInvalid int64) {
	h.t.Helper()
	err := h.ports.Graph.PutEdge(context.Background(), store.Edge{
		ID: id, FromID: from, ToID: to, Relation: rel, Weight: weight,
		TValid: tValid, TInvalid: tInvalid, TCreated: tValid,
	})
	if err != nil {
		h.t.Fatalf("PutEdge(%s): %v", id, err)
	}
}

func (h *harness) concept(id, name string, status store.ConceptStatus, scope store.Scope, origin store.ConceptOrigin) {
	h.t.Helper()
	err := h.ports.Store.PutConcept(context.Background(), store.Concept{
		ID: id, Name: name, Status: status, Scope: scope, Origin: origin, CreatedAt: 1000,
	})
	if err != nil {
		h.t.Fatalf("PutConcept(%s): %v", id, err)
	}
}

func (h *harness) episode(id string, at int64, content string) {
	h.t.Helper()
	err := h.ports.Store.PutEpisode(context.Background(), store.Episode{
		ID: id, Scope: store.ScopeUser, Content: content, At: at, ContentHash: "h-" + id,
	})
	if err != nil {
		h.t.Fatalf("PutEpisode(%s): %v", id, err)
	}
}

// alias 登记词典项（q 模式靠它把关键词对齐到实体锚点）。
func (h *harness) alias(surface, nodeID string) {
	h.t.Helper()
	err := h.ports.Store.PutAlias(context.Background(), store.Alias{
		Surface: surface, NodeID: nodeID, Source: "test", Confidence: 1,
	})
	if err != nil {
		h.t.Fatalf("PutAlias(%s): %v", surface, err)
	}
}

// nodeIDs 汇总返回的节点 ID。
func nodeIDs(resp server.GraphResponse) []string {
	out := make([]string, 0, len(resp.Nodes))
	for _, n := range resp.Nodes {
		out = append(out, n.ID)
	}
	return out
}

func hasID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// recallConfigWithOwnsTransitive 把 owns 从"非传递 1 跳"改成"传递 3 跳"。
//
// 用它证明 /api/graph 的跳数**读的是 recall 的配置表**而不是本地常量：
// 换表即换策略。注意先取默认表再覆盖，不要手写整张表——那样就是在测试里
// 复制了一份跳数规则，恰好是这张用例要反对的东西。
func recallConfigWithOwnsTransitive() recall.Config {
	cfg := recall.Config{}
	rules := recall.DefaultRelationRules()
	for i := range rules {
		if rules[i].Relation == store.RelationOwns {
			rules[i].Transitive = true
			rules[i].MaxHops = 3
		}
	}
	cfg.RelationRules = rules
	return cfg
}

// findNode 按 ID 找返回体里的节点。
func findNode(resp server.GraphResponse, id string) (server.Node, bool) {
	for _, n := range resp.Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return server.Node{}, false
}

// findEdge 按 ID 找返回体里的边。
func findEdge(resp server.GraphResponse, id string) (server.Edge, bool) {
	for _, e := range resp.Edges {
		if e.ID == id {
			return e, true
		}
	}
	return server.Edge{}, false
}
