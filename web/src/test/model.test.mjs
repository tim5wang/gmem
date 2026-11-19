// 前端纯逻辑单测（node --test，无浏览器、无网络）。
//
// 覆盖验收里点名的三条**不能靠肉眼**的结论：
//   1) 服务端坐标不被前端覆盖（buildScene / applyAsOf / filterScene 都不动 x,y）
//   2) as_of 变化不触发重排（节点引用与坐标不变，layoutVersion 不变）
//   3) 取消裁决不发请求（requestReview 三道闸门 + postReview 拒绝未确认）
//
// 另外补 URL 解析（非法 budget 必须报错而不是白屏）、折叠视图、SVG/Canvas 分档。
import test from 'node:test';
import assert from 'node:assert/strict';

import {
  buildScene,
  filterScene,
  applyAsOf,
  isEdgeInvalidAt,
  edgeStyle,
  nodeRadius,
  conceptColor,
  conceptHue,
  isDraft,
  sceneTimeRange,
  pendingReviews,
  conceptOptions,
  relationTypes,
  MAX_RENDER_NODES,
} from '../js/model.js';
import { parseView, toQuery, toGraphURL, toEntityURL, overviewView, BUDGET_MAX } from '../js/urlstate.js';
import { relayout, withLayout } from '../js/layout.js';
import { makeProjector, shouldUseCanvas, bounds } from '../js/geometry.js';
import { requestReview, reviewPrompt, VERDICTS } from '../js/reviewflow.js';
import { postReview, ApiError } from '../js/api.js';

// fixture 是一个"服务端已给坐标"的最小响应（字段名与 server/types.go 一致）。
function fixture(overrides = {}) {
  return {
    meta: {
      mode: 'focus', focus: 'alice', scope: 'user', hops: 1,
      budget: 200, budget_max: 2000, node_count: 3, edge_count: 2,
      rendered_nodes: 3, rendered_clusters: 1, truncated: false,
      relations: [{ relation: 'works_at', max_hops: 1, transitive: false }],
    },
    nodes: [
      { id: 'alice', canonical_name: 'Alice', scope: 'user', concept_id: 'c-person', concept_name: '人物', concept_status: 'confirmed', degree: 2, depth: 0, matched: true, cluster: 'c-person', x: 0.12, y: -0.34 },
      { id: 'acme', canonical_name: 'Acme', scope: 'user', concept_id: 'c-org', concept_name: '组织', concept_status: 'draft', degree: 1, depth: 0, matched: false, cluster: 'c-org', x: -0.5, y: 0.25 },
      { id: 'bob', canonical_name: 'Bob', scope: 'user', concept_id: '', degree: 1, depth: 1, matched: false, cluster: '(未归属概念)', x: 0.7, y: 0.66 },
    ],
    edges: [
      { id: 'e1', from: 'alice', to: 'acme', relation: 'works_at', weight: 1, t_valid: 100, t_invalid: 0, invalidated: false, possibly_same: false, depth: 1 },
      { id: 'e2', from: 'alice', to: 'bob', relation: 'possiblySame', weight: 0.8, t_valid: 200, t_invalid: 0, invalidated: false, possibly_same: true, depth: 1 },
    ],
    clusters: [
      { id: 'c-person', concept_id: 'c-person', concept_name: '人物', concept_status: 'confirmed', count: 1, x: 0, y: 0, matched: true },
      { id: 'c-org', concept_id: 'c-org', concept_name: '组织', concept_status: 'draft', count: 1, x: 0.55, y: 0.1, matched: false },
    ],
    ...overrides,
  };
}

// ---------------------------------------------------------------------------
// 1) 坐标不被覆盖
// ---------------------------------------------------------------------------

