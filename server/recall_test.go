package server_test

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tim5wang/gmem/recall"
	"github.com/tim5wang/gmem/server"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// GET /api/recall（P6-1 Recall Inspector 的数据源）
//
// 本文件钉死的核心不变量：**端点只透传，不重算、不裁剪**。
// 页面上的"可解释"要能当证据用，前提就是页面里的数 = 召回算出来的数。
// 因此这里既有"逐字段相等"的断言（与直接调 Recaller 的结果对照），
// 也有乘性分解的数值一致性断言（score == fusion × boost × recency）。
// ===========================================================================

// recallFixture 建一份能同时产生 plan / 多 lane / 分数分解 / dropped 的数据。
//
// 关键点：
//   - 概念 + 别名 → align 能命中，于是 Plan.Matched 非空、ConceptBoost > 1；
//   - t_valid 固定在 30 天前且注入固定时钟 → RecencyFactor 恰为 0.5（可断言）；
//   - 三条记忆 + k=1 → 至少两条进 Dropped（"最终预算切断"），dropped 表非空。
func (h *harness) recallFixture() {
	h.t.Helper()
	h.concept("c-editor", "编辑器", store.ConceptCertified, store.ScopeUser, "")
	h.concept("c-lang", "语言", store.ConceptCertified, store.ScopeUser, "")
	h.node("n-alice", "Alice", store.ScopeUser, "")
	h.node("n-vim", "vim", store.ScopeUser, "c-editor")
	h.alias("Alice", "n-alice")
	h.edge("e-alice-vim", "n-alice", "n-vim", store.RelationUses, 1, 1000, 0)

	w := recall.NewWriter(h.ports)
	for _, m := range []store.Memory{
		{
			ID: "m-editor", Scope: store.ScopeUser, Kind: store.KindSemantic,
			ConceptID: "c-editor", Content: "Alice 的默认编辑器是 vim",
			TValid: 1000, TCreated: 1000,
		},
		{
			ID: "m-lang", Scope: store.ScopeUser, Kind: store.KindSemantic,
			ConceptID: "c-lang", Content: "Alice 常用编辑器脚本语言 vimscript",
			TValid: 1000, TCreated: 1000,
		},
		{
			ID: "m-note", Scope: store.ScopeUser, Kind: store.KindEpisodic,
			Content: "Alice 提过编辑器配色偏好", TValid: 1000, TCreated: 1000,
		},
	} {
		if _, err := w.PutMemory(context.Background(), m); err != nil {
			h.t.Fatalf("PutMemory(%s): %v", m.ID, err)
		}
	}
}

// recallFixtureConfig 固定时钟：t_valid=1000 与 now 相差 30 天 → 半衰期 30 天 → ×0.5。
//
// 固定时钟是必要的：不注入的话 RecencyFactor 随真实时间漂移，
// "score == fusion × boost × recency"这条断言会变成偶尔为真的噪声测试。
func recallFixtureConfig() recall.Config {
	now := time.Unix(1000, 0).Add(30 * 24 * time.Hour)
	return recall.Config{Now: func() time.Time { return now }}
}

// getRecall 请求 /api/recall 并解析。
func (h *harness) getRecall(params url.Values) (int, server.RecallResponse, []byte) {
	h.t.Helper()
	q := ""
	if len(params) > 0 {
		q = "?" + params.Encode()
	}
	status, body := h.get("/api/recall" + q)
	var resp server.RecallResponse
	if status == http.StatusOK {
		if err := json.Unmarshal(body, &resp); err != nil {
			h.t.Fatalf("unmarshal recall: %v (body=%s)", err, body)
		}
	}
	return status, resp, body
}

// recallQuery 拼一个查询参数集（用 url.Values：手写转义把中文转错过一次）。
func recallQuery(pairs ...string) url.Values {
	v := url.Values{}
	for i := 0; i+1 < len(pairs); i += 2 {
		v.Set(pairs[i], pairs[i+1])
	}
	return v
}

// ---------------------------------------------------------------------------
// 1. 正常路径：plan 六个字段 + unmatched + lane + steps + dropped 全在
// ---------------------------------------------------------------------------

