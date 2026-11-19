// Package demo 写入一组**可信的演示数据**，让空库也能立刻体验 Graph View。
//
// # 为什么需要它
//
// 空库打开界面只能看到降级 banner 与空图。演示数据是"能不能用起来"的门槛。
//
// # 一条不可退让的边界：数据必须是真的
//
// 本包**不直接写 SQL、不伪造 API 响应**。每条数据都走真实写入路径：
//
//	memories  → recall.Writer（主表 + FTS 同事务；绕过它就是 P0 修过的"FTS 静默漏索引"）
//	concepts  → store.Store.PutConcept
//	nodes     → store.GraphStore.PutNode
//	edges     → store.GraphStore.PutEdge
//	episodes  → store.Store.PutEpisode
//	状态      → ops.Backend.EnsureState（侧栏的状态来源）
//
// 因此 `/api/graph` 与 `/api/entity` 返回的是**从真实表里聚合出来的**东西，
// 而不是"看起来像图"的假数据。
//
// # 幂等与"非空库拒绝"
//
//   - 默认**拒绝对非空库写入**：演示数据混进真实记忆会让召回结果不可解释。
//   - 所有 ID 都是确定性的（由名称/内容派生），因此重复写入天然幂等；
//     多版本那条记忆额外用版本链探测跳过（否则重写会把 v1 内容插成 v3）。
//   - --force 允许对非空库写，但**仍然不重复写入**：已存在的对象一律跳过并汇报。
package demo

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tim5wang/gmem/ops"
	"github.com/tim5wang/gmem/recall"
	"github.com/tim5wang/gmem/store"
)

// ErrNotEmpty 表示库非空而调用方没有显式 --force。
var ErrNotEmpty = errors.New("demo: 目标库已有数据，拒绝写入演示数据（如确认要写入请加 --force）")

// Probe 报告库里各表的行数（用于"非空库拒绝"判定）。
//
// 用可选接口而不是往 store.Store 里加方法：Store 是 P0 已发布的公开契约。
// 满足它的实现是 sqlite.DB.DataCounts（只读诊断，不改变被体检对象）。
type Probe interface {
	DataCounts(ctx context.Context) (map[string]int64, error)
}

// HistoryReader 是版本链端口（探测多版本记忆是否已存在，保证 --force 幂等）。
type HistoryReader interface {
	MemoryHistory(ctx context.Context, id string) ([]store.Memory, error)
}

// Options 是 Run 的参数。
type Options struct {
	Ports  store.Ports
	Probe  Probe
	States ops.Backend
	// History 可选：缺省时多版本记忆按"已存在则整体跳过"的保守判定处理。
	History HistoryReader
	// Scope 是演示数据落在哪个边界（默认 user）。
	Scope store.Scope
	// Force 允许对非空库写入（仍然幂等，不重复写入）。
	Force bool
	// Now 可注入时钟（测试用）。
	Now func() time.Time
}

// Report 是一次写入的结果（全部数字都必须可见，便于"重复跑幂等"核对）。
type Report struct {
	Scope    string `json:"scope"`
	Forced   bool   `json:"forced"`
	Concepts int    `json:"concepts"`
	Nodes    int    `json:"nodes"`
	Edges    int    `json:"edges"`
	// Episodes / Memories 是本次**新写入**的条数（--force 重跑时应为 0）。
	Episodes int `json:"episodes"`
	Memories int `json:"memories"`
	States   int `json:"states"`
	Aliases  int `json:"aliases"`
	// Skipped 是"已存在所以没重复写入"的条数（幂等的证据）。
	Skipped    int      `json:"skipped"`
	Warnings   []string `json:"warnings,omitempty"`
	ElapsedMs  int64    `json:"elapsed_ms"`
	TotalAfter int64    `json:"total_after"`
}

// memSpec 是演示记忆的声明（包级而不是 Run 内的局部类型：
// stateFor 需要按 kind/invalid 推导生命周期状态）。
type memSpec struct {
	ID      string
	Kind    store.MemoryKind
	Content string
	Episode string
	TValid  int64
	Invalid bool
	// History 非空表示这条记忆写入两版（before/after 版本链）。
	History []string
}

