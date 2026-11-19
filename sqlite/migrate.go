package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// 迁移机制：带版本号、可重复执行。
//
// 约定：
//   - 每个迁移有唯一 version，按 version 升序应用；
//   - 已应用的版本记录在 schema_migrations，重复执行 Migrate 不会再跑；
//   - 每条 DDL 都写 IF NOT EXISTS，因此即使 schema_migrations 丢失也不会炸库；
//   - 单个迁移在一个事务里完成，失败整版回滚（不会留下半截 schema）。
//
// Apply 用于无法用纯 DDL 表达的变更（例如"重建表以改主键"）：它必须自己判断
// 目标 schema 是否已经是新形态，因为 schema_migrations 丢失后整个清单会被重跑。

type migration struct {
	Version    int
	Name       string
	Statements []string
	Apply      func(ctx context.Context, q querier) error
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
		{
			// D12（B-4/B-5）：memories / edges 主键改为 (id, version)，系统时间轴才真正可用；
			// 同时补上 nodes.source_episode（§2 第 1 条：nodes 也必须能溯源回 episode）。
			Version: 4,
			Name:    "p0_versioned_bitemporal",
			Apply:   applyVersionedBitemporal,
		},
		{
			// D13：memories_fts 的主键语义也必须是 (id, version)，否则 as_of 回溯
			// 会在词法通道上静默返回 0 行。v2 的 DDL 已发布、逐字节不得改，
			// 因此用 v5 重建虚表并把既有行回填为 version=1。
			Version: 5,
			Name:    "p0_versioned_fts",
			Apply:   applyVersionedFTS,
		},
		{
			// D15（B 方案）：concept_id 是**观察性引用**，不是准入门禁。
			// v1/v4 给 memories.concept_id 与 nodes.concept_id 加了外键，导致写入
			// 一个未声明的 concept 会 FOREIGN KEY constraint failed 并把整笔写入
			// （含 FTS）一起回滚——直接违背 D1/R1「本体不拒绝写入」。
			//
			// 同时补 concepts.origin：记录概念是显式声明还是写入路径隐式落成的草稿。
			// source_episode 的外键**必须保留**（血缘约束有意义），见 v1 的现状不动。
			Version: 6,
			Name:    "p1_concept_id_soft_ref",
			Apply:   applyConceptIDSoftRef,
		},
		{
			// P3（TD §7 §8）：生命周期状态 + op 台账 + 审计 + 重定向 + 概念使用统计。
			//
			// 为什么把这些放在独立表而不是往 memories 加列：
			//   - 运营态（status / 四个 importance 分项）**不是内容**，按 D13 不得催生版本；
			//   - audit_log 与 op_runs 是 Append-only 的审计事实，属于 §8 的独立产物；
			//   - memories 是 P0 已发布的表，加列会让 v1~v6 的既有行为与迁移指纹面临风险。
			//
			// 纯 DDL、全部 IF NOT EXISTS：schema_migrations 丢失后重跑不会炸库。
			Version: 7,
			Name:    "p3_lifecycle_ops_audit",
			Statements: []string{
				// memory_state：生命周期状态 + importance 四个分项各一列（TD §7 纪律 2）。
				// 存分项而不是只存总分：系数可以靠 REPLAY 重算，不必回扫全库（§6 同款理由）。
				`CREATE TABLE IF NOT EXISTS memory_state (
					memory_id               TEXT PRIMARY KEY,
					scope                   TEXT NOT NULL CHECK (scope IN ('user','agent','project','global')),
					status                  TEXT NOT NULL CHECK (status IN ('candidate','rejected','active','invalidated','stale','archived')),
					importance_base         REAL NOT NULL DEFAULT 0,
					importance_recency      REAL NOT NULL DEFAULT 1,
					importance_access_boost REAL NOT NULL DEFAULT 1,
					importance_grade        REAL NOT NULL DEFAULT 1,
					importance              REAL NOT NULL DEFAULT 0,
					status_changed_at       INTEGER NOT NULL DEFAULT 0,
					last_recalled_at        INTEGER NOT NULL DEFAULT 0,
					created_at              INTEGER NOT NULL DEFAULT 0,
					updated_at              INTEGER NOT NULL DEFAULT 0
				)`,
				`CREATE INDEX IF NOT EXISTS idx_memory_state_scope_status ON memory_state(scope, status)`,
				`CREATE INDEX IF NOT EXISTS idx_memory_state_status ON memory_state(status)`,

				// op_runs：幂等台账。主键就是幂等 key，**先插后跑**——并发的第二次提交
				// 在主键上直接失败，而不是靠「先查再插」的竞态窗口（TD §8 幂等要求）。
				`CREATE TABLE IF NOT EXISTS op_runs (
					key          TEXT PRIMARY KEY,
					op           TEXT NOT NULL,
					scope        TEXT NOT NULL CHECK (scope IN ('user','agent','project','global')),
					trigger_kind TEXT NOT NULL,
					actor        TEXT NOT NULL DEFAULT '',
					reason       TEXT NOT NULL DEFAULT '',
					status       TEXT NOT NULL CHECK (status IN ('running','applied','noop','duplicate','cancelled','failed')),
					started_at   INTEGER NOT NULL DEFAULT 0,
					finished_at  INTEGER NOT NULL DEFAULT 0,
					note         TEXT NOT NULL DEFAULT ''
				)`,
				`CREATE INDEX IF NOT EXISTS idx_op_runs_scope_op ON op_runs(scope, op, started_at)`,

				// audit_log：op_id · 触发源 · 时间 · before/after diff（TD §8）。
				// seq 自增而不是 (op_id, step_id) 做主键：同一步骤重跑要各留一条痕迹。
				`CREATE TABLE IF NOT EXISTS audit_log (
					seq          INTEGER PRIMARY KEY AUTOINCREMENT,
					op_id        TEXT NOT NULL,
					op           TEXT NOT NULL,
					scope        TEXT NOT NULL,
					trigger_kind TEXT NOT NULL,
					actor        TEXT NOT NULL DEFAULT '',
					reason       TEXT NOT NULL DEFAULT '',
					step_id      TEXT NOT NULL DEFAULT '',
					step_kind    TEXT NOT NULL DEFAULT '',
					subject      TEXT NOT NULL DEFAULT '',
					before_json  TEXT NOT NULL DEFAULT '',
					after_json   TEXT NOT NULL DEFAULT '',
					changed      INTEGER NOT NULL DEFAULT 0,
					at           INTEGER NOT NULL DEFAULT 0
				)`,
				`CREATE INDEX IF NOT EXISTS idx_audit_log_op_id ON audit_log(op_id, seq)`,
				`CREATE INDEX IF NOT EXISTS idx_audit_log_subject ON audit_log(subject, seq)`,

				// memory_redirects：Merge 之后旧 ID → 新 ID（TD §4「保留旧 ID 为重定向」）。
				// 旧 ID 不删除：它仍要在 as_of 回溯与审计里可解释。
				`CREATE TABLE IF NOT EXISTS memory_redirects (
					old_id TEXT PRIMARY KEY,
					new_id TEXT NOT NULL,
					scope  TEXT NOT NULL CHECK (scope IN ('user','agent','project','global')),
					reason TEXT NOT NULL DEFAULT '',
					at     INTEGER NOT NULL DEFAULT 0
				)`,
				`CREATE INDEX IF NOT EXISTS idx_memory_redirects_new ON memory_redirects(new_id)`,

				// concept_usage：D15 的 Prune 判定依据（最后使用时间 + 使用次数）。
				`CREATE TABLE IF NOT EXISTS concept_usage (
					concept_id   TEXT PRIMARY KEY,
					use_count    INTEGER NOT NULL DEFAULT 0,
					last_used_at INTEGER NOT NULL DEFAULT 0
				)`,

				// op_watermarks：阈值触发与「未整理条目」的进度水位（按 scope+op）。
				`CREATE TABLE IF NOT EXISTS op_watermarks (
					scope TEXT NOT NULL,
					op    TEXT NOT NULL,
					at    INTEGER NOT NULL DEFAULT 0,
					PRIMARY KEY (scope, op)
				)`,
			},
		},
		{
			// P3 修复（F-1 / F-4）：concepts 补 created_at 与 scope。
			//
			// # 为什么必须是新迁移
			//
			// v1 的 concepts DDL 已发布、逐字节不得改（publishedMigrations 的指纹钉死了它）。
			// 而这次新增的两列是 **Prune 判定的正确性前提**：
			//
			//	created_at  DraftTTL 判的是「活多久」。旧实现只有 concept_usage.last_used_at，
			//	            结构化写入根本不登记 usage → 缺失被当成 0 → 与 cutoff 相比恒真，
			//	            刚建的草稿首次 Prune 就被删（数据丢失级）。
			//	scope       Prune 的 draft 分支历史上没有 scope 隔离，agent scope 的清理会
			//	            删掉 user scope 的草稿（跨边界删除）。
			//
			// 用 Apply 型（ALTER TABLE ADD COLUMN）而不是重建：concepts 被 memories/nodes 的
			// concept_id 软引用，重建表的收益为零、风险不为零。列存在性判断保证重跑幂等。
			Version: 8,
			Name:    "p3_concept_created_at_scope",
			Apply:   applyConceptCreatedAtScope,
		},
		{
			// F-7 收口：duplicate 路径必须留下可追溯痕迹，且**不得新增任何行**。
			//
			// 受保护用例把两边都钉死了：duplicate 之后 audit_log 行数不变
			// （internal/p3verify/idempotent_test.go:76）且 op_runs 行数不变（:79）。
			// 因此唯一可行的形状是「就地改现有行的列」：命中 duplicate 时把
			// skip_count+1、last_skipped_at=now 写回它自己那一行。
			//
			// 为什么必须是追加迁移：op_runs 的 DDL 在 v1 里，已发布版本逐字节不得改
			// （publishedMigrations 的指纹钉死了 v1~v3，v6~v8 由名称/形态锁定）。
			Version: 9,
			Name:    "p3_op_runs_skip_counters",
			Apply:   applyOpRunsSkipCounters,
		},
	}
}

