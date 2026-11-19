// 本地力导向重排（**只在用户主动点击"重新布局"时发生**）。
//
// 为什么默认不用它：服务端已给出确定性坐标（server/layout.go 的 hash/fnv 播种），
// 前端再跑一次随机初值的力导向会有两个后果：
//   1) 同一 URL 刷新两次画面不一样 —— "URL 即视图"直接作废；
//   2) 布局在数据刷新时重排 —— 点击节点、拖时间轴都会整屏跳动。
// 因此本模块只在显式交互下被调用，输出只写内存（不 fetch、不回写服务端）。
import { forceSimulation, forceLink, forceManyBody, forceCollide, forceX, forceY } from 'd3-force';

// relayout 返回坐标映射（id → {x, y}），**不修改入参**。
//
// 用服务端坐标做初值（而不是随机初值）：这样一次迭代就能收敛到接近原图的形状，
// 视觉上是"微调"而不是"换一张图"。
export function relayout(nodes, edges, { ticks = 220 } = {}) {
  const simNodes = nodes.map((n) => ({ id: n.id, x: n.x, y: n.y }));
  const byId = new Map(simNodes.map((n) => [n.id, n]));
  const links = edges
    .filter((e) => byId.has(e.from) && byId.has(e.to))
    .map((e) => ({ source: e.from, target: e.to }));

  const sim = forceSimulation(simNodes)
    .force('link', forceLink(links).id((d) => d.id).distance(0.24).strength(0.35))
    .force('charge', forceManyBody().strength(-0.09).distanceMax(1.6))
    .force('collide', forceCollide().radius(0.045))
    // 弱锚定：把节点轻轻拉回服务端给出的团心附近，避免整图漂移成一片。
    .force('x', forceX((d) => anchorOf(nodes, d.id).x).strength(0.06))
    .force('y', forceY((d) => anchorOf(nodes, d.id).y).strength(0.06))
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

function anchorOf(nodes, id) {
  const n = nodes.find((x) => x.id === id);
  return { x: n ? n.x : 0, y: n ? n.y : 0 };
}

// round 与 server/layout.go 的 roundCoord 同精度（1e-6）：
// 便于"坐标来自服务端 vs 本地重算"在日志里一眼分辨。
function round(v) {
  return Math.round(v * 1e6) / 1e6;
}

// withLayout 把新坐标应用到场景的**副本**上，并递增 layoutVersion。
// 坐标只改副本：原始响应（来自服务端）永远不被就地改写。
export function withLayout(scene, coords) {
  return {
    ...scene,
    layoutVersion: (scene.layoutVersion || 0) + 1,
    nodes: scene.nodes.map((n) => {
      const c = coords.get(n.id);
      return c ? { ...n, x: c.x, y: c.y } : n;
    }),
  };
}