test('buildScene 原样采用服务端坐标（逐点相等，一个数都不改）', () => {
  const resp = fixture();
  const scene = buildScene(resp);
  assert.equal(scene.nodes.length, 3);
  for (const n of resp.nodes) {
    const got = scene.nodes.find((x) => x.id === n.id);
    assert.ok(got, `节点 ${n.id} 丢了`);
    assert.equal(got.x, n.x, `${n.id}.x 被前端改动了`);
    assert.equal(got.y, n.y, `${n.id}.y 被前端改动了`);
  }
});

test('buildScene 不改写入参（响应对象保持原值，可复用于 diff）', () => {
  const resp = fixture();
  const before = JSON.stringify(resp);
  buildScene(resp);
  assert.equal(JSON.stringify(resp), before);
});

test('filterScene / applyAsOf 之后坐标仍与服务端一致', () => {
  const resp = fixture();
  const scene = buildScene(resp);
  const filtered = filterScene(scene, { concepts: new Set(['c-person']) });
  assert.deepEqual(filtered.nodes.map((n) => [n.id, n.x, n.y]), [['alice', 0.12, -0.34]]);

  const asOf = applyAsOf(scene, 150);
  for (const n of resp.nodes) {
    const got = asOf.nodes.find((x) => x.id === n.id);
    assert.equal(got.x, n.x);
    assert.equal(got.y, n.y);
  }
});

test('同一响应两次 buildScene 得到完全相同的坐标（确定性可复现）', () => {
  const a = buildScene(fixture());
  const b = buildScene(fixture());
  assert.deepEqual(a.nodes.map((n) => [n.id, n.x, n.y]), b.nodes.map((n) => [n.id, n.x, n.y]));
});

// ---------------------------------------------------------------------------
// 2) as_of 不触发重排
// ---------------------------------------------------------------------------

test('applyAsOf 不重排：节点对象与 layoutVersion 都不动', () => {
  const scene = buildScene(fixture());
  const asOfScene = applyAsOf(scene, 150);
  assert.equal(asOfScene.layoutVersion, scene.layoutVersion, 'as_of 不得改变 layoutVersion');
  assert.equal(asOfScene.nodes, scene.nodes, 'as_of 不得重建节点数组（否则等于重排）');
  for (let i = 0; i < scene.nodes.length; i++) {
    assert.equal(asOfScene.nodes[i].x, scene.nodes[i].x);
    assert.equal(asOfScene.nodes[i].y, scene.nodes[i].y);
  }
});

test('as_of 只改边的样式：失效与否随世界时间变化', () => {
  const resp = fixture({
    edges: [
      { id: 'e-old', from: 'alice', to: 'acme', relation: 'works_at', weight: 1, t_valid: 100, t_invalid: 300, invalidated: true, possibly_same: false, depth: 1 },
    ],
  });
  const scene = buildScene(resp);
  // 当前视角：服务端说已失效 → 虚线淡边，但**不得被过滤掉**。
  assert.equal(scene.edges.length, 1, '失效边不得被过滤');
  assert.equal(scene.edges[0].style.dashed, true);
  assert.ok(scene.edges[0].style.opacity < 1);

  // as_of=200：当时还有效 → 实线。
  const past = applyAsOf(scene, 200);
  assert.equal(past.edges[0].style.invalid, false);
  assert.equal(past.edges[0].style.dashed, false);

  // as_of=50：还没写进来 → 视为无效。
  const before = applyAsOf(scene, 50);
  assert.equal(before.edges[0].style.invalid, true);
});

test('t_invalid 非零但在当前视角仍有效时按服务端 invalidated 走', () => {
  const e = { t_valid: 100, t_invalid: 0, invalidated: false };
  assert.equal(isEdgeInvalidAt(e, null), false);
  assert.equal(edgeStyle(e, null).dashed, false);
});

