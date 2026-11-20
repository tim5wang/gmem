// 渲染层：SVG（≤2000 图元）与 Canvas（>2000）两档，共用同一份场景数据。
//
// 分档的意义（TD §9「渲染上限」）：SVG 每个节点都是 DOM，2k 以上浏览器会被
// 样式重算拖死；Canvas 一次 drawImage 画完，但失去逐元素事件——因此大图档
// 用"按坐标命中测试"补回点击与悬停，交互语义与 SVG 档保持一致。
import { makeProjector, placeClusterBadges } from './geometry.js';
import { conceptColor } from './model.js';
import { layoutLabels, clusterObstacles } from './labelLayouter.js';

const SVG_NS = 'http://www.w3.org/2000/svg';

export const STYLE = {
  edgeColor: '#5c6472',
  edgeInvalidColor: '#8a94a6',
  edgePossiblySameColor: '#1d6fd8',
  matchedStroke: '#d32f2f',
  clusterFill: '#eef2f7',
  clusterStroke: '#8d9bb0',
  label: '#1f2933',
};

// render 是渲染入口：按分档选 SVG 或 Canvas，返回**渲染器产物**。
//
// 返回值必须带上 {project, pos, clusterPos}：Canvas 档没有逐元素事件，点击与悬停
// 全靠把世界坐标投到屏幕坐标再命中测试。之前这里只回一个渲染器名字符串，
// 调用方拿到的 project 是 undefined，导致大图档（>2000 图元）一点就 TypeError。
// 因此返回值 = {renderer, project, pos, clusterPos}，由 renderSVG/renderCanvas 供给。
export function render(host, scene, opts) {
  const { svg, canvas } = host;
  const useCanvas = opts.useCanvas;
  if (!useCanvas) {
    canvas.hidden = true;
    svg.hidden = false;
    const out = renderSVG(svg, scene, opts);
    return { renderer: 'svg', ...out };
  }
  svg.hidden = true;
  canvas.hidden = false;
  const out = renderCanvas(canvas, scene, opts);
  return { renderer: 'canvas', ...out };
}

// isPendingReview 判定一条边是不是**当前可裁决**的待审边。
//
// 必须同时看 possiblySame 与 invalid：裁决为 diff 后服务端只给这条边置
// TInvalid（保留历史、不过滤），关系名仍是 possiblySame。只看关系名就会把
// “已经判成不同”的边重新递上“同一”按钮 → 误合并（不可逆）。
// 这条判定与 model.js pendingReviews 的口径一致，两处不得漂移。
export function isPendingReview(e) {
  return Boolean(e.style.possiblySame) && !e.style.invalid;
}

// edgeEndpoints 把一条边裁剪到两端节点圆的**边缘**。
//
// 为什么必须裁：边原本从圆心画到圆心，而节点圆在 SVG 里压在上层（节点要能
// 单独点），于是两端那两段线被圆盖住 —— 鼠标落在那头命中的是节点，不是边。
// 短边（如待审蓝边，长 89px、两端半径 9~13px）的可见可点区因此被吃掉一截。
// 裁到圆外后，画出来的线段与可点热区一致，节点不再吞掉边的点击。
function edgeEndpoints(a, b, ra, rb) {
  const dx = b.x - a.x;
  const dy = b.y - a.y;
  const len = Math.hypot(dx, dy);
  if (!(len > 0.01)) return { x1: a.x, y1: a.y, x2: b.x, y2: b.y };
  const gap = 2; // 留 2px：避免线头被圆描边压住
  let t1 = (ra + gap) / len;
  let t2 = 1 - (rb + gap) / len;
  if (t2 - t1 < 0.08) {
    // 两个圆几乎相接：退化成中点附近的一小段，这样短边仍然有热区可点。
    const m = Math.max(0, Math.min(1, (t1 + t2) / 2));
    t1 = Math.max(0, Math.min(1, m - 0.04));
    t2 = Math.max(0, Math.min(1, m + 0.04));
  }
  return {
    x1: a.x + dx * t1,
    y1: a.y + dy * t1,
    x2: a.x + dx * t2,
    y2: a.y + dy * t2,
  };
}

// ---------------------------------------------------------------------------
// SVG 档
// ---------------------------------------------------------------------------

