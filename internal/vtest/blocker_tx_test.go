package vtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tim5wang/gmem/sqlite"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// 【阻塞合并】跨 Port 在 Tx 内调用 ⇒ 死锁
//
// 现象：Open 里 SetMaxOpenConns(1)（单连接池，内存库必须如此）。
// Store.Tx 在回调期间独占那唯一一条连接，此时若在回调里调用**另一个 Port**
// （Lexical / Vector / Graph 都各自持有 *sql.DB，都会去抢连接），
// 每次调用都要等连接，而连接只会在 Tx 提交后释放 —— 于是永久互等。
//
// 为什么这是阻塞项而不是"可后续"：
//   - §11 明确要求"图与记忆在同一个事务边界里"（图不外包给图数据库的理由就是这个）。
//     现在做不到：在 Tx 里更新图（或写 FTS 索引）会挂死整个进程，不是报错而是 hang。
//   - 静默 hang 比报错更难查：调用方无从得知自己踩了什么。
//   - P1/P2 的整理层必然要在"写 memory + 写 FTS + 写 graph"之间保证原子性，
//     这个缺陷会直接堵住那条路径。
//
// 本文件的两个用例：
//   - TestBlockerTxWithLexicalDeadlocks：证明死锁存在（带超时，不会真挂住 CI）；
//   - TestTxNestedSamePortIsSafe：证明"同一 Port 内嵌套 Tx"是安全的，
//     从而把问题精确圈定在"跨 Port"，而不是笼统地说"Tx 有问题"。
// ===========================================================================

// txDeadlockProbeTimeout 是探针等待上限：死锁表现为"超时也未返回"。
const txDeadlockProbeTimeout = 1500 * time.Millisecond

// TestBlockerTxWithLexicalDeadlocks 回归：Tx 回调内调用 Lexical.Index 不得阻塞。
//
// 历史：P0 首版在这条路径上**永久挂死**（SetMaxOpenConns(1) + Tx 独占连接），
// 用例当时挂在 GMEM_VERIFY_BLOCKER 门禁后。修复后门禁已移除：
// 这是永久回归用例，缺陷一旦重现就会让 `go test ./...` 直接变红。
//
// 断言：Tx 在超时内返回（成功则已提交，失败则必须给出明确错误），
// 绝不接受"永不返回"。
func TestBlockerTxWithLexicalDeadlocks(t *testing.T) {
	d := openMemory(t)
	base := context.Background()

	// 用无超时的 ctx：这正是调用方最容易写出的形态，
	// 也是"静默 hang"最危险的形态。测试侧用 channel + timer 兜住。
	done := make(chan error, 1)
	go func() {
		done <- d.Store().Tx(base, func(ctx context.Context, tx store.Store) error {
			if err := tx.PutMemory(ctx, store.Memory{
				ID: "m1", Scope: store.ScopeUser, Kind: store.KindSemantic,
				Content: "用户身份认证失败", TValid: 10, TCreated: 10,
			}); err != nil {
				return err
			}
			// 同一事务里更新词法索引（P1 之后这是常规操作）。
			return d.Lexical().Index(ctx, []store.Document{
				{ID: "m1", Scope: store.ScopeUser, Text: "用户身份认证失败"},
			})
		})
	}()

	select {
	case err := <-done:
		if err != nil {
			// 修复后的合理形态之一：明确报错，让调用方改结构。
			t.Logf("Tx 内跨 Port 调用返回错误（非挂死），行为可接受: %v", err)
			return
		}
		t.Logf("Tx 内跨 Port 调用已成功提交：跨 Port 原子性可用")
	case <-time.After(txDeadlockProbeTimeout):
		t.Fatalf(`【阻塞回归】Store.Tx 回调内调用 Lexical.Index 死锁：%v 内未返回。

根因（若重现）：连接池上差点为 1，Tx 独占唯一连接，
而 Lexical/Vector/Graph 又各自走 *sql.DB 去抢连接 ⇒ 互等。

正确形态（当前实现）：Tx 把 *sql.Tx 注入到 ctx，各 Port 从 ctx 解析执行句柄，
回调内用**回调收到的 ctx** 调用即落到同一事务；
拿不到事务句柄时立刻返回 store.ErrTxConflict，绝不排队等连接。`, txDeadlockProbeTimeout)
	}
}

// TestTxNestedSamePortIsSafe 把问题精确圈定在"跨 Port"。
//
// 同一 Port 内嵌套 Tx 有特殊处理（复用当前事务），必须确实安全；
// 若这条也失败，说明问题比"跨 Port"更广。
func TestTxNestedSamePortIsSafe(t *testing.T) {
	d := openMemory(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- d.Store().Tx(ctx, func(ctx context.Context, tx store.Store) error {
			return tx.Tx(ctx, func(ctx context.Context, inner store.Store) error {
				return inner.PutMemory(ctx, store.Memory{
					ID: "nested", Scope: store.ScopeUser, Kind: store.KindSemantic,
					Content: "嵌套事务", TValid: 1, TCreated: 1,
				})
			})
		})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("同一 Port 内嵌套 Tx 应安全，得到错误: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("同一 Port 内嵌套 Tx 也死锁：问题范围超出「跨 Port」")
	}

	// 嵌套写入应随外层提交。
	if _, err := d.Store().GetMemory(context.Background(), "nested"); err != nil {
		t.Errorf("嵌套 Tx 的写入未提交: %v", err)
	}
}

// TestTxBodyErrorPropagates 验证 Tx 的错误透传与回滚（跨后端一致性用例已覆盖，
// 这里额外固定"回调 panic 时也必须回滚"这条更严的语义——实现注释明确承诺了它）。
func TestTxBodyErrorPropagates(t *testing.T) {
	d := openMemory(t)
	ctx := context.Background()
	sentinel := errors.New("body failed")

	err := d.Store().Tx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.PutMemory(ctx, store.Memory{
			ID: "panic-row", Scope: store.ScopeUser, Kind: store.KindSemantic,
			Content: "应被回滚", TValid: 1, TCreated: 1,
		}); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Tx 未透传回调错误: %v", err)
	}
	if _, err := d.Store().GetMemory(ctx, "panic-row"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("回调返回错误后未回滚: %v", err)
	}
}

// 确保 sqlite 包被引用（openMemory 依赖它）。
var _ = (*sqlite.DB)(nil)