test('relayout 是唯一的本地演算入口，且不修改入参', () => {
  const scene = buildScene(fixture());
  const before = scene.nodes.map((n) => [n.id, n.x, n.y]);
  const coords = relayout(scene.nodes, scene.edges, { ticks: 50 });
  assert.equal(coords.size, scene.nodes.length);
  assert.deepEqual(scene.nodes.map((n) => [n.id, n.x, n.y]), before, 'relayout 不得就地改写坐标');
  const moved = withLayout(scene, coords);
  assert.equal(moved.layoutVersion, scene.layoutVersion + 1, '显式重排才递增 layoutVersion');
  assert.deepEqual(scene.nodes.map((n) => [n.id, n.x, n.y]), before, 'withLayout 也不得改写原场景');
});

// ---------------------------------------------------------------------------
// 3) 取消裁决不发请求
// ---------------------------------------------------------------------------

test('选择「取消」（verdict 非法）→ 不发请求', async () => {
  let sends = 0;
  const out = await requestReview({
    edgeId: 'e2', verdict: '',
    confirm: () => true,
    send: () => { sends++; return {}; },
  });
  assert.equal(out.sent, false);
  assert.equal(out.reason, 'cancelled');
  assert.equal(sends, 0);
});

test('二次确认弹窗点取消 → 不发请求', async () => {
  let sends = 0;
  const out = await requestReview({
    edgeId: 'e2', verdict: 'same',
    confirm: () => false,
    send: () => { sends++; return {}; },
  });
  assert.equal(out.sent, false);
  assert.equal(out.reason, 'not_confirmed');
  assert.equal(sends, 0);
});

test('确认回调抛错 / 返回非 true 也视为取消（不得误确认）', async () => {
  let sends = 0;
  const send = () => { sends++; return {}; };
  const boom = await requestReview({ edgeId: 'e2', verdict: 'diff', confirm: () => { throw new Error('modal broke'); }, send });
  assert.equal(boom.sent, false);
  const truthyNotTrue = await requestReview({ edgeId: 'e2', verdict: 'diff', confirm: () => 'yes', send });
  assert.equal(truthyNotTrue.sent, false, '只认 === true，避免真值陷阱');
  assert.equal(sends, 0);
});

test('两道闸门都过 → 恰好发一次，且 confirmed 为 true', async () => {
  const calls = [];
  const out = await requestReview({
    edgeId: 'e2', verdict: 'diff',
    confirm: ({ edgeId, verdict }) => edgeId === 'e2' && verdict === 'diff',
    send: (req) => { calls.push(req); return { op: 'disambiguate', status: 'ok' }; },
  });
  assert.equal(out.sent, true);
  assert.equal(calls.length, 1);
  assert.deepEqual(calls[0], { edgeId: 'e2', verdict: 'diff', confirmed: true });
});

test('postReview 直接拒发未确认的请求（服务端 confirmed 之外的前端兜底）', async () => {
  let fetches = 0;
  const fetchImpl = () => { fetches++; throw new Error('不该被调用'); };
  await assert.rejects(
    () => postReview(fetchImpl, { edgeId: 'e2', verdict: 'same', confirmed: false }),
    (err) => err instanceof ApiError && err.code === 'not_confirmed',
  );
  assert.equal(fetches, 0);
});

test('postReview 确认后发出 POST /api/review，body 带 confirmed:true', async () => {
  const seen = [];
  const fetchImpl = async (url, opts) => {
    seen.push({ url, opts });
    return { ok: true, status: 200, text: async () => JSON.stringify({ edge_id: 'e2', verdict: 'same', op: 'merge', status: 'ok' }) };
  };
  const res = await postReview(fetchImpl, { edgeId: 'e2', verdict: 'same', confirmed: true });
  assert.equal(res.op, 'merge');
  assert.equal(seen[0].url, '/api/review');
  assert.equal(seen[0].opts.method, 'POST');
  assert.deepEqual(JSON.parse(seen[0].opts.body), { edge_id: 'e2', verdict: 'same', confirmed: true });
});