export function renderSVG(svg, scene, opts) {
  const { width, height } = opts;
  const project = makeProjector(scene, width, height);
  svg.setAttribute('viewBox', `0 0 ${width} ${height}`);
  svg.setAttribute('width', String(width));
  svg.setAttribute('height', String(height));
  while (svg.firstChild) svg.removeChild(svg.firstChild);

  const defs = el('defs', {});
  // (b) draft 概念用斜纹：SVG pattern 叠在填充之上，颜色仍按 concept（(a)）。
  const pattern = el('pattern', {
    id: 'draft-hatch',
    width: '6',
    height: '6',
    patternUnits: 'userSpaceOnUse',
    patternTransform: 'rotate(45)',
  });
  pattern.appendChild(el('rect', { width: '6', height: '6', fill: 'transparent' }));
  pattern.appendChild(el('line', { x1: '0', y1: '0', x2: '0', y2: '6', stroke: '#20262e', 'stroke-width': '2', opacity: '0.55' }));
  defs.appendChild(pattern);
  svg.appendChild(defs);

  const gEdges = el('g', { class: 'edges' });
  const gClusters = el('g', { class: 'clusters' });
  const gNodes = el('g', { class: 'nodes' });
  const gLabels = el('g', { class: 'labels' });
  svg.appendChild(gEdges);
  svg.appendChild(gClusters);
  svg.appendChild(gNodes);
  svg.appendChild(gLabels);

  const clusterPos = new Map();
  for (const c of scene.clusters) {
    const p = project(c.x, c.y);
    const key = c.id || c.concept_id || '';
    clusterPos.set(key, p);
  }
  // 团徽位置（圆盘 + 团名）可能与团心不同：概览下要让位到节点云外侧，
  // 否则团名会压住团心那个节点（demo 三个团全部命中）。
  // 折叠视图（团里没有可见节点）仍然画在团心，行为不变。
  const clusterBadges = placeClusterBadges(scene, project, { width, height });

  const pos = new Map();
  for (const n of scene.nodes) pos.set(n.id, project(n.x, n.y));
  const nodeRadius = new Map();
  for (const n of scene.nodes) nodeRadius.set(n.id, n.r);

  // (d/e/f) 边：实线=有效，虚线淡边=失效，蓝色=待审 possiblySame。
  //
  // 绘制顺序：先画非待审（普通边 / 失效虚淡边），待审蓝边**最后画**。
  // 为什么：裁决为 diff 后，同一对端点上会同时存在一条新的 negative_SameAs
  // 边和那条已失效的 possiblySame 边，两条线完全共线 —— 后画的会盖住先画的。
  // 待审边是可点入口，必须压在层顶；失效边只是历史痕迹，被盖住无妨。
  const ordered = [
    ...scene.edges.filter((e) => !isPendingReview(e)),
    ...scene.edges.filter(isPendingReview),
  ];
  for (const e of ordered) {
    const a = pos.get(e.from);
    const b = pos.get(e.to);
    if (!a || !b) continue;
    // 待审 = possiblySame **且未失效**：已裁决为 diff 的边照旧画（保留历史），
    // 但必须降级成普通虚淡边，不再蓝、不再可点。
    const pending = isPendingReview(e);
    // 裁到节点圆外：圆压在上层，不裁的话两端线段被圆吞掉（点击热区也一起没了）。
    const seg = edgeEndpoints(a, b, nodeRadius.get(e.from) ?? 0, nodeRadius.get(e.to) ?? 0);
    const line = el('line', {
      x1: seg.x1.toFixed(1),
      y1: seg.y1.toFixed(1),
      x2: seg.x2.toFixed(1),
      y2: seg.y2.toFixed(1),
      stroke: pending ? STYLE.edgePossiblySameColor : e.style.invalid ? STYLE.edgeInvalidColor : STYLE.edgeColor,
      'stroke-width': String(pending ? 2 : 1.2),
      'stroke-opacity': String(e.style.opacity),
      // 失效边画虚线 = 保留历史（不过滤）。
      'stroke-dasharray': e.style.dashed ? '4 3' : 'none',
      'data-edge-id': e.id,
      'data-from': e.from,
      'data-to': e.to,
      'data-invalid': e.style.invalid ? '1' : '0',
      // data-possibly-same 保留服务端语义（关系名），class 才代表“现在可裁决”。
      'data-possibly-same': e.style.possiblySame ? '1' : '0',
      'data-pending': pending ? '1' : '0',
      class: `edge${pending ? ' possibly-same' : ''}${e.style.invalid ? ' invalidated' : ''}`,
    });
    gEdges.appendChild(line);
  }

  // 折叠视图：concept 团 + 计数（truncated=true 时必须可见，否则"白屏"）。
  //
  // F-4 交互层与视觉层分离：团圆**恒为 pointer-events:none**，绝不抢事件。
  // 原因（实测）：未折叠时团只是本体覆盖的装饰信息，而这些十几像素的大圆后画、
  // 浮在上层，会把边的点击热区整块盖掉（一条蓝边 40 个采样点全被团挡住）。
  // 折叠视图下确实需要“点团下钻”，那就单独给一个透明命中圆（cluster-hit），
  // 语义上它是交互层而不是视觉层，删/改视觉不会顺手把点击弄没。
  const drillable = Boolean(scene.meta?.truncated);
  for (const c of scene.clusters) {
    const key = c.id || c.concept_id || '';
    const p = clusterPos.get(key);
    if (!p) continue;
    const r = 10 + Math.min(30, Math.sqrt(Math.max(1, c.count)) * 3.2);
    const badge = clusterBadges.get(key) || { x: p.x, y: p.y, r, labelY: p.y + r + 13 };
    const g = el('g', {
      class: `cluster${drillable ? ' drillable' : ''}`,
      'data-cluster-id': c.concept_id || '',
      'data-cluster-name': c.concept_name || c.id || '',
      'data-count': String(c.count),
      transform: `translate(${badge.x.toFixed(1)},${badge.y.toFixed(1)})`,
    });
    if (drillable) {
      // 命中层：透明、只负责任事件（下钻），不参与视觉。
      g.appendChild(el('circle', {
        class: 'cluster-hit',
        r: r.toFixed(1),
        fill: 'transparent',
        stroke: 'none',
        'pointer-events': 'auto',
        'data-cluster-id': c.concept_id || '',
      }));
    }
    g.appendChild(el('circle', {
      r: r.toFixed(1),
      fill: c.concept_status === 'draft' ? 'url(#draft-hatch)' : STYLE.clusterFill,
      stroke: c.matched ? STYLE.matchedStroke : STYLE.clusterStroke,
      'stroke-width': c.matched ? '2.5' : '1.5',
      'pointer-events': 'none',
    }));
    // 团标签：名字 + 计数（点了才能下钻，所以计数必须直接读得出来）。
    // y 是相对团徽中心的偏移（团徽可能已经被让位到云外）。
    const label = el('text', {
      'text-anchor': 'middle',
      y: String(Math.round((badge.labelY - badge.y) * 10) / 10),
      fill: STYLE.label,
      'font-size': '11',
      class: 'cluster-label',
    });
    label.textContent = `${c.concept_name || c.id || '(未归属概念)'} · ${c.count}`;
    g.appendChild(label);
    gClusters.appendChild(g);
  }

  // 团间聚合边（折叠视图专属）。
  for (const ce of scene.clusterEdges || []) {
    const a = clusterPos.get(ce.from);
    const b = clusterPos.get(ce.to);
    if (!a || !b) continue;
    gClusters.appendChild(el('line', {
      x1: a.x.toFixed(1),
      y1: a.y.toFixed(1),
      x2: b.x.toFixed(1),
      y2: b.y.toFixed(1),
      stroke: STYLE.clusterStroke,
      'stroke-width': String(Math.min(4, 1 + Math.log2(Math.max(1, ce.count)))),
      'stroke-opacity': ce.invalidated ? '0.25' : '0.55',
      'stroke-dasharray': ce.invalidated ? '4 3' : 'none',
      class: 'cluster-edge',
    }));
  }

  for (const n of scene.nodes) {
    const p = pos.get(n.id);
    if (!p) continue;
    const g = el('g', {
      class: `node${n.matched ? ' matched' : ''}`,
      transform: `translate(${p.x.toFixed(1)},${p.y.toFixed(1)})`,
      'data-node-id': n.id,
      'data-concept': n.key,
      'data-degree': String(n.degree),
    });
    // (a) 颜色按 concept；(b) draft 叠斜纹。
    g.appendChild(el('circle', {
      r: n.r.toFixed(1),
      fill: n.draft ? 'url(#draft-hatch)' : conceptColor(n.key),
      stroke: n.matched ? STYLE.matchedStroke : '#ffffff',
      'stroke-width': n.matched ? '2.5' : '1.2',
    }));
    // draft 节点额外描一圈虚线圈：只有斜纹在小圆上太细，看一眼分不清。
    if (n.draft) {
      g.appendChild(el('circle', {
        r: (n.r + 2.5).toFixed(1),
        fill: 'none',
        stroke: '#20262e',
        'stroke-width': '1',
        'stroke-dasharray': '2 2',
        opacity: '0.6',
      }));
    }
    gNodes.appendChild(g);
  }

  // 节点标签：确定性避让（labelLayouter.js），而不是"每个都画在正下方"。
  // 团标签已经先画在图上，因此它们是障碍物：节点标签绕开团名与计数。
  const labelItems = scene.nodes
    .filter((n) => pos.has(n.id))
    .map((n) => {
      const p = pos.get(n.id);
      return {
        id: n.id,
        x: p.x,
        y: p.y,
        r: n.r,
        degree: n.degree,
        text: n.canonical_name || n.id,
      };
    });
  const placed = layoutLabels(labelItems, {
    width,
    height,
    // 障碍框按**实际画出去**的团徽位置算（含让位后的偏移与画布尺寸）。
    obstacles: clusterObstacles(scene, project, { width, height }),
  });
  for (const lab of placed.labels) {
    const text = el('text', {
      x: lab.x.toFixed(1),
      y: lab.y.toFixed(1),
      'text-anchor': lab.anchor === 'right' ? 'start' : lab.anchor === 'left' ? 'end' : 'middle',
      fill: STYLE.label,
      'font-size': '10',
      'data-label-anchor': lab.anchor,
      class: 'node-label',
    });
    text.textContent = lab.text;
    gLabels.appendChild(text);
  }
  // 隐藏了哪些标签必须能被查出来（测试与排查都靠它，不能悄悄丢）。
  svg.setAttribute('data-labels-shown', String(placed.shown));
  svg.setAttribute('data-labels-hidden', String(placed.hidden.length));

  return { project, pos, clusterPos, labels: placed };
}

