# P3 独立测试验证报告

- 被验对象：`ea9e9a1` feat(p3): 生命周期与 op 层（工作区干净；**本包未改任何生产代码、未改 `docs/TD.md`**）
- 验证包：`internal/p3verify/`（**只新增 `_test.go`**，零复用 `ops/*_test.go`，只用公开 API：`ops` / `recall` / `sqlite` / `store`）
- 环境：Go 1.23.9（`/usr/local/go/bin`），`CGO_ENABLED=0`
- 结论：**1 个阻塞合并 + 3 个可后续**。leader 点名要攻击的四项里，**去抖只触发一次、双触发同路径、Prune 只作用于 Archived、D15 在 100 草稿规模下收束**全部**实测成立**；D15 的 Promote 路径有两条覆盖面缺口（见 §3）。

---

## 1. 代码入口与设计理由

| 关注点 | 入口 | 文件 |
|---|---|---|
| 去抖（取消重排） | `Debouncer.Submit` / `fire` | `ops/triggers.go` |
| 四种触发源同一出口 | `Triggers.{AfterWrite,Threshold,Idle,Agent}` | `ops/triggers.go` |
| 唯一执行路径 | `Runner.Run`（幂等台账 → 一个事务 → Plan/Apply） | `ops/runner.go` |
| 幂等 key | `IdempotencyKey` / `withWindow` | `ops/types.go` / `ops/runner.go` |
| 去抖键与扫描窗口 | `scanOps` 表 | `ops/runner.go:58` |
| cancel 语义 | `Run` 的 `canceled` 分支 + `recordCancelled` | `ops/runner.go:219-235` |
| 状态机 | `transitions` / `stateManager.move` | `ops/lifecycle.go` |
| Prune 边界 | `pruneOp.Plan` + `pruneMemory` 二次守卫 | `ops/prune_op.go:45-50,112-119` |
| D15 清理 | `promoteOp` / `pruneOp` 的 draft 分支 | `ops/ops_write.go:518` / `ops/prune_op.go:71-91` |
| draft 统计兜底 | `ListStaleDrafts`（补 use_count=0 行） | `sqlite/ops_backend.go:608` |
| 审计 | `AppendAudit` / `ListAudit` | `sqlite/ops_backend.go:313` |

**为什么用独立包而不是加进 `ops/`**：`ops/*_test.go` 是同一个实现者写的白盒基座，它的
`newEnv` 恰好总是传 `context.Background()`、且只经 `Maintainer` 触发——这两点各掩盖了一个
真实缺陷（见 §3.1 与 §3.2）。独立包强制自己搭基座，缺陷才会暴露。

---

## 2. 四块重点攻击面的实测结论

### 2.1 去抖确实只触发一次，且时刻 = 最后一次提交 + after

`TestDebounceFiresOnceAtLastPlusAfter`（假时钟 + 假调度器，**不靠 sleep**）：
5 次提交、间隔 100ms、`after=400ms` → 断言 `fired==1`、`canceled==4`、
`op_runs` 恰好 1 行，且 `started_at >= 最后一次提交 + after`；并额外断言
「最后一次提交后 399ms 仍未执行」「执行后无残留定时器」「安静 10s 后不再触发」。

`TestDebounceRealSchedulerFiresOnce` 走**生产调度器** `time.AfterFunc`：只测假调度器会
漏掉真实调度器的接线错误（这里同时切了 `Now=time.Now`，否则是拿假时间比真时间）。

> 环境说明：`newEnv` 默认注入手动调度器 + 假时钟。最初只换调度器没换时钟，导致
> `started_at` 是假时间戳、用例自身失效——这是**我的测试的 bug**，已修正（不是产品缺陷）。

**变异验证**：把 `Submit` 改成「每次提交直接 `Run`」（M1）→ 用例变红。

### 2.2 并发去抖：仍只 1 次、无数据竞争

`TestDebounceConcurrentSameKeyFiresOnce`：32 个 goroutine 同时 Submit 同一 key →
`Pending()==1`、`fired==1`、`op_runs` 1 行、且无 `running/failed/cancelled` 残留。
`-race` 全绿（13.2s）。
`TestDebounceDistinctKeysDoNotInterfere`：4 个 scope 各自 1 次、`canceled==0`、`op_runs==4`。

