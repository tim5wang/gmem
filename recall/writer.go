package recall

import (
	"context"
	"fmt"

	"github.com/tim5wang/gmem/store"
)

// Writer 是**单一写入入口**：把 memories 与它的派生索引（FTS，必要时还有 graph）
// 绑在同一个事务里，保证版本一致。
//
// # 为什么必须有它（D13 遗留的 P1 必做项）
//
// P0 的 PutMemory 与 LexicalIndex.Index 是两次独立调用（见 store.Ports）：
//
//	store.PutMemory(ctx, m)                      // 写主表
//	lexical.Index(ctx, []Document{{...}})        // 写 FTS
//
// 漏调后者**不会报错**——它只是让这条记忆在词法通道上永远查不到，
// 表现为"答案变差"而不是"程序出错"。这类缺陷在生产里最难查，所以 P1 用
// 结构性手段排除它：把两者收进同一个方法，中间不允许出现"只写一半"的回调。
//
// # 版本一致（D13）
//
// memories_fts 按 **(id, version)** 索引，因此索引必须写入**这次写入实际产生的
// 版本号**。传 0 或写死 1 的后果很具体：第二次写入产生 v2，但 FTS 仍在 v2 上
// 索引不到（或把 v1 覆盖掉），于是 as_of 回溯与"改过之后的当前版本"都静默漏召回。
//
// 这里不靠猜测版本：PutMemory 之后在**同一事务内**读回当前版本（GetMemory），
// 拿它的 Version 交给 Index。这样三个不变量同时成立：
//   - 写入是幂等的重写时，读回的是既有当前版本，不会凭空造出一个不存在的版本；
//   - 后端内部如何分配版本不需要暴露（不新增/不破坏 P0 的公开契约）；
//   - 索引与主表在同一个 Tx 里，要么都成功、要么都回滚。
//
// # 事务边界
//
// 用 Store.Tx 拿到注入了事务句柄的 ctx，再把该 ctx 传给 Lexical/Graph。
// 任一环节返回错误 → 整体回滚，不会留下"主表有、索引无"的半成品。
type Writer struct {
	ports store.Ports
}

// NewWriter 构造写入入口。需要 Store 与 Lexical 至少存在；Graph 可为 nil
// （此时带节点的写入会返回错误，而不是静默丢掉图部分）。
func NewWriter(ports store.Ports) *Writer { return &Writer{ports: ports} }

// WriteBundle 是一次写入的完整内容。
//
// Nodes/Edges 是"文件情况下连图一起写"（docs/TD.md §10 InlineGraph 的落地）：
// 结构化写入的调用方知道自己要写什么，跳过抽取直接成图，省一次 LLM。
type WriteBundle struct {
	Memory store.Memory
	Nodes  []store.Node
	Edges  []store.Edge
}

// PutResult 汇报这次写入的落地结果。
//
// Version 是**实际写入/复用的版本号**，也是写进 FTS 的版本号——调用方可用它
// 做断言或审计（"我写完的是第几版"）。
type PutResult struct {
	ID      string
	Version int
	// LexicalIndexed 恒为 true 表示词法通道已同步；它是本入口存在的理由，
	// 因此不作为可选行为暴露。
	LexicalIndexed bool
	// NodeCount / EdgeCount 是随本次写入落地的图元素数量。
	NodeCount int
	EdgeCount int
}

// Put 写入一条记忆 + 它的派生索引 + （可选）图元素，全部在同一事务内。
//
// 顺序是刻意的：先主表（分配版本），再读回版本，最后写 FTS 与图。
// 反转顺序就得先猜版本号——那正是 D13 缺陷的来源。
func (w *Writer) Put(ctx context.Context, b WriteBundle) (PutResult, error) {
	if w.ports.Store == nil {
		return PutResult{}, fmt.Errorf("recall: writer: nil store port")
	}
	if w.ports.Lexical == nil {
		return PutResult{}, fmt.Errorf("recall: writer: nil lexical port（写入入口必须能同步 FTS）")
	}
	if err := b.Memory.Validate(); err != nil {
		return PutResult{}, err
	}
	if len(b.Nodes) > 0 && w.ports.Graph == nil {
		// 静默丢掉图部分会让"我明明写了关系"变成谜；宁可失败。
		return PutResult{}, fmt.Errorf("recall: writer: bundle 带 %d 个节点但 graph port 为 nil", len(b.Nodes))
	}

	var res PutResult
	err := w.ports.Store.Tx(ctx, func(tctx context.Context, tx store.Store) error {
		// 1) 主表：后端在此分配版本（首次=1，内容变化=+1，等价重写=不变）。
		if err := tx.PutMemory(tctx, b.Memory); err != nil {
			return fmt.Errorf("writer: put memory: %w", err)
		}

		// 2) 读回**这次写入产生的当前版本**。同一 Tx 内可见，不可能是别的版本。
		cur, err := tx.GetMemory(tctx, b.Memory.ID)
		if err != nil {
			return fmt.Errorf("writer: read back memory %q: %w", b.Memory.ID, err)
		}
		if cur.Version <= 0 {
			// 后端返回了非版本化的记录：继续写 FTS 会以 version=0 落库，
			// 之后 as_of 与当前版本查询都会对不上。宁可报错。
			return fmt.Errorf("writer: memory %q 读回的 version=%d 非法（要求 >=1）", b.Memory.ID, cur.Version)
		}

		// 3) 词法索引：版本号来自上一步的读回值，绝不写死。
		if err := w.ports.Lexical.Index(tctx, []store.Document{{
			ID:      cur.ID,
			Version: cur.Version,
			Scope:   cur.Scope,
			Text:    cur.Content,
		}}); err != nil {
			return fmt.Errorf("writer: lexical index %q v%d: %w", cur.ID, cur.Version, err)
		}

		// 4) 图元素（InlineGraph）：与主表、索引同事务。
		for _, n := range b.Nodes {
			if err := w.ports.Graph.PutNode(tctx, n); err != nil {
				return fmt.Errorf("writer: put node %q: %w", n.ID, err)
			}
		}
		for _, e := range b.Edges {
			if err := w.ports.Graph.PutEdge(tctx, e); err != nil {
				return fmt.Errorf("writer: put edge %q: %w", e.ID, err)
			}
		}

		res = PutResult{
			ID: cur.ID, Version: cur.Version, LexicalIndexed: true,
			NodeCount: len(b.Nodes), EdgeCount: len(b.Edges),
		}
		return nil
	})
	if err != nil {
		return PutResult{}, err
	}
	return res, nil
}

// PutMemory 是只写一条记忆的便捷入口（= Put(WriteBundle{Memory: m})）。
func (w *Writer) PutMemory(ctx context.Context, m store.Memory) (PutResult, error) {
	return w.Put(ctx, WriteBundle{Memory: m})
}
