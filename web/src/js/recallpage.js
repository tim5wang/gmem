// Recall Inspector 页面（TD §9 六页 ②）。
//
// # 分两层，理由是"可解释"必须能被自动检查
//
//   纯函数层（本文件上半部）：把 /api/recall 的响应**只读**投影成页面要显示的行。
//     没有任何 `score = fusion * boost * recency` 这样的重算 —— 一旦前端自己算分，
//     页面上的"可解释"就是伪造的证据。这里只做格式化与分组。
//   DOM 层（下半部）：把行落成元素。全部经 textContent / setAttribute 写入。
//
// # 三因子一致性怎么被证明
//
// scoreBreakdown(item) 返回的 factors 是**后端字段的字面值**，product 只是把这三个
// 后端值相乘用于对照（不是用分数去反推因子）。页面同时显示 score（后端值）与
// product（三因子相乘），并给出两者之差 —— 读者可以自己按计算器验算。
// 差值 > 0.001 时显式标红报警，而不是悄悄取整掩盖。
import { h, fmt, fmtPct, fmtUnix } from './ui/dom.js';
import { emptyState, exampleSuggestions } from './ui/empty.js';
import { skeleton } from './ui/skeleton.js';
import { errorState } from './ui/errorState.js';
import { banner } from './ui/banner.js';
import { sparkline } from './ui/chart.js';
import {
  K_MAX, SCOPES, KINDS, LANES, clearAsOf, selectItem, toggleList, withQuery,
} from './recallstate.js';

// SCORE_EPS 是三因子复算的容差（验收要求 ≤ 0.001）。
export const SCORE_EPS = 0.001;

// ---------------------------------------------------------------------------
// 纯函数层
// ---------------------------------------------------------------------------

// scoreBreakdown 把一条结果的分数分解成"可展示 + 可复算"的结构。
//
// 返回：
//   fusion / boost / recency  —— 后端透传的三个因子（**不再加工**）
//   score                     —— 后端给的最终分
//   product                   —— 三因子相乘（仅用于对照）
//   delta                     —— |product - score|
//   consistent                —— delta <= SCORE_EPS
export function scoreBreakdown(item) {
  const fusion = Number(item?.fusion_score);
  const boost = Number(item?.concept_boost);
  const recency = Number(item?.recency_factor);
  const score = Number(item?.score);
  const product = fusion * boost * recency;
  const delta = Math.abs(product - score);
  return {
    fusion,
    boost,
    recency,
    score,
    product,
    delta,
    consistent: Number.isFinite(delta) && delta <= SCORE_EPS,
  };
}

// planSummary 从响应里抽出 plan 区块要显示的四项具体值。
//
// coverage / theta_coverage 优先取 plan 内的值（那是 recall 的原始结论）；
// plan 缺失时退回顶层的同名字段，并显式标记来源，便于排查"到底读到了哪一份"。
export function planSummary(resp) {
  const plan = resp?.plan || {};
  const hasPlan = resp?.plan !== null && resp?.plan !== undefined;
  return {
    query: plan.query ?? resp?.query ?? '',
    shape: plan.shape ?? '',
    shapeReason: plan.shape_reason ?? '',
    coverage: Number(plan.coverage ?? resp?.coverage ?? NaN),
    thetaCoverage: Number(plan.theta_coverage ?? resp?.theta_coverage ?? NaN),
    hops: Number(plan.hops ?? 0),
    anchors: Array.isArray(plan.anchors) ? plan.anchors : [],
    matched: Array.isArray(plan.matched) ? plan.matched : [],
    unmatched: Array.isArray(plan.unmatched) ? plan.unmatched : [],
    relations: Array.isArray(plan.relations) ? plan.relations : [],
    steps: Array.isArray(plan.steps) ? plan.steps : [],
    fromPlan: hasPlan,
  };
}

