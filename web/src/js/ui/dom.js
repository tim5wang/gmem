// 极小 DOM 构造器（无框架、无虚拟 DOM）。
//
// 存在的理由：六页都要拼 DOM，若每页各自 `document.createElement` +
// `appendChild` + 手工设置属性，代码会比数据本身还长，且"设置了 textContent
// 却又用了 innerHTML"这类安全失误会反复出现。这里只做三件事：
//   - 属性/文本**永远**经 textContent / setAttribute 写入（不会拼 HTML 字符串）；
//   - children 支持字符串（自动变文本节点）与嵌套数组；
//   - 返回普通 Element，便于单测用最小 DOM 打桩直接断言产出结构。
//
// 刻意不做：没有 JSX、没有 diff、没有生命周期。"是否重画整块"由页面自己决定
// （Graph View 一直是全量重画，六页沿用同一模型）。

// h 构造一个元素。
//
// className / textContent / dataset / 事件（on* 键）走属性快速路径，
// 其余键一律 setAttribute —— 这样 `aria-busy`、`role`、`hidden` 之类不会漏。
export function h(tag, props = {}, children = []) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(props)) {
    if (v === null || v === undefined || v === false) continue;
    if (k === 'className') {
      el.className = v;
    } else if (k === 'textContent') {
      el.textContent = String(v);
    } else if (k === 'dataset') {
      for (const [dk, dv] of Object.entries(v)) el.dataset[dk] = String(dv);
    } else if (k.startsWith('on') && typeof v === 'function') {
      el[k] = v;
    } else if (k === 'hidden') {
      el.hidden = Boolean(v);
    } else if (k === 'value') {
      el.value = String(v);
    } else {
      el.setAttribute(k, String(v));
    }
  }
  append(el, children);
  return el;
}

// append 把 children 追加到 el（字符串 → 文本节点；数组递归）。
export function append(el, children) {
  const list = Array.isArray(children) ? children : [children];
  for (const c of list) {
    if (c === null || c === undefined || c === false) continue;
    if (Array.isArray(c)) {
      append(el, c);
    } else if (typeof c === 'string' || typeof c === 'number') {
      el.appendChild(document.createTextNode(String(c)));
    } else {
      el.appendChild(c);
    }
  }
  return el;
}

// clear 清空一个容器（全量重画前的固定动作）。
export function clear(el) {
  if (!el) return el;
  el.textContent = '';
  return el;
}

// fmt 把数字格式化成固定小数位（**所有分数都必须走它**）。
//
// 为什么不能直接拼 `${n}`：可复算的前提是数字格式稳定。同一个 score 在不同
// 精度下显示成 0.00163934 与 0.002，读者会以为页面在自相矛盾。
export function fmt(n, digits = 3) {
  const v = Number(n);
  if (!Number.isFinite(v)) return '—';
  return v.toFixed(digits);
}

// fmtPct 把 0..1 的比例显示成百分比（coverage / theta_coverage 用）。
export function fmtPct(n, digits = 1) {
  const v = Number(n);
  if (!Number.isFinite(v)) return '—';
  return `${(v * 100).toFixed(digits)}%`;
}

// fmtUnix 把 unix 秒显示成可读时刻（as_of 横幅用；非法值原样回显）。
export function fmtUnix(sec) {
  const v = Number(sec);
  if (!Number.isFinite(v)) return String(sec ?? '—');
  const iso = new Date(v * 1000).toISOString();
  return iso.replace('T', ' ').replace('.000Z', ' UTC');
}

// collectText 深度收集文本（**只给单测用**：断言"页面里到底出现了哪些字"）。
export function collectText(el, out = []) {
  if (!el) return out;
  if (typeof el.textContent === 'string' && el.textContent) out.push(el.textContent);
  for (const c of el.children || []) collectText(c, out);
  return out;
}
