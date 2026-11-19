// 视觉/筛选的纯函数层：**无 DOM、无 fetch、无随机**。
//
// 为什么单独一层：验收要求"坐标不被覆盖 / as_of 不触发重排"这类结论必须能被
// 自动检查，而不是靠肉眼。把"数据 → 画面元素"的换算放在这里，node --test 就能
// 直接断言，不需要浏览器。
//
// 硬约束（TD §9「数据接口」）：坐标来自服务端预布局（Node.X / Node.Y），
// **前端默认一个数都不改**。本地重算只发生在用户点"重新布局"时（layout.js），
// 且结果只活在内存里，绝不回写服务端。

// MAX_RENDER_NODES 是 SVG 分档阈值：≤ 该值走 SVG，超过切 Canvas。
export const MAX_RENDER_NODES = 2000;

// 节点半径：按 degree 单调递增，根号压缩避免枢纽节点吃掉整屏。
export function nodeRadius(degree) {
  const d = Number.isFinite(degree) && degree > 0 ? degree : 0;
  return Math.min(22, 5 + Math.sqrt(d) * 3.4);
}

// conceptKey 是聚簇/配色的键：优先用服务端的 cluster（折叠视图里它就是团键），
// 否则退回 concept_id；两者都空 = 未归属概念。
export function conceptKey(node) {
  if (!node) return '';
  return node.cluster || node.concept_id || '';
}

// hashCode 是确定性字符串哈希（djb2）。**不用 Math.random**：
// 同一 concept 在同一台机器上的颜色必须每次一样，否则刷新一次配色全变。
export function hashCode(str) {
  let h = 5381;
  for (let i = 0; i < str.length; i++) {
    h = ((h << 5) + h + str.charCodeAt(i)) >>> 0;
  }
  return h >>> 0;
}

// conceptHue 把 concept 键映射成色相（0..359）。
// 用黄金角步进取色，让相邻哈希值的颜色也可区分。
export function conceptHue(key) {
  if (!key) return 210; // 未归属概念：中性蓝灰
  return (hashCode(key) * 137.508) % 360;
}

export function conceptColor(key, { lightness = 46, saturation = 62 } = {}) {
  return `hsl(${conceptHue(key).toFixed(1)} ${saturation}% ${lightness}%)`;
}

// isDraft 判定"草稿概念"（本体未获认证）→ 画斜纹。
export function isDraft(node) {
  return (node?.concept_status || '').toLowerCase() === 'draft';
}

// isEdgeInvalidAt 判定一条边在给定 as_of 视角下是否"已失效/尚未生效"。
//
// asOf === null 表示"当前视角"：直接用服务端的 invalidated（服务端权威）。
// 给了 asOf 时按世界时间就地推算，这样拖时间轴不必每帧请求服务端。
export function isEdgeInvalidAt(edge, asOf) {
  if (asOf === null || asOf === undefined) return Boolean(edge.invalidated);
  const tValid = Number(edge.t_valid || 0);
  const tInvalid = Number(edge.t_invalid || 0);
  if (tValid > asOf) return true; // 那一刻这条边还没写进来
  if (tInvalid && tInvalid <= asOf) return true; // 那一刻已经失效
  return false;
}

// edgeStyle 是边样式的唯一来源（视觉编码 d/e/f 三项都在这里）。
//
// 关键：invalidated 的边**变淡但绝不返回 null**——历史必须可见（TD §9）。
export function edgeStyle(edge, asOf) {
  const invalid = isEdgeInvalidAt(edge, asOf);
  const possiblySame = Boolean(edge.possibly_same);
  return {
    invalid,
    possiblySame,
    dashed: invalid,
    opacity: invalid ? 0.22 : 1,
    color: possiblySame ? '#1d6fd8' : invalid ? '#8a94a6' : '#5c6472',
    width: possiblySame ? 2 : 1.2,
  };
}