// laneRows 把 plan.lanes 变成逐行可渲染的数据，并把每条 lane 的 LaneHit 归拢。
//
// hits 的来源是**每条结果的 contributions**（recall 的 LaneHit 挂在条目上，
// plan 层只有 lane 决策）：因此这里按 lane 聚合，行内带 memory_id 以说明
// "这条贡献属于哪条记忆"。这是唯一不重算的映射方式。
export function laneRows(resp) {
  const lanes = Array.isArray(resp?.plan?.lanes) ? resp.plan.lanes : [];
  const items = Array.isArray(resp?.items) ? resp.items : [];
  return lanes.map((lp) => {
    const hits = [];
    for (const it of items) {
      for (const c of it.contributions || []) {
        if (c.lane !== lp.lane) continue;
        hits.push({
          memoryId: it.id,
          rank: Number(c.rank),
          weight: Number(c.weight),
          value: Number(c.value),
        });
      }
    }
    hits.sort((a, b) => a.rank - b.rank || String(a.memoryId).localeCompare(String(b.memoryId)));
    return {
      lane: lp.lane,
      weight: Number(lp.weight),
      budget: Number(lp.budget),
      selected: Boolean(lp.selected),
      executed: Boolean(lp.executed),
      // reason 与 skip_reason 至少有一个要能显示（否则读者不知道它为何执行/被跳过）。
      reason: lp.reason || '',
      skipReason: lp.skip_reason || '',
      hits,
    };
  });
}

// droppedRows 归一化被拦候选（页面按条数与 plan.dropped 对齐）。
export function droppedRows(resp) {
  const list = Array.isArray(resp?.plan?.dropped)
    ? resp.plan.dropped
    : (Array.isArray(resp?.dropped) ? resp.dropped : []);
  return list.map((d) => ({
    id: d.id || '',
    reason: d.reason || '',
    score: Number(d.score),
  }));
}

// degradedLines 返回需要显示在 warn 横幅里的**全部**降级条目。
//
// 两类来源都要：响应顶层 recall.degraded，以及 meta.degraded（前端若复用
// 同一份渲染，就不该有"某一类只进 console"的漏口）。
export function degradedLines(resp) {
  const out = [];
  const push = (v) => {
    const s = String(v ?? '').trim();
    if (s) out.push(s);
  };
  for (const d of resp?.degraded || []) push(d);
  for (const d of resp?.meta?.degraded || []) push(d);
  return out;
}

// asOfBanner 返回 as_of 横幅的文案（null 表示不显示）。
//
// 文案必须包含**具体时刻**（验收：不可折叠的"你正在看 <时刻>"）。
export function asOfBanner(resp, view) {
  const asOf = resp?.as_of ?? view?.asOf ?? null;
  if (asOf === null || asOf === undefined) return null;
  return `你正在看 ${fmtUnix(asOf)}（as_of=${asOf}）的历史视角，不是当前状态`;
}

// isEmptyQuery 判断是否处于"还没查询"的状态。
export function isEmptyQuery(view) {
  return !String(view?.q || '').trim();
}

// noCapabilityReason 从 capabilities 里取出"为什么这页不能用"。
//
// 返回 null 表示能力可用。这条分支的存在就是为了让"没数据"与"没能力"
// 在页面上是两句不同的话（验收「空态与双向连线」的反面约束）。
export function noCapabilityReason(resp) {
  const caps = resp?.meta?.capabilities;
  if (!caps) return null;
  if (caps.recall_plan) return null;
  const reasons = Array.isArray(caps.reasons) ? caps.reasons : [];
  const hit = reasons.find((r) => String(r).startsWith('recall_plan'));
  return hit || 'recall_plan 能力不可用';
}

// coverageSeries 给出 plan 区块旁边的迷你图数据（coverage 与 θ 的对照）。
//
// 用 min/max 固定成 [0,1]：纵轴每次刷新都变会让"看起来变好/变差"成为假象。
export const COVERAGE_RANGE = Object.freeze({ min: 0, max: 1 });

// ---------------------------------------------------------------------------
// DOM 层
// ---------------------------------------------------------------------------

// kv 构造一个键值块。
export function kv(pairs) {
  const dl = h('dl', { className: 'kv' });
  for (const [k, v, num] of pairs) {
    dl.appendChild(h('dt', { textContent: k }));
    dl.appendChild(h('dd', { className: num ? 'num' : '', textContent: String(v ?? '—') }));
  }
  return dl;
}

