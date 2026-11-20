// 坐标换算：世界坐标（服务端给的 ~1 量级）→ 屏幕像素。
//
// 抽成纯函数是为了让渲染分档、折叠团位置这些几何结论可以被 node --test 覆盖，
// 不必起浏览器。

// bounds 返回场景内容的包围盒（含半径，避免边缘节点被裁掉）。
export function bounds(scene) {
  let minX = Infinity;
  let minY = Infinity;
  let maxX = -Infinity;
  let maxY = -Infinity;
  const consider = (x, y, r) => {
    if (!Number.isFinite(x) || !Number.isFinite(y)) return;
    if (x - r < minX) minX = x - r;
    if (y - r < minY) minY = y - r;
    if (x + r > maxX) maxX = x + r;
    if (y + r > maxY) maxY = y + r;
  };
  for (const n of scene.nodes) consider(n.x, n.y, 0.02);
  for (const c of scene.clusters) consider(c.x, c.y, 0.09);
  if (!Number.isFinite(minX)) return { minX: -0.5, minY: -0.5, maxX: 0.5, maxY: 0.5 };
  if (maxX - minX < 1e-6) {
    minX -= 0.5;
    maxX += 0.5;
  }
  if (maxY - minY < 1e-6) {
    minY -= 0.5;
    maxY += 0.5;
  }
  return { minX, minY, maxX, maxY };
}

// clusterPixelRadius 是团圆在**屏幕上的**半径（像素）：只吃 count，与缩放无关。
//
// 单独抽出来的理由：视口适配必须知道"内容真正画多大"。旧实现只按世界坐标的
// 固定半径（节点 0.02、团 0.09）估算包围盒，而团圆实际有 10~40px、节点最大
// 22px —— 缩放一大，真实图形就会越过估算边界，于是内容贴边甚至被裁掉，
// 而留白全堆在另一侧（用户看到的"左上方大片空白"）。
export function clusterPixelRadius(count) {
  return 10 + Math.min(30, Math.sqrt(Math.max(1, count)) * 3.2);
}

// drawExtent 返回所有图元中最大的**屏幕**半径（像素），用于给缩放留出余量。
export function drawExtent(scene) {
  let r = 0;
  for (const n of scene.nodes) {
    const nr = Number.isFinite(n.r) ? n.r : 5;
    if (nr > r) r = nr;
  }
  for (const c of scene.clusters || []) {
    const cr = clusterPixelRadius(c.count);
    if (cr > r) r = cr;
  }
  // 描边/红描边是画在圆外的（2.5px），多留 3px 免得压到 padding 边上。
  return r + 3;
}

// makeProjector 构造 世界→屏幕 的等比映射（padding 像素内边距）。
//
// 两个不变量（都由 geometry.test.mjs 钉死）：
//  1. **等比**：x、y 用同一个 scale，图形不变形；
//  2. **居中且不越界**：内容按世界包围盒中心对齐视口中心，并且把
//     drawExtent(scene)（图元自己的像素半径）从可用宽高里扣掉，
//     因此边缘图元整个落在画布内 —— 左右/上下留白自然对称。
//
// 为什么不拉伸到"填满视口"：非等比缩放会把圆压成椭圆、把团心几何关系改掉，
// 视觉编码（大小 = degree）随之失真。宽视口下圆形内容两侧必然留白，这是
// 等比映射的代价，不是缺陷；能改进的是"别把内容顶到边上、别一侧紧一侧空"。
export function makeProjector(scene, width, height, padding = 48) {
  const b = bounds(scene);
  const dx = b.maxX - b.minX;
  const dy = b.maxY - b.minY;
  // 可用区域先扣 padding，再扣图元最大像素半径（它不随缩放变化）。
  const pad = Math.max(0, padding);
  const ext = Math.min(drawExtent(scene), Math.min(width, height) / 4);
  const w = Math.max(1, width - pad * 2 - ext * 2);
  const h = Math.max(1, height - pad * 2 - ext * 2);
  const scale = Math.min(w / dx, h / dy);
  const cx = (b.minX + b.maxX) / 2;
  const cy = (b.minY + b.maxY) / 2;
  const project = (x, y) => ({
    x: width / 2 + (x - cx) * scale,
    y: height / 2 + (y - cy) * scale,
  });
  project.scale = scale;
  return project;
}

