package vtest

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/tim5wang/gmem/sqlite"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// 阻塞项 / 重要项的**确定性回归用例**
//
// 历史：这些用例最初是"已知缺陷复现"，挂在 GMEM_VERIFY_BLOCKER 门禁后，
// 以免已知问题让 `go test ./...` 永久变红、掩盖新回归。
// 缺陷已修复，因此**门禁已移除**：它们是永久回归用例，
// 缺陷一旦重现，`go test ./...` 就会直接变红。
// ===========================================================================

// TestReproOpenBusyTimeoutOrdering 是 §可后续 #1（PRAGMA 未对每条新连接生效）的回归。
//
// 断言口径：Open 要么成功，要么**真的等过 BusyTimeout** 再失败；
// 不接受"几乎立即返回 SQLITE_BUSY"（那是 busy_timeout 未生效的症状）。
//
// 缺陷：sqlite.Open 的执行顺序是 PingContext → PRAGMA busy_timeout。
// database/sql 新建连接的 busy_timeout 默认是 0（完全不等待），
// 因此 PingContext 一旦遇到别处的写锁就立刻返回 SQLITE_BUSY，
// 配置里显式要求的 BusyTimeout 在这个时候**还没生效**。
//
// 生产影响：多进程同时启动（CLI + MCP server 同时跑）、或崩溃恢复后残留锁时，
// Open 会立刻失败而不是按配置等待。实测"4 个 goroutine 并发 Open 同一新库"
// 20 轮中失败 8 轮（见报告）。
//
// 复现手法（确定性，不依赖调度）：
//  1. 用**裸连接**建一个 rollback-journal 模式的库并持有写锁
//     （不能用 sqlite.Open 预置：它会切成 WAL，而 WAL 下读者不被写者阻塞，
//      缺陷就被掩盖了 —— 这也是首轮复现失败的原因）；
//  2. 用生产的 sqlite.Open 指定 BusyTimeout=5s 打开同一文件；
//  3. 判据同时看**结果**与**耗时**：正确行为应等待锁（耗时接近 5s）后报错，
//     现状是若干微秒内立即报 SQLITE_BUSY，证明配置未参与。
func TestReproOpenBusyTimeoutOrdering(t *testing.T) {
	path := filepath.Join(t.TempDir(), "busy-order.db")
	ctx := context.Background()

	// 连接 A：rollback-journal 模式（默认）+ 真实写入占住写锁。
	holder, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("holder open: %v", err)
	}
	defer func() { _ = holder.Close() }()
	holder.SetMaxOpenConns(1)
	if _, err := holder.ExecContext(ctx, `CREATE TABLE lock_probe(x INTEGER)`); err != nil {
		t.Fatalf("holder create: %v", err)
	}
	// 确认仍是 rollback journal（不是 WAL），否则本用例的前提不成立。
	var mode string
	if err := holder.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("holder journal_mode: %v", err)
	}
	if mode != "delete" {
		t.Fatalf("前提失效：journal_mode = %q, want delete（WAL 下读者不被阻塞，本缺陷不可见）", mode)
	}
	if _, err := holder.ExecContext(ctx, `BEGIN EXCLUSIVE`); err != nil {
		t.Fatalf("holder BEGIN EXCLUSIVE: %v", err)
	}
	if _, err := holder.ExecContext(ctx, `INSERT INTO lock_probe VALUES (1)`); err != nil {
		t.Fatalf("holder insert: %v", err)
	}

	// 连接 B：生产 Open，显式要求 5s BusyTimeout。
	const busy = 5 * time.Second
	start := time.Now()
	_, openErr := sqlite.Open(ctx, sqlite.Config{Path: path, BusyTimeout: busy})
	elapsed := time.Since(start)

	if _, err := holder.ExecContext(ctx, `ROLLBACK`); err != nil {
		t.Logf("holder rollback: %v", err)
	}

	if openErr == nil {
		t.Logf("Open 在锁竞争下成功（耗时 %v）：若已修复 ping/pragma 顺序，请把本用例转为常规回归测试", elapsed)
		return
	}
	// 判据：是否真的等过 BusyTimeout。
	if elapsed < busy/2 {
		t.Fatalf(`【缺陷确认】sqlite.Open 未使用配置的 BusyTimeout：
  BusyTimeout = %v
  实际耗时     = %v（几乎立即返回）
  err          = %v

根因：Open 中顺序为 PingContext() → PRAGMA busy_timeout。
database/sql 每条新连接的 busy_timeout 默认 0，ping 遇到 SQLITE_BUSY
立即失败，配置的 %v 完全没有参与。

最小修复方向（不由本测试实施）：
  a) 把 PRAGMA 设置提到 PingContext **之前**（busy_timeout 先于 ping 生效）；
  b) 或把 pragma 写进 DSN：
     "file:<path>?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"，
     modernc 驱动会在每条新连接创建时自动执行，天然早于 ping（已验证 DSN 形式生效）；
  c) 或让 ping 容忍 SQLITE_BUSY（busy 说明库存在且可连，不该判为打开失败）。`,
			busy, elapsed, openErr, busy)
	}
	t.Logf("Open 失败但等待了 %v（>= BusyTimeout/2），说明 busy_timeout 已生效；err=%v", elapsed, openErr)
}

// TestReproGraphAndMemoryNotInSameTx 是【阻塞合并】的第二个可复现后果。
//
// §11 把"图不外包给图数据库"的理由写成"图与记忆在同一个事务边界里"。
// 由于跨 Port 在 Tx 内调用会死锁（见 blocker_tx_test.go），
// 这条设计前提在 P0 交付物上**无法成立**。
//
// 本用例用确定性的"锁等待"而非"永久挂死"来暴露它：
// 在 Tx 回调里调用 Graph.PutNode，并给 ctx 一个短超时。
// 期望：要么成功（跨 Port 事务可用），要么快速返回明确错误。
// 现状：阻塞到 ctx 超时，返回 context deadline exceeded（错误信息完全看不出真因）。
func TestReproGraphAndMemoryNotInSameTx(t *testing.T) {
	d := openMemory(t)
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()

	err := d.Store().Tx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.PutMemory(ctx, store.Memory{
			ID: "m-tx", Scope: store.ScopeUser, Kind: store.KindSemantic,
			Content: "认证服务依赖令牌服务", TValid: 10, TCreated: 10,
		}); err != nil {
			return err
		}
		// 同一逻辑事务里落图：这是 §11 的明确要求。
		return d.Graph().PutNode(ctx, store.Node{
			ID: "n-tx", Scope: store.ScopeUser, CanonicalName: "认证服务",
		})
	})
	if err == nil {
		t.Logf("memory + graph 已能在同一 Tx 内提交：§11 的前提成立")
		return
	}
	t.Fatalf(`【阻塞合并】memory 与 graph 无法在同一事务内写入：
  err = %v

这不是"图写失败"，而是 Graph.PutNode 在 Tx 回调里拿不到连接
（SetMaxOpenConns(1) + Tx 独占），最终被 ctx 超时打断。
错误信息（context deadline exceeded）不指向真因，排查成本高。

影响：§11 "图与记忆在同一事务边界内"在 P0 无法实现；
P1/P2 整理层若按设计直接这么写，会表现为"偶发超时"，而非明确报错。`, err)
}

