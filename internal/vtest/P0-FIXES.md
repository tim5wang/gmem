# P0 修复记录（5 个阻塞项 + 6 个重要项）

对应任务卡：`P0 修复：5 个阻塞项（Tx 死锁/双时态版本化/失效语义）+ 6 个重要项`
权威基准：`docs/TD.md` v1.3 §2（含 D12）、§11（含 Tx 陷阱警告）
基线提交：`2bc078b`

## 1. 设计要点

### B-1 跨 Port 事务：把 `*sql.Tx` 注入 ctx，而不是各 Port 自持 `*sql.DB`

`sqlite.conn` 持有连接池与一个"是否有事务在跑"的标志。执行句柄一律按 ctx 解析：

| ctx 状态 | 执行句柄 |
|---|---|
| 回调内（携带事务） | 该 `*sql.Tx` → 同一事务边界 |
| 无事务，但别处有事务在跑 | 立即返回 `store.ErrTxConflict`，**绝不排队等连接** |
| 无事务在跑 | 连接池 |

`Store.Tx` 通过 `withTx(ctx, tx)` 把事务放进回调收到的 ctx。因此回调内用**回调 ctx**
调 `Lexical.Index` / `Vector.Upsert` / `Graph.PutNode` 会落到同一事务；
用外层 ctx 调则快速失败而非挂死。`SetMaxOpenConns(1)` 保留（内存库必需），
但不再导致互等，因为跨 Port 调用不再抢连接。

### B-4 / B-5 系统时间版本化（D12）

- `memories` / `edges` 主键改为 `(id, version)`，以**新增 migration v4** 追加（重建表 + 拷贝，既有行落成 `version=1`）。
- `PutMemory` / `PutEdge`：与当前版本完全等价 → 幂等空操作；否则插入 `version+1`，并把旧版本 `t_expired` 置为新版本的 `t_created`。这是 `t_expired` 的真实写入路径。
- 默认读当前版本（`t_expired IS NULL`）；`as_of` 按 `t_created <= as_of < t_expired` 选版本，
  因此同一 id 写入 `t_created=1100` / `1200` 后，`as_of=1150` 返回**前一版**而不是 0 行。
- `Neighbors` 的 `as_of` 同时判定世界时间与系统时间；`t_expired=150` 的边默认不可见。

### B-2 / B-3 失效语义

- B-2 两层保护：Go 侧新版本继承旧版的 `TInvalid`；SQL 侧 `ON CONFLICT` 用
  `COALESCE(excluded.t_invalid, memories.t_invalid)`，同版本重写也不改写既有失效时间。
- B-3 `InvalidateMemory` 走 `nullableInt`，`tInvalid=0` 落成 SQL `NULL`，不再是 `{Valid:true Int64:0}`。

### I-1 … I-5

| 编号 | 修法 |
|---|---|
| I-1 | PRAGMA 全部移进 DSN `?_pragma=...`，由驱动在**每条新连接**建立时执行；`Open` 自检 `foreign_keys=1` 失败即报错（不再 `continue` 吞掉） |
| I-2 | `NeighborQuery.Scope` 为必填字段，SQL 强制 `fn.scope=? AND tn.scope=?`；空 Scope 直接报错。内存后端同步实现 |
| I-3 | 无 Limit 时显式补 `LIMIT -1`，让 Offset 生效 |
| I-4 | `nodes` 增加 `source_episode` 列（migration v4，`ALTER TABLE ... ADD COLUMN`），`Node.SourceEpisode` 可写可读 |
| I-5 | `ListMemories` 校验 `Kinds`，非法值返回错误 |

## 2. 变异验证（证明测试真能红）

每条都是"把修复改回缺陷形态 → 跑对应用例 → 确认 FAIL → 还原"。

