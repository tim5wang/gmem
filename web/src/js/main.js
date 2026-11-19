// Graph View 单页应用（原生 DOM，无框架）。
//
// 结构：state（视图参数 + 场景）→ render() 全量重画。
// 为什么不上 React/Vue：整页只有一个画布 + 两个侧栏，状态就十来个字段；
// 框架会多一条构建链风险（版本漂移、打包体积），换不来可维护性。
//
// 八项交互的落点在本文件内有明确注释，便于验收逐条对照。
import { fetchGraph, fetchEntity, postReview, ApiError } from './api.js';
import {
  buildScene,
  filterScene,
  applyAsOf,
  sceneTimeRange,
  relationTypes,
  conceptOptions,
  conceptColor,
} from './model.js';
import { parseView, toQuery, toGraphURL, toEntityURL, overviewView, focusView, searchView, BUDGET_MAX } from './urlstate.js';
import { render, hitTest, hitTestCluster, isPendingReview } from './render.js';
import { shouldUseCanvas } from './geometry.js';
import { relayout, withLayout } from './layout.js';
import { requestReview, reviewPrompt } from './reviewflow.js';

const MAX_SVG = 2000;

const dom = {
  svg: document.getElementById('graph-svg'),
  canvas: document.getElementById('graph-canvas'),
  stage: document.getElementById('stage'),
  tooltip: document.getElementById('tooltip'),
  stageNote: document.getElementById('stage-note'),
  banners: document.getElementById('banners'),
  qInput: document.getElementById('q-input'),
  hopsInput: document.getElementById('hops-input'),
  budgetInput: document.getElementById('budget-input'),
  searchForm: document.getElementById('search-form'),
  overviewBtn: document.getElementById('overview-btn'),
  relayoutBtn: document.getElementById('relayout-btn'),
  copyBtn: document.getElementById('copy-btn'),
  modeBadge: document.getElementById('mode-badge'),
  conceptFilters: document.getElementById('concept-filters'),
  relationFilters: document.getElementById('relation-filters'),
  entityCard: document.getElementById('entity-card'),
  asOfRange: document.getElementById('asof-range'),
  asOfLabel: document.getElementById('asof-label'),
  asOfReset: document.getElementById('asof-reset'),
  confirmModal: document.getElementById('confirm-modal'),
  confirmTitle: document.getElementById('confirm-title'),
  confirmText: document.getElementById('confirm-text'),
  confirmOk: document.getElementById('confirm-ok'),
  confirmCancel: document.getElementById('confirm-cancel'),
};

const state = {
  view: { focus: '', q: '', scope: '', hops: 1, budget: 200, asOf: null },
  raw: null, // 服务端原始响应
  scene: null, // 渲染用场景（过滤后）
  baseScene: null, // 未过滤场景（过滤选项的来源）
  conceptFilter: new Set(),
  relationFilter: new Set(),
  selectedId: '',
  project: null,
  tooltipEdge: '',
  errors: [],
  /** @type {Set<string>} 当前视图中待审的 possiblySame 边（裁决后移除） */
  reviewed: new Set(),
};

// ---------------------------------------------------------------------------
// URL 即视图：唯一的真源是 location，state 是它的投影
// ---------------------------------------------------------------------------

function currentFetchImpl() {
  return globalThis.fetch.bind(globalThis);
}

function syncURL(view, { replace = false } = {}) {
  const qs = toQuery(view);
  const url = `${location.pathname}${qs}`;
  if (replace) history.replaceState({ view }, '', url);
  else history.pushState({ view }, '', url);
}

// ---------------------------------------------------------------------------
// 加载与渲染
// ---------------------------------------------------------------------------

