// Recall Inspector 前端单测（node --test，无浏览器、无网络）。
//
// 覆盖验收点名的四条**不能靠肉眼**的结论：
//   1) score 三因子乘算一致性（且三因子来自后端透传，不是前端自算）
//   2) as_of 横幅与"回到现在"清除
//   3) degraded 全部条目渲染（不得只 console.log）
//   4) q 为空时是空态（不是空白、不是骨架残留）
//
// 另外补 URL 参数解析（非法 as_of / k 超限必须显式报错）、plan / lane / dropped
// 区块的条数与响应一致、路由判定。
import test from 'node:test';
import assert from 'node:assert/strict';

import { installDom, textOf, findAll } from './dom-stub.mjs';

installDom();

const {
  scoreBreakdown,
  planSummary,
  laneRows,
  droppedRows,
  degradedLines,
  asOfBanner,
  noCapabilityReason,
  renderRecallPageView,
  renderDetail,
  renderLaneSection,
  renderDroppedSection,
  SCORE_EPS,
} = await import('../js/recallpage.js');

const {
  parseRecallView,
  toRecallQuery,
  toRecallURL,
  clearAsOf,
  setAsOf,
  selectItem,
  toggleList,
  withQuery,
  K_MAX,
  K_DEFAULT,
} = await import('../js/recallstate.js');

const { matchRoute, isImplemented, notImplementedMessage, normalizePath } = await import('../js/router.js');

// ---------------------------------------------------------------------------
// fixture：一份结构完整、且三因子**确实相乘等于 score** 的响应
// ---------------------------------------------------------------------------

// 三个因子刻意选成非平凡值（fusion 0.032786、boost 1.5、recency 0.5），
// 这样"漏掉任何一个因子"都会让乘积与 score 差得很远 —— 变异会被抓住。
export const FUSION = 0.03278688524590164;
export const BOOST = 1.5;
export const RECENCY = 0.5;
export const SCORE = FUSION * BOOST * RECENCY;

export function makeResponse(overrides = {}) {
  return {
    meta: {
      mode: 'recall',
      query: 'acme',
      scope: 'user',
      kinds: [],
      lanes: [],
      k: 2,
      explain: true,
      k_max: K_MAX,
      theta_coverage: 0.6,
      capabilities: {
        recall_plan: true, audit: true, health: false, ontology: true,
        reasons: ['health: 尚无 store.HealthReader 可选端口'],
      },
    },
    status: 'ok',
    query: 'acme',
    scope: 'user',
    coverage: 0.95,
    theta_coverage: 0.6,
    items: [
      {
        id: 'm-1',
        version: 1,
        scope: 'user',
        concept_id: 'c-1',
        kind: 'semantic',
        content: 'acme 的默认编辑器是 vim',
        t_valid: 1000,
        t_invalid: 0,
        t_created: 1000,
        t_expired: 0,
        importance: 0,
        recall_count: 0,
        score: SCORE,
        fusion_score: FUSION,
        concept_boost: BOOST,
        recency_factor: RECENCY,
        concept_consistent: true,
        contributions: [
          { lane: 'lexical', rank: 1, weight: 1, value: 0.9 },
          { lane: 'graph', rank: 1, weight: 0.8, value: 0.7 },
        ],
        adjustments: [
          { kind: 'concept_consistency', factor: BOOST, reason: 'concept_id 命中对齐概念集合' },
          { kind: 'recency_decay', factor: RECENCY, reason: 'age=720h' },
        ],
        graph_evidence: [
          {
            anchor: 'n-acme', edge_id: 'e-1', from: 'n-acme', to: 'n-vim',
            relation: 'uses', depth: 1, linked_by: 'concept_id',
          },
        ],
      },
      {
        id: 'm-2',
        version: 1,
        scope: 'user',
        kind: 'episodic',
        content: 'acme 提过部署偏好',
        t_valid: 1000, t_invalid: 0, t_created: 1000, t_expired: 0,
        importance: 0, recall_count: 0,
        score: 0.01, fusion_score: 0.02, concept_boost: 1, recency_factor: 0.5,
        concept_consistent: false,
        contributions: [{ lane: 'lexical', rank: 2, weight: 1, value: 0.5 }],
        adjustments: [
          { kind: 'concept_consistency', factor: 1, reason: '未命中' },
          { kind: 'recency_decay', factor: 0.5, reason: 'age=720h' },
        ],
        graph_evidence: [],
      },
    ],
    missing: [],
    dropped: [
      { id: 'm-3', reason: '最终预算切断（final budget=2，调整后排名 2 之后）', score: 0.004 },
      { id: 'm-4', reason: '被 scope/kind 过滤拦截', score: 0.002 },
    ],
    degraded: ['vector lane skipped: no embedder', 'alias 词典为空'],
    plan: {
      query: 'acme',
      shape: 'full_with_anchor',
      shape_reason: 'coverage=0.951 ≥ θ，且有 1 个实体锚点',
      coverage: 0.95,
      theta_coverage: 0.6,
      matched: [
        { slot_index: 0, kind: 'entity', text: 'acme', node_id: 'n-acme', method: 'alias', quality: 1, status_factor: 1, weight: 1 },
      ],
      unmatched: [
        { kind: 'relation', text: '用什么', required: false, weight: 0.5 },
        { kind: 'concept', text: '部署环境', required: true, weight: 1 },
      ],
      anchors: ['n-acme'],
      lanes: [
        { lane: 'lexical', weight: 1, budget: 20, selected: true, executed: true, reason: 'shape=full_with_anchor 默认含词法通道' },
        { lane: 'vector', weight: 0.9, budget: 20, selected: true, executed: false, reason: '', skip_reason: 'no embedder' },
        { lane: 'graph', weight: 0.8, budget: 10, selected: true, executed: true, reason: '有实体锚点 → 定向扩展' },
      ],
      relations: [
        { relation: 'uses', transitive: false, max_hops: 1, weight: 1, origin: 'query-cue' },
        { relation: 'depends_on', transitive: true, max_hops: 3, weight: 0.8, origin: 'shape-default' },
      ],
      hops: 1,
      steps: [
        { step: 'align', detail: '对齐 1/2 槽位' },
        { step: 'fuse', detail: 'RRF(k=60) 融合 2 路通道' },
      ],
      dropped: [
        { id: 'm-3', reason: '最终预算切断（final budget=2，调整后排名 2 之后）', score: 0.004 },
        { id: 'm-4', reason: '被 scope/kind 过滤拦截', score: 0.002 },
      ],
    },
    ...overrides,
  };
}

