// Review Queue 页面（TD §9 六页 ⑤）。
//
// # 两层结构，理由与 Recall Inspector 相同
//
//   纯函数层：把 GET /api/review/pending 的响应**只读**投影成行（reviewstate.js 的
//     reviewRows / sortRows）。这里没有任何"权重再算一遍"或"从 reason 文本反推
//     这行能不能裁"的逻辑——可裁状态只读后端的 reviewable / invalidated 字段
//     （调研 §6 ⑤(3)：用一个 selector 决定行语义，别让页面猜）。
//   DOM 层：把行落成元素，全部经 textContent / setAttribute 写入。
//
// # 三种"没东西"必须分得开（调研 §10.4 / §6 ⑤「cannot read ≠ nothing there」）
//
//   loading  → 骨架屏（还没读到）
//   error    → errorState + 重试（**读不到**，绝不能显示成"池子是空的"）
//   ready+空 → 空态文案（读到了，确实没有待审边）+ 说明它怎么才会产生
import { h, fmt } from './ui/dom.js';
import { emptyState } from './ui/empty.js';
import { skeleton } from './ui/skeleton.js';
import { errorState } from './ui/errorState.js';
import { banner } from './ui/banner.js';
import {
  MAX_BATCH, reviewRows, sortRows, rowLabel, batchSelectable, graphFocusHref,
  selectedCount, overBatchLimit,
} from './reviewstate.js';

// EMPTY_REASON 是空态里必须给出的"怎么才会产生待审边"。
//
// 只说"暂无数据"是没用的：读者会以为功能坏了。待审边来自写入期的实体去重灰区，
// 因此这句话要说清来源，并给出能立刻执行的下步（去图上按概念看一眼）。
export const EMPTY_REASON =
  '待审边只来自**写入期**的实体去重灰区：两条实体疑似同一（possiblySame）时写入侧' +
  '不自动合并，而是留一条待审边等人或 Agent 裁决。池子为空说明当前没有这种灰区，' +
  '不是功能不可用。';

// VERDICT_LABELS 是两种裁决的按钮文案。
export const VERDICT_LABELS = Object.freeze({ same: '同一', diff: '不同' });

// ---------------------------------------------------------------------------
// 纯函数层（可被单测直接断言）
// ---------------------------------------------------------------------------

// pendingRows 返回**已排序**的行（t_valid 降序、同值 edge_id 升序）。
export function pendingRows(resp) {
  return sortRows(reviewRows(resp));
}

// rowsBySelection 把选中集合过滤成真实存在的行（防止陈旧选中项混进批量）。
//
// 场景：裁决完一批后列表已刷新，但 selected 里可能还留着已消失的 edge_id。
// 若不清理，批量提交会带着不存在的边发出请求，失败原因还是"边不存在"——
// 那是对用户毫无信息量的报错。
export function rowsBySelection(rows, selected) {
  const set = selected instanceof Set ? selected : new Set();
  return rows.filter((r) => set.has(r.edgeId) && batchSelectable(r));
}

// verdictSummary 汇总一批裁决结果（**逐条**，不吞掉部分失败）。
//
// 返回 { okCount, failedCount, ok: [{edgeId,op,status}], failed: [{edgeId,message}] }
export function verdictSummary(results) {
  const list = Array.isArray(results) ? results : [];
  const ok = [];
  const failed = [];
  for (const r of list) {
    if (r && r.ok) {
      ok.push({ edgeId: r.edgeId, op: r.result?.op || '', status: r.result?.status || '' });
    } else {
      failed.push({ edgeId: r?.edgeId || '', message: String(r?.message || '未知错误') });
    }
  }
  return { okCount: ok.length, failedCount: failed.length, ok, failed };
}

// ---------------------------------------------------------------------------
// DOM 层
// ---------------------------------------------------------------------------

