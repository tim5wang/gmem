// 默认布局重叠修复的定量验收（根因 A / A 补充 / B / C / D + 性能）。
//
// 为什么必须是断言而不是截图：用户报的"默认有重叠，点一次重新布局才好"是
// **几何**问题，可量化。截图只能证明"这一次看着还行"，而这里每个数字都会在
// 改一个系数后悄悄退化：
//   - 叶序离心项被改回 index/count → 团内第一个节点又落回团心；
//   - 碰撞松弛被删掉 → 窄视口下节点重新叠在一起；
//   - 松弛引入随机数 → 同 URL 刷新两次画面不同（"URL 即视图"作废）；
//   - 松弛把图推开成一条 → 包围盒变形超 15%。
import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

import { buildScene, nodeRadius } from '../js/model.js';
import { makeProjector, fitScene, contentBox, placeClusterBadges, COLLIDE } from '../js/geometry.js';
import { relayout, withLayout } from '../js/layout.js';

const demoResp = JSON.parse(
  readFileSync(new URL('./fixtures/demo-graph.json', import.meta.url), 'utf8'),
);

function demoScene() {
  return buildScene(demoResp);
}

// 三个必须覆盖的视口：900×700 是当前能复现重叠的最严视口（scale 最小）。
const VIEWPORTS = [
  [1400, 1200],
  [1100, 800],
  [900, 700],
];

// overlapPairs 返回屏幕圆相交的节点对（d < r1 + r2）。
function overlapPairs(scene, project, tol = 1e-9) {
  const out = [];
  for (let i = 0; i < scene.nodes.length; i++) {
    for (let j = i + 1; j < scene.nodes.length; j++) {
      const a = project(scene.nodes[i].x, scene.nodes[i].y);
      const b = project(scene.nodes[j].x, scene.nodes[j].y);
      const d = Math.hypot(a.x - b.x, a.y - b.y);
      const need = scene.nodes[i].r + scene.nodes[j].r;
      if (d < need - tol) out.push([scene.nodes[i].id, scene.nodes[j].id, d, need]);
    }
  }
  return out;
}

// 密度最高的两个节点之间的间隙（像素，负数 = 重叠深度）。
function worstGap(scene, project) {
  let worst = Infinity;
  for (let i = 0; i < scene.nodes.length; i++) {
    for (let j = i + 1; j < scene.nodes.length; j++) {
      const a = project(scene.nodes[i].x, scene.nodes[i].y);
      const b = project(scene.nodes[j].x, scene.nodes[j].y);
      const gap = Math.hypot(a.x - b.x, a.y - b.y) - (scene.nodes[i].r + scene.nodes[j].r);
      if (gap < worst) worst = gap;
    }
  }
  return worst;
}

// ===========================================================================
// 根因 A：叶序离心（服务端公式的**结果**在夹具里必须成立）
// ===========================================================================

test('【根因 A】夹具里没有任何节点落在团心（历史缺陷会让三个团的第一个节点 dist=0）', () => {
  const scene = demoScene();
  assert.ok(scene.clusters.length > 0, '夹具必须有团');
  let zeroDist = 0;
  for (const c of scene.clusters) {
    const own = scene.nodes.filter((n) => n.key === (c.id || c.concept_id));
    assert.ok(own.length > 0, `团 ${c.id} 必须有节点`);
    for (const n of own) {
      const d = Math.hypot(n.x - c.x, n.y - c.y);
      if (d <= 0) zeroDist++;
    }
  }
  assert.equal(zeroDist, 0, `有 ${zeroDist} 个节点恰好落在团心（会被团标签压住）`);
});