const view = { q: 'acme', scope: '', kinds: [], lanes: [], asOf: null, k: 2, item: '' };

// ---------------------------------------------------------------------------
// 1. 三因子乘算一致性（硬证据）
// ---------------------------------------------------------------------------

test('scoreBreakdown：三因子相乘等于 score（误差 ≤ 0.001）', () => {
  const resp = makeResponse();
  for (const it of resp.items) {
    const b = scoreBreakdown(it);
    assert.equal(b.consistent, true, `${it.id}: product=${b.product} score=${it.score}`);
    assert.ok(Math.abs(b.product - it.score) <= SCORE_EPS);
    // 因子必须是后端字段的原值（不是反推出来的）
    assert.equal(b.fusion, it.fusion_score);
    assert.equal(b.boost, it.concept_boost);
    assert.equal(b.recency, it.recency_factor);
    assert.equal(b.score, it.score);
  }
});

test('scoreBreakdown：漏掉 recency 的分数必须被判为不一致（变异验证）', () => {
  const resp = makeResponse();
  const tampered = { ...resp.items[0], score: FUSION * BOOST }; // 忘了乘 recency
  const b = scoreBreakdown(tampered);
  assert.equal(b.consistent, false, '少乘一个因子却仍被判一致 → 一致性检查是假的');
  assert.ok(b.delta > SCORE_EPS);
});

test('scoreBreakdown：boost 被改成 1 也必须不一致（变异验证）', () => {
  const resp = makeResponse();
  const tampered = { ...resp.items[0], concept_boost: 1 };
  const b = scoreBreakdown(tampered);
  assert.equal(b.consistent, false);
});

