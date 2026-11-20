// Review Queue 的页面控制器：fetch + URL 同步 + 挂载。
//
// 与 recallmain.js 同一结构：**location 是唯一真源**，state 是它的投影。
// 纯组装逻辑在 reviewpage.js（可被 node --test 直接断言），本文件只负责
// "什么时候请求、请求完把什么放到哪个容器里、什么时候发写请求"。
import { fetchReviewPending, postReview, ApiError } from './api.js';
import { classifyError } from './ui/errorState.js';
import {
  parseReviewView, toReviewQuery, toReviewURL, isReviewPath, MAX_BATCH,
} from './reviewstate.js';
import { renderReviewPageView, pendingRows, rowsBySelection } from './reviewpage.js';
import { requestReview, requestBatchReview, reviewPrompt, batchPrompt } from './reviewflow.js';

// reviewState 是本页的全部状态（没有隐藏变量）。
export const reviewState = {
  view: { scope: '' },
  status: 'idle', // idle | loading | ready | error
  resp: null,
  errors: [],
  error: null,
  /** @type {Set<string>} 勾选中的 edge_id（批量裁决用） */
  selected: new Set(),
  /** @type {{verdict:string, results:Array}|null} 上一批裁决的逐条结果 */
  results: null,
  // 请求序号：并发请求时只有最新一次的结果能落到页面上（与 Recall 页同理）。
  seq: 0,
};

let container = null;

// mount 绑定容器（幂等）。
export function mountReviewPage(el) {
  container = el;
  return container;
}

function currentFetchImpl() {
  return globalThis.fetch.bind(globalThis);
}

// syncReviewURL 把视图写进地址栏。
export function syncReviewURL(view, { replace = false } = {}) {
  const url = toReviewURL(view);
  if (replace) history.replaceState({ view }, '', url);
  else history.pushState({ view }, '', url);
}

// render 把当前状态画到容器里（**唯一**的渲染入口）。
export function render() {
  if (!container) return;
  container.textContent = '';
  container.appendChild(renderReviewPageView({
    view: reviewState.view,
    status: reviewState.status,
    resp: reviewState.resp,
    errors: reviewState.errors,
    error: reviewState.error,
    selected: reviewState.selected,
    results: reviewState.results,
  }, {
    onRetry: () => loadReview(reviewState.view),
    onFixParams: () => loadReview({ scope: '' }),
    onToggle: (edgeId, on) => {
      if (on) reviewState.selected.add(edgeId);
      else reviewState.selected.delete(edgeId);
      render();
    },
    onSelectAll: (on) => {
      const rows = pendingRows(reviewState.resp);
      if (!on) {
        reviewState.selected.clear();
      } else {
        // 全选也受上限约束：超上限时只选前 MAX_BATCH 条并显式说明，
        // 而不是让按钮看起来"全选了"却发不出去。
        reviewState.selected = new Set(rows.slice(0, MAX_BATCH).map((r) => r.edgeId));
      }
      render();
    },
    onClearSelection: () => {
      reviewState.selected.clear();
      render();
    },
    onVerdict: (edgeId, verdict) => submitOne(edgeId, verdict),
    onBatch: (verdict, edgeIds) => submitBatch(verdict, edgeIds),
  }));
}

// openConfirmModal 把二次确认弹窗包成 Promise<boolean>（与 Graph View 共用同一个 DOM 弹窗）。
//
// 取消（点取消 / 点遮罩）= false → 调用方**不得**发请求。
function openConfirmModal({ title, text }) {
  const modal = document.getElementById('confirm-modal');
  const titleEl = document.getElementById('confirm-title');
  const textEl = document.getElementById('confirm-text');
  const okBtn = document.getElementById('confirm-ok');
  const cancelBtn = document.getElementById('confirm-cancel');
  if (!modal || !okBtn || !cancelBtn) return Promise.resolve(false);
  if (titleEl) titleEl.textContent = title;
  if (textEl) textEl.textContent = text;
  return new Promise((resolve) => {
    modal.hidden = false;
    const done = (ok) => {
      modal.hidden = true;
      okBtn.onclick = null;
      cancelBtn.onclick = null;
      modal.onclick = null;
      resolve(ok);
    };
    okBtn.onclick = () => done(true);
    cancelBtn.onclick = () => done(false);
    modal.onclick = (ev) => {
      if (ev.target === modal) done(false);
    };
  });
}

// loadReview 加载并渲染一次视图。
//
// 顺序是刻意的：**先校验参数再发请求**（非法 scope 在本地就报错，不白屏）。
export async function loadReview(view) {
  const { view: parsed, errors } = parseReviewView(toReviewQuery(view));
  reviewState.view = parsed;
  reviewState.errors = errors;
  reviewState.error = null;
  reviewState.selected = new Set(); // 换视图 = 换数据集，陈旧选中项必须清掉
  syncReviewURL(parsed, { replace: false });

  if (errors.length) {
    reviewState.status = 'ready';
    reviewState.resp = null;
    render();
    return;
  }

  reviewState.status = 'loading';
  render();

  const mySeq = ++reviewState.seq;
  let resp;
  try {
    resp = await fetchReviewPending(currentFetchImpl(), toReviewURL(parsed));
  } catch (err) {
    if (mySeq !== reviewState.seq) return;
    reviewState.status = 'error';
    reviewState.resp = null;
    reviewState.error = {
      kind: classifyError(err),
      message: err instanceof ApiError ? err.message : String(err),
      detail: err?.field ? `出错参数：${err.field}` : '',
    };
    render();
    return;
  }
  if (mySeq !== reviewState.seq) return;
  reviewState.resp = resp;
  reviewState.status = 'ready';
  render();
}

