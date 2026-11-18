package ops

import (
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// 生命周期（docs/TD.md §7）
//
// 状态机：
//
//	[*] -> Candidate -> Rejected
//	              \--> Active -> Invalidated -> Archived -> Prune(物理删除)
//	                        \--> Stale ----\--> Archived
//
// 四条纪律（本文件是它们的落地点）：
//  1. Invalidated **不等于删除**：只置 t_invalid，as_of 仍可回溯；
//  2. importance 分项存储（base × recency × access_boost × grade 各一列）；
//  3. Prune 是唯一物理删除，默认只作用于 Archived（见 prune_op.go）；
//  4. Rejected 也落库（"为什么没记住这条"必须可回答）。
// ===========================================================================

// transitions 是合法状态迁移表。任何不在表里的迁移都是缺陷，必须报错而不是静默接受。
var transitions = map[MemoryStatus][]MemoryStatus{
	StatusCandidate:   {StatusActive, StatusRejected},
	StatusRejected:    nil, // 终态：拒写就是拒写，不复活
	StatusActive:      {StatusInvalidated, StatusStale},
	StatusInvalidated: {StatusActive, StatusArchived}, // 回滚与保留期满
	StatusStale:       {StatusArchived, StatusActive},
	StatusArchived:    nil, // 只能被 Prune 删除
}

// ValidateTransition 校验状态迁移是否合法。
func ValidateTransition(from, to MemoryStatus) error {
	if from == to {
		return nil
	}
	for _, ok := range transitions[from] {
		if ok == to {
			return nil
		}
	}
	return fmt.Errorf("ops: 非法状态迁移 %s → %s（TD §7 状态机）", from, to)
}

// composeImportance 是 TD §7 纪律 2 的公式：四项相乘。
func composeImportance(base, recency, accessBoost, grade float64) float64 {
	return base * recency * accessBoost * grade
}

// recencyDecay 是 recency 分项：按半衰期衰减，带下界。
//
// 下界是刻意的（与 recall 的 RecencyFloor 同思路）：旧而正确的记忆不该被压没。
func recencyDecay(now, lastAt int64, halfLife time.Duration, floor float64) float64 {
	if lastAt <= 0 || halfLife <= 0 {
		return 1
	}
	age := time.Duration(now-lastAt) * time.Millisecond
	if age <= 0 {
		return 1
	}
	v := math.Pow(0.5, float64(age)/float64(halfLife))
	if v < floor {
		return floor
	}
	if v > 1 {
		return 1
	}
	return v
}

// accessBoost 是 access_boost 分项：召回越多越重要，但对数增长（不该被热点吞没）。
func accessBoost(recallCount int, k float64) float64 {
	if recallCount <= 0 || k <= 0 {
		return 1
	}
	return 1 + k*math.Log(1+float64(recallCount))
}

// baseImportance 是 base 分项：写入时的静态重要性（提取管线的 importance）。
func baseImportance(m store.Memory) float64 {
	if m.Importance > 0 {
		return m.Importance
	}
	return 0.5
}

// stateSnapshot 是审计里的 before/after 形态（JSON 可读、字段稳定）。
type stateSnapshot struct {
	Status        MemoryStatus `json:"status"`
	Base          float64      `json:"importance_base"`
	Recency       float64      `json:"importance_recency"`
	AccessBoost   float64      `json:"importance_access_boost"`
	Grade         float64      `json:"importance_grade"`
	Importance    float64      `json:"importance"`
	StatusChanged int64        `json:"status_changed_at"`
	UpdatedAt     int64        `json:"updated_at"`
}

func snapshotOf(s State, ok bool) string {
	if !ok {
		return ""
	}
	b, err := json.Marshal(stateSnapshot{
		Status: s.Status, Base: s.Base, Recency: s.Recency, AccessBoost: s.AccessBoost,
		Grade: s.Grade, Importance: s.Importance, StatusChanged: s.StatusChangedAt, UpdatedAt: s.UpdatedAt,
	})
	if err != nil {
		return ""
	}
	return string(b)
}

// stateManager 把"读当前状态 → 算新状态 → 落库"收在一处。
//
// 任何 op 都**不得**自己拼 Store.PutState：分项公式与迁移校验只能有一份实现，
// 否则各 op 的 importance 会互相覆盖成不同口径。
type stateManager struct{ txc TxContext }

// load 读状态；不存在时按记忆本身推导一条**默认状态**（不落库，调用方决定是否写）。
//
// 默认状态是 **Candidate**——严格按 TD §7 的状态机：写入产生 Candidate，
// 质量门（长度/临时信号/密钥）判 Rejected，校验通过才 Active。
// 推进由 Consolidate 统一完成，因此"谁把它变成 Active 的"永远能在审计里查到。
//
// 为什么不对既有数据直接猜 Active：那会让 P2 遗留数据跳过一次质量门，
// 且"状态从何而来"变得不可解释。Candidate 是更诚实的起点，代价只是需要跑一次 Consolidate
// （而 Consolidate 本来就由 threshold/idle 触发，不是额外负担）。
func (sm stateManager) load(m store.Memory) (State, bool) {
	if st, ok, err := sm.txc.Backend().GetState(sm.txc.Context(), m.ID); err == nil && ok {
		return st, true
	}
	cfg := sm.txc.Cfg()
	now := sm.txc.Now().UnixMilli()
	status := StatusCandidate
	base := baseImportance(m)
	rec := recencyDecay(now, m.TValid*1000, cfg.DecayHalfLife, cfg.DecayFloor)
	acc := accessBoost(m.RecallCount, cfg.AccessBoostK)
	grade := 1.0
	hasConcept := m.ConceptID != ""
	if hasConcept {
		c, err := sm.txc.Store().GetConcept(sm.txc.Context(), m.ConceptID)
		if err != nil {
			hasConcept = false
		} else {
			grade = gradeOf(cfg, c.Status, true)
		}
	}
	return State{
		MemoryID: m.ID, Scope: m.Scope, Status: status,
		Base: base, Recency: rec, AccessBoost: acc, Grade: grade,
		Importance:      composeImportance(base, rec, acc, grade),
		StatusChangedAt: m.TCreated * 1000,
		LastRecalledAt:  m.TValid * 1000,
		CreatedAt:       m.TCreated * 1000,
		UpdatedAt:       now,
	}, false
}

// recalc 按分项重算 importance（不改状态）。用于 Decay op。
func (sm stateManager) recalc(m store.Memory, st State) State {
	cfg := sm.txc.Cfg()
	now := sm.txc.Now().UnixMilli()
	next := st
	next.MemoryID = m.ID
	next.Scope = m.Scope
	next.Base = baseImportance(m)
	lastAt := st.LastRecalledAt
	if lastAt <= 0 {
		lastAt = m.TValid * 1000
	}
	next.Recency = recencyDecay(now, lastAt, cfg.DecayHalfLife, cfg.DecayFloor)
	next.AccessBoost = accessBoost(m.RecallCount, cfg.AccessBoostK)
	grade := 1.0
	if m.ConceptID != "" {
		if c, err := sm.txc.Store().GetConcept(sm.txc.Context(), m.ConceptID); err == nil {
			grade = gradeOf(cfg, c.Status, true)
		}
	}
	next.Grade = grade
	next.Importance = composeImportance(next.Base, next.Recency, next.AccessBoost, next.Grade)
	next.UpdatedAt = now
	if next.CreatedAt == 0 {
		next.CreatedAt = m.TCreated * 1000
	}
	if next.StatusChangedAt == 0 {
		next.StatusChangedAt = next.CreatedAt
	}
	return next
}

// move 迁移状态并落库，返回可审计的 Diff。
//
// changed=false 时不写库：**幂等**在这里落地（重跑不会把 updated_at 一直推高，
// 否则"没有语义变化"的运行会伪造出大量 diff）。
func (sm stateManager) move(m store.Memory, to MemoryStatus, note string) (Diff, error) {
	cur, existed := sm.load(m)
	before := snapshotOf(cur, existed)
	if cur.Status == to {
		return Diff{Changed: false, Before: before, After: before, Summary: note + "（已是目标状态）",
			Subjects: []string{m.ID}}, nil
	}
	// 允许沿状态机的合法路径**推进到**目标状态，而不是只接受一步跳。
	//
	// 为什么需要它：写入产生 Candidate（TD §7 的起点），而 Invalidate 的目标是
	// Invalidated（Active 的后继）。若不自动经过 Active，一次 supersede 就会因为
	// "Candidate → Invalidated 非法"而失败——那是把状态机的严格性变成了功能缺失。
	// 中间的 Active 是真实语义（事实先成立，后失效），审计里也会留下它的重要性分项。
	if err := ValidateTransition(cur.Status, to); err != nil {
		via := intermediateFor(cur.Status, to)
		if via == "" {
			return Diff{}, err
		}
		// F-10：Candidate **不得**绕过质量门变成 Invalidated。
		//
		// 旧实现无条件补一跳 Active，于是一条内容过短、本该 Rejected 的记忆，
		// 只要被 supersede 一次就会留下“曾经 Active 过”的记录——把状态机的
		// 非法迁移改写成“两跳合法迁移”，等于放宽了状态机而不是报错。
		// 现在先过质量门：过不了就显式拒绝，让调用方先跑 Consolidate。
		if cur.Status == StatusCandidate {
			if n := len([]rune(m.Content)); n < sm.txc.Cfg().MinContentLength {
				return Diff{}, fmt.Errorf(
					"ops: 拒绝 %s: %s → %s：Candidate 未通过质量门（内容 %d < %d），须先 Consolidate（TD §7）",
					m.ID, cur.Status, to, n, sm.txc.Cfg().MinContentLength)
			}
		}
		mid, mErr := sm.move(m, via, note+"（先进入 "+string(via)+"）")
		if mErr != nil {
			return Diff{}, mErr
		}
		if !mid.Changed && via != to {
			return Diff{}, fmt.Errorf("ops: 无法从 %s 推进到 %s", cur.Status, to)
		}
		// F-10a：中间迁移是**真实落库**（PutState），必须交出可审计的 diff。
		// 否则 Candidate → Active → Invalidated 两次写入只留 1 行审计，
		// before_json 里看不到曾经有过 Active 这一步，因果链断环。
		trail := mid.Trail
		if mid.Changed {
			trail = append(trail, Diff{
				Changed: true, Before: mid.Before, After: mid.After,
				Summary: mid.Summary, Subjects: mid.Subjects,
			})
		}
		cur, _ = sm.load(m)
		before = snapshotOf(cur, true)
		if cur.Status == to {
			mid.Trail = trail
			return mid, nil
		}
		if err := ValidateTransition(cur.Status, to); err != nil {
			return Diff{}, err
		}
		res, err := sm.applyTransition(m, cur, to)
		if err != nil {
			return Diff{}, err
		}
		res.Trail = append(trail, res.Trail...)
		return res, nil
	}
	return sm.applyTransition(m, cur, to)
}

// applyTransition 写一步状态变更（不含中间跳逻辑）。
func (sm stateManager) applyTransition(m store.Memory, cur State, to MemoryStatus) (Diff, error) {
	before := snapshotOf(cur, true)
	next := sm.recalc(m, cur)
	next.Status = to
	next.StatusChangedAt = sm.txc.Now().UnixMilli()
	if err := sm.txc.Backend().PutState(sm.txc.Context(), next); err != nil {
		return Diff{}, err
	}
	return Diff{
		Changed: true, Before: before, After: snapshotOf(next, true),
		Summary:  fmt.Sprintf("%s: %s → %s", m.ID, cur.Status, to),
		Subjects: []string{m.ID},
	}, nil
}

// intermediateFor 返回把 from 推进到 to 时必须先经过的中间状态（"" = 不需要）。
//
// 只覆盖状态机里真实存在的一步跳；更远的组合一律由多次调用完成，
// 而不是在这里编一条"捷径边"（那会让 TD §7 的状态机失去意义）。
func intermediateFor(from, to MemoryStatus) MemoryStatus {
	if from == StatusCandidate {
		switch to {
		case StatusInvalidated, StatusStale:
			// Candidate → Active → Invalidated/Stale
			return StatusActive
		case StatusArchived:
			// Candidate → Active → （Consolidate 归档），这里不提供捷径。
			return ""
		}
	}
	if from == StatusRejected && to == StatusArchived {
		// Rejected 是终态：不归档、不删除（它本身就是一条"没记住"的审计事实）。
		return ""
	}
	return ""
}

// ensure 落一条默认状态（不存在时），用于新写入的记忆。
func (sm stateManager) ensure(m store.Memory) error {
	cur, existed := sm.load(m)
	if existed {
		return nil
	}
	// F-11：落库前必须走 recalc（与 move/Decay 同一口径）。
	//
	// 旧实现直接持久化 load() 的返回值，而 load() 的 recency 曾把秒当毫秒（已修），
	// 结果是每条 Extract 写入都往 memory_state 落一行 importance≈0.025 的错误状态，
	// 而 Consolidate 的 nextStatus 读的正是 st.Importance——刚写入的记忆会被判成 Stale。
	// recalc 重算四个分项，使“首次落库”与“后续重算”不可能出现两个口径。
	return sm.txc.Backend().EnsureState(sm.txc.Context(), sm.recalc(m, cur))
}

// importanceOf 返回当前 importance（不存在时按默认推导）。
func (sm stateManager) importanceOf(m store.Memory) float64 {
	st, _ := sm.load(m)
	return st.Importance
}