// applyOpRunsSkipCounters 给 op_runs 补 skip_count / last_skipped_at 两列。
//
// 语义：这两列只由「命中 duplicate」这一条路径累加（ops.Backend.MarkRunSkipped
// 的 UPDATE），它们回答的是「这一个幂等 key 被安全跳过了多少次、最后一次是什么时候」。
// 旧实现在 duplicate 分支既不 UpdateRun 也不 InsertRun，所以那张表上完全没有痕迹。
//
// 幂等性：schema_migrations 丢失后整个清单会重跑，因此逐列判断是否存在；
// 已存在的行保持 DEFAULT 0（= 「升级之前没有被跳过过」），不做任何回填。
func applyOpRunsSkipCounters(ctx context.Context, q querier) error {
	hasSkipCount, err := tableHasColumn(ctx, q, "op_runs", "skip_count")
	if err != nil {
		return err
	}
	if !hasSkipCount {
		if _, err := q.ExecContext(ctx,
			`ALTER TABLE op_runs ADD COLUMN skip_count INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("sqlite: migration 9 (op_runs.skip_count): %w", err)
		}
	}

	hasLastSkippedAt, err := tableHasColumn(ctx, q, "op_runs", "last_skipped_at")
	if err != nil {
		return err
	}
	if !hasLastSkippedAt {
		if _, err := q.ExecContext(ctx,
			`ALTER TABLE op_runs ADD COLUMN last_skipped_at INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("sqlite: migration 9 (op_runs.last_skipped_at): %w", err)
		}
	}
	return nil
}

