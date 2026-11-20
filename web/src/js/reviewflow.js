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

// MAX_BATCH 是批量裁决上限，必须与服务端 server.MaxReviewBatch 相等
// （server/review_pending_test.go 断言 meta.max_batch，本文件由 review.test.mjs 断言）。
export const MAX_BATCH = 50;

// batchPrompt 生成**批量**二次确认的文案。
//
// 与单条 reviewPrompt 的两点差别（都来自验收要求）：
//   1. 必须**列出将影响的全部边数**（"N 条"），而不是一个模糊的"这些边"；
//   2. 必须给出**不可逆提示**：Merge 会写可回溯重定向但代价极高，
//      / 判异会写出一条此后不再进灰区的 negative_SameAs。
export function batchPrompt(count, verdict) {
  const n = Number(count) || 0;
  const irreversible =
    '批量裁决是**不可逆**的记忆写操作：每一条都会立即落到图谱上，' +
    '没有"撤销"按钮，只能靠后续人工修正。';
  if (verdict === 'same') {
    return {
      title: `确认批量合并 ${n} 条（同一实体）`,
      text: `本次将对 ${n} 条待审边执行 Merge。${irreversible}`,
    };
  }
  return {
    title: `确认批量判为不同 ${n} 条`,
    text: `本次将对 ${n} 条待审边写 negative_SameAs（此后不再进入待审灰区）。${irreversible}`,
  };
}

// requestBatchReview 是批量裁决的**唯一**入口（纯逻辑，无 DOM）。
//
// 与 requestReview 的区别：这里**只弹一次**确认（在循环里逐条弹确认框既磨人
// 又容易误操作，验收明确禁止），确认之后才逐条 send。
//
// 关键约束：**部分失败必须逐条报**。因此返回
//   { sent, reason, results: [{edgeId, ok, result|message}] }
// 而不是 { sent, okCount }——后者会把失败吞掉，用户会以为整批都成了。
export async function requestBatchReview({ edgeIds, verdict, confirm, send }) {
  const ids = Array.isArray(edgeIds) ? edgeIds.filter(Boolean) : [];
  if (!VERDICTS.includes(verdict)) {
    return { sent: false, reason: 'cancelled', results: [] };
  }
  if (ids.length === 0) {
    return { sent: false, reason: 'empty', results: [] };
  }
  if (ids.length > MAX_BATCH) {
    // 超限**直接拒绝**（不静默截断：截断会让人以为"N 条都处理了"）。
    return { sent: false, reason: 'over_limit', results: [] };
  }
  if (typeof confirm !== 'function' || typeof send !== 'function') {
    throw new TypeError('requestBatchReview 需要 confirm 与 send 两个回调');
  }
  let ok = false;
  try {
    ok = await confirm({ edgeIds: ids, verdict, count: ids.length });
  } catch {
    ok = false;
  }
  if (ok !== true) {
    return { sent: false, reason: 'not_confirmed', results: [] };
  }

  const results = [];
  for (const edgeId of ids) {
    try {
      const result = await send({ edgeId, verdict, confirmed: true });
      results.push({ edgeId, ok: true, result });
    } catch (err) {
      // 单条失败**不中断**整批：其余边仍然值得裁决，失败原因单独回报。
      results.push({
        edgeId,
        ok: false,
        message: err && err.message ? err.message : String(err),
      });
    }
  }
  return { sent: true, reason: 'sent', results };
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