// contentBox 返回内容在屏幕上的实际包围盒（含图元半径）。
//
// 与 bounds 的区别：这是**画完真的占了多少像素**，用于断言"居中/不越界/填充率"。
export function contentBox(scene, project) {
  let minX = Infinity;
  let minY = Infinity;
  let maxX = -Infinity;
  let maxY = -Infinity;
  const consider = (x, y, r) => {
    if (x - r < minX) minX = x - r;
    if (y - r < minY) minY = y - r;
    if (x + r > maxX) maxX = x + r;
    if (y + r > maxY) maxY = y + r;
  };
  for (const n of scene.nodes) {
    const p = project(n.x, n.y);
    if (!Number.isFinite(p.x) || !Number.isFinite(p.y)) continue;
    consider(p.x, p.y, Number.isFinite(n.r) ? n.r : 5);
  }
  for (const c of scene.clusters || []) {
    const p = project(c.x, c.y);
    if (!Number.isFinite(p.x) || !Number.isFinite(p.y)) continue;
    consider(p.x, p.y, clusterPixelRadius(c.count));
  }
  if (!Number.isFinite(minX)) return null;
  return { minX, minY, maxX, maxY, width: maxX - minX, height: maxY - minY };
}

// ===========================================================================
// 默认布局的最后一环：确定性碰撞松弛 + 团徽让位
//
// # 为什么需要松弛（用户报的"默认有重叠，点一次重新布局才好"）
//
// 节点半径是**像素**（model.nodeRadius，不随缩放变化），而世界坐标间距随视口
// 缩放线性变化。实测 demo 数据 900×700 下 scale≈218px/world，Acme Corp ↔ Atlas
// 屏幕距 23.0px < 半径和 24.4px —— 窗口一小就必然叠。手点一次"重新布局"能好，
// 是因为 layout.js 里的 d3-force 有 forceCollide；但那是随机初值的力导向、
// 每次结果不同，破坏"URL 即视图"（TestCoordsDeterministic 守着服务端那一半）。
// 所以正确做法不是让默认就跑力导向，而是给默认坐标补一步**确定性**松弛：
// 只把叠在一起的圆心推开，不换布局。
//
// # 确定性是怎么保证的（第一优先级）
//
//   - 不带随机数：没有 Math.random，也没有 d3 的随机初值；
//   - 顺序固定：节点按 id 升序处理，每对只处理一次（i<j）；
//   - 迭代次数固定（COLLIDE.iterations），无重叠就提前收敛；
//   - 两心完全重合时，推开方向由 (i, j) 下标派生（黄金角），不是随机方向。
// 因此同一 (坐标, 视口) 两次调用逐字节一致 —— 见 layout-collision.test.mjs。
//
// # 为什么在"世界坐标"上松弛，却按"像素"判重叠
//
// 投影是等比缩放且不旋转，所以"屏幕圆不相交"等价于"世界坐标距 ≥ (ri+rj)/scale"。
// 在世界坐标上改的好处：下游（project / hitTest / 边端点 / 标签避让）一行都不用
// 改，只要场景里的 x/y 已经是松弛后的值。
// ===========================================================================

// COLLIDE 是松弛的固定参数（全是常量，没有任何随机/时间输入）。
export const COLLIDE = {
  // gap：两圆之间额外留的像素（=0 也保证不叠，留 4px 免得看起来"贴边"）。
  gap: 4,
  // iterations：固定迭代轮数上限（验收要求 ≤30）。
  iterations: 12,
  // maxGrowth：内容包围盒允许的最大增长（验收要求 ≤15%）。
  // 超过就回到"上一轮仍在预算内"的状态 —— 宁可留一点重叠，也不要把图"推开成一条"。
  maxGrowth: 0.15,
  // passes：松弛会让包围盒略涨 ⇒ 缩放略降 ⇒ 像素间距再缩水一丁点，
  // 所以按新尺寸再松一轮。固定 2 轮（不是自适应循环，保持可预测）。
  passes: 2,
};

// roundWorld 固定到 1e-6（与 server/layout.go 的 roundCoord 同精度：
// 便于在日志里一眼分辨"坐标来自服务端"还是"前端松弛过"）。
function roundWorld(v) {
  return Math.round(v * 1e6) / 1e6;
}

// gridKey 是空间网格的桶键。用字符串而不是复合数字键：坐标可能为负，
// 数字键要用 cell 范围做乘法，容易写错；桶数是 O(n)，拼接足够快。
function gridKey(gx, gy) {
  return gx + ',' + gy;
}

// centerBBox 返回一组点（{x,y}）的包围盒。
function centerBBox(list) {
  let minX = Infinity;
  let minY = Infinity;
  let maxX = -Infinity;
  let maxY = -Infinity;
  for (const p of list) {
    if (p.x < minX) minX = p.x;
    if (p.y < minY) minY = p.y;
    if (p.x > maxX) maxX = p.x;
    if (p.y > maxY) maxY = p.y;
  }
  return { minX, minY, maxX, maxY };
}

