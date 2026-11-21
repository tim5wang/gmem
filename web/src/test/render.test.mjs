// 渲染层单测：用**最小 DOM 打桩**跑 renderSVG，直接断言产出的图元属性。
//
// 为什么值得这么做：七项视觉编码里 (d)(e)(f)(g) 只在渲染层表现为描边/虚线/颜色，
// 数据层断言（fill/dash 字段）证明不了"画出来的 SVG 真是那个样式"。
// 有了这层，`npm test` 就能替代"打开浏览器看一眼"。
import test from 'node:test';
import assert from 'node:assert/strict';

import { renderSVG, STYLE, hitTest, hitTestCluster } from '../js/render.js';
import { buildScene } from '../js/model.js';
import { makeProjector } from '../js/geometry.js';

// ---- 最小 DOM 打桩 -------------------------------------------------------

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
  // 深度优先收集（含自身），模拟 querySelectorAll 的用途
  all(pred, out = []) {
    if (pred(this)) out.push(this);
    for (const c of this.children) c.all(pred, out);
    return out;
  }
}

function installDOM() {
  globalThis.document = {
    createElementNS: (_ns, name) => new FakeElement(name),
  };
}

// isEdgeEl 判定“这是一个边图元”。
//
// 边从 `<line>` 改成 `<path>`（二次贝塞尔：长跨团边不再横切画布、不从无关节点
// 中间穿过），写死 `tagName === 'line'` 会一条边都选不到。这里两者都认。
//
// **这不是放宽断言**：真正的内容断言（stroke / dasharray / stroke-opacity /
// 端点是否落在圆外）一条没改，改的只是“怎么定位到那个元素”。
const isEdgeEl = (e) =>
  (e.tagName === 'line' || e.tagName === 'path') && e.getAttribute('data-edge-id') !== null;

function fixture() {
  return {
    meta: { mode: 'focus', budget: 200, budget_max: 2000, node_count: 4, truncated: false },
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
  };
}

function renderFixture(overrides = {}) {
  installDOM();
  const scene = buildScene(fixture(), overrides);
  const svg = new FakeElement('svg');
  renderSVG(svg, scene, { width: 800, height: 600 });
  return { svg, scene };
}

// ---- 断言 ---------------------------------------------------------------

test('渲染产出边 / 团 / 节点 / 标签四层，且图元数与场景一致', () => {
  const { svg, scene } = renderFixture();
  const edges = svg.all((e) => isEdgeEl(e));
  const nodes = svg.all((e) => e.tagName === 'g' && e.getAttribute('data-node-id') !== null);
  const clusters = svg.all((e) => e.tagName === 'g' && e.getAttribute('data-cluster-id') !== null);
  assert.equal(edges.length, scene.edges.length);
  assert.equal(nodes.length, scene.nodes.length);
  assert.equal(clusters.length, scene.clusters.length, '折叠团必须被画出来（否则白屏）');
});

test('(a) 节点填充按 concept 取色：同 concept 同色、不同 concept 不同色', () => {
  const { svg } = renderFixture();
  const circleOf = (id) => svg
    .all((e) => e.tagName === 'g' && e.getAttribute('data-node-id') === id)[0]
    .children[0].getAttribute('fill');
  assert.equal(circleOf('a'), circleOf('c'), '同一 concept 必须同色');
  assert.notEqual(circleOf('a'), circleOf('b'), '不同 concept 必须不同色');
});

test('(b) draft 概念用斜纹 pattern（svge 侧填 url(#draft-hatch)）并额外描虚线圈', () => {
  const { svg } = renderFixture();
  const draft = svg.all((e) => e.tagName === 'g' && e.getAttribute('data-node-id') === 'b')[0];
  assert.equal(draft.children[0].getAttribute('fill'), 'url(#draft-hatch)');
  const ring = draft.children[1];
  assert.equal(ring.getAttribute('stroke-dasharray'), '2 2', 'draft 需可区分的额外描边');
  // 斜纹 pattern 本身也要真的定义出来（否则 fill 指向不存在的 id = 透明）。
  const pattern = svg.all((e) => e.tagName === 'pattern' && e.getAttribute('id') === 'draft-hatch');
  assert.equal(pattern.length, 1);
  assert.equal(pattern[0].getAttribute('patternTransform'), 'rotate(45)');
  // 非 draft 节点不得用斜纹。
  const confirmed = svg.all((e) => e.tagName === 'g' && e.getAttribute('data-node-id') === 'a')[0];
  assert.notEqual(confirmed.children[0].getAttribute('fill'), 'url(#draft-hatch)');
});

test('(c) 节点半径按 degree 递增（degree 9 > degree 1）', () => {
  const { svg } = renderFixture();
  const rOf = (id) => Number(svg
    .all((e) => e.tagName === 'g' && e.getAttribute('data-node-id') === id)[0]
    .children[0].getAttribute('r'));
  assert.ok(rOf('b') > rOf('a'), `degree 大的节点必须更大：${rOf('b')} vs ${rOf('a')}`);
});

test('(d) 当前有效边是实线（无 dasharray）', () => {
  const { svg } = renderFixture();
  const line = svg.all((e) => isEdgeEl(e) && e.getAttribute('data-edge-id') === 'e-ok')[0];
  assert.equal(line.getAttribute('stroke-dasharray'), 'none');
  assert.equal(line.getAttribute('stroke'), STYLE.edgeColor);
  assert.equal(line.getAttribute('stroke-opacity'), '1');
});

