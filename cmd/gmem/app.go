package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/tim5wang/gmem/extract"
	"github.com/tim5wang/gmem/llm"
	"github.com/tim5wang/gmem/memory"
	"github.com/tim5wang/gmem/ops"
	"github.com/tim5wang/gmem/recall"
	"github.com/tim5wang/gmem/server"
	"github.com/tim5wang/gmem/sqlite"
	"github.com/tim5wang/gmem/store"
)

// ===========================================================================
// 组合根：一句话把所有面装起来（serve / mcp / CLI / doctor / demo 共用）
//
// # 为什么必须共用一份装配
//
// 三个接入（CLI / MCP / HTTP）声明的行为必须一致。若各自 new 一套
// Recaller / Maintainer，很容易出现"HTTP 用 A 配置、CLI 用 B 配置"的分叉，
// 而这种分叉不会报错——它只会让两边对同一份数据给出不同答案。
//
// 本包是全仓**唯一**允许 import sqlite 的地方（依赖方向：cmd → server → 各port），
// 因此装配也只能在这里。
// ===========================================================================

// app 是装配好的运行时。
type app struct {
	db       *sqlite.DB
	ports    store.Ports
	runner   *ops.Runner
	maintain *ops.Maintainer
	recaller *recall.Recaller
	server   *server.Server
	service  *memory.Service
	// llmStrat 是 D8 的解析结论（含必须可见的降级说明）。
	llmStrat llm.Strategy
	// dbPath 回显给使用者（错误信息里要能看出是哪个库）。
	dbPath string
}

// appOptions 是装配选项。
type appOptions struct {
	// DBPath 是库文件路径（必填）。
	DBPath string
	// Sampler 是 MCP 的 sampling 能力（CLI / serve 没有，传 nil）。
	Sampler llm.Sampler
	// Getenv 可注入的环境变量读取（测试用；nil → os.Getenv）。
	Getenv func(string) string
}

// openApp 打开库并装配全部接口面。
func openApp(ctx context.Context, opts appOptions) (*app, error) {
	if strings.TrimSpace(opts.DBPath) == "" {
		return nil, errors.New("必须指定 --db <path>（数据库文件路径）")
	}
	if opts.Getenv == nil {
		opts.Getenv = os.Getenv
	}

	db, err := sqlite.Open(ctx, sqlite.Config{Path: opts.DBPath})
	if err != nil {
		return nil, fmt.Errorf("打开数据库 %q 失败: %w", opts.DBPath, err)
	}
	ports := db.Ports()

	runner, err := ops.NewRunner(ports, db.OpsBackend())
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("初始化维护面失败: %w", err)
	}
	maint := ops.NewMaintainer(runner)

	// LLM 策略：配置优先，sampling 兜底，两者都无则**显式**降级（D8）。
	// 注意：本二进制不内置出站 HTTP 客户端（D7 + 依赖方向守卫），因此
	// "配置"这一路的 client 由宿主注入；CLI/serve 下 client 恒为 nil。
	llmStrat := llm.Resolve(llm.FromEnv(opts.Getenv), nil, opts.Sampler)

	// 抽取器：只有 sampling 可用时才接到 L2（否则 NoopExtractor，由管线自己报降级）。
	var extractor extract.Extractor
	if llmStrat.Mode == llm.ModeSampling && opts.Sampler != nil {
		extractor = llm.NewExtractor(
			&llm.SamplingClient{Sampler: opts.Sampler, MaxTokens: llmStrat.Config.MaxTokens},
			ports, extract.Config{},
		)
	}

	// 无 embedder：vector lane 优雅缺席并在 Degraded 上报（P1 的既有语义）。
	recaller := recall.New(ports, recall.Config{}, nil)

	srv, err := server.New(server.Options{
		Ports:      ports,
		Recaller:   recaller,
		Maintainer: maint,
		Catalog:    db.OpsBackend(),
	})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("初始化图/实体面失败: %w", err)
	}

	cfg := memory.Config{
		Ports:      ports,
		Recaller:   recaller,
		Maintainer: maint,
		Extractor:  extractor,
		History:    db.OpsBackend(),
	}
	svc, err := memory.New(cfg)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("初始化动词面失败: %w", err)
	}

	return &app{
		db: db, ports: ports, runner: runner, maintain: maint,
		recaller: recaller, server: srv, service: svc,
		llmStrat: llmStrat, dbPath: opts.DBPath,
	}, nil
}

// Close 关闭库。
func (a *app) Close() error { return a.db.Close() }
