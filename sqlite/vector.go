package sqlite

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"sort"

	"github.com/tim5wang/gmem/store"
)

// vectorIndex 是 VectorIndex 的实现：int8 量化 BLOB 快照 + Go 侧暴力 KNN。
//
// 之所以不用扩展：SQLite 没有原生向量类型，P0 也不该为"可能增长"提前付代价
// （§11 "何时才真的该迁移"）。暴力 KNN 在 P0 的规模下足够，且不引入 CGO。
type vectorIndex struct{ q querier }

var _ store.VectorIndex = (*vectorIndex)(nil)

// quantize 把 float32 向量压成 int8，返回 scale 与字节。
// scale = maxAbs/127，保证最大分量落在 int8 范围内；全零向量 scale=1 避免除零。
func quantize(v []float32) (float64, []byte) {
	var maxAbs float64
	for _, f := range v {
		if a := math.Abs(float64(f)); a > maxAbs {
			maxAbs = a
		}
	}
	scale := maxAbs / 127.0
	if scale == 0 {
		scale = 1
	}
	buf := make([]byte, len(v))
	for i, f := range v {
		q := int(math.Round(float64(f) / scale))
		if q > 127 {
			q = 127
		} else if q < -127 {
			q = -127
		}
		buf[i] = byte(int8(q))
	}
	return scale, buf
}

// dequantize 还原为 float32 向量。
func dequantize(scale float64, data []byte) []float32 {
	out := make([]float32, len(data))
	for i, b := range data {
		out[i] = float32(float64(int8(b)) * scale)
	}
	return out
}

func (v *vectorIndex) Upsert(ctx context.Context, vectors []store.Vector) error {
	for _, vec := range vectors {
		if vec.ID == "" {
			return fmt.Errorf("sqlite: vector upsert: empty id")
		}
		scale, data := quantize(vec.Values)
		if _, err := v.q.ExecContext(ctx, `
			INSERT INTO vec_blob (id, scope, dim, scale, data) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				scope=excluded.scope, dim=excluded.dim, scale=excluded.scale, data=excluded.data`,
			vec.ID, string(vec.Scope), len(vec.Values), scale, data,
		); err != nil {
			return fmt.Errorf("sqlite: vector upsert %q: %w", vec.ID, err)
		}
	}
	return nil
}

func (v *vectorIndex) Delete(ctx context.Context, ids []string) error {
	for _, id := range ids {
		if _, err := v.q.ExecContext(ctx, `DELETE FROM vec_blob WHERE id = ?`, id); err != nil {
			return fmt.Errorf("sqlite: vector delete %q: %w", id, err)
		}
	}
	return nil
}

// Search 对 scope 内所有量化向量做暴力余弦相似度，返回 top-k。
//
// 维度不一致的向量直接跳过（不报错）：重建中的部分索引不该让整次召回失败。
func (v *vectorIndex) Search(ctx context.Context, query []float32, scope store.Scope, k int) ([]store.Hit, error) {
	if len(query) == 0 {
		return nil, nil
	}
	if k <= 0 {
		k = 10
	}
	sqlText := `SELECT id, dim, scale, data FROM vec_blob`
	var args []any
	if scope != "" {
		sqlText += ` WHERE scope = ?`
		args = append(args, string(scope))
	}
	rows, err := v.q.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: vector search: %w", err)
	}
	defer rows.Close()

	type scored struct {
		id string
		s  float64
	}
	var all []scored
	for rows.Next() {
		var id string
		var dim int
		var scale float64
		var data []byte
		if err := rows.Scan(&id, &dim, &scale, &data); err != nil {
			return nil, fmt.Errorf("sqlite: vector search scan: %w", err)
		}
		if dim != len(query) {
			continue
		}
		all = append(all, scored{id: id, s: cosine(query, dequantize(scale, data))})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: vector search iterate: %w", err)
	}

	sort.Slice(all, func(i, j int) bool {
		if all[i].s != all[j].s {
			return all[i].s > all[j].s
		}
		return all[i].id < all[j].id
	})
	if len(all) > k {
		all = all[:k]
	}
	hits := make([]store.Hit, len(all))
	for i, s := range all {
		hits[i] = store.Hit{ID: s.id, Score: s.s, Rank: i + 1}
	}
	return hits, nil
}

// Snapshot 导出该 scope 下全部量化向量，作为可重建的加速物。
func (v *vectorIndex) Snapshot(ctx context.Context, scope store.Scope) ([]byte, error) {
	sqlText := `SELECT id, dim, scale, data FROM vec_blob`
	var args []any
	if scope != "" {
		sqlText += ` WHERE scope = ?`
		args = append(args, string(scope))
	}
	sqlText += ` ORDER BY id ASC`
	rows, err := v.q.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: vector snapshot: %w", err)
	}
	defer rows.Close()

	var buf bytes.Buffer
	for rows.Next() {
		var id string
		var dim int
		var scale float64
		var data []byte
		if err := rows.Scan(&id, &dim, &scale, &data); err != nil {
			return nil, fmt.Errorf("sqlite: vector snapshot scan: %w", err)
		}
		// 行格式：idLen(4) id dim(4) scale(8) dataLen(4) data
		_ = binary.Write(&buf, binary.LittleEndian, int32(len(id)))
		buf.WriteString(id)
		_ = binary.Write(&buf, binary.LittleEndian, int32(dim))
		_ = binary.Write(&buf, binary.LittleEndian, scale)
		_ = binary.Write(&buf, binary.LittleEndian, int32(len(data)))
		buf.Write(data)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: vector snapshot iterate: %w", err)
	}
	return buf.Bytes(), nil
}

func cosine(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		av, bv := float64(a[i]), float64(b[i])
		dot += av * bv
		na += av * av
		nb += bv * bv
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
