// 错误态：三类，且**都能重试**。
//
// 调研结论（§10.6）把错误分成三类，因为它们该给的下一步不同：
//   network  服务不可达（没起、被防火墙拦）→ 重试 + 提示检查服务
//   request  服务端返回 4xx/5xx（有可读 message）→ 重试 + 显示服务端原话
//   param    本地参数非法（如 k 超限）→ **不给重试**（重试还是同一个错），
//            给"改参数"的入口
//
// 关键约束：参数错误不能提供"重试"按钮。给一个点了必然再失败的按钮，
// 比不给按钮更糟——它会让人以为"再试一次就好了"。
import { h } from './dom.js';

// ERROR_KINDS 是错误的三类来源。
export const ERROR_KINDS = Object.freeze(['network', 'request', 'param']);

// classifyError 把 ApiError / Error 归类（纯函数，便于单测）。
//
// 判据只用 error 自身的字段，不看 HTTP 细节：api.js 已经把状态码与
// 出错字段塞进 ApiError，这里只做归类决策。
export function classifyError(err) {
  const status = Number(err && err.status ? err.status : 0);
  const code = String((err && err.code) || '');
  const field = String((err && err.field) || '');
  if (field || code === 'bad_request' || status === 400) return 'param';
  if (status >= 500 || status === 0) return 'network';
  if (code === 'network') return 'network';
  return 'request';
}

// errorState 构造错误态块。
//
//   kind       'network' | 'request' | 'param'（缺省由 error 归类）
//   message    人可读的一句话（服务端原话优先，绝不吞掉）
//   detail     补充信息（如出错字段名）
//   onRetry    重试回调；param 类**忽略它**（不给必然失败的按钮）
//   onFix      改参数回调（param 类的正确下一步）
export function errorState({ kind, message, detail = '', onRetry, onFix } = {}) {
  const k = ERROR_KINDS.includes(kind) ? kind : 'request';
  const wrap = h('div', {
    className: `error-state error-${k}`,
    'data-error-kind': k,
    role: 'alert',
  });
  wrap.appendChild(h('p', { className: 'error-title', textContent: titleFor(k) }));
  wrap.appendChild(h('p', { className: 'error-message', textContent: String(message || '未知错误') }));
  if (detail) {
    wrap.appendChild(h('p', { className: 'error-detail', textContent: String(detail) }));
  }
  const actions = h('div', { className: 'error-actions' });
  // 参数错误只给"改参数"，不给"重试"（重试必然同样失败）。
  if (k !== 'param' && typeof onRetry === 'function') {
    actions.appendChild(h('button', {
      type: 'button', className: 'error-action', onclick: onRetry, textContent: '重试',
    }));
  }
  if (typeof onFix === 'function') {
    actions.appendChild(h('button', {
      type: 'button', className: 'error-action', onclick: onFix, textContent: '修改参数',
    }));
  }
  if (actions.children && actions.children.length) wrap.appendChild(actions);
  return wrap;
}

// titleFor 是三类错误的标题（固定话术，避免每页各写一句）。
function titleFor(kind) {
  switch (kind) {
    case 'network':
      return '连不上服务';
    case 'param':
      return '参数有误';
    default:
      return '请求失败';
  }
}

// errorStateInto 用错误态占满容器。
export function errorStateInto(container, opts) {
  if (!container) return null;
  container.textContent = '';
  const el = errorState(opts);
  container.appendChild(el);
  return el;
}