// TestRecallEndpointReturnsFullPlan 打开 /api/recall?q=... 必须拿到完整 plan。
//
// 页面「plan 区块」要显示 shape / shape_reason / coverage / theta_coverage
// 四个具体值 + unmatched 列表；这四个字段任何一个缺了，页面就只能显示"-"。
func TestRecallEndpointReturnsFullPlan(t *testing.T) {
	h := newHarness(t, recallFixtureConfig())
	h.recallFixture()

	status, resp, body := h.getRecall(recallQuery("q", "Alice 编辑器", "k", "2"))
	if status != http.StatusOK {
		t.Fatalf("status=%d, want 200 (body=%s)", status, truncate(string(body), 300))
	}

	plan := resp.Plan
	if plan.Query == "" {
		t.Error("plan.query 为空：plan 必须回显查询串")
	}
	if plan.Shape == "" {
		t.Error("plan.shape 为空：页面要显示 shape 具体值")
	}
	if plan.ShapeReason == "" {
		t.Error("plan.shape_reason 为空：shape 必须带人话理由")
	}
	if plan.ThetaCoverage <= 0 {
		t.Errorf("plan.theta_coverage=%v，必须来自 recall 配置（R4 门槛）", plan.ThetaCoverage)
	}
	if plan.Coverage != resp.Coverage {
		t.Errorf("plan.coverage=%v 与顶层 coverage=%v 不一致（同一份数据不得两说）",
			plan.Coverage, resp.Coverage)
	}
	if resp.ThetaCoverage != plan.ThetaCoverage {
		t.Errorf("顶层 theta_coverage=%v 与 plan 的 %v 不一致", resp.ThetaCoverage, plan.ThetaCoverage)
	}
	// unmatched 是"我缺哪个维度"，页面要逐条列出；它必须与 plan 自身的切片等长
	// （JSON 往返后仍可对数：这里用 matched+unmatched 覆盖对齐结果）。
	if plan.Matched == nil || plan.Unmatched == nil {
		t.Error("plan.matched / plan.unmatched 不得为 null（空集合要序列化成 []）")
	}
	for _, m := range plan.Matched {
		if m.Method == "" {
			t.Errorf("plan.matched 条目缺 method：%+v", m)
		}
		if m.Text == "" {
			t.Errorf("plan.matched 条目缺 text：%+v", m)
		}
	}

	// 每条 lane 一行：lane/weight/budget/selected/executed/reason|skip_reason。
	if len(plan.Lanes) == 0 {
		t.Fatal("plan.lanes 为空：页面「lane 区块」需要逐行数据")
	}
	var sawExecuted bool
	for _, lp := range plan.Lanes {
		if lp.Lane == "" {
			t.Errorf("lane 缺名字：%+v", lp)
		}
		if lp.Weight <= 0 {
			t.Errorf("lane %s 的 weight=%v，必须透传 Planner 的起点权重", lp.Lane, lp.Weight)
		}
		if lp.Budget < 0 {
			t.Errorf("lane %s 的 budget=%d 非法", lp.Lane, lp.Budget)
		}
		if lp.Reason == "" && lp.SkipReason == "" {
			t.Errorf("lane %s 既无 reason 也无 skip_reason（页面无法解释它为何执行/被跳过）", lp.Lane)
		}
		if lp.Executed {
			sawExecuted = true
		}
	}
	if !sawExecuted {
		t.Error("没有任何 lane 的 executed=true：无 embedder 时 lexical 仍应执行")
	}

	// steps 是"可复现"的载体。
	if len(plan.Steps) == 0 {
		t.Fatal("plan.steps 为空：R5 要求 plan 可重放")
	}

	// 每条结果的 contributions / adjustments / graph_evidence 必须是数组而非 null。
	if len(resp.Items) == 0 {
		t.Fatalf("items 为空（fixture 里有可召回记忆）body=%s", truncate(string(body), 300))
	}
	for _, it := range resp.Items {
		if it.Contributions == nil {
			t.Errorf("item %s 的 contributions 是 null（页面要画分通道贡献）", it.ID)
		}
		if len(it.Contributions) == 0 {
			t.Errorf("item %s 没有任何 lane 贡献", it.ID)
		}
		if it.Adjustments == nil {
			t.Errorf("item %s 的 adjustments 是 null（必须至少含 concept_consistency/recency_decay）", it.ID)
		}
		if it.GraphEvidence == nil {
			t.Errorf("item %s 的 graph_evidence 是 null（空集合要序列化成 []）", it.ID)
		}
	}
}