// bboxGrowth 返回 cur 相对 base 的相对增长（宽、高取较大者）。
function bboxGrowth(base, cur) {
  const w0 = base.maxX - base.minX;
  const h0 = base.maxY - base.minY;
  const w = cur.maxX - cur.minX;
  const h = cur.maxY - cur.minY;
  const gw = w0 > 1e-12 ? (w - w0) / w0 : 0;
  const gh = h0 > 1e-12 ? (h - h0) / h0 : 0;
  return Math.max(gw, gh);
}

// collideOnce 把仍重叠的圆心推开（就地修改 items），同时守住包围盒增长预算。
//
// 每轮迭代重建一次网格：Gauss-Seidel 会在迭代中移动点，网格必须跟着更新，
// 否则邻居查找会漏掉已经挪到别的格子的点。建一次网格是 O(n)，总代价 O(k*n)。
//
// # 为什么超预算时不是整轮回滚
//
// 实测（3000 节点密集图）：第一轮就把包围盒撑过 15%，若直接丢弃整轮，
// 结果就是"一个节点都不动"——松弛静默失效、重叠原封不动（比不写还差，
// 因为它看着像做了事）。正确做法是把位移**按剩余预算缩放**：在"上一轮
// 合法状态"与"本轮结果"之间做确定性的线性插值，把增长恰好用满就停。
// 增长对位移近似线性，所以这一步既确定、又能最大限度消除重叠。
function collideOnce(items, cell, budget, iterations) {
  const start = items.map((it) => ({ x: it.x, y: it.y }));
  let accepted = items.map((it) => ({ x: it.x, y: it.y }));
  let acceptedGrowth = budget ? bboxGrowth(budget.base, centerBBox(items)) : 0;
  for (let iter = 0; iter < iterations; iter++) {
    const grid = new Map();
    for (let i = 0; i < items.length; i++) {
      const it = items[i];
      const k = gridKey(Math.floor(it.x / cell), Math.floor(it.y / cell));
      const bucket = grid.get(k);
      if (bucket) bucket.push(i);
      else grid.set(k, [i]);
    }
    let passMoved = false;
    for (let i = 0; i < items.length; i++) {
      const a = items[i];
      const gx = Math.floor(a.x / cell);
      const gy = Math.floor(a.y / cell);
      for (let ox = -1; ox <= 1; ox++) {
        for (let oy = -1; oy <= 1; oy++) {
          const bucket = grid.get(gridKey(gx + ox, gy + oy));
          if (!bucket) continue;
          for (const j of bucket) {
            if (j <= i) continue; // 每对只处理一次（i<j），不重复推两遍
            const b = items[j];
            let dx = b.x - a.x;
            let dy = b.y - a.y;
            let d = Math.hypot(dx, dy);
            const need = a.r + b.r;
            if (d >= need) continue;
            if (d < 1e-12) {
              // 两心完全重合：方向由下标派生（确定性，不用随机数）。
              const ang = (i + 1) * 2.399963229728653 + j;
              dx = Math.cos(ang);
              dy = Math.sin(ang);
              d = 1;
            }
            const push = (need - d) / 2;
            const ux = (dx / d) * push;
            const uy = (dy / d) * push;
            a.x -= ux;
            a.y -= uy;
            b.x += ux;
            b.y += uy;
            passMoved = true;
          }
        }
      }
    }
    if (!passMoved) break; // 收敛：当前状态就是最终结果
    if (!budget) {
      accepted = items.map((it) => ({ x: it.x, y: it.y }));
      continue;
    }
    const g = bboxGrowth(budget.base, centerBBox(items));
    if (g > budget.maxGrowth + 1e-12) {
      // 超预算：在"上一轮合法态"与"本轮结果"之间插值，把增长恰好用到预算上限。
      const span = g - acceptedGrowth;
      const f = span > 1e-12 ? (budget.maxGrowth - acceptedGrowth) / span : 0;
      const t = Math.max(0, Math.min(1, f));
      for (let i = 0; i < items.length; i++) {
        items[i].x = accepted[i].x + (items[i].x - accepted[i].x) * t;
        items[i].y = accepted[i].y + (items[i].y - accepted[i].y) * t;
      }
      break;
    }
    accepted = items.map((it) => ({ x: it.x, y: it.y }));
    acceptedGrowth = g;
  }
  // 最终是否真的有位移：与进函数时的坐标逐点比对（避免"看着做了事其实没动"）。
  for (let i = 0; i < items.length; i++) {
    if (Math.abs(items[i].x - start[i].x) > 1e-12 || Math.abs(items[i].y - start[i].y) > 1e-12) return true;
  }
  return false;
}

