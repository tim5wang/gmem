// Review Queue 前端单测（node --test，无浏览器、无网络）。
//
// 覆盖验收点名的几条**不能靠肉眼**的结论：
//   1) 列表条数 === 响应条数；排序确定（t_valid 降序、同值 edge_id 升序）
//   2) 取消二次确认 → **零网络请求**（fetch 层打桩断言调用次数）
//   3) 确认后 body 带 confirmed:true，且行从列表移除（靠重新拉取，不在前端删）
//   4) 批量：一次确认 + 逐条 POST；部分失败**逐条报**，不静默
//   5) 空态文案（不是空白、无骨架残留），且与「读不到」分得开
//   6) 行语义来自后端字段（reviewable / invalidated），不从 reason 猜
import test from 'node:test';
import assert from 'node:assert/strict';

import { installDom, textOf, findAll } from './dom-stub.mjs';

installDom();

const {
  pendingRows, rowsBySelection, verdictSummary, renderReviewPageView,
  renderResultBanners, EMPTY_REASON,
} = await import('../js/reviewpage.js');

const {
  parseReviewView, toReviewQuery, toReviewURL, isReviewPath, graphFocusHref,
  reviewRows, sortRows, MAX_BATCH, SCOPES,
} = await import('../js/reviewstate.js');

const {
  requestReview, requestBatchReview, batchPrompt, reviewPrompt, MAX_BATCH: FLOW_MAX_BATCH,
} = await import('../js/reviewflow.js');

const { postReview, fetchReviewPending, ApiError } = await import('../js/api.js');

const { matchRoute, isImplemented } = await import('../js/router.js');

// ---------------------------------------------------------------------------
// fixture：一份结构完整的 /api/review/pending 响应
// ---------------------------------------------------------------------------

export function makePending(overrides = {}) {
  return {
    pending: [
      {
        edge_id: 'e-b', from: 'bob', to: 'bob-dup', from_name: 'Bob', to_name: 'Bob (dup)',
        weight: 0.8, t_valid: 200, scope: 'user',
        reason: '疑似同一实体，待裁决（绝不自动合并）',
        invalidated: false, reviewable: true,
      },
      {
        edge_id: 'e-a', from: 'alice', to: 'alice-dup', from_name: 'Alice', to_name: 'Alice (dup)',
        weight: 0.9, t_valid: 300, scope: 'user',
        reason: '疑似同一实体，待裁决（绝不自动合并）',
        invalidated: false, reviewable: true,
      },
      {
        edge_id: 'e-c', from: 'run-1', to: 'run-1-dup', from_name: 'Run 1', to_name: 'Run 1 (dup)',
        weight: 0.7, t_valid: 100, scope: 'agent',
        reason: '疑似同一实体，待裁决（绝不自动合并）',
        invalidated: false, reviewable: true,
      },
    ],
    count: 3,
    meta: { scope: '', scopes: ['user', 'agent', 'project', 'global'], max_batch: MAX_BATCH },
    ...overrides,
  };
}

// ---------------------------------------------------------------------------
// 1. 行投影与确定性排序
// ---------------------------------------------------------------------------

test('reviewRows 逐字段透传，不改写权重/时间戳', () => {
  const rows = reviewRows(makePending());
  assert.equal(rows.length, 3);
  const a = rows.find((r) => r.edgeId === 'e-a');
  assert.equal(a.weight, 0.9);
  assert.equal(a.tValid, 300);
  assert.equal(a.fromName, 'Alice');
  assert.equal(a.reviewable, true);
  assert.equal(a.invalidated, false);
});

