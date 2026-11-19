package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tim5wang/gmem/demo"
	"github.com/tim5wang/gmem/llm"
	"github.com/tim5wang/gmem/memory"
	"github.com/tim5wang/gmem/ops"
	"github.com/tim5wang/gmem/recall"
	"github.com/tim5wang/gmem/server"
	"github.com/tim5wang/gmem/sqlite"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// CLI（docs/TD.md §10 的命令表）：五动词 + history + 维护面 + graph + doctor + demo
//
// # 三条输出纪律（本文件全部命令都必须遵守）
//
//  1. **默认人类可读**：一行一件事，带对齐的分隔符。
//  2. **--json 输出严格 JSON**：stdout 上只有一个 JSON 文档，可被 jq 直接解析。
//     日志、进度、"已写入 N 条"这类话**一律不得**混进 stdout（否则 jq 立刻报错，
//     而那是最常见的 CLI 事故）。
//  3. **错误走 stderr 且退出码非零**：stdout 只放结果。
//
// # 为什么不用 cobra
//
// 依赖边界（§10 D5 + internal/vtest 的白名单）只允许 modernc.org/sqlite。
// stdlib flag 足够表达"子命令 + 短/长选项"，代价只是多写几十行分发。
// ===========================================================================

// commonFlags 是几乎所有子命令都有的选项。
type commonFlags struct {
	dbPath string
	json   bool
}

// bindCommon 注册 --db 与 --json。
func bindCommon(fs *flag.FlagSet, c *commonFlags) {
	fs.StringVar(&c.dbPath, "db", "", "SQLite 数据库文件路径（必填）")
	fs.BoolVar(&c.json, "json", false, "输出严格 JSON（stdout 上只有一个 JSON 文档）")
}

// newFlagSet 构造子命令的 FlagSet（错误输出走 stderr）。
func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// parseMixed 解析"标志与位置参数混排"的参数，例如：
//
//	gmem recall "query" --explain          ← TD §10 文档里就是这个顺序
//	gmem recall --explain "query"
//
// # 为什么需要它（否则文档里的用法会静默失效）
//
// stdlib 的 flag.Parse 遇到**第一个非标志参数就停止解析**，剩下的全归 fs.Args()。
// 直接调它时，`recall "query" --explain` 里的 --explain 会被当成查询串的一部分
// ——命令不报错、只是没有解释输出。"参数被静默吞掉"是最难查的一类 CLI 缺陷。
//
// 做法：先把参数拆成"标志段"与"位置段"，再让 flag.Parse 只看标志段。
// 拆分后位置参数仍按原相对顺序交给 fs.Args()，因此多词查询串拼接不受影响。
// 用 -- 显式终止标志解析（与 POSIX 一致）。
func parseMixed(fs *flag.FlagSet, args []string) error {
	flags, positional := splitMixedArgs(fs, args)
	return fs.Parse(append(flags, positional...))
}

// splitMixedArgs 把 args 拆成标志段与位置段。
//
// 未知标志**原样保留在标志段**，交给 flag.Parse 报错：本函数不负责判定合法性，
// 否则"未知选项必须非零退出"那条契约就会被绕过。
func splitMixedArgs(fs *flag.FlagSet, args []string) (flags, positional []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			return flags, positional
		}
		if len(a) < 2 || a[0] != '-' {
			positional = append(positional, a)
			continue
		}
		name := strings.TrimLeft(a, "-")
		if strings.Contains(name, "=") {
			// -db=path 形态自带值。
			flags = append(flags, a)
			continue
		}
		f := fs.Lookup(name)
		if f == nil {
			flags = append(flags, a)
			continue
		}
		flags = append(flags, a)
		// 布尔选项不消费下一个参数；其余都要。
		if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
			continue
		}
		if i+1 < len(args) {
			flags = append(flags, args[i+1])
			i++
		}
	}
	return flags, positional
}

// requireDB 校验 --db。
func requireDB(dbPath string) (string, error) {
	p := strings.TrimSpace(dbPath)
	if p == "" {
		return "", errors.New("必须指定 --db <path>（数据库文件路径）")
	}
	return p, nil
}

// emitJSON 输出严格 JSON（缩进 2 空格；末尾换行）。
func emitJSON(w io.Writer, v any) error {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化 JSON 失败: %w", err)
	}
	_, err = w.Write(append(body, '\n'))
	return err
}

// ---------------------------------------------------------------------------
// remember
// ---------------------------------------------------------------------------

// runRemember 实现 `gmem remember`（对齐 Mem0 的 add）。
func runRemember(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := newFlagSet("remember", stderr)
	var c commonFlags
	bindCommon(fs, &c)
	var (
		file      = fs.String("f", "", "从文件读取正文（-f - 表示 stdin）")
		fromStdin = fs.Bool("stdin", false, "从 stdin 读取正文")
		scope     = fs.String("scope", "user", "归属边界：user / agent / project / global")
		kind      = fs.String("kind", "", "记忆类型：semantic / episodic / procedural / decision（省略则走抽取管线）")
		namespace = fs.String("namespace", "", "命名空间（可选）")
		role      = fs.String("role", "", "说话人角色（可选）")
	)
	if err := parseMixed(fs, args); err != nil {
		return fmt.Errorf("参数解析失败: %w", err)
	}
	dbPath, err := requireDB(c.dbPath)
	if err != nil {
		return err
	}
	// 先校验**标志**，再读正文：否则 `--kind bogus --stdin` 会先等 stdin 读完
	// （在管道场景里可能永远等下去）才报"kind 非法"——把参数错误变成了卡死。
	sc := store.Scope(*scope)
	if !sc.Valid() {
		return fmt.Errorf("--scope 必须是 user / agent / project / global 之一，收到 %q", *scope)
	}
	var mk store.MemoryKind
	if *kind != "" {
		mk = store.MemoryKind(*kind)
		if !mk.Valid() {
			return fmt.Errorf("--kind 必须是 semantic / episodic / procedural / decision 之一，收到 %q", *kind)
		}
	}
	content, err := readContent(*file, *fromStdin, stdin)
	if err != nil {
		return err
	}

	ctx := context.Background()
	a, err := openApp(ctx, appOptions{DBPath: dbPath})
	if err != nil {
		return err
	}
	defer func() { _ = a.Close() }()

	res, err := a.service.Remember(ctx, memory.RememberRequest{
		Content: content, Scope: sc, Kind: mk,
		Namespace: *namespace, Role: *role, Actor: "gmem-cli",
	})
	if err != nil {
		return err
	}
	// 降级与拒写必须可见（§13：不得静默）。
	degraded := append(append([]string{}, res.Degraded...), a.llmStrat.Degraded...)

	if c.json {
		return emitJSON(stdout, map[string]any{
			"episode_id": res.EpisodeID, "writes": res.Writes,
			"reviewable": res.Reviewable, "degraded": degraded,
			"warnings": res.Warnings, "rejected": res.Rejected,
			"llm_mode": string(a.llmStrat.Mode),
		})
	}
	if res.EpisodeID != "" {
		fmt.Fprintf(stdout, "episode  %s\n", res.EpisodeID)
	}
	if len(res.Writes) == 0 {
		fmt.Fprintln(stdout, "没有写入任何记忆（见下方的拒写原因）")
	}
	for _, w := range res.Writes {
		fmt.Fprintf(stdout, "记忆     %-28s v%d  %s\n", w.ID, w.Version, w.Kind)
	}
	if res.Reviewable > 0 {
		fmt.Fprintf(stdout, "待审     %d 对（用 gmem review 查看）\n", res.Reviewable)
	}
	printNotes(stdout, "降级", degraded)
	printNotes(stdout, "告警", res.Warnings)
	printNotes(stdout, "拒写", res.Rejected)
	if a.llmStrat.Mode == llm.ModeDegraded {
		// CLI 首次写入打印一次告警（TD §10 的 CLI 行）。
		fmt.Fprintln(stdout, "提示：抽取能力为降级（L0+L1）。配置 GMEM_LLM_* 可启用 L2 抽取。")
	}
	return nil
}