test('【根因 A】团内第一个节点离团心的距离 > 0（离心项生效的直接证据）', () => {
  const scene = demoScene();
  for (const c of scene.clusters) {
    const own = scene.nodes.filter((n) => n.key === (c.id || c.concept_id));
    const ds = own.map((n) => Math.hypot(n.x - c.x, n.y - c.y)).sort((a, b) => a - b);
    assert.ok(ds[0] > 0, `团 ${c.id} 的最近节点仍落在团心：dist=${ds[0]}`);
    // 离心叶序的最近一环是 R*sqrt(0.5/n)；这里只断言"明显离开团心"，
    // R 与 n 的具体取值由服务端 layout_internal_test.go 精确钉死。
    assert.ok(ds[0] >= 0.04, `团 ${c.id} 最近节点 dist=${ds[0].toFixed(6)} 太小（< 0.04）`);
  }
});

// ===========================================================================
// 根因 A 补充：团徽（圆盘 + 团名）不得压住任何节点圆
// ===========================================================================

function badgeNodeHits(scene, project, width, height) {
  const badges = placeClusterBadges(scene, project, { width, height });
  const hits = [];
  for (const [key, b] of badges) {
    for (const n of scene.nodes) {
      const p = project(n.x, n.y);
      const d = Math.hypot(p.x - b.x, p.y - b.y);
      if (d < b.r + n.r) hits.push([key, n.id, d, b.r + n.r]);
    }
  }
  return hits;
}

test('【根因 A 补充】三视口下团徽圆与任何节点圆都不相交', () => {
  for (const [W, H] of VIEWPORTS) {
    const scene = fitScene(demoScene(), W, H);
    const project = makeProjector(scene, W, H);
    const hits = badgeNodeHits(scene, project, W, H);
    assert.equal(
      hits.length, 0,
      `${W}×${H}: 团徽压住了节点 ${JSON.stringify(hits.slice(0, 5))}`,
    );
  }
});

test('【根因 A 补充】敌意输入：节点恰好落在团心时，团徽也必须自动让位', () => {
  // 手搓"第一个节点就在团心"的场景（等价于旧叶序的输出）。
  // 这不依赖服务端改动：即使坐标退化，前端也不允许团徽盖住节点。
  const scene = buildScene({
    meta: { mode: 'overview', truncated: false, node_count: 3 },
    nodes: [
      { id: 'a', canonical_name: 'A', concept_id: 'c1', concept_status: 'certified', degree: 2, depth: 0, matched: false, cluster: 'c1', x: 0, y: 0 },
      { id: 'b', canonical_name: 'B', concept_id: 'c1', concept_status: 'certified', degree: 2, depth: 0, matched: false, cluster: 'c1', x: 0.1, y: 0 },
      { id: 'c', canonical_name: 'C', concept_id: 'c1', concept_status: 'certified', degree: 2, depth: 0, matched: false, cluster: 'c1', x: 0, y: 0.1 },
    ],
    edges: [],
    clusters: [{ id: 'c1', concept_id: 'c1', concept_name: '团一', concept_status: 'certified', count: 3, x: 0, y: 0, matched: false }],
  });
  for (const [W, H] of VIEWPORTS) {
    const project = makeProjector(scene, W, H);
    const hits = badgeNodeHits(scene, project, W, H);
    assert.equal(hits.length, 0, `${W}×${H}: 节点在团心时团徽仍压住了它 ${JSON.stringify(hits)}`);
  }
});

// ===========================================================================
// 根因 B / 定量 A：三视口下节点圆不重叠
// ===========================================================================

test('【定量 A】三视口下任意两节点屏幕距 ≥ r₁+r₂（含最严的 900×700）', () => {
  for (const [W, H] of VIEWPORTS) {
    const scene = fitScene(demoScene(), W, H);
    const project = makeProjector(scene, W, H);
    const pairs = overlapPairs(scene, project, 0.01);
    assert.equal(
      pairs.length, 0,
      `${W}×${H} 仍有 ${pairs.length} 对重叠：` +
      JSON.stringify(pairs.slice(0, 5).map(([a, b, d, need]) => `${a}↔${b} ${d.toFixed(1)}<${need.toFixed(1)}`)),
    );
  }
});