async function loadView(view) {
  state.errors = [];
  syncURL(view, { replace: false });

  // 非法参数（如 budget>2000）**不夹取**：直接显式报错，不发请求、不白屏。
  const { view: parsed, errors } = parseView(toQuery(view));
  if (errors.length) {
    state.errors = errors;
    state.scene = null;
    state.baseScene = null;
    renderBanners();
    renderStageNote('参数错误：' + errors.map((e) => e.message).join('；'));
    dom.entityCard.textContent = '参数有误，未发起请求。';
    return;
  }
  state.view = parsed;
  dom.qInput.value = parsed.q;
  dom.hopsInput.value = String(parsed.hops);
  dom.budgetInput.value = String(parsed.budget);

  let resp;
  try {
    resp = await fetchGraph(currentFetchImpl(), toGraphURL(parsed));
  } catch (err) {
    state.scene = null;
    state.baseScene = null;
    const msg = err instanceof ApiError ? err.message : String(err);
    state.errors = [{ field: err?.field || '', message: msg }];
    renderBanners();
    renderStageNote(`加载失败：${msg}`);
    return;
  }

  state.raw = resp;
  // 默认**直接采用服务端坐标**（buildScene 只复制 x/y，不做任何几何演算）。
  state.baseScene = buildScene(resp, { asOf: parsed.asOf });
  // 先渲染周边（banner / 侧栏 / 时间轴）再画图：banners 占高会改变 stage 高度，
  // 若先画图后填 banner，首帧投影会停在旧尺寸（首屏下方一条空白带）。
  renderBanners();
  renderFilterPanel();
  renderTimeline();
  applyFiltersAndDraw();
}

function applyFiltersAndDraw() {
  if (!state.baseScene) return;
  const filtered = filterScene(state.baseScene, {
    concepts: state.conceptFilter,
    relations: state.relationFilter,
  });
  // 过滤不改变坐标：filterScene 只做筛选，节点对象与其 x/y 原样带过来。
  state.scene = filtered;
  draw();
}

function stageSize() {
  const rect = dom.stage.getBoundingClientRect();
  return {
    width: Math.max(320, Math.floor(rect.width)),
    height: Math.max(240, Math.floor(rect.height)),
    devicePixelRatio: globalThis.devicePixelRatio || 1,
  };
}

function draw() {
  if (!state.scene) return;
  const useCanvas = shouldUseCanvas(state.scene, MAX_SVG);
  const host = { svg: dom.svg, canvas: dom.canvas };
  // 首帧陈旧投影的根治：量一次 → 画 → 再量；尺寸变了就重画。
  //
  // 旧实现只量一次：draw() 跑在周边布局（banner/侧栏）定稿之前，拿到的
  // stage 高度偏小（实测 373 而容器 516），viewBox 就停在旧值 —— 首屏下方
  // 一条空白带，且要等用户 resize 一次才收敛。这里用**同步**重测循环
  // （最多 3 轮，防止容器持续变化时卡死），不依赖 rAF / ResizeObserver 的
  // 时序；render 只画在绝对定位的 svg/canvas 上，不会反过来把容器撑大。
  let size = stageSize();
  let result = null;
  for (let i = 0; i < 3; i++) {
    result = render(host, state.scene, { ...size, useCanvas });
    const again = stageSize();
    if (again.width === size.width && again.height === size.height) break;
    size = again;
  }
  // render() 现在返回渲染器产物（含 project）：Canvas 档的命中测试全靠它。
  state.project = result.project;
  dom.stage.dataset.renderer = useCanvas ? 'canvas' : 'svg';
  dom.stage.dataset.nodes = String(state.scene.nodes.length);
  dom.stage.dataset.clusters = String(state.scene.clusters.length);

  bindInteractions(useCanvas);
  renderStageNote(buildStageNote());
  // 重画后 tooltip 位置已失效，直接收起（避免"标签悬停在旧位置"）。
  hideTooltip();
}

function buildStageNote() {
  const meta = state.scene.meta || {};
  const rendered = state.scene.nodes.length;
  const clusters = state.scene.clusters.length;
  if (!meta.truncated && !clusters) return '';
  const shown = `已折叠，未显示全部：node_count=${meta.node_count ?? '?'}，` +
    `rendered_nodes=${meta.rendered_nodes ?? rendered}，clusters=${clusters}（团内共 ${meta.node_count ?? '?'} 个实体）`;
  return meta.truncated ? shown : `渲染 ${rendered} 个节点；按 concept 聚成 ${clusters} 个团（未截断）`;
}

function renderStageNote(text) {
  if (!text) {
    dom.stageNote.hidden = true;
    dom.stageNote.textContent = '';
    return;
  }
  dom.stageNote.hidden = false;
  dom.stageNote.textContent = text;
}

