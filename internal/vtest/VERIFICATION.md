# P0 独立验证报告

验证对象：`2ece059 feat(p0): 骨架落地 — SQLite schema/迁移/四 Port/FTS5 bigram 双侧对称/RRF`
验证者：独立测试（不修改生产代码、不修改 `docs/TD.md`）
环境：Go 1.23.9 (darwin/arm64)、`CGO_ENABLED=0`、仅 `modernc.org/sqlite`

---

## 1. 结论速览

| 验收项 | 结论 | 证据 |
|---|---|---|
| `go build ./...` / `go test ./...` 独立复现 | **通过** | 连跑 8 轮全绿；离线（`GOPROXY=off`）3 轮全绿 |
| FTS5 中文 bigram 双侧对称 | **通过**，并补了两侧各一处"旧实现必挂"的用例 | `bigram_symmetry_test.go` |
| 独立集成测试（2 字命中 / as_of 回溯 / RRF 顺序） | **通过** | `bitemporal_test.go`、`fusion_rrf_test.go`、`conformance_test.go` |
| Port 抽象无 SQL 泄漏（内存后端跑同一套测试） | **通过** | `memports_test.go` + `conformance_test.go`（同一测试体跑 SQLite/内存两后端） |
| 零网络依赖 / 无 LLM SDK import | **通过** | `no_network_test.go`（静态）+ 离线实跑 |
| 问题分类：阻塞 / 可后续 | 见 §3、§4 | 阻塞 1 项、可后续 4 项 |
| 未改生产代码、未改 `docs/TD.md` | **确认** | `git status` 只多出 `internal/vtest/`；`git diff HEAD` 为空 |

**总体判断：P0 主体质量良好，bigram 对称、双时态、迁移幂等、Port 抽象四项攻击面全部站得住。
但有 1 个阻塞项：`Store.Tx` 回调内调用其它 Port 会永久挂死，导致 §11 "图与记忆在同一事务边界内"无法实现。**

---

## 2. 运行方式

```bash
export PATH=/usr/local/go/bin:$PATH
cd /Users/taiwu.wang/Documents/leader_agent/gmem

# 常规验证（已知缺陷的复现用例默认跳过，不会让测试变红）
CGO_ENABLED=0 go build ./...
CGO_ENABLED=0 go vet ./...
CGO_ENABLED=0 go test ./... -count=1

# 离线验证
CGO_ENABLED=0 GOPROXY=off GOSUMDB=off GOFLAGS=-mod=mod go test ./... -count=1

# 缺陷复现（显式门禁）
CGO_ENABLED=0 GMEM_VERIFY_BLOCKER=1 go test ./internal/vtest/ -v -timeout 120s
```

---

## 3. 【阻塞合并】Tx 内跨 Port 调用 ⇒ 永久挂死

**复现**：`TestBlockerTxWithLexicalDeadlocks`、`TestReproGraphAndMemoryNotInSameTx`
（`GMEM_VERIFY_BLOCKER=1`）

**现象**：`Store.Tx` 回调里调用 `Lexical.Index` / `Graph.PutNode` / `Vector.Upsert`
任意一个，调用永不返回；带 ctx 超时时报 `context deadline exceeded`，
错误文本完全不指向真因。

**根因**：`sqlite.Open` 里 `SetMaxOpenConns(1)`（内存库必须如此）。
`Store.Tx` 在回调期间独占这唯一一条连接；`lexical`/`vector`/`graph`
各自持有同一个 `*sql.DB` 并去抢连接 ⇒ 互等。

实测证据（`TestProbeReadInsideTx` 阶段）：

```
A outer-Store-read inside Tx: err=context deadline exceeded elapsed=3.00s
B Lexical.Index inside Tx:    err=context deadline exceeded elapsed=11µs   (ctx 已被 A 耗尽)
C Graph.PutNode inside Tx:    同上
D Vector.Upsert inside Tx:    同上
```

**为什么算阻塞而不是可后续**

- `docs/TD.md` §11 把"图不外包给图数据库"的理由明确写成**"图与记忆在同一个事务边界里"**。
  当前实现做不到，且不是报错而是挂死。