// ---------------------------------------------------------------------------
// 2. 数值一致性（硬证据）：score == fusion_score × concept_boost × recency_factor
// ---------------------------------------------------------------------------

// TestRecallScoreEqualsProductOfFactors 页面显示的三因子相乘必须等于 score。
//
// 变异验证：把 server/recall.go 里的 `Score: it.Score` 改成
// `Score: it.FusionScore * it.ConceptBoost`（漏掉 recency，正是"前端自己算"的
// 典型错法），本用例立刻变红 —— 因为 fixture 的 recency 恰为 0.5。
func TestRecallScoreEqualsProductOfFactors(t *testing.T) {
	h := newHarness(t, recallFixtureConfig())
	h.recallFixture()

	status, resp, _ := h.getRecall(recallQuery("q", "Alice 编辑器", "k", "3"))
	if status != http.StatusOK {
		t.Fatalf("status=%d, want 200", status)
	}
	if len(resp.Items) == 0 {
		t.Fatal("items 为空，无法验证分数一致性")
	}
	var sawBoost, sawDecay bool
	for _, it := range resp.Items {
		product := it.FusionScore * it.ConceptBoost * it.RecencyFactor
		if math.Abs(product-it.Score) > 1e-3 {
			t.Errorf("item %s: fusion(%v) × boost(%v) × recency(%v) = %v，但 score=%v（误差 %v > 0.001）"+
				"—— 三因子必须能复算出 score，否则'可解释'是伪造的",
				it.ID, it.FusionScore, it.ConceptBoost, it.RecencyFactor, product, it.Score,
				math.Abs(product-it.Score))
		}
		if it.FusionScore <= 0 {
			t.Errorf("item %s 的 fusion_score=%v 非法（RRF 必为正）", it.ID, it.FusionScore)
		}
		if it.ConceptBoost < 1 {
			t.Errorf("item %s 的 concept_boost=%v < 1（概念一致性只加权不惩罚）", it.ID, it.ConceptBoost)
		}
		if it.RecencyFactor <= 0 || it.RecencyFactor > 1 {
			t.Errorf("item %s 的 recency_factor=%v 越界 (0,1]", it.ID, it.RecencyFactor)
		}
		if it.ConceptBoost > 1 {
			sawBoost = true
		}
		if it.RecencyFactor < 1 {
			sawDecay = true
		}
		// 调整明细必须与三因子对得上（页面要逐条显示 Kind/Factor/Reason）。
		var boostFactor, decayFactor float64
		for _, a := range it.Adjustments {
			switch a.Kind {
			case "concept_consistency":
				boostFactor = a.Factor
			case "recency_decay":
				decayFactor = a.Factor
			}
			if a.Reason == "" {
				t.Errorf("item %s 的 adjustment %s 缺 reason", it.ID, a.Kind)
			}
		}
		if boostFactor != it.ConceptBoost {
			t.Errorf("item %s: adjustments 里的 concept_consistency factor=%v 与 concept_boost=%v 不一致",
				it.ID, boostFactor, it.ConceptBoost)
		}
		if decayFactor != it.RecencyFactor {
			t.Errorf("item %s: adjustments 里的 recency_decay factor=%v 与 recency_factor=%v 不一致",
				it.ID, decayFactor, it.RecencyFactor)
		}
	}
	// 只有 Boost 真的 >1 且 decay 真的 <1，上面的乘法断言才覆盖了"三个都非平凡"的情况。
	if !sawBoost {
		t.Error("没有任何 item 的 concept_boost > 1（fixture 命中了概念，应当加权）")
	}
	if !sawDecay {
		t.Error("没有任何 item 的 recency_factor < 1（固定时钟下应为 0.5）")
	}
}

// ---------------------------------------------------------------------------
// 3. 逐字段透传：与直接调 Recaller 的结果完全一致（不重算、不裁剪）
// ---------------------------------------------------------------------------

