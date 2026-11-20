// 布局"松散均匀"的三条定量验收：标签避让（无压叠）、视口适配（居中对称）、性能。
//
// 为什么这些必须是断言而不是截图：截图只能证明"这一次看着还行"，
// 而这三条都是"改一行系数就会悄悄退化"的性质：
//   - 标签避让退化 → 两行字重新叠在一起，图上读不出名字；
//   - 视口适配退化 → 内容贴边/留白全堆一侧（用户报的就是这个）；
//   - 性能退化 → 3000 节点卡住首屏。
import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

import { layoutLabels, textWidth, clusterObstacles, LABEL } from '../js/labelLayouter.js';
import { makeProjector, contentBox, bounds, clusterPixelRadius } from '../js/geometry.js';
import { buildScene } from '../js/model.js';
import { renderSVG } from '../js/render.js';

// demoScene 是**真实服务端输出**的快照（gmem demo 种子 + GET /api/graph 原样保存）。
//
// 用真实数据而不是手搓 4 个节点：标签压叠正是"名字长短不一 + 节点密集"才出现，
// 手工 fixture 容易恰好避开冲突，测出来永远绿。
const demoResp = JSON.parse(
  readFileSync(fileURLToPath(new URL('./fixtures/demo-graph.json', import.meta.url)), 'utf8'),
);

function demoScene() {
  return buildScene(demoResp);
}

// 用与 renderSVG 完全相同的口径算屏幕坐标 + 跑避让。
function labelsFor(scene, width, height) {
  const project = makeProjector(scene, width, height);
  const items = scene.nodes.map((n) => {
    const p = project(n.x, n.y);
    return { id: n.id, x: p.x, y: p.y, r: n.r, degree: n.degree, text: n.canonical_name || n.id };
  });
  const out = layoutLabels(items, { width, height, obstacles: clusterObstacles(scene, project) });
  return { out, project };
}

// box 还原一个已摆放标签的包围盒（与 labelLayouter 内部同一估算口径）。
function labelBox(label, fontSize = LABEL.fontSize) {
  const w = textWidth(label.text, fontSize);
  const ascent = fontSize * 0.8;
  let minX = label.x;
  if (label.anchor === 'below' || label.anchor === 'above') minX = label.x - w / 2;
  else if (label.anchor === 'left') minX = label.x - w;
  return {
    minX,
    maxX: minX + w,
    minY: label.y - ascent,
    maxY: label.y - ascent + LABEL.lineHeight,
  };
}

function boxesIntersect(a, b) {
  return a.minX < b.maxX && b.minX < a.maxX && a.minY < b.maxY && b.minY < a.maxY;
}

// ---------------------------------------------------------------------------
// 定量验收 C：标签无重叠 + 不靠大面积隐藏来"解决"
// ---------------------------------------------------------------------------

test('【定量 C】demo 数据在 1400×1200 下：任意两个标签包围盒不相交', () => {
  const scene = demoScene();
  const { out } = labelsFor(scene, 1400, 1200);
  assert.ok(out.labels.length >= 2, '必须真的摆出多个标签（否则断言空转）');
  const boxes = out.labels.map((l) => ({ l, b: labelBox(l) }));
  for (let i = 0; i < boxes.length; i++) {
    for (let j = i + 1; j < boxes.length; j++) {
      assert.equal(
        boxesIntersect(boxes[i].b, boxes[j].b),
        false,
        `标签压叠："${boxes[i].l.text}" 与 "${boxes[j].l.text}" 的包围盒相交`,
      );
    }
  }
});

test('【定量 C】被隐藏的标签 ≤ 总数的 40%（不能靠隐藏绝大多数来"解决"压叠）', () => {
  const scene = demoScene();
  for (const [w, h] of [[1400, 1200], [1200, 900], [800, 1200]]) {
    const { out } = labelsFor(scene, w, h);
    const ratio = out.hidden.length / out.total;
    assert.ok(
      ratio <= 0.4,
      `${w}×${h}: 隐藏 ${out.hidden.length}/${out.total}（${(ratio * 100).toFixed(1)}%）> 40%`,
    );
    assert.ok(out.shown >= out.total * 0.6, `${w}×${h}: 只显示了 ${out.shown} 个标签`);
  }
});