- P1/P2 整理层按设计必然要"写 memory + 写 FTS + 写 graph"原子提交，这条路会直接堵死。
- 静默 hang 比报错更难排查：调用方拿到的只有 `context deadline exceeded`。

**修复方向（不由本次验证实施）**

- a) 让四个 Port 共享事务句柄：`Tx` 把 `*sql.Tx` 注入到 lex/vector/graph 的副本上；
- b) 或明确禁止跨 Port 事务，跨 Port 调用快速返回 `ErrTxConflict`，而不是挂死。
  至少要让失败可见。

**可接受的部分**：同一 Port 内嵌套 `Tx` 是安全的（`TestTxNestedSamePortIsSafe` 通过），
问题被精确圈定在"跨 Port"。

---

## 4. 【可后续】4 项

### #1 `Open` 在 `Ping` 之前还没设上 `busy_timeout`

复现：`TestReproOpenBusyTimeoutOrdering`（`GMEM_VERIFY_BLOCKER=1`）。

```
BusyTimeout = 5s
实际耗时     = 381µs        ← 几乎立即返回
err          = sqlite: ping ...: database is locked (5) (SQLITE_BUSY)
```

`Open` 的顺序是 `PingContext()` → `PRAGMA busy_timeout`，而 `database/sql`
新建连接的 `busy_timeout` 默认 0（不等待），所以 ping 遇写锁立刻失败，
配置的 5s 完全没参与。

生产影响：多进程同时启动（CLI + MCP server）、崩溃后残留锁时，`Open` 会立刻失败。
实测"4 goroutine 并发 Open 同一新库"20 轮失败 8 轮。

> 注：WAL 模式下读者不被写者阻塞，所以这个缺陷只在 rollback-journal 模式或
> `PRAGMA journal_mode=WAL` 切换竞争时出现——这也是它不容易被常规测试发现的原因。

修复方向：pragma 提到 ping 之前；或写进 DSN
`?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)`（已验证 DSN 形式生效）；
或让 ping 容忍 `SQLITE_BUSY`。

### #2 单字中文查询永远零命中

`bigram.Tokens` 只对 CJK 串产出 bigram（长度 1 时才保留单字），
因此索引里**不存在单字 token**，查 `"认"` / `"证"` 一律零命中。

这不是新 bug（实现注释已承认该取舍），但它是一个会被 P1 召回侧踩到的行为边界：
若要支持 1 字查询，必须另加单字列或前缀匹配，不能指望现结构。
已固化为 `TestBigramSingleCharQueryDocumentedGap`。

### #3 外键约束是后端 schema 表达、Port 契约不表达

`aliases.node_id`、`memories.source_episode`、`edges.from_id` 在 SQLite 侧有
`FOREIGN KEY` 约束（`PRAGMA foreign_keys=ON` 已生效，实测拒绝写入）。
但 `store.Ports` 的接口注释未提及这条隐含要求，
故"同一测试体跑两个后端"时，第二个后端（PG 或内存）必须自行复刻引用完整性，
否则 parity 测试会在无关处失败——本次写出内存后端时就踩到了。

建议：把引用完整性写进 Port 文档，作为后端实现必须满足的契约之一。

### #4 向量快照格式是后端私有约定

`VectorIndex.Snapshot` 在 Port 注释里只说"导出量化快照"，没有格式约定。
SQLite 实现用了私有二进制布局（`idLen/id/dim/scale/dataLen/data`），
内存后端只能返回 `nil`。P0 无碍（快照是"可重建的加速物"），
但一旦需要跨后端迁移快照，就必须先定义格式。

---

## 5. 各攻击面的验证细节

### 5.1 bigram 双侧对称（最高优先级）

dev 已有 `TestBigramBothSidesSymmetric`，但它只比对两个函数的输出，
测不出"某一侧真的被接错"的情况。本次改为**分别制造两侧的真实故障**：

