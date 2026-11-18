package recall

import (
	"context"
	"fmt"

	"github.com/tim5wang/gmem/store"
)

// resolveMemories 把融合出的 ID 解析成"该请求语境下正确的记忆版本"。
//
// 两种语境（D12 双时态）：
//   - as_of 为 nil：取**当前版本**（t_expired IS NULL），逐条 GetMemory；
//   - as_of 非 nil：取**该时刻有效**的版本，用一次 ListMemories(AsOf) 全量解析
//     再按 ID 索引。逐条查在这里不可行：Store 的公开契约没有"按 id + as_of 读单条"，
//     而拿最新版冒充历史版本会给出"未来的答案"，且 plan 里看不出来。
//
// 解析不到的候选记入 Dropped（而不是静默跳过）：索引有行、主表在该时刻没有有效版本，
// 是索引与主表不一致或版本语义出错的直接信号，必须可见。
func (r *Recaller) resolveMemories(ctx context.Context, req RecallRequest, ids []string) (map[string]store.Memory, []DroppedItem, error) {
	out := make(map[string]store.Memory, len(ids))
	var dropped []DroppedItem

	if req.AsOf == nil {
		for _, id := range ids {
			m, err := r.ports.Store.GetMemory(ctx, id)
			if err != nil {
				dropped = append(dropped, DroppedItem{
					ID: id, Reason: fmt.Sprintf("GetMemory: %v（索引与主表不一致，或已无当前版本）", err),
				})
				continue
			}
			out[id] = m
		}
		return out, dropped, nil
	}

	asOf := *req.AsOf
	all, err := r.ports.Store.ListMemories(ctx, store.MemoryQuery{
		Scope: req.Scope,
		Kinds: req.Kinds,
		AsOf:  &asOf,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("recall: list memories as_of=%d: %w", asOf, err)
	}
	byID := make(map[string]store.Memory, len(all))
	for _, m := range all {
		// 同一 ID 在该时刻最多应有一个有效版本；若后端返回多个，取版本号最大的那个
		// （系统时间轴上最后写入的版本），并保持确定性。
		if old, ok := byID[m.ID]; !ok || m.Version > old.Version {
			byID[m.ID] = m
		}
	}
	for _, id := range ids {
		m, ok := byID[id]
		if !ok {
			dropped = append(dropped, DroppedItem{
				ID:     id,
				Reason: fmt.Sprintf("as_of=%d 时该 ID 没有有效版本（索引存在但当时未创建或已过期）", asOf),
			})
			continue
		}
		out[id] = m
	}
	return out, dropped, nil
}