// applyConceptCreatedAtScope 给 concepts 补 created_at / scope 两列并回填。
//
// 幂等性：schema_migrations 丢失后整个清单会被重跑，因此每一步都先判列是否存在；
// 回填只动「还是默认值」的行，不会把已归属的概念搬来搬去。
//
// 回填口径（刻意保守）：
//   - created_at = 迁移时刻（不是 0）。0 会被 TTL 判为「很久以前」，迁移边界上
//     存在未登记的旧草稿会被立刻清掉——那正是 F-1 要防的数据丢失；给它一个完整
//     TTL 宽限期，代价只是清理晚一个窗口。
//   - scope 取引用该概念的最近一条记忆的 scope；真正无人引用的旧概念保持 ”（无归属），
//     只由 global scope 的 Prune 处理（见 ops/prune_op.go 的隔离说明）。
func applyConceptCreatedAtScope(ctx context.Context, q querier) error {
	hasCreatedAt, err := tableHasColumn(ctx, q, "concepts", "created_at")
	if err != nil {
		return err
	}
	if !hasCreatedAt {
		if _, err := q.ExecContext(ctx,
			`ALTER TABLE concepts ADD COLUMN created_at INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("sqlite: migration 8 (concepts.created_at): %w", err)
		}
		if _, err := q.ExecContext(ctx,
			`UPDATE concepts SET created_at = ? WHERE created_at = 0`,
			time.Now().UnixMilli()); err != nil {
			return fmt.Errorf("sqlite: migration 8 (backfill created_at): %w", err)
		}
	}

	hasScope, err := tableHasColumn(ctx, q, "concepts", "scope")
	if err != nil {
		return err
	}
	if !hasScope {
		if _, err := q.ExecContext(ctx,
			`ALTER TABLE concepts ADD COLUMN scope TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("sqlite: migration 8 (concepts.scope): %w", err)
		}
		// 无归属概念从引用它的记忆继承边界（同一概念被多 scope 引用时取最近写入者）。
		if _, err := q.ExecContext(ctx, `
			UPDATE concepts SET scope = COALESCE((
				SELECT m.scope FROM memories m
				WHERE m.concept_id = concepts.id
				ORDER BY m.t_created DESC LIMIT 1
			), '') WHERE scope = ''`); err != nil {
			return fmt.Errorf("sqlite: migration 8 (backfill scope): %w", err)
		}
	}
	if _, err := q.ExecContext(ctx,
		`CREATE INDEX IF NOT EXISTS idx_concepts_scope_status ON concepts(scope, status)`); err != nil {
		return fmt.Errorf("sqlite: migration 8 (index): %w", err)
	}
	return nil
}