test('排序：t_valid 降序，同值按 edge_id 升序（确定性）', () => {
  const rows = pendingRows(makePending());
  // t_valid: e-a 300 > e-b 200 > e-c 100
  assert.deepEqual(rows.map((r) => r.edgeId), ['e-a', 'e-b', 'e-c']);

  // 同值：必须按 edge_id 升序，且与输入顺序无关（这里故意倒序喂）
  const tie = sortRows([
    { edgeId: 'e-z', tValid: 100 },
    { edgeId: 'e-m', tValid: 100 },
    { edgeId: 'e-a', tValid: 100 },
  ]);
  assert.deepEqual(tie.map((r) => r.edgeId), ['e-a', 'e-m', 'e-z']);
  // 同一份数据排两次，结果必须逐项相同（不稳定排序会让行在两次渲染间换位）。
  const again = sortRows([
    { edgeId: 'e-a', tValid: 100 },
    { edgeId: 'e-z', tValid: 100 },
    { edgeId: 'e-m', tValid: 100 },
  ]);
  assert.deepEqual(again.map((r) => r.edgeId), tie.map((r) => r.edgeId));
});

test('sortRows 不修改入参（纯函数）', () => {
  const input = [
    { edgeId: 'e-b', tValid: 1 },
    { edgeId: 'e-a', tValid: 2 },
  ];
  const copy = JSON.parse(JSON.stringify(input));
  sortRows(input);
  assert.deepEqual(input, copy);
});

// ---------------------------------------------------------------------------
// 2. 行语义来自后端字段，不从字符串猜
// ---------------------------------------------------------------------------

test('行语义只读 reviewable / invalidated，不解析 reason 文本', () => {
  // reason 故意写成"可裁决"字样，但 reviewable=false → 仍不得可裁。
  const rows = reviewRows({
    pending: [
      {
        edge_id: 'e-x', from: 'a', to: 'b', from_name: 'A', to_name: 'B',
        weight: 1, t_valid: 10, scope: 'user',
        reason: '这条可以裁决（诱导性文案）',
        invalidated: true, reviewable: false,
      },
    ],
  });
  assert.equal(rows[0].reviewable, false);
  assert.equal(rows[0].invalidated, true);
  // 不可裁的行不进入批量选择。
  assert.equal(rowsBySelection(rows, new Set(['e-x'])).length, 0);
});

test('页面上不可裁的行不渲染裁决按钮', () => {
  const view = renderReviewPageView({
    view: { scope: '' },
    status: 'ready',
    resp: {
      pending: [
        {
          edge_id: 'e-lock', from: 'a', to: 'b', from_name: 'A', to_name: 'B',
          weight: 1, t_valid: 10, scope: 'user', reason: 'r',
          invalidated: true, reviewable: false,
        },
      ],
      count: 1,
      meta: { scope: '', scopes: [], max_batch: MAX_BATCH },
    },
  }, {});
  const buttons = findAll(view, (n) => n.getAttribute && n.getAttribute('data-action'));
  const verdicts = buttons.filter((b) => String(b.getAttribute('data-action')).startsWith('verdict-'));
  assert.equal(verdicts.length, 0, '不可裁的行不得出现「同一/不同」按钮');
  assert.match(textOf(view), /不可再裁|不可裁决/);
});

// ---------------------------------------------------------------------------
// 3. 列表条数 === 响应条数；每行该显示的六列都在
// ---------------------------------------------------------------------------

test('列表行数等于响应条数，且六列内容都在', () => {
  const resp = makePending();
  const view = renderReviewPageView({ view: { scope: '' }, status: 'ready', resp }, {});
  const rows = findAll(view, (n) => n.tagName === 'tr' && n.getAttribute('data-edge-id'));
  assert.equal(rows.length, resp.pending.length);

  const txt = textOf(view);
  // edge_id / 两端节点名（含 ID）/ weight / t_valid / scope / reason
  for (const want of ['e-a', 'e-b', 'e-c', 'Alice', 'Alice (dup)', 'Run 1', '300', 'agent', '疑似同一实体']) {
    assert.ok(txt.includes(want), `页面缺少 ${want}`);
  }
  // weight 必须固定三位小数（与其它页一致的数字格式）
  assert.ok(txt.includes('0.900'), 'weight 必须用固定小数位显示');
});