test('详情面板显示的 score 与三因子乘积都与后端一致（DOM 层）', () => {
  const resp = makeResponse();
  const detail = renderDetail(resp, 'm-1');
  const text = textOf(detail);
  // 公式里三个因子都要出现（各三位小数），否则读者无法手算
  assert.ok(text.includes(FUSION.toFixed(3)), `公式缺 fusion：${text}`);
  assert.ok(text.includes(BOOST.toFixed(3)), `公式缺 boost：${text}`);
  assert.ok(text.includes(RECENCY.toFixed(3)), `公式缺 recency：${text}`);
  assert.ok(text.includes(SCORE.toFixed(3)), `缺最终 score：${text}`);
  // 一致性标记必须是 ok（不是 bad）
  const check = findAll(detail, (n) => n.getAttribute('data-consistent') !== null)[0];
  assert.ok(check, '详情里没有一致性标记节点');
  assert.equal(check.getAttribute('data-consistent'), 'true');
});

// ---------------------------------------------------------------------------
// 2. plan 区块：四个具体值 + unmatched 条数与响应一致
// ---------------------------------------------------------------------------

test('plan 区块显示 shape / shape_reason / coverage / theta_coverage 与 unmatched 条数', () => {
  const resp = makeResponse();
  const root = renderRecallPageView({ view, status: 'ready', resp });
  const text = textOf(root);
  assert.ok(text.includes('full_with_anchor'), 'shape 未显示');
  assert.ok(text.includes('coverage=0.951'), 'shape_reason 未显示');
  assert.ok(text.includes('0.950'), 'coverage 数值未显示');
  assert.ok(text.includes('0.600'), 'theta_coverage 数值未显示');

  // unmatched 行数必须等于响应的 plan.unmatched 长度
  const table = findAll(root, (n) => n.getAttribute('data-table') === 'unmatched')[0];
  assert.ok(table, '没有 unmatched 表');
  const rows = findAll(table, (n) => n.tagName === 'tr');
  // 1 行表头 + N 行数据
  assert.equal(rows.length, 1 + resp.plan.unmatched.length);
});

test('planSummary 在缺 plan 时退回顶层字段并显式标记', () => {
  const resp = makeResponse({ plan: undefined });
  const p = planSummary(resp);
  assert.equal(p.fromPlan, false);
  assert.equal(p.coverage, resp.coverage);
  assert.equal(p.thetaCoverage, resp.theta_coverage);
});

// ---------------------------------------------------------------------------
// 3. lane 区块：每行都有 weight/budget/selected/executed，且 LaneHit 聚合正确
// ---------------------------------------------------------------------------

test('laneRows：每条 lane 一行，LaneHit 来自 items[].contributions', () => {
  const resp = makeResponse();
  const rows = laneRows(resp);
  assert.equal(rows.length, resp.plan.lanes.length);
  const lexical = rows.find((r) => r.lane === 'lexical');
  const vector = rows.find((r) => r.lane === 'vector');
  assert.ok(lexical && vector);
  assert.equal(lexical.executed, true);
  assert.equal(vector.executed, false);
  assert.equal(vector.skipReason, 'no embedder');
  // lexical 在两条 item 上都有贡献 → 2 条 LaneHit
  assert.equal(lexical.hits.length, 2);
  assert.equal(vector.hits.length, 0);
  // rank 升序
  assert.deepEqual(lexical.hits.map((x) => x.rank), [1, 2]);
});

test('lane 区块的展开行初始隐藏，且包含 rank/weight/value 表头', () => {
  const resp = makeResponse();
  const section = renderLaneSection(resp);
  const details = findAll(section, (n) => n.getAttribute('data-lane-hits') !== null);
  assert.equal(details.length, resp.plan.lanes.length);
  for (const d of details) assert.equal(d.hidden, true, 'LaneHit 展开行必须默认隐藏');
  const text = textOf(section);
  for (const label of ['rank', 'weight', 'value']) assert.ok(text.includes(label), `缺表头 ${label}`);
});

test('lane 区块每行显示 selected / executed 的布尔文本（不是空白）', () => {
  const resp = makeResponse();
  const section = renderLaneSection(resp);
  const rows = findAll(section, (n) => n.getAttribute('data-lane') !== null);
  assert.equal(rows.length, resp.plan.lanes.length);
  for (const r of rows) {
    const t = textOf(r);
    assert.ok(t.includes('是') || t.includes('否'), `lane 行没有布尔文本：${t}`);
  }
});

// ---------------------------------------------------------------------------
// 4. dropped：条数与响应 plan.dropped 相同
// ---------------------------------------------------------------------------