// TestRecallEndpointPassesThroughWithoutRecalculation 端点结果必须与 Recaller 逐字段相同。
//
// 变异验证：把 buildRecall 里任何一个复制语句改成字面量（如 Score: 0），
// 或加一句"只保留前 k 条"，本用例都会红。这是"不得裁剪、不得重算"的机器证据。
func TestRecallEndpointPassesThroughWithoutRecalculation(t *testing.T) {
	cfg := recallFixtureConfig()
	h := newHarness(t, cfg)
	h.recallFixture()

	const query = "Alice 编辑器"
	const k = 3
	status, resp, _ := h.getRecall(recallQuery("q", query, "k", "3"))
	if status != http.StatusOK {
		t.Fatalf("status=%d, want 200", status)
	}

	direct, err := h.rec.Recall(context.Background(), recall.RecallRequest{
		Query: query, Scope: store.ScopeUser, Budget: recall.Budget{Final: k},
	})
	if err != nil {
		t.Fatalf("Recaller.Recall: %v", err)
	}

	if string(resp.Status) != string(direct.Status) || resp.Status != string(direct.Status) {
		t.Errorf("status=%q，直接调用得到 %q", resp.Status, direct.Status)
	}
	if resp.Coverage != direct.Coverage {
		t.Errorf("coverage=%v，直接调用得到 %v", resp.Coverage, direct.Coverage)
	}
	if len(resp.Items) != len(direct.Items) {
		t.Fatalf("items 条数=%d，直接调用得到 %d（端点不得裁剪）", len(resp.Items), len(direct.Items))
	}
	for i, want := range direct.Items {
		got := resp.Items[i]
		if got.ID != want.Memory.ID {
			t.Errorf("items[%d].id=%q，want %q（顺序也必须一致）", i, got.ID, want.Memory.ID)
			continue
		}
		if got.Score != want.Score || got.FusionScore != want.FusionScore ||
			got.ConceptBoost != want.ConceptBoost || got.RecencyFactor != want.RecencyFactor {
			t.Errorf("items[%d] 分数被改动：got {%v %v %v %v}，want {%v %v %v %v}",
				i, got.Score, got.FusionScore, got.ConceptBoost, got.RecencyFactor,
				want.Score, want.FusionScore, want.ConceptBoost, want.RecencyFactor)
		}
		if got.Content != want.Memory.Content {
			t.Errorf("items[%d].content=%q，want %q", i, got.Content, want.Memory.Content)
		}
		if got.Version != want.Memory.Version || got.Kind != string(want.Memory.Kind) {
			t.Errorf("items[%d] 记忆元信息不一致：%+v vs %+v", i, got, want.Memory)
		}
		if len(got.Contributions) != len(want.Contributions) {
			t.Errorf("items[%d] contributions 条数=%d，want %d",
				i, len(got.Contributions), len(want.Contributions))
		}
		if len(got.Adjustments) != len(want.Adjustments) {
			t.Errorf("items[%d] adjustments 条数=%d，want %d",
				i, len(got.Adjustments), len(want.Adjustments))
		}
		if len(got.GraphEvidence) != len(want.GraphEvidence) {
			t.Errorf("items[%d] graph_evidence 条数=%d，want %d",
				i, len(got.GraphEvidence), len(want.GraphEvidence))
		}
	}

	// plan 全字段对照
	if direct.Plan == nil {
		t.Fatal("直接调用返回的 plan 为 nil（R5 违约）")
	}
	if resp.Plan.Shape != string(direct.Plan.Shape) ||
		resp.Plan.ShapeReason != direct.Plan.ShapeReason ||
		resp.Plan.Coverage != direct.Plan.Coverage ||
		resp.Plan.ThetaCoverage != direct.Plan.ThetaCoverage ||
		resp.Plan.Hops != direct.Plan.Hops {
		t.Errorf("plan 标量字段被改动：got %+v，want shape=%s reason=%s coverage=%v theta=%v hops=%d",
			resp.Plan, direct.Plan.Shape, direct.Plan.ShapeReason,
			direct.Plan.Coverage, direct.Plan.ThetaCoverage, direct.Plan.Hops)
	}
	if len(resp.Plan.Matched) != len(direct.Plan.Matched) {
		t.Errorf("plan.matched 条数=%d，want %d", len(resp.Plan.Matched), len(direct.Plan.Matched))
	}
	if len(resp.Plan.Unmatched) != len(direct.Plan.Unmatched) {
		t.Errorf("plan.unmatched 条数=%d，want %d（页面要按数量对照）",
			len(resp.Plan.Unmatched), len(direct.Plan.Unmatched))
	}
	if len(resp.Plan.Lanes) != len(direct.Plan.Lanes) {
		t.Errorf("plan.lanes 条数=%d，want %d", len(resp.Plan.Lanes), len(direct.Plan.Lanes))
	}
	if len(resp.Plan.Relations) != len(direct.Plan.Relations) {
		t.Errorf("plan.relations 条数=%d，want %d", len(resp.Plan.Relations), len(direct.Plan.Relations))
	}
	if len(resp.Plan.Steps) != len(direct.Plan.Steps) {
		t.Errorf("plan.steps 条数=%d，want %d", len(resp.Plan.Steps), len(direct.Plan.Steps))
	}
	// 关系跳数表的 MaxHops / Origin 是"视觉与检索共用一张表"的证据。
	for i, rp := range direct.Plan.Relations {
		got := resp.Plan.Relations[i]
		if got.Relation != string(rp.Relation) || got.MaxHops != rp.MaxHops ||
			got.Transitive != rp.Transitive || got.Origin != rp.Origin {
			t.Errorf("plan.relations[%d]=%+v，want relation=%s max_hops=%d transitive=%v origin=%s",
				i, got, rp.Relation, rp.MaxHops, rp.Transitive, rp.Origin)
		}
		if got.Origin == "" {
			t.Errorf("plan.relations[%d] 缺 origin（query-cue / shape-default）", i)
		}
	}
}

