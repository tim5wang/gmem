package extract

import (
	"context"
	"strings"
	"testing"

	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// L0 确定性提取：七类规则逐一验证（零 LLM）
//
// 每类一个子用例，且断言的是**该类产出的具体 Finding**——这样"拆掉某条规则"
// 的变异才会精确打红对应用例，而不是被别的规则顺手兜住。
// ===========================================================================

// l0WithDict 构造带词典的 L0（不连库：词典是纯数据）。
func l0WithDict(t *testing.T) *L0 {
	t.Helper()
	l := NewL0(Config{MinContentLength: 10})
	l.SetDictionary(Dictionary{
		Aliases: []store.Alias{
			{Surface: "vim", NodeID: "n-vim", Source: "dict", Confidence: 0.99},
			{Surface: "PostgreSQL", NodeID: "n-pg", Source: "dict", Confidence: 0.95},
		},
		Concepts: []store.Concept{
			{ID: "c-editor", Name: "编辑器", Status: store.ConceptDraft},
		},
	})
	return l
}

func findingsOf(res L0Result, cat FindingCategory) []Finding {
	var out []Finding
	for _, f := range res.Findings {
		if f.Category == cat {
			out = append(out, f)
		}
	}
	return out
}

func textsOf(fs []Finding) string {
	parts := make([]string, 0, len(fs))
	for _, f := range fs {
		parts = append(parts, f.Text)
	}
	return strings.Join(parts, " | ")
}

// TestL0Structured 验证结构化规则：代码块/URL/文件路径/版本号/日期/数字+单位。
func TestL0Structured(t *testing.T) {
	l := l0WithDict(t)
	content := strings.Join([]string{
		"部署脚本见 docs/deploy.md，配置写在 src/config/app.yaml。",
		"参考 https://example.com/spec/v2 的说明。",
		"运行 go build 时需要 30s，内存上限 512 mb。",
		"```go",
		"fmt.Println(1)",
		"```",
		"版本 v1.2.3 于 2025-03-04 发布，日期标注 2025年3月5日。",
	}, "\n")

	res, err := l.Extract(context.Background(), RawEpisode{ID: "e1", Scope: store.ScopeUser, Content: content})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	fs := findingsOf(res, CatStructured)
	if len(fs) == 0 {
		t.Fatalf("结构化规则没有产出任何 Finding（res.Findings=%d）", len(res.Findings))
	}

	// 逐个子规则断言"该类型确实被抓到"：Detail 是子类型标注，用它区分六条规则。
	wantDetails := []string{"lang=go", "url", "path", "version", "date", "quantity"}
	got := make(map[string]bool, len(fs))
	for _, f := range fs {
		got[f.Detail] = true
	}
	for _, detail := range wantDetails {
		if !got[detail] {
			t.Errorf("缺少结构化子规则 %q 的产出；实际：%s", detail, dumpFindings(fs))
		}
	}
}

// TestL0Dictionary 验证词典规则（aliases + concepts.name），且置信度最高。
func TestL0Dictionary(t *testing.T) {
	l := l0WithDict(t)
	content := "我在项目里长期使用 vim 和 PostgreSQL 这两个工具，也常用编辑器插件。"
	res, err := l.Extract(context.Background(), RawEpisode{ID: "e2", Scope: store.ScopeUser, Content: content})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	fs := findingsOf(res, CatDictionary)
	// aliases 两条（vim / PostgreSQL）+ concepts.name 一条（编辑器）。
	for _, want := range []string{"vim", "PostgreSQL", "编辑器"} {
		if !strings.Contains(textsOf(fs), want) {
			t.Errorf("词典规则未命中 %q；实际：%s", want, textsOf(fs))
		}
	}
	for _, f := range fs {
		if f.Confidence < 0.9 {
			t.Errorf("词典命中 %q 的置信度 %.2f 应 >= 0.9（最高置信来源）", f.Text, f.Confidence)
		}
	}
}

// TestL0Definition 验证定义句四种形态（是/指/冒号/means）。
func TestL0Definition(t *testing.T) {
	l := l0WithDict(t)
	content := strings.Join([]string{
		"身份认证服务是一种校验调用方身份的中间件。",
		"灰度发布指的是按比例放量的上线方式。",
		"向量索引：把文本映射成向量后做近邻检索的结构。",
		"idempotency means the same request can be retried safely.",
	}, "")
	res, err := l.Extract(context.Background(), RawEpisode{ID: "e3", Scope: store.ScopeUser, Content: content})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	fs := findingsOf(res, CatDefinition)
	if len(fs) < 4 {
		t.Fatalf("定义句应产出 >=4 条，实际 %d：%s", len(fs), dumpFindings(fs))
	}
	for _, f := range fs {
		if f.Kind != "concept" || f.Detail == "" {
			t.Errorf("定义句 %q 应带 Kind=concept 与 definition，实际 Kind=%q Detail=%q",
				f.Text, f.Kind, f.Detail)
		}
	}
}

// TestL0Preference 验证偏好句（中英两种形态）。
func TestL0Preference(t *testing.T) {
	l := l0WithDict(t)
	content := "我更喜欢用暗色主题写代码。preferences: I prefer tab over spaces in Makefiles."
	res, err := l.Extract(context.Background(), RawEpisode{ID: "e4", Scope: store.ScopeUser, Content: content})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	fs := findingsOf(res, CatPreference)
	if len(fs) == 0 {
		t.Fatalf("偏好句规则没有产出：%s", dumpFindings(res.Findings))
	}
	if !strings.Contains(textsOf(fs), "暗色主题") {
		t.Errorf("偏好句未抓到目标片段：%s", textsOf(fs))
	}
}

// TestL0Decision 验证决策句。
func TestL0Decision(t *testing.T) {
	l := l0WithDict(t)
	content := "我们决定把存储层从 MySQL 改用 SQLite，选型理由是零外部依赖。"
	res, err := l.Extract(context.Background(), RawEpisode{ID: "e5", Scope: store.ScopeUser, Content: content})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	fs := findingsOf(res, CatDecision)
	if len(fs) == 0 {
		t.Fatalf("决策句规则没有产出：%s", dumpFindings(res.Findings))
	}
	if !strings.Contains(textsOf(fs), "决定") {
		t.Errorf("决策句未抓到含'决定'的句子：%s", textsOf(fs))
	}
}

// TestL0Relation 验证关系三元组（动词映射到关系白名单）。
func TestL0Relation(t *testing.T) {
	l := l0WithDict(t)
	content := "Alice 使用 vim 写代码。认证服务依赖 Redis 做会话缓存。"
	res, err := l.Extract(context.Background(), RawEpisode{ID: "e6", Scope: store.ScopeUser, Content: content})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	fs := findingsOf(res, CatRelation)
	if len(fs) < 2 {
		t.Fatalf("关系三元组应产出 >=2 条，实际 %d：%s", len(fs), dumpFindings(fs))
	}
	rels := make(map[store.Relation]bool, len(fs))
	for _, f := range fs {
		if f.Subject == "" || f.Object == "" {
			t.Errorf("关系 %q 缺少主/宾语（%q/%q）", f.Text, f.Subject, f.Object)
		}
		if !relationWhitelisted(f.Relation) {
			t.Errorf("关系 %v 不在白名单内", f.Relation)
		}
		rels[f.Relation] = true
	}
	if !rels[store.RelationUses] || !rels[store.RelationDependsOn] {
		t.Errorf("应同时抽出 uses 与 depends_on，实际 %v", rels)
	}
}

// TestL0Recency 验证时效信号（ephemeral）。
func TestL0Recency(t *testing.T) {
	l := l0WithDict(t)
	content := "这个接口的鉴权方案还是待定的，下周再确认。"
	res, err := l.Extract(context.Background(), RawEpisode{ID: "e7", Scope: store.ScopeUser, Content: content})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(findingsOf(res, CatRecency)) == 0 {
		t.Fatalf("时效信号规则没有产出：%s", dumpFindings(res.Findings))
	}
	if !res.Ephemeral {
		t.Error("含时效信号的片段必须把 L0Result.Ephemeral 置为 true（否则会被当长期记忆）")
	}
}

// TestL0MinLength 验证长度下限（<10 字符拒写）且原因可见。
func TestL0MinLength(t *testing.T) {
	l := l0WithDict(t)
	res, err := l.Extract(context.Background(), RawEpisode{ID: "e8", Scope: store.ScopeUser, Content: "太短了"})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res.Findings) != 0 {
		t.Errorf("过短内容不应产出任何 Finding，实际 %d", len(res.Findings))
	}
	if len(res.Rejected) == 0 {
		t.Fatal("过短内容必须进 Rejected（否则'为什么没写进去'无从回答）")
	}
	if !strings.Contains(res.Rejected[0], "长度下限") {
		t.Errorf("Rejected 文案应说明长度下限，实际 %q", res.Rejected[0])
	}

	// 边界：恰好 10 个字符应当通过。
	// "十个字符刚刚好对吧" = 9 rune；补一个到 10。
	ok, err := l.Extract(context.Background(), RawEpisode{
		ID: "e9", Scope: store.ScopeUser, Content: "十个字符刚刚好对吧啊",
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(ok.Rejected) != 0 {
		t.Errorf("10 字符内容不应被拒写，实际 Rejected=%v", ok.Rejected)
	}
}

// TestL0AllCategoriesPresent 验证七类规则在同一段文本里可以共存，
// 且 AllCategories 的顺序稳定（变异验证按类定位）。
func TestL0AllCategoriesPresent(t *testing.T) {
	l := l0WithDict(t)
	content := strings.Join([]string{
		"我们的身份认证服务是一种校验调用方身份的中间件。",
		"我更喜欢用 vim 编辑代码。",
		"团队决定改用 PostgreSQL 做持久化存储。",
		"认证服务依赖 Redis 做缓存。",
		"这个方案暂时待定，下周再定。",
		"实现见 src/auth/service.go，版本 v2.1.0，延迟 12 ms。",
	}, "")

	res, err := l.Extract(context.Background(), RawEpisode{ID: "e10", Scope: store.ScopeUser, Content: content})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	present := make(map[FindingCategory]bool, len(res.Findings))
	for _, f := range res.Findings {
		present[f.Category] = true
	}
	for _, cat := range AllCategories() {
		if !present[cat] {
			t.Errorf("类别 %q 在这段'七类齐全'的文本里没有产出", cat)
		}
	}
	if len(AllCategories()) != 7 {
		t.Fatalf("AllCategories 必须是七类，实际 %d", len(AllCategories()))
	}
	for i, cat := range AllCategories() {
		if AllCategories()[i] != cat {
			t.Fatalf("AllCategories 必须稳定有序")
		}
	}
}

func dumpFindings(fs []Finding) string {
	var b strings.Builder
	for _, f := range fs {
		b.WriteString(string(f.Category) + "/" + f.Kind + "/" + f.Text + "/" + f.Detail + "; ")
	}
	if b.Len() == 0 {
		return "(空)"
	}
	return b.String()
}