test('【定量 C】重要节点（degree 高）优先保住标签：degree=0 的必须先被牺牲', () => {
  // 人为制造一条"密集线 + 低 degree 节点"，看是否会优先丢掉低 degree 的标签。
  const items = [];
  for (let i = 0; i < 12; i++) {
    items.push({ id: `low${i}`, x: 200 + i * 3, y: 300, r: 5, degree: 0, text: `低${i}` });
  }
  items.push({ id: 'hub', x: 200, y: 300, r: 14, degree: 99, text: 'Hub' });
  const out = layoutLabels(items, { width: 400, height: 600 });
  assert.ok(out.labels.some((l) => l.id === 'hub'), 'degree 最高的节点标签必须保住');
  assert.ok(out.hidden.length > 0, '这种密度下必然有标签被隐藏（否则避让没生效）');
  assert.ok(out.hidden.every((id) => id.startsWith('low')), `只应牺牲低 degree 节点，实际隐藏 ${out.hidden}`);
});

test('【定量 C】标签避让是确定性的：同一输入两次结果逐字段一致', () => {
  const scene = demoScene();
  const a = labelsFor(scene, 1400, 1200).out;
  const b = labelsFor(scene, 1400, 1200).out;
  assert.deepEqual(b, a, '同一份数据两次布局必须完全一致（不得含随机/时间因素）');
});

test('【定量 C】渲染出来的 SVG 里 node-label 也不压叠（避让真的接进了 renderSVG）', () => {
  const scene = demoScene();
  const svg = renderDemoSVG(scene, 1400, 1200);
  const texts = svg.all((e) => e.tagName === 'text' && e.getAttribute('class') === 'node-label');
  assert.ok(texts.length > 0, '必须画出了节点标签');
  const boxes = texts.map((t) => ({
    text: t.textContent,
    b: labelBox({
      x: Number(t.getAttribute('x')),
      y: Number(t.getAttribute('y')),
      anchor: t.getAttribute('data-label-anchor'),
      text: t.textContent,
    }),
  }));
  for (let i = 0; i < boxes.length; i++) {
    for (let j = i + 1; j < boxes.length; j++) {
      assert.equal(
        boxesIntersect(boxes[i].b, boxes[j].b),
        false,
        `SVG 中标签压叠："${boxes[i].text}" 与 "${boxes[j].text}"`,
      );
    }
  }
  // 隐藏比例同样受 40% 约束（信息不能被藏起来）。
  const shown = Number(svg.getAttribute('data-labels-shown'));
  const hidden = Number(svg.getAttribute('data-labels-hidden'));
  assert.equal(shown, texts.length);
  assert.ok(hidden / (shown + hidden) <= 0.4, `SVG 隐藏比例 ${hidden}/${shown + hidden} 超过 40%`);
});

test('【定量 C】标签全部落在画布内（不得画出 viewBox 被切掉半个字）', () => {
  const scene = demoScene();
  const W = 1400;
  const H = 1200;
  const { out } = {
    out: labelsFor(scene, W, H).out,
  };
  for (const l of out.labels) {
    const b = labelBox(l);
    assert.ok(b.minX >= 0 && b.maxX <= W, `标签 "${l.text}" 水平越界：[${b.minX},${b.maxX}] vs [0,${W}]`);
    assert.ok(b.minY >= 0 && b.maxY <= H, `标签 "${l.text}" 垂直越界：[${b.minY},${b.maxY}] vs [0,${H}]`);
  }
});

test('【定量 C】团标签也算障碍：节点标签不压在团名上', () => {
  const scene = demoScene();
  const W = 1400;
  const H = 1200;
  const { out, project } = labelsFor(scene, W, H);
  const obstacles = clusterObstacles(scene, project);
  for (const l of out.labels) {
    const b = labelBox(l);
    for (const o of obstacles) {
      assert.equal(boxesIntersect(b, o), false, `节点标签 "${l.text}" 压在团标签上`);
    }
  }
});

// ---------------------------------------------------------------------------
// 定量验收 D：视口适配（居中 + 留白对称 + 等比）
// ---------------------------------------------------------------------------

