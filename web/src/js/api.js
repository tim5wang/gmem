// API 客户端：薄封装 fetch，**所有失败都变成可读错误**（不许静默 console.log）。
//
// 降级可见是硬要求（TD §9 / 验收「降级可见」）：服务端把 meta.degraded 放在
// 响应体里，HTTP 层也可能有非 200 —— 两者都必须能被 UI 拿到并展示。

// ApiError 携带 HTTP 状态与出错的参数名（服务端 ErrorResponse 的 field）。
export class ApiError extends Error {
  constructor(message, { status = 0, code = '', field = '' } = {}) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
    this.field = field;
  }
}

// request 是统一入口：JSON 请求 + 统一错误映射。
export async function request(fetchImpl, url, options = {}) {
  let resp;
  try {
    resp = await fetchImpl(url, options);
  } catch (err) {
    // 网络层失败（服务没起、被防火墙拦）与 HTTP 错误必须区分开：
    // 前者提示"服务不可达"，后者要显示服务端给的人话。
    throw new ApiError(`请求失败：${err && err.message ? err.message : err}`, { code: 'network' });
  }
  const text = await resp.text();
  let body = null;
  if (text) {
    try {
      body = JSON.parse(text);
    } catch {
      body = null;
    }
  }
  if (!resp.ok) {
    const message = body?.message || body?.error || text || `HTTP ${resp.status}`;
    throw new ApiError(message, {
      status: resp.status,
      code: body?.error || '',
      field: body?.field || '',
    });
  }
  if (body === null) {
    throw new ApiError('响应不是合法 JSON', { status: resp.status, code: 'bad_json' });
  }
  return body;
}

export function fetchGraph(fetchImpl, url) {
  return request(fetchImpl, url, { headers: { Accept: 'application/json' } });
}

export function fetchEntity(fetchImpl, url) {
  return request(fetchImpl, url, { headers: { Accept: 'application/json' } });
}

// postReview 是**唯一**的写请求（POST /api/review）。
//
// confirmed 由调用方显式传 true —— UI 侧必须已经过二次确认弹窗。
// 这里不做"默认 true"的兜底：那等于把二次确认做成装饰。
export function postReview(fetchImpl, { edgeId, verdict, confirmed }) {
  if (!confirmed) {
    // 前端也拦一道：即使上层逻辑写错，也不会发出未确认的写请求。
    return Promise.reject(new ApiError('未确认的裁决不会发出请求', { code: 'not_confirmed' }));
  }
  return request(fetchImpl, '/api/review', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
    body: JSON.stringify({ edge_id: edgeId, verdict, confirmed: true }),
  });
}
