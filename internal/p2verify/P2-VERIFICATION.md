# P2 独立测试验证报告

- 被验对象：`e386d10` feat(p2): 提取与消歧（HEAD，工作区干净，未改任何生产代码 / 未改 `docs/TD.md`）
- 验证包：`internal/p2verify/`（**只新增 `_test.go`**，零复用 `extract/*_test.go`，只用公开 API）
- 环境：Go 1.23.9（`/usr/local/go/bin`），`CGO_ENABLED=0`
- 结论：**1 个阻塞合并 + 4 个可后续 + 3 个门禁风险项**。D10 的 `MaxTurns+1` 硬保证、工具面只读与跳数夹紧、灰区不合并、写入必经 `recall.Writer` 这四块**实测成立**。

---

## 1. 代码入口与设计理由

| 关注点 | 入口 | 文件 |
|---|---|---|
| 多轮循环与五门禁（D10） | `LLMExtractor.Extract` | `extract/llm.go` |
| 轨迹可审计 | `LLMExtractor.LastTrajectory` | `extract/llm.go` |
| 三个只读工具 | `ToolSpecs` / `toolRunner.run` | `extract/tools.go` |
| hops 硬上限 | `HardMaxHops = 1`（常量，非配置） | `extract/tools.go` |
| 三区判定 | `Disambiguator.scoreMention` | `extract/disambiguate.go` |
| 灰区记账 / 去重 | `appendGrayCandidate` / `pairHasPending` | `extract/pipeline.go` |
| 裁决 | `Pipeline.Judge` | `extract/pipeline.go` |
| 唯一写入入口 | `Pipeline.write` → `recall.Writer.Put` | `extract/pipeline.go` |
| 阈值默认值 | `Config.withDefaults` | `extract/config.go` |

### 为什么开新包 `internal/p2verify` 而不是加白盒用例

白盒测试能直接读 `traj`、`toolResult` 等内部字段，而**内部字段"看起来对"不等于外部行为对**。本包刻意只站在宿主视角：

- 工具面与截断标记：断言打在 `LLMClient` 收到的 `User` 文本上——那是模型**唯一**的视野。工具返回 `Truncated=true` 但回注时丢掉标记，效果等于没做。
- 三区消歧：断言打在 `nodes`/`edges`/`aliases` 的 **SQL 行**上，而不是"判定标签写着 gray"。
- 写入路径：断言打在"内容能不能被词法通道搜到"上——绕过 Writer 的实现不报错，只表现为恒空。

### 为什么 D10 用"每轮不同查询"的对抗序列

仓库里既有的 `TestG1ForcedFinalExactlyMaxTurnsPlusOne` 每轮发**同一条** `lookup_entity`。实测其 gates 为 `[G3 同 query 重复, G4 无进展]`（MaxTurns≥5 时）——**收口者是 G4，不是 G1**。它在 MaxTurns=3 时恰好也是 4 次调用，因此"看起来"通过了，但把 MaxTurns 提到 5 后调用次数停在 4 而不再是 6。

所以本包每轮发**不同且都能命中**的查询（`vim` + 递增 `!`，利用 `lookup_entity` 的双向包含匹配），从而把 G3/G4 排除在外，并额外断言"其它四条门禁均未触发"来证明隔离。这是本包与实现者测试的主要差别。

---

## 2. 运行命令与真实结果

```bash
export PATH=/usr/local/go/bin:$PATH
cd /Users/taiwu.wang/Documents/leader_agent/gmem

CGO_ENABLED=0 go vet ./...      # 无输出
CGO_ENABLED=0 go build ./...    # BUILD_OK
CGO_ENABLED=0 go test ./...     # 9 包全绿（含 p2verify 35 个用例）
```

实测输出（`go test ./...`）：

```
ok  github.com/tim5wang/gmem/bigram             (cached)
ok  github.com/tim5wang/gmem/extract            (cached)
ok  github.com/tim5wang/gmem/fusion             (cached)
ok  github.com/tim5wang/gmem/internal/p1verify  (cached)
ok  github.com/tim5wang/gmem/internal/p2verify  2.462s
ok  github.com/tim5wang/gmem/internal/vtest     5.719s
ok  github.com/tim5wang/gmem/recall             (cached)
ok  github.com/tim5wang/gmem/sqlite             (cached)
ok  github.com/tim5wang/gmem/store              (cached)
```