// 降级可见：meta.degraded / recall.degraded / 参数错误都必须出现在页面上，
// 不能只写 console（TD §9 与验收「降级可见」）。
function renderBanners() {
  const parts = [];
  const meta = state.scene?.meta || state.raw?.meta || {};

  for (const e of state.errors) {
    parts.push(banner('error', `参数/请求错误${e.field ? `（${e.field}）` : ''}：${e.message}`));
  }
  if (Array.isArray(meta.degraded) && meta.degraded.length) {
    parts.push(banner('warn', `降级（meta.degraded，共 ${meta.degraded.length} 条）：${meta.degraded.join('；')}`));
  }
  if (meta.recall && Array.isArray(meta.recall.degraded) && meta.recall.degraded.length) {
    parts.push(banner('warn', `召回降级（recall.degraded）：${meta.recall.degraded.join('；')}`));
  }
  if (meta.recall && meta.recall.status && meta.recall.status !== 'ok') {
    parts.push(banner('info', `召回状态：${meta.recall.status}（coverage=${meta.recall.coverage ?? '?'}，items=${meta.recall.items ?? '?'}）`));
  }
  if (meta.truncated) {
    parts.push(banner('info',
      `已折叠，未显示全部：node_count=${meta.node_count}，rendered_nodes=${meta.rendered_nodes}，` +
      `clusters=${meta.rendered_clusters ?? state.scene?.clusters.length ?? 0}，budget=${meta.budget}/${meta.budget_max}`));
  }
  const reviewed = state.reviewed.size
    ? banner('ok', `本次已裁决 ${state.reviewed.size} 条 possiblySame 边（服务端为准，刷新后可见结果）`)
    : '';
  if (reviewed) parts.push(reviewed);

  // 安全说明（F-5）：parts 里每一条都由 banner() 用 textContent 构造后再取
  // outerHTML —— 服务端可控字符串（meta.degraded / recall.degraded / ApiError.message）
  // 已经被 textContent 转义成 HTML 实体，永远不可能变成标签或事件属性。
  // 基线差分实测：恶意库只多出“尖括号被当文本”的元素，可执行元素 0 条。
  dom.banners.innerHTML = parts.join('');
  dom.modeBadge.textContent = meta.mode ? `${meta.mode} · hops=${meta.hops ?? '-'} · budget=${meta.budget ?? '-'}` : '-';
}

// banner 里的 text 全部经由 textContent 写入，所以下面取 outerHTML 时已是转义后的
// 字符串（服务端值不会作为标签生效）。
function banner(kind, text) {
  const div = document.createElement('div');
  div.className = `banner ${kind}`;
  div.textContent = text;
  return div.outerHTML;
}

// ---------------------------------------------------------------------------
// 过滤面板（concept 多选 / relation 多选）+ 时间轴
// ---------------------------------------------------------------------------

function renderFilterPanel() {
  const scene = state.baseScene;
  if (!scene) return;

  const prevConceptKeys = new Set([...dom.conceptFilters.querySelectorAll('input')].map((i) => i.value));
  dom.conceptFilters.textContent = '';
  const options = conceptOptions(scene);
  for (const o of options) {
    const id = `cf-${o.key || 'unclustered'}`;
    const label = document.createElement('label');
    label.className = 'check';
    const cb = document.createElement('input');
    cb.type = 'checkbox';
    cb.value = o.key;
    cb.id = id;
    cb.checked = state.conceptFilter.size ? state.conceptFilter.has(o.key) : false;
    cb.dataset.role = 'concept';
    const sw = document.createElement('span');
    sw.className = 'sw';
    sw.style.background = o.draft ? 'repeating-linear-gradient(45deg,#20262e 0 2px,transparent 2px 5px)' : conceptColor(o.key);
    const txt = document.createElement('span');
    txt.textContent = `${o.label || '(未归属概念)'} (${o.count})`;
    label.append(cb, sw, txt);
    dom.conceptFilters.appendChild(label);
    if (prevConceptKeys.has(o.key) && state.conceptFilter.has(o.key)) cb.checked = true;
  }

  const prevRels = new Set([...dom.relationFilters.querySelectorAll('input')].map((i) => i.value));
  dom.relationFilters.textContent = '';
  for (const r of relationTypes(scene)) {
    const label = document.createElement('label');
    label.className = 'check';
    const cb = document.createElement('input');
    cb.type = 'checkbox';
    cb.value = r.relation;
    cb.dataset.role = 'relation';
    cb.checked = state.relationFilter.size ? state.relationFilter.has(r.relation) : false;
    if (prevRels.has(r.relation) && state.relationFilter.has(r.relation)) cb.checked = true;
    const txt = document.createElement('span');
    txt.textContent = `${r.relation} (${r.count})`;
    label.append(cb, txt);
    dom.relationFilters.appendChild(label);
  }
}

