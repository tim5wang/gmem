package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// 迁移机制：带版本号、可重复执行。
//
// 约定：
//   - 每个迁移有唯一 version，按 version 升序应用；
//   - 已应用的版本记录在 schema_migrations，重复执行 Migrate 不会再跑；
//   - 每条 DDL 都写 IF NOT EXISTS，因此即使 schema_migrations 丢失也不会炸库；
//   - 单个迁移在一个事务里完成，失败整版回滚（不会留下半截 schema）。

type migration struct {
	Version    int
	Name       string
	Statements []string
}

const migrationsTableDDL = `CREATE TABLE IF NOT EXISTS schema_migrations (
	version    INTEGER PRIMARY KEY,
	name       TEXT    NOT NULL,
	applied_at INTEGER NOT NULL
)`

// migrations 是唯一的迁移清单。新增 schema 变更只往后追加，不改历史条目。
func migrations() []migration {
	return []migration{
		{
			Version: 1,
			Name:    "p0_core_schema",
			Statements: []string{
				// episodes：不可变的事实来源
				`CREATE TABLE IF NOT EXISTS episodes (
					id           TEXT PRIMARY KEY,
					scope        TEXT NOT NULL CHECK (scope IN ('user','agent','project','global')),
					namespace    TEXT NOT NULL DEFAULT '',
					role         TEXT NOT NULL DEFAULT '',
					content      TEXT NOT NULL,
					content_hash TEXT NOT NULL DEFAULT '',
					at           INTEGER NOT NULL DEFAULT 0
				)`,
				`CREATE INDEX IF NOT EXISTS idx_episodes_scope_at ON episodes(scope, at)`,
				`CREATE INDEX IF NOT EXISTS idx_episodes_content_hash ON episodes(content_hash)`,

				// concepts：本体，status 是 D1 的关键（draft / certified / deprecated）
				`CREATE TABLE IF NOT EXISTS concepts (
					id         TEXT PRIMARY KEY,
					name       TEXT NOT NULL,
					parent_id  TEXT REFERENCES concepts(id),
					status     TEXT NOT NULL DEFAULT 'draft'
					           CHECK (status IN ('draft','certified','deprecated')),
					definition TEXT NOT NULL DEFAULT ''
				)`,
				`CREATE INDEX IF NOT EXISTS idx_concepts_status ON concepts(status)`,
				`CREATE INDEX IF NOT EXISTS idx_concepts_parent_id ON concepts(parent_id)`,

				// memories：双时态事实。t_invalid / t_expired 为 NULL 表示"尚未失效"。
				`CREATE TABLE IF NOT EXISTS memories (
					id             TEXT PRIMARY KEY,
					scope          TEXT NOT NULL CHECK (scope IN ('user','agent','project','global')),
					concept_id     TEXT REFERENCES concepts(id),
					kind           TEXT NOT NULL
					               CHECK (kind IN ('semantic','episodic','procedural','decision')),
					content        TEXT NOT NULL,
					t_valid        INTEGER NOT NULL DEFAULT 0,
					t_invalid      INTEGER,
					t_created      INTEGER NOT NULL DEFAULT 0,
					t_expired      INTEGER,
					source_episode TEXT REFERENCES episodes(id),
					importance     REAL NOT NULL DEFAULT 0.5,
					recall_count   INTEGER NOT NULL DEFAULT 0
				)`,
				`CREATE INDEX IF NOT EXISTS idx_memories_scope_bi ON memories(scope, t_invalid, t_valid)`,
				`CREATE INDEX IF NOT EXISTS idx_memories_concept_id ON memories(concept_id)`,
				`CREATE INDEX IF NOT EXISTS idx_memories_source_episode ON memories(source_episode)`,

				// nodes：图节点
				`CREATE TABLE IF NOT EXISTS nodes (
					id             TEXT PRIMARY KEY,
					scope          TEXT NOT NULL CHECK (scope IN ('user','agent','project','global')),
					concept_id     TEXT REFERENCES concepts(id),
					canonical_name TEXT NOT NULL,
					props_json     TEXT NOT NULL DEFAULT '{}'
				)`,
				`CREATE INDEX IF NOT EXISTS idx_nodes_scope_name ON nodes(scope, canonical_name)`,
				`CREATE INDEX IF NOT EXISTS idx_nodes_concept_id ON nodes(concept_id)`,

				// edges：图边，同样双时态
				`CREATE TABLE IF NOT EXISTS edges (
					id         TEXT PRIMARY KEY,
					from_id    TEXT NOT NULL REFERENCES nodes(id),
					to_id      TEXT NOT NULL REFERENCES nodes(id),
					relation   TEXT NOT NULL,
					weight     REAL NOT NULL DEFAULT 1.0,
					t_valid    INTEGER NOT NULL DEFAULT 0,
					t_invalid  INTEGER,
					t_created  INTEGER NOT NULL DEFAULT 0,
					t_expired  INTEGER
				)`,
				`CREATE INDEX IF NOT EXISTS idx_edges_from_rel ON edges(from_id, relation)`,
				`CREATE INDEX IF NOT EXISTS idx_edges_to ON edges(to_id)`,
				`CREATE INDEX IF NOT EXISTS idx_edges_rel_bi ON edges(relation, t_invalid)`,

				// aliases：surface → node 词典（L0 词典规则的最高置信来源）
				`CREATE TABLE IF NOT EXISTS aliases (
					surface    TEXT NOT NULL,
					node_id    TEXT NOT NULL REFERENCES nodes(id),
					source     TEXT NOT NULL DEFAULT '',
					confidence REAL NOT NULL DEFAULT 0,
					PRIMARY KEY (surface, node_id)
				)`,
				`CREATE INDEX IF NOT EXISTS idx_aliases_node_id ON aliases(node_id)`,
			},
		},
		{
			Version: 2,
			Name:    "p0_fts5_lexical",
			Statements: []string{
				// bigrams 列存预转换后的 token 串；body 仅留原文便于排错与重建，不参与检索。
				`CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(
					id UNINDEXED,
					scope UNINDEXED,
					body UNINDEXED,
					bigrams,
					tokenize = 'unicode61'
				)`,
			},
		},
		{
			Version: 3,
			Name:    "p0_vector_blob",
			Statements: []string{
				// int8 量化快照：可从主表重建，仅作加速物
				`CREATE TABLE IF NOT EXISTS vec_blob (
					id    TEXT PRIMARY KEY,
					scope TEXT NOT NULL CHECK (scope IN ('user','agent','project','global')),
					dim   INTEGER NOT NULL,
					scale REAL NOT NULL,
					data  BLOB NOT NULL
				)`,
				`CREATE INDEX IF NOT EXISTS idx_vec_blob_scope ON vec_blob(scope)`,
			},
		},
	}
}

