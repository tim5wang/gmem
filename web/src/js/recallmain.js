// Recall Inspector 的页面控制器：fetch + URL 同步 + 挂载。
//
// 与 main.js（Graph View）同一结构：**location 是唯一真源**，state 是它的投影。
// 纯组装逻辑在 recallpage.js（可被 node --test 直接断言），本文件只负责
// "什么时候请求、请求完把什么放到哪个容器里"。
import { fetchRecall, ApiError } from './api.js';
import { classifyError } from './ui/errorState.js';
import { parseRecallView, toRecallQuery, toRecallURL, isRecallPath } from './recallstate.js';
import { renderRecallPageView } from './recallpage.js';

// recallState 是本页的全部状态（没有隐藏变量）。
export const recallState = {
  view: { q: '', scope: '', kinds: [], lanes: [], asOf: null, k: 10, item: '' },
  status: 'idle', // idle | loading | ready | error
  resp: null,
  errors: [],
  error: null,
  // 请求序号：并发请求（快速连打）时只有最新一次的结果能落到页面上，
  // 否则慢请求后到会覆盖快请求的新结果 —— 表现为"页面显示的是上一次的答案"。
  seq: 0,
};

let container = null;

// mount 绑定容器（幂等：重复调用只换容器，不重复绑事件）。
export function mountRecallPage(el) {
  container = el;
  return container;
}

function currentFetchImpl() {
  return globalThis.fetch.bind(globalThis);
}

// syncURL 把视图写进地址栏（replace=true 时不产生历史记录）。
//
// as_of 的清除依赖这里：clearAsOf 之后序列化出的 URL 里没有 as_of 键，
// history 替换后地址栏就真的没有了（验收：「URL 中 as_of 消失」）。
export function syncRecallURL(view, { replace = false } = {}) {
  const qs = toRecallQuery(view);
  const url = `/recall${qs}`;
  if (replace) history.replaceState({ view }, '', url);
  else history.pushState({ view }, '', url);
}

// render 把当前状态画到容器里（**唯一**的渲染入口）。
export function render() {
  if (!container) return;
  container.textContent = '';
  container.appendChild(renderRecallPageView({
    view: recallState.view,
    status: recallState.status,
    resp: recallState.resp,
    errors: recallState.errors,
    error: recallState.error,
  }, {
    onSelectItem: (id) => {
      // 选中项也进 URL：刷新后仍是同一条被选中（验收「URL 即视图」）。
      const next = { ...recallState.view, item: id || '' };
      recallState.view = next;
      syncRecallURL(next, { replace: true });
      render();
    },
    onViewChange: (next) => {
      // 过滤条件变化 → 重新请求（k/scope/kinds/lanes 都会改变结果集）。
      loadRecall(next);
    },
    onQuery: (q) => {
      loadRecall({ ...recallState.view, q, item: '' });
    },
    onBackToNow: () => {
      // 「回到现在」= 去掉 as_of 后**重新请求**（而不是在前端过滤数据：
      // 历史视角与当前视角的结果集不同，前端没有做这个换算的信息）。
      loadRecall({ ...recallState.view, asOf: null, item: '' });
    },
    onRetry: () => {
      loadRecall(recallState.view);
    },
    onFixParams: () => {
      // 参数错误的下一步是"去掉错参数"：回到只带 q 的最小视图。
      //
      // 不在这里再调 syncRecallURL：loadRecall 内部会用自己的解析结果写 URL，
      // 这里再写一次会把刚被规范化/纠正的地址盖回去（例如非法 k 被丢掉以后）。
      loadRecall({ q: recallState.view.q, scope: '', kinds: [], lanes: [], asOf: null, k: 10, item: '' });
    },
  }));
}

// loadRecall 加载并渲染一次视图。
//
// 顺序是刻意的：**先校验参数再发请求**。非法参数（as_of 非数字、k 超限）
// 必须显式报错，而不是把错参数发给服务端然后显示一个 400（那样页面上
// 只剩服务端的一句话，地址栏里还留着一个看起来合法的 URL）。
export async function loadRecall(view) {
  const { view: parsed, errors } = parseRecallView(toRecallQuery(view));
  recallState.view = parsed;
  recallState.errors = errors;
  recallState.error = null;

  // URL 先行：即便参数非法，地址栏也要如实反映用户输入（否则刷新会"自愈"，
  // 让人以为刚才那个错参数没发生过）。
  syncRecallURL(parsed, { replace: false });

  if (errors.length) {
    recallState.status = 'ready';
    recallState.resp = null;
    render();
    return;
  }
  if (!parsed.q) {
    // 空查询：**不发请求**（服务端会 400），直接空态。
    recallState.status = 'idle';
    recallState.resp = null;
    render();
    return;
  }

  recallState.status = 'loading';
  render();

  const mySeq = ++recallState.seq;
  let resp;
  try {
    resp = await fetchRecall(currentFetchImpl(), toRecallURL(parsed));
  } catch (err) {
    if (mySeq !== recallState.seq) return; // 已有更新的请求在飞
    recallState.status = 'error';
    recallState.resp = null;
    recallState.error = {
      kind: classifyError(err),
      message: err instanceof ApiError ? err.message : String(err),
      detail: err?.field ? `出错参数：${err.field}` : '',
    };
    render();
    return;
  }
  if (mySeq !== recallState.seq) return;
  recallState.resp = resp;
  recallState.status = 'ready';
  render();
}

// bootRecall 是路由入口：解析当前 URL 并加载。
export async function bootRecall(el) {
  mountRecallPage(el);
  document.body.classList.add('route-recall');
  const { view, errors } = parseRecallView(location.search);
  if (errors.length) {
    recallState.view = view;
    recallState.errors = errors;
    recallState.status = 'ready';
    render();
    return;
  }
  await loadRecall(view);
}

// handlePopstate 让浏览器前进/后退复用同一套加载逻辑。
export function handlePopstate() {
  if (!isRecallPath(location.pathname)) return;
  const { view, errors } = parseRecallView(location.search);
  if (errors.length) {
    recallState.view = view;
    recallState.errors = errors;
    recallState.status = 'ready';
    render();
    return;
  }
  loadRecall(view);
}

// 便于单测注入 fetch 之外的依赖（当前只是把状态暴露出来）。
export { isRecallPath };