| 变异 | 结果 | 证据 |
|---|---|---|
| `RRF` 的 `k<=0` 改成 `k=1` | RED | `TestRRFDefaultK`: k=0 Score 得到 0.5，want 1/61 |
| 快照行格式把 `scale` 与 `dim` 顺序写反 | RED | `TestVectorSnapshot`: dataLen=3, dim=-2130574328 |
| 去掉 `hops > maxHops` 裁剪 | RED | `TestGraphHopsClamped`: maxDepth=10, want 5 |
| `schema_migrations` DDL 去掉 `IF NOT EXISTS` | RED | `TestMigrateConcurrentSafe`: table already exists |
| 去掉 B-2 的 Go 侧 `TInvalid` 继承 | RED | `TestFixB2`: TInvalid=0, want 500 |
| 去掉 B-5 的记忆 `t_expired` 置位 | RED | `TestFixB4`: v1.t_expired 为 NULL |
| 去掉边的 `t_expired` 置位 | RED | `TestFixB5`: current=2 expired=0, want 1/1 |
| DSN 去掉 `busy_timeout` | RED | `TestFixI1`: 新连接 busy_timeout 不等于配置 |
| 邻域 scope 过滤改成恒真 | RED | `TestFixI2`: 跨 scope 边泄漏 |
| 无 Limit 时丢弃 Offset | RED | `TestFixI3`: 返回 3 行，want 2 |
| 去掉 Kinds 校验 | RED | `TestFixI5`: 非法 Kinds 未报错 |
| `InvalidateMemory` 不走 `nullableInt` | RED | `TestFixB3`: t_invalid 落成非 NULL |

> 记录两处**测试自身缺口**（变异时才发现，已修正）：
> 1. `TestMigrateConcurrentSafe` 原先用同一个 handle 起 4 个 goroutine，不是真并发，对
>    `INSERT OR IGNORE` 与 `busy_timeout` 变异都不敏感。现改为 4 个独立 `sqlite.Open`，
>    并对 `IF NOT EXISTS` 变异确认会红。
> 2. `TestFixB5` 原先给旧边显式写了 `TExpired`，导致"旧版置过期"断言自证。现改用不显式
>    设置 `TExpired` 的边，边侧变异才真正变红。

## 3. 门禁已移除

`GMEM_VERIFY_BLOCKER` 门禁已删掉（`blocker_tx_test.go`、`repro_findings_test.go`）。
这些用例现在是**永久回归**：缺陷一旦重现，`go test ./...` 直接变红。
设 `GMEM_VERIFY_BLOCKER=1` 仍然全绿，但该变量已无作用。

## 4. 运行命令与结果

```bash
export PATH=/usr/local/go/bin:$PATH
go build ./...                                  # OK
go vet ./...                                    # OK
go test ./... -count=1                          # 5/5 包全绿
GOPROXY=off GOFLAGS=-mod=mod go test ./... -count=1   # 全绿（无网络）
GMEM_VERIFY_BLOCKER=1 go test ./internal/vtest/ -count=1      # 全绿
GMEM_VERIFY_BLOCKER=1 go test ./internal/vtest/ \
  -run 'TestBlockerTxWithLexicalDeadlocks|TestReproGraphAndMemoryNotInSameTx' -v   # 两条均 PASS
go test ./internal/vtest/ -run 'TestDependencyAllowlist|TestNoLLMSDKImports|TestNoCGOImport' -v  # PASS
```

## 5. 已知限制

- 迁移 v4 用"建新表 → 拷贝 → 换名"重建 `memories` / `edges`。数据量大时这一步是 O(n) 且需要
  额外磁盘空间；P0 的规模下可接受，生产前应评估。
- `PutMemory` 的"新版本"判定是**整行等价比较**：只有所有字段都相同才视为幂等重写。
  仅 `RecallCount` 变化的写入会产生新版本（当前没有独立的计数更新路径）。
- `NeighborQuery.Scope` 为必填：这是**破坏性契约变更**，调用方必须传 Scope。
- `t_expired` 只由 `PutMemory`/`PutEdge` 的版本推进产生；没有"显式退役某条记忆"的 op
  （属 P1 的 maintain 能力）。
- 并发写仍是单连接串行（`SetMaxOpenConns(1)`），P0 不追求写并发；多进程写入的触发条件见 §11。
- 未做性能与规模验证；未覆盖 P1+ 的召回语义（lane 选择、insufficient、消歧）。

## 6. 回滚

单一提交，`git revert <sha>` 即可。注意：回滚会同时撤销 migration v4 与 `(id, version)` 主键，
已升级的库需要从备份恢复（v4 重建表后旧结构已不可逆还原）。