// Migrate 应用所有未应用的迁移。重复执行安全（幂等）。
func (d *DB) Migrate(ctx context.Context) error {
	if _, err := d.db.ExecContext(ctx, migrationsTableDDL); err != nil {
		return fmt.Errorf("sqlite: create schema_migrations: %w", err)
	}

	applied := make(map[int]bool, 8)
	rows, err := d.db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("sqlite: read schema_migrations: %w", err)
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return fmt.Errorf("sqlite: scan schema_migrations: %w", err)
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("sqlite: iterate schema_migrations: %w", err)
	}
	rows.Close()

	for _, m := range migrations() {
		if applied[m.Version] {
			continue
		}
		if err := d.applyMigration(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

func (d *DB) applyMigration(ctx context.Context, m migration) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin migration %d: %w", m.Version, err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, stmt := range m.Statements {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("sqlite: migration %d (%s): %w", m.Version, m.Name, err)
		}
	}
	// INSERT OR IGNORE：两个进程并发迁移时，后到者不报错（DDL 本身也已 IF NOT EXISTS）。
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO schema_migrations(version, name, applied_at) VALUES (?, ?, ?)`,
		m.Version, m.Name, time.Now().Unix(),
	); err != nil {
		return fmt.Errorf("sqlite: record migration %d: %w", m.Version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit migration %d: %w", m.Version, err)
	}
	return nil
}

// SchemaVersion 返回当前已应用的最大迁移版本；空库返回 0。
func (d *DB) SchemaVersion(ctx context.Context) (int, error) {
	var v sql.NullInt64
	err := d.db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("sqlite: schema version: %w", err)
	}
	if !v.Valid {
		return 0, nil
	}
	return int(v.Int64), nil
}

// Tables 返回库中所有用户表/虚表名，按名排序。仅供诊断与测试使用。
func (d *DB) Tables(ctx context.Context) ([]string, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type IN ('table','view') AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list tables: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("sqlite: scan table name: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
