// Recall Inspector 的 URL ⇄ 视图状态（"URL 即视图"，TD §9）。
//
// 与 urlstate.js 同一手法：只做字符串 ⇄ 对象，不碰 DOM。
// 理由也一样 —— 非法参数必须能被单测固定住，否则"白屏"只能靠人肉发现。
//
// 本页参数：q / scope / kinds / as_of / lanes / k
// 全部进 URL，刷新可复现（验收「URL 即视图」）。

// K_MAX 必须与服务端 server/recall.go 的 maxRecallK 一致。
// 前端**不夹取**，只报错：静默把 200 改成 50 会让使用者以为"它给了 200 条"。
export const K_MAX = 50;
export const K_DEFAULT = 10;

// SCOPES / KINDS / LANES 是服务端接受的取值（与 store / recall 的枚举一致）。
export const SCOPES = Object.freeze(['user', 'agent', 'project', 'global']);
export const KINDS = Object.freeze(['semantic', 'episodic', 'procedural', 'decision']);
export const LANES = Object.freeze(['lexical', 'vector', 'graph']);

const INT_RE = /^-?\d+$/;

// splitList 解析逗号分隔的多选参数（空串 → 空数组）。
export function splitList(raw) {
  return String(raw || '')
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean);
}

// parseRecallView 解析 location.search 与 location.pathname。
//
// 返回 {view, errors}：errors 非空时**界面必须显式报错**，而不是白屏，
// 也不是"带着错参数发一个请求然后显示后端 400"。
export function parseRecallView(search) {
  const params = new URLSearchParams(String(search || '').replace(/^\?/, ''));
  const errors = [];
  const view = {
    q: (params.get('q') || '').trim(),
    scope: (params.get('scope') || '').trim(),
    kinds: [],
    lanes: [],
    asOf: null,
    k: K_DEFAULT,
    item: (params.get('item') || '').trim(),
  };

  if (view.scope && !SCOPES.includes(view.scope)) {
    errors.push({
      field: 'scope',
      message: `scope 必须是 ${SCOPES.join(' / ')} 之一，收到 ${JSON.stringify(view.scope)}`,
    });
  }

  for (const k of splitList(params.get('kinds'))) {
    if (!KINDS.includes(k)) {
      errors.push({
        field: 'kinds',
        message: `kinds 只能是 ${KINDS.join(' / ')}，收到 ${JSON.stringify(k)}`,
      });
      continue;
    }
    view.kinds.push(k);
  }

  for (const l of splitList(params.get('lanes'))) {
    if (!LANES.includes(l)) {
      errors.push({
        field: 'lanes',
        message: `lanes 只能是 ${LANES.join(' / ')}，收到 ${JSON.stringify(l)}`,
      });
      continue;
    }
    view.lanes.push(l);
  }

  const kRaw = params.get('k');
  if (kRaw !== null && kRaw !== '') {
    if (!INT_RE.test(kRaw)) {
      errors.push({ field: 'k', message: `k 必须是整数，收到 ${JSON.stringify(kRaw)}` });
    } else {
      const n = Number(kRaw);
      if (n < 1) {
        errors.push({ field: 'k', message: `k 必须 >= 1，收到 ${n}` });
      } else if (n > K_MAX) {
        errors.push({
          field: 'k',
          message: `k 不能超过硬上限 ${K_MAX}，收到 ${n}（不会静默夹取）`,
        });
      } else {
        view.k = n;
      }
    }
  }

  const asOfRaw = params.get('as_of');
  if (asOfRaw !== null && asOfRaw !== '') {
    if (!INT_RE.test(asOfRaw)) {
      errors.push({
        field: 'as_of',
        message: `as_of 必须是 unix 秒（整数），收到 ${JSON.stringify(asOfRaw)}`,
      });
    } else {
      view.asOf = Number(asOfRaw);
    }
  }

  return { view, errors };
}

// toRecallQuery 把视图序列化成 query string（空值不写进 URL）。
//
// 顺序固定，便于"复制链接"与断言；数组用逗号分隔（服务端就是按逗号切）。
export function toRecallQuery(view) {
  const params = new URLSearchParams();
  if (view?.q) params.set('q', view.q);
  if (view?.scope) params.set('scope', view.scope);
  if (view?.kinds?.length) params.set('kinds', view.kinds.join(','));
  if (view?.lanes?.length) params.set('lanes', view.lanes.join(','));
  if (view?.asOf !== null && view?.asOf !== undefined) params.set('as_of', String(view.asOf));
  if (view?.k !== null && view?.k !== undefined && view.k !== K_DEFAULT) {
    params.set('k', String(view.k));
  }
  if (view?.item) params.set('item', view.item);
  const qs = params.toString();
  return qs ? `?${qs}` : '';
}

// toRecallURL 是 /api/recall 的请求路径。
//
// explain=1：本页的用途就是解释召回，因此恒定请求解释。
// item 是**页面选中项**，不属于 API 契约，因此不带给服务端。
export function toRecallURL(view) {
  const apiView = { ...view, item: '' };
  const qs = toRecallQuery(apiView);
  return `/api/recall${qs}${qs ? '&' : '?'}explain=1`;
}

// setAsOf 返回带上 as_of 的新视图（不动其它参数）。
export function setAsOf(view, sec) {
  return { ...view, asOf: Number(sec) };
}

// clearAsOf 返回**去掉 as_of** 的新视图（"回到现在"唯一的语义）。
//
// 单独一个函数而不是 `setAsOf(view, null)`：验收要求"点回到现在后 URL 中
// as_of 消失"，把这件事写成一个有名字的纯函数，就能被单测钉死。
export function clearAsOf(view) {
  return { ...view, asOf: null };
}

// withQuery 返回换了查询串的新视图（同时清掉选中项，因为结果集要重算）。
export function withQuery(view, rawQuery) {
  return { ...view, q: String(rawQuery || '').trim(), item: '' };
}

// selectItem 返回选中某条结果的新视图（选中项也进 URL：刷新后可复现）。
export function selectItem(view, id) {
  return { ...view, item: String(id || '') };
}

// toggleList 在数组参数里增删一项（kinds / lanes 的多选用）。
export function toggleList(view, field, value) {
  const cur = Array.isArray(view?.[field]) ? view[field] : [];
  const next = cur.includes(value) ? cur.filter((v) => v !== value) : [...cur, value];
  // 保持枚举顺序，便于 URL 稳定（"URL 即视图"要求同一视图同一串）。
  const order = field === 'kinds' ? KINDS : LANES;
  return { ...view, [field]: order.filter((v) => next.includes(v)), item: '' };
}

// isRecallPath 判断当前路径是否属于 Recall Inspector 页。
//
// 末尾斜杠归一：/recall/ 与 /recall 是同一页（刷新时浏览器可能带斜杠）。
export function isRecallPath(pathname) {
  const p = String(pathname || '/').replace(/\/+$/, '');
  return p === '/recall';
}
