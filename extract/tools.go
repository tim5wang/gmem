package extract

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// tool-calling 的三个只读工具（docs/TD.md §3.2 D10）
//
// 工具面刻意极小，且**没有任何写操作**：模型不能借工具改数据，
// 因此"抽取一次 = 最多一次多读循环"这个成本上界成立。
//
// hops 硬上限 1：让模型不能自己发起多跳遍历，否则 G5 的墙钟超时拦不住
// （一次 3 跳遍历在稠密图上可以吃掉整个预算）。
// ===========================================================================

// HardMaxHops 是 neighbors 的**硬上限**，不随配置放大。
//
// 它之所以是常量而不是配置项：配置项意味着"某个部署可以放开"，
// 而放开它就等于把多跳遍历的控制权交给模型，G5 就不再是硬保证。
const HardMaxHops = 1

// defaultSearchLimit 是 search_memories 的默认条数上限（§3.2 表：5）。
//
// 它是 G3 归一化的依据之一：k 缺省、k=0、k>5 在工具侧都收敛到同一个生效值，
// 因此它们是**同一条查询**（M2），不能因为字面参数不同就被当成新查询。
const defaultSearchLimit = 5

// 工具名常量。
const (
	ToolLookupEntity   = "lookup_entity"
	ToolNeighbors      = "neighbors"
	ToolSearchMemories = "search_memories"
)