test('表格按 t_valid 降序渲染（页面顺序 = 排序结果）', () => {
  const view = renderReviewPageView({ view: { scope: '' }, status: 'ready', resp: makePending() }, {});
  const rows = findAll(view, (n) => n.tagName === 'tr' && n.getAttribute('data-edge-id'));
  assert.deepEqual(rows.map((r) => r.getAttribute('data-edge-id')), ['e-a', 'e-b', 'e-c']);
});

// ---------------------------------------------------------------------------
// 4. 空态 / 加载态 / 错误态：三者不可混淆
// ---------------------------------------------------------------------------

test('空池显示空态文案（并说明怎么才会产生待审边），无骨架残留', () => {
  const view = renderReviewPageView({
    view: { scope: '' },
    status: 'ready',
    resp: { pending: [], count: 0, meta: { scope: '', scopes: [], max_batch: MAX_BATCH } },
  }, {});
  const txt = textOf(view);
  assert.match(txt, /待审池是空的/);
  assert.match(txt, /写入期/); // 说明来源：怎么才会产生待审边
  assert.equal(findAll(view, (n) => String(n.className).includes('skeleton')).length, 0,
    '空态不得残留骨架屏');
  assert.equal(findAll(view, (n) => n.getAttribute('data-empty-kind')).length, 1);
  assert.ok(EMPTY_REASON.length > 20);
});

test('加载中显示 skeleton，且不显示空态', () => {
  const view = renderReviewPageView({ view: { scope: '' }, status: 'loading', resp: null }, {});
  const sk = findAll(view, (n) => n.getAttribute && n.getAttribute('data-skeleton-variant') === 'review');
  assert.equal(sk.length, 1, '加载中必须有 skeleton');
  assert.equal(findAll(view, (n) => n.getAttribute('data-empty-kind')).length, 0,
    '加载中不得显示空态（会被误读成"池子是空的"）');
});

test('读不到后端 → errorState + 重试，绝不显示成空态', () => {
  let retried = 0;
  const view = renderReviewPageView({
    view: { scope: '' },
    status: 'error',
    resp: null,
    error: { kind: 'network', message: '连不上服务', detail: '' },
  }, { onRetry: () => { retried += 1; } });

  assert.equal(findAll(view, (n) => n.getAttribute('data-error-kind')).length, 1);
  assert.equal(findAll(view, (n) => n.getAttribute('data-empty-kind')).length, 0,
    '读不到 ≠ 池子空（cannot read ≠ nothing there）');
  assert.match(textOf(view), /连不上服务/);
  const retry = findAll(view, (n) => n.tagName === 'button' && n.textContent === '重试');
  assert.equal(retry.length, 1, '错误态必须给重试');
  retry[0].onclick();
  assert.equal(retried, 1);
});

test('参数错误不显示成"池子空"，也不给必然失败的重试按钮', () => {
  const view = renderReviewPageView({
    view: { scope: 'bogus' },
    status: 'ready',
    resp: null,
    errors: [{ field: 'scope', message: 'scope 必须是 user / agent / project / global 之一' }],
  }, { onFixParams: () => {} });
  assert.equal(findAll(view, (n) => n.getAttribute('data-empty-kind')).length, 0);
  assert.match(textOf(view), /参数错误/);
  assert.equal(findAll(view, (n) => n.tagName === 'button' && n.textContent === '重试').length, 0);
});

// ---------------------------------------------------------------------------
// 5. 二次确认：取消零请求、确认才发写请求
// ---------------------------------------------------------------------------

