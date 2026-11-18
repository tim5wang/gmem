package extract

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// L0 确定性提取（0 次 LLM，docs/TD.md §3）
//
// 存在理由是**成本控制而不是能力降级**：把明显不该进记忆的内容（过短）与
// 能零成本判定的结构（URL/版本号/日期/路径/代码块/数字+单位）在 L0 拦下或定型，
// 只把真正需要语义判断的片段交给 L2。
//
// 七类规则与 TD 表一一对应，顺序固定（AllCategories），因此"拆掉某一类"的
// 变异验证可以对准单一 Category 断言。
// ===========================================================================

// Dictionary 是 L0 词典规则的来源：aliases（最高置信）+ concepts.name。
type Dictionary struct {
	Aliases  []store.Alias
	Concepts []store.Concept
}

// LoadDictionary 通过可选的 OntologyReader 枚举本体与词典。
//
// 后端不实现 OntologyReader 时返回 nil 并让调用方记入 Degraded——
// 词典规则缺席会让命中率下降，但 L0 的其余六类仍然可跑（不得静默失败）。
func LoadDictionary(ctx context.Context, or store.OntologyReader) (Dictionary, error) {
	if or == nil {
		return Dictionary{}, nil
	}
	concepts, err := or.ListConcepts(ctx)
	if err != nil {
		return Dictionary{}, fmt.Errorf("extract: list concepts: %w", err)
	}
	aliases, err := or.ListAliases(ctx)
	if err != nil {
		return Dictionary{}, fmt.Errorf("extract: list aliases: %w", err)
	}
	return Dictionary{Aliases: aliases, Concepts: concepts}, nil
}

// L0 是确定性提取器。零值不可用，用 NewL0 构造。
type L0 struct {
	cfg Config
	// dict 是词典规则的快照。由 SetDictionary 更新（每次 Run 前重载，
	// 这样本体增长后同一批次内的新概念也能立即被词典命中）。
	dict Dictionary
	// bySurface 是 surface(lower) → node 的索引，避免每条片段线性扫全表。
	bySurface map[string]store.Alias
	// conceptNames 是 concept name(lower) → concept。
	conceptNames map[string]store.Concept
	// aliasSurfaces 是按长度降序的 surface 列表（最长匹配优先）。
	aliasSurfaces []string
}

// NewL0 构造 L0 提取器。
func NewL0(cfg Config) *L0 {
	l := &L0{cfg: cfg.withDefaults()}
	l.SetDictionary(Dictionary{})
	return l
}

// SetDictionary 更新词典快照并重建索引。
func (l *L0) SetDictionary(d Dictionary) {
	l.dict = d
	l.bySurface = make(map[string]store.Alias, len(d.Aliases))
	l.aliasSurfaces = make([]string, 0, len(d.Aliases))
	for _, a := range d.Aliases {
		key := strings.ToLower(strings.TrimSpace(a.Surface))
		if key == "" {
			continue
		}
		// 同一 surface 多条时保留置信度最高的那条：L0 词典是"最高置信来源"，
		// 但同一表面形式指向多个节点时不能在这里做消歧决定（那是 §4 的事）。
		if old, ok := l.bySurface[key]; !ok || a.Confidence > old.Confidence {
			l.bySurface[key] = a
		}
		l.aliasSurfaces = append(l.aliasSurfaces, key)
	}
	sort.Slice(l.aliasSurfaces, func(i, j int) bool {
		if len(l.aliasSurfaces[i]) != len(l.aliasSurfaces[j]) {
			return len(l.aliasSurfaces[i]) > len(l.aliasSurfaces[j])
		}
		return l.aliasSurfaces[i] < l.aliasSurfaces[j]
	})
	l.conceptNames = make(map[string]store.Concept, len(d.Concepts))
	for _, c := range d.Concepts {
		key := strings.ToLower(strings.TrimSpace(c.Name))
		if key == "" {
			continue
		}
		l.conceptNames[key] = c
	}
}

// Config 返回生效配置。
func (l *L0) Config() Config { return l.cfg }

// ---------------------------------------------------------------------------
// 规则表
// ---------------------------------------------------------------------------

type structuredRule struct {
	name string
	re   *regexp.Regexp
	// detail 把匹配结果转成 Detail（单位、类型标注）。
	detail func(m []string) string
}

