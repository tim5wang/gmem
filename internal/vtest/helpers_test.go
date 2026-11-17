package vtest

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/tim5wang/gmem/sqlite"
	"github.com/tim5wang/gmem/store"

	_ "modernc.org/sqlite"
)

// 本包是 P0 的**独立验证**包：不 import gmem 内部实现文件，只通过 store 包的
// 四个 Port 接口 + fusion/bigram 两个纯函数包观察行为，也不复用 dev 的 _test.go。
// 目的：找出实现者自己的测试测不到的问题（尤其是"索引侧/查询侧"这类需要跨侧比对的缺陷）。

// openMemory 打开一个内存库（Path 留空）。
func openMemory(t *testing.T) *sqlite.DB {
	t.Helper()
	d, err := sqlite.Open(context.Background(), sqlite.Config{})
	if err != nil {
		t.Fatalf("open memory db: %v", err)
	}
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("close memory db: %v", err)
		}
	})
	return d
}

// openFile 打开一个文件库，并额外返回一条**直连连接**。
//
// 直连连接用于观察落库结果（memories_fts.bigrams 列、schema_migrations 行），
// 从而可以在不改生产代码的前提下，手工制造"旧实现"写入的行做对照。
func openFile(t *testing.T) (*sqlite.DB, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gmem-verify.db")
	d, err := sqlite.Open(context.Background(), sqlite.Config{Path: path})
	if err != nil {
		t.Fatalf("open file db: %v", err)
	}
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("close file db: %v", err)
		}
	})

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw conn: %v", err)
	}
	t.Cleanup(func() {
		if err := raw.Close(); err != nil {
			t.Errorf("close raw conn: %v", err)
		}
	})
	return d, raw
}

// hitIDs 把命中列表压成 id 切片，便于比较。
func hitIDs(hits []store.Hit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.ID
	}
	return out
}

// contains 报告 ids 里是否含 want。
func contains(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// countInt 执行一条返回单整数的诊断查询。
func countInt(t *testing.T, raw *sql.DB, query string) int {
	t.Helper()
	var n int
	if err := raw.QueryRowContext(context.Background(), query).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return n
}