// renderPlanSection 构造 plan 区块（shape / shape_reason / coverage / theta_coverage
// + unmatched 槽位列表）。
export function renderPlanSection(resp) {
  const p = planSummary(resp);
  const section = h('section', { className: 'recall-section', 'data-section': 'plan' });
  section.appendChild(h('h2', { textContent: 'plan（召回规划）' }));

  const cov = h('div', { className: 'chips' });
  cov.appendChild(h('span', { className: 'chip', textContent: `coverage ${fmt(p.coverage)}` }));
  cov.appendChild(h('span', { className: 'chip', textContent: `θ ${fmt(p.thetaCoverage)}` }));
  // 迷你图：两个点的折线（coverage vs θ）只是"差距"的视觉提示，数值在 chips 里。
  cov.appendChild(sparkline({
    values: [p.coverage, p.thetaCoverage],
    width: 120, height: 22,
    min: COVERAGE_RANGE.min, max: COVERAGE_RANGE.max,
    label: `coverage ${fmt(p.coverage)}，门槛 ${fmt(p.thetaCoverage)}`,
  }));

  section.appendChild(kv([
    ['query', p.query || '—'],
    ['shape', p.shape || '—'],
    ['shape_reason', p.shapeReason || '—'],
    ['coverage', fmt(p.coverage), true],
    ['theta_coverage', fmt(p.thetaCoverage), true],
    ['hops', String(p.hops)],
    ['anchors', p.anchors.length ? p.anchors.join(', ') : '（无实体锚点）'],
    ['matched', String(p.matched.length), true],
    ['unmatched', String(p.unmatched.length), true],
  ]));
  if (!p.fromPlan) {
    section.appendChild(h('p', { className: 'hint', textContent: '注意：响应里没有 plan 对象，上面取自顶层字段。' }));
  }

  section.appendChild(h('h3', { textContent: `未对齐槽位（${p.unmatched.length}）` }));
  if (p.unmatched.length === 0) {
    section.appendChild(h('p', { className: 'hint', textContent: '全部槽位都已对齐。' }));
  } else {
    const table = h('table', { className: 'table', 'data-table': 'unmatched' });
    const head = h('tr', {});
    for (const [label, cls] of [['kind', ''], ['text', ''], ['required', 'num'], ['weight', 'num']]) {
      head.appendChild(h('th', { className: cls, textContent: label }));
    }
    table.appendChild(h('thead', {}, [head]));
    const body = h('tbody', {});
    for (const s of p.unmatched) {
      body.appendChild(h('tr', {}, [
        h('td', { textContent: s.kind || '' }),
        h('td', { textContent: s.text || '' }),
        h('td', { className: 'num', textContent: s.required ? '必需' : '可选' }),
        h('td', { className: 'num', textContent: fmt(s.weight) }),
      ]));
    }
    table.appendChild(body);
    section.appendChild(table);
  }

  // 关系跳数（MaxHops / Origin）：与 /api/graph 共用一张表的证据。
  if (p.relations.length) {
    section.appendChild(h('h3', { textContent: `关系跳数（${p.relations.length}）` }));
    const table = h('table', { className: 'table', 'data-table': 'relations' });
    const head = h('tr', {});
    for (const [label, cls] of [['relation', ''], ['transitive', ''], ['max_hops', 'num'], ['weight', 'num'], ['origin', '']]) {
      head.appendChild(h('th', { className: cls, textContent: label }));
    }
    table.appendChild(h('thead', {}, [head]));
    const body = h('tbody', {});
    for (const r of p.relations) {
      body.appendChild(h('tr', {}, [
        h('td', { textContent: r.relation || '' }),
        h('td', { textContent: r.transitive ? '传递' : '非传递' }),
        h('td', { className: 'num', textContent: String(r.max_hops ?? '') }),
        h('td', { className: 'num', textContent: fmt(r.weight) }),
        h('td', { textContent: r.origin || '' }),
      ]));
    }
    table.appendChild(body);
    section.appendChild(table);
  }
  return section;
}