test('单条：取消二次确认 → **零**网络请求', async () => {
  let fetches = 0;
  const fetchImpl = () => { fetches += 1; throw new Error('不该被调用'); };
  const outcome = await requestReview({
    edgeId: 'e-a',
    verdict: 'same',
    confirm: () => false,
    send: ({ edgeId, verdict }) => postReview(fetchImpl, { edgeId, verdict, confirmed: true }),
  });
  assert.equal(outcome.sent, false);
  assert.equal(outcome.reason, 'not_confirmed');
  assert.equal(fetches, 0, '取消后不得发出任何请求');
});

test('单条：确认后发出 POST /api/review，body 含 confirmed:true', async () => {
  const seen = [];
  const fetchImpl = async (url, opts) => {
    seen.push({ url, opts });
    return {
      ok: true, status: 200,
      text: async () => JSON.stringify({ edge_id: 'e-a', verdict: 'same', op: 'merge', status: 'ok' }),
    };
  };
  const outcome = await requestReview({
    edgeId: 'e-a',
    verdict: 'same',
    confirm: () => true,
    send: ({ edgeId, verdict }) => postReview(fetchImpl, { edgeId, verdict, confirmed: true }),
  });
  assert.equal(outcome.sent, true);
  assert.equal(seen.length, 1);
  assert.equal(seen[0].url, '/api/review');
  assert.equal(seen[0].opts.method, 'POST');
  assert.deepEqual(JSON.parse(seen[0].opts.body), { edge_id: 'e-a', verdict: 'same', confirmed: true });
});

test('二次确认文案说清后果（same=合并 / diff=negative_SameAs）', () => {
  assert.match(reviewPrompt('A ⇄ B', 'same').title, /确认合并/);
  assert.match(reviewPrompt('A ⇄ B', 'same').text, /可回溯重定向/);
  assert.match(reviewPrompt('A ⇄ B', 'diff').text, /negative_SameAs/);
});

// ---------------------------------------------------------------------------
// 6. 批量：一次确认 + 逐条 POST + 部分失败逐条报
// ---------------------------------------------------------------------------

test('批量：只弹**一次**确认，确认后逐条 POST（每条都带 confirmed:true）', async () => {
  let confirms = 0;
  const bodies = [];
  const fetchImpl = async (url, opts) => {
    bodies.push(JSON.parse(opts.body));
    return { ok: true, status: 200, text: async () => JSON.stringify({ op: 'merge', status: 'ok' }) };
  };
  const outcome = await requestBatchReview({
    edgeIds: ['e-a', 'e-b', 'e-c'],
    verdict: 'same',
    confirm: () => { confirms += 1; return true; },
    send: ({ edgeId, verdict }) => postReview(fetchImpl, { edgeId, verdict, confirmed: true }),
  });
  assert.equal(confirms, 1, '批量裁决只允许弹一次确认（循环里逐条弹是禁止的）');
  assert.equal(bodies.length, 3);
  assert.deepEqual(bodies.map((b) => b.edge_id), ['e-a', 'e-b', 'e-c']);
  assert.ok(bodies.every((b) => b.confirmed === true));
  assert.equal(outcome.results.filter((r) => r.ok).length, 3);
});

test('批量：取消确认 → 零请求', async () => {
  let fetches = 0;
  const fetchImpl = () => { fetches += 1; throw new Error('不该被调用'); };
  const outcome = await requestBatchReview({
    edgeIds: ['e-a', 'e-b'],
    verdict: 'diff',
    confirm: () => false,
    send: ({ edgeId, verdict }) => postReview(fetchImpl, { edgeId, verdict, confirmed: true }),
  });
  assert.equal(outcome.sent, false);
  assert.equal(fetches, 0);
});