### 2.3 双触发确实同一条代码路径

三个用例逐项比对**外部可观测物**（不是"看起来一样"）：

- `TestAutoAndAgentShareIdenticalPath`：after_write vs Maintainer
- `TestThresholdAndAgentShareIdenticalPath`：threshold（同步、不经去抖）vs Maintainer
- `TestTriggersAgentEntryMatchesAfterWrite`：**`Triggers.Agent`** vs after_write

断言四项：幂等 key 符合 `<op>-<hex32>` 且等于 `IdempotencyKey(scope, op, Input{Window})`
的计算值；`memory_state` 迁移序列相同；`audit_log` 的 `(op, step_kind, subject, changed,
has_before, has_after)` 形状逐条相同；只有 `trigger_kind` 不同。

> **这一项最初漏测了**：只测 `Maintainer` 是不够的，`Triggers.Agent` 是独立公开入口。
> 变异 M2 把它改成自拼 `"agent:"+scope` 前缀时，**原有用例全部存活**。补上
> `TestTriggersAgentEntryMatchesAfterWrite` 后 M2 被杀死。这正是变异验证的价值。

### 2.4 Prune 确实只作用于 Archived

`TestPruneDefaultOnlyTouchesArchived`：六种状态各造一条 → 默认 Prune 后逐条断言
Candidate / Active / Rejected / Invalidated / Stale **全部还在**，只有 Archived 消失，
且其 FTS 行与状态行一并清理（无孤儿、无幽灵召回）。
`TestPruneInvalidatedRequiresExplicitOptIn`：默认不动；显式 `IncludeInvalidated` 才删。

### 2.5 D15 在 100 草稿规模下确实收束

`TestD15HundredDraftsStayBounded`：100 条记忆各带一个隐式 draft（真经 `recall.Writer`
→ `ensureConcept`）→ Promote 15 个 → 另 70 条走**完整合法生命周期**（Active →
Invalidate → 保留期满 Archived → Prune 物理删除）→ Prune 带 DraftTTL。
断言：`draft` 只剩 15 个（仍被引用的）、`certified` 15 个一个不少、
`concepts` 总数 == 30（**漂移收束**）、重复 Promote/Prune 不报错不重复删。
`TestD15PromoteMakesConceptParticipateInHardContract`：Promote 后 Aligner 的
`StatusFactor` 从 `<1.0` 变 `1.0`（真的开始参与硬契约），且 `origin=implicit` 保留。

---

## 3. 发现清单

### 🚫 阻塞合并 #1：去抖执行依赖「提交者 ctx 仍存活」

**用例**：`TestDebounceStillFiresWhenSubmitterCtxCanceled`（`GMEM_VERIFY_BLOCKER=1` 时红）

**现象**：`Debouncer.Submit` 把**提交时的 ctx** 捕获进定时器闭包
（`ops/triggers.go:110` → `func() { d.fire(ctx, key) }`）。写入路径的 ctx 通常是
请求/对话级的，after 之后真正执行时它已被 cancel：

```
fired=1, op_runs.applied=0, op_runs.cancelled=1
```

即「触发了，但什么都没做，而且不会自动重试」——这正是 TD §8 要防的**「永不执行」**极端。

**影响**：去抖在真实宿主里可能完全不产出整理结果；`fired` 计数却显示正常，最难点查。

**建议**：`fire` 里用不继承取消的 ctx（`context.WithoutCancel`，与 `recordCancelled`
同一手法），或在文档里硬性要求宿主传长生命周期 ctx。

### ⚠️ 可后续 #1：`Promote{All:true}` 的幂等 key 是常量 → 只会生效一次

**用例**：`TestNonScanOpConstantKeyBlocksLaterWork`（`GMEM_VERIFY_DEFERRED=1` 时红）

`scanOps` 表刻意不含 Promote，于是 `Promote{All:true}` 的输入是常量、key 也是常量。
第一次认证完当时的 draft；后来新长出的 draft 再调 `Promote(All)` → `RunDuplicate`，
**永远不会被认证**。实测：第二批 3 个 draft 全部留在 draft（`certified=2`）。