// renderLaneSection 构造 lane 区块。
//
// 每行一条 lane，显示 lane / weight / budget / selected / executed / reason|skip_reason；
// 点行展开该 lane 的 LaneHit（rank / weight / value）。
export function renderLaneSection(resp, { onToggle } = {}) {
  const rows = laneRows(resp);
  const section = h('section', { className: 'recall-section', 'data-section': 'lanes' });
  section.appendChild(h('h2', { textContent: `lane（${rows.length} 条通道的规划与贡献）` }));
  if (rows.length === 0) {
    section.appendChild(h('p', { className: 'hint', textContent: '响应里没有 lane 规划。' }));
    return section;
  }

  const table = h('table', { className: 'table', 'data-table': 'lanes' });
  const head = h('tr', {});
  for (const [label, cls] of [
    ['lane', ''], ['weight', 'num'], ['budget', 'num'],
    ['selected', 'num'], ['executed', 'num'], ['reason / skip_reason', ''],
  ]) {
    head.appendChild(h('th', { className: cls, textContent: label }));
  }
  table.appendChild(h('thead', {}, [head]));
  const body = h('tbody', {});

  for (const r of rows) {
    const tr = h('tr', { className: 'lane-row', 'data-lane': r.lane });
    const btn = h('button', {
      type: 'button',
      className: 'row-button',
      'aria-expanded': 'false',
      textContent: r.lane,
    });
    tr.appendChild(h('td', {}, [btn]));
    tr.appendChild(h('td', { className: 'num', textContent: fmt(r.weight) }));
    tr.appendChild(h('td', { className: 'num', textContent: String(r.budget) }));
    tr.appendChild(h('td', { className: `num lane-flag ${r.selected ? 'flag-yes' : 'flag-no'}`, textContent: r.selected ? '是' : '否' }));
    tr.appendChild(h('td', { className: `num lane-flag ${r.executed ? 'flag-yes' : 'flag-no'}`, textContent: r.executed ? '是' : '否' }));
    tr.appendChild(h('td', { textContent: r.reason || (r.skipReason ? `跳过：${r.skipReason}` : '—') }));
    body.appendChild(tr);

    // 展开行：LaneHit 表（默认隐藏）。
    const detail = h('tr', { className: 'lane-detail', hidden: true, 'data-lane-hits': r.lane });
    const cell = h('td', { colspan: '6' });
    if (r.hits.length === 0) {
      cell.appendChild(h('p', {
        className: 'hint',
        textContent: r.executed
          ? '这一路被执行，但对最终结果没有贡献（没有条目在它上面得分）。'
          : '这一路没有执行，因此没有贡献。',
      }));
    } else {
      const hitTable = h('table', { className: 'table', 'data-table': 'lane-hits' });
      const hh = h('tr', {});
      for (const [label, cls] of [['memory', ''], ['rank', 'num'], ['weight', 'num'], ['value', 'num']]) {
        hh.appendChild(h('th', { className: cls, textContent: label }));
      }
      hitTable.appendChild(h('thead', {}, [hh]));
      const hb = h('tbody', {});
      for (const hit of r.hits) {
        hb.appendChild(h('tr', {}, [
          h('td', { textContent: hit.memoryId }),
          h('td', { className: 'num', textContent: String(hit.rank) }),
          h('td', { className: 'num', textContent: fmt(hit.weight) }),
          h('td', { className: 'num', textContent: fmt(hit.value) }),
        ]));
      }
      hitTable.appendChild(hb);
      cell.appendChild(hitTable);
    }
    detail.appendChild(cell);
    body.appendChild(detail);

    const toggle = () => {
      const open = detail.hidden !== false;
      detail.hidden = !open;
      tr.classList.toggle('open', open);
      btn.setAttribute('aria-expanded', open ? 'true' : 'false');
      if (typeof onToggle === 'function') onToggle(r.lane, open);
    };
    btn.onclick = toggle;
  }
  table.appendChild(body);
  section.appendChild(table);
  return section;
}