缺陷项使用与 `internal/vtest/repro_findings_test.go` 相同的门禁约定（已知缺陷不得让 CI 永久变红）：

```bash
GMEM_VERIFY_BLOCKER=1  go test ./internal/p2verify/ -run TestBlocker   # 红 = 阻塞项仍在
GMEM_VERIFY_DEFERRED=1 go test ./internal/p2verify/ -run TestDeferred  # 红 = 可后续项仍在
```

未置位时，用例仍会**实测并打印**完整事实与根因，避免缺陷被静默遗忘；修复后自动转绿并提示摘除门禁。

---

## 3. 验收项逐条结论

| # | 验收项 | 结论 | 用例 |
|---|---|---|---|
| 1 | 独立测试 + 「永远返回 tool call」+ `总调用 == MaxTurns+1`；MaxTurns=1/5 各验 | ✅ **成立**。MaxTurns=1/2/3/5/8 → 调用 2/3/4/6/9，严格线性；每次只触发 G1 | `TestD10ExactlyMaxTurnsPlusOne`、`TestD10CallCountScalesWithMaxTurns` |
| 2 | G3：同 query 重复被拦、强制终稿、不死循环 | ✅ **成立**。字段顺序/空白/大小写不同的同一条查询被归一识别（`DuplicateCalls=2`），被拒的跳在回注里有"已被去重拒绝"。MaxTurns=100 + 永久重复查询 → ≤4 次调用收口 | `TestG3RepeatedIdenticalQueryIsRejected`、`TestG3G4BoundUnboundedModel` |
| 3 | G4：连续空结果被拦、强制终稿 | ✅ **成立**。每轮查不同但不存在的名字 → 恰好 3 次调用，`NoProgressRounds=2`，G1 未参与 | `TestG4ConsecutiveEmptyResultsForceFinal` |
| 4 | G5：超时降级而非挂死 | ⚠️ **部分成立**（见阻塞项 #1）。调用方给 deadline、以及强制终稿轮的 `FinalGrace` 兜底**均已验证有效**；但 `WallClock` 本身**不约束工具轮** | `TestG5CallerDeadlineDegrades`、`TestG5ForcedFinalGraceDegrades`、`TestG5WallClockBoundsBlockingToolRound` |
| 5 | 工具面只有只读；hops 传 2/99 被夹到 1 | ✅ **成立**。恰好 3 个工具，名字与参数 schema 均无写动词；`hops=2/99/1000000` 全部只拿到 depth=1（A→B→C 链上 B→C 被拦），并带截断标记 | `TestToolSurfaceIsThreeReadOnlyTools`、`TestHopsClampedToHardMaxOne` |
| 6 | 截断标记：lookup 15→10、neighbors 40→30、search 5 | ✅ **成立**（search 有一处限制，见可后续 #4）。lookup 返回 10 条 + `共 15 条` + 截断；neighbors 返回 30 条 + `共 40 条` + 截断 | `TestLookupEntityTruncatesAtTen`、`TestNeighborsTruncatesAtThirty`、`TestSearchMemoriesCapsAtFive` |
| 7 | 灰区不合并：s∈[θ_lo,θ_hi) 只多一条 possiblySame；反复写不重复入池 | ✅ **成立**。`nodes` 恰 2 行（候选未被改写 + 提及独立节点）、无 `extract:auto` 别名、语义关系 0 条；连跑 3 次 possiblySame 恒 1 条 | `TestGrayZoneNeverMerges`、`TestGrayZoneDedupOnRepeatedWrite` |
| 8 | negative_SameAs：裁决后不进灰区；不误伤真同一实体 | ✅ **判定层成立**（记账层见可后续 #2）。二次运行判为 `ZoneNegative`、`Reviewable=0`、不增殖；同名的另一节点仍能 `auto_merge`，被裁决的那对在 breakdown 里标 `Negative` | `TestNegativeSameAsStopsGrayZone`、`TestNegativeSameAsDoesNotPoisonOtherPairs` |
| 9 | θ_hi/θ_lo 可配；默认 0.92/0.72 且注明待校准 | ✅ **成立**。同一对（s=0.80）在"默认→灰区 / θ_hi=0.75→合并 / θ_lo=0.85→新建"三种配置下判定随之改变；默认值与"必须校准"注释均实测 | `TestThresholdsAreConfigurableNotHardcoded`、`TestDefaultThresholdsAreCalibrationPending`、`TestThetaLoAboveThetaHiFallsBackToNonEmpty` |
| 10 | NoopExtractor 降级可见 + L0/L1 仍产出 + 内容可词法搜到 | ✅ **成立**。`Degraded` 含 NoopExtractor 说明且 `SkippedL2=false`；decision 记忆与关系边均落地；`SQLite` 在词法通道命中 | `TestNoopExtractorDegradeVisibleAndL0L1StillWork` |
| 11 | 生产代码无 `Store.PutMemory`；词法失败整体回滚 | ✅ **成立**。`extract/` 全目录无 `Store.PutMemory(` / `.PutMemory(ctx` 调用；注入失败的 Lexical 后 `memories`/`memories_fts`/`nodes`/`aliases`/`edges` 全为 0 行 | `TestNoPutMemoryBypassInProductionCode`、`TestFailedLexicalRollsBackWholeWrite` |
| 12 | 不修改生产代码 / 不改 TD.md / 产物在仓库内 | ✅ 仅新增 `internal/p2verify/*.go`（6 文件 2355 行）；`git status` 只有该目录；scratch 在 `.godex/`（已 gitignore） | — |
| 13 | `go test ./...` 全绿 + 变异验证表 | ✅ 9 包全绿；**18/18 变异均变红，零漏网** | 见 §5 |