test('droppedRows 与 plan.dropped 等长且逐条保留 reason', () => {
  const resp = makeResponse();
  const rows = droppedRows(resp);
  assert.equal(rows.length, resp.plan.dropped.length);
  rows.forEach((r, i) => {
    assert.equal(r.id, resp.plan.dropped[i].id);
    assert.equal(r.reason, resp.plan.dropped[i].reason);
  });
});

test('dropped 区块条数与响应一致（DOM 层）', () => {
  const resp = makeResponse();
  const section = renderDroppedSection(resp);
  const table = findAll(section, (n) => n.getAttribute('data-table') === 'dropped')[0];
  assert.ok(table);
  const rows = findAll(table, (n) => n.tagName === 'tr');
  assert.equal(rows.length, 1 + resp.plan.dropped.length);
});

// ---------------------------------------------------------------------------
// 5. degraded：**全部**条目出现在页面上（不得只 console.log）
// ---------------------------------------------------------------------------

test('degradedLines 汇总顶层与 meta 两处来源，且不含空串', () => {
  const resp = makeResponse({
    meta: { ...makeResponse().meta, degraded: ['meta 侧降级'] },
  });
  const lines = degradedLines(resp);
  assert.deepEqual(lines, [...resp.degraded, 'meta 侧降级']);
  assert.ok(lines.every((l) => l.trim().length > 0));
});

test('degraded 非空时出现 warn 横幅，且包含**每一条**原文', () => {
  const resp = makeResponse();
  const root = renderRecallPageView({ view, status: 'ready', resp });
  const warns = findAll(root, (n) => String(n.className).includes('banner warn'));
  assert.equal(warns.length, 1, '应当恰好一条 warn 横幅（把全部条目合并显示）');
  const text = textOf(warns[0]);
  for (const line of resp.degraded) {
    assert.ok(text.includes(line), `degraded 条目未出现在页面上：${line}`);
  }
});

test('degraded 为空时不出现 warn 横幅（不要把"正常"也变成噪音）', () => {
  const resp = makeResponse({ degraded: [] });
  const root = renderRecallPageView({ view, status: 'ready', resp });
  const warns = findAll(root, (n) => String(n.className).includes('banner warn'));
  assert.equal(warns.length, 0);
});

// ---------------------------------------------------------------------------
// 6. as_of 横幅与"回到现在"
// ---------------------------------------------------------------------------

test('asOfBanner：带 as_of 时返回含具体时刻的文案，不带时返回 null', () => {
  const resp = makeResponse({ as_of: 1735689600 });
  const text = asOfBanner(resp, { ...view, asOf: 1735689600 });
  assert.ok(text, 'as_of 存在却没有文案');
  assert.ok(text.includes('2025-01-01'), `文案未包含具体时刻：${text}`);
  assert.ok(text.includes('1735689600'), '文案未包含 as_of 数值');
  assert.equal(asOfBanner(makeResponse(), view), null);
});

test('as_of 横幅出现在页面上且**不可折叠**（没有关闭按钮，只有回到现在）', () => {
  const resp = makeResponse({ as_of: 1735689600 });
  const root = renderRecallPageView({
    view: { ...view, asOf: 1735689600 }, status: 'ready', resp,
  });
  const el = findAll(root, (n) => n.getAttribute('data-section') === 'asof')[0];
  assert.ok(el, '页面上没有 as_of 横幅');
  const text = textOf(el);
  assert.ok(text.includes('你正在看'), `横幅文案不对：${text}`);
  const buttons = findAll(el, (n) => n.tagName === 'button');
  assert.equal(buttons.length, 1, 'as_of 横幅只应有"回到现在"一个按钮（不得有关闭按钮）');
  assert.ok(textOf(buttons[0]).includes('回到现在'));
});

test('点"回到现在"会触发 onBackToNow（清除动作由页面层重新请求完成）', () => {
  const resp = makeResponse({ as_of: 1735689600 });
  let called = 0;
  const root = renderRecallPageView({
    view: { ...view, asOf: 1735689600 }, status: 'ready', resp,
  }, { onBackToNow: () => { called += 1; } });
  const el = findAll(root, (n) => n.getAttribute('data-section') === 'asof')[0];
  const btn = findAll(el, (n) => n.tagName === 'button')[0];
  btn.onclick();
  assert.equal(called, 1);
});