// renderDetail 构造"分数分解"详情（宽屏右侧 / 窄屏抽屉共用同一份内容）。
export function renderDetail(resp, itemId, { onClose } = {}) {
  const wrap = h('div', { className: 'recall-detail', 'data-section': 'detail' });
  const head = h('div', { className: 'drawer-head' });
  head.appendChild(h('span', { textContent: '分数分解' }));
  if (typeof onClose === 'function') {
    head.appendChild(h('button', { type: 'button', onclick: onClose, textContent: '关闭' }));
  }
  wrap.appendChild(head);

  const item = (resp?.items || []).find((it) => it.id === itemId);
  if (!item) {
    wrap.appendChild(h('p', {
      className: 'hint',
      textContent: itemId ? `结果 ${itemId} 不在本次响应里（可能已被预算切断）。` : '点左侧任意一条结果，这里显示它的分数分解。',
    }));
    return wrap;
  }

  const b = scoreBreakdown(item);
  wrap.appendChild(h('h2', { textContent: item.id }));

  const box = h('div', { className: 'score-box', 'data-section': 'score' });

  // 公式：三个因子都是后端字段，逐个显示完整精度（三位小数）便于手算。
  const formula = h('p', {
    className: 'score-formula',
    textContent: `score = fusion_score × concept_boost × recency_factor = ` +
      `${fmt(b.fusion)} × ${fmt(b.boost)} × ${fmt(b.recency)} = ${fmt(b.product)}`,
  });
  box.appendChild(formula);

  const finalRow = h('div', { className: 'score-final' });
  finalRow.appendChild(h('span', { textContent: fmt(b.score) }));
  finalRow.appendChild(h('span', { className: 'unit', textContent: 'score（后端）' }));
  finalRow.appendChild(h('span', {
    className: `score-check ${b.consistent ? 'ok' : 'bad'}`,
    'data-consistent': b.consistent ? 'true' : 'false',
    textContent: b.consistent
      ? `✓ 三因子相乘与 score 一致（差 ${fmt(b.delta)} ≤ ${SCORE_EPS}）`
      : `✗ 三因子相乘 = ${fmt(b.product)}，与 score = ${fmt(b.score)} 差 ${fmt(b.delta)}（> ${SCORE_EPS}）`,
  }));
  box.appendChild(finalRow);

  box.appendChild(kv([
    ['fusion_score', fmt(b.fusion), true],
    ['concept_boost', fmt(b.boost), true],
    ['recency_factor', fmt(b.recency), true],
    ['三因子相乘', fmt(b.product), true],
  ]));
  wrap.appendChild(box);

  // 调整明细（Kind / Factor / Reason）
  const adj = Array.isArray(item.adjustments) ? item.adjustments : [];
  wrap.appendChild(h('h3', { textContent: `调整明细（${adj.length}）` }));
  if (adj.length === 0) {
    wrap.appendChild(h('p', { className: 'hint', textContent: '没有调整记录。' }));
  } else {
    const ul = h('ul', { className: 'plain-list', 'data-list': 'adjustments' });
    for (const a of adj) {
      ul.appendChild(h('li', {
        textContent: `${a.kind} ×${fmt(a.factor)} — ${a.reason || '（无说明）'}`,
      }));
    }
    wrap.appendChild(ul);
  }

  // 分通道贡献（LaneHit）
  const contrib = Array.isArray(item.contributions) ? item.contributions : [];
  wrap.appendChild(h('h3', { textContent: `分通道贡献（${contrib.length}）` }));
  if (contrib.length === 0) {
    wrap.appendChild(h('p', { className: 'hint', textContent: '没有通道贡献记录。' }));
  } else {
    const table = h('table', { className: 'table', 'data-table': 'contributions' });
    const hh = h('tr', {});
    for (const [label, cls] of [['lane', ''], ['rank', 'num'], ['weight', 'num'], ['value', 'num']]) {
      hh.appendChild(h('th', { className: cls, textContent: label }));
    }
    table.appendChild(h('thead', {}, [hh]));
    const tb = h('tbody', {});
    for (const c of contrib) {
      tb.appendChild(h('tr', {}, [
        h('td', { textContent: c.lane || '' }),
        h('td', { className: 'num', textContent: String(c.rank ?? '') }),
        h('td', { className: 'num', textContent: fmt(c.weight) }),
        h('td', { className: 'num', textContent: fmt(c.value) }),
      ]));
    }
    table.appendChild(tb);
    wrap.appendChild(table);
  }

  // 图证据（"凭什么是它"）
  const ev = Array.isArray(item.graph_evidence) ? item.graph_evidence : [];
  wrap.appendChild(h('h3', { textContent: `图证据（${ev.length}）` }));
  if (ev.length === 0) {
    wrap.appendChild(h('p', { className: 'hint', textContent: '这条结果不是经由 graph lane 召回的，没有边证据。' }));
  } else {
    const ul = h('ul', { className: 'plain-list', 'data-list': 'graph-evidence' });
    for (const e of ev) {
      ul.appendChild(h('li', {
        textContent: `${e.from} -[${e.relation}]-> ${e.to}（anchor=${e.anchor}，depth=${e.depth}，` +
          `linked_by=${e.linked_by}，edge=${e.edge_id}）`,
      }));
    }
    wrap.appendChild(ul);
  }

  wrap.appendChild(h('h3', { textContent: '记忆正文' }));
  wrap.appendChild(h('p', { className: 'result-content', textContent: item.content || '（空）' }));
  return wrap;
}