// ---------------------------------------------------------------------------
// 4. dropped：条数与 plan.dropped 相同（页面按条数对照）
// ---------------------------------------------------------------------------

// TestRecallDroppedMatchesPlanDropped dropped 表条数必须等于 plan.dropped。
//
// fixture 里 3 条记忆 + k=1 → 至少 2 条被"最终预算切断"，因此这张表非空。
func TestRecallDroppedMatchesPlanDropped(t *testing.T) {
	h := newHarness(t, recallFixtureConfig())
	h.recallFixture()

	status, resp, body := h.getRecall(recallQuery("q", "Alice 编辑器", "k", "1"))
	if status != http.StatusOK {
		t.Fatalf("status=%d, want 200 (body=%s)", status, truncate(string(body), 300))
	}
	if len(resp.Items) > 1 {
		t.Fatalf("items=%d，k=1 时不得超发", len(resp.Items))
	}
	if len(resp.Plan.Dropped) == 0 {
		t.Fatalf("plan.dropped 为空：fixture 有 3 条候选而 k=1，必须有被拦截的")
	}
	if len(resp.Dropped) != len(resp.Plan.Dropped) {
		t.Fatalf("顶层 dropped=%d 条，plan.dropped=%d 条（必须同源等长）",
			len(resp.Dropped), len(resp.Plan.Dropped))
	}
	for i, d := range resp.Dropped {
		if d.ID == "" {
			t.Errorf("dropped[%d] 缺 id", i)
		}
		if d.Reason == "" {
			t.Errorf("dropped[%d] 缺 reason（页面要显示为什么被拦）", i)
		}
		if d != resp.Plan.Dropped[i] {
			t.Errorf("dropped[%d]=%+v 与 plan.dropped[%d]=%+v 不一致", i, d, i, resp.Plan.Dropped[i])
		}
	}
}

// ---------------------------------------------------------------------------
// 5. 降级可见：degraded 非空时必须带文本（不得静默）
// ---------------------------------------------------------------------------

// TestRecallDegradedIsVisible 无 embedder 时 vector lane 缺席必须在 degraded 里说清。
func TestRecallDegradedIsVisible(t *testing.T) {
	h := newHarness(t, recallFixtureConfig())
	h.recallFixture()

	status, resp, _ := h.getRecall(recallQuery("q", "Alice 编辑器"))
	if status != http.StatusOK {
		t.Fatalf("status=%d, want 200", status)
	}
	if len(resp.Degraded) == 0 {
		t.Fatal("degraded 为空：无 embedder 时 vector lane 缺席必须显式上报")
	}
	joined := strings.Join(resp.Degraded, "；")
	if !strings.Contains(joined, "vector") {
		t.Errorf("degraded 未提到 vector lane 缺席：%v", resp.Degraded)
	}
	for i, d := range resp.Degraded {
		if strings.TrimSpace(d) == "" {
			t.Errorf("degraded[%d] 是空字符串（不得用空串冒充'已降级'）", i)
		}
	}
	if resp.Degraded == nil {
		t.Error("degraded 不得为 null（空集合要序列化成 []）")
	}
}

