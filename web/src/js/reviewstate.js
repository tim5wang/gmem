// Review Queue 的 URL ⇄ 视图状态 + **确定性排序**（纯函数，不碰 DOM、不发请求）。
//
// 与 recallstate.js 同一手法，理由也一样：
//   - 非法 scope 必须能被单测固定住（否则页面白屏，只能靠人肉发现）；
//   - "按 t_valid 降序、同值按 edge_id 升序"是本页的验收点，它必须是**纯函数**，
//     否则只能靠肉眼比对表格行序。
//
// 本页参数：scope（留空 = 所有 scope 的并集）

// SCOPES 与服务端 store.Scope 的枚举一致。
export const SCOPES = Object.freeze(['user', 'agent', 'project', 'global']);

// MAX_BATCH 是批量裁决上限，必须与服务端 server.MaxReviewBatch 相等。
//
// 两边各有一份断言（web/src/test/review.test.mjs 与 server/review_pending_test.go
// 的 meta.max_batch）。UI 侧拦一道是为了在用户勾选时就说清楚，而不是等到点了
// 确认才被服务端拒绝。
export const MAX_BATCH = 50;

// REVIEW_PATH 是本页的路径（站内链接与路由判定共用一处）。
export const REVIEW_PATH = '/review';

// parseReviewView 解析 location.search。
//
// 返回 {view, errors}：errors 非空时**界面必须显式报错**（不发请求、不白屏），
// 与 Recall 页同一策略。
export function parseReviewView(search) {
  const params = new URLSearchParams(String(search || '').replace(/^\?/, ''));
  const errors = [];
  const view = { scope: (params.get('scope') || '').trim() };
  if (view.scope && !SCOPES.includes(view.scope)) {
    errors.push({
      field: 'scope',
      message: `scope 必须是 ${SCOPES.join(' / ')} 之一（留空 = 全部），收到 ${JSON.stringify(view.scope)}`,
    });
  }
  return { view, errors };
}

// toReviewQuery 把视图序列化成查询串（空视图 → ''）。
export function toReviewQuery(view) {
  const scope = String(view?.scope || '').trim();
  return scope ? `?scope=${encodeURIComponent(scope)}` : '';
}

// toReviewURL 是页面地址（"视图即 URL"，刷新可复现）。
export function toReviewURL(view) {
  return `${REVIEW_PATH}${toReviewQuery(view)}`;
}

// isReviewPath 判断当前路径是否属于本页（末尾斜杠等价）。
export function isReviewPath(pathname) {
  const p = String(pathname || '').replace(/\/+$/, '');
  return p === REVIEW_PATH;
}

// graphFocusHref 生成"去图上看这个节点"的链接（TD §9 的 Review → Graph 箭头）。
//
// 裁决之后用它做跳转：合并后旧 ID 会被重定向，但服务端 /api/graph 自己会跟到
// 新实体（focus 走 GetNode + 重定向），所以这里直接用节点 ID 是安全的。
export function graphFocusHref(nodeId) {
  const id = String(nodeId || '');
  if (!id) return '/';
  return `/?focus=${encodeURIComponent(id)}`;
}

// reviewRows 把响应里的 pending 投影成页面行（**不做任何重算**）。
//
// 每行的可裁状态来自后端的显式字段（reviewable / invalidated），
// 不从 reason 的自然语言里反推——调研 §6 ⑤(3) 的结论。
export function reviewRows(resp) {
  const pending = Array.isArray(resp?.pending) ? resp.pending : [];
  return pending.map((row) => ({
    edgeId: String(row.edge_id || ''),
    from: String(row.from || ''),
    to: String(row.to || ''),
    fromName: String(row.from_name || row.from || ''),
    toName: String(row.to_name || row.to || ''),
    weight: Number(row.weight),
    tValid: Number(row.t_valid),
    scope: String(row.scope || ''),
    reason: String(row.reason || ''),
    reviewable: row.reviewable === true,
    invalidated: row.invalidated === true,
  }));
}

// sortRows 按 **t_valid 降序、同值按 edge_id 升序** 排序（确定性）。
//
// 为什么必须是确定性的：待审池是会随时间增长的灰区，两条边 t_valid 相同很常见；
// 若顺序不稳定，同样一份数据在两次渲染里换行，用户会以为"列表内容变了"。
// 排序只发生在这里（后端透传的是 edge_id 升序），因此"谁排的序"只有一个答案。
export function sortRows(rows) {
  const list = Array.isArray(rows) ? [...rows] : [];
  list.sort((a, b) => {
    const ta = Number.isFinite(a.tValid) ? a.tValid : 0;
    const tb = Number.isFinite(b.tValid) ? b.tValid : 0;
    if (ta !== tb) return tb - ta;
    return a.edgeId < b.edgeId ? -1 : a.edgeId > b.edgeId ? 1 : 0;
  });
  return list;
}

// rowLabel 是给人看的边标签（两端名称）。
export function rowLabel(row) {
  return `${row.fromName} ⇄ ${row.toName}`;
}

// batchSelectable 判断该行能否被勾选（不可裁的行不给勾选框）。
export function batchSelectable(row) {
  return row.reviewable === true && row.invalidated !== true;
}

// selectedCount 统计当前选中条数。
export function selectedCount(selected) {
  return selected instanceof Set ? selected.size : 0;
}

// overBatchLimit 报告是否超出上限（超了就**拒绝**，不静默截断）。
export function overBatchLimit(count) {
  return Number(count) > MAX_BATCH;
}