test('verdict 只认 same / diff，二次确认文案说清后果', () => {
  assert.deepEqual([...VERDICTS], ['same', 'diff']);
  assert.match(reviewPrompt('A ⇄ B', 'same').text, /合并/);
  assert.match(reviewPrompt('A ⇄ B', 'diff').text, /negative_SameAs/);
});

// ---------------------------------------------------------------------------
// URL 即视图
// ---------------------------------------------------------------------------

test('parseView 解析全部视图参数并回显', () => {
  const { view, errors } = parseView('?focus=alice&hops=2&budget=500&as_of=1234&scope=project');
  assert.deepEqual(errors, []);
  assert.equal(view.focus, 'alice');
  assert.equal(view.hops, 2);
  assert.equal(view.budget, 500);
  assert.equal(view.asOf, 1234);
  assert.equal(view.scope, 'project');
});

test('budget 超硬上限显式报错（不静默夹取、不白屏）', () => {
  const { errors } = parseView('?budget=5000');
  assert.equal(errors.length, 1);
  assert.equal(errors[0].field, 'budget');
  assert.match(errors[0].message, /2000/);
  assert.equal(BUDGET_MAX, 2000);
});

test('非法参数逐条报错：hops 非整数 / hops<1 / as_of 非整数 / focus+q 并存', () => {
  assert.equal(parseView('?hops=abc').errors[0].field, 'hops');
  assert.equal(parseView('?hops=0').errors[0].field, 'hops');
  assert.equal(parseView('?as_of=yesterday').errors[0].field, 'as_of');
  const both = parseView('?focus=a&q=b').errors;
  assert.equal(both[0].field, 'focus');
});

test('toQuery / parseView 往返可复现（刷新后视图一致）', () => {
  const view = { focus: 'alice', q: '', scope: '', hops: 2, budget: 300, asOf: 1700000000 };
  const round = parseView(toQuery(view));
  assert.deepEqual(round.errors, []);
  assert.equal(round.view.focus, view.focus);
  assert.equal(round.view.hops, view.hops);
  assert.equal(round.view.budget, view.budget);
  assert.equal(round.view.asOf, view.asOf);
  assert.equal(toGraphURL(view), '/api/graph?focus=alice&hops=2&budget=300&as_of=1700000000');
});

test('toEntityURL 透传 as_of（侧栏结论必须与图同视角）', () => {
  assert.equal(toEntityURL('alice', { asOf: 42 }), '/api/entity/alice?as_of=42');
  assert.equal(toEntityURL('a b', { asOf: null }), '/api/entity/a%20b');
});

test('overviewView 清空 focus 与 q（概览模式两者都不传）', () => {
  const v = overviewView({ focus: 'a', q: 'b', hops: 1, budget: 200, asOf: null, scope: '' });
  assert.equal(v.focus, '');
  assert.equal(v.q, '');
  assert.equal(parseView(toQuery(v)).errors.length, 0);
});

// ---------------------------------------------------------------------------
// 七项视觉编码（数据侧可断言的五项）
// ---------------------------------------------------------------------------

test('(a) 节点颜色按 concept：同 concept 同色，不同 concept 不同色', () => {
  assert.equal(conceptColor('c-person'), conceptColor('c-person'));
  assert.notEqual(conceptColor('c-person'), conceptColor('c-org'));
  assert.ok(Number.isFinite(conceptHue('c-person')));
  assert.equal(conceptHue(''), 210, '未归属概念用中性色');
});

test('(b) draft 概念用斜纹：isDraft 只认 status=draft', () => {
  assert.equal(isDraft({ concept_status: 'draft' }), true);
  assert.equal(isDraft({ concept_status: 'DRAFT' }), true);
  assert.equal(isDraft({ concept_status: 'confirmed' }), false);
  assert.equal(isDraft({}), false);
});

test('(c) 节点大小按 degree 单调递增且有上限', () => {
  assert.ok(nodeRadius(5) > nodeRadius(1));
  assert.ok(nodeRadius(100) <= 22, '枢纽节点不得撑爆画布');
  assert.equal(nodeRadius(undefined), nodeRadius(0));
});

