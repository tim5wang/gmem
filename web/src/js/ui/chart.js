// 手写 SVG 迷你图表（**不引图表库**）。
//
// 为什么手写：TD §9 的工程约束是"运行时依然零外部依赖"，而图表库动辄上百 KB；
// 这里需要的只是"一条折线 + 几个刻度"，手写比配置一个图表库更短，也更容易被单测
// 直接断言 SVG 属性（与 render.js 的做法一致）。
//
// 本模块只做**纯计算 + 元素产出**：不做动画、不发请求、不读全局状态。
// P6-1 只需要它在 plan 区块旁边画一条 coverage 对照的小图；Metrics 页
// （P6-4）会继续复用它。
import { h, fmt } from './dom.js';

// niceTicks 生成"好看"的刻度（1/2/5 × 10^n 步进）。
//
// 借 CortexDB 的 niceTicks 思路（调研 §8.1）：刻度必须是可读的整数步进，
// 否则读者会看到 0.037、0.074、0.111 这种没法心算的轴。
export function niceTicks(min, max, count = 4) {
  const lo = Number(min);
  const hi = Number(max);
  if (!Number.isFinite(lo) || !Number.isFinite(hi) || count < 2) return [];
  if (hi === lo) return [lo];
  const span = hi - lo;
  const rawStep = span / (count - 1);
  const mag = Math.pow(10, Math.floor(Math.log10(rawStep)));
  const norm = rawStep / mag;
  let step;
  if (norm <= 1) step = 1;
  else if (norm <= 2) step = 2;
  else if (norm <= 5) step = 5;
  else step = 10;
  step *= mag;
  const out = [];
  // 从 lo 向上取到第一个 >= lo 的步进点，再逐格推进。
  const start = Math.ceil(lo / step) * step;
  for (let v = start; v <= hi + step * 1e-9; v += step) {
    // 消掉浮点累积误差（0.1+0.2 类问题会让刻度变成 0.30000000000000004）。
    out.push(Number(v.toFixed(10)));
  }
  return out;
}

// linePath 把点列转成 SVG path 的 d 属性（M/L）。
//
// points 的每项是 {x, y}（已映射到画布坐标）。空数组返回 ''（不画）。
export function linePath(points) {
  if (!Array.isArray(points) || points.length === 0) return '';
  return points
    .map((p, i) => `${i === 0 ? 'M' : 'L'}${round(p.x)} ${round(p.y)}`)
    .join(' ');
}

function round(v) {
  const n = Number(v);
  return Number.isFinite(n) ? Number(n.toFixed(2)) : 0;
}

// sparkline 产出一个迷你折线图（SVG 元素）。
//
//   values  数据点（number[]）
//   width / height  画布尺寸
//   label   aria-label（**必填**：纯图形对读屏用户不可见）
//   min / max  固定值域（有的时候就传，避免"每次刷新纵轴都在变"）
export function sparkline({
  values = [], width = 160, height = 36, label = '图表', min, max, className = '',
} = {}) {
  const nums = (Array.isArray(values) ? values : []).map(Number).filter((v) => Number.isFinite(v));
  const svg = h('svg', {
    className: `spark ${className}`.trim(),
    viewBox: `0 0 ${width} ${height}`,
    width: String(width),
    height: String(height),
    role: 'img',
    'aria-label': String(label || '图表'),
  });
  if (nums.length === 0) return svg;

  const lo = Number.isFinite(min) ? Number(min) : Math.min(...nums);
  const hi = Number.isFinite(max) ? Number(max) : Math.max(...nums);
  const span = hi - lo || 1;
  const pad = 2;
  const innerW = width - pad * 2;
  const innerH = height - pad * 2;
  const pts = nums.map((v, i) => ({
    x: pad + (nums.length === 1 ? innerW / 2 : (i / (nums.length - 1)) * innerW),
    y: pad + innerH - ((v - lo) / span) * innerH,
  }));
  svg.appendChild(h('path', {
    className: 'spark-line',
    d: linePath(pts),
    fill: 'none',
    'stroke-width': '1.5',
  }));
  return svg;
}

// barChart 产出一个迷你条形图（用于"每条 lane 的贡献"这一类离散量）。
//
// bar 的 title 属性给出精确值，鼠标悬停即可读到**未被图形近似**的数字。
export function barChart({
  items = [], width = 240, height = 80, label = '条形图', digits = 3,
} = {}) {
  const svg = h('svg', {
    className: 'bars',
    viewBox: `0 0 ${width} ${height}`,
    width: String(width),
    height: String(height),
    role: 'img',
    'aria-label': String(label || '条形图'),
  });
  const rows = (Array.isArray(items) ? items : []).filter((it) => it && Number.isFinite(Number(it.value)));
  if (rows.length === 0) return svg;
  const maxV = Math.max(...rows.map((r) => Number(r.value)), 1e-9);
  const gap = 3;
  const barH = Math.max(2, (height - gap * (rows.length - 1)) / rows.length);
  rows.forEach((r, i) => {
    const v = Number(r.value);
    const w = Math.max(0, (v / maxV) * (width - 2));
    const bar = h('rect', {
      className: 'bar',
      x: '1',
      y: String(round(i * (barH + gap))),
      width: String(round(w)),
      height: String(round(barH)),
    });
    // title 给精确值：图形只表达相对大小，数字必须可读。
    bar.appendChild(h('title', { textContent: `${r.label ?? i}: ${fmt(v, digits)}` }));
    svg.appendChild(bar);
  });
  return svg;
}