// ---------------------------------------------------------------------------
// 6. 错误路径：非法参数显式 400 + 字段名（不得白屏、不得静默夹取）
// ---------------------------------------------------------------------------

// TestRecallEndpointRejectsBadParams 每个非法参数都必须 400 且指明字段。
//
// 前端的「URL 即视图 + 非法参数显式报错」完全依赖这条：服务端若静默兜底，
// 页面就会显示一份"看起来正常但和 URL 不符"的结果。
func TestRecallEndpointRejectsBadParams(t *testing.T) {
	h := newHarness(t, recallFixtureConfig())
	h.recallFixture()

	cases := []struct {
		name  string
		query string
		field string
	}{
		{"缺 q", "", "q"},
		{"q 只有空白", "?q=%20%20", "q"},
		{"as_of 非数字", "?q=acme&as_of=abc", "as_of"},
		{"k 非数字", "?q=acme&k=abc", "k"},
		{"k 为 0", "?q=acme&k=0", "k"},
		{"k 超上限", "?q=acme&k=51", "k"},
		{"scope 非法", "?q=acme&scope=bogus", "scope"},
		{"kinds 非法", "?q=acme&kinds=bogus", "kinds"},
		{"lanes 非法", "?q=acme&lanes=bogus", "lanes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := h.get("/api/recall" + tc.query)
			if status != http.StatusBadRequest {
				t.Fatalf("status=%d, want 400 (body=%s)", status, truncate(string(body), 300))
			}
			var e server.ErrorResponse
			if err := json.Unmarshal(body, &e); err != nil {
				t.Fatalf("错误体不是 JSON：%v (body=%s)", err, truncate(string(body), 300))
			}
			if e.Error != "bad_request" {
				t.Errorf("error=%q, want bad_request", e.Error)
			}
			if e.Field != tc.field {
				t.Errorf("field=%q, want %q（前端要能定位到出错参数）", e.Field, tc.field)
			}
			if strings.TrimSpace(e.Message) == "" {
				t.Error("message 不得为空（必须人可读）")
			}
		})
	}
}

// TestRecallEndpointRejectsNonGet 只读端点不接受 POST。
func TestRecallEndpointRejectsNonGet(t *testing.T) {
	h := newHarness(t, recallFixtureConfig())
	if status, _ := h.postJSON("/api/recall?q=acme", `{}`); status != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/recall status=%d, want 405", status)
	}
}

// TestRecallEndpointWithoutRecallerFailsLoudly 缺端口时必须是 500 + 原因，
// 而不是 200 加空 items（那会让页面以为"库里没数据"）。
func TestRecallEndpointWithoutRecallerFailsLoudly(t *testing.T) {
	h := newHarness(t, recallFixtureConfig())
	// 直接构造一个没有 Recaller 的 Server：这是 capabilities 里
	// recall_plan=false 的同一情形。
	bare, err := server.New(server.Options{Ports: h.ports})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	w := &recordingWriter{}
	req, err := http.NewRequest(http.MethodGet, "/api/recall?q=acme", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	bare.Handler().ServeHTTP(w, req)
	if w.status != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500（缺端口不得伪装成空结果）", w.status)
	}
	var e server.ErrorResponse
	if err := json.Unmarshal(w.body, &e); err != nil {
		t.Fatalf("错误体不是 JSON：%v", err)
	}
	if !strings.Contains(e.Message, "Recaller") {
		t.Errorf("message=%q 应说明缺的是 Recaller", e.Message)
	}
	if e.Error != "internal_error" {
		t.Errorf("error=%q, want internal_error", e.Error)
	}
}

// ---------------------------------------------------------------------------
// 7. Meta.capabilities：缺端口时必须显式 false + reason
// ---------------------------------------------------------------------------