test('【定量 A】松弛前的默认坐标在 900×700 确实会重叠（证明这条断言有鉴别力）', () => {
  // 对照组：不跑 fitScene，直接用服务端坐标投影。
  // 注意夹具已按"离心叶序"更新，所以这里把小窗口进一步收紧来复现重叠，
  // 确保断言不是"恒真"。
  const scene = demoScene();
  const project = makeProjector(scene, 900, 700);
  // 人为把视口再缩一半：间距腰斩而像素半径不变 ⇒ 必然叠。
  const tiny = (x, y) => {
    const p = project(x, y);
    return { x: p.x / 2, y: p.y / 2 };
  };
  tiny.scale = project.scale;
  const pairs = overlapPairs(scene, tiny, 0.01);
  assert.ok(pairs.length > 0, '对照组失效：连缩小一半的视口都不重叠，说明重叠断言没有鉴别力');
});

// ===========================================================================
// 定量 B：默认已够好（与"点一次重新布局"的力导向对比）
// ===========================================================================

test('【定量 B】默认与力导向的重叠对数都是 0（默认不需要用户手点）', () => {
  for (const [W, H] of VIEWPORTS) {
    const scene = fitScene(demoScene(), W, H);
    const pDefault = makeProjector(scene, W, H);
    const ovDefault = overlapPairs(scene, pDefault, 0.01);

    const coords = relayout(scene.nodes, scene.edges);
    const forced = withLayout(scene, coords);
    const pForce = makeProjector(forced, W, H);
    const ovForce = overlapPairs(forced, pForce, 0.01);

    assert.equal(ovDefault.length, 0, `${W}×${H} 默认布局仍有重叠：${JSON.stringify(ovDefault.slice(0, 3))}`);
    assert.equal(ovForce.length, 0, `${W}×${H} 力导向布局有重叠：${JSON.stringify(ovForce.slice(0, 3))}`);
  }
});

test('【定量 B】默认的最差间隙已为正，力导向没有"可修的剩余重叠"', () => {
  // 判据不是"两份坐标有多接近"，而是**默认是否已经不需要修**：
  // 若默认的最差间隙 ≥ 0，力导向能修的重叠就是 0 —— 用户点它只是换一种排布，
  // 不是为了消除重叠。（两份坐标的平均位移很大，是因为 d3-force 的 link/charge
  // 参数会把整图摊得更开，属于"另一种构图"，不是"修复缺陷"。）
  for (const [W, H] of VIEWPORTS) {
    const scene = fitScene(demoScene(), W, H);
    const pDefault = makeProjector(scene, W, H);
    const worst = worstGap(scene, pDefault);
    assert.ok(worst >= 0, `${W}×${H} 默认布局最差间隙 ${worst.toFixed(2)}px < 0（仍有重叠）`);
  }
});

// ===========================================================================
// 定量 C：确定性（不含随机数、不依赖输入顺序）
// ===========================================================================

test('【定量 C】同一输入两次 fitScene 逐字节一致（含碰撞松弛后）', () => {
  for (const [W, H] of VIEWPORTS) {
    const a = fitScene(demoScene(), W, H);
    const b = fitScene(demoScene(), W, H);
    assert.deepEqual(
      a.nodes.map((n) => [n.id, n.x, n.y]),
      b.nodes.map((n) => [n.id, n.x, n.y]),
      `${W}×${H}: 两次松弛结果不同（引入了随机数/时间）`,
    );
  }
});

test('【定量 C】节点输入顺序打乱后结果不变（无顺序依赖）', () => {
  const W = 900;
  const H = 700;
  const base = demoScene();
  const shuffled = { ...base, nodes: base.nodes.slice().reverse() };
  const a = fitScene(base, W, H);
  const b = fitScene(shuffled, W, H);
  const byId = new Map(b.nodes.map((n) => [n.id, n]));
  for (const n of a.nodes) {
    const m = byId.get(n.id);
    assert.equal(m.x, n.x, `节点 ${n.id} 的 x 依赖输入顺序`);
    assert.equal(m.y, n.y, `节点 ${n.id} 的 y 依赖输入顺序`);
  }
});

