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
//
// 跨 Port 原子性（docs/TD.md §11）：回调收到的 ctx **携带事务句柄**，
// 在回调内用该 ctx 调其它 Port（Lexical / Vector / Graph）即落到同一事务边界。
// 反过来，用外层 ctx 调其它 Port：后端无法注入事务时必须**立即**返回
// ErrTxConflict，绝不允许阻塞等连接（静默挂死比报错难查得多）。
type Store interface {
	// episodes：不可变事实来源
	PutEpisode(ctx context.Context, e Episode) error
	GetEpisode(ctx context.Context, id string) (Episode, error)

	// memories：双时态事实
	// PutMemory 写入一条记忆。同一 ID 的再次写入按 D12 产生**新版本**：
	// 旧版置 TExpired，默认读当前版本；完全等价的重写是幂等的空操作。
	// 已失效（TInvalid 非零）的事实不得因再次写入而复活。
	PutMemory(ctx context.Context, m Memory) error
	// GetMemory 取该 ID 的**当前版本**（TExpired 为空）；不存在返回 ErrNotFound。
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
//
// Neighbors 的 Scope 是**必填**隔离边界（不是"不限"）：后端必须强制过滤，
// 只返回起点与终点都在该 scope 内的边。
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
