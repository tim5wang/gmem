package p2verify

import (
	"context"
	"strings"
	"testing"

	"github.com/tim5wang/gmem/extract"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// P2 修复的独立回归用例（外部视角，只看公共 API 与库行）
//
// 与 findings_test.go 的门禁用例不同：这些缺陷已经修好，因此**不再挂门禁**，
// 默认套件里就必须绿。它们的价值是"改回去就变红"。
// ===========================================================================

// TestAutoMergeEdgeLandsOnExistingNodeWhenIDDecoupled 是 B-1 的正式回归。
//
// # 判据（leader 点名要求的那一条）
//
// 当节点的 **id 与 canonical_name 脱钩**（真实库里必然发生：Judge(Same=true)
// 写 alias、P3 的 Merge 保留旧 id、宿主导入）时，一段"实体 + 关系"的文本
// 走 auto_merge 必须满足：
//  1. Run 返回 **nil**（不得因 FK 787 把整笔图写入回滚）；
//  2. 关系边正确落到**既有节点**（端点解析到 d.NodeID，而不是 NodeID(scope,surface)）；
//  3. 边端点不得指向一个不存在的节点（那正是 787 的来源）。
//
// 变异验证：把 addEdge 改回 `NodeID(scope, surface)` 现算，本用例必须变红（Run 报错）。
func TestAutoMergeEdgeLandsOnExistingNodeWhenIDDecoupled(t *testing.T) {
	d, raw := openFileDB(t)
	ctx := context.Background()

	// 既有节点：id 与 canonical_name 刻意脱钩。
	const canon = "缓存服务"
	cacheID := "n-legacy-cache-77"
	if cacheID == extract.NodeID(store.ScopeUser, canon) {
		t.Fatalf("用例前提失效：候选 id 应脱钩")
	}
	if err := d.Graph().PutNode(ctx, store.Node{
		ID: cacheID, Scope: store.ScopeUser, CanonicalName: canon,
	}); err != nil {
		t.Fatalf("PutNode: %v", err)
	}
	seedAlias(t, d, canon, cacheID, 0.99)
	seedNode(t, d, "n-redis2", "Redis")
	seedAlias(t, d, "Redis", "n-redis2", 0.99)
	// 结构一致性信号：候选节点已有一条 depends_on 边（使 s 达 θ_hi）。
	seedNode(t, d, "n-store2", "存储")
	seedEdge(t, d, "e-seed-dep2", cacheID, "n-store2", store.RelationDependsOn)

	p := extract.NewPipeline(d.Ports(), extract.Config{}, extract.PipelineOptions{})
	res, err := p.Run(ctx, extract.RawEpisode{
		ID: "e-decoupled", Scope: store.ScopeUser,
		Content: "Redis 依赖 缓存服务 做会话存储。",
	})
	// 判据 1：不得整笔回滚。
	if err != nil {
		t.Fatalf("id 与 canonical_name 脱钩时 Run 必须返回 nil（否则就是 FK 787 回滚）：%v", err)
	}

	// 前提：这段文本确实走了 auto_merge（否则本用例没测到目标路径）。
	var merged bool
	for _, z := range res.Disambiguation {
		if z.Zone == extract.ZoneAutoMerge && z.Mention == canon {
			merged = true
			if z.NodeID != cacheID {
				t.Errorf("auto_merge 应指向既有节点 %q，实际 %q", cacheID, z.NodeID)
			}
		}
	}
	if !merged {
		t.Fatalf("用例前提失效：%q 应判为 auto_merge，实际 %+v", canon, res.Disambiguation)
	}

	// 判据 2：新边必须落在既有节点上（关系终点 = 缓存服务的脱钩 id）。
	//
	// 注意断言的粒度：**终点**必须是既有节点 cacheID（这正是缺陷所在：
	// 旧代码会指向 NodeID(scope,"缓存服务")——那里没有行）。
	// 起点由消歧决定（本例 "Redis" 判 new，于是本次为它建了新节点），因此只要求
	// 起点是一个真实存在的节点，不写死 id。
	if n := countRows(t, raw,
		`SELECT COUNT(1) FROM edges WHERE relation = ? AND to_id = ?`,
		string(store.RelationDependsOn), cacheID); n != 1 {
		t.Errorf("depends_on 边的终点应为既有节点 %q，实际 %d 行", cacheID, n)
	}
	// 判据 3：不得写出指向不存在节点的边（这就是 787 的根因）。
	if n := countRows(t, raw,
		`SELECT COUNT(1) FROM edges e
		 WHERE NOT EXISTS (SELECT 1 FROM nodes n WHERE n.id = e.from_id)
		    OR NOT EXISTS (SELECT 1 FROM nodes n WHERE n.id = e.to_id)`); n != 0 {
		t.Errorf("不应存在悬空端点的边，实际 %d 条", n)
	}
	// 也不应凭空多出一个由表面形式派生的同义节点。
	if n := countRows(t, raw, `SELECT COUNT(1) FROM nodes WHERE id = ?`,
		extract.NodeID(store.ScopeUser, canon)); n != 0 {
		t.Errorf("auto_merge 不得新建表面形式派生的节点，实际 %d 行", n)
	}
}

// extractOnly 是一个只返回固定实体/关系的 L2（不经 LLM）。
//
// 为什么需要它：要确定性地构造“关系端点未参与本次消歧”的形态。
// LLM 路径会在 applyPayload 里把关系端点限制在“结果或已知实体”内，
// 而这里的自定义 Extractor 直接给出实体/关系，绕过那一层校验，
// 从而精确命中 writeEpisode 的端点解析分支。
type extractOnly struct {
	entities  []extract.ExtractedEntity
	relations []extract.ExtractedRelation
}

func (e extractOnly) Extract(context.Context, extract.ExtractRequest) (extract.ExtractResult, error) {
	return extract.ExtractResult{Entities: e.entities, Relations: e.relations}, nil
}

// TestMissingEdgeEndpointSkippedNotRolledBack 覆盖“端点解析不到时跳过该边”这一半：
//
// 一条边的端点未能解析到节点时，**跳过这条边并记 Warnings**，而不是让整笔写入失败。
// 判据：Run 返回 nil、Warnings 里可见跳过原因、其余节点/别名/记忆仍然落库、
// 且库里没有任何悬空端点的边。
//
// 构造（确定性，不靠巧合）：L2 返回一条关系，其**终点**是库里已有、
// 但**未参与本次消歧**的实体（不在正文里 → 不进 mentions → resolved 里没有它）。
// 这种形态在真实语料里很常见：模型把“已知实体清单”里的名字用作关系端点，
// 而那个名字本身没有在本文里被再次提到。
//
// 变异验证：把 addEdge 里的 `if !okF || !okT` 跳过分支去掉（直接走到 EdgeID/Validate），
// 本用例必须以“空端点”报错变红。
func TestMissingEdgeEndpointSkippedNotRolledBack(t *testing.T) {
	d, raw := openFileDB(t)
	ctx := context.Background()
	// 库里已有“旧服务”节点，但它不在正文里 → 本次不会被消歧。
	seedNode(t, d, "n-legacy-old", "旧服务")

	ext := extractOnly{
		entities: []extract.ExtractedEntity{{Name: "Redis", Confidence: 0.9}},
		relations: []extract.ExtractedRelation{{
			From: "Redis", To: "旧服务",
			Relation: store.RelationDependsOn, Confidence: 0.8,
		}},
	}
	p := extract.NewPipeline(d.Ports(), extract.Config{}, extract.PipelineOptions{Extractor: ext})
	res, err := p.Run(ctx, extract.RawEpisode{
		ID: "e-missing", Scope: store.ScopeUser,
		Content: "Redis 是本次讨论的缓存层组件。",
	})
	// 判据 1：不得让整笔写入失败。
	if err != nil {
		t.Fatalf("端点未解析不得让整笔写入失败：%v", err)
	}
	// 判据 2：其余产物必须落库（一条边不值得丢掉整段 episode）。
	if len(res.Writes) == 0 {
		t.Error("即使跳过了坏边，其余节点/别名/记忆仍必须落库")
	}
	// 判据 3：被跳过的那条边不得落地（否则就是 FK 787 或悬空行）。
	if n := countRows(t, raw,
		`SELECT COUNT(1) FROM edges WHERE relation = ? AND to_id = ?`,
		string(store.RelationDependsOn), "n-legacy-old"); n != 0 {
		t.Errorf("端点未解析的边不得落库，实际 %d 行", n)
	}
	if n := countRows(t, raw,
		`SELECT COUNT(1) FROM edges e
		 WHERE NOT EXISTS (SELECT 1 FROM nodes n WHERE n.id = e.from_id)
		    OR NOT EXISTS (SELECT 1 FROM nodes n WHERE n.id = e.to_id)`); n != 0 {
		t.Errorf("不得写出悬空端点的边，实际 %d 条", n)
	}
	// 判据 4：跳过必须可见（绝不静默）。
	joined := strings.Join(append(append([]string{}, res.Warnings...), res.Degraded...), " ")
	if !strings.Contains(joined, "已跳过") {
		t.Errorf("跳过边必须在 Warnings/Degraded 里可见，实际 warnings=%v degraded=%v",
			res.Warnings, res.Degraded)
	}
	// 判据 5：另一端的实体仍然正常建节点（不是“一条坏边拖垮一整批”）。
	if n := countRows(t, raw, `SELECT COUNT(1) FROM nodes WHERE canonical_name = ?`, "Redis"); n != 1 {
		t.Errorf("Redis 节点应正常落库，实际 %d 行", n)
	}
}