function renderTimeline() {
  const range = state.scene ? sceneTimeRange(state.scene) : null;
  if (!range) {
    dom.asOfRange.disabled = true;
    dom.asOfLabel.textContent = '当前';
    return;
  }
  dom.asOfRange.disabled = false;
  dom.asOfRange.min = String(range.min);
  dom.asOfRange.max = String(range.max);
  const cur = state.view.asOf ?? range.max;
  dom.asOfRange.value = String(cur);
  dom.asOfLabel.textContent = state.view.asOf === null ? '当前' : new Date(Number(state.view.asOf) * 1000).toISOString();
}

// ---------------------------------------------------------------------------
// 交互绑定
// ---------------------------------------------------------------------------

function bindInteractions(useCanvas) {
  const svgNodes = dom.svg.querySelectorAll('g.node');
  if (!useCanvas) {
    // (1) 点击节点 → 侧栏实体卡；(2) 双击 → 以它为焦点展开一跳。
    for (const g of svgNodes) {
      const id = g.getAttribute('data-node-id');
      g.addEventListener('click', (ev) => {
        ev.stopPropagation();
        selectNode(id);
      });
      g.addEventListener('dblclick', (ev) => {
        ev.stopPropagation();
        focusNode(id);
      });
      g.addEventListener('mouseenter', (ev) => showNodeTooltip(id, ev));
      g.addEventListener('mouseleave', hideTooltip);
    }
    // (7) 待审 possiblySame 边点「同一/不同」：点击蓝边打开裁决小窗。
    //
    // 选择器只看 class：render 层已经把 class 定为 possiblySame **且未失效**，
    // 已裁决过的边不再带 possibly-same（不再蓝、不绑 click），与服务端门禁双保险。
    for (const line of dom.svg.querySelectorAll('line.edge.possibly-same')) {
      const edgeId = line.getAttribute('data-edge-id');
      line.addEventListener('click', (ev) => {
        ev.stopPropagation();
        openReview(edgeId);
      });
      line.addEventListener('mouseenter', (ev) => showEdgeTooltip(edgeId, ev));
      line.addEventListener('mouseleave', hideTooltip);
    }
    // (d/e) 非待审边也允许悬停看标签（"边标签仅悬停显示"）。
    for (const line of dom.svg.querySelectorAll('line.edge:not(.possibly-same)')) {
      const edgeId = line.getAttribute('data-edge-id');
      line.addEventListener('mouseenter', (ev) => showEdgeTooltip(edgeId, ev));
      line.addEventListener('mouseleave', hideTooltip);
    }
    // 折叠视图下钻：点 concept 团 → 以该概念的一个代表实体为焦点。
    // 只在团真的可点时绑（render 层未折叠时给团 circle 设了 pointer-events:none，
    // 就不该再有 click 语义）。
    for (const g of dom.svg.querySelectorAll('g.cluster.drillable')) {
      const conceptId = g.getAttribute('data-cluster-id');
      g.addEventListener('click', (ev) => {
        ev.stopPropagation();
        drillIntoCluster(conceptId);
      });
    }
  } else {
    // Canvas 档：用坐标命中测试补回同一套交互（失去逐元素事件，语义不变）。
    const canvas = dom.canvas;
    let lastClickAt = 0;
    let lastClickId = '';
    canvas.onclick = (ev) => {
      const { x, y } = localPoint(ev);
      const node = hitTest(state.scene, state.project, x, y);
      if (node) {
        const now = Date.now();
        if (lastClickId === node.id && now - lastClickAt < 350) {
          focusNode(node.id); // 双击
        } else {
          selectNode(node.id); // 单击
        }
        lastClickId = node.id;
        lastClickAt = now;
        return;
      }
      const cluster = state.scene.meta?.truncated
        ? hitTestCluster(state.scene, state.project, x, y)
        : null;
      if (cluster) drillIntoCluster(cluster.concept_id || '');
    };
    canvas.onmousemove = (ev) => {
      const { x, y } = localPoint(ev);
      const node = hitTest(state.scene, state.project, x, y);
      if (node) {
        showNodeTooltip(node.id, ev);
        return;
      }
      const edge = nearestEdge(x, y);
      if (edge) showEdgeTooltip(edge.id, ev);
      else hideTooltip();
    };
    canvas.onmouseleave = hideTooltip;
  }
}

