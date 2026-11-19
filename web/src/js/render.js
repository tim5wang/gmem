// 渲染层：SVG（≤2000 图元）与 Canvas（>2000）两档，共用同一份场景数据。
//
// 分档的意义（TD §9「渲染上限」）：SVG 每个节点都是 DOM，2k 以上浏览器会被
// 样式重算拖死；Canvas 一次 drawImage 画完，但失去逐元素事件——因此大图档
// 用"按坐标命中测试"补回点击与悬停，交互语义与 SVG 档保持一致。
import { makeProjector } from './geometry.js';
import { conceptColor } from './model.js';

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

// render 是渲染入口：按分档选 SVG 或 Canvas，返回实际使用的渲染器名。
export function render(host, scene, opts) {
  const { svg, canvas } = host;
  const useCanvas = opts.useCanvas;
  if (!useCanvas) {
    canvas.hidden = true;
    svg.hidden = false;
    renderSVG(svg, scene, opts);
    return 'svg';
  }
  svg.hidden = true;
  canvas.hidden = false;
  renderCanvas(canvas, scene, opts);
  return 'canvas';
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

  const pos = new Map();
  for (const n of scene.nodes) pos.set(n.id, project(n.x, n.y));

  // (d/e/f) 边：实线=有效，虚线淡边=失效，蓝色=possiblySame。
  for (const e of scene.edges) {
    const a = pos.get(e.from);
    const b = pos.get(e.to);
    if (!a || !b) continue;
    const line = el('line', {
      x1: a.x.toFixed(1),
      y1: a.y.toFixed(1),
      x2: b.x.toFixed(1),
      y2: b.y.toFixed(1),
      stroke: e.style.possiblySame ? STYLE.edgePossiblySameColor : e.style.invalid ? STYLE.edgeInvalidColor : STYLE.edgeColor,
      'stroke-width': String(e.style.possiblySame ? 2 : 1.2),
      'stroke-opacity': String(e.style.opacity),
      // 失效边画虚线 = 保留历史（不过滤）。
      'stroke-dasharray': e.style.dashed ? '4 3' : 'none',
      'data-edge-id': e.id,
      'data-from': e.from,
      'data-to': e.to,
      'data-invalid': e.style.invalid ? '1' : '0',
      'data-possibly-same': e.style.possiblySame ? '1' : '0',
      class: `edge${e.style.possiblySame ? ' possibly-same' : ''}${e.style.invalid ? ' invalidated' : ''}`,
    });
    gEdges.appendChild(line);
  }

  // 折叠视图：concept 团 + 计数（truncated=true 时必须可见，否则"白屏"）。
  for (const c of scene.clusters) {
    const p = clusterPos.get(c.id || c.concept_id || '');
    if (!p) continue;
    const r = 10 + Math.min(30, Math.sqrt(Math.max(1, c.count)) * 3.2);
    const g = el('g', {
      class: 'cluster',
      'data-cluster-id': c.concept_id || '',
      'data-cluster-name': c.concept_name || c.id || '',
      'data-count': String(c.count),
      transform: `translate(${p.x.toFixed(1)},${p.y.toFixed(1)})`,
    });
    g.appendChild(el('circle', {
      r: r.toFixed(1),
      fill: c.concept_status === 'draft' ? 'url(#draft-hatch)' : STYLE.clusterFill,
      stroke: c.matched ? STYLE.matchedStroke : STYLE.clusterStroke,
      'stroke-width': c.matched ? '2.5' : '1.5',
    }));
    // 团标签：名字 + 计数（点了才能下钻，所以计数必须直接读得出来）。
    const label = el('text', {
      'text-anchor': 'middle',
      y: String(r + 13),
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

    const text = el('text', {
      x: p.x.toFixed(1),
      y: (p.y + n.r + 11).toFixed(1),
      'text-anchor': 'middle',
      fill: STYLE.label,
      'font-size': '10',
      class: 'node-label',
    });
    text.textContent = n.canonical_name || n.id;
    gLabels.appendChild(text);
  }

  return { project, pos, clusterPos };
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

  for (const e of scene.edges) {
    const a = pos.get(e.from);
    const b = pos.get(e.to);
    if (!a || !b) continue;
    ctx.save();
    ctx.globalAlpha = e.style.opacity;
    ctx.strokeStyle = e.style.possiblySame
      ? STYLE.edgePossiblySameColor
      : e.style.invalid
        ? STYLE.edgeInvalidColor
        : STYLE.edgeColor;
    ctx.lineWidth = e.style.possiblySame ? 2 : 1;
    if (e.style.dashed) ctx.setLineDash([4, 3]);
    ctx.beginPath();
    ctx.moveTo(a.x, a.y);
    ctx.lineTo(b.x, b.y);
    ctx.stroke();
    ctx.restore();
  }

  for (const c of scene.clusters) {
    const p = project(c.x, c.y);
    const r = 10 + Math.min(30, Math.sqrt(Math.max(1, c.count)) * 3.2);
    ctx.save();
    ctx.beginPath();
    ctx.arc(p.x, p.y, r, 0, Math.PI * 2);
    ctx.fillStyle = STYLE.clusterFill;
    ctx.fill();
    ctx.strokeStyle = c.matched ? STYLE.matchedStroke : STYLE.clusterStroke;
    ctx.lineWidth = c.matched ? 2.5 : 1.5;
    ctx.stroke();
    ctx.fillStyle = STYLE.label;
    ctx.font = '11px system-ui, sans-serif';
    ctx.textAlign = 'center';
    ctx.fillText(`${c.concept_name || c.id || '(未归属概念)'} · ${c.count}`, p.x, p.y + r + 13);
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
  return { project, pos };
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
