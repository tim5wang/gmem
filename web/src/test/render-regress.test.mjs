// P4 修复回归（F-1 / F-3 / F-4 / F-7）—— 与 render.test.mjs 分文件，避免改动既有 51 条。
//
// 为什么必须**穿过 render()**：既有 render.test.mjs 全部直接调 renderSVG，而
// F-1 的缺陷正出在 render() 的**返回值**上 —— 它过去只回渲染器名字符串
// 'svg'/'canvas'，调用方拿到 result.project 为 undefined，于是 Canvas 档
// （>2000 图元）一点就 TypeError。只测 renderSVG 永远测不到这条。
import test from 'node:test';
import assert from 'node:assert/strict';

import { render, renderSVG, STYLE, hitTest, isPendingReview } from '../js/render.js';
import { buildScene, pendingReviews } from '../js/model.js';
import { makeProjector } from '../js/geometry.js';

// ---- 最小 DOM / Canvas 打桩 ---------------------------------------------

class FakeElement {
  constructor(name) {
    this.tagName = name;
    this.attributes = new Map();
    this.children = [];
    this.textContent = '';
    this.style = {};
  }

  setAttribute(k, v) {
    this.attributes.set(k, String(v));
  }

  getAttribute(k) {
    return this.attributes.has(k) ? this.attributes.get(k) : null;
  }

  appendChild(child) {
    this.children.push(child);
    return child;
  }

  get firstChild() {
    return this.children.length ? this.children[0] : null;
  }

  removeChild(child) {
    const i = this.children.indexOf(child);
    if (i >= 0) this.children.splice(i, 1);
    return child;
  }

  all(pred, out = []) {
    if (pred(this)) out.push(this);
    for (const c of this.children) c.all(pred, out);
    return out;
  }
}

class FakeCanvas {
  constructor() {
    this.hidden = false;
    this.style = {};
    this.width = 0;
    this.height = 0;
    this.ops = [];
  }

  getContext() {
    const rec = (name) => (...args) => this.ops.push([name, ...args]);
    return {
      setTransform: rec('setTransform'), clearRect: rec('clearRect'),
      save: rec('save'), restore: rec('restore'),
      beginPath: rec('beginPath'), arc: rec('arc'),
      moveTo: rec('moveTo'), lineTo: rec('lineTo'), stroke: rec('stroke'),
      // 边改为二次贝塞尔曲线后，真实 ctx 有这个方法；打桩补齐它，
      // 否则 Canvas 档渲染直接抛 TypeError: quadraticCurveTo is not a function。
      quadraticCurveTo: rec('quadraticCurveTo'),
      fill: rec('fill'), fillText: rec('fillText'), setLineDash: rec('setLineDash'),
    };
  }
}

function installDOM() {
  globalThis.document = {
    createElementNS: (_ns, name) => new FakeElement(name),
  };
}

// fixture：与 render.test.mjs 同形（字段名与 server/types.go 一致）。
// isEdgeEl 判定“这是一个边图元”。
//
// 边从 `<line>` 改成 `<path>`（二次贝塞尔：长跨团边不再横切画布、不从无关节点
// 中间穿过），写死 `tagName === 'line'` 会一条边都选不到。这里两者都认。
//
// **这不是放宽断言**：真正的内容断言（stroke / dasharray / stroke-opacity /
// 端点是否落在圆外 / class 是否带 possibly-same）一条没改，改的只是“怎么定位元素”。
const isEdgeEl = (e) =>
  (e.tagName === 'line' || e.tagName === 'path') && e.getAttribute('data-edge-id') !== null;

