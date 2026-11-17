package store

import "context"

// 四个 Port 接口。见 docs/TD.md §11：Store（事务·CRUD）、LexicalIndex（索引/查询）、
// VectorIndex（upsert/KNN/快照）、GraphStore（邻接/定向跳）。
//
// 上层只依赖这些接口，不依赖任何后端包，因此 SQL 不会泄漏到召回侧与整理层。
// 编译期注册（不是插件系统）：后端在 init/构造函数里显式选定，存储是热路径。

// Store 是事务性 CRUD 端口。
//
// 事务语义：Tx 内的回调拿到一个绑定到同一事务的 Store，回调返回错误即整体回滚。
type Store interface {
	// episodes：不可变事实来源
	PutEpisode(ctx context.Context, e Episode) error
	GetEpisode(ctx context.Context, id string) (Episode, error)

	// memories：双时态事实
	PutMemory(ctx context.Context, m Memory) error
	GetMemory(ctx context.Context, id string) (Memory, error)
	ListMemories(ctx context.Context, q MemoryQuery) ([]Memory, error)
	// InvalidateMemory 只置 t_invalid（世界时间）,不做物理删除。
	InvalidateMemory(ctx context.Context, id string, tInvalid int64) error

	// concepts：本体
	PutConcept(ctx context.Context, c Concept) error
	GetConcept(ctx context.Context, id string) (Concept, error)

	// aliases：surface → node 词典
	PutAlias(ctx context.Context, a Alias) error
	GetAlias(ctx context.Context, surface string) (Alias, error)

	// Tx 在同一事务内执行 fn；fn 里的操作用 fn 收到的 Store。
	Tx(ctx context.Context, fn func(ctx context.Context, tx Store) error) error
}

// LexicalIndex 是词法索引端口（SQLite: FTS5 + bigram 列；PG: tsvector/pg_trgm）。
type LexicalIndex interface {
	// Index 写入或覆盖一组文档。实现必须自己完成 bigram 预转换（索引侧）。
	Index(ctx context.Context, docs []Document) error
	// Delete 按 ID 删除文档。
	Delete(ctx context.Context, ids []string) error
	// Search 按 BM25 相关性返回命中，Rank 从 1 开始。查询侧同样走 bigram 预转换。
	Search(ctx context.Context, query string, scope Scope, limit int) ([]Hit, error)
}

// VectorIndex 是向量索引端口（SQLite: Go 侧暴力 KNN + int8 量化 BLOB）。
type VectorIndex interface {
	Upsert(ctx context.Context, vectors []Vector) error
	Delete(ctx context.Context, ids []string) error
	// Search 返回余弦相似度最高的 K 条，Rank 从 1 开始。
	Search(ctx context.Context, query []float32, scope Scope, k int) ([]Hit, error)
	// Snapshot 导出该 scope 下的量化快照；索引可从主表重建，快照只是加速物。
	Snapshot(ctx context.Context, scope Scope) ([]byte, error)
}

// GraphStore 是图端口：节点/边两表 + 邻接查询，不外包给图数据库。
type GraphStore interface {
	PutNode(ctx context.Context, n Node) error
	GetNode(ctx context.Context, id string) (Node, error)
	PutEdge(ctx context.Context, e Edge) error
	// Neighbors 从 NodeID 出发按关系白名单做 ≤Hops 跳的邻域遍历。
	Neighbors(ctx context.Context, q NeighborQuery) ([]EdgeHop, error)
}

// Ports 把四个端口打包，便于上层一次注入。
type Ports struct {
	Store   Store
	Lexical LexicalIndex
	Vector  VectorIndex
	Graph   GraphStore
}