function localPoint(ev) {
  const rect = dom.canvas.getBoundingClientRect();
  return { x: ev.clientX - rect.left, y: ev.clientY - rect.top };
}

// nearestEdge 是 Canvas 档的边悬停命中（点到线段的距离 < 5px）。
function nearestEdge(x, y) {
  let best = null;
  let bestD = 6;
  for (const e of state.scene.edges) {
    const a = state.scene.nodes.find((n) => n.id === e.from);
    const b = state.scene.nodes.find((n) => n.id === e.to);
    if (!a || !b) continue;
    const pa = state.project(a.x, a.y);
    const pb = state.project(b.x, b.y);
    const d = pointSegmentDistance(x, y, pa.x, pa.y, pb.x, pb.y);
    if (d < bestD) {
      best = e;
      bestD = d;
    }
  }
  return best;
}

function pointSegmentDistance(px, py, x1, y1, x2, y2) {
  const dx = x2 - x1;
  const dy = y2 - y1;
  const len2 = dx * dx + dy * dy;
  if (len2 === 0) return Math.hypot(px - x1, py - y1);
  let t = ((px - x1) * dx + (py - y1) * dy) / len2;
  t = Math.max(0, Math.min(1, t));
  return Math.hypot(px - (x1 + t * dx), py - (y1 + t * dy));
}

// (1) 点击节点 → 实体卡（走 /api/entity/{id}）
async function selectNode(id) {
  state.selectedId = id;
  dom.entityCard.textContent = `加载实体卡 ${id}…`;
  try {
    const card = await fetchEntity(currentFetchImpl(), toEntityURL(id, state.view));
    dom.entityCard.textContent = '';
    dom.entityCard.appendChild(renderEntityCard(card));
  } catch (err) {
    dom.entityCard.textContent = `实体卡加载失败：${err instanceof ApiError ? err.message : err}`;
  }
}

function renderEntityCard(card) {
  const frag = document.createDocumentFragment();
  const h = document.createElement('h3');
  h.textContent = card.canonical_name || card.id;
  frag.appendChild(h);

  const meta = document.createElement('p');
  meta.className = 'hint';
  meta.textContent = `id=${card.id} · scope=${card.scope}` +
    (card.concept ? ` · concept=${card.concept.name || card.concept.id}（${card.concept.status}）` : ' · 无概念');
  frag.appendChild(meta);

  if (Array.isArray(card.meta?.degraded) && card.meta.degraded.length) {
    const d = document.createElement('p');
    d.className = 'banner warn';
    d.textContent = `降级：${card.meta.degraded.join('；')}`;
    frag.appendChild(d);
  }

  if (Array.isArray(card.why) && card.why.length) {
    const hh = document.createElement('h4');
    hh.textContent = '为何生效 / 替换了什么';
    frag.appendChild(hh);
    const ul = document.createElement('ul');
    for (const w of card.why) {
      const li = document.createElement('li');
      li.textContent = w;
      ul.appendChild(li);
    }
    frag.appendChild(ul);
  }

  if (card.source) {
    const hh = document.createElement('h4');
    hh.textContent = '来源 episode';
    frag.appendChild(hh);
    const p = document.createElement('p');
    p.className = 'episode';
    p.textContent = `${card.source.id} @ ${new Date(Number(card.source.at) * 1000).toISOString()}` +
      (card.source.content ? `：${card.source.content}` : '');
    frag.appendChild(p);
  }

  if (Array.isArray(card.relations) && card.relations.length) {
    const hh = document.createElement('h4');
    hh.textContent = '关系';
    frag.appendChild(hh);
    for (const g of card.relations) {
      const box = document.createElement('div');
      box.className = 'rel-group';
      const title = document.createElement('div');
      title.className = 'rel-title';
      title.textContent = g.relation;
      box.appendChild(title);
      if (g.effective) box.appendChild(edgeRow(g.effective, '生效'));
      for (const r of g.replaced || []) box.appendChild(edgeRow(r, '已替换'));
      if (g.reason) {
        const p = document.createElement('p');
        p.className = 'hint';
        p.textContent = g.reason;
        box.appendChild(p);
      }
      frag.appendChild(box);
    }
  }
  return frag;
}