test('【定量 C】fitScene 不修改入参（服务端坐标永不被就地改写）', () => {
  const scene = demoScene();
  const before = scene.nodes.map((n) => [n.id, n.x, n.y]);
  fitScene(scene, 900, 700);
  assert.deepEqual(scene.nodes.map((n) => [n.id, n.x, n.y]), before, 'fitScene 就地改写了坐标');
});

test('【定量 C】完全无重叠时原样返回同一个对象（零漂移、零开销）', () => {
  const scene = demoScene();
  // 1400×1200 下默认坐标本就不重叠 ⇒ 不应产生任何位移。
  const out = fitScene(scene, 1400, 1200);
  const moved = out.nodes.some((n, i) => n.x !== scene.nodes[i].x || n.y !== scene.nodes[i].y);
  assert.equal(moved, false, '无重叠时仍然改动了坐标（会让"URL 即视图"轻微漂移）');
});

// ===========================================================================
// 定量 D：不得挤压变形 / 不得越出 viewBox
// ===========================================================================

test('【定量 D】松弛后内容包围盒变形 ≤ 15%，且整个落在 viewBox 内', () => {
  for (const [W, H] of VIEWPORTS) {
    const raw = demoScene();
    const fitted = fitScene(raw, W, H);
    const b0 = contentBox(raw, makeProjector(raw, W, H));
    const b1 = contentBox(fitted, makeProjector(fitted, W, H));
    const growW = (b1.width - b0.width) / b0.width;
    const growH = (b1.height - b0.height) / b0.height;
    assert.ok(growW <= 0.15 + 1e-9, `${W}×${H} 横向变形 ${(growW * 100).toFixed(1)}% > 15%`);
    assert.ok(growH <= 0.15 + 1e-9, `${W}×${H} 纵向变形 ${(growH * 100).toFixed(1)}% > 15%`);
    assert.ok(b1.minX >= -0.5 && b1.maxX <= W + 0.5, `${W}×${H} 内容水平越出 viewBox：[${b1.minX},${b1.maxX}]`);
    assert.ok(b1.minY >= -0.5 && b1.maxY <= H + 0.5, `${W}×${H} 内容垂直越出 viewBox：[${b1.minY},${b1.maxY}]`);
  }
});

test('【定量 D】密集图：松弛削减重叠，但包围盒增长仍受 15% 预算约束', () => {
  // 造一张"必然重叠"的密集图（同团节点挤在很小的团半径里）。
  const nodes = [];
  const clusters = [];
  const CL = 6;
  const PER = 40;
  for (let c = 0; c < CL; c++) {
    const ang = c * 2.399963229728653;
    const cr = 0.9 * Math.sqrt(c + 0.5);
    clusters.push({
      id: `c-${c}`, concept_id: `c-${c}`, concept_name: `团${c}`,
      concept_status: 'certified', count: PER,
      x: Math.round(cr * Math.cos(ang) * 1e6) / 1e6,
      y: Math.round(cr * Math.sin(ang) * 1e6) / 1e6,
      matched: false,
    });
  }
  for (let c = 0; c < CL; c++) {
    for (let j = 0; j < PER; j++) {
      const k = clusters[c];
      const R = 0.1 + 0.055 * Math.sqrt(PER);
      const rad = R * Math.sqrt((j + 0.5) / PER);
      const a = j * 2.399963229728653;
      nodes.push({
        id: `n-${c}-${String(j).padStart(3, '0')}`,
        canonical_name: `N${c}-${j}`,
        concept_id: k.id, concept_name: k.concept_name, concept_status: 'certified',
        degree: (j * 7) % 12, depth: 0, matched: false, cluster: k.id,
        x: Math.round((k.x + rad * Math.cos(a)) * 1e6) / 1e6,
        y: Math.round((k.y + rad * Math.sin(a)) * 1e6) / 1e6,
      });
    }
  }
  const scene = buildScene({ meta: { mode: 'overview', truncated: false, node_count: nodes.length }, nodes, edges: [], clusters });
  const W = 900;
  const H = 700;
  const before = overlapPairs(scene, makeProjector(scene, W, H), 0.01).length;
  const fitted = fitScene(scene, W, H);
  const after = overlapPairs(fitted, makeProjector(fitted, W, H), 0.01).length;
  assert.ok(before > 0, '对照场景不重叠，这条用例失去意义');
  assert.ok(after < before, `松弛没有削减重叠：${before} → ${after}`);

  const b0 = contentBox(scene, makeProjector(scene, W, H));
  const b1 = contentBox(fitted, makeProjector(fitted, W, H));
  const grow = Math.max((b1.width - b0.width) / b0.width, (b1.height - b0.height) / b0.height);
  assert.ok(grow <= COLLIDE.maxGrowth + 1e-6, `密集图松弛后包围盒增长 ${(grow * 100).toFixed(1)}% 超过预算`);
});