test('批量：部分失败**逐条报**，不整批静默、不中断其余边', async () => {
  const view = { scope: '' };
  void view;
  // e-b 失败（例如刚被别人裁掉），其余成功。
  const fetchImpl = async (url, opts) => {
    const body = JSON.parse(opts.body);
    if (body.edge_id === 'e-b') {
      return {
        ok: false, status: 400,
        text: async () => JSON.stringify({ error: 'bad_request', message: '边 e-b 已失效，不能裁决', field: 'edge_id' }),
      };
    }
    return { ok: true, status: 200, text: async () => JSON.stringify({ op: 'merge', status: 'ok' }) };
  };
  const outcome = await requestBatchReview({
    edgeIds: ['e-a', 'e-b', 'e-c'],
    verdict: 'same',
    confirm: () => true,
    send: ({ edgeId, verdict }) => postReview(fetchImpl, { edgeId, verdict, confirmed: true }),
  });
  assert.equal(outcome.results.length, 3, '三条边都要有结果（失败不得被吞掉）');
  const sum = verdictSummary(outcome.results);
  assert.equal(sum.okCount, 2);
  assert.equal(sum.failedCount, 1);
  assert.equal(sum.failed[0].edgeId, 'e-b');
  assert.match(sum.failed[0].message, /已失效/);

  // 页面上必须同时出现成功条数与**逐条失败原因**。
  const banners = renderResultBanners({ verdict: 'same', results: outcome.results }, {});
  const txt = banners.map((b) => textOf(b)).join(' | ');
  assert.match(txt, /本次已裁决 2 条/);
  assert.match(txt, /失败 1 条/);
  assert.match(txt, /e-b：边 e-b 已失效，不能裁决/);
});

test('批量上限：超过上限直接拒绝（不静默截断），且 UI 与接口的上限一致', async () => {
  assert.equal(MAX_BATCH, 50, '批量上限必须是 50');
  assert.equal(FLOW_MAX_BATCH, MAX_BATCH, 'UI 与 reviewflow 的上限必须一致');
  assert.equal(MAX_BATCH, makePending().meta.max_batch, 'UI 上限必须等于接口 meta.max_batch');

  let fetches = 0;
  const fetchImpl = () => { fetches += 1; throw new Error('不该被调用'); };
  const outcome = await requestBatchReview({
    edgeIds: Array.from({ length: MAX_BATCH + 1 }, (_, i) => `e-${i}`),
    verdict: 'same',
    confirm: () => true,
    send: ({ edgeId, verdict }) => postReview(fetchImpl, { edgeId, verdict, confirmed: true }),
  });
  assert.equal(outcome.sent, false);
  assert.equal(outcome.reason, 'over_limit');
  assert.equal(fetches, 0, '超限时一条请求都不该发出');
});

test('批量确认文案列出将影响的全部边数与不可逆提示', () => {
  const p = batchPrompt(7, 'same');
  assert.match(p.title, /7 条/);
  assert.match(p.text, /7 条/);
  assert.match(p.text, /不可逆/);
  const d = batchPrompt(3, 'diff');
  assert.match(d.text, /3 条/);
  assert.match(d.text, /不可逆/);
  assert.match(d.text, /negative_SameAs/);
});

test('UI 上超上限时禁用批量按钮并显式提示', () => {
  const many = Array.from({ length: MAX_BATCH + 1 }, (_, i) => ({
    edge_id: `e-${String(i).padStart(3, '0')}`, from: 'a', to: 'b', from_name: 'A', to_name: 'B',
    weight: 1, t_valid: 10, scope: 'user', reason: 'r', invalidated: false, reviewable: true,
  }));
  const selected = new Set(many.map((r) => r.edge_id));
  const view = renderReviewPageView({
    view: { scope: '' }, status: 'ready',
    resp: { pending: many, count: many.length, meta: { scope: '', scopes: [], max_batch: MAX_BATCH } },
    selected,
  }, {});
  const txt = textOf(view);
  assert.match(txt, /超过批量上限 50 条/);
  const batchButtons = findAll(view, (n) => String(n.getAttribute && n.getAttribute('data-action')).startsWith('batch-'));
  assert.ok(batchButtons.length > 0);
  assert.ok(batchButtons.every((b) => b.getAttribute('disabled') === 'disabled'),
    '超上限时批量按钮必须禁用');
});

