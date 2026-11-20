// 客户端路由（六页共用同一份 index.html，页面选择在这里发生）。
//
// 服务端只保证"任何非 /api/ 非 /assets/ 的 GET 都回 index.html"（前置 1），
// 因此**路径 → 页面**的映射必须在前端：
//
//   /            → graph      （Graph View，P4 已完成）
//   /recall      → recall     （Recall Inspector，P6-1）
//   /review      → review     （P6-2，尚未实现 → 显示"未实现"视图）
//   /stream      → stream     （P6-3）
//   /ontology    → ontology   （P6-5）
//   /metrics     → metrics    （P6-4）
//   其它          → missing    （由前端显示 404 视图，而不是白屏）
//
// 为什么未实现的页也要登记：它们各自有真实 URL，直接给 404 会让人以为
// "路径写错了"，而这些页是**已规划但还没做**。两者的下一步完全不同。
//
// 本模块是纯函数（不碰 DOM、不发请求），因此路由判定能被单测直接固定。
export const ROUTES = Object.freeze({
  '/': 'graph',
  '/recall': 'recall',
  '/review': 'review',
  '/stream': 'stream',
  '/ontology': 'ontology',
  '/metrics': 'metrics',
});

// PAGE_TITLES 是各页的 <title> 与导航文案（缺省用路径本身）。
export const PAGE_TITLES = Object.freeze({
  graph: 'Graph View',
  recall: 'Recall Inspector',
  review: 'Review Queue',
  stream: 'Memory Stream',
  ontology: 'Ontology Drift',
  metrics: 'Metrics',
  missing: '页面不存在',
});

// IMPLEMENTED 是**当前已实现**的页面集合。
//
// 与 ROUTES 分开：ROUTES 表达"这个 URL 属于哪一页"（长期稳定），
// IMPLEMENTED 表达"这一页做完了没有"（随卡推进）。混成一个表会让
// "新增一页"和"做完一页"两件事互相牵连。
export const IMPLEMENTED = Object.freeze(['graph', 'recall']);

// normalizePath 去掉末尾斜杠（/recall/ 与 /recall 是同一页）。
export function normalizePath(pathname) {
  const p = String(pathname || '/').trim() || '/';
  if (p === '/') return '/';
  return p.replace(/\/+$/, '') || '/';
}

// matchRoute 返回 {page, path}。
//
// page ∈ ROUTES 的值，或 'missing'（未知路径）。**永不抛异常**：
// 路由层崩溃会让整页白屏，而这是验收明确禁止的。
export function matchRoute(pathname) {
  const path = normalizePath(pathname);
  const page = ROUTES[path] || 'missing';
  return { page, path };
}

// isImplemented 判断该页是否已实现（否则显示"未实现"视图）。
export function isImplemented(page) {
  return IMPLEMENTED.includes(page);
}

// notImplementedMessage 生成"未实现"页的文案（说明它是规划中的页，不是笔误）。
export function notImplementedMessage(page) {
  const title = PAGE_TITLES[page] || page;
  return `「${title}」这一页还没实现（详见 TD §9 六页计划）。` +
    `当前可用：Graph View 与 Recall Inspector。`;
}