---

## 4. 发现清单

### 【阻塞合并】#1 — 自动合并 + 关系边导致整笔图写入回滚（FK 787）

**用例**：`TestBlockerAutoMergeEdgeFK`（`GMEM_VERIFY_BLOCKER=1` 下变红）

**现象**：零 LLM 的普通路径即可触发，`Pipeline.Run` 直接返回错误：

```
err = extract: 写入图元素/别名: writer: put edge "e-b9ca2168": 
      sqlite: put edge "e-b9ca2168" v1: constraint failed: FOREIGN KEY constraint failed (787)
消歧结论：mention="缓存服务" zone=auto_merge node="n-cache-svc" score=1.000
后果：nodes=3 edges=1 aliases=2（本次的节点/别名/边全部丢失）
```

**最小复现（不依赖 LLM）**：

1. 库里已有节点 `id="n-cache-svc"`, `canonical_name="缓存服务"`，并有一条出边（提供结构一致性证据）；
2. 别名词典里 `"缓存服务" → n-cache-svc`；
3. 跑一段含关系句的文本：`"Redis 依赖 缓存服务 做会话存储。"`；
4. 判定为 `auto_merge` → 写入时报 FK 787。

**根因**（`extract/pipeline.go` `writeEpisode`）：边端点由**表面形式现算**：

```go
addEdge(NodeID(ep.Scope, r.From), NodeID(ep.Scope, r.To), r.Relation, r.Confidence)
```

而 `ZoneAutoMerge` 分支**只写 alias、不建节点**（这本身是对的：合并到已有节点）。于是当候选节点 id ≠ `NodeID(scope, 表面形式)` 时，端点节点在库里不存在，`edges` 外键失败，整个 bundle 回滚。

**为什么这不是构造出来的怪例**：id 与表面形式脱钩有两条完全正常的生产路径——`Judge(Same=true)` 写 alias、以及自动合并时把 L2 给的 `aliases` 一并写成 alias。一旦某别名在词典里指向另一个 id，后续提到该别名 + 它的关系就是本形态。

**影响**：这段 episode 的实体与关系永久缺失，**且没有任何 Degraded 记录能解释原因**（只有裸 error）。

**最小修复方向**（不由本测试实施）：把"表面形式 → 实际节点 id"收敛成一次映射（auto_merge 记 `surface → d.NodeID`，gray/new 记 `surface → newID`），`addEdge` 用映射后的 id；端点缺失时跳过该边并记 `Warnings`，而不是让整笔写入失败。

---

### 【可后续】#2 — `Judge` 只沿一个方向作废 `possiblySame`

**用例**：`TestDeferredJudgeKeepsStalePending`（`GMEM_VERIFY_DEFERRED=1` 下变红）

`Judge` 只从 `KeepNodeID` 的**出边**查 `possiblySame`，而灰区边方向是「新提及 → 候选」。调用方按更自然的语义传 `Keep=候选节点` 时出边为空，陈旧边保持 `t_invalid IS NULL`（实测仍有效 1 条）。