// structuredRules 是"结构化"规则（TD §3 L0 表第一行）：
// 代码块、URL、文件路径、版本号、日期、数字+单位。
//
// 顺序即去重优先：代码块先剥掉，避免块内的 URL/版本号再被当成独立字面量淹没结果。
var structuredRules = []structuredRule{
	{
		name: "code_block",
		re:   regexp.MustCompile("(?s)```([a-zA-Z0-9_+#-]*)\\s*\\n?(.*?)```"),
		detail: func(m []string) string {
			lang := strings.TrimSpace(m[1])
			if lang == "" {
				lang = "text"
			}
			return "lang=" + lang
		},
	},
	{
		name: "url",
		re:   regexp.MustCompile(`https?://[^\s<>"'）)，。；]+`),
		detail: func([]string) string {
			return "url"
		},
	},
	{
		name: "file_path",
		re:   regexp.MustCompile(`(?:\./|\.\./|~/|/)?(?:[A-Za-z0-9_.-]+/)+[A-Za-z0-9_.-]+\.[A-Za-z0-9]{1,8}\b`),
		detail: func([]string) string {
			return "path"
		},
	},
	{
		name: "version",
		re:   regexp.MustCompile(`\bv\d+\.\d+(?:\.\d+)*(?:-[A-Za-z0-9.]+)?\b`),
		detail: func([]string) string {
			return "version"
		},
	},
	{
		name: "date",
		// 三种写法：ISO、中文、斜杠。刻意不把 1.2.3 当日期（那是版本号）。
		re: regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}\b|\b\d{4}年\d{1,2}月\d{1,2}日\b|\b\d{1,2}/\d{1,2}/\d{4}\b`),
		detail: func([]string) string {
			return "date"
		},
	},
	{
		name: "number_unit",
		re:   regexp.MustCompile(`\b\d+(?:\.\d+)?\s?(?:ms|milliseconds?|s|sec|secs|seconds?|min|mins|minutes?|h|hrs?|hours?|kb|mb|gb|tb|tokens?)\b|\d+(?:\.\d+)?\s?(?:条|个|次|天|小时|分钟|秒|毫秒)`),
		detail: func([]string) string {
			return "quantity"
		},
	},
}

// relationPattern 是一条"关系三元组"规则。
//
// 动词短语映射到 store 的关系白名单：抽出来的关系必须能在召回侧的
// RelationRules 里查到，否则图里会出现无法被 planner 使用的边。
type relationPattern struct {
	relation store.Relation
	re       *regexp.Regexp
}

// 主语/宾语字符集：汉字、拉丁、数字与少量标识符符号。
const (
	subjPat = `[\p{Han}A-Za-z0-9_@.\-]{1,40}`
	objPat  = `[\p{Han}A-Za-z0-9_@.\-]{1,40}`
)

// 注意这条表**不在句尾锚定**（没有 “$“）：真实句子里宾语后面常跟一个谓词短语
// （"Alice 使用 vim 写代码"、"认证服务依赖 Redis 做会话缓存"），锚定句尾会让
// 这类最常见的表述一条都抽不出来——L0 的职责是给出**候选**，截断宾语由 L2 修正，
// 而不是为了整齐而漏抽。主语与宾语之间仍要求出现动词短语，因此不会退化成任意匹配。
var relationPatterns = []relationPattern{
	{
		relation: store.RelationUses,
		re:       regexp.MustCompile(`^(` + subjPat + `)\s*(?:使用|用的是|用了|用)\s*(` + objPat + `)`),
	},
	{
		relation: store.RelationPartOf,
		re:       regexp.MustCompile(`^(` + subjPat + `)\s*(?:属于|是一部分|包含于)\s*(` + objPat + `)`),
	},
	{
		relation: store.RelationSubtypeOf,
		re:       regexp.MustCompile(`^(` + subjPat + `)\s*(?:是一种|属于一种)\s*(` + objPat + `)`),
	},
	{
		relation: store.RelationDependsOn,
		re:       regexp.MustCompile(`^(` + subjPat + `)\s*(?:依赖|取决于|requires)\s*(` + objPat + `)`),
	},
	{
		relation: store.RelationOwns,
		re:       regexp.MustCompile(`^(` + subjPat + `)\s*(?:拥有|持有|owns)\s*(` + objPat + `)`),
	},
	{
		relation: store.RelationWorksAt,
		re:       regexp.MustCompile(`^(` + subjPat + `)\s*(?:就职于|就职在|工作于|works at)\s*(` + objPat + `)`),
	},
	{
		relation: store.RelationCausedBy,
		re:       regexp.MustCompile(`^(` + subjPat + `)\s*(?:由|由于|因为)\s*(` + objPat + `)`),
	},
}

var (
	// 定义句：X 是 Y / X 是指 Y / X 指 Y / X：Y / X means Y。
	reDefinitionCN = regexp.MustCompile(`^(.{1,40}?)\s*(?:是指|指的是|是一种|是一个|是|指)\s*(.{2,})$`)
	reDefinitionCL = regexp.MustCompile(`^(.{1,40}?)\s*[:：]\s*(.{2,})$`)
	reDefinitionEN = regexp.MustCompile(`(?i)^(.{1,40}?)\s+(?:means|refers to|is defined as)\s+(.{2,})$`)

	// 偏好句：我(更)?(喜欢|偏好|倾向|不要|避免|别) X。
	rePreferenceCN = regexp.MustCompile(`我(?:更|最|比较|不太)?\s*(?:喜欢|偏好|倾向于|倾向|不要|避免|别用|不喜欢|讨厌)\s*(.+)`)
	rePreferenceEN = regexp.MustCompile(`(?i)\bi\s+(?:prefer|like|don'?t\s+want|avoid|hate)\s+(.+)`)

	// 决策句：决定|选型|定为|改用|弃用（TD §3）。
	reDecision = regexp.MustCompile(`决定|选型|定为|改用|弃用|选定|拍板|\bdecided\b|\bwe chose\b`)

	// 时效信号：待定|等确认|明天|下周|临时（TD §3 最后一行）。
	reRecency = regexp.MustCompile(`待定|等确认|待确认|明天|下周|下个月|临时|暂时|稍后|\btbd\b|\btemporary\b`)
)

var (
	// sentenceSplit 把正文切成句子。中英文标点都要算，冒号不算分隔
	// （定义句的定义部分常含冒号）。
	sentenceSplit = regexp.MustCompile(`[。！？!?；;\n]+`)
	// clauseSplit 再按逗号切，用于关系三元组（"Alice 使用 vim，Bob 使用 emacs"）。
	clauseSplit = regexp.MustCompile(`[，,、]+`)
)

// ---------------------------------------------------------------------------
// 提取
// ---------------------------------------------------------------------------

// Extract 对一段 episode 原文做 L0 确定性提取。
//
// 前置质量门：正文（去空白后）的 rune 数 < MinContentLength 时**整段拒写**，
// 原因进 Rejected —— 它必须对调用方可见，否则"为什么这条没进记忆"无从回答。
func (l *L0) Extract(ctx context.Context, ep RawEpisode) (L0Result, error) {
	if err := ctx.Err(); err != nil {
		return L0Result{}, err
	}
	content := strings.TrimSpace(ep.Content)
	res := L0Result{}
	n := len([]rune(content))
	if n < l.cfg.MinContentLength {
		res.Rejected = append(res.Rejected, fmt.Sprintf(
			"长度下限：正文 %d 字符 < MinContentLength=%d，拒写（TD §3 L0）",
			n, l.cfg.MinContentLength))
		return res, nil
	}

	// 七类规则：顺序固定，便于变异验证与 explain。
	res.Findings = append(res.Findings, l.structured(content)...)
	res.Findings = append(res.Findings, l.dictionary(content)...)
	res.Findings = append(res.Findings, l.definitions(content)...)
	res.Findings = append(res.Findings, l.preferences(content)...)
	res.Findings = append(res.Findings, l.decisions(content)...)
	res.Findings = append(res.Findings, l.relations(content)...)
	res.Findings = append(res.Findings, l.recency(content)...)

	for _, f := range res.Findings {
		if f.Ephemeral {
			res.Ephemeral = true
			break
		}
	}
	return res, nil
}

// structured 提取结构化字面量（Category=CatStructured）。
func (l *L0) structured(content string) []Finding {
	var out []Finding
	for _, rule := range structuredRules {
		for _, m := range rule.re.FindAllStringSubmatch(content, -1) {
			text := strings.TrimSpace(m[0])
			if text == "" {
				continue
			}
			out = append(out, Finding{
				Category:   CatStructured,
				Kind:       "literal",
				Text:       text,
				Detail:     rule.detail(m),
				Confidence: 0.90,
			})
		}
	}
	return dedupeFindings(out)
}

// dictionary 命中 aliases 与 concepts.name（Category=CatDictionary，最高置信）。
//
// 最长匹配优先：aliases 里同时有 "vim" 与 "vim-go" 时，命中后者不再命中前者。
func (l *L0) dictionary(content string) []Finding {
	lower := strings.ToLower(content)
	var out []Finding
	consumed := make([][2]int, 0, 8) // 已命中的 rune 区间，防子串重复命中

	blocked := func(start, end int) bool {
		for _, r := range consumed {
			if start < r[1] && r[0] < end {
				return true
			}
		}
		return false
	}

	// 先在**小写字符串**上定位，再按字节偏移换算 rune 偏移。
	// 只含 ASCII 的 surface 才会出现字节/rune 偏移不一致，因此用 byte 索引
	// 直接做区间判重也成立（用于去重叠已足够，且不引入额外依赖）。
	for _, surface := range l.aliasSurfaces {
		if surface == "" {
			continue
		}
		from := 0
		for {
			idx := strings.Index(lower[from:], surface)
			if idx < 0 {
				break
			}
			start := from + idx
			end := start + len(surface)
			from = end
			if blocked(start, end) {
				continue
			}
			consumed = append(consumed, [2]int{start, end})
			a := l.bySurface[surface]
			out = append(out, Finding{
				Category:   CatDictionary,
				Kind:       "entity",
				Text:       a.Surface,
				Detail:     a.NodeID,
				Confidence: 0.98,
			})
		}
	}

	for name, c := range l.conceptNames {
		if name == "" {
			continue
		}
		idx := strings.Index(lower, name)
		if idx < 0 {
			continue
		}
		out = append(out, Finding{
			Category:   CatDictionary,
			Kind:       "concept",
			Text:       c.Name,
			Detail:     c.ID,
			Confidence: 0.95,
		})
	}
	// map 迭代顺序随机 → 排序保证确定性（同一输入必须产出同一结果）。
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Category != out[j].Category {
			return out[i].Category < out[j].Category
		}
		if out[i].Text != out[j].Text {
			return out[i].Text < out[j].Text
		}
		return out[i].Detail < out[j].Detail
	})
	return out
}