// ftsDDLWithVersion 生成带 version 列的 FTS5 虚表 DDL。
//
// version 也是 UNINDEXED：它只用于 join 与过滤，绝不参与 MATCH，
// 否则“30s”这种查询会意外命中版本号字符串。
func ftsDDLWithVersion(name string) string {
	return `CREATE VIRTUAL TABLE IF NOT EXISTS ` + name + ` USING fts5(
			id UNINDEXED,
			version UNINDEXED,
			scope UNINDEXED,
			body UNINDEXED,
			bigrams,
			tokenize = 'unicode61'
		)`
}

// applyVersionedFTS 把 memories_fts 重建为带 version 列的形态，既有行回填 version=1。
//
// 为什么重建而不是 ALTER：FTS5 虚表不支持 ADD COLUMN。
// 为什么不直接 DROP 重建空表：那会丢掉全部已发布的词法数据；
// 这里的 INSERT ... SELECT 把 id/scope/body/bigrams 原样搬过去，只补 version=1
// （v5 之前每个 id 在索引里只有一行）。
//
// 幂等性：schema_migrations 丢失后整个清单会重跑，因此先判断 version 列是否已存在，
// 已存在就跳过。每个迁移本身在单个事务里执行，因此不会留下“已 DROP 未 RENAME”的中间态。
func applyVersionedFTS(ctx context.Context, q querier) error {
	exists, err := tableExists(ctx, q, "memories_fts")
	if err != nil {
		return err
	}
	hasVersion, err := tableHasColumn(ctx, q, "memories_fts", "version")
	if err != nil {
		return err
	}
	if exists && hasVersion {
		return nil // 已是新形态
	}
	if !exists {
		// v2 已建表但被外力删除（本例不应发生），直接建成新形态。
		if _, err := q.ExecContext(ctx, ftsDDLWithVersion("memories_fts")); err != nil {
			return fmt.Errorf("sqlite: migration 5 (create memories_fts): %w", err)
		}
		return nil
	}

	stmts := []string{
		`DROP TABLE IF EXISTS memories_fts_v5`,
		ftsDDLWithVersion("memories_fts_v5"),
		`INSERT INTO memories_fts_v5 (id, version, scope, body, bigrams)
		 SELECT id, 1, scope, body, bigrams FROM memories_fts`,
		`DROP TABLE memories_fts`,
		`ALTER TABLE memories_fts_v5 RENAME TO memories_fts`,
	}
	for _, stmt := range stmts {
		if _, err := q.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("sqlite: migration 5 (versioned fts): %w", err)
		}
	}
	return nil
}