test('clearAsOf 让 URL 里**没有** as_of 键（验收：点回到现在后 URL 中消失）', () => {
  const withAsOf = setAsOf(view, 1735689600);
  assert.ok(toRecallQuery(withAsOf).includes('as_of=1735689600'));
  const cleared = clearAsOf(withAsOf);
  assert.equal(toRecallQuery(cleared).includes('as_of'), false);
  // 其它参数不受影响
  assert.equal(cleared.q, view.q);
});

// ---------------------------------------------------------------------------
// 7. 空态：q 为空 → 空态文案 + 可点示例；不是空白、不是骨架残留
// ---------------------------------------------------------------------------

test('q 为空时显示空态（含示例查询按钮），且没有骨架/结果表', () => {
  const root = renderRecallPageView({ view: { ...view, q: '' }, status: 'idle', resp: null });
  const empty = findAll(root, (n) => String(n.className).includes('empty'))[0];
  assert.ok(empty, 'q 为空时没有空态块');
  const text = textOf(empty);
  assert.ok(text.includes('还没有查询'), `空态文案不对：${text}`);
  // 必须是可点的示例查询
  const buttons = findAll(empty, (n) => n.tagName === 'button');
  assert.ok(buttons.length > 0, '空态里没有可点的示例查询');
  // 不得是骨架残留
  assert.equal(findAll(root, (n) => n.getAttribute('aria-busy') !== null).length, 0, '空态里残留了骨架屏');
  // 不得是结果表
  assert.equal(findAll(root, (n) => n.getAttribute('data-table') === 'items').length, 0);
});

test('加载中显示骨架屏，带 aria-busy 与 aria-label', () => {
  const root = renderRecallPageView({ view, status: 'loading', resp: null });
  const sk = findAll(root, (n) => n.getAttribute('aria-busy') === 'true')[0];
  assert.ok(sk, '加载态没有骨架屏');
  assert.equal(sk.getAttribute('aria-label'), '正在加载召回解释');
});

test('capabilities.recall_plan=false 时显示"没能力"而不是"没数据"', () => {
  const resp = makeResponse({
    meta: {
      ...makeResponse().meta,
      capabilities: {
        recall_plan: false, audit: false, health: false, ontology: false,
        reasons: ['recall_plan: 未注入 recall.Recaller'],
      },
    },
  });
  assert.equal(noCapabilityReason(resp), 'recall_plan: 未注入 recall.Recaller');
  const root = renderRecallPageView({ view, status: 'ready', resp });
  const empty = findAll(root, (n) => String(n.className).includes('empty'))[0];
  assert.ok(empty, '缺能力时没有空态块');
  const text = textOf(empty);
  assert.ok(text.includes('不支持召回解释'), `文案未说明是能力问题：${text}`);
  assert.ok(text.includes('Recaller'), '未显示后端给出的原因');
  assert.equal(text.includes('还没有查询'), false, '把"没能力"说成了"还没查询"');
});

// ---------------------------------------------------------------------------
// 8. URL 即视图：解析 / 序列化 / 非法参数
// ---------------------------------------------------------------------------

test('parseRecallView 解析全部参数并保留顺序', () => {
  const { view: v, errors } = parseRecallView('?q=acme&scope=agent&kinds=semantic,decision&lanes=lexical&as_of=1735689600&k=7');
  assert.deepEqual(errors, []);
  assert.equal(v.q, 'acme');
  assert.equal(v.scope, 'agent');
  assert.deepEqual(v.kinds, ['semantic', 'decision']);
  assert.deepEqual(v.lanes, ['lexical']);
  assert.equal(v.asOf, 1735689600);
  assert.equal(v.k, 7);
});

test('parseRecallView 对非法 as_of / k / scope / kinds / lanes 显式报错（不白屏、不夹取）', () => {
  const cases = [
    ['?q=a&as_of=abc', 'as_of'],
    ['?q=a&as_of=1.5', 'as_of'],
    ['?q=a&k=0', 'k'],
    [`?q=a&k=${K_MAX + 1}`, 'k'],
    ['?q=a&k=abc', 'k'],
    ['?q=a&scope=bogus', 'scope'],
    ['?q=a&kinds=bogus', 'kinds'],
    ['?q=a&lanes=bogus', 'lanes'],
  ];
  for (const [qs, field] of cases) {
    const { errors } = parseRecallView(qs);
    assert.ok(errors.length > 0, `${qs} 没有报错`);
    assert.ok(errors.some((e) => e.field === field), `${qs} 的报错未指明 ${field}`);
  }
});