// renderReviewPageView 是整页的纯 DOM 组装（不含 fetch、不改 location）。
//
// snapshot: { view, status, resp, errors, error, selected, results }
//   status: 'idle' | 'loading' | 'ready' | 'error'
export function renderReviewPageView(snapshot, handlers = {}) {
  const {
    view = {}, status = 'idle', resp = null, errors = [], error = null,
    selected = new Set(), results = null,
  } = snapshot || {};
  const root = h('div', { className: 'review-body', 'data-status': status });

  // 1) 横幅：结果（ok/失败逐条）、参数错误
  const banners = h('div', { className: 'review-banners' });
  for (const b of renderResultBanners(results, handlers)) banners.appendChild(b);
  for (const e of errors) {
    banners.appendChild(banner('error', `参数错误（${e.field}）：${e.message}`));
  }
  root.appendChild(banners);

  // 2) 三态：加载 / 读不到 / 就绪
  if (status === 'loading' || status === 'idle') {
    root.appendChild(skeleton({ variant: 'review', rows: 6, label: '正在读取待审池' }));
    return root;
  }
  if (errors.length) {
    root.appendChild(errorState({
      kind: 'param',
      message: errors.map((e) => e.message).join('；'),
      detail: `出错参数：${errors.map((e) => e.field).join(', ')}`,
      onFix: handlers.onFixParams,
    }));
    return root;
  }
  if (status === 'error') {
    // **读不到 ≠ 池子空**：这里是 errorState（带重试），不是空态。
    root.appendChild(errorState({
      kind: error?.kind || 'request',
      message: error?.message || '未知错误',
      detail: error?.detail || '',
      onRetry: handlers.onRetry,
    }));
    return root;
  }

  const rows = pendingRows(resp);

  // 3) 空态（读到了，确实没有待审边）
  if (rows.length === 0) {
    root.appendChild(emptyState({
      kind: 'noData',
      title: '待审池是空的：没有疑似同一的实体对',
      reason: EMPTY_REASON,
      suggestions: [
        { label: '去 Graph View 看看', href: '/' },
        { label: '重新读取', onClick: handlers.onRetry },
      ],
    }));
    return root;
  }

  // 4) 就绪：工具条 + 表格
  root.appendChild(renderToolbar(resp, view, rows, selected, handlers));
  root.appendChild(renderTable(rows, selected, handlers));
  return root;
}

// renderToolbar 是"共 N 条 / 已选 K 条 / 上限 M"与批量按钮。
//
// 上限**显示出来**（而不是只在超限时报错）：读者在勾选前就知道边界。
function renderToolbar(resp, view, rows, selected, handlers) {
  const bar = h('div', { className: 'review-toolbar' });
  const meta = resp?.meta || {};
  const chosen = rowsBySelection(rows, selected);

  bar.appendChild(h('span', {
    className: 'review-count',
    textContent: `共 ${rows.length} 条待审 · 已选 ${chosen.length} 条 · 批量上限 ${MAX_BATCH} 条`,
  }));
  const scopeText = meta.scope
    ? `scope=${meta.scope}`
    : `scope=全部（${(meta.scopes || []).join(' / ') || '—'}）`;
  bar.appendChild(h('span', { className: 'review-scope', textContent: scopeText }));

  const over = overBatchLimit(chosen.length);
  if (over) {
    bar.appendChild(banner('error',
      `已选 ${chosen.length} 条，超过批量上限 ${MAX_BATCH} 条：请分批裁决（一次误操作影响的范围必须有界）。`));
  }

  const actions = h('div', { className: 'review-actions' });
  for (const verdict of ['same', 'diff']) {
    actions.appendChild(h('button', {
      type: 'button',
      className: verdict === 'same' ? 'danger' : '',
      disabled: chosen.length === 0 || over ? 'disabled' : undefined,
      'data-action': `batch-${verdict}`,
      textContent: `批量判为${VERDICT_LABELS[verdict]}（${chosen.length}）`,
      onclick: () => {
        if (typeof handlers.onBatch === 'function') handlers.onBatch(verdict, chosen.map((r) => r.edgeId));
      },
    }));
  }
  actions.appendChild(h('button', {
    type: 'button',
    'data-action': 'clear-selection',
    disabled: chosen.length === 0 ? 'disabled' : undefined,
    textContent: '清空选择',
    onclick: () => {
      if (typeof handlers.onClearSelection === 'function') handlers.onClearSelection();
    },
  }));
  bar.appendChild(actions);

  // 双向连线：从待审池回到 Graph View（概览模式，蓝边可裁入口在那里）。
  bar.appendChild(h('a', {
    className: 'review-jump',
    href: '/',
    textContent: '← 回到 Graph View（蓝边 = 可裁入口）',
  }));
  return bar;
}

// renderTable 是待审池列表。
//
// 列：勾选 / edge_id / 两端节点名 / weight / t_valid / scope / reason / 动作。
function renderTable(rows, selected, handlers) {
  const set = selected instanceof Set ? selected : new Set();
  const wrap = h('div', { className: 'review-table-wrap' });
  const table = h('table', { className: 'table review-table' });

  const thead = h('thead');
  const hr = h('tr');
  const allChecked = rows.length > 0 && rows.every((r) => set.has(r.edgeId) || !batchSelectable(r));
  hr.appendChild(h('th', { className: 'review-check' }, [
    h('input', {
      type: 'checkbox',
      'aria-label': '全选',
      'data-action': 'select-all',
      checked: allChecked ? 'checked' : undefined,
      onchange: () => {
        if (typeof handlers.onSelectAll === 'function') handlers.onSelectAll(!allChecked);
      },
    }),
  ]));
  for (const [label, cls] of [
    ['edge_id', ''], ['两端节点', ''], ['weight', 'num'],
    ['t_valid', 'num'], ['scope', ''], ['reason', ''], ['动作', ''],
  ]) {
    hr.appendChild(h('th', { className: cls || undefined, textContent: label }));
  }
  thead.appendChild(hr);
  table.appendChild(thead);

  const tbody = h('tbody');
  for (const row of rows) {
    tbody.appendChild(renderRow(row, set.has(row.edgeId), handlers));
  }
  table.appendChild(tbody);
  wrap.appendChild(table);
  return wrap;
}