// applyVersionedBitemporal 重建 memories / edges 为 (id, version) 主键，并补 nodes.source_episode。
//
// 为什么必须重建表：SQLite 不支持改主键。用"建新表 → 拷数据 → 换名"三步，
// 既有数据全部落成 version=1（此前每个 id 只有一行）。
//
// 幂等性：整个清单在 schema_migrations 丢失后会被重跑，因此每步都先判断列是否存在，
// 已是新形态就跳过，绝不做第二次拷贝。
func applyVersionedBitemporal(ctx context.Context, q querier) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS memories_v4 (
			id             TEXT    NOT NULL,
			version        INTEGER NOT NULL DEFAULT 1,
			scope          TEXT    NOT NULL CHECK (scope IN ('user','agent','project','global')),
			concept_id     TEXT REFERENCES concepts(id),
			kind           TEXT    NOT NULL
			               CHECK (kind IN ('semantic','episodic','procedural','decision')),
			content        TEXT    NOT NULL,
			t_valid        INTEGER NOT NULL DEFAULT 0,
			t_invalid      INTEGER,
			t_created      INTEGER NOT NULL DEFAULT 0,
			t_expired      INTEGER,
			source_episode TEXT REFERENCES episodes(id),
			importance     REAL    NOT NULL DEFAULT 0.5,
			recall_count   INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (id, version)
		)`,
		`INSERT INTO memories_v4 (id, version, scope, concept_id, kind, content, t_valid, t_invalid,
			t_created, t_expired, source_episode, importance, recall_count)
		 SELECT id, 1, scope, concept_id, kind, content, t_valid, t_invalid,
			t_created, t_expired, source_episode, importance, recall_count
		 FROM memories WHERE true
		 ON CONFLICT(id, version) DO NOTHING`,
		`DROP TABLE IF EXISTS memories`,
		`ALTER TABLE memories_v4 RENAME TO memories`,
		`CREATE INDEX IF NOT EXISTS idx_memories_scope_bi ON memories(scope, t_invalid, t_valid)`,
		`CREATE INDEX IF NOT EXISTS idx_memories_concept_id ON memories(concept_id)`,
		`CREATE INDEX IF NOT EXISTS idx_memories_source_episode ON memories(source_episode)`,
		`CREATE INDEX IF NOT EXISTS idx_memories_current ON memories(id, t_expired)`,

		`CREATE TABLE IF NOT EXISTS edges_v4 (
			id         TEXT    NOT NULL,
			version    INTEGER NOT NULL DEFAULT 1,
			from_id    TEXT    NOT NULL REFERENCES nodes(id),
			to_id      TEXT    NOT NULL REFERENCES nodes(id),
			relation   TEXT    NOT NULL,
			weight     REAL    NOT NULL DEFAULT 1.0,
			t_valid    INTEGER NOT NULL DEFAULT 0,
			t_invalid  INTEGER,
			t_created  INTEGER NOT NULL DEFAULT 0,
			t_expired  INTEGER,
			PRIMARY KEY (id, version)
		)`,
		`INSERT INTO edges_v4 (id, version, from_id, to_id, relation, weight, t_valid, t_invalid,
			t_created, t_expired)
		 SELECT id, 1, from_id, to_id, relation, weight, t_valid, t_invalid,
			t_created, t_expired
		 FROM edges WHERE true
		 ON CONFLICT(id, version) DO NOTHING`,
		`DROP TABLE IF EXISTS edges`,
		`ALTER TABLE edges_v4 RENAME TO edges`,
		`CREATE INDEX IF NOT EXISTS idx_edges_from_rel ON edges(from_id, relation)`,
		`CREATE INDEX IF NOT EXISTS idx_edges_to ON edges(to_id)`,
		`CREATE INDEX IF NOT EXISTS idx_edges_rel_bi ON edges(relation, t_invalid)`,
		`CREATE INDEX IF NOT EXISTS idx_edges_current ON edges(id, t_expired)`,
	}

	// 已是新形态（memories/edges 都已有 version 列）时只做索引补齐，不重跑拷贝。
	memoriesDone, err := tableHasColumn(ctx, q, "memories", "version")
	if err != nil {
		return err
	}
	edgesDone, err := tableHasColumn(ctx, q, "edges", "version")
	if err != nil {
		return err
	}
	switch {
	case memoriesDone && edgesDone:
		// 只需保证索引存在（重复 CREATE INDEX IF NOT EXISTS 无害）。
		for _, stmt := range []string{
			`CREATE INDEX IF NOT EXISTS idx_memories_current ON memories(id, t_expired)`,
			`CREATE INDEX IF NOT EXISTS idx_edges_current ON edges(id, t_expired)`,
		} {
			if _, err := q.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("sqlite: migration 4 index: %w", err)
			}
		}
	case memoriesDone:
		// 半完成状态：只补 edges 部分。
		for _, stmt := range stmts[7:] {
			if _, err := q.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("sqlite: migration 4 (edges): %w", err)
			}
		}
	default:
		for _, stmt := range stmts {
			if _, err := q.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("sqlite: migration 4: %w", err)
			}
		}
	}

	// I-4：nodes 必须能溯源回 episode。ALTER TABLE ADD COLUMN 幂等由列判断保证。
	hasSourceEpisode, err := tableHasColumn(ctx, q, "nodes", "source_episode")
	if err != nil {
		return err
	}
	if !hasSourceEpisode {
		if _, err := q.ExecContext(ctx,
			`ALTER TABLE nodes ADD COLUMN source_episode TEXT REFERENCES episodes(id)`); err != nil {
			return fmt.Errorf("sqlite: migration 4 (nodes.source_episode): %w", err)
		}
	}
	return nil
}

// applyConceptIDSoftRef 是 D15 的 schema 侧：移除 memories.concept_id 与
// nodes.concept_id 的外键，保留 source_episode 的外键，并补 concepts.origin。
//
// 为什么必须重建表：SQLite 不支持 DROP CONSTRAINT。重建顺序是刻意的：
// nodes 被 edges/aliases 用外键引用，直接 DROP nodes 会让这两个子表的外键悬空，
// 提交时整笔事务报 FOREIGN KEY constraint failed（已实测）。
// 因此在同一事务内**先重建子表并把它们指向新父表**，再删旧父表、统一改名：
// 改名后 SQLite 会把子表的外键一并重定向到新表名，约束因此完整保留。
//
// 幂等性：schema_migrations 丢失后整个清单会被重跑，因此先查 concept_id 上
// 是否还有外键，没有就只补列与索引，绝不第二次拷贝数据。
func applyConceptIDSoftRef(ctx context.Context, q querier) error {
	memoriesFK, err := foreignKeyExists(ctx, q, "memories", "concept_id")
	if err != nil {
		return err
	}
	nodesFK, err := foreignKeyExists(ctx, q, "nodes", "concept_id")
	if err != nil {
		return err
	}

	if memoriesFK {
		if err := rebuildTable(ctx, q, "memories", memoriesV6DDL("memories_v6"), memoryCols); err != nil {
			return fmt.Errorf("sqlite: migration 6 (memories): %w", err)
		}
	}
	if nodesFK {
		if err := rebuildGraphTables(ctx, q); err != nil {
			return fmt.Errorf("sqlite: migration 6 (graph): %w", err)
		}
	}

	// 索引在重建后需要补回（重复 CREATE INDEX IF NOT EXISTS 无害）。
	for _, stmt := range memoriesIndexes {
		if _, err := q.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("sqlite: migration 6 index: %w", err)
		}
	}

	// concepts.origin：ALTER TABLE ADD COLUMN 幂等由列判断保证。
	hasOrigin, err := tableHasColumn(ctx, q, "concepts", "origin")
	if err != nil {
		return err
	}
	if !hasOrigin {
		if _, err := q.ExecContext(ctx,
			`ALTER TABLE concepts ADD COLUMN origin TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("sqlite: migration 6 (concepts.origin): %w", err)
		}
	}
	return nil
}

