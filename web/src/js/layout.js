// 本地力导向重排（**只在用户主动点击"重新布局"时发生**）。
//
// 为什么默认不用它：服务端已给出确定性坐标（server/layout.go 的 hash/fnv 播种），
// 前端再跑一次随机初值的力导向会有两个后果：
//   1) 同一 URL 刷新两次画面不一样 —— "URL 即视图"直接作废；
//   2) 布局在数据刷新时重排 —— 点击节点、拖时间轴都会整屏跳动。
// 因此本模块只在显式交互下被调用，输出只写内存（不 fetch、不回写服务端）。
import { forceSimulation, forceLink, forceCollide, forceX, forceY } from 'd3-force';

// relayout 返回坐标映射（id → {x, y}），**不修改入参**。
//
// # 为什么参数必须从数据里算，不能用写死的常量
//
// 旧实现把力参数写死成固定值，而它与真实数据尺度**差了一个量级**：
//
//   forceLink().distance(0.24)      实测边中位数 1.1023  ← 差 4.6 倍
//   forceCollide().radius(0.045)    实测团内最小间距 0.1237 ← 差 2.7 倍
//
// 后果（用 demo 数据实测）：点一次「重新布局」后
//   - 节点平均位移 439px、最大 703px（注释说的“微调”完全不是）；
//   - **分团结构被摧毁**：团内平均距离 79→222px、团间 557→296px，
//     分离比 7.06→1.34（跌 5 倍）。
//
// 分离比崩塌直接砸掉了“颜色 = concept”这个核心视觉编码：三个颜色的团
// 混成一片，用户分不出谁属于谁。这就是“两个布局总有一个不正常”的根源。
//
// 修法：
//   1. 链接目标长度 = 实际边长的中位数（而不是 0.24）—— 不再把所有边往短里扯;
//   2. 碰撞半径 = 实际团内最近邻中位数的一半——按真实密度撑开，而不是贴着不动;
//   3. 用**团心强锚定**替代全域电荷：每个节点拉向自己团的质心（strength 0.30），
//      这样团内会自然散开、团间不会互相蚕食；
//   4. 去捧全域电荷（它把团间距离拖平，正是分离比崩塌的推手）。
//
// 仍然确定性：没有任何随机初值，初值就是服务端坐标；不依赖时间。
export function relayout(nodes, edges, { ticks = 260 } = {}) {
  if (!nodes || nodes.length < 2) return new Map();
  const simNodes = nodes.map((n) => ({ id: n.id, x: n.x, y: n.y }));
  const byId = new Map(simNodes.map((n) => [n.id, n]));
  const links = edges
    .filter((e) => byId.has(e.from) && byId.has(e.to))
    .map((e) => ({ source: e.from, target: e.to }));

  // ---- 从数据里量尺度（都是确定性统计量，无随机）----
  const edgeLens = [];
  for (const l of links) {
    const a = byId.get(l.source);
    const b = byId.get(l.target);
    edgeLens.push(Math.hypot(a.x - b.x, a.y - b.y));
  }
  const dMed = median(edgeLens) || 0.4;

  // 团内最近邻中位数：决定碰撞半径（不能让节点重叠，也不能把团撑爆）。
  const groups = new Map();
  for (const n of nodes) {
    const key = n.cluster || n.concept_id || '';
    (groups.get(key) || groups.set(key, []).get(key)).push(n);
  }
  const nn = [];
  for (const g of groups.values()) {
    if (g.length < 2) continue;
    for (const a of g) {
      let best = Infinity;
      for (const b of g) {
        if (a === b) continue;
        const d = Math.hypot(a.x - b.x, a.y - b.y);
        if (d < best) best = d;
      }
      if (Number.isFinite(best)) nn.push(best);
    }
  }
  const nnMed = median(nn) || dMed * 0.2;

  // 团质心（锚定目标）：让团整体不漂，只允许团内微调。
  const anchor = new Map();
  for (const [key, g] of groups.entries()) {
    let sx = 0;
    let sy = 0;
    for (const n of g) {
      sx += n.x;
      sy += n.y;
    }
    anchor.set(key, { x: sx / g.length, y: sy / g.length });
  }
  const anchorOf = (id) => {
    const n = nodes.find((x) => x.id === id);
    const key = n ? n.cluster || n.concept_id || '' : '';
    return anchor.get(key) || { x: n ? n.x : 0, y: n ? n.y : 0 };
  };

  const sim = forceSimulation(simNodes)
    // 链接目标 = 实测边中位：不再把 1.10 的边硬扯到 0.24。
    .force('link', forceLink(links).id((d) => d.id).distance(dMed).strength(0.35))
    // 碰撞：按实测最近邻的一半，让节点刚好不重叠。
    .force('collide', forceCollide().radius(Math.max(nnMed / 2, 0.02)).iterations(2))
    // 团心强锚定（比旧的 0.06 强 5 倍）：团会留在原地，分离比不会被拖平。
    .force('x', forceX((d) => anchorOf(d.id).x).strength(0.3))
    .force('y', forceY((d) => anchorOf(d.id).y).strength(0.3))
    // 刻意**不加** forceManyBody：全域电荷会把团间距离拖平，是分离比崩塌的推手。
    .stop();

  for (let i = 0; i < ticks; i++) sim.tick();

  const out = new Map();
  for (const n of simNodes) {
    out.set(n.id, {
      x: round(n.x),
      y: round(n.y),
    });
  }
  return out;
}