// definitionSubjectStopwords 是不做定义句主语的词。
//
// 为什么必须有它："这是一个足够长的句子"、"那是缓存层"这类日常表述会被
// `X 是 Y` 规则当成概念定义，于是每次写入都凭空造出叫"这""那"的概念草稿，
// 灰区里还会多出"这 vs 某实体"的候选。定义句只在主语**可命名**时才有意义。
// MaxDefinitionSubjectRunes 是定义句主语的**最大字符数**（M5）。
//
// 实体/概念名不会是一个从句：超过这个长度的“主语”几乎必然是句子片段
// （实测 19 字的垃圾实体就是这么来的）。取 12 是因为带小限定语的合法
// 中文/英文概念名（如“身份认证服务”“Kubernetes 集群”——不含空格）就在这个量级内。
const MaxDefinitionSubjectRunes = 12

var definitionSubjectStopwords = map[string]struct{}{
	"这": {}, "那": {}, "它": {}, "他": {}, "她": {}, "此": {}, "其": {},
	"这个": {}, "那个": {}, "这里": {}, "那里": {}, "这些": {}, "那些": {},
	"this": {}, "that": {}, "it": {}, "there": {}, "these": {}, "those": {},
	"我": {}, "你": {}, "我们": {}, "你们": {}, "他们": {},
}