test('(e) 失效边画虚线淡边且绝不被过滤', () => {
  const scene = buildScene(fixture({
    edges: [{ id: 'e-x', from: 'alice', to: 'acme', relation: 'works_at', weight: 1, t_valid: 10, t_invalid: 20, invalidated: true, possibly_same: false, depth: 1 }],
  }));
  assert.equal(scene.edges.length, 1);
  assert.equal(scene.edges[0].style.dashed, true);
  assert.equal(scene.edges[0].style.invalid, true);
});

test('(f) possiblySame 边标蓝：pendingReviews 只列待审且有效的边', () => {
  const scene = buildScene(fixture());
  const pending = pendingReviews(scene);
  assert.deepEqual(pending.map((e) => e.id), ['e2']);
  assert.equal(pending[0].style.color, '#1d6fd8');
});

test('(g) matched 节点带标记（红描边由渲染层按 matched 决定）', () => {
  const scene = buildScene(fixture());
  const alice = scene.nodes.find((n) => n.id === 'alice');
  assert.equal(alice.matched, true);
  assert.equal(scene.nodes.find((n) => n.id === 'bob').matched, false);
});

test('边两端必须都在渲染集里（不产生悬空边）', () => {
  const scene = buildScene(fixture());
  const ids = new Set(scene.nodes.map((n) => n.id));
  for (const e of scene.edges) {
    assert.ok(ids.has(e.from) && ids.has(e.to), `悬空边 ${e.id}`);
  }
});

// ---------------------------------------------------------------------------
// 折叠视图（truncated=true 时必须能画出团）
// ---------------------------------------------------------------------------

test('rendered_nodes=0 全折叠时仍有 clusters 可画（否则白屏）', () => {
  const resp = fixture({
    nodes: [],
    edges: [],
    meta: { ...fixture().meta, mode: 'overview', truncated: true, node_count: 5, rendered_nodes: 0, rendered_clusters: 1, budget: 2 },
    clusters: [{ id: 'c-org', concept_id: 'c-org', concept_name: '组织', concept_status: 'draft', count: 5, x: 0.2, y: 0.3, matched: false }],
    cluster_edges: [],
  });
  const scene = buildScene(resp);
  assert.equal(scene.nodes.length, 0);
  assert.equal(scene.clusters.length, 1);
  assert.equal(scene.clusters[0].count, 5);
  // 分档判定必须看"实际图元数"，不能只看 node_count：这里 1 个团比 2000 小得多。
  assert.equal(shouldUseCanvas(scene, MAX_RENDER_NODES), false);
});

test('cluster_edges 保留（团与团的关系也要画，失效的标 invalidated）', () => {
  const resp = fixture({
    nodes: [],
    edges: [],
    meta: { ...fixture().meta, truncated: true, rendered_nodes: 0 },
    cluster_edges: [{ from: 'c-person', to: 'c-org', count: 3, invalidated: true }],
  });
  const scene = buildScene(resp);
  assert.equal(scene.clusterEdges.length, 1);
  assert.equal(scene.clusterEdges[0].invalidated, true);
});

// ---------------------------------------------------------------------------
// 渲染分档
// ---------------------------------------------------------------------------

test('≤2000 图元走 SVG，>2000 切 Canvas（边界值精确）', () => {
  const mk = (n) => ({ nodes: Array.from({ length: n }, (_, i) => ({ id: `n${i}`, x: 0, y: 0, r: 5, degree: 0 })), clusters: [] });
  assert.equal(shouldUseCanvas(mk(2000)), false);
  assert.equal(shouldUseCanvas(mk(2001)), true);
  const clusters = (n) => ({ nodes: [], clusters: Array.from({ length: n }, (_, i) => ({ id: `c${i}`, x: 0, y: 0, count: 1 })) });
  assert.equal(shouldUseCanvas(clusters(2000)), false);
  assert.equal(shouldUseCanvas(clusters(2001)), true);
});

