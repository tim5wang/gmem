package sqlite

import (
	"context"
	"fmt"
	"strings"

	"github.com/tim5wang/gmem/bigram"
	"github.com/tim5wang/gmem/store"
)

// lexicalIndex 是 LexicalIndex 的 FTS5 实现。
//
// 核心不变量（P0 验收）：**索引侧与查询侧走同一个 bigram.Tokens**。
//   - 写入：bigram.IndexText(text) 落进 memories_fts.bigrams 列；
//   - 查询：bigram.QueryText(q) 生成 MATCH 表达式。
//
// 任何一侧单独改分词规则都会让"认证"这种 2 字查询失效，专项测试
// TestBigramBothSidesSymmetric 与 TestLexicalSearchChineseTwoChar 会立刻报错。
//
// 版本语义（D13）：memories_fts 的主键语义是 **(id, version)**（FTS5 本身没有主键，
// 由"按 (id, version) 先删后插"保证唯一）。旧版本必须继续留在索引里，
// 否则 as_of 回溯查询会在词法通道上静默返回 0 行——那正是 D12 承诺的反面。
//
// 事务：执行句柄来自 ctx（见 conn.querier）。在 Store.Tx 回调内调用时，
// 索引写入落到同一事务；不在事务内时自建一个，保证"按 (id, version) 先删后插"的原子性。
type lexicalIndex struct{ c *conn }

var _ store.LexicalIndex = (*lexicalIndex)(nil)

// Index 写入或覆盖文档，覆盖粒度是 (id, version)。
//
// 刻意**不再** `DELETE ... WHERE id = ?`：那会把同 ID 的其它版本一起删掉，
// 使 as_of 回溯丢失词法通道（D13 缺口 1）。
func (l *lexicalIndex) Index(ctx context.Context, docs []store.Document) error {
	if len(docs) == 0 {
		return nil
	}
	if _, err := l.c.querier(ctx); err != nil {
		return err
	}
	return l.c.runInTx(ctx, func(tctx context.Context, q querier) error {
		for _, d := range docs {
			if d.ID == "" {
				return fmt.Errorf("sqlite: lexical index: empty document id")
			}
			version := d.Key().VersionOr1()
			if _, err := q.ExecContext(tctx,
				`DELETE FROM memories_fts WHERE id = ? AND version = ?`, d.ID, version); err != nil {
				return fmt.Errorf("sqlite: lexical index delete %q v%d: %w", d.ID, version, err)
			}
			// 索引侧 bigram 预转换：与查询侧共用 bigram.IndexText。
			if _, err := q.ExecContext(tctx,
				`INSERT INTO memories_fts (id, version, scope, body, bigrams) VALUES (?, ?, ?, ?, ?)`,
				d.ID, version, string(d.Scope), d.Text, bigram.IndexText(d.Text)); err != nil {
				return fmt.Errorf("sqlite: lexical index insert %q v%d: %w", d.ID, version, err)
			}
		}
		return nil
	})
}

// Delete 按 (id, version) 精确删除文档。
//
// 只删指明的版本：同一 ID 的其它版本必须原样保留（D13）。
func (l *lexicalIndex) Delete(ctx context.Context, keys []store.DocumentKey) error {
	if len(keys) == 0 {
		return nil
	}
	if _, err := l.c.querier(ctx); err != nil {
		return err
	}
	return l.c.runInTx(ctx, func(tctx context.Context, q querier) error {
		for _, k := range keys {
			if k.ID == "" {
				return fmt.Errorf("sqlite: lexical delete: empty document id")
			}
			version := k.VersionOr1()
			if _, err := q.ExecContext(tctx,
				`DELETE FROM memories_fts WHERE id = ? AND version = ?`, k.ID, version); err != nil {
				return fmt.Errorf("sqlite: lexical delete %q v%d: %w", k.ID, version, err)
			}
		}
		return nil
	})
}

// Search 用 bm25 排序返回命中。
//
// 查询侧 bigram 预转换：bigram.QueryText 产出 `"认证" OR "证令" OR "令牌"` 形式的表达式。
// token 之间用 OR 而非 AND：bigram 分词会带来大量伪相邻二元组（"证令"），
// 用 AND 会让"认证令牌"这类真实查询直接零命中；改用 OR 后由 bm25 把
// 命中更多 token 的文档排到前面，融合侧只看 rank，因此不影响 RRF 的正确性。
//
// 双时态过滤（D13）：
//   - 默认（AsOf 为 nil）只召回**当前版本**，即主表 t_expired IS NULL；
//   - AsOf 非 nil 时按系统时间轴回溯：t_created <= AsOf < t_expired（t_expired 可空）。
//
// 用 LEFT JOIN 而不是 INNER JOIN：词法索引是独立可写的派生索引，
// 允许存在“已索引但主表尚无对应行”的文档（P0 测试与将来的纯文本索引都如此）。
// 这类行在默认查询里保留（没有系统时间轴可判），
// 但在 as_of 回溯时**不保留**——没有 t_created/t_expired 就无法断言它当时存在，
// 静默当成命中比漏召回更危险。主表有行时，它是有效性的唯一权威。
func (l *lexicalIndex) Search(ctx context.Context, lq store.LexicalQuery) ([]store.Hit, error) {
	expr := bigram.QueryText(lq.Query)
	if expr == "" {
		return nil, nil
	}
	limit := lq.Limit
	if limit <= 0 {
		limit = 10
	}
	q, err := l.c.querier(ctx)
	if err != nil {
		return nil, err
	}

	// memories_fts 是虚表，bm25() 必须在 WHERE 子句里且作用于该表名，
	// 因此用子查询把 bm25 算完再 join 主表做有效性过滤。
	var sqlText strings.Builder
	sqlText.WriteString(` SELECT f.id, f.version, f.score FROM (
		SELECT id, COALESCE(version, 1) AS version, bm25(memories_fts) AS score FROM memories_fts
		WHERE memories_fts MATCH ?`)
	// COALESCE(version, 1)：v5 之前的写入若没带版本列（或外部写入者漏填），
	// 按“第一版”解释，与 v5 的回填口径一致；绝不把 NULL 当成“命中未知版本”。
	args := []any{expr}
	if lq.Scope != "" {
		sqlText.WriteString(` AND scope = ?`)
		args = append(args, string(lq.Scope))
	}
	sqlText.WriteString(`) AS f LEFT JOIN memories AS m ON m.id = f.id AND m.version = f.version WHERE `)
	if lq.AsOf != nil {
		sqlText.WriteString(`m.id IS NOT NULL
			AND m.t_created <= ? AND (m.t_expired IS NULL OR m.t_expired > ?)`)
		args = append(args, *lq.AsOf, *lq.AsOf)
	} else {
		sqlText.WriteString(`(m.id IS NULL OR m.t_expired IS NULL)`)
	}
	sqlText.WriteString(` ORDER BY f.score ASC, f.id ASC, f.version ASC LIMIT ?`)
	args = append(args, limit)

	rows, err := q.QueryContext(ctx, sqlText.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: lexical search %q: %w", lq.Query, err)
	}
	defer rows.Close()

	var hits []store.Hit
	for rows.Next() {
		var id string
		var version int
		var bm25 float64
		if err := rows.Scan(&id, &version, &bm25); err != nil {
			return nil, fmt.Errorf("sqlite: lexical search scan: %w", err)
		}
		// bm25() 越小越相关，转成"越大越相关"便于 explain 阅读。
		hits = append(hits, store.Hit{
			ID: id, Version: version, Score: -bm25, Rank: len(hits) + 1,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: lexical search iterate: %w", err)
	}
	return hits, nil
}
