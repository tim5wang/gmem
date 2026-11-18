package ops

import (
	"context"
	"testing"
	"time"

	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// F-11 的**包内**回归：load() 的 recency 单位错误
//
// # 为什么必须直接断言 load()，而不是只看落库结果
//
// 落库路径会经过 recalc()，而 recalc 用的是 LastRecalledAt（这个字段在 load 里
// 本来就是 *1000 的毫秒），因此它会把 load 算错的 recency 覆盖掉。
// 也就是说：**只看库里的行，永远发现不了 load 的单位错误**——这正是这个缺陷
// 当时能活下来的原因（review 的原话是"仅因 recalc 自愈而未爆"）。
//
// 变异点：把 `recencyDecay(now, m.TValid*1000, ...)` 改回 `m.TValid` → 本用例必红。
// ===========================================================================

// emptyStateBackend 让 GetState 永远"查无此行"，从而迫使 load() 走默认推导分支
// （这正是单位错误所在的那条路径）。
type emptyStateBackend struct {
	Backend
}

func (emptyStateBackend) GetState(ctx context.Context, memoryID string) (State, bool, error) {
	return State{}, false, nil
}

// loadProbeTx 是最小 TxContext：只实现 load() 真正用到的那几个方法。
type loadProbeTx struct {
	TxContext
	be  Backend
	cfg Config
	now time.Time
}

func (p *loadProbeTx) Context() context.Context { return context.Background() }
func (p *loadProbeTx) Backend() Backend         { return p.be }
func (p *loadProbeTx) Cfg() Config              { return p.cfg }
func (p *loadProbeTx) Now() time.Time           { return p.now }

// TestLoadRecencyUsesMillis 是 F-11 的回归。
func TestLoadRecencyUsesMillis(t *testing.T) {
	cfg := DefaultConfig()
	now := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

	txc := &loadProbeTx{be: emptyStateBackend{}, cfg: cfg, now: now}
	sm := stateManager{txc: txc}

	// 一条"刚刚写入"的记忆：TValid 是**秒**（store 的时钟单位）。
	m := store.Memory{
		ID: "m-fresh", Scope: store.ScopeUser, Kind: store.KindSemantic,
		Content:    "刚刚写入的记忆正文（长度充足）",
		TValid:     now.Unix(), // 秒
		TCreated:   now.Unix(),
		Importance: 0.5,
	}

	st, existed := sm.load(m)
	if existed {
		t.Fatal("前置不成立：后端不应有状态行")
	}

	// 秒当毫秒时 age≈1.8e12ms → 命中 DecayFloor（0.05）。
	if st.Recency < 0.99 {
		t.Errorf("刚写入的记忆 recency=%.4f, want ≈1.0（TValid 是秒，必须 *1000 再算衰减；F-11）",
			st.Recency)
	}
	if st.Importance <= cfg.StaleBelow {
		t.Errorf("importance=%.4f 已 <= StaleBelow(%.2f)：这条记忆会被下一次 Consolidate 判成 Stale（F-11）",
			st.Importance, cfg.StaleBelow)
	}

	// 反向对照：TValid 为 0（未知时刻）时 recency 必须是 1，不得命下界。
	zero := m
	zero.TValid = 0
	if st0, _ := sm.load(zero); st0.Recency != 1 {
		t.Errorf("TValid=0 时 recency=%.4f, want 1（未知时刻不衰减）", st0.Recency)
	}

	// 正向对照：确实很旧的记忆必须衰减（证明上面不是"恒 1"的假绿）。
	old := m
	old.TValid = now.Add(-90 * 24 * time.Hour).Unix()
	if stOld, _ := sm.load(old); stOld.Recency >= 0.5 {
		t.Errorf("90 天前的记忆 recency=%.4f, want < 0.5（衰减未生效，断言没有判别力）",
			stOld.Recency)
	}
}