// Run 写入演示数据。
func Run(ctx context.Context, opts Options) (Report, error) {
	if opts.Ports.Store == nil {
		return Report{}, errors.New("demo: nil store port")
	}
	if opts.Ports.Graph == nil {
		return Report{}, errors.New("demo: nil graph port（演示数据的核心就是图）")
	}
	if opts.Scope == "" {
		opts.Scope = store.ScopeUser
	}
	if !opts.Scope.Valid() {
		return Report{}, fmt.Errorf("demo: scope 非法 %q", opts.Scope)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	start := opts.Now()
	rep := Report{Scope: string(opts.Scope), Forced: opts.Force}

	// ---- 1) 非空库闸门 ----
	if opts.Probe != nil {
		counts, err := opts.Probe.DataCounts(ctx)
		if err != nil {
			return Report{}, fmt.Errorf("demo: 读取库状态: %w", err)
		}
		var total int64
		for _, n := range counts {
			total += n
		}
		rep.TotalAfter = total
		if total > 0 && !opts.Force {
			return rep, fmt.Errorf("%w（当前库含 %d 行数据；演示数据只应写进空库）", ErrNotEmpty, total)
		}
	}

	w := recall.NewWriter(opts.Ports)
	now := opts.Now().Unix()
	// tBase 是演示数据的"世界里发生时间"锚点。
	//
	// # 为什么必须能"回读上一次的锚点"（幂等性的关键）
	//
	// 时间戳参与写入等价比较（memories/edges 的 edgeSame / memorySame 都比
	// TCreated / TValid）。若每次运行都用 now 现算锚点，同一个 --force 重跑就会
	// 得到不同的时间戳，于是同一条边被当成"内容变了"而插入新版本——行数一直涨。
	// （实测：61 行 → 84 行，多出的 23 行正好是 23 条边的新版本。）
	//
	// 因此首次写入用 now-1h，后续运行从已有的 demo episode 回读同一个锚点：
	// 同一份演示数据永远算出同一批时间戳，重跑就是真正的幂等空操作。
	tBase := now - 3600
	if ep, err := opts.Ports.Store.GetEpisode(ctx, "ep-demo-1"); err == nil && ep.At != 0 {
		tBase = ep.At
	}
	// tCreated 是系统时间（写入时刻）。同样取**稳定值**而不是 now：
	// 它参与 memorySame，用 now 会让每次重跑都产生新版本。
	// 放在 tBase+2h 是为了不小于任何一条演示数据的 TValid（避免"创建早于成立"）。
	tCreated := tBase + 7200

	// ---- 2) 概念（含 1 个 draft + implicit） ----
	// draft/implicit 是刻意留的：D15 的"本体从使用中生长"与 Graph View 的斜纹
	// 视觉编码都需要一个真实存在的草稿概念才能看出来。
	concepts := []store.Concept{
		{
			ID: "c-person", Name: "人物", Status: store.ConceptCertified,
			Definition: "组织里的自然人成员。", Scope: opts.Scope, CreatedAt: tBase * 1000,
		},
		{
			ID: "c-org", Name: "组织", Status: store.ConceptCertified,
			Definition: "公司、团队等有边界的组织实体。", Scope: opts.Scope, CreatedAt: tBase * 1000,
		},
		{
			// 唯一一个 draft：origin=implicit 表示它是写入时被隐式落成的草稿。
			ID: "c-tool", Name: "工具", Status: store.ConceptDraft,
			Origin:     store.ConceptOriginImplicit,
			Definition: "团队日常使用的基础设施/组件（尚未认证）。",
			Scope:      opts.Scope, CreatedAt: tBase * 1000,
		},
	}
	for _, c := range concepts {
		if err := opts.Ports.Store.PutConcept(ctx, c); err != nil {
			return rep, fmt.Errorf("demo: 写入概念 %q: %w", c.ID, err)
		}
		rep.Concepts++
	}

	// ---- 3) episodes（不可变事实来源：节点的 source_episode 指向它们） ----
	type episodeSpec struct {
		ID      string
		Role    string
		Content string
	}
	episodes := []episodeSpec{
		{
			ID: "ep-demo-1", Role: "user",
			Content: "Alice 是 Acme 的工程师，Bob 也在 Acme。Carol 在 Beta Labs 做架构。" +
				"团队用 Postgres 和 Go 搭 Atlas，用 SQLite 和 Redis 搭 Borealis。",
		},
		{
			ID: "ep-demo-2", Role: "assistant",
			Content: "决策：Atlas 的存储继续用 Postgres，不切到 SQLite；Borealis 依赖 Go 运行时。" +
				"Q3 路线图把两个项目都列进去了。",
		},
	}
	for i, e := range episodes {
		if err := opts.Ports.Store.PutEpisode(ctx, store.Episode{
			ID: e.ID, Scope: opts.Scope, Namespace: "demo", Role: e.Role,
			Content: e.Content, ContentHash: contentHash(e.Content),
			At: tBase + int64(i)*60,
		}); err != nil {
			return rep, fmt.Errorf("demo: 写入 episode %q: %w", e.ID, err)
		}
		rep.Episodes++
	}

	// ---- 4) 节点 ----
	type nodeSpec struct {
		ID        string
		Name      string
		ConceptID string
		Episode   string
	}
	nodes := []nodeSpec{
		// 人物
		{ID: "n-demo-alice", Name: "Alice", ConceptID: "c-person", Episode: "ep-demo-1"},
		{ID: "n-demo-bob", Name: "Bob", ConceptID: "c-person", Episode: "ep-demo-1"},
		{ID: "n-demo-carol", Name: "Carol", ConceptID: "c-person", Episode: "ep-demo-1"},
		{ID: "n-demo-dave", Name: "Dave", ConceptID: "c-person", Episode: "ep-demo-2"},
		// 类型层（subtype_of 传递链的三环：Engineer → Employee → Person）
		{ID: "n-demo-person", Name: "Person", ConceptID: "c-person"},
		{ID: "n-demo-employee", Name: "Employee", ConceptID: "c-person"},
		{ID: "n-demo-engineer", Name: "Engineer", ConceptID: "c-person"},
		{ID: "n-demo-manager", Name: "Manager", ConceptID: "c-person"},
		// 组织
		{ID: "n-demo-acme", Name: "Acme Corp", ConceptID: "c-org"},
		{ID: "n-demo-beta", Name: "Beta Labs", ConceptID: "c-org"},
		// 工具（draft 概念下的节点：Graph View 会画斜纹）
		{ID: "n-demo-postgres", Name: "Postgres", ConceptID: "c-tool", Episode: "ep-demo-1"},
		{ID: "n-demo-sqlite", Name: "SQLite", ConceptID: "c-tool", Episode: "ep-demo-1"},
		{ID: "n-demo-go", Name: "Go", ConceptID: "c-tool", Episode: "ep-demo-1"},
		{ID: "n-demo-redis", Name: "Redis", ConceptID: "c-tool", Episode: "ep-demo-1"},
		// 项目
		{ID: "n-demo-atlas", Name: "Atlas", ConceptID: "c-org", Episode: "ep-demo-1"},
		{ID: "n-demo-borealis", Name: "Borealis", ConceptID: "c-org", Episode: "ep-demo-1"},
		{ID: "n-demo-roadmap", Name: "Q3 Roadmap", ConceptID: "c-org", Episode: "ep-demo-2"},
		{ID: "n-demo-designdoc", Name: "Atlas Design Doc", ConceptID: "c-org", Episode: "ep-demo-2"},
		// 与 Alice 疑似同一实体（待审池的那一对的另一半）
		{ID: "n-demo-alice-w", Name: "Alice W.", ConceptID: "c-person", Episode: "ep-demo-2"},
	}
	for _, n := range nodes {
		if err := opts.Ports.Graph.PutNode(ctx, store.Node{
			ID: n.ID, Scope: opts.Scope, ConceptID: n.ConceptID,
			CanonicalName: n.Name, SourceEpisode: n.Episode, PropsJSON: "{}",
		}); err != nil {
			return rep, fmt.Errorf("demo: 写入节点 %q: %w", n.ID, err)
		}
		rep.Nodes++
	}

	// ---- 5) 边 ----
	// 必须覆盖四类：
	//   subtype_of 传递链（Engineer→Employee→Person）、works_at 非传递、
	//   一条 possiblySame 待审边、一条 t_invalid 非零的已失效边。
	type edgeSpec struct {
		ID       string
		From     string
		To       string
		Relation store.Relation
		Weight   float64
		TValid   int64
		TInvalid int64
	}
	edges := []edgeSpec{
		// subtype_of 传递链（3 跳内可传：Engineer → Employee → Person；Manager → Employee）
		{ID: "e-demo-sub-1", From: "n-demo-engineer", To: "n-demo-employee", Relation: store.RelationSubtypeOf, Weight: 1},
		{ID: "e-demo-sub-2", From: "n-demo-employee", To: "n-demo-person", Relation: store.RelationSubtypeOf, Weight: 1},
		{ID: "e-demo-sub-3", From: "n-demo-manager", To: "n-demo-employee", Relation: store.RelationSubtypeOf, Weight: 1},

		// works_at：非传递（2 跳就会走到"别人公司"，规则表 MaxHops=1）
		{ID: "e-demo-work-1", From: "n-demo-alice", To: "n-demo-acme", Relation: store.RelationWorksAt, Weight: 1},
		{ID: "e-demo-work-2", From: "n-demo-bob", To: "n-demo-acme", Relation: store.RelationWorksAt, Weight: 1},
		{ID: "e-demo-work-3", From: "n-demo-carol", To: "n-demo-beta", Relation: store.RelationWorksAt, Weight: 1},
		{ID: "e-demo-work-4", From: "n-demo-dave", To: "n-demo-beta", Relation: store.RelationWorksAt, Weight: 1},
		// 已失效的 works_at：Bob 曾在 Beta Labs（t_invalid 非零 → 图上画虚线淡边）
		{
			ID: "e-demo-work-5", From: "n-demo-bob", To: "n-demo-beta",
			Relation: store.RelationWorksAt, Weight: 1,
			TValid: tBase - 86400, TInvalid: tBase - 3600,
		},

		// uses：关系型问句的主通道
		{ID: "e-demo-use-1", From: "n-demo-atlas", To: "n-demo-postgres", Relation: store.RelationUses, Weight: 1},
		{ID: "e-demo-use-2", From: "n-demo-atlas", To: "n-demo-go", Relation: store.RelationUses, Weight: 1},
		{ID: "e-demo-use-3", From: "n-demo-borealis", To: "n-demo-sqlite", Relation: store.RelationUses, Weight: 1},
		{ID: "e-demo-use-4", From: "n-demo-borealis", To: "n-demo-redis", Relation: store.RelationUses, Weight: 1},
		{ID: "e-demo-use-5", From: "n-demo-atlas", To: "n-demo-redis", Relation: store.RelationUses, Weight: 1},
		{ID: "e-demo-use-6", From: "n-demo-borealis", To: "n-demo-go", Relation: store.RelationUses, Weight: 1},

		// part_of / depends_on / owns
		{ID: "e-demo-part-1", From: "n-demo-atlas", To: "n-demo-roadmap", Relation: store.RelationPartOf, Weight: 1},
		{ID: "e-demo-part-2", From: "n-demo-borealis", To: "n-demo-roadmap", Relation: store.RelationPartOf, Weight: 1},
		{ID: "e-demo-part-3", From: "n-demo-designdoc", To: "n-demo-atlas", Relation: store.RelationPartOf, Weight: 1},
		{ID: "e-demo-dep-1", From: "n-demo-atlas", To: "n-demo-borealis", Relation: store.RelationDependsOn, Weight: 1},
		{ID: "e-demo-dep-2", From: "n-demo-borealis", To: "n-demo-go", Relation: store.RelationDependsOn, Weight: 1},
		{ID: "e-demo-own-1", From: "n-demo-alice", To: "n-demo-atlas", Relation: store.RelationOwns, Weight: 1},
		{ID: "e-demo-own-2", From: "n-demo-carol", To: "n-demo-borealis", Relation: store.RelationOwns, Weight: 1},

		// 待审的一对（possiblySame）：**绝不自动合并**，进待审池等裁决。
		{ID: "e-demo-same-1", From: "n-demo-alice", To: "n-demo-alice-w", Relation: relationPossiblySame, Weight: 0.72},

		// caused_by（传递但仅 2 跳）
		{ID: "e-demo-cause-1", From: "n-demo-designdoc", To: "n-demo-atlas", Relation: store.RelationCausedBy, Weight: 1},
	}
	for _, e := range edges {
		tv := e.TValid
		if tv == 0 {
			tv = tBase
		}
		if err := opts.Ports.Graph.PutEdge(ctx, store.Edge{
			ID: e.ID, FromID: e.From, ToID: e.To, Relation: e.Relation,
			Weight: e.Weight, TValid: tv, TInvalid: e.TInvalid, TCreated: tBase,
		}); err != nil {
			return rep, fmt.Errorf("demo: 写入边 %q: %w", e.ID, err)
		}
		rep.Edges++
	}

	// ---- 6) 别名（L0 词典规则的最高置信来源；也解释了同名不同写法） ----
	aliases := []store.Alias{
		{Surface: "Alice", NodeID: "n-demo-alice", Source: "demo", Confidence: 0.95},
		{Surface: "Alice W.", NodeID: "n-demo-alice-w", Source: "demo", Confidence: 0.5},
		{Surface: "Acme", NodeID: "n-demo-acme", Source: "demo", Confidence: 0.9},
		{Surface: "Beta", NodeID: "n-demo-beta", Source: "demo", Confidence: 0.9},
		{Surface: "PostgreSQL", NodeID: "n-demo-postgres", Source: "demo", Confidence: 0.9},
		{Surface: "Golang", NodeID: "n-demo-go", Source: "demo", Confidence: 0.9},
	}
	for _, a := range aliases {
		if err := opts.Ports.Store.PutAlias(ctx, a); err != nil {
			return rep, fmt.Errorf("demo: 写入别名 %q: %w", a.Surface, err)
		}
		rep.Aliases++
	}

	// ---- 7) 记忆（含不同 kind / 不同状态 / 一条多版本 + 一条已失效） ----
	mems := []memSpec{
		{
			ID: "m-demo-episodic-1", Kind: store.KindEpisodic, Episode: "ep-demo-1", TValid: tBase,
			Content: episodes[0].Content,
		},
		{
			ID: "m-demo-episodic-2", Kind: store.KindEpisodic, Episode: "ep-demo-2", TValid: tBase + 60,
			Content: episodes[1].Content,
		},
		{
			ID: "m-demo-semantic-alice", Kind: store.KindSemantic, Episode: "ep-demo-1", TValid: tBase,
			Content: "Alice 是 Acme 的工程师，负责 Atlas 项目。",
		},
		{
			ID: "m-demo-semantic-carol", Kind: store.KindSemantic, Episode: "ep-demo-1", TValid: tBase,
			Content: "Carol 在 Beta Labs 做架构，负责 Borealis。",
		},
		{
			ID: "m-demo-procedural", Kind: store.KindProcedural, Episode: "ep-demo-2", TValid: tBase + 60,
			Content: "本地起服务：gmem serve --db ./test_bd.db，然后打开 http://127.0.0.1:7700/ 。",
		},
		{
			ID: "m-demo-decision", Kind: store.KindDecision, Episode: "ep-demo-2", TValid: tBase + 60,
			Content: "决策：Atlas 的存储继续用 Postgres，不切到 SQLite。",
			// 两版：初版说"考虑切 SQLite"，后来改主意 —— 这是 history 动词的真实素材。
			History: []string{
				"决策：Atlas 的存储考虑从 Postgres 切到 SQLite（待评估）。",
				"决策：Atlas 的存储继续用 Postgres，不切到 SQLite。",
			},
		},
		{
			ID: "m-demo-invalidated", Kind: store.KindSemantic, Episode: "ep-demo-1", TValid: tBase - 86400,
			Content: "Bob 在 Beta Labs 工作。", Invalid: true,
		},
	}

	for _, m := range mems {
		contents := []string{m.Content}
		if len(m.History) > 0 {
			contents = m.History
		}
		// 幂等：已有版本链的记忆整体跳过（否则 --force 会把初版内容插成新版本）。
		if opts.History != nil {
			if vs, err := opts.History.MemoryHistory(ctx, m.ID); err == nil && len(vs) > 0 {
				rep.Skipped++
				continue
			}
		}
		var last recall.PutResult
		for i, content := range contents {
			res, err := w.PutMemory(ctx, store.Memory{
				ID: m.ID, Scope: opts.Scope, Kind: m.Kind, Content: content,
				SourceEpisode: m.Episode, TValid: m.TValid,
				// tCreated 是稳定值（见上）：它参与 memorySame，用 now 会破幂等。
				// 逐版 +1 只为让版本链的系统时间严格递增（同一刻会让过期边界含糊）。
				TCreated: tCreated + int64(i), Importance: 0.6,
			})
			if err != nil {
				return rep, fmt.Errorf("demo: 写入记忆 %q: %w", m.ID, err)
			}
			last = res
		}
		rep.Memories++
		if m.Invalid {
			if err := opts.Ports.Store.InvalidateMemory(ctx, m.ID, tBase-1800); err != nil {
				return rep, fmt.Errorf("demo: 失效记忆 %q（as_of 演示需要它）: %w", m.ID, err)
			}
		}
		// 状态（侧栏与生命周期演示）：EnsureState 是 INSERT OR IGNORE，天然幂等。
		if opts.States != nil {
			st := ops.State{
				MemoryID: last.ID, Scope: opts.Scope, Status: stateFor(m),
				Base: 0.6, Recency: 1, AccessBoost: 1, Grade: 1, Importance: 0.6,
				StatusChangedAt: tCreated * 1000, CreatedAt: tCreated * 1000, UpdatedAt: tCreated * 1000,
			}
			if err := opts.States.EnsureState(ctx, st); err != nil {
				return rep, fmt.Errorf("demo: 写入状态 %q: %w", last.ID, err)
			}
			rep.States++
		}
	}

	rep.TotalAfter = 0
	if opts.Probe != nil {
		if counts, err := opts.Probe.DataCounts(ctx); err == nil {
			for _, n := range counts {
				rep.TotalAfter += n
			}
		}
	}
	rep.ElapsedMs = opts.Now().Sub(start).Milliseconds()
	sort.Strings(rep.Warnings)
	return rep, nil
}

// stateFor 给每种记忆一个不同的生命周期状态（驱动侧栏/生命周期视图）。
func stateFor(m memSpec) ops.MemoryStatus {
	if m.Invalid {
		return ops.StatusInvalidated
	}
	switch m.Kind {
	case store.KindDecision:
		return ops.StatusActive
	case store.KindProcedural:
		return ops.StatusActive
	case store.KindEpisodic:
		// episode 是事实来源，长期保留；给其中一个 stale 以展示非活跃态。
		if strings.HasSuffix(m.ID, "-1") {
			return ops.StatusStale
		}
		return ops.StatusActive
	default:
		return ops.StatusActive
	}
}

// relationPossiblySame 与 extract/ops 的取值一致（重复一份字符串是刻意的：
// 那两处是派发实现细节，这里只是演示数据要造一条待审边）。
const relationPossiblySame store.Relation = "possiblySame"

// contentHash 是稳定的内容摘要（FNV-1a 64 位，十六进制）。
func contentHash(s string) string {
	const prime = uint64(1099511628211)
	h := uint64(14695981039346656037)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime
	}
	const hex = "0123456789abcdef"
	buf := make([]byte, 16)
	for i := 0; i < 16; i++ {
		buf[15-i] = hex[h&0xf]
		h >>= 4
	}
	return string(buf)
}