// memoriesV6DDL 返回与 v4 同构、但 concept_id 不再带外键的 memories 表 DDL。
// source_episode 的外键刻意保留（D15 明确要求）。
func memoriesV6DDL(table string) string {
	return `CREATE TABLE ` + table + ` (
	id             TEXT    NOT NULL,
	version        INTEGER NOT NULL DEFAULT 1,
	scope          TEXT    NOT NULL CHECK (scope IN ('user','agent','project','global')),
	concept_id     TEXT,
	kind           TEXT    NOT NULL
	               CHECK (kind IN ('semantic','episodic','procedural','decision')),
	content        TEXT    NOT NULL,
	t_valid        INTEGER NOT NULL DEFAULT 0,
	t_invalid      INTEGER,
	t_created      INTEGER NOT NULL DEFAULT 0,
	t_expired      INTEGER,
	source_episode TEXT REFERENCES episodes(id),
	importance     REAL    NOT NULL DEFAULT 0.5,
	recall_count   INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (id, version)
)`
}

var memoriesIndexes = []string{
	`CREATE INDEX IF NOT EXISTS idx_memories_scope_bi ON memories(scope, t_invalid, t_valid)`,
	`CREATE INDEX IF NOT EXISTS idx_memories_concept_id ON memories(concept_id)`,
	`CREATE INDEX IF NOT EXISTS idx_memories_source_episode ON memories(source_episode)`,
	`CREATE INDEX IF NOT EXISTS idx_memories_current ON memories(id, t_expired)`,
}