// TestCapabilitiesReportMissingPortsExplicitly 缺什么能力就说 false，并给原因。
//
// 这条用例的意义：六页里"没数据"与"答不了"必须能被区分。若缺端口时
// 能力位仍是 true（或 reasons 为空），前端就没有任何办法区分二者。
//
// 走 /api/entity 而不是 /api/graph：未注入 Recaller 时 /api/graph 会因
// graph.go 直接解引用 s.recaller 而 panic（既有缺陷，不在本卡范围，已上报）；
// entity 路径用 recallerConfigRules() 做了 nil 退化，因此能稳定观察能力位。
func TestCapabilitiesReportMissingPortsExplicitly(t *testing.T) {
	h := newHarness(t, recallFixtureConfig())
	h.node("alice", "Alice", store.ScopeUser, "")
	bare, err := server.New(server.Options{Ports: h.ports})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}

	w := &recordingWriter{}
	req, err := http.NewRequest(http.MethodGet, "/api/entity/alice", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	bare.Handler().ServeHTTP(w, req)
	if w.status != http.StatusOK {
		t.Fatalf("/api/entity/alice status=%d, want 200 (body=%s)", w.status, truncate(string(w.body), 200))
	}
	var card server.EntityCard
	if err := json.Unmarshal(w.body, &card); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	caps := card.Meta.Capabilities
	if caps.RecallPlan {
		t.Error("capabilities.recall_plan=true，但该装配没有 Recaller")
	}
	if caps.Audit {
		t.Error("capabilities.audit=true，但该装配没有 Maintainer")
	}
	if caps.Health {
		t.Error("capabilities.health=true，但尚无 HealthReader 端口（P6-4 才引入）")
	}
	joined := strings.Join(caps.Reasons, "；")
	for _, want := range []string{"recall_plan", "audit", "health"} {
		if !strings.Contains(joined, want) {
			t.Errorf("capabilities.reasons 未说明 %s 为 false 的原因：%v", want, caps.Reasons)
		}
	}
	if len(caps.Reasons) == 0 {
		t.Fatal("capabilities.reasons 为空：缺端口不得静默")
	}
}

// TestCapabilitiesTrueWhenPortsInjected 端口齐备时能力位为 true，且不再写原因。
//
// 与上一条对照：能力强必须由**类型断言**决定（端口真的在），不是配置开关。
func TestCapabilitiesTrueWhenPortsInjected(t *testing.T) {
	h := newHarness(t, recallFixtureConfig())
	_, resp, _ := h.getGraph("")
	caps := resp.Meta.Capabilities
	if !caps.RecallPlan {
		t.Error("capabilities.recall_plan=false，但 harness 注入了 Recaller")
	}
	if !caps.Audit {
		t.Error("capabilities.audit=false，但 harness 注入了 Maintainer")
	}
	if !caps.Ontology {
		t.Error("capabilities.ontology=false，但 sqlite 后端实现了 store.OntologyReader")
	}
	joined := strings.Join(caps.Reasons, "；")
	for _, bad := range []string{"recall_plan:", "audit:", "ontology:"} {
		if strings.Contains(joined, bad) {
			t.Errorf("能力已为 true 却仍在 reasons 里抱怨 %q：%v", bad, caps.Reasons)
		}
	}
	// health 是唯一恒 false 的（端口未定义），它必须留下原因。
	if !strings.Contains(joined, "health") {
		t.Errorf("capabilities.reasons 未说明 health 为 false 的原因：%v", caps.Reasons)
	}
}

// TestRecallMetaCarriesCapabilities /api/recall 的 meta 也要带能力位。
func TestRecallMetaCarriesCapabilities(t *testing.T) {
	h := newHarness(t, recallFixtureConfig())
	h.recallFixture()
	status, resp, _ := h.getRecall(recallQuery("q", "acme"))
	if status != http.StatusOK {
		t.Fatalf("status=%d, want 200", status)
	}
	if !resp.Meta.Capabilities.RecallPlan {
		t.Error("meta.capabilities.recall_plan 必须为 true（本端点本身就是它的证据）")
	}
	if resp.Meta.Mode != "recall" {
		t.Errorf("meta.mode=%q, want recall（前端按 mode 判断页面）", resp.Meta.Mode)
	}
	if resp.Meta.KMax != 50 {
		t.Errorf("meta.k_max=%d, want 50（前端据此报错而不是静默夹取）", resp.Meta.KMax)
	}
}

// ---------------------------------------------------------------------------
// 8. as_of：同一 query 两个时间点结果不同（Gmem 必做第五点）
// ---------------------------------------------------------------------------

