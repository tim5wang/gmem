# P1 独立测试验证报告

- 被验对象：`47cf9bf` feat(p1): 召回侧本体契约（HEAD == origin/master，0/0）
- 验证包：`internal/p1verify/`（**只新增 _test.go**，零复用 `recall/*_test.go`，未改生产代码与 docs/TD.md）
- 环境：Go 1.23.9（`/usr/local/go/bin`），`CGO_ENABLED=0`
- 结论：**3 个阻塞合并 + 3 个可后续**。写入编排原子性、D13 版本号、跳数白名单、降级可见、scope/kind 隔离这五块**实测成立**；缺陷全部集中在 `insufficient` 判定与 Explain 保真度上。

## 1. 代码入口与设计理由

| 关注点 | 入口 | 文件 |
|---|---|---|
| 召回主流程 | `recall.Recaller.Recall` | `recall/recall.go` |
| 对齐 | `recall.Aligner.Align` | `recall/align.go` |
| 规划（lane/跳数） | `Planner.makePlan`/`shape`/`relations` | `recall/plan.go` |
| 三路召回 | `runLanes`/`graphLane`/`mapHopsToMemories` | `recall/lanes.go` |
| 声明式表 | `Config`/`DefaultConfig` | `recall/config.go` |
| 写入编排 | `Writer.Put` | `recall/writer.go` |
| 版本解析 | `Recaller.resolveMemories` | `recall/resolve.go` |
| SQLite 端口 | `storeImpl`/`lexicalIndex`/`graphStore` | `sqlite/*.go` |

### 为什么用"双向口径"（缺陷钉桩）

三个阻塞项在 `47cf9bf` 上客观存在。若直接断言正确行为，`go test ./...` 会永久变红、掩盖后续新回归（P0 踩过这个坑，见 `internal/vtest/repro_findings_test.go` 的历史注释）。因此每个缺陷用例有两个口径：

```
go test ./internal/p1verify/                         # 默认：断言缺陷现状 → 全绿（可复现）
GMEM_P1_EXPECT_FIXED=1 go test ./internal/p1verify/  # 验收：断言正确行为 → 当前红
```

修复者一旦改好实现，**默认口径会变红并指名要翻转断言**——不允许缺陷被悄悄修掉而用例继续绿着。这是刻意的强制收敛点。

## 2. 运行命令与真实结果

```
export PATH=/usr/local/go/bin:$PATH
CGO_ENABLED=0 go vet ./...                 # 无输出
CGO_ENABLED=0 go test ./...                # 七包全绿（含 p1verify，30 个用例）
CGO_ENABLED=0 go test -race ./internal/p1verify/ ./recall/   # 两包 ok
GOPROXY=off CGO_ENABLED=0 go test ./...    # 全绿（零网络）
GMEM_P1_EXPECT_FIXED=1 CGO_ENABLED=0 go test ./internal/p1verify/  # 6 红 = 6 个缺陷均存在
```

`GMEM_P1_EXPECT_FIXED=1` 的 6 条红，正是下面 6 个缺陷的机器可读凭证。

## 3. 阻塞合并（3 项，均附最小复现用例）

### B1 关系型计划里没有 lexical 通道 → 词法能答却答不出

- 用例：`TestBlockerRelationalPlanOmitsLexical`（`insufficient_test.go`）
- 位置：`recall/config.go` 的 `DefaultLaneRules()`，`ShapeRelational` 行 = `[graph 0.7, vector 0.3]`，**无 lexical**
- 最小复现：库里 1 条记忆 `"认证服务从 session 换成 JWT"`；查询 `"为什么换成 JWT"`；不注入 embedder。
- 实测：`coverage=0.667 ≥ θ=0.6`、`RequiredMissing=0`（**不是覆盖率不足**）；graph 因无锚点跳过、vector 因无 embedder 跳过 → `Plan.Steps` 写 `"RRF(k=60) 融合 0 路通道（）"` → `status=insufficient`、`items=0`。
- 对照证据：同一 query 直接 `d.Lexical().Search(...)` **有 1 条命中**（用例内断言）。
- 影响：TD §5 的 relational 行只给了 graph/vector 权重，实现把"未列出"当成"不参与"。宿主看到的是 `insufficient` + Missing 说"库里没有这个维度"，用户会去补数据，而真相是计划里根本没跑能答的通道。
- 建议修法（一处）：`ShapeRelational` 行加 `{Lane: LaneLexical, Weight: 0.2}`，或把"至少一路必执行"做成不变量（选中的 lane 全跳过时降级到 lexical）。