test('k 超限时报错文案含硬上限且明说“不静默夹取”（不会）', () => {
  const { errors } = parseRecallView(`?q=a&k=${K_MAX + 1}`);
  const e = errors.find((x) => x.field === 'k');
  assert.ok(e.message.includes(String(K_MAX)));
  // 与服务端同一话术（server/recall.go: "不会静默夹取"），前后端不得两说。
  assert.ok(e.message.includes('静默夹取'), `报错文案未明说不夹取：${e.message}`);
});

test('toRecallQuery 省略默认值，toRecallURL 不带 item 但带 explain=1', () => {
  const minimal = { q: 'acme', scope: '', kinds: [], lanes: [], asOf: null, k: K_DEFAULT, item: '' };
  assert.equal(toRecallQuery(minimal), '?q=acme');
  const full = { ...minimal, scope: 'user', kinds: ['semantic'], lanes: ['graph'], asOf: 100, k: 3, item: 'm-1' };
  const qs = toRecallQuery(full);
  assert.ok(qs.includes('scope=user'));
  assert.ok(qs.includes('kinds=semantic'));
  assert.ok(qs.includes('lanes=graph'));
  assert.ok(qs.includes('as_of=100'));
  assert.ok(qs.includes('k=3'));
  assert.ok(qs.includes('item=m-1'));
  const url = toRecallURL(full);
  assert.equal(url.includes('item='), false, 'item 是页面状态，不该进 API 请求');
  assert.ok(url.includes('explain=1'));
});

test('toggleList 增删并保持枚举顺序（URL 稳定 → 同一视图同一串）', () => {
  let v = { ...view, kinds: [] };
  v = toggleList(v, 'kinds', 'decision');
  v = toggleList(v, 'kinds', 'semantic');
  assert.deepEqual(v.kinds, ['semantic', 'decision'], '应按枚举顺序而不是点击顺序');
  v = toggleList(v, 'kinds', 'semantic');
  assert.deepEqual(v.kinds, ['decision']);
});

test('selectItem / withQuery 写进视图（选中项也进 URL）', () => {
  assert.equal(selectItem(view, 'm-1').item, 'm-1');
  assert.equal(withQuery(view, '  acme  ').q, 'acme');
  // 换查询串必须清掉选中项（结果集已变）
  assert.equal(withQuery(selectItem(view, 'm-1'), 'other').item, '');
});

test('参数错误时页面显式报错并给"修改参数"，**不给重试**', () => {
  const root = renderRecallPageView({
    view, status: 'ready', resp: null,
    errors: [{ field: 'k', message: 'k 不能超过硬上限 50' }],
  });
  const err = findAll(root, (n) => n.getAttribute('data-error-kind') !== null)[0];
  assert.ok(err, '没有错误态块');
  assert.equal(err.getAttribute('data-error-kind'), 'param');
  const text = textOf(err);
  assert.ok(text.includes('k 不能超过硬上限 50'), '未显示服务端/本地原话');
  const buttons = findAll(err, (n) => n.tagName === 'button');
  assert.equal(buttons.some((b) => textOf(b).includes('重试')), false, '参数错误不该给"重试"按钮');
});

// ---------------------------------------------------------------------------
// 9. 路由：页面判定与"未实现"话术
// ---------------------------------------------------------------------------

test('matchRoute 归一末尾斜杠并识别六页 + 404', () => {
  assert.equal(matchRoute('/').page, 'graph');
  assert.equal(matchRoute('/recall').page, 'recall');
  assert.equal(matchRoute('/recall/').page, 'recall');
  assert.equal(matchRoute('/review').page, 'review');
  assert.equal(matchRoute('/metrics').page, 'metrics');
  assert.equal(matchRoute('/nope').page, 'missing');
  assert.equal(normalizePath('/recall//'), '/recall');
});

test('未实现的页有专门话术，与 404 区分开', () => {
  assert.equal(isImplemented('graph'), true);
  assert.equal(isImplemented('recall'), true);
  assert.equal(isImplemented('metrics'), false);
  const msg = notImplementedMessage('metrics');
  assert.ok(msg.includes('还没实现'));
  assert.ok(msg.includes('Recall Inspector'));
});