// definitions 提取定义句（Category=CatDefinition）：concept 候选 + definition。
func (l *L0) definitions(content string) []Finding {
	var out []Finding
	for _, sentence := range sentences(content) {
		for _, re := range []*regexp.Regexp{reDefinitionEN, reDefinitionCN, reDefinitionCL} {
			m := re.FindStringSubmatch(sentence)
			if m == nil {
				continue
			}
			subject := strings.TrimSpace(m[1])
			def := strings.TrimSpace(m[2])
			if subject == "" || len([]rune(def)) < 2 {
				continue
			}
			// 主语必须是**一个可命名的词**，而不是句子片段（M5）。
			//
			// 旧规则只要求“1-40 字符 + 含字母/汉字”，于是
			// “vim 使用 插件管理器 来扩展功能，这 是 …” 会把整段从句当成实体名，
			// 每次写入都凭空造一个垃圾节点 + alias，污染消歧候选池。
			//
			// 两道闸：长度上限（实体名不会是一个从句），以及**不得含句读标点**——
			// 逗号/句号/冒号/括号出现就意味着已经跨过了一个短语边界。
			// 内部空格**不拦**：多词实体名（如 “vim 编辑器”）是正常形态，
			// 拦它会把合法概念一起干掉，那是另一个缺陷。
			if len([]rune(subject)) > MaxDefinitionSubjectRunes {
				continue
			}
			if strings.ContainsAny(subject, "\t\u00a0，,。、；;：:！!？?\n\r") {
				continue
			}
			// 主语不可命名（指示代词/人称代词）时不构成定义句：
			// 否则"这是一个句子"会造出叫"这"的概念，并污染消歧候选池。
			if _, stop := definitionSubjectStopwords[strings.ToLower(subject)]; stop {
				continue
			}
			// 主语必须是可命名片段：含至少一个字母或汉字（纯数字/标点不是概念名）。
			if !hasNameRune(subject) {
				continue
			}
			out = append(out, Finding{
				Category:   CatDefinition,
				Kind:       "concept",
				Text:       subject,
				Detail:     def,
				Confidence: 0.85,
			})
			break
		}
	}
	return dedupeFindings(out)
}

