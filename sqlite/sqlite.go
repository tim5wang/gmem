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
	"net/url"
	"strings"
	"sync"
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
	c  *conn

	storeImpl *storeImpl
	lexical   *lexicalIndex
	vector    *vectorIndex
	graph     *graphStore
}

// querier 统一 *sql.DB 与 *sql.Tx，让同一份实现既能直接跑也能在事务里跑。
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// txKey 是 ctx 中携带当前事务的键（§11 的"把 *sql.Tx 注入给各 Port 副本"）。
type txKey struct{}

func withTx(ctx context.Context, tx *sql.Tx) context.Context {
	return context.WithValue(ctx, txKey{}, tx)
}

func txFromCtx(ctx context.Context) *sql.Tx {
	tx, _ := ctx.Value(txKey{}).(*sql.Tx)
	return tx
}

// conn 是四个 Port 共享的连接状态。
//
// 为什么需要 tx 标志位（B-1 的修复核心）：
// 连接池上差点为 1（内存库必须如此），当事务独占唯一连接时，任何走 *sql.DB
// 的调用都会**阻塞等连接**直到事务提交 —— 这就是跨 Port 死锁。
// 因此：
//   - 事务期间的跨 Port 调用必须通过 ctx 拿到同一个 *sql.Tx（落到同一事务）；
//   - 拿不到事务句柄的调用必须**立刻**收到 ErrTxConflict，绝不排队等待。
type conn struct {
	db *sql.DB

	mu sync.Mutex
	tx bool // 是否有事务正占用连接
}

// querier 返回当前 ctx 应使用的执行句柄。
//
// ctx 里带事务 → 直接用事务（同一条连接、同一事务边界）；
// 否则若有事务在跑 → 快速失败；都没有 → 用连接池。
func (c *conn) querier(ctx context.Context) (querier, error) {
	if tx := txFromCtx(ctx); tx != nil {
		return tx, nil
	}
	if c.txRunning() {
		return nil, store.ErrTxConflict
	}
	return c.db, nil
}

func (c *conn) txRunning() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tx
}

// acquire 尝试独占事务资格；已有事务在跑时返回 ErrTxConflict（不阻塞）。
func (c *conn) acquire() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tx {
		return store.ErrTxConflict
	}
	c.tx = true
	return nil
}

func (c *conn) release() {
	c.mu.Lock()
	c.tx = false
	c.mu.Unlock()
}

// runInTx 在事务内执行 fn：ctx 已带事务时直接复用（不新开），否则在无他人事务时新开一个。
func (c *conn) runInTx(ctx context.Context, fn func(context.Context, querier) error) error {
	if tx := txFromCtx(ctx); tx != nil {
		return fn(ctx, tx)
	}
	if err := c.acquire(); err != nil {
		return err
	}
	defer c.release()
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if err := fn(withTx(ctx, tx), tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	committed = true
	return nil
}

// Open 打开数据库、应用迁移，并返回四个 Port 的实现。
//
// Open 会调用 Migrate，因此调用方拿到的库一定是当前 schema 版本；
// Migrate 也对外暴露，重复调用是安全的。
//
// PRAGMA 全部写进 DSN：modernc 驱动会在**每条新连接**建立时执行，
// 因此不依赖"Open 里恰好跑过一次"（那对连接池里后来新建的连接无效），
// 也不会像旧的 `for ... continue` 那样把失败静默吞掉。
func Open(ctx context.Context, cfg Config) (*DB, error) {
	path := cfg.Path
	timeout := cfg.BusyTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	dsn := buildDSN(path, timeout)
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %q: %w", path, err)
	}
	// 内存库每连接各自独立，必须收敛到单连接才是同一个库；
	// 文件库也用单连接：SQLite 单写者语义下串行写更稳，且 P0 无并发写需求。
	// 跨 Port 的原子性不依赖连接数，而依赖 Tx 把 *sql.Tx 注入到 ctx（见 conn）。
	sqlDB.SetMaxOpenConns(1)

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(pingCtx); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("sqlite: ping %q: %w", path, err)
	}

	// 自检：foreign_keys 必须对新连接生效。失败即报错，不再 continue 吞掉。
	var fk int
	if err := sqlDB.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("sqlite: verify foreign_keys: %w", err)
	}
	if fk != 1 {
		sqlDB.Close()
		return nil, fmt.Errorf("sqlite: PRAGMA foreign_keys 未生效（= %d, want 1）", fk)
	}

	c := &conn{db: sqlDB}
	d := &DB{
		db:        sqlDB,
		c:         c,
		storeImpl: &storeImpl{c: c},
		lexical:   &lexicalIndex{c: c},
		vector:    &vectorIndex{c: c},
		graph:     &graphStore{c: c},
	}
	if err := d.Migrate(ctx); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return d, nil
}

// buildDSN 生成带 _pragma 的 DSN。内存库用 file::memory: 形式，pragma 仍然生效。
func buildDSN(path string, timeout time.Duration) string {
	base := "file:" + path
	if path == "" {
		base = "file::memory:"
	}
	pragmas := []string{
		"foreign_keys(1)",
		fmt.Sprintf("busy_timeout(%d)", timeout.Milliseconds()),
		// 内存库没有 WAL，驱动会退化为 memory 模式而不报错（已实测）。
		"journal_mode(WAL)",
	}
	parts := make([]string, 0, len(pragmas))
	for _, p := range pragmas {
		parts = append(parts, "_pragma="+url.QueryEscape(p))
	}
	return base + "?" + strings.Join(parts, "&")
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