// readContent 解析正文来源：-f 文件 / -f - / --stdin。
func readContent(file string, fromStdin bool, stdin io.Reader) (string, error) {
	switch {
	case file != "" && fromStdin:
		return "", errors.New("-f 与 --stdin 只能二选一")
	case file == "-":
		fromStdin = true
		file = ""
	case file == "" && !fromStdin:
		return "", errors.New("必须给出正文来源：-f <file> 或 --stdin")
	}
	if fromStdin {
		if stdin == nil {
			return "", errors.New("--stdin 需要可读的 stdin")
		}
		body, err := io.ReadAll(stdin)
		if err != nil {
			return "", fmt.Errorf("读取 stdin 失败: %w", err)
		}
		if strings.TrimSpace(string(body)) == "" {
			return "", errors.New("stdin 内容为空")
		}
		return string(body), nil
	}
	body, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("读取文件 %q 失败: %w", file, err)
	}
	if strings.TrimSpace(string(body)) == "" {
		return "", fmt.Errorf("文件 %q 内容为空", file)
	}
	return string(body), nil
}

// printNotes 打印非空提示列表。
func printNotes(w io.Writer, label string, items []string) {
	if len(items) == 0 {
		return
	}
	for _, it := range items {
		fmt.Fprintf(w, "%s     %s\n", label, it)
	}
}

// ---------------------------------------------------------------------------
// recall
// ---------------------------------------------------------------------------

func runRecall(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("recall", stderr)
	var c commonFlags
	bindCommon(fs, &c)
	var (
		explain = fs.Bool("explain", false, "打印 plan（lane 选择 + 分数分解）")
		scope   = fs.String("scope", "", "限定边界：user / agent / project / global")
		asOf    = fs.String("as-of", "", "双时态回溯（unix 秒，或 RFC3339）")
		k       = fs.Int("k", 0, "返回条数")
		kinds   = fs.String("kind", "", "限定记忆类型（逗号分隔）")
	)
	if err := parseMixed(fs, args); err != nil {
		return fmt.Errorf("参数解析失败: %w", err)
	}
	query := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if query == "" {
		return errors.New("缺少查询串：gmem recall \"query\" [--explain]")
	}
	dbPath, err := requireDB(c.dbPath)
	if err != nil {
		return err
	}
	sc := store.Scope(*scope)
	// Scope 是**必填**的隔离边界（recall 的 S-4 守卫不接受空值：空 ≠ 不限）。
	// 因此 CLI 的"可省略"必须落成**一个具体默认值**，而不是把空值原样传下去——
	// 那样用户会撞上内部守卫的报错，而不是得到他想要的默认行为。
	if sc == "" {
		sc = store.ScopeUser
	}
	if !sc.Valid() {
		return fmt.Errorf("--scope 必须是 user / agent / project / global 之一，收到 %q", *scope)
	}
	asOfTS, err := parseAsOfFlag(*asOf)
	if err != nil {
		return err
	}
	ks, err := parseKindList(*kinds)
	if err != nil {
		return err
	}

	ctx := context.Background()
	a, err := openApp(ctx, appOptions{DBPath: dbPath})
	if err != nil {
		return err
	}
	defer func() { _ = a.Close() }()

	res, err := a.service.Recall(ctx, memory.RecallRequest{
		Query: query, Scope: sc, Kinds: ks, AsOf: asOfTS, K: *k, Actor: "gmem-cli",
	})
	if err != nil {
		return err
	}
	degraded := append(append([]string{}, res.Degraded...), a.llmStrat.Degraded...)

	if c.json {
		out := map[string]any{
			"status": string(res.Status), "coverage": res.Coverage,
			"items": recallItemsJSON(res), "missing": res.Missing,
			"degraded": degraded,
		}
		if *explain {
			// --explain 才带完整 plan：默认输出保持可被 jq 消费的稳定形状。
			out["plan"] = res.Plan
		}
		return emitJSON(stdout, out)
	}

	fmt.Fprintf(stdout, "状态     %s（覆盖率 %.2f）\n", res.Status, res.Coverage)
	if len(res.Items) == 0 {
		fmt.Fprintln(stdout, "没有命中任何记忆。")
	}
	for i, it := range res.Items {
		fmt.Fprintf(stdout, "%d. %-26s v%d  score=%.4f  [%s]\n",
			i+1, it.Memory.ID, it.Memory.Version, it.Score, it.Memory.Kind)
		fmt.Fprintf(stdout, "   %s\n", truncateRunes(it.Memory.Content, 160))
	}
	for _, m := range res.Missing {
		fmt.Fprintf(stdout, "缺失     %s（%s）\n", m.Text, m.Reason)
	}
	printNotes(stdout, "降级", degraded)
	if *explain {
		printPlan(stdout, res)
	}
	return nil
}