// ===========================================================================
// 性能：2000 节点 ≤ 100ms，3000 节点保持 n log n 量级
// ===========================================================================

function denseScene(n, clusters) {
  const nodes = [];
  const cs = [];
  for (let c = 0; c < clusters; c++) {
    const ang = c * 2.399963229728653;
    const cr = 0.9 * Math.sqrt(c + 0.5);
    cs.push({
      id: `c-${c}`, concept_id: `c-${c}`, concept_name: `团${c}`,
      concept_status: 'certified', count: 0,
      x: Math.round(cr * Math.cos(ang) * 1e6) / 1e6,
      y: Math.round(cr * Math.sin(ang) * 1e6) / 1e6,
      matched: false,
    });
  }
  const per = Math.ceil(n / clusters);
  for (let i = 0; i < n; i++) {
    const c = cs[i % clusters];
    const idx = Math.floor(i / clusters);
    const R = Math.min(0.44, 0.1 + 0.055 * Math.sqrt(per));
    const rad = R * Math.sqrt((idx + 0.5) / per);
    const a = idx * 2.399963229728653;
    nodes.push({
      id: `n-${String(i).padStart(5, '0')}`,
      canonical_name: `N${i}`,
      concept_id: c.id, concept_name: c.concept_name, concept_status: 'certified',
      degree: (i * 7) % 12, depth: 0, matched: false, cluster: c.id,
      x: Math.round((c.x + rad * Math.cos(a)) * 1e6) / 1e6,
      y: Math.round((c.y + rad * Math.sin(a)) * 1e6) / 1e6,
    });
    c.count++;
  }
  return buildScene({ meta: { mode: 'overview', truncated: false, node_count: n }, nodes, edges: [], clusters: cs });
}

test('【性能】2000 节点碰撞松弛 ≤ 100ms（纯计算，无 DOM 测量）', () => {
  const scene = denseScene(2000, 40);
  const t0 = performance.now();
  fitScene(scene, 900, 700);
  const ms = performance.now() - t0;
  assert.ok(ms <= 100, `2000 节点松弛耗时 ${ms.toFixed(1)}ms > 100ms`);
});

test('【性能】3000 节点不爆炸（耗时不随 n 平方增长）', () => {
  const s2000 = denseScene(2000, 40);
  const s3000 = denseScene(3000, 40);
  const time = (scene) => {
    const t0 = performance.now();
    fitScene(scene, 900, 700);
    return performance.now() - t0;
  };
  // 预热一次，避免首次 JIT 影响比较。
  time(denseScene(600, 6));
  const t2000 = time(s2000);
  const t3000 = time(s3000);
  // n 增长 1.5 倍时，n² 会涨 2.25 倍；这里允许 6 倍余量以吸收机器噪声，
  // 但远小于"平方增长 + 噪声"会达到的量级。
  assert.ok(
    t3000 <= Math.max(t2000, 1) * 6,
    `3000 节点耗时 ${t3000.toFixed(1)}ms 相对 2000 节点的 ${t2000.toFixed(1)}ms 增长过快（疑似 O(n²)）`,
  );
  assert.ok(t3000 <= 200, `3000 节点耗时 ${t3000.toFixed(1)}ms 超过 200ms`);
});