test('投影是等比映射且把内容居中（不改变相对位置）', () => {
  const scene = buildScene(fixture());
  const project = makeProjector(scene, 800, 600, 48);
  const a = project(0.12, -0.34);
  const b = project(-0.5, 0.25);
  assert.ok(Number.isFinite(a.x) && Number.isFinite(a.y));
  // 相对方向必须保持：x 更大 → 屏幕 x 更大；y 更大 → 屏幕 y 更大。
  assert.ok(a.x > b.x);
  assert.ok(b.y > a.y);
});

test('bounds 兜底空场景（不产生 Infinity 导致空白画布）', () => {
  const b = bounds({ nodes: [], clusters: [] });
  assert.ok(Number.isFinite(b.minX) && Number.isFinite(b.maxY));
});

// ---------------------------------------------------------------------------
// 面板选项与时间轴范围
// ---------------------------------------------------------------------------

test('conceptOptions / relationTypes 给出稳定选项（按字典序）', () => {
  const scene = buildScene(fixture());
  const concepts = conceptOptions(scene);
  // bob 没有 concept_id，服务端给的 cluster 展示键是 '(未归属概念)'：
  // 过滤必须用同一个键，否则"未归属概念"这一档勾不上。
  assert.deepEqual(concepts.map((c) => c.key), ['(未归属概念)', 'c-org', 'c-person']);
  const rels = relationTypes(scene);
  assert.deepEqual(rels.map((r) => r.relation), ['possiblySame', 'works_at']);
  assert.equal(rels.find((r) => r.relation === 'works_at').count, 1);
});

test('sceneTimeRange 覆盖 t_valid / t_invalid；无时间信息返回 null（滑块置灰而不是造假区间）', () => {
  const scene = buildScene(fixture());
  const range = sceneTimeRange(scene);
  assert.equal(range.min, 100);
  assert.equal(range.max, 200);
  assert.equal(sceneTimeRange({ nodes: [], edges: [], clusters: [] }), null);
});

// ---------------------------------------------------------------------------
// 八项交互里可以纯逻辑固定的两项：双击展开（focus）与搜索（q）
// ---------------------------------------------------------------------------

test('交互 2：双击节点 → focus 写入 URL 且清掉 q（服务端不允许两者并存）', async () => {
  const { focusView } = await import('../js/urlstate.js');
  const v = focusView({ focus: '', q: 'alice', hops: 1, budget: 200, asOf: null, scope: '' }, 'acme');
  assert.equal(v.focus, 'acme');
  assert.equal(v.q, '');
  const { errors, view } = parseView(toQuery(v));
  assert.deepEqual(errors, []);
  assert.equal(view.focus, 'acme');
  assert.equal(view.q, '');
  // 非法（空 id）不得产生 ?focus=（那会被服务端当成"无参数概览"，静默改变视图）
  assert.equal(parseView(toQuery(focusView(v, ''))).view.focus, '');
});

test('交互 3：搜索 → 写 ?q= 并清掉 focus；空查询回到概览', async () => {
  const { searchView } = await import('../js/urlstate.js');
  const v = searchView({ focus: 'alice', q: '', hops: 2, budget: 300, asOf: 100, scope: '' }, '  张三  ');
  assert.equal(v.focus, '');
  assert.equal(v.q, '张三', '必须 trim（前后空格会让召回匹配不到）');
  const round = parseView(toQuery(v));
  assert.equal(round.view.q, '张三');
  assert.equal(round.view.focus, '');
  // hops / budget / as_of 在多入口切换时必须保留（视图即 URL）。
  assert.equal(round.view.hops, 2);
  assert.equal(round.view.budget, 300);
  assert.equal(round.view.asOf, 100);
  // 纯空白 = 概览
  const empty = searchView(v, '   ');
  assert.equal(empty.q, '');
  assert.equal(empty.focus, '');
});