// printPlan 打印 plan：lane 选择 + 分数分解（R5）。
func printPlan(w io.Writer, res recall.RecallResult) {
	if res.Plan == nil {
		fmt.Fprintln(w, "\n（本次没有 plan：召回未产出计划）")
		return
	}
	fmt.Fprintf(w, "\nplan\n  shape=%s（%s）  coverage=%.3f / θ=%.3f  hops=%d\n",
		res.Plan.Shape, res.Plan.ShapeReason, res.Plan.Coverage, res.Plan.ThetaCoverage, res.Plan.Hops)
	fmt.Fprintln(w, "  lanes:")
	for _, lp := range res.Plan.Lanes {
		state := "未选中"
		switch {
		case lp.Executed:
			state = "已执行"
		case lp.Selected:
			state = "选中但未执行"
		}
		fmt.Fprintf(w, "    %-8s %-16s weight=%.2f budget=%d", lp.Lane, state, lp.Weight, lp.Budget)
		if lp.Reason != "" {
			fmt.Fprintf(w, "  reason=%s", lp.Reason)
		}
		if lp.SkipReason != "" {
			fmt.Fprintf(w, "  skip=%s", lp.SkipReason)
		}
		fmt.Fprintln(w)
	}
	if len(res.Plan.Relations) > 0 {
		fmt.Fprintln(w, "  relations:")
		for _, rp := range res.Plan.Relations {
			fmt.Fprintf(w, "    %-12s transitive=%-5t max_hops=%d origin=%s\n",
				rp.Relation, rp.Transitive, rp.MaxHops, rp.Origin)
		}
	}
	if len(res.Items) > 0 {
		fmt.Fprintln(w, "  分数分解:")
		for _, it := range res.Items {
			fmt.Fprintf(w, "    %s  score=%.4f = fusion %.4f × boost %.3f × recency %.3f\n",
				it.Memory.ID, it.Score, it.FusionScore, it.ConceptBoost, it.RecencyFactor)
			for _, ct := range it.Contributions {
				fmt.Fprintf(w, "      lane=%-8s rank=%d weight=%.2f value=%.4f\n",
					ct.Lane, ct.Rank, ct.Weight, ct.Value)
			}
			for _, ad := range it.Adjustments {
				fmt.Fprintf(w, "      adjust %-20s factor=%.3f  %s\n", ad.Kind, ad.Factor, ad.Reason)
			}
		}
	}
	for _, d := range res.Plan.Dropped {
		fmt.Fprintf(w, "  拦截 %s  %s（score=%.4f）\n", d.ID, d.Reason, d.Score)
	}
}

// ---------------------------------------------------------------------------
// get / list
// ---------------------------------------------------------------------------

func runGet(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("get", stderr)
	var c commonFlags
	bindCommon(fs, &c)
	if err := parseMixed(fs, args); err != nil {
		return fmt.Errorf("参数解析失败: %w", err)
	}
	id := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if id == "" {
		return errors.New("缺少记忆 ID：gmem get <id>")
	}
	dbPath, err := requireDB(c.dbPath)
	if err != nil {
		return err
	}
	ctx := context.Background()
	a, err := openApp(ctx, appOptions{DBPath: dbPath})
	if err != nil {
		return err
	}
	defer func() { _ = a.Close() }()

	m, err := a.service.Get(ctx, id)
	if err != nil {
		return err
	}
	if c.json {
		return emitJSON(stdout, memoryViewJSON(m))
	}
	printMemory(stdout, m)
	return nil
}

func runList(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("list", stderr)
	var c commonFlags
	bindCommon(fs, &c)
	var (
		scope   = fs.String("scope", "", "限定边界")
		kind    = fs.String("kind", "", "限定单类记忆类型")
		limit   = fs.Int("limit", 0, "返回上限（0 = 不限）")
		invalid = fs.Bool("include-invalid", false, "连已失效的事实一起返回")
	)
	if err := parseMixed(fs, args); err != nil {
		return fmt.Errorf("参数解析失败: %w", err)
	}
	dbPath, err := requireDB(c.dbPath)
	if err != nil {
		return err
	}
	sc := store.Scope(*scope)
	if sc != "" && !sc.Valid() {
		return fmt.Errorf("--scope 必须是 user / agent / project / global 之一，收到 %q", *scope)
	}
	ks, err := parseKindList(*kind)
	if err != nil {
		return err
	}
	if len(ks) > 1 {
		return errors.New("--kind 只接受单个类型（要多种请用 --json 配合 recall）")
	}
	ctx := context.Background()
	a, err := openApp(ctx, appOptions{DBPath: dbPath})
	if err != nil {
		return err
	}
	defer func() { _ = a.Close() }()

	mems, err := a.service.List(ctx, memory.ListRequest{
		Scope: sc, Kinds: ks, Limit: *limit, IncludeInvalid: *invalid,
	})
	if err != nil {
		return err
	}
	if c.json {
		views := make([]map[string]any, 0, len(mems))
		for _, m := range mems {
			views = append(views, memoryViewJSON(m))
		}
		return emitJSON(stdout, map[string]any{"count": len(views), "memories": views})
	}
	if len(mems) == 0 {
		fmt.Fprintln(stdout, "（没有记忆）")
		return nil
	}
	fmt.Fprintf(stdout, "%-28s %-4s %-10s %-10s %s\n", "ID", "VER", "SCOPE", "KIND", "内容")
	for _, m := range mems {
		fmt.Fprintf(stdout, "%-28s v%-3d %-10s %-10s %s\n",
			m.ID, m.Version, m.Scope, m.Kind, truncateRunes(m.Content, 72))
	}
	fmt.Fprintf(stdout, "\n共 %d 条\n", len(mems))
	return nil
}

// ---------------------------------------------------------------------------
// forget
// ---------------------------------------------------------------------------