// renderResultList 构造结果列表（点一条 → 详情）。
export function renderResultList(resp, { selectedId, onSelect } = {}) {
  const items = Array.isArray(resp?.items) ? resp.items : [];
  const section = h('section', { className: 'recall-section', 'data-section': 'items' });
  section.appendChild(h('h2', { textContent: `结果（${items.length}）` }));
  if (items.length === 0) {
    section.appendChild(h('p', {
      className: 'hint',
      textContent: '本次召回没有返回条目：见上方 plan 的 unmatched / missing 与降级横幅。',
    }));
    return section;
  }
  const table = h('table', { className: 'table', 'data-table': 'items' });
  const head = h('tr', {});
  for (const [label, cls] of [['score', 'num'], ['记忆', ''], ['kind', ''], ['概念', '']]) {
    head.appendChild(h('th', { className: cls, textContent: label }));
  }
  table.appendChild(h('thead', {}, [head]));
  const body = h('tbody', {});
  for (const it of items) {
    const tr = h('tr', {
      className: `result-row${it.id === selectedId ? ' selected' : ''}`,
      'data-item-id': it.id,
    });
    const btn = h('button', {
      type: 'button',
      className: 'row-button',
      textContent: fmt(it.score),
    });
    btn.onclick = () => {
      if (typeof onSelect === 'function') onSelect(it.id);
    };
    tr.appendChild(h('td', { className: 'num' }, [h('span', { className: 'result-score' }, [btn])]));
    tr.appendChild(h('td', {}, [
      h('div', { className: 'result-content', textContent: it.content || '（空）' }),
      h('div', {
        className: 'result-meta',
        textContent: `id=${it.id} · v${it.version} · scope=${it.scope}` +
          (it.source_episode ? ` · src=${it.source_episode}` : ''),
      }),
    ]));
    tr.appendChild(h('td', { textContent: it.kind || '' }));
    tr.appendChild(h('td', { textContent: it.concept_id || '—' }));
    body.appendChild(tr);
  }
  table.appendChild(body);
  section.appendChild(table);
  return section;
}

// renderDroppedSection 构造被拦候选表（条数与 plan.dropped 相同）。
export function renderDroppedSection(resp) {
  const rows = droppedRows(resp);
  const section = h('section', { className: 'recall-section', 'data-section': 'dropped' });
  section.appendChild(h('h2', { textContent: `被拦候选（${rows.length}）` }));
  if (rows.length === 0) {
    section.appendChild(h('p', { className: 'hint', textContent: '没有被拦截的候选。' }));
    return section;
  }
  const table = h('table', { className: 'table', 'data-table': 'dropped' });
  const head = h('tr', {});
  for (const [label, cls] of [['id', ''], ['reason', ''], ['score', 'num']]) {
    head.appendChild(h('th', { className: cls, textContent: label }));
  }
  table.appendChild(h('thead', {}, [head]));
  const body = h('tbody', {});
  for (const d of rows) {
    body.appendChild(h('tr', {}, [
      h('td', { textContent: d.id }),
      h('td', { textContent: d.reason }),
      h('td', { className: 'num', textContent: fmt(d.score) }),
    ]));
  }
  table.appendChild(body);
  section.appendChild(table);
  return section;
}