function edgeRow(e, tag) {
  const row = document.createElement('div');
  row.className = 'rel-row';
  const arrow = e.direction === 'in' ? '←' : '→';
  row.textContent = `${tag} ${arrow} ${e.counterpart_name || e.counterpart}（${e.relation}，` +
    `t_valid=${e.t_valid}${e.t_invalid ? `，t_invalid=${e.t_invalid}` : ''}${e.invalidated ? '，本次视角已失效' : ''}）`;
  return row;
}

// (2) 双击节点 → 以它为焦点展开一跳（写进 URL 并重拉）。
function focusNode(id) {
  if (!id) return;
  loadView(focusView(state.view, id));
}

// (3) 搜索框 → 写 ?q= 并重拉
dom.searchForm.addEventListener('submit', (ev) => {
  ev.preventDefault();
  loadView(searchView(state.view, dom.qInput.value));
});

dom.overviewBtn.addEventListener('click', () => {
  dom.qInput.value = '';
  loadView(overviewView(state.view));
});

dom.hopsInput.addEventListener('change', () => {
  const n = Number(dom.hopsInput.value);
  if (!Number.isFinite(n) || n < 1) return;
  loadView({ ...state.view, hops: Math.floor(n) });
});

dom.budgetInput.addEventListener('change', () => {
  const n = Number(dom.budgetInput.value);
  if (!Number.isFinite(n) || n < 1) return;
  // 超上限**不夹取**：让 loadView 的校验把它显式报出来（与服务端 400 一致）。
  loadView({ ...state.view, budget: n > BUDGET_MAX ? n : Math.floor(n) });
});

// (4)(5) concept 多选 / relation 多选：纯前端过滤，不发请求、不改 URL
// （过滤是"看的范围"，不是"视图定义"；视图定义仍由 focus/q/hops/budget/as_of 决定）。
dom.conceptFilters.addEventListener('change', (ev) => {
  if (ev.target?.dataset?.role !== 'concept') return;
  toggleFilter(state.conceptFilter, ev.target.value, ev.target.checked);
  // banner 先于画图：banner 占高会改变 stage 高度，顺序反了首帧投影又会陈旧。
  renderBanners();
  applyFiltersAndDraw();
  renderStageNote(buildStageNote());
});

dom.relationFilters.addEventListener('change', (ev) => {
  if (ev.target?.dataset?.role !== 'relation') return;
  toggleFilter(state.relationFilter, ev.target.value, ev.target.checked);
  renderBanners();
  applyFiltersAndDraw();
  renderStageNote(buildStageNote());
});

function toggleFilter(set, value, checked) {
  if (checked) set.add(value);
  else set.delete(value);
}

// (6) 时间轴 as_of 拖拽：边实时变淡，**布局不重排**（applyAsOf 只换边样式）。
dom.asOfRange.addEventListener('input', () => {
  if (!state.baseScene) return;
  const n = Number(dom.asOfRange.value);
  state.view = { ...state.view, asOf: n };
  // 只有边样式变化：节点坐标与 layoutVersion 都不动。
  state.baseScene = applyAsOf({ ...state.baseScene, layoutVersion: state.baseScene.layoutVersion }, n);
  state.scene = filterScene(state.baseScene, {
    concepts: state.conceptFilter,
    relations: state.relationFilter,
  });
  dom.asOfLabel.textContent = new Date(n * 1000).toISOString();
  draw();
  // URL 同步但不重拉：拖拽过程中不打断手感（松手后 URL 已是最新）。
  syncURL(state.view, { replace: true });
});

dom.asOfReset.addEventListener('click', () => {
  state.view = { ...state.view, asOf: null };
  syncURL(state.view, { replace: true });
  loadView(state.view);
});

// 重新布局：**唯一的本地演算入口**，输出只写内存，不回写服务端。
dom.relayoutBtn.addEventListener('click', () => {
  if (!state.scene) return;
  const coords = relayout(state.scene.nodes, state.scene.edges);
  state.scene = withLayout(state.scene, coords);
  state.baseScene = withLayout(state.baseScene, coords);
  draw();
  renderStageNote(`${buildStageNote()}（本地重排：坐标仅在本页生效，不写服务端）`.trim());
});

// (8) 复制当前视图链接
dom.copyBtn.addEventListener('click', async () => {
  const link = `${location.origin}${location.pathname}${toQuery(state.view)}`;
  try {
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(link);
    } else {
      throw new Error('clipboard 不可用');
    }
    flashBanner('ok', `已复制当前视图链接：${link}`);
  } catch {
    // 非 https / 无权限时会失败：退化成"把链接显示出来让用户手抄"，
    // 绝不静默失败（用户会以为复制成功）。
    flashBanner('warn', `无法写入剪贴板，请手动复制：${link}`);
  }
});