- `TestBigramSymmetryIndexSide`：用直连把原文直接写进 `memories_fts.bigrams`
  列（模拟索引侧没做 bigram），断言 2 字查询 `认证/身份/登录` 必须只命中
  正确索引的行、**旧实现的行必须漏召回**；并直接核对落库值等于
  `bigram.IndexText(...)`、且不含未切分的中文长串。
- `TestBigramSymmetryQuerySide`：关键判别用 **3 字查询** `身份认证`。
  索引里只有 bigram token（身份/份认/认证），没有整词 token，所以查询侧不切分必然零命中；
  而 2 字查询在两侧都不切分时会侥幸撞上同名 token —— **只用 2 字查询做验收测不出查询侧缺陷**。
  用例先断言"旧实现（原文 MATCH）在此语料上确实 0 命中"，保证判别力存在，
  再断言经 `bigram.QueryText` 后必须命中。
- `TestBigramBothSidesSameTokens`：对语料里**每一个 2 字中文子串**做查询，
  必须命中它所在的文档，且查询侧 token 必须存在于索引侧 token 集合中。

结论：双侧对称成立，且现在有能真正抓出单侧漂移的用例。

### 5.2 双时态

- `TestBitemporalContradictionKeepsRow`：矛盾后旧记录**物理行仍在**（直连 `COUNT`）、
  `content` 未被改写、`t_invalid` 被置上；`IncludeInvalid` 能看到两条；
  重复失效幂等且**不会把 `t_invalid` 往后推**；对不存在 ID 失效返回 `ErrNotFound`。
- `TestBitemporalAsOfRewinds`：9 个时刻覆盖半开区间端点（`as_of=100` 可见、
  `as_of=499` 可见、`as_of=500` 不可见），并单独验证"系统时间闸门"——
  `as_of=1999` 时新事实世界时间已成立但 `t_created=2000` 尚未入库，必须不可见。
  最后回查 `as_of=300` 的内容确实是旧内容（证明不是新内容覆盖旧行）。
- `TestBitemporalSystemTimeExpiry`：隔离 `t_created`/`t_expired` 轴，
  验证 `t_expired` 端点不可见、早于 `t_created` 不可见。
- `TestBitemporalFiltersCombine`：过滤条件与双时态条件是 AND 而非互相覆盖。

结论：四条时间戳的两条轴语义正确，实现与 §2 一致。

### 5.3 迁移幂等

dev 的 `TestMigrateIdempotent` 只验证"不报错 + 版本不变"。本次补三层：

- `TestMigrateIdempotentNoDoubleWrite`：**先写业务数据再重复迁移**（避免空库假通过），
  断言 `schema_migrations`/`memories_fts`/`memories` 行数都不增长，且迁移后检索仍可用。
- `TestMigrateSurvivesMetadataLoss`：清空 `schema_migrations` 后重跑全部迁移不报错、
  数据存活、版本收敛——这是"DDL 全 `IF NOT EXISTS`"设计的实际检验。
- `TestMigrateConcurrentHandlesConverge`：多轮独立 handle 反复迁移，版本唯一且收敛。

结论：幂等成立。

> 注：首轮把这条写成"4 goroutine 同时 Open"时测出 8/20 随机失败，
> 追查后归因为 §4 #1（ping/busy_timeout 顺序），属于 `Open` 的缺陷而非迁移的缺陷，
> 因此把迁移用例改成不依赖该缺陷的确定性形态，缺陷本身单独复现并隔离。

### 5.4 Port 抽象是否真抽象

判据：**能否在不 import `database/sql`、不写任何 SQL 的前提下实现全部四个 Port。**

答案：能。`memports_test.go` 实现了一套纯内存后端（含 BFS 代替递归 CTE、
快照回滚代替事务），并且：

- 它复用了 `bigram.Tokens` 做两侧分词 —— 说明 bigram 是可复用的独立关注点，
  而不是埋在 sqlite 包里的私有逻辑；
- `TestPortsConformance` 用**同一个测试体**跑 SQLite 与内存两个后端，
  10 组断言（CRUD/双时态/Episode+Concept+Alias/Tx 回滚/中文 2 字/scope 与覆盖写/
  bigram 对称/KNN/图邻域/RRF 顺序）两边同样通过。