test('【定量 D】1400×1200 与 800×1200：内容居中，左右留白差 ≤ 8px', () => {
  const scene = demoScene();
  for (const [W, H] of [[1400, 1200], [800, 1200]]) {
    const project = makeProjector(scene, W, H);
    const b = contentBox(scene, project);
    const left = b.minX;
    const right = W - b.maxX;
    const top = b.minY;
    const bottom = H - b.maxY;
    assert.ok(
      Math.abs(left - right) <= 8,
      `${W}×${H} 左右留白不对称：left=${left.toFixed(1)} right=${right.toFixed(1)}`,
    );
    assert.ok(
      Math.abs(top - bottom) <= 8,
      `${W}×${H} 上下留白不对称：top=${top.toFixed(1)} bottom=${bottom.toFixed(1)}`,
    );
    // 内容必须整个落在画布内（含图元自身半径）。
    assert.ok(b.minX >= 0 && b.maxX <= W, `${W}×${H} 内容水平越界：[${b.minX},${b.maxX}]`);
    assert.ok(b.minY >= 0 && b.maxY <= H, `${W}×${H} 内容垂直越界：[${b.minY},${b.maxY}]`);
  }
});

test('【定量 D】投影等比（不变形）：横竖同比例，且方向保持', () => {
  const scene = demoScene();
  const project = makeProjector(scene, 1400, 1200);
  // 世界坐标里一个正方形，投到屏幕上必须还是正方形（边长相等）。
  const a = project(0, 0);
  const b = project(0.1, 0);
  const c = project(0, 0.1);
  const dx = Math.abs(b.x - a.x);
  const dy = Math.abs(c.y - a.y);
  assert.ok(Math.abs(dx - dy) < 1e-9, `投影非等比：dx=${dx} dy=${dy}`);
  assert.ok(b.x > a.x && c.y > a.y, '投影必须保持方向（x 更大 → 屏幕 x 更大）');
});

test('【定量 D】内容宽高比越接近视口，填充率越高（宽视口下圆形内容两侧留白更大）', () => {
  const scene = demoScene();
  const b = bounds(scene);
  const contentAspect = (b.maxX - b.minX) / (b.maxY - b.minY);
  const fill = (W, H) => {
    const project = makeProjector(scene, W, H);
    const box = contentBox(scene, project);
    const limits = { w: W - 96 - clusterPixelRadius(30) * 2, h: H - 96 - clusterPixelRadius(30) * 2 };
    // 填充率按"可用区域"算：等比映射下短边方向必然被吃满。
    return Math.max(box.width / limits.w, box.height / limits.h);
  };
  // 与内容同宽高比的视口应比"极端宽视口"填得更满。
  const matched = contentAspect > 0 ? 1200 * contentAspect : 1200;
  const fMatched = fill(Math.round(matched), 1200);
  const fWide = fill(Math.round(matched * 3), 1200);
  assert.ok(
    fMatched >= fWide - 1e-9,
    `宽高比接近时应填充更多：matched=${fMatched.toFixed(3)} wide=${fWide.toFixed(3)}`,
  );
});

// ---------------------------------------------------------------------------
// 性能：3000 节点标签避让 ≤ 200ms
// ---------------------------------------------------------------------------

test('【性能】3000 节点标签避让 ≤ 200ms（纯函数，无 DOM 测量）', () => {
  const items = [];
  for (let i = 0; i < 3000; i++) {
    // 蜂窝式密排：最坏情况（几乎每个标签都要试满候选位）。
    const col = i % 60;
    const row = Math.floor(i / 60);
    items.push({
      id: `n${i}`,
      x: 40 + col * 22,
      y: 40 + row * 22,
      r: 6,
      degree: i % 17,
      text: `节点 ${i}`,
    });
  }
  const t0 = performance.now();
  const out = layoutLabels(items, { width: 1400, height: 1200 });
  const ms = performance.now() - t0;
  assert.equal(out.total, 3000);
  assert.ok(ms <= 200, `3000 节点标签避让耗时 ${ms.toFixed(1)}ms > 200ms`);
  // 顺带守住信息量：密排下也不该全丢。
  assert.ok(out.shown > 0, '不能把所有标签都藏起来');
});

// ---------------------------------------------------------------------------
// 最小 DOM 打桩（跑 renderSVG）
// ---------------------------------------------------------------------------

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

function renderDemoSVG(scene, width, height) {
  globalThis.document = { createElementNS: (_ns, name) => new FakeElement(name) };
  const svg = new FakeElement('svg');
  renderSVG(svg, scene, { width, height });
  return svg;
}