// submitOne 裁决单条（沿用 reviewflow 的三道闸门）。
export async function submitOne(edgeId, verdict) {
  const row = pendingRows(reviewState.resp).find((r) => r.edgeId === edgeId);
  const label = row ? `${row.fromName} ⇄ ${row.toName}` : edgeId;
  let outcome;
  try {
    outcome = await requestReview({
      edgeId,
      verdict,
      confirm: () => {
        const p = reviewPrompt(label, verdict);
        return openConfirmModal(p);
      },
      send: ({ edgeId: id, verdict: v }) => postReview(currentFetchImpl(), {
        edgeId: id, verdict: v, confirmed: true,
      }),
    });
  } catch (err) {
    reviewState.results = {
      verdict,
      results: [{ edgeId, ok: false, message: err instanceof ApiError ? err.message : String(err) }],
    };
    render();
    return { sent: false };
  }
  if (!outcome.sent) {
    // 取消：不发请求、不改列表，什么都不提示横幅以外的东西。
    if (outcome.reason === 'not_confirmed') {
      reviewState.results = null;
    }
    render();
    return outcome;
  }
  reviewState.results = { verdict, results: [{ edgeId, ok: true, result: outcome.result }] };
  // 成功后**重新拉取**：融合并会改 ID、判异会置 t_invalid，
  // 前端不能自己把行删掉就宣布"好了"（服务端才是权威）。
  await loadReview(reviewState.view);
  return outcome;
}

// submitBatch 批量裁决：**一次**二次确认 → 逐条 POST → 逐条报结果。
//
// 为什么用 requestBatchReview 而不是把 submitOne 循环一遍：后者会在循环里
// 逐条弹确认框（验收明确禁止），且"取消其中一条"会让整批状态变得不可解释。
export async function submitBatch(verdict, edgeIds) {
  const ids = Array.isArray(edgeIds) ? edgeIds : [];
  let outcome;
  try {
    outcome = await requestBatchReview({
      edgeIds: ids,
      verdict,
      confirm: ({ count }) => openConfirmModal(batchPrompt(count, verdict)),
      send: ({ edgeId, verdict: v }) => postReview(currentFetchImpl(), {
        edgeId, verdict: v, confirmed: true,
      }),
    });
  } catch (err) {
    reviewState.results = {
      verdict,
      results: ids.map((edgeId) => ({
        edgeId, ok: false, message: err instanceof ApiError ? err.message : String(err),
      })),
    };
    render();
    return { sent: false };
  }

  if (outcome.reason === 'over_limit') {
    reviewState.results = {
      verdict,
      results: [{
        edgeId: '', ok: false,
        message: `已选 ${ids.length} 条，超过批量上限 ${MAX_BATCH} 条：请分批裁决。`,
      }],
    };
    render();
    return outcome;
  }
  if (!outcome.sent) {
    if (outcome.reason !== 'not_confirmed') render();
    else {
      reviewState.results = null;
      render();
    }
    return outcome;
  }

  reviewState.results = { verdict, results: outcome.results };
  await loadReview(reviewState.view);
  return outcome;
}

// bindScopeForm 把顶栏的 scope 选择器接到 URL 上（视图即 URL）。
//
// 绑在表单 submit 而不是 change：scope 变化会改变数据集，应该是**显式动作**，
// 而不是滚轮摇中一个选项就冲一次后端。
function bindScopeForm() {
  const form = document.getElementById('review-scope-form');
  const input = document.getElementById('review-scope-input');
  if (!form || !input || form.dataset.bound === '1') return;
  form.dataset.bound = '1';
  form.addEventListener('submit', (ev) => {
    ev.preventDefault();
    loadReview({ scope: input.value || '' });
  });
}

// bootReview 是路由入口：解析当前 URL 并加载。
export async function bootReview(el) {
  mountReviewPage(el);
  document.body.classList.add('route-review');
  bindScopeForm();
  const { view, errors } = parseReviewView(location.search);
  if (errors.length) {
    reviewState.view = view;
    reviewState.errors = errors;
    reviewState.status = 'ready';
    render();
    return;
  }
  await loadReview(view);
}

// handlePopstate 让浏览器前进/后退复用同一套加载逻辑。
export function handlePopstate() {
  if (!isReviewPath(location.pathname)) return;
  const { view, errors } = parseReviewView(location.search);
  if (errors.length) {
    reviewState.view = view;
    reviewState.errors = errors;
    reviewState.status = 'ready';
    render();
    return;
  }
  loadReview(view);
}

export { isReviewPath, MAX_BATCH, rowsBySelection };