// ---------------------------------------------------------------------------
// 7. 跳转与双向连线
// ---------------------------------------------------------------------------

test('graphFocusHref 生成 /?focus= 链接（裁决后去图上看合并结果）', () => {
  assert.equal(graphFocusHref('alice'), '/?focus=alice');
  assert.equal(graphFocusHref('a b'), '/?focus=a%20b');
  assert.equal(graphFocusHref(''), '/');
});

test('每行都有跳回图上的链接，页面有回到 Graph View 的入口', () => {
  const view = renderReviewPageView({ view: { scope: '' }, status: 'ready', resp: makePending() }, {});
  const links = findAll(view, (n) => n.tagName === 'a' && String(n.getAttribute('href') || '').includes('focus='));
  assert.equal(links.length, 3, '每行都要有跳回 /graph?focus= 的链接');
  assert.ok(links.some((a) => a.getAttribute('href') === '/?focus=bob-dup'));
});

// ---------------------------------------------------------------------------
// 8. URL 即视图 / 路由
// ---------------------------------------------------------------------------

test('parseReviewView 解析 scope；非法 scope 显式报错', () => {
  assert.deepEqual(parseReviewView('').errors, []);
  assert.equal(parseReviewView('?scope=agent').view.scope, 'agent');
  const bad = parseReviewView('?scope=nope');
  assert.equal(bad.errors.length, 1);
  assert.equal(bad.errors[0].field, 'scope');
  assert.match(bad.errors[0].message, /user/);
  assert.deepEqual([...SCOPES], ['user', 'agent', 'project', 'global']);
});

test('toReviewQuery / toReviewURL 往返一致', () => {
  assert.equal(toReviewQuery({ scope: '' }), '');
  assert.equal(toReviewQuery({ scope: 'agent' }), '?scope=agent');
  assert.equal(toReviewURL({ scope: '' }), '/review');
  assert.equal(toReviewURL({ scope: 'agent' }), '/review?scope=agent');
  assert.equal(parseReviewView(toReviewQuery({ scope: 'global' })).view.scope, 'global');
});

test('isReviewPath 容忍末尾斜杠', () => {
  assert.equal(isReviewPath('/review'), true);
  assert.equal(isReviewPath('/review/'), true);
  assert.equal(isReviewPath('/reviewx'), false);
  assert.equal(isReviewPath('/'), false);
});

test('/review 已登记为已实现页面（不再是"未实现"视图）', () => {
  assert.equal(matchRoute('/review').page, 'review');
  assert.equal(isImplemented('review'), true);
  assert.equal(isImplemented('metrics'), false);
});

// ---------------------------------------------------------------------------
// 9. fetchReviewPending 走 GET（只读端点）
// ---------------------------------------------------------------------------

test('fetchReviewPending 发出 GET 到 /api/review/pending', async () => {
  const seen = [];
  const fetchImpl = async (url, opts) => {
    seen.push({ url, opts });
    return { ok: true, status: 200, text: async () => JSON.stringify(makePending()) };
  };
  const resp = await fetchReviewPending(fetchImpl, '/api/review/pending?scope=agent');
  assert.equal(seen[0].url, '/api/review/pending?scope=agent');
  assert.equal(seen[0].opts.method, undefined, '只读端点不得带 method（= GET）');
  assert.equal(resp.pending.length, 3);
});

test('读取失败变成可读 ApiError（不静默）', async () => {
  const fetchImpl = async () => ({
    ok: false, status: 500,
    text: async () => JSON.stringify({ error: 'internal', message: '后端未提供边表端口' }),
  });
  await assert.rejects(
    () => fetchReviewPending(fetchImpl, '/api/review/pending'),
    (err) => err instanceof ApiError && /后端未提供边表端口/.test(err.message) && err.status === 500,
  );
});