// renderMissingSection 构造 R4 的"缺什么"（含可执行建议）。
export function renderMissingSection(resp) {
  const missing = Array.isArray(resp?.missing) ? resp.missing : [];
  if (missing.length === 0) return null;
  const section = h('section', { className: 'recall-section', 'data-section': 'missing' });
  section.appendChild(h('h2', { textContent: `缺什么（${missing.length}）` }));
  const ul = h('ul', { className: 'plain-list', 'data-list': 'missing' });
  for (const m of missing) {
    ul.appendChild(h('li', {
      textContent: `[${m.kind}] ${m.text}：${m.reason}` +
        (m.suggestion ? `（建议：${m.suggestion}）` : ''),
    }));
  }
  section.appendChild(ul);
  return section;
}

// renderDegradedBanners 构造降级横幅：**每一条**都要出现在页面上。
export function renderDegradedBanners(resp) {
  const lines = degradedLines(resp);
  if (lines.length === 0) return [];
  return [banner('warn', `降级（共 ${lines.length} 条，本次召回结果不完整）：${lines.join('；')}`)];
}

// renderAsOfBanner 构造不可折叠的 as_of 横幅。
//
// 没有关闭按钮：这是"你在看历史"的唯一提示，关掉它就会让人误以为看到的是当前状态。
export function renderAsOfBanner(resp, view, { onBackToNow } = {}) {
  const text = asOfBanner(resp, view);
  if (!text) return null;
  const el = h('div', { className: 'asof-banner', role: 'status', 'data-section': 'asof' });
  el.appendChild(h('span', { textContent: text }));
  el.appendChild(h('button', {
    type: 'button',
    onclick: () => {
      if (typeof onBackToNow === 'function') onBackToNow();
    },
    textContent: '回到现在',
  }));
  return el;
}

// renderEmptyState 构造空态（q 为空时**不是**空白，也不是骨架残留）。
export function renderEmptyState({ onPick } = {}) {
  return emptyState({
    kind: 'idle',
    title: '还没有查询',
    reason: 'Recall Inspector 需要一个问题：输入关键词后，这里会给出召回规划、每条结果的分数分解，以及被拦下的候选。',
    suggestions: exampleSuggestions(onPick),
  });
}

// renderNoCapability 构造"后端没有这个能力"的空态（与"没数据"分开说）。
export function renderNoCapability(reason) {
  return emptyState({
    kind: 'noCapability',
    title: '这个后端不支持召回解释',
    reason: reason || 'meta.capabilities.recall_plan = false',
    suggestions: [],
  });
}

// renderLoading 构造骨架屏（带 aria-busy + aria-label）。
export function renderLoading() {
  return skeleton({ rows: 6, label: '正在加载召回解释' });
}

// renderError 构造错误态（参数错误不给"重试"）。
export function renderError({ kind, message, detail, onRetry, onFix }) {
  return errorState({ kind, message, detail, onRetry, onFix });
}

// renderFilters 构造过滤条（scope / kinds / lanes / k 全部进 URL）。
export function renderFilters(view, { onChange } = {}) {
  const wrap = h('div', { className: 'chips', 'data-section': 'filters' });
  const fire = (next) => {
    if (typeof onChange === 'function') onChange(next);
  };

  const scope = h('select', { 'aria-label': 'scope' });
  scope.appendChild(h('option', { value: '', textContent: 'scope：默认（user）' }));
  for (const s of SCOPES) {
    scope.appendChild(h('option', { value: s, textContent: s, selected: view.scope === s ? 'selected' : false }));
  }
  scope.onchange = () => fire({ ...view, scope: scope.value, item: '' });
  wrap.appendChild(scope);

  for (const kind of KINDS) {
    const on = (view.kinds || []).includes(kind);
    wrap.appendChild(h('button', {
      type: 'button',
      className: on ? 'chip on' : 'chip',
      'aria-pressed': on ? 'true' : 'false',
      textContent: kind,
      onclick: () => fire(toggleList(view, 'kinds', kind)),
    }));
  }
  for (const lane of LANES) {
    const on = (view.lanes || []).includes(lane);
    wrap.appendChild(h('button', {
      type: 'button',
      className: on ? 'chip on' : 'chip',
      'aria-pressed': on ? 'true' : 'false',
      textContent: `lane:${lane}`,
      onclick: () => fire(toggleList(view, 'lanes', lane)),
    }));
  }

  const k = h('input', {
    type: 'number', min: '1', max: String(K_MAX), step: '1',
    value: String(view.k ?? ''), 'aria-label': 'k（返回条数）',
  });
  k.onchange = () => {
    const n = Number(k.value);
    // 不夹取：把非法值原样交给 URL，让 parseRecallView 显式报错。
    fire({ ...view, k: Number.isFinite(n) ? n : k.value, item: '' });
  };
  wrap.appendChild(k);
  return wrap;
}