function fixture(overrides = {}) {
  return {
    meta: { mode: 'focus', budget: 200, budget_max: 2000, node_count: 3, truncated: false },
    nodes: [
      { id: 'a', canonical_name: 'A', concept_id: 'c-1', concept_status: 'confirmed', degree: 1, depth: 0, matched: true, cluster: 'c-1', x: 0.1, y: 0.1 },
      { id: 'b', canonical_name: 'B', concept_id: 'c-2', concept_status: 'draft', degree: 9, depth: 1, matched: false, cluster: 'c-2', x: -0.4, y: 0.2 },
      { id: 'c', canonical_name: 'C', concept_id: 'c-1', concept_status: 'confirmed', degree: 3, depth: 1, matched: false, cluster: 'c-1', x: 0.5, y: -0.3 },
    ],
    edges: [
      { id: 'e-ok', from: 'a', to: 'c', relation: 'works_at', weight: 1, t_valid: 10, t_invalid: 0, invalidated: false, possibly_same: false },
      { id: 'e-dead', from: 'a', to: 'b', relation: 'subtype_of', weight: 1, t_valid: 10, t_invalid: 90, invalidated: true, possibly_same: false },
      { id: 'e-same', from: 'b', to: 'c', relation: 'possiblySame', weight: 0.7, t_valid: 10, t_invalid: 0, invalidated: false, possibly_same: true },
    ],
    clusters: [
      { id: 'c-1', concept_id: 'c-1', concept_name: '人物', concept_status: 'confirmed', count: 2, x: 0.2, y: 0.0, matched: true },
      { id: 'c-2', concept_id: 'c-2', concept_name: '组织', concept_status: 'draft', count: 9, x: -0.3, y: 0.1, matched: false },
    ],
    ...overrides,
  };
}

function renderThroughRender(scene, useCanvas, { width = 800, height = 600 } = {}) {
  installDOM();
  const svg = new FakeElement('svg');
  const canvas = new FakeCanvas();
  const out = render({ svg, canvas }, scene, { width, height, useCanvas });
  return { out, svg, canvas };
}

// ---- F-1 -----------------------------------------------------------------

test('【F-1】render() 返回渲染器产物：Canvas 档 project 是真函数且能投影世界坐标', () => {
  const scene = buildScene(fixture());
  const { out } = renderThroughRender(scene, true);

  assert.equal(out.renderer, 'canvas');
  assert.equal(typeof out.project, 'function', 'render() 必须交出 project（否则 Canvas 档点击即 TypeError: result.project is not a function）');
  assert.ok(out.pos instanceof Map, 'pos 必须一起返回：命中测试与工具提示都用它');
  assert.ok(out.clusterPos instanceof Map, 'clusterPos 必须一起返回：团下钻命中测试用它');

  const a = scene.nodes.find((n) => n.id === 'a');
  const p = out.project(a.x, a.y);
  assert.ok(Number.isFinite(p.x) && Number.isFinite(p.y), `投影必须给出有限坐标，实际 ${JSON.stringify(p)}`);
  assert.ok(p.x >= 0 && p.x <= 800 && p.y >= 0 && p.y <= 600, `投影必须落在画布内，实际 ${JSON.stringify(p)}`);
  assert.deepEqual(out.pos.get('a'), p, 'pos 与 project 必须给出同一套屏幕坐标');

  // 与直接用 makeProjector 的结果一致（两档共用同一份几何，档与档之间不得漂移）。
  const direct = makeProjector(scene, 800, 600);
  assert.deepEqual(p, direct(a.x, a.y));

  // 命中测试能吃下这个 project：这正是 Canvas 档点击所走的那条路。
  assert.equal(hitTest(scene, out.project, p.x, p.y)?.id, 'a');
});

test('【F-1】render() 的 SVG 档同样交出 project（调用方不必区分两档）', () => {
  const scene = buildScene(fixture());
  const { out } = renderThroughRender(scene, false);
  assert.equal(out.renderer, 'svg');
  assert.equal(typeof out.project, 'function');
  const b = scene.nodes.find((n) => n.id === 'b');
  const p = out.project(b.x, b.y);
  assert.ok(Number.isFinite(p.x) && Number.isFinite(p.y));
});

// ---- F-3 -----------------------------------------------------------------

