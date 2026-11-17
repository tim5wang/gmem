package sqlite

import (
	"context"
	"sort"
	"testing"
)

// openTest 用内存库打开一个准备好 schema 的实例。
func openTest(t *testing.T) *DB {
	t.Helper()
	ctx := context.Background()
	d, err := Open(ctx, Config{}) // Path 留空 = 内存库
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return d
}

// TestMigrateIdempotent：重复执行迁移安全，版本号收敛到清单最大版本。
func TestMigrateIdempotent(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	first, err := d.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	want := migrations()[len(migrations())-1].Version
	if first != want {
		t.Fatalf("schema version = %d, want %d", first, want)
	}

	for i := 0; i < 3; i++ {
		if err := d.Migrate(ctx); err != nil {
			t.Fatalf("Migrate #%d: %v", i+2, err)
		}
		got, err := d.SchemaVersion(ctx)
		if err != nil {
			t.Fatalf("SchemaVersion: %v", err)
		}
		if got != want {
			t.Fatalf("after re-migrate version = %d, want %d", got, want)
		}
	}

	// 迁移记录不重复：每条迁移恰好一行。
	var rows int
	if err := d.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM schema_migrations`).Scan(&rows); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if rows != len(migrations()) {
		t.Fatalf("schema_migrations rows = %d, want %d", rows, len(migrations()))
	}
}

// TestMigrateConcurrentSafe：并发调 Migrate 不出错（DDL 皆 IF NOT EXISTS + INSERT OR IGNORE）。
func TestMigrateConcurrentSafe(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() { errs <- d.Migrate(ctx) }()
	}
	for i := 0; i < 4; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent Migrate: %v", err)
		}
	}
}

// TestSchemaTables：§2 的六张主表 + 两张派生索引都在。
func TestSchemaTables(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	got, err := d.Tables(ctx)
	if err != nil {
		t.Fatalf("Tables: %v", err)
	}
	have := make(map[string]bool, len(got))
	for _, n := range got {
		have[n] = true
	}
	for _, want := range []string{
		"episodes", "memories", "nodes", "edges", "concepts", "aliases",
		"memories_fts", "vec_blob", "schema_migrations",
	} {
		if !have[want] {
			sort.Strings(got)
			t.Errorf("missing table %q; have %v", want, got)
		}
	}
}

// TestSchemaBitemporalColumns：memories / edges 都有四时间戳列。
func TestSchemaBitemporalColumns(t *testing.T) {
	d := openTest(t)

	for _, table := range []string{"memories", "edges"} {
		cols := columnNames(t, d, table)
		for _, want := range []string{"t_valid", "t_invalid", "t_created", "t_expired"} {
			if !cols[want] {
				t.Errorf("%s missing column %q", table, want)
			}
		}
	}
}

// TestConceptStatusCheck：concepts.status 只接受 draft/certified/deprecated。
func TestConceptStatusCheck(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	if _, err := d.db.ExecContext(ctx,
		`INSERT INTO concepts (id, name, status) VALUES ('c1', '认证', 'bogus')`); err == nil {
		t.Fatal("invalid concepts.status accepted, want CHECK violation")
	}
	for _, st := range []string{"draft", "certified", "deprecated"} {
		if _, err := d.db.ExecContext(ctx,
			`INSERT INTO concepts (id, name, status) VALUES (?, ?, ?)`, "c_"+st, "概念", st); err != nil {
			t.Errorf("status %q rejected: %v", st, err)
		}
	}
}

func columnNames(t *testing.T, d *DB, table string) map[string]bool {
	t.Helper()
	rows, err := d.db.QueryContext(context.Background(), `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatalf("table_info %s: %v", table, err)
	}
	defer rows.Close()
	out := make(map[string]bool, 16)
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		out[n] = true
	}
	return out
}
