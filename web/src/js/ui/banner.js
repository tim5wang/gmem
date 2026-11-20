// 横幅（banner）：错误 / 降级 / 信息 / 成功四类。
//
// 从 main.js 迁出的**行为不变**版本：同样的 class 名（banner error|warn|info|ok）、
// 同样的 textContent 写入方式、同样的 8 秒自动消失的 flash。
//
// # 为什么 textContent 是硬约束（F-5 的结论）
//
// banner 的文本里有服务端可控字符串（meta.degraded / recall.degraded /
// ApiError.message）。只要它们经 textContent 写入，就永远不可能变成标签或事件
// 属性。因此本模块**不提供**任何 innerHTML 入口；老代码里的
// `banners.innerHTML = parts.join('')` 也被换成逐节点 append（语义等价，但不再
// 需要"先 outerHTML 再拼串"这一圈）。
import { h } from './dom.js';

// BANNER_KINDS 是合法的横幅类别（拼错会得到无色样式，因此显式收敛）。
export const BANNER_KINDS = Object.freeze(['error', 'warn', 'info', 'ok']);

// banner 构造一个横幅元素（文本一律走 textContent）。
export function banner(kind, text, { role = 'status' } = {}) {
  const k = BANNER_KINDS.includes(kind) ? kind : 'info';
  return h('div', { className: `banner ${k}`, role, textContent: String(text ?? '') });
}

// bannerHTML 返回横幅元素的 outerHTML（保留给"必须拼字符串"的老调用点）。
//
// 只在已确定文本安全（已经过 textContent）时使用；新代码请直接用 banner()。
export function bannerHTML(kind, text) {
  return banner(kind, text).outerHTML;
}

// renderBanners 用一组 {kind, text} 全量重画容器。
//
// 全量重画（而不是增量 diff）与 Graph View 的渲染模型一致：状态就几条，
// diff 带来的 bug 比它能省的 DOM 操作更贵。
export function renderBanners(container, items) {
  if (!container) return container;
  container.textContent = '';
  for (const it of items || []) {
    if (!it || !it.text) continue;
    container.appendChild(banner(it.kind, it.text));
  }
  return container;
}

// flash 追加一条短时横幅（8 秒后自动移除，不阻塞其它横幅）。
export function flash(container, kind, text, { ms = 8000 } = {}) {
  if (!container) return null;
  const el = banner(kind, text);
  container.appendChild(el);
  // 定时器在测试里可能不存在（node 有 setTimeout，最小 DOM 打桩可能没有）。
  if (typeof setTimeout === 'function' && ms > 0) {
    setTimeout(() => el.remove && el.remove(), ms);
  }
  return el;
}
