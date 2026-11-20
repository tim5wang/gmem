// 前端入口：**唯一的** bundle 起点（build.mjs 的 entryPoints 指向它）。
//
// 为什么需要这一层：六页共用一份 index.html（前置 1 的服务端通配路由），
// 因此"打开的是哪一页"必须在前端判定一次，并且**只启动那一页**。
// 若把分发写在 main.js 里，Graph View 会在 /recall 上照样自启动并去请求
// /api/graph —— 页面上看不到它，但网络面板里全是无关请求，且第一次
// 交互会拿到另一页的 state。
//
// 分发是纯函数（router.js），本文件只做"按判定结果调用对应的 boot"。
import { matchRoute, isImplemented, notImplementedMessage, PAGE_TITLES } from './router.js';
import { boot as bootGraph } from './main.js';
import { bootRecall } from './recallmain.js';
import { bootReview } from './reviewmain.js';
import { h } from './ui/dom.js';

// hideAllPages 把几页的容器都藏起来（分发前归一，避免上一页残留）。
function hideAllPages() {
  for (const id of ['page-recall', 'page-review', 'page-generic']) {
    const el = document.getElementById(id);
    if (el) el.hidden = true;
  }
  document.body.classList.remove('route-recall', 'route-review', 'route-generic');
}

// renderGeneric 渲染"未实现 / 404"视图。
//
// 两者话术必须不同：未实现是"规划中但还没做"，404 是"这个地址没有对应页面"。
// 混成一句会让人把打错的 URL 当成功能缺失（或反之）。
function renderGeneric(container, { page, path }) {
  if (!container) return;
  container.textContent = '';
  const wrap = h('div', { className: 'page-missing' });
  if (page === 'missing') {
    wrap.appendChild(h('h1', { textContent: '页面不存在' }));
    wrap.appendChild(h('p', { textContent: `地址 ${path} 没有对应的页面。` }));
  } else {
    wrap.appendChild(h('h1', { textContent: PAGE_TITLES[page] || page }));
    wrap.appendChild(h('p', { textContent: notImplementedMessage(page) }));
  }
  const nav = h('div', { className: 'chips' });
  nav.appendChild(h('a', { className: 'empty-action', href: '/', textContent: '← Graph View' }));
  nav.appendChild(h('a', { className: 'empty-action', href: '/recall?q=acme', textContent: 'Recall Inspector →' }));
  wrap.appendChild(nav);
  container.appendChild(wrap);
  container.hidden = false;
  document.body.classList.add('route-generic');
}

// dispatch 是路由分发的唯一入口（boot 与 popstate 共用）。
export async function dispatch(pathname) {
  const { page, path } = matchRoute(pathname);
  hideAllPages();

  if (page === 'graph') {
    document.title = 'Gmem · Graph View';
    await bootGraph();
    return { page, path };
  }
  if (page === 'recall') {
    document.title = 'Gmem · Recall Inspector';
    const el = document.getElementById('page-recall');
    if (el) el.hidden = false;
    await bootRecall(document.getElementById('recall-root'));
    return { page, path };
  }
  if (page === 'review') {
    document.title = 'Gmem · Review Queue';
    const el = document.getElementById('page-review');
    if (el) el.hidden = false;
    await bootReview(document.getElementById('review-root'));
    return { page, path };
  }
  renderGeneric(document.getElementById('page-generic'), { page, path });
  if (!isImplemented(page)) {
    document.title = `Gmem · ${PAGE_TITLES[page] || page}`;
  }
  return { page, path };
}

// 启动：只在浏览器里跑（node --test 导入本模块时不会误触发 DOM 逻辑）。
if (typeof document !== 'undefined' && typeof location !== 'undefined') {
  dispatch(location.pathname);
  // 站内导航走客户端路由（六页共享一份产物，整页刷新没有必要）。
  window.addEventListener('popstate', () => {
    dispatch(location.pathname);
  });
  // 拦截站内链接点击：只有同源、且不是新标签/修饰键时才接管。
  document.addEventListener('click', (ev) => {
    if (ev.defaultPrevented || ev.button !== 0 || ev.metaKey || ev.ctrlKey || ev.shiftKey || ev.altKey) return;
    const a = ev.target && ev.target.closest ? ev.target.closest('a[href]') : null;
    if (!a) return;
    const href = a.getAttribute('href');
    if (!href || !href.startsWith('/') || href.startsWith('//')) return;
    if (a.target && a.target !== '_self') return;
    ev.preventDefault();
    history.pushState({}, '', href);
    dispatch(location.pathname);
  });
}