function flashBanner(kind, text) {
  const div = document.createElement('div');
  div.className = `banner ${kind}`;
  div.textContent = text;
  dom.banners.appendChild(div);
  setTimeout(() => div.remove(), 8000);
}

// 折叠团下钻：以该 concept 下的一个实体为焦点（服务端不支持按 concept 取邻域）。
function drillIntoCluster(conceptId) {
  const candidate = state.baseScene?.nodes.find((n) => (n.concept_id || '') === (conceptId || ''));
  if (candidate) {
    focusNode(candidate.id);
    return;
  }
  flashBanner('info', `概念 ${conceptId || '(未归属概念)'} 在本次折叠里没有保留实体（折叠时只保留命中实体）；` +
    '可提高 budget 或先搜索该概念下的实体。');
}

// (7) possiblySame 边裁决：**必须二次确认**（服务端还要求 confirmed:true）。
//
// 三段式（每一段都能单独取消，取消就什么都不发）：
//   点蓝边 → 选「同一 / 不同」→ 二次确认弹窗 → POST /api/review
function openReview(edgeId) {
  const edge = state.scene?.edges.find((e) => e.id === edgeId);
  if (!edge) return;
  // 已失效/已裁决的边不得再进裁决（服务端还会再拦一道 400）。
  if (!isPendingReview(edge)) return;
  const from = state.scene.nodes.find((n) => n.id === edge.from);
  const to = state.scene.nodes.find((n) => n.id === edge.to);
  const label = `${from?.canonical_name || edge.from} ⇄ ${to?.canonical_name || edge.to}`;
  chooseVerdict(label, edgeId);
}

// chooseVerdict 用非阻塞的按钮组让用户先表达意图，再进二次确认。
// 导出它以便于 e2e 直接驱动（避免依赖具体 DOM 选择器细节）。
function chooseVerdict(label, edgeId) {
  const wrap = document.createElement('div');
  wrap.className = 'banner info verdict-picker';
  wrap.dataset.edgeId = edgeId;
  wrap.textContent = `待审边 ${label}：`;
  const same = document.createElement('button');
  same.type = 'button';
  same.textContent = '同一';
  const diff = document.createElement('button');
  diff.type = 'button';
  diff.textContent = '不同';
  const cancel = document.createElement('button');
  cancel.type = 'button';
  cancel.textContent = '取消';
  const pick = (verdict) => {
    wrap.remove();
    if (!verdict) return; // 取消：不发请求，不弹确认
    submitReview(edgeId, verdict, label);
  };
  same.onclick = () => pick('same');
  diff.onclick = () => pick('diff');
  cancel.onclick = () => pick('');
  wrap.append(same, diff, cancel);
  dom.banners.appendChild(wrap);
  return wrap;
}

// confirmThenRun 是二次确认的通用包装（供将来其它写操作复用）。
function confirmThenRun({ title, text, onConfirm }) {
  dom.confirmTitle.textContent = title;
  dom.confirmText.textContent = text;
  openConfirmModal().then((ok) => {
    if (ok === true) onConfirm();
  });
}

// submitReview 是唯一会发起写请求的入口。
async function submitReview(edgeId, verdict, label = edgeId) {
  try {
    // 三道闸门：verdict 合法 → 二次确认 → api.postReview 再验 confirmed。
    // 任何一道取消都不会发出写请求（服务端还要求 confirmed:true，双保险）。
    const outcome = await requestReview({
      edgeId,
      verdict,
      confirm: () => {
        const p = reviewPrompt(label, verdict);
        dom.confirmTitle.textContent = p.title;
        dom.confirmText.textContent = p.text;
        return openConfirmModal();
      },
      send: ({ edgeId: id, verdict: v }) => postReview(currentFetchImpl(), { edgeId: id, verdict: v, confirmed: true }),
    });
    if (!outcome.sent) {
      if (outcome.reason === 'not_confirmed') flashBanner('info', '已取消：未发出裁决请求');
      return;
    }
    const res = outcome.result || {};
    state.reviewed.add(edgeId);
    const extra = [
      res.degraded?.length ? `降级：${res.degraded.join('；')}` : '',
      res.warnings?.length ? `提示：${res.warnings.join('；')}` : '',
    ].filter(Boolean).join(' ');
    flashBanner('ok', `裁决已提交：${verdict} → op=${res.op || '?'} status=${res.status || '?'}${extra ? `（${extra}）` : ''}`);
    renderBanners();
    // 重新拉一次以反映服务端结果（合并后的实体会换 ID，缓存会过期）。
    loadView(state.view);
  } catch (err) {
    flashBanner('error', `裁决失败：${err instanceof ApiError ? err.message : err}`);
  }
}