// fitScene 返回**松弛后**的场景副本（不改入参、不动 edges/clusters）。
//
// 只有投影后真的会重叠的节点才会位移；其余节点的 x/y 一个数都不动
// （同一对象引用也尽量保持 —— 没有位移时直接返回入参本身）。
export function fitScene(scene, width, height, opts = {}) {
  const nodes = scene && Array.isArray(scene.nodes) ? scene.nodes : [];
  if (nodes.length < 2) return scene;
  const gap = Number.isFinite(opts.gap) ? opts.gap : COLLIDE.gap;
  const iterations = Number.isFinite(opts.iterations) ? opts.iterations : COLLIDE.iterations;
  const maxGrowth = Number.isFinite(opts.maxGrowth) ? opts.maxGrowth : COLLIDE.maxGrowth;
  const passes = Number.isFinite(opts.passes) ? opts.passes : COLLIDE.passes;

  const finite = nodes.filter((n) => Number.isFinite(n.x) && Number.isFinite(n.y));
  if (finite.length < 2) return scene;
  // 增长基准按**原始**场景算：每轮都对着它，累积增长才受控。
  const baseBox = centerBBox(finite);

  let cur = scene;
  let changed = false;
  for (let p = 0; p < passes; p++) {
    const scale = makeProjector(cur, width, height).scale;
    if (!(scale > 0) || !Number.isFinite(scale)) break;
    // 像素半径 + 半个像素间隙 → 世界半径；这样"不叠"的判据就是 d ≥ ra+rb。
    const items = cur.nodes
      .filter((n) => Number.isFinite(n.x) && Number.isFinite(n.y))
      .map((n) => ({
        id: String(n.id),
        x: n.x,
        y: n.y,
        r: ((Number.isFinite(n.r) ? n.r : 5) + gap / 2) / scale,
      }))
      .sort((a, b) => (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));
    if (items.length < 2) break;
    let maxR = 0;
    for (const it of items) if (it.r > maxR) maxR = it.r;
    // 网格边长 ≥ 2*maxR ⇒ 半径内的邻居必落在相邻 3×3 格内（不会漏查）。
    const cell = Math.max(2 * maxR, 1e-6);
    if (!collideOnce(items, cell, { base: baseBox, maxGrowth }, iterations)) break; // 已无重叠
    const byId = new Map(items.map((it) => [it.id, it]));
    cur = {
      ...cur,
      nodes: cur.nodes.map((n) => {
        const it = byId.get(String(n.id));
        if (!it) return n;
        const x = roundWorld(it.x);
        const y = roundWorld(it.y);
        return x === n.x && y === n.y ? n : { ...n, x, y };
      }),
    };
    changed = true;
  }
  return changed ? cur : scene;
}

// ===========================================================================
// 团徽让位：概览下团标签不再压在"团心那个节点"上
//
// 根因：服务端旧叶序 r = R*sqrt(index/count) 在 index=0 时恒为 0 ⇒ 团内第一个
// 节点必然落在团心；而团圆 + 团名就画在团心（render.js 的 translate(p.x,p.y)），
// 于是那个节点永远被团圆盖住（demo 三个团全中）。
//
// 服务端已改成离心叶序（不再"必然"），但第一个节点离团心只有 R*sqrt(0.5/n)，
// 投影后仍可能小于团徽半径（16~24px）。所以这里再补一层保险：团徽整组
// （圆盘 + 文字）挪到**本团节点云的外侧**，且与任何节点圆不相交。
// 折叠视图（团里没有可见节点）保持原来的"圆心 + 下方文字"，行为不变。
//
// 纯函数：方向按固定序列试，先试正上方；都不行就取"离节点圆最远"的那个方向。
// 没有随机数，同一输入每次落点一致。
// ===========================================================================

// BADGE_DIRECTIONS 是候选方向（度，-90 = 正上方）。顺序即优先级。
const BADGE_DIRECTIONS = [-90, -50, -130, -10, -170, 30, 210, 90, 10, 170, 50, 130];

