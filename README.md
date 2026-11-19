# gmem

单体 Agent 记忆系统。设计文档见 [`docs/TD.md`](docs/TD.md)。

## 快速开始

```sh
# 1) 空库也能立刻体验界面（写入一组真实落库的演示数据）
gmem demo --db ./brain.db

# 2) 起本地可视化（只绑回环）
gmem serve --db ./brain.db --addr 127.0.0.1:7700
# 打开 http://127.0.0.1:7700/

# 3) 体检（降级项一律可见；存在 FAIL 时退出码非 0）
gmem doctor --db ./brain.db --self-test
```

构建需要 Go 1.23+：`go build ./...`。仓库内 `web/dist` 已提交，因此**未装 Node 也能构建**。

## CLI

默认输出人类可读；加 `--json` 时 stdout 上只有一个 JSON 文档（可被 `jq` 直接解析），
日志与进度不会混入 stdout；错误一律走 stderr 且退出码非 0。

| 命令 | 说明 |
|---|---|
| `gmem remember -f <file>\|--stdin --scope S [--kind K]` | 写入。给 `--kind` 走结构化写入；不给则走整理管线（L0 规则 + 消歧 + 成图）。对齐 Mem0 的 `add` |
| `gmem recall "query" [--explain] [--scope S] [--as-of T] [--k N]` | 召回。`--explain` 打印 plan（lane 选择 + 分数分解 + 被拦截项）。对齐 Mem0 的 `search` |
| `gmem get <id>` | 取一条记忆的当前版本 |
| `gmem list [--scope S] [--kind K] [--limit N] [--include-invalid]` | 枚举记忆 |
| `gmem forget <id>\|--scope S [-y] [--dry-run]` | 让记忆失效。**需二次确认**：非 `-y` 时交互提示，输入 `yes` 才执行 |
| `gmem history <id>` | 版本链（before/after） |
| `gmem consolidate [--scope S]` | 手动触发整理（与自动触发共用同一套 op） |
| `gmem review [--scope S]` | 打印待审 `possiblySame` 池（灰区**绝不自动合并**） |
| `gmem graph --focus <id> --hops N [--as-of T] [--format json\|dot]` | 导出邻域子图，便于管道处理 |
| `gmem serve --db <path> [--addr ...]` | 起本地 Graph View |
| `gmem mcp --db <path>` | MCP stdio 服务（JSON-RPC 2.0） |
| `gmem doctor --db <path> [--self-test]` | 逐项体检：schema 版本 / FTS5 / WAL / busy_timeout / 各 scope 行数 / 抽取器与 vector lane 降级声明 |
| `gmem demo --db <path> [--force]` | 向**空库**写入演示数据（幂等；非空库默认拒绝） |

### `forget` 的语义（重要）

`forget` **只置 `t_invalid`，不物理删除**——因此 `--as-of` 仍能回溯到它。
真正不可恢复的物理删除只有 Prune，且默认只清理 Archived 状态。

### `gmem demo` 的数据是真的

演示数据全部经**真实写入路径**落库（`recall.Writer` / `PutConcept` / `PutNode`·`PutEdge` /
`EnsureState`），没有任何一行是直接插 DB 或伪造的 API 响应。因此 `/api/graph` 与
`/api/entity` 返回的是从真实表聚合出来的内容。

覆盖的形状包括：`subtype_of` 传递链、`works_at` 非传递、一条 `possiblySame` 待审边、
一条 `t_invalid` 非零的已失效边（图上画虚线），以及多版本与已失效态的记忆。
重复执行不会重复写入（多版本那条按版本链探测跳过），`--force` 也不会让行数增长。

## MCP

```sh
gmem mcp --db ./brain.db      # JSON-RPC 2.0 over stdio；日志只走 stderr
```

工具面（主面，对齐五动词）：

`gmem_remember` / `gmem_recall` / `gmem_get` / `gmem_list` / `gmem_forget` /
`gmem_history` / `gmem_consolidate` / `gmem_review_similar` /
`gmem_merge` / `gmem_link` / `gmem_supersede`

图专用（复用 `server` 包既有的聚合，不另写一套）：

- `gmem_graph`（`focus` / `hops` / `as_of` / `concept` / `q` / `budget`）
- `gmem_review`（不带 `edge_id` 列待审池；带 `edge_id` + `verdict` + `confirmed` 执行裁决）

### 兼容别名层

为让已按 MCP 官方 memory server 写好的客户端零改动接入，额外提供：
`create_entities` / `create_relations` / `add_observations` / `search_nodes` /
`open_nodes` / `read_graph`。

> ⚠️ **别名层只做能力映射，不承诺与 MCP 官方 memory server 的 JSON 存储格式互通。**
> 同名概念在两边落在不同表形态里，因此**不能**靠"读那边写的文件"来迁移数据。
> 这句话同样写死在 `gmem doctor` 的输出里（`mcp_alias_layer` 一项）。

### LLM 策略：配置优先，sampling 兜底

| 情况 | 行为 |
|---|---|
| `GMEM_LLM_BASE_URL` + `GMEM_LLM_MODEL` 已配置 | 走配置（`mode=config`） |
| 未配置，客户端在 `initialize` 声明了 `sampling` 能力 | 走 `sampling/createMessage` 零密钥兜底（`mode=sampling`） |
| 两者都无 | 降级为 L0+L1，**显式**在 `doctor`、`remember` 的工具响应与 `recall` 的 `degraded` 里报出（`mode=degraded`） |

环境变量：`GMEM_LLM_BASE_URL` / `GMEM_LLM_API_KEY` / `GMEM_LLM_MODEL` /
`GMEM_LLM_MAX_TOKENS` / `GMEM_LLM_TIMEOUT` / `GMEM_EXTRACT_MODE`（`auto`\|`l0_only`\|`off`）。

**降级绝不静默**：这是本项目最容易出的事故，因此每条降级路径都有可观测出口。

## 依赖边界

只允许 `modernc.org/sqlite`（纯 Go，内置 FTS5，无 CGO）。

- **不引入任何 MCP SDK / CLI 框架**：JSON-RPC 与 flag 解析全部标准库手写。
- **不引入任何 LLM SDK**（TD §0 D7）：Gmem 不直连 LLM、不保存密钥；
  LLM 始终由宿主（Go package）或客户端（MCP sampling）提供。

以上两条由 `internal/vtest/no_network_test.go` 的静态守卫（`TestDependencyAllowlist` /
`TestNoLLMSDKImports` / `TestServePackagesMakeNoOutboundHTTP`）机器化执行。

## 测试

```sh
go build ./... && go vet ./... && gofmt -l . && go test ./... -count=1
GOPROXY=off go test ./... -count=1          # 零网络
go test -race ./cmd/... ./server/           # 竞态
make web-check                              # 前端产物与源码同步（需 Node）
```