// openConfirmModal 把二次确认弹窗包成 Promise<boolean>：
// 关窗（取消 / ESC / 点遮罩）= false，按「确认」= true。
function openConfirmModal() {
  return new Promise((resolve) => {
    dom.confirmModal.hidden = false;
    const done = (ok) => {
      dom.confirmModal.hidden = true;
      dom.confirmOk.onclick = null;
      dom.confirmCancel.onclick = null;
      dom.confirmModal.onclick = null;
      resolve(ok);
    };
    dom.confirmOk.onclick = () => done(true);
    dom.confirmCancel.onclick = () => done(false);
    // 点遮罩也算取消（但点弹窗内部不算）。
    dom.confirmModal.onclick = (ev) => {
      if (ev.target === dom.confirmModal) done(false);
    };
  });
}

// ---------------------------------------------------------------------------
// Tooltip
// ---------------------------------------------------------------------------

function showNodeTooltip(id, ev) {
  const n = state.scene?.nodes.find((x) => x.id === id);
  if (!n) return;
  dom.tooltip.hidden = false;
  dom.tooltip.textContent =
    `${n.canonical_name || n.id} · concept=${n.concept_name || n.key || '(无)'}` +
    `（${n.concept_status || '?'}）· degree=${n.degree} · depth=${n.depth}` +
    (n.matched ? ' · 本次命中' : '');
  moveTooltip(ev);
}

// 边标签**仅悬停显示**（TD §9 视觉编码最后一行）。
function showEdgeTooltip(edgeId, ev) {
  const e = state.scene?.edges.find((x) => x.id === edgeId);
  if (!e) return;
  dom.tooltipEdge = edgeId;
  dom.tooltip.hidden = false;
  dom.tooltip.textContent =
    `${e.relation} · ${e.from} → ${e.to} · weight=${e.weight}` +
    ` · t_valid=${e.t_valid}${e.t_invalid ? ` · t_invalid=${e.t_invalid}` : ''}` +
    (e.style.invalid ? ' · 本次视角已失效（虚线保留）' : '') +
    // 已失效的边不得再自称“待审”：那句提示就是“同一/不同”按钮的入口。
    (isPendingReview(e) ? ' · 待审（点击可裁决）' : '');
  moveTooltip(ev);
}

function moveTooltip(ev) {
  const rect = dom.stage.getBoundingClientRect();
  dom.tooltip.style.left = `${Math.min(ev.clientX - rect.left + 12, rect.width - 260)}px`;
  dom.tooltip.style.top = `${Math.max(8, ev.clientY - rect.top - 28)}px`;
}

function hideTooltip() {
  dom.tooltip.hidden = true;
  dom.tooltipEdge = '';
}

// ---------------------------------------------------------------------------
// 启动
// ---------------------------------------------------------------------------

window.addEventListener('popstate', () => {
  const { view, errors } = parseView(location.search);
  if (errors.length) {
    state.errors = errors;
    renderBanners();
    return;
  }
  loadView(view);
});

let resizeTimer = null;
window.addEventListener('resize', () => {
  clearTimeout(resizeTimer);
  resizeTimer = setTimeout(() => draw(), 120);
});

export async function boot() {
  const { view, errors } = parseView(location.search);
  if (errors.length) {
    state.errors = errors;
    renderBanners();
    renderStageNote('参数错误：' + errors.map((e) => e.message).join('；'));
    return;
  }
  dom.hopsInput.value = String(view.hops);
  dom.budgetInput.value = String(view.budget);
  dom.qInput.value = view.q;
  await loadView(view);
}

// 便于 node --test / e2e 复用的导出（不影响浏览器行为）。
export { state, loadView, applyFiltersAndDraw, buildStageNote, openReview, confirmThenRun, submitReview, chooseVerdict };

if (typeof document !== 'undefined' && document.getElementById('graph-svg')) {
  boot();
}
