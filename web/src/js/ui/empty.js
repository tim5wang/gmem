// 空态：**说清楚为什么空 + 给一个能立刻执行的下步**。
//
// 调研结论（§10.4）把空态分成四种，不可混用：
//   1. 还没查询（q 为空）        → 给示例查询
//   2. 查询了但没有数据          → 给"换词/放宽 scope"的建议
//   3. 后端没有这个能力          → 指向 capabilities 的 reason（不是"暂无数据"）
//   4. 请求失败                  → 交给 errorState.js（有重试），不放这里
//
// 本模块只负责 1/2/3 的**呈现**；判断是哪一种由页面传 kind 决定 ——
// 让"没数据"与"答不了"在代码里也是两条分支，而不是同一句话。
import { h } from './dom.js';

// EMPTY_KINDS 是空态的四种来源（第 4 种在 errorState.js）。
export const EMPTY_KINDS = Object.freeze(['idle', 'noData', 'noCapability']);

// emptyState 构造一个空态块。
//
//   title        一句话说明现状（**不要**写"暂无数据"就完事）
//   reason       为什么空（有就显示，通常来自后端的 degraded / capability reason）
//   suggestions  可执行的下步；每条 {label, onClick} 渲染成按钮，
//                或 {label, href} 渲染成链接（示例查询用后者，便于复制/开新标签）
export function emptyState({ kind = 'idle', title, reason = '', suggestions = [] } = {}) {
  const k = EMPTY_KINDS.includes(kind) ? kind : 'idle';
  const wrap = h('div', { className: `empty empty-${k}`, 'data-empty-kind': k, role: 'status' });
  wrap.appendChild(h('p', { className: 'empty-title', textContent: title || '这里还没有内容' }));
  if (reason) {
    wrap.appendChild(h('p', { className: 'empty-reason', textContent: reason }));
  }
  const actions = suggestions.filter((s) => s && s.label);
  if (actions.length) {
    const list = h('div', { className: 'empty-actions' });
    for (const s of actions) {
      if (s.href) {
        list.appendChild(h('a', {
          className: 'empty-action',
          href: s.href,
          // 站内链接不整页刷新：交给调用方的客户端路由处理。
          onclick: typeof s.onClick === 'function' ? s.onClick : undefined,
          textContent: s.label,
        }));
      } else {
        list.appendChild(h('button', {
          type: 'button',
          className: 'empty-action',
          onclick: typeof s.onClick === 'function' ? s.onClick : undefined,
          textContent: s.label,
        }));
      }
    }
    wrap.appendChild(list);
  }
  return wrap;
}

// EXAMPLE_QUERIES 是空态里推荐的可点示例（**真实可跑的查询串**）。
//
// 选它们的原则：短、常见、不依赖特定库内容（即便库里没数据，点下去也能看到
// plan 区块与 degraded 提示——那本身就是一条有效信息）。
export const EXAMPLE_QUERIES = Object.freeze(['acme', 'Alice', 'deploy']);

// exampleSuggestions 把示例查询变成空态里的按钮（点一下就走真查询）。
export function exampleSuggestions(onPick, queries = EXAMPLE_QUERIES) {
  return queries.map((q) => ({
    label: `试试「${q}」`,
    onClick: () => {
      if (typeof onPick === 'function') onPick(q);
    },
  }));
}