test('【F-3】已裁决（invalidated）的 possiblySame 边不再蓝、不再带 possibly-same class', () => {
  // 服务端语义：裁决为 diff 后关系名仍是 possiblySame，只是 invalidated=true。
  const scene = buildScene(fixture({
    edges: [
      { id: 'e-pending', from: 'a', to: 'b', relation: 'possiblySame', weight: 1, t_valid: 10, t_invalid: 0, invalidated: false, possibly_same: true },
      { id: 'e-judged', from: 'b', to: 'c', relation: 'possiblySame', weight: 1, t_valid: 10, t_invalid: 90, invalidated: true, possibly_same: true },
    ],
  }));
  installDOM();
  const svg = new FakeElement('svg');
  renderSVG(svg, scene, { width: 800, height: 600 });
  const byId = (id) => svg.all((e) => isEdgeEl(e) && e.getAttribute('data-edge-id') === id)[0];

  const pending = byId('e-pending');
  assert.equal(pending.getAttribute('stroke'), STYLE.edgePossiblySameColor);
  assert.match(pending.getAttribute('class'), /possibly-same/, '待审边必须能被 click 选择器选中');
  assert.equal(pending.getAttribute('data-pending'), '1');

  const judged = byId('e-judged');
  assert.notEqual(judged.getAttribute('stroke'), STYLE.edgePossiblySameColor, '已裁决边不得再画成蓝边');
  assert.doesNotMatch(judged.getAttribute('class'), /possibly-same/, '已裁决边不得再被 click 选中（否则能再判一次 same → 真 Merge）');
  assert.match(judged.getAttribute('class'), /invalidated/, '已裁决边仍留作失效虚淡边（历史可见，不过滤）');
  assert.equal(judged.getAttribute('data-pending'), '0');
  // 服务端语义如实保留：关系名确实还是 possiblySame（只是不能再裁决）。
  assert.equal(judged.getAttribute('data-possibly-same'), '1');
});

test('【F-3】isPendingReview 与 model.pendingReviews 口径一致（不得漂移）', () => {
  const scene = buildScene(fixture({
    edges: [
      { id: 'e-pending', from: 'a', to: 'b', relation: 'possiblySame', weight: 1, t_valid: 10, t_invalid: 0, invalidated: false, possibly_same: true },
      { id: 'e-judged', from: 'b', to: 'c', relation: 'possiblySame', weight: 1, t_valid: 10, t_invalid: 90, invalidated: true, possibly_same: true },
    ],
  }));
  const fromModel = pendingReviews(scene).map((e) => e.id);
  const fromRender = scene.edges.filter(isPendingReview).map((e) => e.id);
  assert.deepEqual(fromRender, fromModel);
  assert.deepEqual(fromRender, ['e-pending']);
});

// ---- F-4 -----------------------------------------------------------------

test('【F-4】边从节点圆边缘起画（节点圆不再吞掉边的两端热区）', () => {
  const scene = buildScene(fixture());
  installDOM();
  const svg = new FakeElement('svg');
  renderSVG(svg, scene, { width: 800, height: 600 });

  const line = svg.all((e) => isEdgeEl(e) && e.getAttribute('data-edge-id') === 'e-ok')[0];
  const nodeA = scene.nodes.find((n) => n.id === 'a');
  const nodeC = scene.nodes.find((n) => n.id === 'c');
  const project = makeProjector(scene, 800, 600);
  const pa = project(nodeA.x, nodeA.y);
  const pc = project(nodeC.x, nodeC.y);

  const x1 = Number(line.getAttribute('x1'));
  const y1 = Number(line.getAttribute('y1'));
  const x2 = Number(line.getAttribute('x2'));
  const y2 = Number(line.getAttribute('y2'));

  // 起点/终点必须离开圆心至少一个半径（否则那两段仍被节点圆盖住 = 点不到）。
  const d1 = Math.hypot(x1 - pa.x, y1 - pa.y);
  const d2 = Math.hypot(x2 - pc.x, y2 - pc.y);
  assert.ok(d1 >= nodeA.r, `边起点必须落在节点圆外：距圆心 ${d1.toFixed(1)}，半径 ${nodeA.r.toFixed(1)}`);
  assert.ok(d2 >= nodeC.r, `边终点必须落在节点圆外：距圆心 ${d2.toFixed(1)}，半径 ${nodeC.r.toFixed(1)}`);
  // 但仍必须在两点之间（不能裁到反向或把线裁没）。
  assert.ok(d1 < Math.hypot(pc.x - pa.x, pc.y - pa.y));
  assert.ok(d2 < Math.hypot(pc.x - pa.x, pc.y - pa.y));
});