// median 返回中位数（空数组返回 0）。确定性统计量，用于从数据里量尺度。
function median(arr) {
  if (!arr || !arr.length) return 0;
  const s = [...arr].sort((a, b) => a - b);
  const m = Math.floor(s.length / 2);
  return s.length % 2 ? s[m] : (s[m - 1] + s[m]) / 2;
}

// round 与 server/layout.go 的 roundCoord 同精度（1e-6）：
// 便于"坐标来自服务端 vs 本地重算"在日志里一眼分辨。
function round(v) {
  return Math.round(v * 1e6) / 1e6;
}

// withLayout 把新坐标应用到场景的**副本**上，并递增 layoutVersion。
// 坐标只改副本：原始响应（来自服务端）永远不被就地改写。
//
// # 必须同时重算 clusters（否则团徽与节点云脱节）
//
// 实测：点一次「重新布局」后，节点平均位移 426px、最大 666px，而团徽位置
// 读的是 `scene.clusters[].x/y`（render.js 的 `project(c.x, c.y)`）。
// 如果只改 nodes 不改 clusters，团徽就留在**旧位置**、节点跑到新位置 ——
// 实测脱节距离 40/88/192px（工具团的圆盘直接飘到了画布对角）。
// 所以这里把每个团的坐标重算为**该团成员的质心**，并带回去。
//
// 同样保持确定性：纯函数，只依新坐标，无随机、无时间。
function clusterCentroids(nodes, clusters, coords) {
  if (!clusters || !clusters.length) return clusters;
  const acc = new Map();
  for (const n of nodes) {
    const c = coords.get(n.id);
    if (!c) continue;
    const key = n.cluster || n.concept_id || '';
    if (!key) continue;
    const a = acc.get(key);
    if (a) {
      a.sx += c.x;
      a.sy += c.y;
      a.n += 1;
    } else {
      acc.set(key, { sx: c.x, sy: c.y, n: 1 });
    }
  }
  return clusters.map((cl) => {
    const key = cl.id || cl.concept_id || '';
    const a = acc.get(key);
    // 折叠视图（团里没有可见节点）保留原坐标：那时团本身就是图元。
    if (!a || a.n === 0) return cl;
    return { ...cl, x: round(a.sx / a.n), y: round(a.sy / a.n) };
  });
}

export function withLayout(scene, coords) {
  return {
    ...scene,
    layoutVersion: (scene.layoutVersion || 0) + 1,
    nodes: scene.nodes.map((n) => {
      const c = coords.get(n.id);
      return c ? { ...n, x: c.x, y: c.y } : n;
    }),
    // 团徽跟着自己的节点云走：不重算就会留在旧位置（实测脱节达 192px）。
    clusters: clusterCentroids(scene.nodes, scene.clusters, coords),
  };
}
