# P1 召回侧本体契约 — 设计说明与验证记录

对应 docs/TD.md §5（R1–R5）、§3（L0 词典规则）、§13（降级可见 / 证据边界）。
交付物：新包 `recall/`、SQLite 侧补 `store.OntologyReader` 实现（`sqlite/ontology.go`）、
`store` 包两处**纯新增**（`OntologyReader` 接口、关系常量）。

## 1. 代码入口

| 关注点 | 入口 | 文件 |
|---|---|---|
| Recall API 主体（align→plan→并行召回→RRF→加权→降权） | `recall.Recaller.Recall` | `recall/recall.go` |
| Concept Aligner | `recall.Aligner.Align` | `recall/align.go` |
| Recall Planner（选 lane / 定跳数） | `recall.Planner`（`makePlan`/`shape`/`relations`） | `recall/plan.go` |
| 声明式表（lane 权重 / 关系跳数 / 槽位权重 / 关系线索） | `recall.DefaultConfig` + `Config` | `recall/config.go` |
| 三路召回（并行） | `runLanes` / `lexicalLane` / `vectorLane` / `graphLane` | `recall/lanes.go` |
| 版本解析（as_of 双时态） | `Recaller.resolveMemories` | `recall/resolve.go` |
| **写入编排（D13 遗留必做项）** | `recall.Writer.Put` / `PutMemory` | `recall/writer.go` |
| 本体枚举（可选 Port） | `store.OntologyReader` → `sqlite.storeImpl` | `store/ports.go`、`sqlite/ontology.go` |

## 2. 关键设计理由

### 2.1 写入编排：为什么"读回版本"而不是"传入版本"

`Writer.Put` 在一个 `Store.Tx` 里按固定顺序执行：`PutMemory` → `GetMemory`（读回当前版本）
→ `Lexical.Index(version=读回值)` → 图元素。三个理由：

1. **版本号无法由调用方预知**：`PutMemory` 可能产生 `version+1`（内容变化）、也可能因
   幂等重写不产生新版本。写死 1 或传 0 都会让 per-version FTS 与实际版本错位——这正是
   D13 的缺陷形态。
2. **不破坏 P0 公开契约**：不新增"PutMemory 返回版本"之类的签名改动（那会破坏所有既有
   实现），而是用已有的 `GetMemory` 在同一事务内读回。后端内部如何分配版本仍是它自己的事。
3. **不可能只写一半**：两者在同一 Tx 内，任一环节报错整体回滚。用外层 ctx 调 Port 会
   立即得到 `ErrTxConflict`（P0 的既有语义），不会静默排队。

顺序（先主表、后索引）是刻意的：反转就必须先猜版本号。

### 2.2 跳数由关系表决定，不是全局常量

`Config.RelationRules` 是一张表：`subtype_of/part_of/depends_on=3`、`caused_by=2`、
`works_at/owns=1`（`uses` 额外给 1）。`Planner.relations` 只做三件事——查表、
把 `Transitive`/`MaxHops` 抄进 plan、按线索或形态决定**用哪几条**关系。
表外关系（如自定义 relation）按"非传递 1 跳"处理，**仍然写进 plan**，
因此不会出现"用了图却没有规则可查"。

这是与 Graphiti `bfs_max_depth=3` 的结构性区别：同一张图里 `subtype_of` 走 3 跳、
`works_at` 只走 1 跳，噪声源被白名单掐掉。

### 2.3 lane 权重来自表格，且带跳过原因

`Config.LaneRules` 是 TD §5「lane 选择规则表」的数据化版本（含 5 种形态）。
`Plan.Lanes` 每条都带 `Weight`（表值）、`Selected`/`Executed`、`Reason`（规则表来源）、
`SkipReason`（无 embedder / 无锚点 / 无 scope / 空 query）。权重没有任何一处写在 if/else 里。

### 2.4 insufficient 的判定：为什么要 `CoverageAuthoritative`

R4 的两个触发条件都实现了：`coverage < θ_cov`，或存在"必需且未对齐"的槽。

但直接拿覆盖率判定会踩两个坑，都做了显式处理：