test('【F-4】团圆默认 pointer-events:none；折叠视图改用独立命中层', () => {
  // 未折叠：团是装饰信息，不得拦截鼠标（实测一条蓝边 40 个采样点全被团挡住）。
  installDOM();
  const flat = buildScene(fixture());
  const svg1 = new FakeElement('svg');
  renderSVG(svg1, flat, { width: 800, height: 600 });
  const clusterGroups1 = svg1.all((e) => e.tagName === 'g' && e.getAttribute('data-cluster-id') !== null);
  assert.ok(clusterGroups1.length > 0);
  for (const g of clusterGroups1) {
    const circles = g.children.filter((c) => c.tagName === 'circle');
    assert.ok(circles.length > 0, '团必须画出可见的圆');
    for (const c of circles) {
      assert.equal(c.getAttribute('pointer-events'), 'none', '未折叠时团圆必须 pointer-events:none');
    }
  }
  assert.equal(svg1.all((e) => e.getAttribute('class') === 'cluster-hit').length, 0, '未折叠时不应有命中层');

  // 折叠视图：视觉圆仍然 none，另给一个透明命中圆负责下钻（交互层与视觉层分离）。
  installDOM();
  const folded = buildScene(fixture({
    nodes: [],
    edges: [],
    meta: { mode: 'overview', truncated: true, node_count: 11, rendered_nodes: 0, rendered_clusters: 2 },
    cluster_edges: [{ from: 'c-1', to: 'c-2', count: 4, invalidated: false }],
  }));
  const svg2 = new FakeElement('svg');
  renderSVG(svg2, folded, { width: 800, height: 600 });
  const hits = svg2.all((e) => e.getAttribute('class') === 'cluster-hit');
  assert.equal(hits.length, folded.clusters.length, '折叠视图每个团都要有一个命中层');
  for (const h of hits) assert.equal(h.getAttribute('pointer-events'), 'auto');
  const visual = svg2.all((e) => e.tagName === 'circle' && e.getAttribute('class') !== 'cluster-hit');
  assert.ok(visual.length > 0);
  for (const c of visual) {
    assert.equal(c.getAttribute('pointer-events'), 'none', '视觉团圆必须永不吞事件');
  }
});

// ---- F-7 -----------------------------------------------------------------

test('【F-7】Canvas 档也画 cluster_edges（与 SVG 档一致）', () => {
  const scene = buildScene(fixture({
    nodes: [],
    edges: [],
    meta: { mode: 'overview', truncated: true, node_count: 11, rendered_nodes: 0, rendered_clusters: 2 },
    cluster_edges: [
      { from: 'c-1', to: 'c-2', count: 4, invalidated: false },
      { from: 'c-2', to: 'c-1', count: 1, invalidated: true },
    ],
  }));
  const { out, canvas } = renderThroughRender(scene, true);
  assert.equal(out.renderer, 'canvas');
  // 场景里没有普通边，因此每次 lineTo 都来自团间聚合边。
  const lineTos = canvas.ops.filter((o) => o[0] === 'lineTo');
  assert.equal(lineTos.length, scene.clusterEdges.length, 'Canvas 档必须画团间聚合边（否则大图折叠后团间关系凭空消失）');

  // 与 SVG 档对齐：同一份场景，两档画同样多的团间线段。
  installDOM();
  const svg = new FakeElement('svg');
  renderSVG(svg, scene, { width: 800, height: 600 });
  const svgClusterEdges = svg.all((e) => e.tagName === 'line' && e.getAttribute('class') === 'cluster-edge');
  assert.equal(svgClusterEdges.length, scene.clusterEdges.length);
  assert.equal(lineTos.length, svgClusterEdges.length, '两档画的团间边数必须一致');
});