// ===========================================================================
// 配套：节点半径只按 degree（视觉编码未变）
// ===========================================================================

test('松弛只改坐标，不改半径/颜色/degree 等视觉编码', () => {
  const scene = demoScene();
  const fitted = fitScene(scene, 900, 700);
  const byId = new Map(fitted.nodes.map((n) => [n.id, n]));
  for (const n of scene.nodes) {
    const m = byId.get(n.id);
    assert.equal(m.r, n.r, `节点 ${n.id} 的半径被改了`);
    assert.equal(m.r, nodeRadius(n.degree), '半径必须仍只由 degree 决定');
    assert.equal(m.key, n.key, '颜色键被改了');
    assert.equal(m.matched, n.matched, '命中标记被改了');
  }
});

// ===========================================================================
// 真实 stage 尺寸（浏览器里 makeProjector 拿到的**不是**窗口尺寸）
//
// 实测（Playwright + 真实页面）：
//   窗口 1400×1200 → stage 840×626
//   窗口 1100×800  → stage 540×626
//   窗口 900×700   → stage 900×280   ← 顶栏吃掉大半高度，scale 只有 53.7px/world
//
// 为什么必须单独钉死：只按"窗口尺寸"算投影会得出过于乐观的结论。
// 900×700 窗口的真实 stage 是 900×280，节点像素半径不变而间距腰斩，
// 这一档才是最该复现"默认重叠"的视口。
// ===========================================================================

const REAL_STAGES = [
  [840, 626], // 窗口 1400×1200
  [540, 626], // 窗口 1100×800
  [900, 280], // 窗口 900×700（最严：顶栏占掉大半高度）
];

test('【真实 stage】团徽压节点必须为 0（三个真实 stage 尺寸）', () => {
  for (const [W, H] of REAL_STAGES) {
    const scene = fitScene(demoScene(), W, H);
    const project = makeProjector(scene, W, H);
    const hits = badgeNodeHits(scene, project, W, H);
    assert.equal(hits.length, 0, `${W}×${H}（真实 stage）团徽压住节点 ${JSON.stringify(hits.slice(0, 3))}`);
  }
});

test('【真实 stage】1400×1200 与 1100×800 窗口下重叠归零', () => {
  for (const [W, H] of [[840, 626], [540, 626]]) {
    const scene = fitScene(demoScene(), W, H);
    const project = makeProjector(scene, W, H);
    const pairs = overlapPairs(scene, project, 0.01);
    assert.equal(pairs.length, 0, `${W}×${H}（真实 stage）仍有 ${pairs.length} 对重叠`);
  }
});

test('【真实 stage】最严的 900×280：重叠至少减半，且增长守住预算', () => {
  // 这一档是"15% 包围盒预算"与"重叠归零"真正冲突的地方：
  // 实测要在这档归零需增长约 34%（违反验收 D 的 15% 上限），所以本用例断言
  //   ①重叠数至少减半；②增长仍守住预算。
  // 冲突本身在交付说明里如实报告，由人决定放宽哪一条。
  const W = 900;
  const H = 280;
  const raw = demoScene();
  const fitted = fitScene(raw, W, H);
  const before = overlapPairs(raw, makeProjector(raw, W, H), 0.01).length;
  const after = overlapPairs(fitted, makeProjector(fitted, W, H), 0.01).length;
  assert.ok(before > 0, '这一档应当能复现重叠（否则用例失效）');
  assert.ok(after <= before / 2, `最严视口重叠只从 ${before} 降到 ${after}（未减半）`);

  const b0 = contentBox(raw, makeProjector(raw, W, H));
  const b1 = contentBox(fitted, makeProjector(fitted, W, H));
  const grow = Math.max((b1.width - b0.width) / b0.width, (b1.height - b0.height) / b0.height);
  assert.ok(grow <= COLLIDE.maxGrowth + 1e-6, `最严视口增长 ${(grow * 100).toFixed(1)}% 超预算`);
});