- **本体为空时覆盖率没有语义**。query 里每个词都"不在空集合里"，覆盖率恒为低，
  会把"记忆写好了但本体还没长出来"的库变成永远 insufficient。此时置
  `Alignment.CoverageAuthoritative=false`，走 `Degraded` 上报（"coverage 不作 R4 判据"），
  不拿它当硬失败依据。
- **残槽不都是必需槽**。自然语言问句里大量残槽是限定语（"最新的"、"怎么配置"）。
  规则（`finalizeResidualRequired`）：只有"本体可对齐 且 query 的实质内容完全没对齐
  （或残槽是唯一内容槽）"时，残槽才判 `Required=true`。否则只由 coverage 削弱权重。
  这样"Alice 用什么 编辑器"（主体都对齐）不会被误判，而"部署环境是什么"（实体一个都
  没对上）会被硬判 insufficient。

`Missing[]` 每条都含 `Reason`（含槽位原文，可直接回显）与 `Suggestion`（可执行补救）。
insufficient 时 `Items` 为空——刻意的，不返回低分硬答。

### 2.5 Explain：Plan 恒非空且可断言

没有"关掉 explain"的开关。`Plan` 提供：`Matched`/`Unmatched`（对齐了什么、没对齐什么）、
`Shape`+`ShapeReason`（形态判定依据）、`Lanes`（选谁/权重/预算/理由/跳过原因）、
`Relations`（关系/传递性/跳数/来源 query-cue 或 shape-default）、`Hops`、
`Steps`（align/relations/lane:*/fuse/score:* 可复现步骤）、`Dropped`（被预算切断、
被 scope/kind 过滤、索引与主表不一致、节点无法映射到记忆）。

每条 `RecallItem` 的分数分解是**可复算**的：
`Score = FusionScore × ConceptBoost × RecencyFactor`，三个因子与 `Contributions`（每 lane 的
rank/weight/value）、`Adjustments`（concept_consistency / recency_decay 各带 Reason）、
`GraphEvidence`（anchor/edge/relation/depth/LinkedBy）全部保留。

### 2.6 降级可见（§13）

`Degraded` 覆盖：无本体枚举、本体为空、无 embedder（vector 缺席）、embedder 报错或返回空
向量、单 lane 执行失败、graph 节点映射不到记忆。任一存在即 `Status=degraded`（覆盖率达标但
结果不完整）。单 lane 失败**不让整次召回失败**，只有 ctx 取消才整体返回错误。

## 3. 破坏性变更

**无。** `store` 包只有纯新增（`OntologyReader` 接口、7 个 `Relation` 常量）；
`LexicalQuery` 未改动；所有 P0 Port 签名原样保留。
`git diff store/` 的删除行为 0（已验证）。
现有实现无需适配：`OntologyReader` 是可选扩展，未实现时 Aligner 降级并上报。

## 4. 测试与真实结果

命令（均在工作目录内执行）：

```
export PATH=/usr/local/go/bin:$PATH && go build ./... && go vet ./... && go test ./...
GOPROXY=off go build ./... && GOPROXY=off go vet ./... && GOPROXY=off go test ./...
go test -race ./recall/ ./sqlite/
```

结果：全部 `ok`（bigram / fusion / internal/vtest / recall / sqlite / store 六包全绿；
GOPROXY=off 下同样全绿；race 下 recall 与 sqlite 绿）。

关键用例：