// ToolSpecs 返回三个只读工具的声明（供 LLMRequest.Tools）。
//
// 参数 schema 用 JSON 表达，宿主可直接转给自家模型的 tool 定义。
func ToolSpecs() []ToolSpec {
	return []ToolSpec{
		{
			Name:        ToolLookupEntity,
			Description: "按名称（可加概念过滤）查当前 scope 已有实体候选。只读，最多 10 条。",
			Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "name":    {"type": "string", "description": "实体表面形式或片段"},
    "concept": {"type": "string", "description": "可选概念 id 过滤"}
  },
  "required": ["name"],
  "additionalProperties": false
}`),
		},
		{
			Name:        ToolNeighbors,
			Description: "查某节点的邻接边（hops 硬上限 1，只读，最多 30 条，含已失效边与有效期）。",
			Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "node_id":  {"type": "string"},
    "relation": {"type": "string", "description": "可选关系类型过滤"},
    "hops":     {"type": "integer", "description": "跳数，<=1", "minimum": 1, "maximum": 1}
  },
  "required": ["node_id"],
  "additionalProperties": false
}`),
		},
		{
			Name:        ToolSearchMemories,
			Description: "按关键词检索当前 scope 的相关记忆摘要。只读，最多 5 条。",
			Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {"type": "string"},
    "k":     {"type": "integer", "description": "条数，<=5", "minimum": 1, "maximum": 5}
  },
  "required": ["query"],
  "additionalProperties": false
}`),
		},
	}
}

// toolResult 是工具执行的统一返回形状。
//
// Truncated 是**契约的一部分**：模型必须知道"你看到的不是全部"，
// 否则它会把截断当成"没有更多"，从而漏抽关系。
type toolResult struct {
	Tool      string           `json:"tool"`
	Items     []map[string]any `json:"items"`
	Total     int              `json:"total"`
	Truncated bool             `json:"truncated"`
}

// resultIDs 抽取本次结果里各行稳定标识（node/edge/memory）。
//
// G4 的"无进展"必须是"无**新增**"（M1）：只看 len(Items) 的话，模型把同一条
// 查询换个措辞（`vim` / `vi` / `v`）反复命中同一行，noProgress 永远归零、G4 永不触发。
// 这里只返回**已存在**于 Items 里的 id；缺失的整行不计入（宁可偏保守）。
func (r toolResult) resultIDs() []string {
	out := make([]string, 0, len(r.Items))
	for _, it := range r.Items {
		for _, k := range []string{"node_id", "edge_id", "memory_id"} {
			if v, ok := it[k].(string); ok && v != "" {
				out = append(out, k+":"+v)
				break
			}
		}
	}
	return out
}

// toolRunner 执行只读工具。它只持有 ports，不持有 Writer——
// 结构上排除"工具里写库"的可能性。
type toolRunner struct {
	ports store.Ports
	cfg   Config
	scope store.Scope
}

func newToolRunner(ports store.Ports, cfg Config, scope store.Scope) *toolRunner {
	return &toolRunner{ports: ports, cfg: cfg.withDefaults(), scope: scope}
}

// run 执行一次工具调用。未知工具名是**错误**（不是静默空结果）：
// 静默空结果会让模型以为"确实没有"，而真实原因是它编了个工具名。
func (t *toolRunner) run(ctx context.Context, call ToolCall) (toolResult, error) {
	// 空工具面时任何调用都不可执行：强制终稿以后的回合走这里。
	switch call.Name {
	case ToolLookupEntity:
		return t.lookupEntity(ctx, call.Arguments)
	case ToolNeighbors:
		return t.neighbors(ctx, call.Arguments)
	case ToolSearchMemories:
		return t.searchMemories(ctx, call.Arguments)
	default:
		return toolResult{}, fmt.Errorf("未知工具 %q（工具面固定为三个只读工具）", call.Name)
	}
}

type lookupArgs struct {
	Name    string `json:"name"`
	Concept string `json:"concept"`
}

func (t *toolRunner) lookupEntity(ctx context.Context, raw json.RawMessage) (toolResult, error) {
	var a lookupArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return toolResult{}, fmt.Errorf("lookup_entity 参数解析失败: %w", err)
	}
	name := strings.TrimSpace(a.Name)
	if name == "" {
		return toolResult{}, fmt.Errorf("lookup_entity: 空 name")
	}
	res := toolResult{Tool: ToolLookupEntity, Items: []map[string]any{}}

	or, ok := t.ports.Store.(store.OntologyReader)
	if !ok || or == nil {
		// 没有本体枚举端口：显式空结果 + 错误，不假装"确实没有"。
		return res, fmt.Errorf("lookup_entity: 后端未实现 OntologyReader，无法枚举词典")
	}
	aliases, err := or.ListAliases(ctx)
	if err != nil {
		return res, fmt.Errorf("lookup_entity: list aliases: %w", err)
	}

	needle := strings.ToLower(name)
	type hit struct {
		nodeID  string
		surfac  string
		conf    float64
		concept string
	}
	var hits []hit
	seen := map[string]struct{}{}
	for _, al := range aliases {
		s := strings.ToLower(strings.TrimSpace(al.Surface))
		if s == "" {
			continue
		}
		// 双向包含：模型可能给全名也可能给片段。
		if !strings.Contains(s, needle) && !strings.Contains(needle, s) {
			continue
		}
		if _, dup := seen[al.NodeID]; dup {
			continue
		}
		// 概念过滤需要节点的 concept_id；GetNode 是唯一只读入口。
		var conceptID string
		if n, err := t.ports.Graph.GetNode(ctx, al.NodeID); err == nil {
			conceptID = n.ConceptID
			if n.Scope != "" && n.Scope != t.scope {
				continue // 范围隔离：跨 scope 的候选绝不返回
			}
		}
		if a.Concept != "" && conceptID != a.Concept {
			continue
		}
		seen[al.NodeID] = struct{}{}
		hits = append(hits, hit{nodeID: al.NodeID, surfac: al.Surface, conf: al.Confidence, concept: conceptID})
	}
	// 确定性：置信度高者优先，再按节点 id。
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].conf != hits[j].conf {
			return hits[i].conf > hits[j].conf
		}
		return hits[i].nodeID < hits[j].nodeID
	})
	res.Total = len(hits)
	limit := t.cfg.LookupLimit
	for i, h := range hits {
		if i >= limit {
			res.Truncated = true
			break
		}
		item := map[string]any{
			"node_id":    h.nodeID,
			"surface":    h.surfac,
			"confidence": h.conf,
		}
		if h.concept != "" {
			item["concept_id"] = h.concept
		}
		res.Items = append(res.Items, item)
	}
	return res, nil
}

type neighborsArgs struct {
	NodeID   string `json:"node_id"`
	Relation string `json:"relation"`
	Hops     int    `json:"hops"`
}

func (t *toolRunner) neighbors(ctx context.Context, raw json.RawMessage) (toolResult, error) {
	var a neighborsArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return toolResult{}, fmt.Errorf("neighbors 参数解析失败: %w", err)
	}
	if strings.TrimSpace(a.NodeID) == "" {
		return toolResult{}, fmt.Errorf("neighbors: 空 node_id")
	}
	// hops 硬上限 1（HardMaxHops），并且**向上裁剪**：模型要 3 也只能拿到 1，
	// 结果里用 truncated 提示它被裁剪过。
	hops := a.Hops
	if hops <= 0 {
		hops = HardMaxHops
	}
	clamped := false
	if hops > HardMaxHops {
		hops = HardMaxHops
		clamped = true
	}
	q := store.NeighborQuery{NodeID: a.NodeID, Scope: t.scope, Hops: hops}
	if rel := strings.TrimSpace(a.Relation); rel != "" {
		q.Relations = []store.Relation{store.Relation(rel)}
	}
	edges, err := t.ports.Graph.Neighbors(ctx, q)
	if err != nil {
		return toolResult{Tool: ToolNeighbors, Items: []map[string]any{}}, fmt.Errorf("neighbors: %w", err)
	}
	res := toolResult{Tool: ToolNeighbors, Items: []map[string]any{}, Total: len(edges)}
	limit := t.cfg.NeighborsLimit
	for i, hop := range edges {
		if i >= limit {
			res.Truncated = true
			break
		}
		res.Items = append(res.Items, map[string]any{
			"edge_id":   hop.Edge.ID,
			"from_id":   hop.Edge.FromID,
			"to_id":     hop.Edge.ToID,
			"relation":  string(hop.Edge.Relation),
			"depth":     hop.Depth,
			"weight":    hop.Edge.Weight,
			"t_valid":   hop.Edge.TValid,
			"t_invalid": hop.Edge.TInvalid,
		})
	}
	if clamped {
		// 裁剪不是错误，但必须让模型知道（截断标记属于契约）。
		res.Truncated = true
	}
	return res, nil
}

type searchArgs struct {
	Query string `json:"query"`
	K     int    `json:"k"`
}

func (t *toolRunner) searchMemories(ctx context.Context, raw json.RawMessage) (toolResult, error) {
	var a searchArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return toolResult{}, fmt.Errorf("search_memories 参数解析失败: %w", err)
	}
	query := strings.TrimSpace(a.Query)
	if query == "" {
		return toolResult{}, fmt.Errorf("search_memories: 空 query")
	}
	// k 夹取到 [1, SearchLimit]：上限 5 是**契约**，不是建议。
	limit := t.cfg.SearchLimit
	k := a.K
	clamped := false
	if k <= 0 || k > limit {
		if k > limit {
			clamped = true
		}
		k = limit
	}
	hits, err := t.ports.Lexical.Search(ctx, store.LexicalQuery{
		Query: query, Scope: t.scope, Limit: k + 1,
	})
	if err != nil {
		return toolResult{Tool: ToolSearchMemories, Items: []map[string]any{}}, fmt.Errorf("search_memories: %w", err)
	}
	res := toolResult{Tool: ToolSearchMemories, Items: []map[string]any{}, Total: len(hits)}
	// 内部按 k+1 查询：多出 1 条就说明“还有更多”，必须给出截断标记。
	//
	// 为什么不能只靠 k 被夹紧：k≤SearchLimit 时后端只返回 k 条，工具无从知道
	// “还有更多”，模型会把“只给了 k 条”理解成“只有 k 条相关记忆”，
	// 从而漏掉已有的上下文证据（表现为抽取质量下降，不报错）。
	if len(hits) > k {
		hits = hits[:k]
		res.Truncated = true
		res.Total = k + 1 // 命中的下界：至少还有 1 条没返回
	}
	for _, h := range hits {
		item := map[string]any{"memory_id": h.ID, "version": h.Version, "score": h.Score}
		if m, err := t.ports.Store.GetMemory(ctx, h.ID); err == nil {
			item["content"] = truncateRunes(m.Content, 200)
			item["kind"] = string(m.Kind)
		}
		res.Items = append(res.Items, item)
	}
	if clamped {
		res.Truncated = true
	}
	return res, nil
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
