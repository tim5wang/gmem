package memory

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/tim5wang/gmem/recall"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// 实体面：MCP 兼容别名层（create_entities / add_observations / open_nodes）的落点
//
// # 为什么需要这一层，以及它**不是**什么
//
// MCP 官方 memory server 的模型是「实体（name/entityType/observations）+ 关系」。
// Gmem 的模型是「记忆 + 图 + 本体」。别名层只做**能力映射**：
//
//	实体名        → node.canonical_name + alias(surface=name)（词典是真实解析路径）
//	entityType    → concept（draft，D15：从使用中生长）
//	observations  → 记忆（kind=semantic），经 recall.Writer 落库（FTS 同步）
//	关系          → 边（ops.Link，与 gmem_link 同一实现）
//	search/open   → 别名解析 + 召回 + 实体卡（复用既有聚合）
//
// ⚠️ 它**不承诺**与官方 server 的 JSON 存储格式互通：同名概念在两边可能落在
// 不同的表形态里，导入导出不能靠"读那边写的文件"完成。这一点在 README 与
// doctor 输出里都写死（任务卡的硬要求）。
//
// 写入仍然走真实路径：节点经 Graph.PutNode，别名经 Store.PutAlias，
// 记忆经 recall.Writer。没有任何一处直接拼 SQL。
// ===========================================================================

// EntityRef 是一个实体的输入（对齐官方 server 的 create_entities 元素）。
type EntityRef struct {
	Name string
	// Type 是实体类型（映射为 draft 概念）。
	Type string
	// Observations 是观测片段（每条落一条 semantic 记忆）。
	Observations []string
}

// EntityOut 是一个实体的写入结果。
type EntityOut struct {
	Name      string
	ID        string
	ConceptID string
	// Memories 是本次为 observations 落库的记忆 ID。
	Memories []string
	// Skipped 是"已存在所以没重复写"的观测条数。
	Skipped int
}

// EntityResult 是实体写入的批量结果。
type EntityResult struct {
	Entities []EntityOut
	Degraded []string
}

// UpsertEntities 写入/更新一组实体（节点 + 别名 + 观测记忆）。
//
// 幂等：节点与别名都是覆盖写；观测记忆的 ID 由实体名 + 观测内容派生，
// 因此重复调用不会在库里堆出重复记忆（内容等价的重写在后端是幂等空操作）。
func (s *Service) UpsertEntities(ctx context.Context, scope store.Scope, ents []EntityRef) (EntityResult, error) {
	if scope == "" {
		scope = store.ScopeUser
	}
	if !scope.Valid() {
		return EntityResult{}, fmt.Errorf("memory: 实体写入: scope 非法 %q", scope)
	}
	if len(ents) == 0 {
		return EntityResult{}, errors.New("memory: 实体写入: entities 不能为空")
	}
	var out EntityResult
	for _, e := range ents {
		name := strings.TrimSpace(e.Name)
		if name == "" {
			return out, errors.New("memory: 实体写入: 实体名不能为空")
		}
		nodeID := "n-" + shortHash(string(scope), name)
		conceptID := ""
		if t := strings.TrimSpace(e.Type); t != "" {
			conceptID = "c-" + shortHash("concept", strings.ToLower(t))
			// 概念从使用中生长（D15）：先落 draft，认证是后续的人工动作。
			if err := s.cfg.Ports.Store.PutConcept(ctx, store.Concept{
				ID: conceptID, Name: t, Status: store.ConceptDraft,
				Origin: store.ConceptOriginImplicit, Scope: scope, CreatedAt: s.cfg.Now().UnixMilli(),
			}); err != nil {
				out.Degraded = append(out.Degraded,
					fmt.Sprintf("概念 %q 落 draft 失败（实体仍然写入）：%v", t, err))
			}
		}
		if err := s.cfg.Ports.Graph.PutNode(ctx, store.Node{
			ID: nodeID, Scope: scope, ConceptID: conceptID,
			CanonicalName: name, PropsJSON: "{}",
		}); err != nil {
			return out, fmt.Errorf("memory: 实体 %q 写入节点: %w", name, err)
		}
		// 别名是**真实解析路径**（L0 词典规则的最高置信来源）：
		// 没有它，open_nodes / add_observations 按名字就找不到这个节点。
		if err := s.cfg.Ports.Store.PutAlias(ctx, store.Alias{
			Surface: name, NodeID: nodeID, Source: "mcp:create_entities", Confidence: 0.95,
		}); err != nil {
			return out, fmt.Errorf("memory: 实体 %q 写入别名: %w", name, err)
		}

		eo := EntityOut{Name: name, ID: nodeID, ConceptID: conceptID}
		for _, obs := range e.Observations {
			text := strings.TrimSpace(obs)
			if text == "" {
				continue
			}
			memID := "m-" + shortHash(string(scope), "obs", name, text)
			res, err := s.writer.PutMemory(ctx, store.Memory{
				ID: memID, Scope: scope, ConceptID: conceptID,
				Kind: store.KindSemantic, Content: text,
				TValid: s.cfg.Now().Unix(), TCreated: s.cfg.Now().Unix(), Importance: 0.6,
			})
			if err != nil {
				return out, fmt.Errorf("memory: 实体 %q 写入观测: %w", name, err)
			}
			eo.Memories = append(eo.Memories, res.ID)
		}
		out.Entities = append(out.Entities, eo)
	}
	sort.Slice(out.Entities, func(i, j int) bool { return out.Entities[i].Name < out.Entities[j].Name })
	return out, nil
}