### B2 概念名遮挡关系线索词 → 同义问法从 1 跳退化为 3 跳盲扩展

- 用例：`TestBlockerMaskedCueLosesRelation`（`insufficient_test.go`）
- 位置：`recall/align.go` 的 `Align`——先按"最长优先"接受 aliases/concepts.name，再处理关系线索词，且线索候选**只要与已接受槽位有任何重叠就整条丢弃**（`if overlaps(blocked, ...) { continue }`）
- 最小复现：概念「公司」已入库；记忆内容设为 `"Acme 雇佣"`（与两个问句都无 bigram 重合，词法打不中，graph 是唯一通道）。
  - `"Alice 在哪个公司工作"` → 「公司」吃掉概念槽 → 线索 `"哪个公司"` 被丢 → `RelationHints` 空 → `Shape=full_with_anchor` → shape-default 白名单 `[part_of, subtype_of, depends_on]`（**不含 works_at**）→ `Hops=3` → `items=[]`、`insufficient`
  - `"Alice 在哪工作"` → 线索 `"在哪工作"` 不重叠 → `works_at` 1 跳 → 答出 `m-acme`
- 影响：两个语义相同的问法，一个答得出一个答不出。这也直接削弱了"跳数按 relation 决定"的收益——自然措辞下线索识别不出来，白名单就退化成 3 跳盲扩展（正是 TD §5 反对的 Graphiti 形态）。注意**白名单本身是对的**（见 §4 的 hops 用例），缺陷在"线索被遮挡后丢弃而非降级"。
- 建议修法：线索候选与概念槽重叠时，不要整条丢弃——把关系**附加**到已接受槽位上（或允许重叠匹配但只取 relation hint），因为"公司"同时是概念名和关系线索的一部分是正常语言现象。

### B3 Writer 只带 Edges 且 Graph=nil 时 nil 解引用 panic

- 用例：`TestBlockerWriterNilGraphPanicsOnEdges`（`writer_test.go`）
- 位置：`recall/writer.go` 前置检查 `if len(b.Nodes) > 0 && w.ports.Graph == nil` —— **漏了 Edges**
- 最小复现：`NewWriter(store.Ports{Store, Lexical})`（Graph 为 nil）+ `Put(WriteBundle{Memory, Edges: []store.Edge{{...}}})`（Nodes 为空）
- 实测：`panic: runtime error: invalid memory address or nil pointer dereference`（在 `w.ports.Graph.PutEdge` 处）
- 影响：库代码 panic 会穿透宿主进程（CLI / MCP server）。而"节点早已存在、只补一条边"是最常见的增量写法，文档也鼓励（InlineGraph）。对比：带 Nodes 的情形有检查、返回可读错误，行为不一致。
- 建议修法（一行）：`if (len(b.Nodes) > 0 || len(b.Edges) > 0) && w.ports.Graph == nil`。

## 4. 可后续（3 项）

### F1 `insufficient` 短路时 plan 仍声称 lane 已执行（Explain 保真度）

- 用例：`TestFollowUpInsufficientClaimsLaneExecuted`
- 位置：`recall/recall.go` 的 R4 判定直接 `return`（早于 `runLanes`），但 `LanePlan.Executed` 在 `plan.go` 末尾已置位
- 实测：`status=insufficient` 时 `Plan.Lanes[lexical].Executed=true`，而 `Plan.Steps` 里没有 `fuse`、也没有任何命中数。独立 `Lexical.Search` 对同一 query 有 1 条命中。
- 影响：读 plan 的人会得出"词法跑过、什么都没找到"，实际一次通道调用都没发生。这正是 B1 能被长期掩盖的原因——降级与短路共享同一套"看起来执行过"的字段。
- 建议：短路时不要把未执行的 lane 标成 `Executed`；或在短路前做一次轻量探测，把"词法本可命中 N 条"写进 plan。

### F2 节点→记忆按 concept 扇出，一抢全中且证据不可区分

- 用例：`TestFollowUpConceptFanoutIsImprecise`
- 位置：`recall/lanes.go` 的 `mapHopsToMemories`——按 `node.ConceptID` 调 `ListMemories(ConceptID: ...)`
- 实测：图上命中 1 个节点（1 跳边），同 concept 下 5 条内容无关的记忆**全部**被召回，且每条都挂同一条边证据（`LinkedBy: concept_id:c-company`）
- 影响：精确率问题，且 `GraphEvidence` 无法回答"这一条凭什么"。属 P2 抽取质量议题（节点应尽量一一对应，或记忆带节点级指针）。P1 作为已知限制记录。