**建议**：让「作用域是当下集合」的 op（Promote/Prune 的 batch 模式）窗口参与摘要。

### ⚠️ 可后续 #2：`Promote(All)` 的「在用」判据只由 Extract 一侧填充

**用例**：`TestPromoteAllMissesStructuredWrites`（`GMEM_VERIFY_DEFERRED=1` 时红）

`concept_usage` 只有一个填充点：`extractOp.Apply` 的 `TouchConceptUsage`。因此**不经
Extract** 的结构化写入（TD §10 明确鼓励，省一次 LLM）产生的 draft 永远没有统计行，
`GetConceptStat` 返回 `ok=false`，`Promote(All)` 恒跳过它们。
**不对称之处**：Prune 一侧有 `ListStaleDrafts` 给缺失行补 `(0,0)` 兜底，Promote 没有。

### ⚠️ 可后续 #3：`status=cancelled` 时 `Run` 返回 `err=nil`

**用例**：`TestCanceledRunReturnsNilErrorIsFinding`（`GMEM_VERIFY_DEFERRED=1` 时红）

op 内部放弃（返回 `context.DeadlineExceeded`）时走 cancelled 分支，最后
`return res, ctx.Err()` —— 用的是**外层 ctx**，它没取消，于是 `err=nil`。
调用方按 `if err != nil` 判断会以为成功，实际事务已回滚、什么都没做。

**建议**：cancelled 时返回非 nil（携带真实原因），或在文档里要求必须检查 Status。

---

## 4. 变异验证表（9 个变异体，**9/9 KILLED**）

全部在 `副本` 中打补丁，**未改动工作区生产代码**（`.godex/tmp/p3mut.py`，已随 `.godex/` gitignore）。

| # | 变异 | 改动 | 杀死它的用例 |
|---|---|---|---|
| M1 | 去抖失效 | `Submit` 每次都直接 `Run` | `TestDebounceFiresOnceAtLastPlusAfter` |
| M2 | 双触发分叉 | `Triggers.Agent` 自拼 `"agent:"+scope` key | `TestTriggersAgentEntryMatchesAfterWrite` |
| M3 | Prune 削到 Active | `statuses` 加 `StatusActive` + 去掉二次守卫 | `TestPruneDefaultOnlyTouchesArchived` |
| M4 | 忽略 DraftTTL | `before := now` | `TestD15PruneDraftsRespectsTTLAndRefs` |
| M5 | 去掉引用保护 | `if false && refs > 0` | `TestD15PruneDraftsRespectsTTLAndRefs` |
| M6 | 幂等判重失效 | `dup = false` | `TestIdempotentRerunSkipsWithoutDoubleWrite` |
| M7 | cancel 分类失效 | `isCancel` 恒 `false` | `TestCancelMidRunLeavesNoPartialState` |
| M8 | ops 绕过 Writer | 新增 `s.PutMemory(...)` 调用点 | `TestOpsProductionCodeNeverWritesMemories` |
| M9 | 扫描窗口不参与摘要 | `withWindow` 直接 `return in` | `TestScanOpSameWindowSkippedNewWindowWorks` |

### 诚实说明一处等价变异体（证据边界）

最初我把 M7 写成「cancel 分支 `return err` → `return nil`」（即取消也提交已应用步骤），
**该变异体存活**。原因不是覆盖不足，而是**数据库驱动独立兜底**：事务的 `*sql.Tx` 由已取消的
`runCtx` 创建，`database/sql` 会在 ctx 取消时自动回滚，`Commit` 返回
`commit tx: context canceled`，数据照样不落地。

**这意味着**：「`TestCancelMidRunLeavesNoPartialState` 证明了 ops 的取消语义正确」这个说法
**过强**——该断言在「驱动兜底」与「ops 显式回滚」两条路径下都成立，无法区分。ops 侧真正被
这条用例钉死的是**台账被标成 `cancelled` 而不是卡在 `running`**（否则重跑永久被挡）。
因此我改用 M7（`isCancel` 恒 false，可被区分）来验证取消分类本身。

---

## 5. 运行命令与真实结果

