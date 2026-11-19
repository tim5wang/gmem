// URL ⇄ 视图状态（"URL 即视图"，TD §9）。
//
// 只做字符串 ⇄ 对象，不碰 DOM：非法参数必须能被单测固定住，
// 否则"背景白屏"这种问题只能靠人肉发现。

// 服务端硬上限（server/graph.go: maxBudget）。前端**不夹取**，只报错：
// 静默把 5000 改成 2000 会让使用者以为"我要 5000 个，它给了"。
export const BUDGET_MAX = 2000;
export const DEFAULTS = { hops: 1, budget: 200 };

const INT_RE = /^-?\d+$/;

function parsePositiveInt(raw, field, errors) {
  if (!INT_RE.test(raw)) {
    errors.push({ field, message: `${field} 必须是整数，收到 ${JSON.stringify(raw)}` });
    return null;
  }
  const n = Number(raw);
  if (n < 1) {
    errors.push({ field, message: `${field} 必须 >= 1，收到 ${n}` });
    return null;
  }
  return n;
}

// parseView 解析 location.search。
//
// 返回 {view, errors}：errors 非空时**界面必须显式报错**，而不是发出请求后白屏。
export function parseView(search) {
  const params = new URLSearchParams(String(search || '').replace(/^\?/, ''));
  const errors = [];
  const view = {
    focus: (params.get('focus') || '').trim(),
    q: (params.get('q') || '').trim(),
    scope: (params.get('scope') || '').trim(),
    hops: DEFAULTS.hops,
    budget: DEFAULTS.budget,
    asOf: null,
  };

  if (view.focus && view.q) {
    errors.push({ field: 'focus', message: 'focus 与 q 只能二选一（概览模式请两者都不传）' });
  }
  if (view.scope && !['user', 'agent', 'project', 'global'].includes(view.scope)) {
    errors.push({ field: 'scope', message: `scope 必须是 user / agent / project / global，收到 ${JSON.stringify(view.scope)}` });
  }

  const hopsRaw = params.get('hops');
  if (hopsRaw !== null && hopsRaw !== '') {
    const n = parsePositiveInt(hopsRaw, 'hops', errors);
    if (n !== null) view.hops = n;
  }

  const budgetRaw = params.get('budget');
  if (budgetRaw !== null && budgetRaw !== '') {
    const n = parsePositiveInt(budgetRaw, 'budget', errors);
    if (n !== null) {
      if (n > BUDGET_MAX) {
        errors.push({ field: 'budget', message: `budget 不能超过硬上限 ${BUDGET_MAX}，收到 ${n}（不会静默夹取）` });
      } else {
        view.budget = n;
      }
    }
  }

  const asOfRaw = params.get('as_of');
  if (asOfRaw !== null && asOfRaw !== '') {
    if (!INT_RE.test(asOfRaw)) {
      errors.push({ field: 'as_of', message: `as_of 必须是 unix 秒（整数），收到 ${JSON.stringify(asOfRaw)}` });
    } else {
      view.asOf = Number(asOfRaw);
    }
  }

  return { view, errors };
}

// toQuery 把视图编码成 query string（含前导 ?）。
//
// hops / budget 总是写进 URL：视图必须可复现，少一个参数就少一分确定性。
export function toQuery(view) {
  const params = new URLSearchParams();
  if (view.focus) params.set('focus', view.focus);
  if (view.q) params.set('q', view.q);
  if (view.scope) params.set('scope', view.scope);
  params.set('hops', String(view.hops ?? DEFAULTS.hops));
  params.set('budget', String(view.budget ?? DEFAULTS.budget));
  if (view.asOf !== null && view.asOf !== undefined) params.set('as_of', String(view.asOf));
  return `?${params.toString()}`;
}

// toGraphURL 是 GET /api/graph 的请求路径（服务端多认一个 k，前端不用）。
export function toGraphURL(view) {
  return `/api/graph${toQuery(view)}`;
}

// toEntityURL 是实体卡请求路径：as_of 必须透传，否则侧栏结论会与图不一致。
export function toEntityURL(id, view) {
  const params = new URLSearchParams();
  if (view?.asOf !== null && view?.asOf !== undefined) params.set('as_of', String(view.asOf));
  const qs = params.toString();
  return `/api/entity/${encodeURIComponent(id)}${qs ? `?${qs}` : ''}`;
}

// overviewView 回到概览模式（清空 focus / q），保留 hops / budget 习惯。
export function overviewView(view) {
  return { ...view, focus: '', q: '' };
}

// focusView 是「双击节点 → 以它为焦点展开一跳」的视图变换。
//
// focus 与 q 只能二选一（服务端就是 400），因此这里同时清掉 q。
// 抽取成纯函数的目的：交互 2 的效果可以被单测固定住（断言 URL），
// 而不是只能靠手动双击验证。
export function focusView(view, id) {
  return { ...view, focus: String(id || ''), q: '' };
}

// searchView 是「搜索框 → 写 ?q=」的视图变换。
// 空查询等于回到概览（避免发一个 q= 后服务端回 400）。
export function searchView(view, rawQuery) {
  const q = String(rawQuery || '').trim();
  if (!q) return overviewView(view);
  return { ...view, q, focus: '' };
}