function el(name, attrs) {
  const node = document.createElementNS(SVG_NS, name);
  for (const [k, v] of Object.entries(attrs)) node.setAttribute(k, v);
  return node;
}

// ---------------------------------------------------------------------------
// Canvas 档（> 2000 图元）
// ---------------------------------------------------------------------------

export function renderCanvas(canvas, scene, opts) {
  const { width, height } = opts;
  const ratio = opts.devicePixelRatio || 1;
  canvas.width = Math.max(1, Math.floor(width * ratio));
  canvas.height = Math.max(1, Math.floor(height * ratio));
  canvas.style.width = `${width}px`;
  canvas.style.height = `${height}px`;
  const ctx = canvas.getContext('2d');
  ctx.setTransform(ratio, 0, 0, ratio, 0, 0);
  ctx.clearRect(0, 0, width, height);

  const project = makeProjector(scene, width, height);
  const pos = new Map();
  for (const n of scene.nodes) pos.set(n.id, project(n.x, n.y));
  const nodeRadius = new Map();
  for (const n of scene.nodes) nodeRadius.set(n.id, n.r);

  // 与 SVG 档同样的绘制顺序：待审蓝边最后画，避免被共线的失效边盖住。
  const ordered = [
    ...scene.edges.filter((e) => !isPendingReview(e)),
    ...scene.edges.filter(isPendingReview),
  ];
  for (const e of ordered) {
    const a = pos.get(e.from);
    const b = pos.get(e.to);
    if (!a || !b) continue;
    // 与 SVG 档同一口径：已失效的 possiblySame 边不再当待审边画（会骗人）。
    const pending = isPendingReview(e);
    const seg = edgeEndpoints(a, b, nodeRadius.get(e.from) ?? 0, nodeRadius.get(e.to) ?? 0);
    ctx.save();
    ctx.globalAlpha = e.style.opacity;
    ctx.strokeStyle = pending
      ? STYLE.edgePossiblySameColor
      : e.style.invalid
        ? STYLE.edgeInvalidColor
        : STYLE.edgeColor;
    ctx.lineWidth = pending ? 2 : 1;
    if (e.style.dashed) ctx.setLineDash([4, 3]);
    ctx.beginPath();
    ctx.moveTo(seg.x1, seg.y1);
    ctx.lineTo(seg.x2, seg.y2);
    ctx.stroke();
    ctx.restore();
  }

  // 团间聚合边（F-7）：SVG 档画了，Canvas 档必须画同一层，否则大图折叠后
  // 团间关系凭空消失 —— 两档的交互/视觉语义必须一致（TD §9）。
  const clusterPos = new Map();
  for (const c of scene.clusters) {
    clusterPos.set(c.id || c.concept_id || '', project(c.x, c.y));
  }
  for (const ce of scene.clusterEdges || []) {
    const a = clusterPos.get(ce.from);
    const b = clusterPos.get(ce.to);
    if (!a || !b) continue;
    ctx.save();
    ctx.strokeStyle = STYLE.clusterStroke;
    ctx.globalAlpha = ce.invalidated ? 0.25 : 0.55;
    ctx.lineWidth = Math.min(4, 1 + Math.log2(Math.max(1, ce.count)));
    if (ce.invalidated) ctx.setLineDash([4, 3]);
    ctx.beginPath();
    ctx.moveTo(a.x, a.y);
    ctx.lineTo(b.x, b.y);
    ctx.stroke();
    ctx.restore();
  }

  // 与 SVG 档同一份团徽位置（含概览下的让位），避免两档画在不同位置。
  const clusterBadges = placeClusterBadges(scene, project, { width, height });
  for (const c of scene.clusters) {
    const key = c.id || c.concept_id || '';
    const cp = clusterPos.get(key);
    if (!cp) continue;
    const r = 10 + Math.min(30, Math.sqrt(Math.max(1, c.count)) * 3.2);
    const badge = clusterBadges.get(key) || { x: cp.x, y: cp.y, r, labelY: cp.y + r + 13 };
    ctx.save();
    ctx.beginPath();
    ctx.arc(badge.x, badge.y, r, 0, Math.PI * 2);
    ctx.fillStyle = STYLE.clusterFill;
    ctx.fill();
    ctx.strokeStyle = c.matched ? STYLE.matchedStroke : STYLE.clusterStroke;
    ctx.lineWidth = c.matched ? 2.5 : 1.5;
    ctx.stroke();
    ctx.fillStyle = STYLE.label;
    ctx.font = '11px system-ui, sans-serif';
    ctx.textAlign = 'center';
    ctx.fillText(`${c.concept_name || c.id || '(未归属概念)'} · ${c.count}`, badge.x, badge.labelY);
    ctx.restore();
  }

  for (const n of scene.nodes) {
    const p = pos.get(n.id);
    if (!p) continue;
    ctx.save();
    ctx.beginPath();
    ctx.arc(p.x, p.y, n.r, 0, Math.PI * 2);
    ctx.fillStyle = conceptColor(n.key);
    ctx.fill();
    ctx.strokeStyle = n.matched ? STYLE.matchedStroke : '#ffffff';
    ctx.lineWidth = n.matched ? 2.5 : 1.2;
    ctx.stroke();
    // draft 用白色虚线内圈代替 SVG 的斜纹（Canvas 每帧建 pattern 太贵）。
    if (n.draft) {
      ctx.beginPath();
      ctx.arc(p.x, p.y, Math.max(2, n.r - 2), 0, Math.PI * 2);
      ctx.setLineDash([2, 2]);
      ctx.strokeStyle = '#20262e';
      ctx.lineWidth = 1;
      ctx.stroke();
    }
    ctx.restore();
  }

  // 命中测试用同一套屏幕坐标：交互语义与 SVG 档一致。
  return { project, pos, clusterPos };
}

// hitTest 返回鼠标位置下最近的节点（半径外扩 4px，便于小圆点击）。
export function hitTest(scene, project, x, y) {
  let best = null;
  let bestD = Infinity;
  for (const n of scene.nodes) {
    const p = project(n.x, n.y);
    const d = Math.hypot(p.x - x, p.y - y);
    const hit = Math.max(6, n.r) + 4;
    if (d <= hit && d < bestD) {
      best = n;
      bestD = d;
    }
  }
  return best;
}

// hitTestCluster 同上的团版本（折叠视图的"点开团下钻"）。
export function hitTestCluster(scene, project, x, y) {
  let best = null;
  let bestD = Infinity;
  for (const c of scene.clusters) {
    const p = project(c.x, c.y);
    const d = Math.hypot(p.x - x, p.y - y);
    const hit = 10 + Math.min(30, Math.sqrt(Math.max(1, c.count)) * 3.2) + 4;
    if (d <= hit && d < bestD) {
      best = c;
      bestD = d;
    }
  }
  return best;
}