### F3 任一锚点失败即丢弃整个 graph lane 的成功结果（全有或全无）

- 用例：`TestFollowUpOneAnchorFailureDiscardsWholeGraphLane`
- 位置：`recall/lanes.go` 的 `graphLane`：`if len(degraded) > 0 { return nil, nil, dropped, fmt.Errorf(...) }`
- 实测：两锚点问句，只有 `n-bob` 的 `Neighbors` 失败 → `n-alice` 已算出的 `m-vim` **一起被丢**，最终 `items=[]`
- 影响：多锚点问句里一个实体查询失败，整条图通道归零。若该形态没有别的通道（B1 让 relational 只剩 graph+vector，而 vector 无 embedder 又缺席），用户看到的就是"库里没有"。降级本应是"部分可用"（`lanes.go` 注释也这么写）。
- 建议：失败的锚点记 `Degraded`，但保留成功锚点的候选。

## 5. 变异表（19 项，全部 RED）

脚本：`.godex/tmp/mutate.py`（正则锚点，逐条 `git checkout` 还原；跑完 `go test ./...` PASS 且 `git diff` 为空）。

| # | 变异 | 命中用例 | 结果 |
|---|---|---|---|
| M1 | `ShapeRelational` 的 lane 表删掉 graph | `TestGraphLaneAnswersRelationalWithoutEmbedder` | RED |
| M2 | `works_at` MaxHops 1→3（盲 BFS） | `TestHopWhitelistWorksAtIsOneHop` | RED |
| M3 | `subtype_of` MaxHops 3→1 | `TestHopWhitelistSubtypeOfIsThreeHops` | RED |
| M4 | `owns` MaxHops 1→3 | `TestHopWhitelistPartOfIsThreeHopsOwnsIsOne` | RED |
| M5 | `Document.Version` 写死 1（D13 缺陷形态） | `TestWriterPassesRealVersionToFTS` | RED |
| M6 | 整段删掉 `Lexical.Index`（静默漏召回） | `TestWriterPassesRealVersionToFTS` | RED |
| M7 | `Store.Tx` 不走事务（主表/索引各自提交） | `TestWriterAtomicWhenGraphNodeFails` | RED |
| M8 | 跳过的 lane 不记入 `Degraded`（静默降级） | `TestVectorLaneAbsentWithoutEmbedder` | RED |
| M9 | embedder 报错 → 整体失败（不降级） | `TestVectorLaneRecordsEmbedderError` | RED |
| M10 | kind 过滤失效 | `TestKindFilterVisibleInDropped` | RED |
| M11 | scope 隔离失效（后端 + 召回侧同时破坏） | `TestScopeIsolation` | RED |
| M12 | R4 结构化 Missing 被抹掉 | `TestInsufficientForUnknownDimension` | RED |
| M13 | 所有关系强制 3 跳 | `TestHopWhitelistWorksAtIsOneHop` | RED |
| M14 | `Adjustments` 丢掉 `concept_consistency` | `TestExplainIsReproducible` | RED |
| M15 | 分数分解文本格式漂移 | `TestExplainIsReproducible` | RED |
| M16 | lane 权重写死 1.0（不查表） | `TestExplainIsReproducible` | RED |
| M17 | RRF 通道名统一为 `ch`（贡献丢失来源） | `TestGraphLaneAnswersRelationalWithoutEmbedder` | RED |
| M18/M19 | graph lane 只遍历第一个锚点 | `TestGraphLaneTraversesAllAnchors` | RED |

**过程中的一处自我修正（值得记录）**：M18 最初对 `TestBlockerMaskedCueLosesRelation` 保持 GREEN——因为该用例只有单个锚点，对"只取第一个锚点"这类变异天然不敏感。这是**我的测试覆盖缺口，不是实现没问题**。因此新增 `TestGraphLaneTraversesAllAnchors`（双锚点、1:1 概念映射）把该变异钉红。

## 6. 实测成立的部分（本卡要求验证、结论为"通过"）