// renderRecallPageView 是整页的纯 DOM 组装（不含 fetch、不改 location）。
//
// 输入是一份**状态快照**，因此单测可以直接喂固定响应断言页面结构：
//
//	{ view, status, resp, errors, error }
//
// status: 'idle' | 'loading' | 'ready' | 'error'
export function renderRecallPageView(snapshot, handlers = {}) {
  const { view = {}, status = 'idle', resp = null, errors = [], error = null } = snapshot || {};
  const root = h('div', { className: 'recall-body', 'data-status': status });

  // 1) as_of 横幅（最先出现：它在语义上"先于"结果）
  const asof = renderAsOfBanner(resp, view, handlers);
  if (asof) root.appendChild(asof);

  // 2) 错误 / 降级横幅
  const banners = h('div', { className: 'recall-banners' });
  for (const e of errors) {
    banners.appendChild(banner('error', `参数错误（${e.field}）：${e.message}`));
  }
  if (resp) {
    for (const b of renderDegradedBanners(resp)) banners.appendChild(b);
    const st = resp.status;
    if (st && st !== 'ok') {
      banners.appendChild(banner('info', `召回状态：${st}（coverage=${fmt(resp.coverage)}）`));
    }
  }
  root.appendChild(banners);

  // 3) 三态：加载 / 参数或请求错误 / 就绪
  if (status === 'loading') {
    root.appendChild(renderLoading());
    return root;
  }
  if (errors.length) {
    root.appendChild(renderError({
      kind: 'param',
      message: errors.map((e) => e.message).join('；'),
      detail: `出错参数：${errors.map((e) => e.field).join(', ')}`,
      onFix: handlers.onFixParams,
    }));
    return root;
  }
  if (status === 'error') {
    root.appendChild(renderError({
      kind: error?.kind || 'request',
      message: error?.message || '未知错误',
      detail: error?.detail || '',
      onRetry: handlers.onRetry,
    }));
    return root;
  }

  // 4) 空态（还没查询）——在**任何**骨架/结果之前判定，避免"骨架残留"
  if (isEmptyQuery(view)) {
    root.appendChild(renderEmptyState({ onPick: (q) => {
      if (typeof handlers.onQuery === 'function') handlers.onQuery(q);
    } }));
    return root;
  }

  // 5) 没有能力 ≠ 没有数据
  const noCap = noCapabilityReason(resp);
  if (noCap) {
    root.appendChild(renderNoCapability(noCap));
    return root;
  }

  if (!resp) {
    root.appendChild(renderLoading());
    return root;
  }

  // 6) 就绪：过滤条 + 列表 + 详情并排
  root.appendChild(renderFilters(view, { onChange: handlers.onViewChange }));
  const layout = h('div', { className: 'recall-layout' });
  const list = h('div', { className: 'recall-list' });
  list.appendChild(renderPlanSection(resp));
  list.appendChild(renderLaneSection(resp));
  const missing = renderMissingSection(resp);
  if (missing) list.appendChild(missing);
  list.appendChild(renderResultList(resp, {
    selectedId: view.item || '',
    onSelect: (id) => {
      if (typeof handlers.onSelectItem === 'function') handlers.onSelectItem(id);
    },
  }));
  list.appendChild(renderDroppedSection(resp));
  layout.appendChild(list);

  const detail = renderDetail(resp, view.item || '', {
    onClose: () => {
      if (typeof handlers.onSelectItem === 'function') handlers.onSelectItem('');
    },
  });
  // 宽屏并排；窄屏由 CSS 变成底部抽屉。没有选中项时也保留面板
  // （否则宽屏右侧会空出一块，读者不知道那里本来该有什么）。
  layout.appendChild(detail);
  root.appendChild(layout);
  return root;
}

// 便于页面层与单测复用的导出。
export {
  clearAsOf, selectItem, withQuery, toggleList,
};