```bash
export PATH=/usr/local/go/bin:$PATH
export CGO_ENABLED=0

# 1) 全量（11 个包全绿）
go test ./...                    # ok × 11（含 internal/p3verify）

# 2) 本包单独
go test ./internal/p3verify/ -count=1        # ok  1.19s（27 个用例）

# 3) 数据竞争（并发去抖靠它）
go test -race ./internal/p3verify/ -count=1  # ok 13.20s

# 4) 门禁：默认绿
go test ./internal/p3verify/ -count=1                                  # ok
GMEM_VERIFY_BLOCKER=1  go test ./internal/p3verify/ -count=1           # FAIL（阻塞项 #1）
GMEM_VERIFY_DEFERRED=1 go test ./internal/p3verify/ -count=1           # FAIL（可后续 #1 #3 及 #2）

# 5) 旧契约未被破坏
GMEM_VERIFY_BLOCKER=1  go test ./internal/p2verify/ -count=1           # ok
GMEM_VERIFY_DEFERRED=1 go test ./internal/p2verify/ -count=1           # ok
go test ./internal/p1verify/ ./internal/vtest/ -count=1                # ok / ok
git diff --stat -- internal/p1verify internal/vtest internal/p2verify ops docs/TD.md   # 空
```

`git status --short` 唯一输出：`?? internal/p3verify/`（生产代码零改动）。

---

## 6. 产物

| 文件 | 用例数 | 覆盖 |
|---|---|---|
| `harness_test.go` | — | 独立假时钟/假调度器/直连 SQL（零复用 `ops/*_test.go`） |
| `gates_test.go` | — | `GMEM_VERIFY_BLOCKER` / `_DEFERRED` 门禁 + 源码扫描 |
| `debounce_test.go` | 5 | 去抖只触发一次（假+真调度器）、并发、键隔离、ctx 取消 |
| `dualtrigger_test.go` | 5 | 双触发同路径 ×3、审计因果链、步骤顺序确定性 |
| `idempotent_test.go` | 5 | 幂等不双写、窗口语义、常量 key、failed 可重试、cancel 返回 nil |
| `prune_d15_test.go` | 8 | Prune 六状态边界、opt-in、as_of、D15 Promote/Prune、回滚、Writer 守卫 |
| `cancel_d15scale_test.go` | 3 | 取消不留半截状态、重试不被卡、100 草稿规模收束 |
| `promote_coverage_test.go` | 1 | `Promote(All)` 漏掉结构化写入的 draft |

合计 **27 个用例**。

---

## 7. 已知限制（未验证 / 无法验证）

1. **只在 SQLite 单后端验证**。PG 后端（D6）未实测：`pruneMemory` 的 FTS/向量删除顺序、
   `ListStaleDrafts` 的 `INSERT OR IGNORE` 都是 SQLite 方言，换后端需重跑。
2. **并发去抖的 -race 覆盖限于单进程**。多进程/多实例共用同一库时的去抖互斥不在 `Debouncer`
   范围内（它只有进程内 `sync.Mutex`），生产多实例部署会各自去抖一次——本包未测，也超出 P3 范围。
3. **阈值触发的水位只在 `Maintainer.Consolidate` 推进**（`ops/maintain.go:170`）。
   自动触发的 threshold 路径**不会**推进水位，因此「未整理条目」只增不减——本包观察到了
   这个事实但没有把它写成门禁用例（需要先与设计意图确认）。
4. **`Triggers.Agent` 在生产代码里没有任何调用点**（只有 `Maintainer` 用 `runner.Run`）。
   它目前是「为对齐四触发源而存在的 API」，宿主接入方式未定；本包按公开契约覆盖了它。
5. **time 相关断言用假时钟**，`RealScheduler` 用例有 2s 等待上限，极慢 CI 上可能偶发。
6. **Prune 后的 as_of**：物理删除后不可回溯已按 TD 断言为预期行为；但「部分架构上
   memories 删了、edges 仍指向它」的跨表一致性**未测**（P3 的 Prune 不碰 edges）。

---

## 8. 回滚说明

本包**只新增测试**，回滚 = 删除 `internal/p3verify/` 整个目录即可，无 schema 变更、
无配置变更、无生产代码依赖。三个门禁用例默认不红，摘除方式：删除对应 `_test.go`
或保持环境变量未置位。