| 硬要求 | 用例 |
|---|---|
| 写入入口后词汇通道必然可检索 | `TestWriterMakesLexicalSearchable`（含中文 bigram） |
| 索引版本 = 写入版本，旧版本索引保留 | `TestWriterIndexesActualVersionPerRewrite`（v1/v2 + as_of 双向断言） |
| 主表与索引同事务 | `TestWriterRollsBackBothSides` |
| InlineGraph 一次成图 | `TestWriterWithInlineGraph` |
| 关系型问句无 embedder 靠 graph 答出 | `TestRelationalQuestionAnsweredByGraphLane` |
| 缺维度 → insufficient + Missing | `TestInsufficientForMissingDimension`、`TestInsufficientWhenNoChannelHits` |
| Explain 可断言（lane/跳数/贡献/分解） | `TestPlanIsAssertable`、`TestPlanRecordsDroppedItems`、`TestKindFilterDroppedIsVisible` |
| 一致性加权 / 时效降权 | `TestPlanIsAssertable`（复算 Score）、`TestRecencyDecayApplied` |
| embedder 路径与降级 | `TestEmbedderEnablesVectorLane`、`TestBrokenEmbedderDegradesNotFails` |
| Aligner 无 embedder | `TestAlignerAliasAndConceptName`、`TestAlignerDefinitionSimilarity`、`TestAlignerStatusFactor` |
| 声明式表可替换 | `TestRelationHopsDeclarative`、`TestPlannerLaneRulesDeclarative` |

## 5. 变异验证（证明测试真能红）

每条都做了"改成缺陷形态 → 跑用例 → 确认 FAIL → 还原"。

| # | 变异 | 结果 | 证据 |
|---|---|---|---|
| A | `Writer.Put` 里删掉 `Lexical.Index` 调用（复现 D13 静默漏召回） | **RED** | `TestWriterMakesLexicalSearchable`: `走写入入口后词法通道查不到（静默漏召回缺陷）：hits=0` |
| B | `Document.Version` 写死 `1` 而非 `cur.Version` | **RED** | `TestWriterIndexesActualVersionPerRewrite`: `as_of=1500 查 session: hits=[], want 单条 v1`（v2 覆盖了 v1） |
| C | 不把跳过的 lane 记入 `Degraded`（静默降级） | **RED** | `TestRelationalQuestionAnsweredByGraphLane`: `Degraded=[]，必须显式报告 vector lane 因无 embedder 而缺席` |
| D | `Planner.relations` 改成全局常量 3 跳（Graphiti 式盲 BFS） | **RED** | `TestPlannerShapeAndSkip`: `Hops=3, want 1（uses 白名单 1 跳）`；`TestPlanIsAssertable` 同断言 |

变异 D 尤其说明"跳数不能是全局常量"这条设计被测试真正钉住了：
一旦退回全局 3 跳，两个独立用例同时红。

## 6. 已知限制

1. **所有阈值与权重都是待校准起点**（代码注释里逐条标了 `⚠️ 待校准`）：
   `ThetaCoverage=0.6`、lane 权重表、`DefinitionMinSim=0.5`、`ConceptBoost=0.5`、
   `RecencyHalfLife=30d`、`RecencyFloor=0.3`、关系跳数表。TD §13 明确这些尚无评测集支撑。
2. **对齐是词法的，没有语义泛化**。定义句相似度用 bigram 包含度，不做同义/跨语言扩展。
   这是 P1 的边界（不引入 embedder/LLM），也是 §13「中文分词语义弱」风险的直接体现。
3. **`graph lane` 依赖"节点 → 记忆"的可映射性**（`concept_id` 或 `source_episode`）。
   两者都为空的节点无法映射，会被记进 `Plan.Dropped` 而不是静默丢弃。P2 抽取应保证
   节点带 `source_episode`。
4. **as_of 回溯用 `ListMemories(AsOf)` 全量解析**再按 ID 索引：单条精确查询需要新的
   Store 契约方法（会破坏 P0 公开契约，本阶段不做）。scope 内记忆量大时这是 O(n)，
   属于已知代价，迁移到 P2/P3 时再评估。
5. **并行 lane 共享同一 SQLite 单连接**（P0 `SetMaxOpenConns(1)`）。lane 之间是读操作，
   实际表现为串行排队；`-race` 下无数据竞争。真正并行需要读写分离连接池，属 P4+ 议题。
6. `Relation` 白名单外的自定义关系走"非传递 1 跳"兜底，且 `Origin` 标为
   `shape-default`——这是保守默认，不是校准结果。

## 7. 回滚

单一提交，`git revert <sha>` 即可：`recall/` 为新增包，`sqlite/ontology.go` 为新增文件，
`store` 两处纯新增不影响既有调用方。回滚后 P0 行为完全不变。
