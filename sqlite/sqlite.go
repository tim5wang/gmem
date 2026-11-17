// Package sqlite 是 Gmem 的 SQLite 后端实现（docs/TD.md §11）。
//
// 它实现了 store 包定义的四个 Port：Store / LexicalIndex / VectorIndex / GraphStore。
// 上层只拿接口，不 import 本包的具体类型，因此 SQL 不会泄漏到召回侧与整理层。
//
// 依赖：仅 modernc.org/sqlite（纯 Go，FTS5 内置，无需 CGO 与 build tag）。
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/tim5wang/gmem/store"

	_ "modernc.org/sqlite" // 纯 Go 驱动，注册名为 "sqlite"
)

// Config 是 Open 的配置。
type Config struct {
	// Path 是数据库文件路径；留空表示内存库（测试用）。
	Path string
	// BusyTimeout 是写锁等待时间，<=0 时取 5s。
	BusyTimeout time.Duration
}

// DB 持有连接池与四个 Port 的实现。
type DB struct {
	db *sql.DB

	storeImpl *storeImpl
	lexical   *lexicalIndex
	vector    *vectorIndex
	graph     *graphStore
}

// Open 打开数据库、应用迁移，并返回四个 Port 的实现。
//
// Open 会调用 Migrate，因此调用方拿到的库一定是当前 schema 版本；
// Migrate 也对外暴露，重复调用是安全的。
func Open(ctx context.Context, cfg Config) (*DB, error) {
	path := cfg.Path
	inMemory := path == ""
	if inMemory {
		path = ":memory:"
	}
	timeout := cfg.BusyTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	sqlDB, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %q: %w", path, err)
	}
	// 内存库每连接各自独立，必须收敛到单连接才是同一个库；
	// 文件库也用单连接：SQLite 单写者语义下串行写更稳，且 P0 无并发写需求。
	sqlDB.SetMaxOpenConns(1)

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(pingCtx); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("sqlite: ping %q: %w", path, err)
	}

	for _, pragma := range []string{
		// 外键约束：episodes/memories/nodes/edges/concepts/aliases 之间的引用完整性
		`PRAGMA foreign_keys = ON`,
		fmt.Sprintf(`PRAGMA busy_timeout = %d`, timeout.Milliseconds()),
		`PRAGMA journal_mode = WAL`,
	} {
		if _, err := sqlDB.ExecContext(ctx, pragma); err != nil {
			// 内存库不支持 WAL，只警告不失败；其余 pragma 失败同样不致命。
			continue
		}
	}

	d := &DB{
		db:        sqlDB,
		storeImpl: &storeImpl{q: sqlDB},
		lexical:   &lexicalIndex{db: sqlDB},
		vector:    &vectorIndex{q: sqlDB},
		graph:     &graphStore{q: sqlDB},
	}
	if err := d.Migrate(ctx); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return d, nil
}

// Close 关闭底层连接池。
func (d *DB) Close() error {
	if d == nil || d.db == nil {
		return nil
	}
	return d.db.Close()
}

// Store 返回事务性 CRUD 端口。
func (d *DB) Store() store.Store { return d.storeImpl }

// Lexical 返回词法索引端口（FTS5 + bigram）。
func (d *DB) Lexical() store.LexicalIndex { return d.lexical }

// Vector 返回向量索引端口（int8 量化 + 暴力 KNN）。
func (d *DB) Vector() store.VectorIndex { return d.vector }

// Graph 返回图端口（nodes/edges + 递归 CTE）。
func (d *DB) Graph() store.GraphStore { return d.graph }

// Ports 一次返回四个端口，便于上层注入。
func (d *DB) Ports() store.Ports {
	return store.Ports{
		Store:   d.storeImpl,
		Lexical: d.lexical,
		Vector:  d.vector,
		Graph:   d.graph,
	}
}
