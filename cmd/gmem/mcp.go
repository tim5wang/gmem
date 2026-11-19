package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/tim5wang/gmem/extract"
	"github.com/tim5wang/gmem/llm"
	"github.com/tim5wang/gmem/mcp"
	"github.com/tim5wang/gmem/store"
)

// runMCP 起 MCP stdio 服务（JSON-RPC 2.0 over stdio）。
//
// # 三条硬约束（本卡最容易做错的地方）
//
//  1. **stdout 只能有 JSON-RPC 报文**：任何日志、进度、欢迎语都会破坏协议。
//     因此这里把日志固定到 stderr，并且**不打印任何启动横幅**到 stdout。
//  2. **初始化顺序**：先开库（失败要能给出可读错误并退出），再交接协议流。
//     开库失败时 stdout 上不能有半行东西——否则客户端会解析失败而不是看到错误。
//  3. **sampling 兜底**：LLM 配置来自 GMEM_LLM_*；未配置时由服务端在
//     initialize 后按客户端声明的 sampling 能力决定（D8）。因此抽取器必须
//     **延迟**绑定（见下面 SetExtractorProvider 的说明）。
func runMCP(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("mcp", stderr)
	var c commonFlags
	bindCommon(fs, &c)
	var scope = fs.String("scope", "user", "缺省 scope：user / agent / project / global")
	if err := fs.Parse(args); err != nil {
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
	// --json 对 mcp 无意义：stdio 上的输出本身就是 JSON-RPC 报文。
	// 明确拒绝而不是静默忽略——静默忽略会让使用者以为"开了 JSON 模式"。
	if c.json {
		return errors.New("--json 对 mcp 不适用：stdio 上的输出本身就是 JSON-RPC 报文")
	}
	return serveMCP(context.Background(), mcpRunOptions{
		dbPath: dbPath, scope: sc, in: os.Stdin, out: stdout, log: stderr,
	})
}

// mcpRunOptions 是 MCP 的两条流与库路径（便于测试注入）。
type mcpRunOptions struct {
	dbPath string
	scope  store.Scope
	in     io.Reader
	out    io.Writer
	log    io.Writer
}

// serveMCP 装配并运行 MCP 服务。
//
// 先开库再交接协议流：开库失败时 stdout 上一行都没有，客户端看到的是
// "进程退出 + stderr 里的原因"，而不是一个解析到一半的 JSON。
func serveMCP(ctx context.Context, o mcpRunOptions) error {
	a, err := openApp(ctx, appOptions{DBPath: o.dbPath})
	if err != nil {
		return err
	}
	defer func() { _ = a.Close() }()

	cfg := llm.FromEnv(nil)
	// sampler 在 mcp.New 内部创建（它需要持有写锁与回包投递表），
	// 通过 OnSampler 回调把它交给这里，供下面的延迟抽取器使用。
	var sampler llm.Sampler

	srv, err := mcp.New(mcp.Options{
		Service: a.service,
		Graph:   a.server,
		// 收**配置**而不是收结论：sampling 能力在 initialize 之后才知道（D8）。
		LLMConfig: cfg,
		In:        o.in,
		Out:       o.out,
		Log:       o.log,
		Scope:     o.scope,
		OnSampler: func(s llm.Sampler) { sampler = s },
	})
	if err != nil {
		return err
	}

	// 延迟绑定抽取器：每次 remember 时才判断"现在有没有 L2"。
	// 客户端在 initialize 里声明 sampling 之后，这里立刻返回可用的抽取器；
	// 之前返回 nil（管线用自己的 NoopExtractor，并把降级写进 Degraded）。
	//
	// 两条路径（config / sampling）落成**同一个** extract.LLMExtractor，
	// 因此产出的结果形状与降级语义完全相同（§3.3 第 3 条注意点）。
	a.service.SetExtractorProvider(func() extract.Extractor {
		if sampler == nil || !sampler.SamplingAvailable() {
			return nil
		}
		return llm.NewExtractor(
			&llm.SamplingClient{Sampler: sampler, MaxTokens: cfg.MaxTokens},
			a.ports, extract.Config{},
		)
	})

	fmt.Fprintln(o.log, "gmem mcp: 已就绪（JSON-RPC 2.0 over stdio；日志走 stderr）")
	return srv.Serve(ctx)
}