// rebuildTable 用"建新表 → 拷数据 → 删旧表 → 改名"重建一张表。
//
// 表名只来自本包常量（不拼接外部输入），因此这里直接用字符串拼 SQL。
func rebuildTable(ctx context.Context, q querier, name, ddl, cols string) error {
	tmp := name + "_v6"
	stmts := []string{
		`DROP TABLE IF EXISTS ` + tmp,
		ddl,
		`INSERT INTO ` + tmp + ` (` + cols + `) SELECT ` + cols + ` FROM ` + name,
		`DROP TABLE ` + name,
		`ALTER TABLE ` + tmp + ` RENAME TO ` + name,
	}
	for _, stmt := range stmts {
		if _, err := q.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

// rebuildGraphTables 重建 nodes（去掉 concept_id 外键）及其两个子表 edges/aliases。
//
// 子表必须先指向**新**父表再删旧父表：否则 DROP nodes 时子表外键指向不存在的表，
// 整个事务在提交时失败。这里用同一套三步法，父表改名后子表的外键自动重定向。
func rebuildGraphTables(ctx context.Context, q querier) error {
	stmts := []string{
		`DROP TABLE IF EXISTS nodes_v6`,
		`CREATE TABLE nodes_v6 (
			id             TEXT PRIMARY KEY,
			scope          TEXT NOT NULL CHECK (scope IN ('user','agent','project','global')),
			concept_id     TEXT,
			canonical_name TEXT NOT NULL,
			props_json     TEXT NOT NULL DEFAULT '{}',
			source_episode TEXT REFERENCES episodes(id)
		)`,
		`INSERT INTO nodes_v6 (id, scope, concept_id, canonical_name, props_json, source_episode)
		 SELECT id, scope, concept_id, canonical_name, props_json, source_episode FROM nodes`,
		`DROP TABLE IF EXISTS edges_v6`,
		`CREATE TABLE edges_v6 (
			id         TEXT    NOT NULL,
			version    INTEGER NOT NULL DEFAULT 1,
			from_id    TEXT    NOT NULL REFERENCES nodes_v6(id),
			to_id      TEXT    NOT NULL REFERENCES nodes_v6(id),
			relation   TEXT    NOT NULL,
			weight     REAL    NOT NULL DEFAULT 1.0,
			t_valid    INTEGER NOT NULL DEFAULT 0,
			t_invalid  INTEGER,
			t_created  INTEGER NOT NULL DEFAULT 0,
			t_expired  INTEGER,
			PRIMARY KEY (id, version)
		)`,
		`INSERT INTO edges_v6 (id, version, from_id, to_id, relation, weight, t_valid, t_invalid,
			t_created, t_expired)
		 SELECT id, version, from_id, to_id, relation, weight, t_valid, t_invalid,
			t_created, t_expired FROM edges`,
		`DROP TABLE IF EXISTS aliases_v6`,
		`CREATE TABLE aliases_v6 (
			surface    TEXT NOT NULL,
			node_id    TEXT NOT NULL REFERENCES nodes_v6(id),
			source     TEXT NOT NULL DEFAULT '',
			confidence REAL NOT NULL DEFAULT 0,
			PRIMARY KEY (surface, node_id)
		)`,
		`INSERT INTO aliases_v6 (surface, node_id, source, confidence)
		 SELECT surface, node_id, source, confidence FROM aliases`,
		// 先删子表，再删父表：父表被引用时直接 DROP 会违反外键。
		`DROP TABLE edges`,
		`DROP TABLE aliases`,
		`DROP TABLE nodes`,
		`ALTER TABLE nodes_v6 RENAME TO nodes`,
		`ALTER TABLE edges_v6 RENAME TO edges`,
		`ALTER TABLE aliases_v6 RENAME TO aliases`,
		`CREATE INDEX IF NOT EXISTS idx_nodes_scope_name ON nodes(scope, canonical_name)`,
		`CREATE INDEX IF NOT EXISTS idx_nodes_concept_id ON nodes(concept_id)`,
		`CREATE INDEX IF NOT EXISTS idx_edges_from_rel ON edges(from_id, relation)`,
		`CREATE INDEX IF NOT EXISTS idx_edges_to ON edges(to_id)`,
		`CREATE INDEX IF NOT EXISTS idx_edges_rel_bi ON edges(relation, t_invalid)`,
		`CREATE INDEX IF NOT EXISTS idx_edges_current ON edges(id, t_expired)`,
		`CREATE INDEX IF NOT EXISTS idx_aliases_node_id ON aliases(node_id)`,
	}
	for _, stmt := range stmts {
		if _, err := q.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

// foreignKeyExists 报告 table 的 column 上是否存在外键约束。
func foreignKeyExists(ctx context.Context, q querier, table, column string) (bool, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT COUNT(1) FROM pragma_foreign_key_list(?) WHERE "from" = ?`, table, column)
	if err != nil {
		return false, fmt.Errorf("sqlite: fk list %s.%s: %w", table, column, err)
	}
	defer rows.Close()
	var n int
	if rows.Next() {
		if err := rows.Scan(&n); err != nil {
			return false, fmt.Errorf("sqlite: scan fk list %s.%s: %w", table, column, err)
		}
	}
	return n > 0, rows.Err()
}

// tableExists 报告表（含虚表）是否存在。
func tableExists(ctx context.Context, q querier, table string) (bool, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT COUNT(1) FROM sqlite_master WHERE name = ? AND type IN ('table','view')`, table)
	if err != nil {
		return false, fmt.Errorf("sqlite: table_exists %s: %w", table, err)
	}
	defer rows.Close()
	var n int
	if rows.Next() {
		if err := rows.Scan(&n); err != nil {
			return false, fmt.Errorf("sqlite: scan table_exists %s: %w", table, err)
		}
	}
	return n > 0, rows.Err()
}

// tableHasColumn 报告 table 是否有 column。表不存在时返回 false。
func tableHasColumn(ctx context.Context, q querier, table, column string) (bool, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT COUNT(1) FROM pragma_table_info(?) WHERE name = ?`, table, column)
	if err != nil {
		return false, fmt.Errorf("sqlite: table_info %s: %w", table, err)
	}
	defer rows.Close()
	var n int
	if rows.Next() {
		if err := rows.Scan(&n); err != nil {
			return false, fmt.Errorf("sqlite: scan table_info %s: %w", table, err)
		}
	}
	return n > 0, rows.Err()
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

// beginImmediate 在一条专用连接上开启 BEGIN IMMEDIATE 事务，返回执行句柄与结束函数。
//
// 为什么不能用 database/sql 的 d.db.BeginTx(ctx, nil)：它发的是 BEGIN（DEFERRED）。
// DEFERRED 事务先只拿读锁，直到第一条写语句才尝试升级为写锁；SQLite 对"锁升级"
// 失败**立即**返回 SQLITE_BUSY，busy_timeout 的忙等只对一开始就要写锁的情形生效
// （否则两个 DEFERRED 事务互相等待升级会真死锁）。两个独立 handle 并发迁移时，
// 后到者必然踩这个立即失败——这就是 TestMigrateConcurrentSafe 报
// `migration 5 (p0_versioned_fts): database is locked (5) (SQLITE_BUSY)` 的根因。
// migration 4/5 都是"建新表→COPY→DROP→RENAME"的整表重建（长写），争用窗口很大。
//
// BEGIN IMMEDIATE 一开始就取写锁（RESERVED），拿不到时由 busy_timeout 忙等，
// 因此并发迁移变成"排队"而不是"报错"。写操作在事务内不再需要升级锁。
func (d *DB) beginImmediate(ctx context.Context) (querier, func(commit bool) error, error) {
	conn, err := d.db.Conn(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("sqlite: acquire conn: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("sqlite: begin immediate: %w", err)
	}
	done := func(commit bool) error {
		defer func() { _ = conn.Close() }()
		stmt := `ROLLBACK`
		if commit {
			stmt = `COMMIT`
		}
		// 收尾不受调用方 ctx 取消影响，否则回滚/提交会被跳过并泄漏事务。
		if _, err := conn.ExecContext(context.WithoutCancel(ctx), stmt); err != nil {
			return fmt.Errorf("sqlite: %s: %w", strings.ToLower(stmt), err)
		}
		return nil
	}
	return conn, done, nil
}

func (d *DB) applyMigration(ctx context.Context, m migration) error {
	q, done, err := d.beginImmediate(ctx)
	if err != nil {
		return fmt.Errorf("sqlite: begin migration %d: %w", m.Version, err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = done(false)
		}
	}()

	// 拿到写锁后重新确认：等锁期间可能已有另一个 handle 应用完这一版。
	// 这样既不做重复的长写重建，也保证 schema_migrations 每版恰好一行。
	var already int
	if err := q.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM schema_migrations WHERE version = ?`, m.Version,
	).Scan(&already); err != nil {
		return fmt.Errorf("sqlite: check migration %d: %w", m.Version, err)
	}
	if already > 0 {
		if err := done(false); err != nil {
			return err
		}
		committed = true
		return nil
	}

	for _, stmt := range m.Statements {
		if _, err := q.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("sqlite: migration %d (%s): %w", m.Version, m.Name, err)
		}
	}
	if m.Apply != nil {
		if err := m.Apply(ctx, q); err != nil {
			return fmt.Errorf("sqlite: migration %d (%s): %w", m.Version, m.Name, err)
		}
	}
	// INSERT OR IGNORE：两个进程并发迁移时，后到者不报错（DDL 本身也已 IF NOT EXISTS）。
	if _, err := q.ExecContext(ctx,
		`INSERT OR IGNORE INTO schema_migrations(version, name, applied_at) VALUES (?, ?, ?)`,
		m.Version, m.Name, time.Now().Unix(),
	); err != nil {
		return fmt.Errorf("sqlite: record migration %d: %w", m.Version, err)
	}
	if err := done(true); err != nil {
		return fmt.Errorf("sqlite: commit migration %d: %w", m.Version, err)
	}
	committed = true
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
