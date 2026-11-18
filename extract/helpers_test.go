package extract

import (
	"context"
	"hash/fnv"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tim5wang/gmem/bigram"
	"github.com/tim5wang/gmem/sqlite"
	"github.com/tim5wang/gmem/store"
)

// 本包测试分两层：
//
//   - 白盒用例（本目录 *_test.go，package extract）：直接对准七类规则、五条门禁、
//     三区阈值等内部契约——变异验证需要"拆掉某条规则就让对应用例变红"，因此必须
//     能单独触达这些单元。
//   - 黑盒行为（同样在本目录）：通过 Pipeline + sqlite 真实后端观察落库结果，
//     尤其是"写入必须走 recall.Writer"这条只能从 FTS 能否搜到来验证的契约。

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

func openFileDB(t *testing.T) *sqlite.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "extract.db")
	d, err := sqlite.Open(context.Background(), sqlite.Config{Path: path})
	if err != nil {
		t.Fatalf("sqlite.Open(file): %v", err)
	}
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("close db: %v", err)
		}
	})
	return d
}

// seedOntology 写入概念 + 节点 + 词典项（L0 词典规则与消歧候选的来源）。
func seedOntology(t *testing.T, d *sqlite.DB) {
	t.Helper()
	ctx := context.Background()
	if err := d.Store().PutConcept(ctx, store.Concept{
		ID: "c-editor", Name: "编辑器", Status: store.ConceptCertified,
		Definition: "用于编写代码的文本编辑程序",
	}); err != nil {
		t.Fatalf("PutConcept: %v", err)
	}
	if err := d.Graph().PutNode(ctx, store.Node{
		ID: "n-vim", Scope: store.ScopeUser, ConceptID: "c-editor",
		CanonicalName: "vim", PropsJSON: `{"definition":"一个模式化文本编辑器"}`,
	}); err != nil {
		t.Fatalf("PutNode: %v", err)
	}
	if err := d.Store().PutAlias(ctx, store.Alias{
		Surface: "vim", NodeID: "n-vim", Source: "dict", Confidence: 0.99,
	}); err != nil {
		t.Fatalf("PutAlias: %v", err)
	}
}

func newTestPipeline(t *testing.T, d *sqlite.DB, cfg Config, opts PipelineOptions) *Pipeline {
	t.Helper()
	return NewPipeline(d.Ports(), cfg, opts)
}

// mustRun 跑一次管线并要求无错误。
func mustRun(t *testing.T, p *Pipeline, ep RawEpisode) PipelineResult {
	t.Helper()
	res, err := p.Run(context.Background(), ep)
	if err != nil {
		t.Fatalf("pipeline.Run(%q): %v", ep.ID, err)
	}
	return res
}

// bigramEmbedder 是**确定性、文本相关**的测试 embedder。
//
// 为什么不能用"常量向量"替身：常量向量会让任意两个名称的 cosine 都等于 1，
// 于是"无关名称"也会被判进灰区，把三区用例变成永远灰区的假绿。
// 这里把 bigram token 哈希进固定维度，相同文本 cosine=1、无共享 token cosine=0，
// 语义上接近真实 embedder 的判别力，且不依赖任何模型。
func bigramEmbedder(dim int) Embedder {
	return EmbedderFunc(func(ctx context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i, t := range texts {
			v := make([]float32, dim)
			for _, tok := range bigram.Tokens(strings.ToLower(t)) {
				h := fnv.New32a()
				_, _ = h.Write([]byte(tok))
				v[int(h.Sum32()%uint32(dim))] += 1
			}
			out[i] = v
		}
		return out, nil
	})
}

// seedEdge 在库里种一条边（结构一致性信号的来源）。
func seedEdge(t *testing.T, d *sqlite.DB, from, to string, rel store.Relation) {
	t.Helper()
	ctx := context.Background()
	for _, id := range []string{from, to} {
		if _, err := d.Graph().GetNode(ctx, id); err == nil {
			continue
		}
		if err := d.Graph().PutNode(ctx, store.Node{
			ID: id, Scope: store.ScopeUser, CanonicalName: id,
		}); err != nil {
			t.Fatalf("PutNode(%q): %v", id, err)
		}
	}
	if err := d.Graph().PutEdge(ctx, store.Edge{
		ID:     "e-seed-" + string(rel) + "-" + from + "-" + to,
		FromID: from, ToID: to, Relation: rel, Weight: 1, TCreated: 1,
	}); err != nil {
		t.Fatalf("PutEdge: %v", err)
	}
}