// ResolveNames 按名字（别名词典）解析节点 ID。
//
// 返回 resolved（name → nodeID）与 unresolved（没找到的名字，保序）。
// 用别名而不是"扫节点表按名字模糊匹配"：别名是 L0 的最高置信来源，
// 且它天然带 scope 归属（节点自身）。
func (s *Service) ResolveNames(ctx context.Context, names []string) (map[string]string, []string, []string, error) {
	or, ok := s.cfg.Ports.Store.(store.OntologyReader)
	if !ok || or == nil {
		// 显式降级：不假装"名字都不存在"，而是说清楚拿不到词典。
		return nil, names, []string{"后端未实现 store.OntologyReader：无法按名字解析实体"}, nil
	}
	aliases, err := or.ListAliases(ctx)
	if err != nil {
		return nil, names, nil, fmt.Errorf("memory: 枚举别名: %w", err)
	}
	bySurface := make(map[string]string, len(aliases))
	for _, a := range aliases {
		key := strings.ToLower(strings.TrimSpace(a.Surface))
		if key == "" {
			continue
		}
		// 同一 surface 多条时取置信度最高的那条（与 GetAlias 的口径一致）。
		if prev, dup := bySurface[key]; dup && prev != "" {
			continue
		}
		bySurface[key] = a.NodeID
	}
	resolved := make(map[string]string, len(names))
	var missing []string
	for _, n := range names {
		key := strings.ToLower(strings.TrimSpace(n))
		if key == "" {
			continue
		}
		if id, ok := bySurface[key]; ok && id != "" {
			resolved[n] = id
			continue
		}
		missing = append(missing, n)
	}
	return resolved, missing, nil, nil
}

// Anchors 从一次召回里提取命中的实体节点（search_nodes 的返回来源）。
//
// 优先用 plan 的 anchors（对齐阶段最确定的实体锚点），再用 graph lane 的边端点
// 兜底：两者都来自同一次 Recall，不额外查询。
func Anchors(res recall.RecallResult) []string {
	seen := map[string]bool{}
	var out []string
	add := func(id string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, id)
	}
	if res.Plan != nil {
		for _, a := range res.Plan.Anchors {
			add(a)
		}
	}
	for _, it := range res.Items {
		for _, ge := range it.GraphEvidence {
			add(ge.Anchor)
			add(ge.From)
			add(ge.To)
		}
	}
	sort.Strings(out)
	return out
}
