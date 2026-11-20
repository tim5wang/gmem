// 骨架屏（loading）：带 aria-busy + aria-label 的占位块。
//
// 为什么不用转圈：调研结论（§10.5）是"骨架屏，不用转圈（除首帧）"。
// 骨架给出**即将出现的结构**，读者不会以为页面已经加载完但是空的；
// 转圈只说明"在等"，不说明"等出什么"。
//
// 无障碍硬要求（验收点名的）：容器必须 aria-busy="true" 且带 aria-label，
// 否则读屏用户只会遇到一片静默。
import { h } from './dom.js';

// skeleton 构造一个骨架屏容器。
//
//   rows    行数（默认 3）
//   label   aria-label（读屏播报"正在加载……"；**必填**，缺了就退回默认话术）
//   variant 'list' | 'table' | 'block'（只影响样式类，不影响语义）
export function skeleton({ rows = 3, label = '正在加载', variant = 'list' } = {}) {
  const n = Math.max(1, Math.min(20, Number(rows) || 3));
  const wrap = h('div', {
    className: `skeleton skeleton-${variant}`,
    'aria-busy': 'true',
    'aria-label': String(label || '正在加载'),
    role: 'status',
    'data-skeleton-variant': variant,
  });
  for (let i = 0; i < n; i += 1) {
    wrap.appendChild(h('div', { className: 'skeleton-row', 'aria-hidden': 'true' }));
  }
  return wrap;
}

// skeletonInto 用骨架屏占满容器（重画前的固定动作）。
export function skeletonInto(container, opts) {
  if (!container) return null;
  container.textContent = '';
  const el = skeleton(opts);
  container.appendChild(el);
  return el;
}