// placeClusterBadges 返回 团键 → { x, y, r, labelY, above } 的**屏幕**位置表。
//
// labelY 是团名文字的基线 y；above=true 表示文字画在圆盘上方（远离云的一侧）。
export function placeClusterBadges(scene, project, { width = 0, height = 0, gap = 6 } = {}) {
  const out = new Map();
  const clusters = (scene && scene.clusters) || [];
  if (!clusters.length) return out;
  const nodes = (scene && scene.nodes) || [];

  const pts = [];
  let maxNodeR = 0;
  for (const n of nodes) {
    const p = project(n.x, n.y);
    if (!Number.isFinite(p.x) || !Number.isFinite(p.y)) continue;
    const r = Number.isFinite(n.r) ? n.r : 5;
    if (r > maxNodeR) maxNodeR = r;
    pts.push({ x: p.x, y: p.y, r, key: n.key });
  }
  const cell = Math.max(2 * (maxNodeR + gap), 16);
  const grid = new Map();
  for (const q of pts) {
    const k = gridKey(Math.floor(q.x / cell), Math.floor(q.y / cell));
    const bucket = grid.get(k);
    if (bucket) bucket.push(q);
    else grid.set(k, [q]);
  }
  const clear = (x, y, r) => {
    const gx = Math.floor(x / cell);
    const gy = Math.floor(y / cell);
    for (let ox = -1; ox <= 1; ox++) {
      for (let oy = -1; oy <= 1; oy++) {
        const bucket = grid.get(gridKey(gx + ox, gy + oy));
        if (!bucket) continue;
        for (const q of bucket) {
          if (Math.hypot(q.x - x, q.y - y) < r + q.r + 2) return false;
        }
      }
    }
    return true;
  };

  const placed = [];
  for (const c of clusters) {
    const key = c.id || c.concept_id || '';
    const cp = project(c.x, c.y);
    const r = clusterPixelRadius(c.count);
    // 本团节点云的外沿（世界坐标 → 屏幕像素）。
    let cloudR = 0;
    for (const q of pts) {
      if (q.key !== key) continue;
      const d = Math.hypot(q.x - cp.x, q.y - cp.y) + q.r;
      if (d > cloudR) cloudR = d;
    }
    if (cloudR <= 0) {
      // 折叠视图：团本身就是图元，保持原来的"圆心 + 下方文字"。
      out.set(key, { x: cp.x, y: cp.y, r, labelY: cp.y + r + 13, above: false });
      placed.push({ x: cp.x, y: cp.y, r });
      continue;
    }
    let best = null;
    for (let ring = 0; ring <= 4 && !best; ring++) {
      const dist = cloudR + r + gap + ring * (r + gap);
      for (const deg of BADGE_DIRECTIONS) {
        const ang = (deg * Math.PI) / 180;
        const x = cp.x + Math.cos(ang) * dist;
        const y = cp.y + Math.sin(ang) * dist;
        // 圆盘必须整个落在画布内（否则宁可选别的方向）。
        if (width > 0 && (x - r < 0 || x + r > width)) continue;
        if (height > 0 && (y - r < 0 || y + r > height)) continue;
        if (!clear(x, y, r)) continue;
        let clash = false;
        for (const q of placed) {
          if (Math.hypot(q.x - x, q.y - y) < r + q.r + gap) {
            clash = true;
            break;
          }
        }
        if (clash) continue;
        best = { x, y };
        break;
      }
    }
    if (!best) {
      // 兜底：取"离最近节点圆最远"的方向（仍是确定性选择）。
      let bestScore = -Infinity;
      for (const deg of BADGE_DIRECTIONS) {
        const ang = (deg * Math.PI) / 180;
        const x = cp.x + Math.cos(ang) * (cloudR + r + gap);
        const y = cp.y + Math.sin(ang) * (cloudR + r + gap);
        let clearance = Infinity;
        for (const q of pts) {
          clearance = Math.min(clearance, Math.hypot(q.x - x, q.y - y) - (r + q.r));
        }
        const score = Number.isFinite(clearance) ? clearance : 0;
        if (score > bestScore) {
          bestScore = score;
          best = { x, y };
        }
      }
      if (!best) best = { x: cp.x, y: cp.y };
    }
    const above = best.y <= cp.y;
    out.set(key, {
      x: best.x,
      y: best.y,
      r,
      labelY: above ? best.y - r - 6 : best.y + r + 13,
      above,
    });
    placed.push({ x: best.x, y: best.y, r });
  }
  return out;
}

// shouldUseCanvas 是渲染分档判定（TD §9：SVG ≤ 2k，超过切 Canvas）。
//
// 判决依据是**实际要画的图元数**（节点 + 折叠团），不是 node_count：
// overview 下 node_count 可能上千但 rendered_nodes 为 0（全进 clusters），
// 那时画的是几个团，用 Canvas 反而看不清标签。
export function shouldUseCanvas(scene, maxSvg = 2000) {
  return scene.nodes.length + scene.clusters.length > maxSvg;
}