test('(e) t_invalid 非零的已失效边画虚线淡边，且**仍然出现在图里**（不过滤）', () => {
  const { svg } = renderFixture();
  const dead = svg.all((e) => isEdgeEl(e) && e.getAttribute('data-edge-id') === 'e-dead');
  assert.equal(dead.length, 1, '失效边不得被过滤掉');
  assert.equal(dead[0].getAttribute('stroke-dasharray'), '4 3');
  assert.ok(Number(dead[0].getAttribute('stroke-opacity')) < 1, '失效边必须变淡');
  assert.equal(dead[0].getAttribute('data-invalid'), '1');
});

test('(e2) 时间轴回拨到失效前：同一条边变回实线（只改样式，不增删图元）', () => {
  const { svg, scene } = renderFixture({ asOf: 50 });
  const dead = svg.all((e) => isEdgeEl(e) && e.getAttribute('data-edge-id') === 'e-dead')[0];
  assert.equal(dead.getAttribute('stroke-dasharray'), 'none', 'as_of=50 时该边还没失效');
  assert.equal(dead.getAttribute('stroke-opacity'), '1');
  // 图元数不变 = 没有重排/重建结构。
  assert.equal(svg.all((e) => isEdgeEl(e)).length, scene.edges.length);
});

test('(f) possiblySame 边是蓝色（且可点裁决）', () => {
  const { svg } = renderFixture();
  const same = svg.all((e) => isEdgeEl(e) && e.getAttribute('data-edge-id') === 'e-same')[0];
  assert.equal(same.getAttribute('stroke'), STYLE.edgePossiblySameColor);
  assert.equal(same.getAttribute('data-possibly-same'), '1');
  assert.match(same.getAttribute('class'), /possibly-same/, '需要 class 供点击裁决绑定');
});

test('(g) matched 节点用红色描边；未命中的不是红描边', () => {
  const { svg } = renderFixture();
  const matched = svg.all((e) => e.tagName === 'g' && e.getAttribute('data-node-id') === 'a')[0];
  assert.equal(matched.children[0].getAttribute('stroke'), STYLE.matchedStroke);
  assert.match(matched.getAttribute('class'), /matched/);
  const plain = svg.all((e) => e.tagName === 'g' && e.getAttribute('data-node-id') === 'c')[0];
  assert.notEqual(plain.children[0].getAttribute('stroke'), STYLE.matchedStroke);
});

test('边标签默认不出现在图元里（仅悬停显示，避免文字堆叠）', () => {
  const { svg } = renderFixture();
  const texts = svg.all((e) => e.tagName === 'text');
  const edgeLabels = texts.filter((t) => /works_at|subtype_of|possiblySame/.test(t.textContent));
  assert.equal(edgeLabels.length, 0, '默认不得画边标签');
  // 但节点标签与团标签（名字 + 计数）必须有。
  assert.ok(texts.some((t) => t.textContent === 'A'));
  assert.ok(texts.some((t) => /人事|人物/.test(t.textContent) || /人物 · 2/.test(t.textContent)));
});

test('折叠团显示 concept 名与计数（折叠视图必须自解释）', () => {
  const { svg } = renderFixture();
  const texts = svg.all((e) => e.tagName === 'text').map((t) => t.textContent);
  assert.ok(texts.includes('人物 · 2'), `团标签应含名字与计数，实际：${JSON.stringify(texts)}`);
  assert.ok(texts.includes('组织 · 9'));
});

test('团间聚合边也画出来（cluster_edges）', () => {
  installDOM();
  const scene = buildScene({
    ...fixture(),
    nodes: [],
    edges: [],
    meta: { mode: 'overview', truncated: true, node_count: 11, rendered_nodes: 0, rendered_clusters: 2 },
    cluster_edges: [{ from: 'c-1', to: 'c-2', count: 4, invalidated: false }],
  });
  const svg = new FakeElement('svg');
  renderSVG(svg, scene, { width: 800, height: 600 });
  const clusterEdges = svg.all((e) => e.tagName === 'line' && e.getAttribute('class') === 'cluster-edge');
  assert.equal(clusterEdges.length, 1);
});

test('非法坐标（NaN）不会崩渲染（几何退化时不抛异常）', () => {
  installDOM();
  const scene = buildScene({
    ...fixture(),
    nodes: [{ id: 'x', canonical_name: 'X', degree: 0, x: NaN, y: NaN }],
    edges: [],
    clusters: [],
  });
  const svg = new FakeElement('svg');
  renderSVG(svg, scene, { width: 400, height: 300 });
  const nodes = svg.all((e) => e.tagName === 'g' && e.getAttribute('data-node-id') !== null);
  assert.equal(nodes.length, 1);
});

test('Canvas 档的命中测试与 SVG 档语义一致（点得中节点与团）', () => {
  const scene = buildScene(fixture());
  const project = makeProjector(scene, 800, 600);
  const a = scene.nodes.find((n) => n.id === 'a');
  const p = project(a.x, a.y);
  const hit = hitTest(scene, project, p.x, p.y);
  assert.equal(hit?.id, 'a');
  // 远离任何节点 → 不命中（避免"点空白也开侧栏"）。
  assert.equal(hitTest(scene, project, 0, 0), null);
  const c = scene.clusters[0];
  const cp = project(c.x, c.y);
  assert.equal(hitTestCluster(scene, project, cp.x, cp.y)?.id, c.id);
});