func runForget(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := newFlagSet("forget", stderr)
	var c commonFlags
	bindCommon(fs, &c)
	var (
		scope   = fs.String("scope", "", "按边界批量失效（与 <id> 二选一）")
		yes     = fs.Bool("y", false, "跳过交互确认（等同 --yes）")
		yesLong = fs.Bool("yes", false, "跳过交互确认")
		reason  = fs.String("reason", "", "原因（写入审计）")
		dryRun  = fs.Bool("dry-run", false, "只列出将要失效的记忆，不做任何变更")
	)
	if err := parseMixed(fs, args); err != nil {
		return fmt.Errorf("参数解析失败: %w", err)
	}
	dbPath, err := requireDB(c.dbPath)
	if err != nil {
		return err
	}
	id := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if id == "" && *scope == "" {
		return errors.New("必须给出 <id> 或 --scope（两者都没有就不知道该删什么）")
	}
	if id != "" && *scope != "" {
		return errors.New("<id> 与 --scope 只能二选一")
	}
	sc := store.Scope(*scope)
	if sc != "" && !sc.Valid() {
		return fmt.Errorf("--scope 必须是 user / agent / project / global 之一，收到 %q", *scope)
	}

	ctx := context.Background()
	a, err := openApp(ctx, appOptions{DBPath: dbPath})
	if err != nil {
		return err
	}
	defer func() { _ = a.Close() }()

	// 先算出"将要失效什么"，用它做确认提示与 dry-run 输出。
	targets, err := forgetTargets(ctx, a, id, sc)
	if err != nil {
		return err
	}
	if *dryRun {
		if c.json {
			return emitJSON(stdout, map[string]any{"would_invalidate": targets, "count": len(targets)})
		}
		if len(targets) == 0 {
			fmt.Fprintln(stdout, "（没有可失效的记忆）")
			return nil
		}
		for _, t := range targets {
			fmt.Fprintf(stdout, "将失效  %s\n", t)
		}
		fmt.Fprintf(stdout, "\n共 %d 条（dry-run，未做任何变更）\n", len(targets))
		return nil
	}

	confirmed := *yes || *yesLong
	if !confirmed {
		// **二次确认**：非 -y 时必须交互确认。stdin 不可读时按"未确认"处理。
		fmt.Fprintf(stderr, "将要让以下 %d 条记忆失效（只置 t_invalid，不物理删除，as_of 仍可回溯）：\n", len(targets))
		for _, t := range targets {
			fmt.Fprintf(stderr, "  %s\n", t)
		}
		fmt.Fprint(stderr, "确认请输入 yes（其它任何输入都视为取消）：")
		if stdin == nil {
			return errors.New("需要交互确认，但没有可读的 stdin；确认要删除请加 -y")
		}
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("读取确认失败: %w", err)
		}
		if strings.TrimSpace(strings.ToLower(line)) != "yes" {
			fmt.Fprintln(stderr, "已取消，未做任何变更。")
			return nil
		}
		confirmed = true
	}

	res, err := a.service.Forget(ctx, memory.ForgetRequest{
		ID: id, Scope: sc, Reason: *reason, Actor: "gmem-cli", Confirmed: confirmed,
	})
	if err != nil {
		return err
	}
	if c.json {
		return emitJSON(stdout, map[string]any{
			"invalidated": res.Invalidated, "count": len(res.Invalidated),
			"degraded": res.Degraded, "warnings": res.Warnings,
		})
	}
	for _, x := range res.Invalidated {
		fmt.Fprintf(stdout, "已失效  %s\n", x)
	}
	fmt.Fprintf(stdout, "\n共 %d 条（只置 t_invalid，未物理删除）\n", len(res.Invalidated))
	printNotes(stdout, "告警", res.Warnings)
	printNotes(stdout, "降级", res.Degraded)
	return nil
}