不改变判定正确性（`negative_SameAs` 优先），但僵尸边计入 §4 灰区积压护栏；积压超 `GrayBacklogLimit` 会让 L2 静默暂停、抽取质量下降。
修复方向：两端各查一次——`pairHasPending` 里已有正确的双向实现可直接参考。`Same=true` 路径同理。

---

### 【可后续】#3 — 自洽 id 的候选永不自动合并

**用例**：`TestDeferredSelfDerivedCandidatesNeverAutoMerge`

`scoreMention` 的 `if c.node.ID == selfID { continue }` 排除过宽：当库里的节点**本来就是**由该表面形式创建时，候选池被清空 → 判 `new` → `makeNode` 重写同一个 id。

结果无害（幂等覆盖，不产生重复节点），但：① §4 的"自动合并 → 写 alias · confidence=high"在最常见的实体上**从不发生**；② 它掩盖了 #1 的触发面（FK 只出现在脱钩 id 上），修 #1 时必须同时想清这里的语义。

---

### 【可后续】#4 — `search_memories` 在 `k ≤ 5` 时无法报告截断

**用例**：`TestSearchMemoriesCapsAtFive`（子用例「k=5 时无法感知还有更多」）

`k > SearchLimit` 时夹紧并标记截断（✅）；但 `k ≤ 5` 时后端只返回 k 条，工具无从知道"还有更多"。实测种 8 条、返回 5 条、**回注里没有截断标记**。模型会把"只给 5 条"理解成"只有 5 条相关记忆"，从而漏掉已有上下文证据（表现为抽取质量下降，不报错）。
最小修法：内部按 `limit+1` 查询，多出 1 条即置 `Truncated`（不改公开契约）。

---

### 【可后续】#5 — `extract.WriteReport` 丢失 `LexicalIndexed`

**用例**：`TestDeferredWriteReportLacksLexicalFlag`

`recall.PutResult.LexicalIndexed` 恒为 true（那是 Writer 存在的理由），但折叠成 `extract.WriteReport` 时该字段被丢掉。行为层面索引确实同步了（可检索），但宿主无法在**写入点**就发现"索引没同步"，只能事后检索——那时已经晚了。
最小修法：给 `extract.WriteReport` 加 `LexicalIndexed bool`，从 `recall.PutResult` 透传。

---

### 【风险已实测，未判为缺陷】G5 不约束工具轮

见 `TestG5WallClockBoundsBlockingToolRound`（`GMEM_VERIFY_BLOCKER=1` 下变红）。

门禁只在**发请求之间**比较 `cfg.now().After(deadline)`，从未把 deadline 变成 ctx 的 deadline（`context.WithTimeout` 只用在强制终稿轮的 `FinalGrace`）。因此宿主 `LLMClient` 在工具轮阻塞时，Gmem 无法中断它——TD §3.2 对 G5 的"中断并降级"在工具轮上不成立。

实测：`WallClock=20ms`，模型阻塞，管线阻塞 > 600ms 直到测试看门狗解开。
为什么**不**列为阻塞：`LLMClient` 契约本意是单轮 primitive，宿主客户端通常自带超时；且调用方给 ctx deadline 时行为正确（已验）。但它确实使"Gmem 硬保证收敛"这句承诺在工具轮上打了折扣，**建议至少补一条 TD 风险说明**，或每轮 `context.WithDeadline(ctx, deadline)`（`DeadlineExceeded` 归到已有的 GateWallClock 降级分支即可）。

---

## 5. 变异验证表（18/18 变红，零漏网）

脚本：`.godex/tmp/mutate.py`（scratch，已 gitignore）。每个变异要求原文**恰好出现一次**，跑完立即 `git checkout --` 还原。

