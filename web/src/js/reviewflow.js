// 待审裁决的**前置闸门**（纯逻辑，无 DOM、无网络）。
//
// 抽出来的唯一理由：验收要求"取消裁决不发请求"必须能被自动检查。
// 把它写进 DOM 事件处理里就只能靠人肉点，那等于没有覆盖。
//
// 三段闸门（任一关取消，都不发请求）：
//   1. 选「同一 / 不同」时取消        → verdict 非 same|diff
//   2. 二次确认弹窗里取消            → confirm() 返回 false
//   3. （api.postReview 再兜一道）    → confirmed !== true 直接 reject
// 第 3 道是防"上层写错"的最后一道墙，不是第 2 道的替代。

export const VERDICTS = Object.freeze(['same', 'diff']);

// requestReview 依次走三道闸门，返回结构化结果（不抛异常，便于 UI 汇报）。
//
// send 只在两道闸门都通过后被调用一次，且 confirmed 恒为 true。
export async function requestReview({ edgeId, verdict, confirm, send }) {
  if (!VERDICTS.includes(verdict)) {
    return { sent: false, reason: 'cancelled' };
  }
  if (typeof confirm !== 'function' || typeof send !== 'function') {
    throw new TypeError('requestReview 需要 confirm 与 send 两个回调');
  }
  let ok = false;
  try {
    ok = await confirm({ edgeId, verdict });
  } catch {
    ok = false; // 弹窗本身出错也不得变成"误确认"
  }
  if (ok !== true) {
    return { sent: false, reason: 'not_confirmed' };
  }
  const result = await send({ edgeId, verdict, confirmed: true });
  return { sent: true, reason: 'sent', result };
}

// reviewPrompt 生成二次确认的文案（两种裁决说清后果，避免"手滑合并"）。
export function reviewPrompt(label, verdict) {
  if (verdict === 'same') {
    return {
      title: '确认合并（同一实体）',
      text: `将把「${label}」合并为一个实体（Merge，可回溯重定向）。误合并是记忆系统最难恢复的错误，请确认。`,
    };
  }
  return {
    title: '确认判为不同实体',
    text: `将把「${label}」记为 different（写 negative_SameAs），此后不再进入待审灰区。请确认。`,
  };
}