// forgetTargets 计算"将要失效的记忆 ID"（供确认提示与 --dry-run）。
func forgetTargets(ctx context.Context, a *app, id string, sc store.Scope) ([]string, error) {
	if id != "" {
		if _, err := a.service.Get(ctx, id); err != nil {
			return nil, err
		}
		return []string{id}, nil
	}
	mems, err := a.ports.Store.ListMemories(ctx, store.MemoryQuery{Scope: sc})
	if err != nil {
		return nil, fmt.Errorf("枚举 scope %q 的记忆: %w", sc, err)
	}
	out := make([]string, 0, len(mems))
	for _, m := range mems {
		out = append(out, m.ID)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// history
// ---------------------------------------------------------------------------

func runHistory(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("history", stderr)
	var c commonFlags
	bindCommon(fs, &c)
	if err := parseMixed(fs, args); err != nil {
		return fmt.Errorf("参数解析失败: %w", err)
	}
	id := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if id == "" {
		return errors.New("缺少记忆 ID：gmem history <id>")
	}
	dbPath, err := requireDB(c.dbPath)
	if err != nil {
		return err
	}
	ctx := context.Background()
	a, err := openApp(ctx, appOptions{DBPath: dbPath})
	if err != nil {
		return err
	}
	defer func() { _ = a.Close() }()

	res, err := a.service.History(ctx, id)
	if err != nil {
		return err
	}
	if c.json {
		return emitJSON(stdout, res)
	}
	fmt.Fprintf(stdout, "%s 的版本链（%d 版）\n", res.ID, len(res.Entries))
	for i, e := range res.Entries {
		mark := "  "
		if e.IsCurrent {
			mark = "* "
		}
		fmt.Fprintf(stdout, "%sv%d  kind=%s  t_valid=%d", mark, e.Version, e.Kind, e.TValid)
		if e.TInvalid != 0 {
			fmt.Fprintf(stdout, "  t_invalid=%d", e.TInvalid)
		}
		if e.TExpired != 0 {
			fmt.Fprintf(stdout, "  t_expired=%d", e.TExpired)
		}
		fmt.Fprintln(stdout)
		fmt.Fprintf(stdout, "   %s\n", truncateRunes(e.Content, 200))
		if i > 0 {
			// before → after：差异是使用者真正关心的东西。
			if e.Content != res.Entries[i-1].Content {
				fmt.Fprintln(stdout, "   （内容已变更）")
			} else {
				fmt.Fprintln(stdout, "   （内容未变：仅时间戳/运营字段变化）")
			}
		}
	}
	if len(res.Degraded) > 0 {
		printNotes(stdout, "降级", res.Degraded)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 维护面：consolidate / review
// ---------------------------------------------------------------------------

func runConsolidate(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("consolidate", stderr)
	var c commonFlags
	bindCommon(fs, &c)
	var (
		scope = fs.String("scope", "user", "边界：user / agent / project / global")
		limit = fs.Int("limit", 0, "本次处理的条数上限")
	)
	if err := parseMixed(fs, args); err != nil {
		return fmt.Errorf("参数解析失败: %w", err)
	}
	dbPath, err := requireDB(c.dbPath)
	if err != nil {
		return err
	}
	sc := store.Scope(*scope)
	if !sc.Valid() {
		return fmt.Errorf("--scope 必须是 user / agent / project / global 之一，收到 %q", *scope)
	}
	ctx := context.Background()
	a, err := openApp(ctx, appOptions{DBPath: dbPath})
	if err != nil {
		return err
	}
	defer func() { _ = a.Close() }()

	res, err := a.service.Consolidate(ctx, sc, *limit, "gmem-cli")
	if err != nil {
		return err
	}
	if c.json {
		return emitJSON(stdout, opsResultJSON(res))
	}
	fmt.Fprintf(stdout, "consolidate  %s  status=%s  步骤 %d\n", res.Scope, res.Status, len(res.Steps))
	for _, s := range res.Steps {
		if s.Diff.Summary != "" {
			fmt.Fprintf(stdout, "  %s\n", s.Diff.Summary)
		}
	}
	printNotes(stdout, "告警", res.Warnings)
	printNotes(stdout, "降级", res.Degraded)
	return nil
}

func runReview(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("review", stderr)
	var c commonFlags
	bindCommon(fs, &c)
	var scope = fs.String("scope", "", "限定边界（省略 = 全部 scope）")
	if err := parseMixed(fs, args); err != nil {
		return fmt.Errorf("参数解析失败: %w", err)
	}
	dbPath, err := requireDB(c.dbPath)
	if err != nil {
		return err
	}
	sc := store.Scope(*scope)
	if sc != "" && !sc.Valid() {
		return fmt.Errorf("--scope 必须是 user / agent / project / global 之一，收到 %q", *scope)
	}
	ctx := context.Background()
	a, err := openApp(ctx, appOptions{DBPath: dbPath})
	if err != nil {
		return err
	}
	defer func() { _ = a.Close() }()

	// 复用 server 的待审池口径：与 HTTP 裁决、MCP gmem_review 完全一致。
	pending, err := a.server.PendingReviews(ctx, sc)
	if err != nil {
		return err
	}
	if c.json {
		return emitJSON(stdout, map[string]any{"pending": pending, "count": len(pending)})
	}
	if len(pending) == 0 {
		fmt.Fprintln(stdout, "（待审池为空：没有疑似重复的实体对）")
		return nil
	}
	fmt.Fprintf(stdout, "待审池（%d 对）——灰区**绝不自动合并**\n\n", len(pending))
	fmt.Fprintf(stdout, "%-22s %-24s %-24s %-6s %s\n", "EDGE", "FROM", "TO", "SCORE", "SCOPE")
	for _, p := range pending {
		fmt.Fprintf(stdout, "%-22s %-24s %-24s %-6.2f %s\n", p.EdgeID, p.From, p.To, p.Weight, p.Scope)
	}
	fmt.Fprintln(stdout, "\n裁决方式：gmem serve 后在图上点裁，或用 MCP 的 gmem_review（edge_id + verdict + confirmed）。")
	return nil
}

// ---------------------------------------------------------------------------
// graph（导出子图，便于管道处理）
// ---------------------------------------------------------------------------

func runGraph(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("graph", stderr)
	var c commonFlags
	bindCommon(fs, &c)
	var (
		focus   = fs.String("focus", "", "中心节点 ID")
		query   = fs.String("q", "", "关键词（走 Recall 取命中实体作种子）")
		concept = fs.String("concept", "", "只保留该概念下的节点")
		hops    = fs.Int("hops", 0, "跳数（默认 1，受关系传递性表裁剪）")
		asOf    = fs.String("as-of", "", "双时态视角（unix 秒，或 RFC3339）")
		scope   = fs.String("scope", "", "边界")
		budget  = fs.Int("budget", 0, "节点预算")
		format  = fs.String("format", "json", "输出格式：json / dot")
	)
	if err := parseMixed(fs, args); err != nil {
		return fmt.Errorf("参数解析失败: %w", err)
	}
	dbPath, err := requireDB(c.dbPath)
	if err != nil {
		return err
	}
	if *focus != "" && *query != "" {
		return errors.New("--focus 与 --q 只能二选一")
	}
	f := strings.ToLower(strings.TrimSpace(*format))
	if f != "json" && f != "dot" {
		return fmt.Errorf("--format 只接受 json 或 dot，收到 %q", *format)
	}
	sc := store.Scope(*scope)
	if sc != "" && !sc.Valid() {
		return fmt.Errorf("--scope 必须是 user / agent / project / global 之一，收到 %q", *scope)
	}
	asOfTS, err := parseAsOfFlag(*asOf)
	if err != nil {
		return err
	}

	ctx := context.Background()
	a, err := openApp(ctx, appOptions{DBPath: dbPath})
	if err != nil {
		return err
	}
	defer func() { _ = a.Close() }()

	resp, err := a.server.GraphView(ctx, server.GraphQuery{
		Focus: *focus, Query: *query, Concept: *concept, Scope: sc,
		Hops: *hops, AsOf: asOfTS, Budget: *budget,
	})
	if err != nil {
		return err
	}
	if f == "dot" {
		// DOT 直接写 stdout（它就是结果本身，不是日志）。
		return writeDOT(stdout, resp)
	}
	// json / --json 都输出严格 JSON（graph 的天然输出就是 JSON）。
	return emitJSON(stdout, resp)
}

// writeDOT 输出 Graphviz DOT。
//
// 已失效边用 dashed 表示（与 Graph View 的虚线视觉编码一致）：
// CLI 导出的图必须能看出"这条关系已经失效了"，否则管道处理会把它当成现行事实。
func writeDOT(w io.Writer, resp server.GraphResponse) error {
	fmt.Fprintln(w, "digraph gmem {")
	fmt.Fprintln(w, "  rankdir=LR;")
	fmt.Fprintf(w, "  // mode=%s scope=%s nodes=%d edges=%d truncated=%t\n",
		resp.Meta.Mode, resp.Meta.Scope, resp.Meta.NodeCount, resp.Meta.EdgeCount, resp.Meta.Truncated)
	for _, n := range resp.Nodes {
		label := escapeDOT(n.CanonicalName)
		attrs := ""
		if n.ConceptName != "" {
			attrs += fmt.Sprintf(", concept=%q", n.ConceptName)
		}
		if n.ConceptStatus != "" {
			attrs += fmt.Sprintf(", status=%q", n.ConceptStatus)
		}
		fmt.Fprintf(w, "  %q [label=%q%s];\n", n.ID, label, attrs)
	}
	for _, e := range resp.Edges {
		attrs := fmt.Sprintf("label=%q", e.Relation)
		if e.Invalidated {
			attrs += ", style=dashed"
		}
		if e.PossiblySame {
			attrs += ", color=blue"
		}
		fmt.Fprintf(w, "  %q -> %q [%s];\n", e.From, e.To, attrs)
	}
	fmt.Fprintln(w, "}")
	if len(resp.Meta.Degraded) > 0 {
		fmt.Fprintf(w, "// 降级：%s\n", strings.Join(resp.Meta.Degraded, "；"))
	}
	return nil
}

// escapeDOT 转义 DOT 的字符串内容。
func escapeDOT(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `"`, `\"`)
}

// ---------------------------------------------------------------------------
// doctor
// ---------------------------------------------------------------------------

// runDoctor 实现 `gmem doctor --self-test`。
//
// 每一项都输出 PASS / WARN / FAIL，且**降级必须可见**（§13）。
// 退出码：存在 FAIL 时非零；WARN 不影响退出码（否则"没装 LLM"会让 CI 红）。
func runDoctor(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("doctor", stderr)
	var c commonFlags
	bindCommon(fs, &c)
	var selfTest = fs.Bool("self-test", false, "跑完整体检（默认即是；保留此开关以对齐文档）")
	if err := parseMixed(fs, args); err != nil {
		return fmt.Errorf("参数解析失败: %w", err)
	}
	_ = selfTest
	dbPath, err := requireDB(c.dbPath)
	if err != nil {
		return err
	}

	ctx := context.Background()
	report, exitErr := doctorReport(ctx, dbPath)
	if c.json {
		if err := emitJSON(stdout, report); err != nil {
			return err
		}
	} else {
		printDoctor(stdout, report)
	}
	return exitErr
}

// doctorCheck 是一项体检结果。
type doctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"` // PASS | WARN | FAIL
	Detail string `json:"detail"`
	Remedy string `json:"remedy,omitempty"`
}

// doctorReportData 是 doctor 的完整报告（--json 时整体输出）。
type doctorReportData struct {
	DBPath    string           `json:"db_path"`
	Checks    []doctorCheck    `json:"checks"`
	LLM       llm.Status       `json:"llm"`
	Health    any              `json:"health,omitempty"`
	Scopes    map[string]any   `json:"scopes,omitempty"`
	Counts    map[string]int64 `json:"counts,omitempty"`
	Degraded  []string         `json:"degraded,omitempty"`
	Verdict   string           `json:"verdict"`
	AliasNote string           `json:"alias_note"`
}

// doctorReport 跑完全部检查。
func doctorReport(ctx context.Context, dbPath string) (doctorReportData, error) {
	rep := doctorReportData{
		DBPath:    dbPath,
		AliasNote: doctorAliasNote,
		Scopes:    map[string]any{},
	}
	var fail bool

	// 1) 开库（失败即整体 FAIL）。
	db, err := sqlite.Open(ctx, sqlite.Config{Path: dbPath})
	if err != nil {
		rep.Checks = append(rep.Checks, doctorCheck{
			Name: "database", Status: "FAIL", Detail: fmt.Sprintf("打开数据库失败: %v", err),
			Remedy: "检查 --db 路径与目录权限",
		})
		rep.Verdict = "FAIL"
		return rep, fmt.Errorf("体检失败：数据库打不开")
	}
	defer func() { _ = db.Close() }()
	rep.Checks = append(rep.Checks, doctorCheck{
		Name: "database", Status: "PASS", Detail: "数据库可打开（迁移已应用）",
	})

	// 2) schema 版本。
	health, err := db.Health(ctx)
	if err != nil {
		rep.Checks = append(rep.Checks, doctorCheck{
			Name: "schema", Status: "FAIL", Detail: fmt.Sprintf("读取库状态失败: %v", err),
		})
		fail = true
	} else {
		latest := sqlite.LatestSchemaVersion()
		switch {
		case health.SchemaVersion == latest:
			rep.Checks = append(rep.Checks, doctorCheck{
				Name: "schema", Status: "PASS",
				Detail: fmt.Sprintf("schema 版本 v%d（与二进制一致）", health.SchemaVersion),
			})
		case health.SchemaVersion < latest:
			rep.Checks = append(rep.Checks, doctorCheck{
				Name: "schema", Status: "FAIL",
				Detail: fmt.Sprintf("schema 版本 v%d < 二进制期望的 v%d（迁移未跑完）",
					health.SchemaVersion, latest),
				Remedy: "重新运行任意 gmem 命令触发迁移；若仍失败请检查 migations 报错",
			})
			fail = true
		default:
			rep.Checks = append(rep.Checks, doctorCheck{
				Name: "schema", Status: "WARN",
				Detail: fmt.Sprintf("schema 版本 v%d > 二进制期望的 v%d（库比程序新）",
					health.SchemaVersion, latest),
				Remedy: "升级 gmem 到与库匹配的版本，否则部分列可能读不到",
			})
		}
		rep.Health = health
		rep.Counts = health.RowCounts
		for sc, c := range health.ScopeCounts {
			rep.Scopes[string(sc)] = c
		}

		// 3) FTS5。
		if health.FTS5Available {
			rep.Checks = append(rep.Checks, doctorCheck{
				Name: "fts5", Status: "PASS", Detail: "FTS5 可用（memories_fts 查询成功）",
			})
		} else {
			// 没有 FTS5 → 词法通道不可用 → 召回质量大幅下降，这是 FAIL 而不是 WARN。
			rep.Checks = append(rep.Checks, doctorCheck{
				Name: "fts5", Status: "FAIL",
				Detail: "FTS5 不可用: " + health.FTS5Error,
				Remedy: "modernc.org/sqlite 内置 FTS5；若失败通常是库文件损坏或版本不匹配",
			})
			fail = true
		}

		// 4) WAL / busy_timeout / foreign_keys。
		if health.JournalMode == "wal" {
			rep.Checks = append(rep.Checks, doctorCheck{
				Name: "journal_mode", Status: "PASS", Detail: "WAL 已生效",
			})
		} else if health.JournalMode == "memory" {
			rep.Checks = append(rep.Checks, doctorCheck{
				Name: "journal_mode", Status: "WARN",
				Detail: "journal_mode=memory（内存库没有 WAL，这是预期行为）",
			})
		} else {
			rep.Checks = append(rep.Checks, doctorCheck{
				Name: "journal_mode", Status: "WARN",
				Detail: "journal_mode=" + health.JournalMode + "（期望 wal）",
				Remedy: "确认库文件所在文件系统支持 WAL（网络盘不支持）",
			})
		}
		if health.BusyTimeoutMs > 0 {
			rep.Checks = append(rep.Checks, doctorCheck{
				Name: "busy_timeout", Status: "PASS",
				Detail: fmt.Sprintf("busy_timeout=%dms 已生效", health.BusyTimeoutMs),
			})
		} else {
			rep.Checks = append(rep.Checks, doctorCheck{
				Name: "busy_timeout", Status: "FAIL", Detail: "busy_timeout=0（并发写会立刻报 SQLITE_BUSY）",
				Remedy: "检查 DSN 的 _pragma 是否被环境覆盖",
			})
			fail = true
		}
		if health.ForeignKeys {
			rep.Checks = append(rep.Checks, doctorCheck{
				Name: "foreign_keys", Status: "PASS", Detail: "foreign_keys=ON",
			})
		} else {
			rep.Checks = append(rep.Checks, doctorCheck{
				Name: "foreign_keys", Status: "FAIL", Detail: "foreign_keys=OFF（血缘约束失效）",
			})
			fail = true
		}

		// 5) 各 scope 行数快照（空库必须被明确指出来）。
		if health.TotalRows == 0 {
			rep.Checks = append(rep.Checks, doctorCheck{
				Name: "data", Status: "WARN", Detail: "库是空的（所有数据表 0 行）",
				Remedy: "跑 `gmem demo --db " + dbPath + "` 写入演示数据，或 gmem remember 写入真实内容",
			})
		} else {
			rep.Checks = append(rep.Checks, doctorCheck{
				Name: "data", Status: "PASS",
				Detail: fmt.Sprintf("共 %d 行数据（各 scope 明细见 scopes）", health.TotalRows),
			})
		}
	}

	// 6) 抽取器 / LLM 策略（降级必须可见）。
	strat := llm.Resolve(llm.FromEnv(nil), nil, nil)
	rep.LLM = strat.Status(false)
	switch strat.Mode {
	case llm.ModeConfig:
		rep.Checks = append(rep.Checks, doctorCheck{
			Name: "extraction", Status: "PASS",
			Detail: fmt.Sprintf("L2 抽取已启用（model=%s）", strat.Config.Model),
		})
	case llm.ModeSampling:
		rep.Checks = append(rep.Checks, doctorCheck{
			Name: "extraction", Status: "PASS", Detail: "L2 抽取走 MCP sampling（零密钥）",
		})
	default:
		// 降级：WARN（不是 FAIL：没有 LLM 仍能跑 L0+L1，库是可用的）。
		rep.Checks = append(rep.Checks, doctorCheck{
			Name: "extraction", Status: "WARN",
			Detail: "抽取降级为 L0+L1（未配置 GMEM_LLM_* 且无 sampling 能力）：实体/关系抽取不可用",
			Remedy: "设置 GMEM_LLM_BASE_URL / GMEM_LLM_MODEL，或在支持 sampling 的 MCP 客户端里使用",
		})
		rep.Degraded = append(rep.Degraded, strat.Degraded...)
	}
	rep.Degraded = append(rep.Degraded, strat.Warnings...)

	// 7) embedder / vector lane 降级声明。
	// 本二进制不注入 embedder（D7：向量化也是宿主能力），因此 vector lane 恒缺席。
	rep.Checks = append(rep.Checks, doctorCheck{
		Name: "vector_lane", Status: "WARN",
		Detail: "vector lane 降级：未注入 Embedder，召回只用 lexical + graph 两路" +
			"（RecallResult.Degraded 会显式说明）",
		Remedy: "宿主注入 recall.Embedder 后 vector lane 自动启用（Gmem 不自带模型）",
	})

	// 8) 别名层免责声明（写死在 doctor 输出里，任务卡硬要求）。
	rep.Checks = append(rep.Checks, doctorCheck{
		Name: "mcp_alias_layer", Status: "PASS", Detail: doctorAliasNote,
	})

	if fail {
		rep.Verdict = "FAIL"
		return rep, fmt.Errorf("体检发现 FAIL 项：见 checks")
	}
	rep.Verdict = "PASS"
	return rep, nil
}

// doctorAliasNote 是必须出现在 doctor 输出里的免责声明。
const doctorAliasNote = "MCP 兼容别名层（create_entities / create_relations / add_observations / " +
	"search_nodes / open_nodes / read_graph）只做**能力映射**，" +
	"不承诺与 MCP 官方 memory server 的 JSON 存储格式互通"

// printDoctor 人类可读地打印体检报告。
func printDoctor(w io.Writer, rep doctorReportData) {
	fmt.Fprintf(w, "gmem doctor —— %s\n\n", rep.DBPath)
	for _, c := range rep.Checks {
		fmt.Fprintf(w, "[%-4s] %-16s %s\n", c.Status, c.Name, c.Detail)
		if c.Remedy != "" {
			fmt.Fprintf(w, "       %-16s → %s\n", "", c.Remedy)
		}
	}
	if rep.Health != nil {
		if h, ok := rep.Health.(sqlite.Health); ok {
			fmt.Fprintf(w, "\n各 scope 行数快照：\n")
			fmt.Fprintf(w, "  %-10s %-10s %-10s %-10s %s\n", "SCOPE", "EPISODES", "MEMORIES", "NODES", "CONCEPTS")
			for _, sc := range []store.Scope{
				store.ScopeUser, store.ScopeAgent, store.ScopeProject, store.ScopeGlobal,
			} {
				c := h.ScopeCounts[sc]
				fmt.Fprintf(w, "  %-10s %-10d %-10d %-10d %d\n",
					sc, c.Episodes, c.Memories, c.Nodes, c.Concepts)
			}
			fmt.Fprintf(w, "  %-10s 共 %d 行（数据表合计）\n", "", h.TotalRows)
		}
	}
	if len(rep.Degraded) > 0 {
		fmt.Fprintln(w, "\n降级明细（必须可见）：")
		for _, d := range rep.Degraded {
			fmt.Fprintf(w, "  - %s\n", d)
		}
	}
	fmt.Fprintf(w, "\n结论：%s\n", rep.Verdict)
}

// ---------------------------------------------------------------------------
// demo
// ---------------------------------------------------------------------------

func runDemo(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("demo", stderr)
	var c commonFlags
	bindCommon(fs, &c)
	var (
		force = fs.Bool("force", false, "允许对非空库写入（仍然幂等，不重复写入）")
		scope = fs.String("scope", "user", "演示数据落在哪个边界")
	)
	if err := parseMixed(fs, args); err != nil {
		return fmt.Errorf("参数解析失败: %w", err)
	}
	dbPath, err := requireDB(c.dbPath)
	if err != nil {
		return err
	}
	sc := store.Scope(*scope)
	if !sc.Valid() {
		return fmt.Errorf("--scope 必须是 user / agent / project / global 之一，收到 %q", *scope)
	}
	ctx := context.Background()
	a, err := openApp(ctx, appOptions{DBPath: dbPath})
	if err != nil {
		return err
	}
	defer func() { _ = a.Close() }()

	rep, err := demoRun(ctx, a, sc, *force)
	if err != nil {
		return err
	}
	if c.json {
		return emitJSON(stdout, rep)
	}
	fmt.Fprintf(stdout, "演示数据已写入 %s\n\n", dbPath)
	fmt.Fprintf(stdout, "  scope      %s\n", rep.Scope)
	fmt.Fprintf(stdout, "  概念       %d\n", rep.Concepts)
	fmt.Fprintf(stdout, "  节点       %d\n", rep.Nodes)
	fmt.Fprintf(stdout, "  边         %d（含 1 条 possiblySame 待审、1 条已失效）\n", rep.Edges)
	fmt.Fprintf(stdout, "  episodes   %d\n", rep.Episodes)
	fmt.Fprintf(stdout, "  记忆       %d（含多版本链与已失效态）\n", rep.Memories)
	fmt.Fprintf(stdout, "  状态       %d\n", rep.States)
	fmt.Fprintf(stdout, "  别名       %d\n", rep.Aliases)
	if rep.Skipped > 0 {
		fmt.Fprintf(stdout, "  跳过       %d（已存在，未重复写入）\n", rep.Skipped)
	}
	fmt.Fprintf(stdout, "\n库里现在共 %d 行。\n", rep.TotalAfter)
	fmt.Fprintf(stdout, "\n下一步：gmem serve --db %s\n", dbPath)
	return nil
}

// demoRun 是 demo 的装配薄壳（把 app 的端口接到 demo 包）。
//
// 这里只做注入：数据写入全部在 demo.Run 里，且走真实写入路径
// （recall.Writer / PutConcept / PutNode·PutEdge / EnsureState）。
func demoRun(ctx context.Context, a *app, scope store.Scope, force bool) (demo.Report, error) {
	return demo.Run(ctx, demo.Options{
		Ports:   a.ports,
		Probe:   a.db,
		States:  a.db.OpsBackend(),
		History: a.db.OpsBackend(),
		Scope:   scope,
		Force:   force,
	})
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// parseAsOfFlag 解析 --as-of：接受 unix 秒或 RFC3339。
func parseAsOfFlag(v string) (*int64, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return &n, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return nil, fmt.Errorf("--as-of 必须是 unix 秒或 RFC3339 时间，收到 %q", v)
	}
	n := t.Unix()
	return &n, nil
}

// parseKindList 解析逗号分隔的 kind 列表。
func parseKindList(v string) ([]store.MemoryKind, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	parts := strings.Split(v, ",")
	out := make([]store.MemoryKind, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		k := store.MemoryKind(p)
		if !k.Valid() {
			return nil, fmt.Errorf("--kind 必须是 semantic / episodic / procedural / decision 之一，收到 %q", p)
		}
		out = append(out, k)
	}
	return out, nil
}

// truncateRunes 按 rune 截断并加省略号。
func truncateRunes(s string, max int) string {
	rs := []rune(s)
	if len(rs) <= max {
		return s
	}
	return string(rs[:max]) + "…"
}

// printMemory 人类可读地打印一条记忆。
func printMemory(w io.Writer, m store.Memory) {
	fmt.Fprintf(w, "ID        %s\n", m.ID)
	fmt.Fprintf(w, "版本      v%d\n", m.Version)
	fmt.Fprintf(w, "scope     %s\n", m.Scope)
	fmt.Fprintf(w, "kind      %s\n", m.Kind)
	if m.ConceptID != "" {
		fmt.Fprintf(w, "concept   %s\n", m.ConceptID)
	}
	if m.SourceEpisode != "" {
		fmt.Fprintf(w, "episode   %s\n", m.SourceEpisode)
	}
	fmt.Fprintf(w, "t_valid   %d\n", m.TValid)
	if m.TInvalid != 0 {
		fmt.Fprintf(w, "t_invalid %d（已失效，但仍可 as_of 回溯）\n", m.TInvalid)
	}
	fmt.Fprintf(w, "t_created %d\n", m.TCreated)
	if m.TExpired != 0 {
		fmt.Fprintf(w, "t_expired %d\n", m.TExpired)
	}
	fmt.Fprintf(w, "重要度    %.2f   召回次数 %d\n", m.Importance, m.RecallCount)
	fmt.Fprintf(w, "\n%s\n", m.Content)
}

// memoryViewJSON 是记忆的 snake_case 投影（与 MCP / 前端同一套命名）。
func memoryViewJSON(m store.Memory) map[string]any {
	return map[string]any{
		"id": m.ID, "version": m.Version, "scope": string(m.Scope),
		"kind": string(m.Kind), "content": m.Content,
		"concept_id": m.ConceptID, "source_episode": m.SourceEpisode,
		"t_valid": m.TValid, "t_invalid": m.TInvalid,
		"t_created": m.TCreated, "t_expired": m.TExpired,
		"importance": m.Importance, "recall_count": m.RecallCount,
	}
}

// recallItemsJSON 是召回结果的 JSON 投影（保留分数分解，R5）。
//
// 与 MCP 同口径：同一份数据在 CLI 与 MCP 上的字段名必须一致，
// 否则调用方要为两个接入各写一套解析。
func recallItemsJSON(res recall.RecallResult) []map[string]any {
	out := make([]map[string]any, 0, len(res.Items))
	for _, it := range res.Items {
		out = append(out, map[string]any{
			"id": it.Memory.ID, "version": it.Memory.Version,
			"kind": string(it.Memory.Kind), "scope": string(it.Memory.Scope),
			"content": it.Memory.Content, "score": it.Score,
			"fusion_score": it.FusionScore, "concept_boost": it.ConceptBoost,
			"recency_factor": it.RecencyFactor,
			"contributions":  it.Contributions, "adjustments": it.Adjustments,
			"graph_evidence":     it.GraphEvidence,
			"concept_consistent": it.ConceptConsistent,
		})
	}
	return out
}

// opsResultJSON 是 op 结果的 JSON 投影（步骤顺序保留：它本身就是语义）。
func opsResultJSON(res ops.Result) map[string]any {
	steps := make([]string, 0, len(res.Steps))
	for _, s := range res.Steps {
		if s.Diff.Summary != "" {
			steps = append(steps, s.Diff.Summary)
		}
	}
	return map[string]any{
		"op": string(res.Op), "scope": string(res.Scope), "status": string(res.Status),
		"key": res.Key, "steps": steps,
		"degraded": res.Degraded, "warnings": res.Warnings,
	}
}