// buildScene 把 /api/graph 的响应转成渲染数据。
//
// 这里对 x/y **只做复制**（`x: n.x`）：任何"顺手加点抖动/重排"都会破坏
// 服务端确定性坐标的意义，也会让刷新前后画面跳变。
export function buildScene(resp, { asOf = null, concepts = null, relations = null } = {}) {
  const raw = Array.isArray(resp?.nodes) ? resp.nodes : [];
  const nodes = raw.map((n) => ({
    ...n,
    x: Number(n.x),
    y: Number(n.y),
    r: nodeRadius(n.degree),
    key: conceptKey(n),
    draft: isDraft(n),
    matched: Boolean(n.matched),
  }));
  const present = new Set(nodes.map((n) => n.id));
  const edges = (Array.isArray(resp?.edges) ? resp.edges : [])
    .filter((e) => present.has(e.from) && present.has(e.to))
    .map((e) => ({ ...e, style: edgeStyle(e, asOf) }));

  const scene = {
    meta: resp?.meta || {},
    nodes,
    edges,
    clusters: Array.isArray(resp?.clusters) ? resp.clusters : [],
    clusterEdges: Array.isArray(resp?.cluster_edges) ? resp.cluster_edges : [],
    asOf,
    // layoutVersion 只由 layout.js 的显式重排递增；用它给"是否重排过"留痕，
    // 单测据此断言 as_of 变化不触发重排。
    layoutVersion: 0,
  };
  return filterScene(scene, { concepts, relations });
}

// filterScene 按 concept / relation 多选过滤（空集合 = 不过滤）。
export function filterScene(scene, { concepts = null, relations = null } = {}) {
  const conceptSet = concepts && concepts.size ? concepts : null;
  const relationSet = relations && relations.size ? relations : null;
  const nodes = conceptSet
    ? scene.nodes.filter((n) => conceptSet.has(n.key))
    : scene.nodes.slice();
  const present = new Set(nodes.map((n) => n.id));
  const edges = scene.edges.filter(
    (e) => present.has(e.from) && present.has(e.to) && (!relationSet || relationSet.has(e.relation)),
  );
  const clusters = conceptSet
    ? scene.clusters.filter((c) => conceptSet.has(c.id || c.concept_id || ''))
    : scene.clusters.slice();
  return { ...scene, nodes, edges, clusters };
}

// applyAsOf 只重算**边的样式**：节点对象原样保留（同一引用、同一坐标）。
//
// 这就是"时间轴不引起布局抖动"的实现保证：没有任何坐标计算发生在这里。
export function applyAsOf(scene, asOf) {
  return {
    ...scene,
    asOf,
    edges: scene.edges.map((e) => ({ ...e, style: edgeStyle(e, asOf) })),
    nodes: scene.nodes, // 引用不变：坐标一个都没动
  };
}

// sceneTimeRange 给出时间轴的取值范围（世界时间秒）。
// 没有任何时间信息时返回 null（前端把滑块置灰，而不是造一个假区间）。
export function sceneTimeRange(scene) {
  let min = Infinity;
  let max = -Infinity;
  const consider = (v) => {
    const n = Number(v);
    if (Number.isFinite(n) && n > 0) {
      if (n < min) min = n;
      if (n > max) max = n;
    }
  };
  for (const e of scene.edges) {
    consider(e.t_valid);
    consider(e.t_invalid);
  }
  for (const c of scene.clusters) consider(c.t_valid);
  if (!Number.isFinite(min) || !Number.isFinite(max) || max <= 0) return null;
  if (max <= min) max = min + 1;
  return { min, max };
}

// pendingReviews 列出当前视图里待审的 possiblySame 边（蓝边，可直接裁决）。
export function pendingReviews(scene) {
  return scene.edges.filter((e) => e.style.possiblySame && !e.style.invalid);
}

// relationTypes / conceptOptions 给过滤面板提供稳定的选项列表（按字典序）。
export function relationTypes(scene) {
  const seen = new Map();
  for (const e of scene.edges) {
    if (!seen.has(e.relation)) seen.set(e.relation, 0);
    seen.set(e.relation, seen.get(e.relation) + 1);
  }
  return [...seen.entries()]
    .sort((a, b) => a[0].localeCompare(b[0]))
    .map(([relation, count]) => ({ relation, count }));
}

export function conceptOptions(scene) {
  const seen = new Map();
  for (const n of scene.nodes) {
    const label = n.key || '(未归属概念)';
    if (!seen.has(label)) seen.set(label, { key: n.key, label, count: 0, draft: false });
    const o = seen.get(label);
    o.count += 1;
    o.draft = o.draft || n.draft;
  }
  return [...seen.values()].sort((a, b) => a.label.localeCompare(b.label));
}