// renderRow 是一行（每行都能单独裁决，也能被勾选进批量）。
function renderRow(row, checked, handlers) {
  const selectable = batchSelectable(row);
  const tr = h('tr', { className: selectable ? '' : 'review-row-locked', 'data-edge-id': row.edgeId });

  tr.appendChild(h('td', { className: 'review-check' }, selectable
    ? h('input', {
      type: 'checkbox',
      'aria-label': `选择 ${row.edgeId}`,
      'data-edge-id': row.edgeId,
      checked: checked ? 'checked' : undefined,
      onchange: (ev) => {
        const on = ev?.target ? ev.target.checked !== false : !checked;
        if (typeof handlers.onToggle === 'function') handlers.onToggle(row.edgeId, on);
      },
    })
    : h('span', { className: 'review-locked', textContent: '—' })));

  tr.appendChild(h('td', { className: 'review-edge', textContent: row.edgeId }));
  tr.appendChild(h('td', {
    className: 'review-pair',
    textContent: `${row.fromName}（${row.from}） ⇄ ${row.toName}（${row.to}）`,
  }));
  tr.appendChild(h('td', { className: 'num', textContent: fmt(row.weight, 3) }));
  // t_valid 同时给原始值与可读时刻：可读时刻便于人看，原始值便于与接口对照。
  tr.appendChild(h('td', { className: 'num', textContent: String(row.tValid) }));
  tr.appendChild(h('td', { textContent: row.scope }));
  tr.appendChild(h('td', { className: 'review-reason', textContent: row.reason }));

  const actions = h('td', { className: 'review-row-actions' });
  if (selectable) {
    for (const verdict of ['same', 'diff']) {
      actions.appendChild(h('button', {
        type: 'button',
        'data-action': `verdict-${verdict}`,
        'data-edge-id': row.edgeId,
        textContent: VERDICT_LABELS[verdict],
        onclick: () => {
          if (typeof handlers.onVerdict === 'function') handlers.onVerdict(row.edgeId, verdict);
        },
      }));
    }
  } else {
    // 状态由后端显式给出（reviewable=false）：这里只展示，不让点。
    actions.appendChild(h('span', {
      className: 'review-locked',
      textContent: row.invalidated ? '已裁决（不可再裁）' : '不可裁决',
    }));
  }
  // 双向连线：每行都能跳回 /graph?focus=（TD §9 的 Review → Graph 箭头）。
  actions.appendChild(h('a', {
    className: 'review-jump',
    href: graphFocusHref(row.to || row.from),
    'data-focus': row.to || row.from,
    textContent: '在图上查看 →',
  }));
  tr.appendChild(actions);
  return tr;
}

// renderResultBanners 把**批量裁决的逐条结果**渲染成横幅。
//
// 为什么必须逐条：整批里可能只有几条失败（例如某条边刚被别人裁掉），
// 只报"成功 N 条"会把失败吞掉，用户会以为全部完成。
export function renderResultBanners(results, handlers = {}) {
  if (!results) return [];
  const sum = verdictSummary(results.results || results);
  const out = [];
  if (sum.okCount > 0) {
    const box = h('div', { className: 'banner ok' });
    box.appendChild(h('span', { textContent: `本次已裁决 ${sum.okCount} 条` }));
    // 逐条列出走的是哪条 op（merge / disambiguate），便于核对每条走了哪条写路径。
    const links = h('span', { className: 'review-result-links' });
    for (const it of sum.ok) {
      links.appendChild(h('span', {
        className: 'review-result-item',
        textContent: ` · ${it.edgeId}${it.op ? `（op=${it.op}）` : ''}`,
      }));
    }
    box.appendChild(links);
    out.push(box);
  }
  if (sum.failedCount > 0) {
    const box = h('div', { className: 'banner error' });
    box.appendChild(h('span', { textContent: `失败 ${sum.failedCount} 条（逐条原因如下）：` }));
    const list = h('ul', { className: 'review-failed-list' });
    for (const f of sum.failed) {
      list.appendChild(h('li', { textContent: `${f.edgeId || '(未知边)'}：${f.message}` }));
    }
    box.appendChild(list);
    out.push(box);
  }
  return out;
}

export { MAX_BATCH, rowLabel, batchSelectable, graphFocusHref, selectedCount, overBatchLimit };