| 攻击面 | 用例 | 实测结论 |
|---|---|---|
| 无 embedder 关系型问句靠 graph 答出 | `TestGraphLaneAnswersRelationalWithoutEmbedder` | graph `Executed=true`；top1 带 `e-alice-vim/uses/depth=1` 证据；vector 缺席显式进 `Degraded`，`status=degraded` |
| insufficient 正向 | `TestInsufficientForUnknownDimension` | `status=insufficient`、`items=0`、Missing 覆盖「部署环境」、每项 Reason 含槽位文本 + Suggestion 非空、plan 有 `insufficient`/`missing` 步骤 |
| insufficient 反向（不误报） | `TestNoFalseInsufficientForLiteralHit`、`TestNoFalseInsufficientForPartialMatch` | 词法有命中时不误报；本体为空时不误判（`CoverageAuthoritative` 机制有效） |
| Explain 可复现 | `TestExplainIsReproducible`、`TestExplainDroppedIsReproducible` | lane 集合/权重==声明式表；`MaxHops`/`Transitive` 双向对齐 TD §5 表；`LaneHit.Value == weight/(K+rank)` 且 `Σ==FusionScore`；`fusion×concept×recency==Score`；plan 文本四个数可解析并与结构体逐项相等；Dropped 数量守恒且不与已返回条目重叠 |
| 跳数白名单 | `TestHopWhitelist*`（4 条） | `works_at`/`owns` 实测 1 跳、两跳外节点既不在结果也不在 Dropped；`subtype_of` 3 跳且 depth=1/2/3 如实、4 跳不出现；表外关系按"非传递 1 跳"兜底且仍写进 plan |
| 写入原子性（双向） | `TestWriterAtomic*`（4 条） | Lexical 失败→主表/FTS 均无行（独立连接核 `SELECT COUNT`）；**先写后报错**同样回滚；Graph 第 2 节点失败→第 1 节点也回滚 |
| 真实版本号（D13） | `TestWriterPassesRealVersionToFTS`、`TestWriterNeverIndexesVersionZero` | FTS 行级 `(id,version) = {1:v1内容, 2:v2内容}`；等价重写仍 v2；as_of 1500→v1、2500→v2，且 v2 内容不泄漏到过去；无 version=0 行 |
| 降级两条路径 | `TestVectorLaneAbsentWithoutEmbedder`、`TestVectorLaneRecordsEmbedderError`、`TestVectorLaneEmptyEmbeddingDegrades`、`TestForcedVectorWithoutEmbedderIsVisible` | nil→三处显式（Executed/SkipReason/Degraded）；错误→原错误文本进 Degraded、整体不失败；空向量也上报 |
| scope/kind 隔离 | `TestScopeIsolation`、`TestKindFilterVisibleInDropped` | 三 scope 互不泄漏（双向）；kind 过滤生效且 `Plan.Dropped` 明确写 "kind" |

## 7. 产物与提交

- 新增（未提交，交人验收）：`internal/p1verify/`
  - `helpers_test.go` 开库/种子/断言/failLexical·failGraphNode·failNeighborsFor 故障注入
  - `pin_test.go` 双向口径机制
  - `graph_lane_test.go`、`graph_anchor_test.go` 无 embedder 图通道 + 多锚点
  - `insufficient_test.go` 双向 insufficient + B1/B2/F1
  - `explain_test.go` Explain 逐项复算
  - `hops_test.go` 跳数白名单 4 条 + F2
  - `writer_test.go` 原子性/版本 + B3
  - `degrade_isolation_test.go` 降级/隔离
- 脚本（scratch，`.gitignore` 已忽略）：`.godex/tmp/mutate.py`、`.godex/tmp/mutation-results.json`
- 生产代码与 `docs/TD.md`：**零改动**（`git diff` 为空）

## 8. 已知限制

1. `CoverageAuthoritative` 的判据是"concepts + aliases 是否全空"。本体**非空但与 query 无关**时会走权威分支，此时逐字命中仍被判 insufficient（`"认证"` 对含"认证服务"的记忆即如此）。这是 B1/F1 的同源根因之一：R4 只看对齐覆盖率，不看通道是否真能命中。本报告将其归入 B1 修复时的连带考量，未单独立为阻塞项（TD §5 R4 的公式本身只定义 coverage）。
2. 未验证真实 LLM/embedder 语义质量（P1 不引入）；`internal/vtest` 已覆盖"零网络"。
3. 评测集校准（θ_cov=0.6、lane 权重、跳数表数值）不在本卡范围：只验证了"值来自表且表与 plan 一致"，未验证取值优劣。
4. as_of 回溯用 `ListMemories` 全量解析（O(n)），作者已在 P1-NOTES 记录，本卡未重复验证性能。

## 9. 回滚

新增包为纯测试代码，不改生产行为：

```
git checkout -- .            # 放弃工作区改动（若有）
rm -rf internal/p1verify     # 删除本卡交付物
```

删除后 `go test ./...` 与 `47cf9bf` 行为完全一致（本次验证全程未改生产代码，已在每次变异后 `git checkout` 还原并全量复测）。