// TestRecallAsOfReproducesPastVersion 同一 query 在 as_of 下必须看到**当时的版本**。
//
// 这是 Gmem 必做第五点（as_of 下的召回复现）的最小证据：写入 v1（t_created=1000）
// 后改写成 v2（t_created=1001），则：
//
//	不带 as_of → 看到 v2 的内容（"lua"）
//	as_of=1000 → 看到 v1 的内容（"vimrc"），且 URL 里的 as_of 被回显
//
// 变异验证：把 buildRecall 里的 AsOf 从 RecallRequest 里删掉，第二条断言立刻变红。
func TestRecallAsOfReproducesPastVersion(t *testing.T) {
	h := newHarness(t, recallFixtureConfig())
	h.concept("c-editor", "编辑器", store.ConceptCertified, store.ScopeUser, "")
	h.node("n-vim", "vim", store.ScopeUser, "c-editor")
	w := recall.NewWriter(h.ports)

	if _, err := w.PutMemory(context.Background(), store.Memory{
		ID: "m-cfg", Scope: store.ScopeUser, Kind: store.KindSemantic,
		ConceptID: "c-editor", Content: "编辑器配置使用 vimrc 旧版",
		TValid: 1000, TCreated: 1000,
	}); err != nil {
		t.Fatalf("PutMemory v1: %v", err)
	}
	// 改写：v2 的 t_created 晚 1 秒，于是 as_of=1000 时它尚未写入。
	if _, err := w.PutMemory(context.Background(), store.Memory{
		ID: "m-cfg", Scope: store.ScopeUser, Kind: store.KindSemantic,
		ConceptID: "c-editor", Content: "编辑器配置改为 lua 新版",
		TValid: 1000, TCreated: 1001,
	}); err != nil {
		t.Fatalf("PutMemory v2: %v", err)
	}

	// (a) 当前视角：看到 v2。
	status, now, body := h.getRecall(recallQuery("q", "编辑器"))
	if status != http.StatusOK {
		t.Fatalf("status=%d, want 200 (body=%s)", status, truncate(string(body), 200))
	}
	if now.AsOf != nil {
		t.Errorf("不带 as_of 时响应不得回显 as_of：%v", *now.AsOf)
	}
	if !containsContent(now.Items, "lua") {
		t.Fatalf("当前视角没看到 v2 内容：%v", contents(now.Items))
	}

	// (b) as_of=1000：看到 v1，且看不到 v2。
	statusAsOf, past, body := h.getRecall(recallQuery("q", "编辑器", "as_of", "1000"))
	if statusAsOf != http.StatusOK {
		t.Fatalf("as_of 请求 status=%d, want 200 (body=%s)", statusAsOf, truncate(string(body), 200))
	}
	if past.AsOf == nil || *past.AsOf != 1000 {
		t.Fatalf("响应未回显 as_of=1000：%v", past.AsOf)
	}
	if !containsContent(past.Items, "vimrc") {
		t.Fatalf("as_of=1000 时没看到 v1 内容（应看到当时的版本，而不是最新版）：%v", contents(past.Items))
	}
	if containsContent(past.Items, "lua") {
		t.Fatalf("as_of=1000 时看到了 v2 内容（未来答案泄漏到过去）：%v", contents(past.Items))
	}
	// 两个时间点的结果必须能对比：内容确实不同。
	if equalContents(now.Items, past.Items) {
		t.Error("as_of 与当前视角返回了完全相同的内容，无法对比（复现失效）")
	}
}

// equalContents 判断两组结果的内容集合是否完全相同。
func equalContents(a, b []server.RecallItemJSON) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].Content != b[i].Content {
			return false
		}
	}
	return true
}

// containsContent 判断结果里是否有内容包含 needle。
func containsContent(items []server.RecallItemJSON, needle string) bool {
	for _, it := range items {
		if strings.Contains(it.Content, needle) {
			return true
		}
	}
	return false
}

// contents 汇总内容，便于失败信息可读。
func contents(items []server.RecallItemJSON) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.ID+": "+it.Content)
	}
	return out
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

// recordingWriter 是最小 ResponseWriter（用于直接驱动 Handler，绕过 httptest.Server）。
type recordingWriter struct {
	status int
	body   []byte
	header http.Header
}

func (w *recordingWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}

func (w *recordingWriter) Write(b []byte) (int, error) {
	w.body = append(w.body, b...)
	return len(b), nil
}

func (w *recordingWriter) WriteHeader(status int) { w.status = status }