| # | 变异 | 文件 | 目标用例 | 结果 |
|---|---|---|---|---|
| M1 | `turns >= MaxTurns` → `turns > MaxTurns`（G1 off-by-one） | `llm.go` | `TestD10ExactlyMaxTurnsPlusOne` | 🔴 RED |
| M2 | G4 门禁关闭 `noProgress>=2` → `>=999` | `llm.go` | `TestG4ConsecutiveEmptyResultsForceFinal` | 🔴 RED |
| M3 | hops 夹紧失效 `if hops > HardMaxHops` → `if false && …` | `tools.go` | `TestHopsClampedToHardMaxOne` | 🔴 RED |
| M4 | `HardMaxHops = 1` → `2` | `tools.go` | `TestHopsClampedToHardMaxOne` | 🔴 RED |
| M5 | `LookupLimit = 10` → `20` | `config.go` | `TestLookupEntityTruncatesAtTen` | 🔴 RED |
| M6 | `NeighborsLimit = 30` → `50` | `config.go` | `TestNeighborsTruncatesAtThirty` | 🔴 RED |
| M7 | `SearchLimit = 5` → `9` | `config.go` | `TestSearchMemoriesCapsAtFive` | 🔴 RED |
| M8 | lookup 截断标记丢失（删 `res.Truncated = true`） | `tools.go` | `TestLookupEntityTruncatesAtTen` | 🔴 RED |
| M9 | 灰区去重失效 `if already` → `if false && already` | `pipeline.go` | `TestGrayZoneDedupOnRepeatedWrite` | 🔴 RED |
| M10 | `negative_SameAs` 被忽略 | `disambiguate.go` | `TestNegativeSameAsStopsGrayZone` | 🔴 RED |
| M11 | 默认 `ThetaHi 0.92` → `0.99` | `config.go` | `TestDefaultThresholdsAreCalibrationPending` | 🔴 RED |
| M12 | θ_hi 被写死（`if true` 覆盖配置） | `config.go` | `TestThresholdsAreConfigurableNotHardcoded` | 🔴 RED |
| M13 | G3 去重失效 | `llm.go` | `TestG3RepeatedIdenticalQueryIsRejected` | 🔴 RED |
| M14 | 工具面多插一个 `update_entity` 写工具 | `tools.go` | `TestToolSurfaceIsThreeReadOnlyTools` | 🔴 RED |
| M15 | NoopExtractor 不再上报 Degraded | `noop_fake.go` | `TestNoopExtractorDegradeVisibleAndL0L1StillWork` | 🔴 RED |
| M16 | Writer 不再同步 FTS 索引 | `recall/writer.go` | `TestWrittenContentIsLexicallySearchable` | 🔴 RED |
| M17 | 词法索引失败被吞掉（不回滚） | `recall/writer.go` | `TestFailedLexicalRollsBackWholeWrite` | 🔴 RED |
| M18 | 强制终稿轮不再套 `FinalGrace` | `llm.go` | `TestG5ForcedFinalGraceDegrades` | 🔴 RED |

还原后复跑 `go test ./...` → 9 包全绿，`git status` 只剩 `?? internal/p2verify/`。

---

## 6. 产物

```
internal/p2verify/helpers_test.go        348 行  测试替身 + 行级断言工具
internal/p2verify/gates_test.go          537 行  D10 收敛 / G1–G5
internal/p2verify/tools_test.go          335 行  工具面只读 / hops 夹紧 / 截断标记
internal/p2verify/disambiguate_test.go   493 行  三区消歧 / 灰区 / negative_SameAs / 阈值
internal/p2verify/writer_test.go         288 行  写入必经 Writer / 回滚 / 降级
internal/p2verify/findings_test.go       354 行  阻塞项与可后续项的可复现用例
```

**未提交**（按任务约束留给验收人决定是否入库）：`git status` 显示 `?? internal/p2verify/`。
如需入库：`git add internal/p2verify && git commit -m "test(p2): 独立测试验证包"`。

---

## 7. 真实结果边界

- 已实测：D10 调用次数、G3/G4/G5 各分支、工具面与截断、hops 夹紧、三区判定与阈值可配、灰区不合并与去重、`negative_SameAs` 语义、写入原子性与 FTS 可检索、NoopExtractor 与 L2 失败降级、18 项变异。全部跑在真实 SQLite 后端（内存库 + 文件库）。
- **未验证**：真实宿主 LLM 的行为（无需网络，`FakeLLM`/自定义 client 是刻意的确定性替身）；并发写入下的判定一致性；`AutoMerge` 在真实语料上的合并准确率（阈值 0.92/0.72 仍需真实数据校准，这是 TD §4 自己声明的证据边界）。
- 单测/构建通过**不等于**功能在真实 agent 中可用——本报告只覆盖库层可观测行为。

## 8. 回滚说明

本次只新增 `_test.go`，零生产代码改动，**无需回滚**。如需移除：

```bash
rm -rf internal/p2verify/
```

`docs/TD.md` 未被修改。`extract/*.go` 经 `git diff` 确认与 `e386d10` 完全一致。