// preferences 提取偏好句（Category=CatPreference → preference 类型 memory）。
//
// store 的 MemoryKind 没有 preference：TD §3 的 "preference 类型 memory" 在
// P0 的枚举里没有对应项，因此落成 semantic（内容才是偏好的载体），
// 并在 Finding.Detail 里保留目标片段，由 Pipeline 写进 memory 内容。
func (l *L0) preferences(content string) []Finding {
	var out []Finding
	for _, sentence := range sentences(content) {
		for _, re := range []*regexp.Regexp{rePreferenceCN, rePreferenceEN} {
			m := re.FindStringSubmatch(sentence)
			if m == nil {
				continue
			}
			target := strings.TrimSpace(m[1])
			if target == "" {
				continue
			}
			out = append(out, Finding{
				Category:   CatPreference,
				Kind:       "memory",
				Text:       sentence,
				Detail:     target,
				Confidence: 0.80,
			})
			break
		}
	}
	return dedupeFindings(out)
}

// decisions 提取决策句（Category=CatDecision → decision 类型 memory）。
func (l *L0) decisions(content string) []Finding {
	var out []Finding
	for _, sentence := range sentences(content) {
		if !reDecision.MatchString(sentence) {
			continue
		}
		out = append(out, Finding{
			Category:   CatDecision,
			Kind:       "memory",
			Text:       sentence,
			Confidence: 0.85,
		})
	}
	return dedupeFindings(out)
}

// relations 提取关系三元组（Category=CatRelation → edge 候选）。
func (l *L0) relations(content string) []Finding {
	var out []Finding
	for _, clause := range clauses(content) {
		for _, rp := range relationPatterns {
			m := rp.re.FindStringSubmatch(clause)
			if m == nil {
				continue
			}
			subj := strings.TrimSpace(m[1])
			obj := strings.TrimSpace(m[2])
			if subj == "" || obj == "" || subj == obj {
				continue
			}
			out = append(out, Finding{
				Category:   CatRelation,
				Kind:       "edge",
				Text:       clause,
				Subject:    subj,
				Object:     obj,
				Relation:   rp.relation,
				Confidence: 0.75,
			})
			break
		}
	}
	return dedupeFindings(out)
}

// recency 提取时效信号（Category=CatRecency → ephemeral: true）。
//
// 这是 A-MEM 的写入侧质量门思路：**先便宜地拒掉或降级**，
// 别把临时状态当长期记忆。它只打标，不阻止写入。
func (l *L0) recency(content string) []Finding {
	var out []Finding
	for _, sentence := range sentences(content) {
		if !reRecency.MatchString(sentence) {
			continue
		}
		out = append(out, Finding{
			Category:   CatRecency,
			Kind:       "marker",
			Text:       sentence,
			Detail:     "ephemeral",
			Ephemeral:  true,
			Confidence: 0.70,
		})
	}
	return dedupeFindings(out)
}

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

func sentences(content string) []string {
	raw := sentenceSplit.Split(content, -1)
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		out = append(out, s)
	}
	return out
}

func clauses(content string) []string {
	var out []string
	for _, s := range sentences(content) {
		for _, c := range clauseSplit.Split(s, -1) {
			c = strings.TrimSpace(c)
			if c == "" {
				continue
			}
			out = append(out, c)
		}
	}
	return out
}

// dedupeFindings 去掉同 Category+Text+Detail 的重复项，保留首次出现。
//
// 结构化规则之间会重叠（URL 里常含日期、代码块里含版本号），去重避免同一事实写两次。
func dedupeFindings(in []Finding) []Finding {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]Finding, 0, len(in))
	for _, f := range in {
		key := string(f.Category) + "\x00" + f.Text + "\x00" + f.Detail
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, f)
	}
	return out
}

// hasNameRune 报告 s 是否含可用于命名的字符（字母或汉字）。
func hasNameRune(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) {
			return true
		}
	}
	return false
}