结论：SQL 没有泄漏到 Port 之上。隐含契约（引用完整性、快照格式）见 §4 #3/#4。

### 5.5 RRF 融合顺序

独立手算而非复用 dev 的期望值：

```
lex: a r1 b r2 c r3 d r4  |  vec: b r1 c r2  |  grp: c r1 d r2 e r3

a = 1/61                                  = 0.0163934
b = 1/62 + 1/61                           = 0.0325225
c = 1/63 + 1/62 + 1/61                    = 0.0483953
d = 1/64 + 1/62                           = 0.0317540
e = 1/63                                  = 0.0158730
→ 顺序 c > b > d > a > e
```

另外验证了：`Contributions` 能复现 `Score`（§13 "任意一次召回能复现 plan"）、
权重真的改变顺序（并给出 `W/61 > (W+1)/62` 即 `W > 61` 才反超的算术依据）、
空 lane 不稀释其它通道、**真同分**时按 ID 升序且 100 次运行顺序不变、
不修改调用方切片、通道内重复 ID 只算首次。

结论：RRF 正确且稳定可复现。

### 5.6 零网络依赖 / 无 LLM SDK

- `TestNoLLMSDKImports`：解析全部 10 个生产 `.go` 文件的 import，
  禁用片段含各 LLM SDK 与 `net/http`。结果：无命中。
- `TestDependencyAllowlist`：`go.mod` 只允许 modernc SQLite 栈及传递依赖。
- `TestNoCGOImport`：无 `import "C"`。
- 离线实跑：`GOPROXY=off GOSUMDB=off` 下 `go test ./...` 3 轮全绿。

> 说明：`go list -deps` 能看到 `net`/`net/url`/`os/exec`，但它们来自
> `modernc.org/libc`（`os/exec` 用于 libc 探测）与 `github.com/google/uuid`，
> 不是库本体发起的网络调用。库自身源码无 `net/http` import。

---

## 6. 产物

新增目录 `internal/vtest/`（仅测试代码，未触碰生产代码）：

| 文件 | 内容 |
|---|---|
| `helpers_test.go` | 打开内存/文件库、直连观察落库、断言辅助 |
| `bigram_symmetry_test.go` | 双侧对称 4 用例（含两侧旧实现故障注入） |
| `bitemporal_test.go` | 双时态 4 用例（物理存在 / as_of 回溯 / 系统时间轴 / 过滤组合） |
| `fusion_rrf_test.go` | RRF 7 用例（手算顺序 / 权重 / 缺 lane / 平局 / 入参不变 / 去重 / k 语义） |
| `migrate_idem_test.go` | 迁移 4 用例（不双写 / 元数据丢失 / 多轮收敛 / 失败不留半截 schema） |
| `memports_test.go` | 纯内存后端，实现四个 Port |
| `conformance_test.go` | 同一测试体跑 SQLite + 内存后端（10 组断言） |
| `no_network_test.go` | 无 LLM SDK / 依赖白名单 / 无 CGO 的静态断言 |
| `blocker_tx_test.go` | 跨 Port 死锁复现（默认跳过）+ 同 Port 嵌套 Tx 安全 |
| `repro_findings_test.go` | 阻塞项的两个确定性最小复现（默认跳过，`GMEM_VERIFY_BLOCKER=1` 打开） |

## 7. 已知限制

- 内存后端只实现 Port **契约**，不做持久化、不做 SQLite 级事务隔离、
  不实现 BM25（用 token 命中数代替）。因此一致性测试**刻意只断言契约**
  （集合、顺序、错误值），不断言通道内分值。
- `as_of` / 迁移并发相关的结论基于单机单文件、`SetMaxOpenConns(1)` 的现状，
  不代表多进程生产负载。
- 未做性能与规模验证（P0 不要求）。
- 未覆盖 P1+ 才出现的语义（lane 选择、insufficient 判定、消歧）。

## 8. 回滚

本次改动只是新增 `internal/vtest/` 目录，**不涉及任何生产代码与 `docs/TD.md`**。
回滚：`rm -rf internal/vtest/`，无需其它动作。
