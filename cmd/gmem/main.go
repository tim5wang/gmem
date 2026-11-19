// Command gmem 是 Gmem 的进程入口。
//
// 本卡（P4-a）只实现 `gmem serve` 一个子命令：起本地 HTTP 数据接口，
// 供 Graph View 前端消费（docs/TD.md §9）。完整 CLI 的五个动词（recall /
// remember / link / review / maintain）留给 P5，因此这里**不**顺手加半成品
// 子命令——半成品 CLI 比没有 CLI 更容易被误用。
//
// # 本包是组合根，也是唯一允许 import sqlite 的地方
//
// 依赖方向：cmd/gmem → server → {store, recall, ops} → （后端由 cmd 注入）。
// server 不 import sqlite（由 server/guard_test.go 钉死），SQL 与 schema
// 细节留在这里装配，这样"换后端只换注入点"的 P0 契约才成立。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tim5wang/gmem/ops"
	"github.com/tim5wang/gmem/recall"
	"github.com/tim5wang/gmem/server"
	"github.com/tim5wang/gmem/sqlite"
)

// defaultAddr 是默认监听地址。
//
// **只绑回环**：这是一个本地单人记忆库，默认暴露到 0.0.0.0 等于把整库实体
// 与关系（含"谁在哪工作"）广播到局域网。要对外必须显式传 --addr。
const defaultAddr = "127.0.0.1:7700"

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "gmem: "+err.Error())
		os.Exit(1)
	}
}

// run 是可测试的入口：错误一律返回，由 main 决定退出码。
func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		usage(stderr)
		return errors.New("缺少子命令（本版本只提供 serve）")
	}
	switch args[0] {
	case "serve":
		return runServe(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		usage(stdout)
		return nil
	default:
		usage(stderr)
		return fmt.Errorf("未知子命令 %q（本版本只提供 serve，其余动词留给 P5）", args[0])
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `gmem —— 单体 Agent 记忆系统

用法：
  gmem serve --db <path> [--addr 127.0.0.1:7700]

serve 的选项：
  --db     SQLite 数据库文件路径（必填；不存在则自动创建并迁移）
  --addr   监听地址，默认 `+defaultAddr+`（默认只绑回环，不对外暴露）

说明：本版本只提供 Graph View 的数据接口（GET /api/graph、GET /api/entity/{id}、
POST /api/review）。完整 CLI（五个动词）与其余页面留给后续版本。
`)
}

// runServe 起 HTTP 服务。
//
// 失败路径都给出**可读**信息并非零退出（端口占用 / 库打不开 / 地址非法）：
// "listen tcp: bind: address already in use" 这种裸错误对使用者没有指导意义。
func runServe(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		dbPath  = fs.String("db", "", "SQLite 数据库文件路径（必填）")
		addr    = fs.String("addr", defaultAddr, "监听地址")
		noWatch = fs.Bool("no-signal-watch", false, "不安装信号处理（测试用）")
	)
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("参数解析失败: %w", err)
	}
	if *dbPath == "" {
		return errors.New("必须指定 --db <path>（数据库文件路径）")
	}
	if strings.TrimSpace(*addr) == "" {
		return errors.New("--addr 不能为空（例：127.0.0.1:7700）")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1) 先开库：库打不开就没必要占端口。
	db, err := sqlite.Open(ctx, sqlite.Config{Path: *dbPath})
	if err != nil {
		return fmt.Errorf("打开数据库 %q 失败: %w", *dbPath, err)
	}
	defer func() { _ = db.Close() }()

	// 2) 装配三个面（同一实现：TD §1「三个接口面，同一实现」）。
	ports := db.Ports()
	runner, err := ops.NewRunner(ports, db.OpsBackend())
	if err != nil {
		return fmt.Errorf("初始化维护面失败: %w", err)
	}
	recaller := recall.New(ports, recall.Config{}, nil) // 无 embedder：vector lane 优雅缺席并在 Degraded 上报
	srv, err := server.New(server.Options{
		Ports:      ports,
		Recaller:   recaller,
		Maintainer: ops.NewMaintainer(runner),
		Catalog:    db.OpsBackend(), // 概览枚举 + 已失效边补齐
	})
	if err != nil {
		return fmt.Errorf("初始化 HTTP 服务失败: %w", err)
	}

	// 3) 显式 Listen：把"端口占用"变成可读错误，而不是 http.ListenAndServe 的裸文案。
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("无法监听 %s: %w（端口被占用时请换 --addr，例如 127.0.0.1:7701）", *addr, err)
	}

	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	if !*noWatch {
		sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()
		go func() {
			<-sigCtx.Done()
			shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer shutCancel()
			_ = httpSrv.Shutdown(shutCtx)
		}()
	}

	fmt.Fprintf(stdout, "gmem serve 已启动：http://%s\n", ln.Addr().String())
	fmt.Fprintf(stdout, "数据库：%s\n", *dbPath)
	fmt.Fprintf(stdout, "接口：GET /api/graph、GET /api/entity/{id}、POST /api/review\n")

	if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("HTTP 服务异常退出: %w", err)
	}
	return nil
}
