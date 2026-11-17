package sqlite

import (
	"context"
	"fmt"

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
// 事务：执行句柄来自 ctx（见 conn.querier）。在 Store.Tx 回调内调用时，
// 索引写入落到同一事务；不在事务内时自建一个，保证"按 id 先删后插"的原子性。
type lexicalIndex struct{ c *conn }

var _ store.LexicalIndex = (*lexicalIndex)(nil)

// Index 写入或覆盖文档。FTS5 无 PRIMARY KEY，所以先按 id 删除再插入。
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
			if _, err := q.ExecContext(tctx, `DELETE FROM memories_fts WHERE id = ?`, d.ID); err != nil {
				return fmt.Errorf("sqlite: lexical index delete %q: %w", d.ID, err)
			}
			// 索引侧 bigram 预转换：与查询侧共用 bigram.IndexText。
			if _, err := q.ExecContext(tctx,
				`INSERT INTO memories_fts (id, scope, body, bigrams) VALUES (?, ?, ?, ?)`,
				d.ID, string(d.Scope), d.Text, bigram.IndexText(d.Text)); err != nil {
				return fmt.Errorf("sqlite: lexical index insert %q: %w", d.ID, err)
			}
		}
		return nil
	})
}

// Delete 按 ID 删除文档。
func (l *lexicalIndex) Delete(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := l.c.querier(ctx); err != nil {
		return err
	}
	return l.c.runInTx(ctx, func(tctx context.Context, q querier) error {
		for _, id := range ids {
			if _, err := q.ExecContext(tctx, `DELETE FROM memories_fts WHERE id = ?`, id); err != nil {
				return fmt.Errorf("sqlite: lexical delete %q: %w", id, err)
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
func (l *lexicalIndex) Search(ctx context.Context, query string, scope store.Scope, limit int) ([]store.Hit, error) {
	expr := bigram.QueryText(query)
	if expr == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	q, err := l.c.querier(ctx)
	if err != nil {
		return nil, err
	}
	sqlText := `SELECT id, bm25(memories_fts) AS s FROM memories_fts WHERE memories_fts MATCH ?`
	args := []any{expr}
	if scope != "" {
		sqlText += ` AND scope = ?`
		args = append(args, string(scope))
	}
	sqlText += ` ORDER BY s ASC, id ASC LIMIT ?`
	args = append(args, limit)

	rows, err := q.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: lexical search %q: %w", query, err)
	}
	defer rows.Close()

	var hits []store.Hit
	for rows.Next() {
		var id string
		var bm25 float64
		if err := rows.Scan(&id, &bm25); err != nil {
			return nil, fmt.Errorf("sqlite: lexical search scan: %w", err)
		}
		// bm25() 越小越相关，转成"越大越相关"便于 explain 阅读。
		hits = append(hits, store.Hit{ID: id, Score: -bm25, Rank: len(hits) + 1})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: lexical search iterate: %w", err)
	}
	return hits, nil
}
