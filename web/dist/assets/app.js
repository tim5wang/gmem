// js/router.js
var ROUTES = Object.freeze({
  "/": "graph",
  "/recall": "recall",
  "/review": "review",
  "/stream": "stream",
  "/ontology": "ontology",
  "/metrics": "metrics"
});
var PAGE_TITLES = Object.freeze({
  graph: "Graph View",
  recall: "Recall Inspector",
  review: "Review Queue",
  stream: "Memory Stream",
  ontology: "Ontology Drift",
  metrics: "Metrics",
  missing: "页面不存在"
});
var IMPLEMENTED = Object.freeze(["graph", "recall"]);
function normalizePath(pathname) {
  const p = String(pathname || "/").trim() || "/";
  if (p === "/") return "/";
  return p.replace(/\/+$/, "") || "/";
}
function matchRoute(pathname) {
  const path = normalizePath(pathname);
  const page = ROUTES[path] || "missing";
  return { page, path };
}
function isImplemented(page) {
  return IMPLEMENTED.includes(page);
}
function notImplementedMessage(page) {
  const title = PAGE_TITLES[page] || page;
  return `「${title}」这一页还没实现（详见 TD §9 六页计划）。当前可用：Graph View 与 Recall Inspector。`;
}

// js/api.js
var ApiError = class extends Error {
  constructor(message, { status = 0, code = "", field = "" } = {}) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
    this.field = field;
  }
};
async function request(fetchImpl, url, options = {}) {
  let resp;
  try {
    resp = await fetchImpl(url, options);
  } catch (err) {
    throw new ApiError(`请求失败：${err && err.message ? err.message : err}`, { code: "network" });
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
      code: body?.error || "",
      field: body?.field || ""
    });
  }
  if (body === null) {
    throw new ApiError("响应不是合法 JSON", { status: resp.status, code: "bad_json" });
  }
  return body;
}
function fetchGraph(fetchImpl, url) {
  return request(fetchImpl, url, { headers: { Accept: "application/json" } });
}
function fetchEntity(fetchImpl, url) {
  return request(fetchImpl, url, { headers: { Accept: "application/json" } });
}
function fetchRecall(fetchImpl, url) {
  return request(fetchImpl, url, { headers: { Accept: "application/json" } });
}
function postReview(fetchImpl, { edgeId, verdict, confirmed }) {
  if (!confirmed) {
    return Promise.reject(new ApiError("未确认的裁决不会发出请求", { code: "not_confirmed" }));
  }
  return request(fetchImpl, "/api/review", {
    method: "POST",
    headers: { "Content-Type": "application/json", Accept: "application/json" },
    body: JSON.stringify({ edge_id: edgeId, verdict, confirmed: true })
  });
}

// js/model.js
function nodeRadius(degree) {
  const d = Number.isFinite(degree) && degree > 0 ? degree : 0;
  return Math.min(22, 5 + Math.sqrt(d) * 3.4);
}
function conceptKey(node) {
  if (!node) return "";
  return node.cluster || node.concept_id || "";
}
function hashCode(str) {
  let h2 = 5381;
  for (let i = 0; i < str.length; i++) {
    h2 = (h2 << 5) + h2 + str.charCodeAt(i) >>> 0;
  }
  return h2 >>> 0;
}
function conceptHue(key) {
  if (!key) return 210;
  return hashCode(key) * 137.508 % 360;
}
function conceptColor(key, { lightness = 46, saturation = 62 } = {}) {
  return `hsl(${conceptHue(key).toFixed(1)} ${saturation}% ${lightness}%)`;
}
function isDraft(node) {
  return (node?.concept_status || "").toLowerCase() === "draft";
}
function isEdgeInvalidAt(edge, asOf) {
  if (asOf === null || asOf === void 0) return Boolean(edge.invalidated);
  const tValid = Number(edge.t_valid || 0);
  const tInvalid = Number(edge.t_invalid || 0);
  if (tValid > asOf) return true;
  if (tInvalid && tInvalid <= asOf) return true;
  return false;
}
function edgeStyle(edge, asOf) {
  const invalid = isEdgeInvalidAt(edge, asOf);
  const possiblySame = Boolean(edge.possibly_same);
  return {
    invalid,
    possiblySame,
    dashed: invalid,
    opacity: invalid ? 0.22 : 1,
    color: possiblySame ? "#1d6fd8" : invalid ? "#8a94a6" : "#5c6472",
    width: possiblySame ? 2 : 1.2
  };
}
function buildScene(resp, { asOf = null, concepts = null, relations = null } = {}) {
  const raw = Array.isArray(resp?.nodes) ? resp.nodes : [];
  const nodes = raw.map((n) => ({
    ...n,
    x: Number(n.x),
    y: Number(n.y),
    r: nodeRadius(n.degree),
    key: conceptKey(n),
    draft: isDraft(n),
    matched: Boolean(n.matched)
  }));
  const present = new Set(nodes.map((n) => n.id));
  const edges = (Array.isArray(resp?.edges) ? resp.edges : []).filter((e) => present.has(e.from) && present.has(e.to)).map((e) => ({ ...e, style: edgeStyle(e, asOf) }));
  const scene = {
    meta: resp?.meta || {},
    nodes,
    edges,
    clusters: Array.isArray(resp?.clusters) ? resp.clusters : [],
    clusterEdges: Array.isArray(resp?.cluster_edges) ? resp.cluster_edges : [],
    asOf,
    // layoutVersion 只由 layout.js 的显式重排递增；用它给"是否重排过"留痕，
    // 单测据此断言 as_of 变化不触发重排。
    layoutVersion: 0
  };
  return filterScene(scene, { concepts, relations });
}
function filterScene(scene, { concepts = null, relations = null } = {}) {
  const conceptSet = concepts && concepts.size ? concepts : null;
  const relationSet = relations && relations.size ? relations : null;
  const nodes = conceptSet ? scene.nodes.filter((n) => conceptSet.has(n.key)) : scene.nodes.slice();
  const present = new Set(nodes.map((n) => n.id));
  const edges = scene.edges.filter(
    (e) => present.has(e.from) && present.has(e.to) && (!relationSet || relationSet.has(e.relation))
  );
  const clusters = conceptSet ? scene.clusters.filter((c2) => conceptSet.has(c2.id || c2.concept_id || "")) : scene.clusters.slice();
  return { ...scene, nodes, edges, clusters };
}
function applyAsOf(scene, asOf) {
  return {
    ...scene,
    asOf,
    edges: scene.edges.map((e) => ({ ...e, style: edgeStyle(e, asOf) })),
    nodes: scene.nodes
    // 引用不变：坐标一个都没动
  };
}
function sceneTimeRange(scene) {
  let min = Infinity;
  let max = -Infinity;
  const consider = (v) => {
    const n = Number(v);
    if (Number.isFinite(n) && n > 0) {
      if (n < min) min = n;
      if (n > max) max = n;
    }
  };
  for (const e of scene.edges) {
    consider(e.t_valid);
    consider(e.t_invalid);
  }
  for (const c2 of scene.clusters) consider(c2.t_valid);
  if (!Number.isFinite(min) || !Number.isFinite(max) || max <= 0) return null;
  if (max <= min) max = min + 1;
  return { min, max };
}
function relationTypes(scene) {
  const seen = /* @__PURE__ */ new Map();
  for (const e of scene.edges) {
    if (!seen.has(e.relation)) seen.set(e.relation, 0);
    seen.set(e.relation, seen.get(e.relation) + 1);
  }
  return [...seen.entries()].sort((a2, b) => a2[0].localeCompare(b[0])).map(([relation, count]) => ({ relation, count }));
}
function conceptOptions(scene) {
  const seen = /* @__PURE__ */ new Map();
  for (const n of scene.nodes) {
    const label = n.key || "(未归属概念)";
    if (!seen.has(label)) seen.set(label, { key: n.key, label, count: 0, draft: false });
    const o = seen.get(label);
    o.count += 1;
    o.draft = o.draft || n.draft;
  }
  return [...seen.values()].sort((a2, b) => a2.label.localeCompare(b.label));
}

// js/urlstate.js
var BUDGET_MAX = 2e3;
var DEFAULTS = { hops: 1, budget: 200 };
var INT_RE = /^-?\d+$/;
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
function parseView(search) {
  const params = new URLSearchParams(String(search || "").replace(/^\?/, ""));
  const errors = [];
  const view = {
    focus: (params.get("focus") || "").trim(),
    q: (params.get("q") || "").trim(),
    scope: (params.get("scope") || "").trim(),
    hops: DEFAULTS.hops,
    budget: DEFAULTS.budget,
    asOf: null
  };
  if (view.focus && view.q) {
    errors.push({ field: "focus", message: "focus 与 q 只能二选一（概览模式请两者都不传）" });
  }
  if (view.scope && !["user", "agent", "project", "global"].includes(view.scope)) {
    errors.push({ field: "scope", message: `scope 必须是 user / agent / project / global，收到 ${JSON.stringify(view.scope)}` });
  }
  const hopsRaw = params.get("hops");
  if (hopsRaw !== null && hopsRaw !== "") {
    const n = parsePositiveInt(hopsRaw, "hops", errors);
    if (n !== null) view.hops = n;
  }
  const budgetRaw = params.get("budget");
  if (budgetRaw !== null && budgetRaw !== "") {
    const n = parsePositiveInt(budgetRaw, "budget", errors);
    if (n !== null) {
      if (n > BUDGET_MAX) {
        errors.push({ field: "budget", message: `budget 不能超过硬上限 ${BUDGET_MAX}，收到 ${n}（不会静默夹取）` });
      } else {
        view.budget = n;
      }
    }
  }
  const asOfRaw = params.get("as_of");
  if (asOfRaw !== null && asOfRaw !== "") {
    if (!INT_RE.test(asOfRaw)) {
      errors.push({ field: "as_of", message: `as_of 必须是 unix 秒（整数），收到 ${JSON.stringify(asOfRaw)}` });
    } else {
      view.asOf = Number(asOfRaw);
    }
  }
  return { view, errors };
}
function toQuery(view) {
  const params = new URLSearchParams();
  if (view.focus) params.set("focus", view.focus);
  if (view.q) params.set("q", view.q);
  if (view.scope) params.set("scope", view.scope);
  params.set("hops", String(view.hops ?? DEFAULTS.hops));
  params.set("budget", String(view.budget ?? DEFAULTS.budget));
  if (view.asOf !== null && view.asOf !== void 0) params.set("as_of", String(view.asOf));
  return `?${params.toString()}`;
}
function toGraphURL(view) {
  return `/api/graph${toQuery(view)}`;
}
function toEntityURL(id, view) {
  const params = new URLSearchParams();
  if (view?.asOf !== null && view?.asOf !== void 0) params.set("as_of", String(view.asOf));
  const qs = params.toString();
  return `/api/entity/${encodeURIComponent(id)}${qs ? `?${qs}` : ""}`;
}
function overviewView(view) {
  return { ...view, focus: "", q: "" };
}
function focusView(view, id) {
  return { ...view, focus: String(id || ""), q: "" };
}
function searchView(view, rawQuery) {
  const q = String(rawQuery || "").trim();
  if (!q) return overviewView(view);
  return { ...view, q, focus: "" };
}

// js/geometry.js
function bounds(scene) {
  let minX = Infinity;
  let minY = Infinity;
  let maxX = -Infinity;
  let maxY = -Infinity;
  const consider = (x2, y2, r) => {
    if (!Number.isFinite(x2) || !Number.isFinite(y2)) return;
    if (x2 - r < minX) minX = x2 - r;
    if (y2 - r < minY) minY = y2 - r;
    if (x2 + r > maxX) maxX = x2 + r;
    if (y2 + r > maxY) maxY = y2 + r;
  };
  for (const n of scene.nodes) consider(n.x, n.y, 0.02);
  for (const c2 of scene.clusters) consider(c2.x, c2.y, 0.09);
  if (!Number.isFinite(minX)) return { minX: -0.5, minY: -0.5, maxX: 0.5, maxY: 0.5 };
  if (maxX - minX < 1e-6) {
    minX -= 0.5;
    maxX += 0.5;
  }
  if (maxY - minY < 1e-6) {
    minY -= 0.5;
    maxY += 0.5;
  }
  return { minX, minY, maxX, maxY };
}
function clusterPixelRadius(count) {
  return 10 + Math.min(30, Math.sqrt(Math.max(1, count)) * 3.2);
}
function drawExtent(scene) {
  let r = 0;
  for (const n of scene.nodes) {
    const nr = Number.isFinite(n.r) ? n.r : 5;
    if (nr > r) r = nr;
  }
  for (const c2 of scene.clusters || []) {
    const cr = clusterPixelRadius(c2.count);
    if (cr > r) r = cr;
  }
  return r + 3;
}
function makeProjector(scene, width, height, padding = 48) {
  const b = bounds(scene);
  const dx = b.maxX - b.minX;
  const dy = b.maxY - b.minY;
  const pad = Math.max(0, padding);
  const ext = Math.min(drawExtent(scene), Math.min(width, height) / 4);
  const w = Math.max(1, width - pad * 2 - ext * 2);
  const h2 = Math.max(1, height - pad * 2 - ext * 2);
  const scale = Math.min(w / dx, h2 / dy);
  const cx = (b.minX + b.maxX) / 2;
  const cy = (b.minY + b.maxY) / 2;
  const project = (x2, y2) => ({
    x: width / 2 + (x2 - cx) * scale,
    y: height / 2 + (y2 - cy) * scale
  });
  project.scale = scale;
  return project;
}
function shouldUseCanvas(scene, maxSvg = 2e3) {
  return scene.nodes.length + scene.clusters.length > maxSvg;
}

// js/labelLayouter.js
var LABEL = {
  fontSize: 10,
  lineHeight: 12,
  // CJK / 全角字符按 1.75em 计（10px 字号 → 17.5px/字）；
  latinWidth: 0.6,
  cjkWidth: 1.75,
  // 文本框之间额外留 2px，避免"刚好贴边"看起来仍像压叠。
  gap: 2,
  // 候选方位的外扩倍数：先贴近节点，贴不下再往外挪一档（仍是同样四个方位）。
  rings: [1, 1.9]
};
function textWidth(text, fontSize = LABEL.fontSize) {
  let w = 0;
  for (const ch of text) {
    const code = ch.codePointAt(0);
    w += (code > 11904 ? LABEL.cjkWidth : LABEL.latinWidth) * fontSize;
  }
  return w;
}
function candidate(item, anchor, ring, fontSize) {
  const h2 = LABEL.lineHeight;
  const ascent = fontSize * 0.8;
  const descent = fontSize * 0.2;
  const w = item.w;
  const gapPx = (LABEL.gap + 2) * ring;
  let x2;
  let y2;
  if (anchor === "below") {
    x2 = item.x;
    y2 = item.y + item.r + LABEL.fontSize * 0.9 + gapPx;
  } else if (anchor === "above") {
    x2 = item.x;
    y2 = item.y - item.r - gapPx;
  } else if (anchor === "right") {
    x2 = item.x + item.r + gapPx + 2;
    y2 = item.y + LABEL.fontSize * 0.35;
  } else {
    x2 = item.x - item.r - gapPx - 2;
    y2 = item.y + LABEL.fontSize * 0.35;
  }
  let minX;
  if (anchor === "below" || anchor === "above") {
    minX = x2 - w / 2;
  } else if (anchor === "right") {
    minX = x2;
  } else {
    minX = x2 - w;
  }
  return {
    x: x2,
    y: y2,
    anchor,
    minX,
    minY: y2 - ascent,
    maxX: minX + w,
    maxY: y2 - ascent + h2
  };
}
function overlaps(a2, b, gap) {
  return a2.minX - gap < b.maxX && b.minX - gap < a2.maxX && a2.minY - gap < b.maxY && b.minY - gap < a2.maxY;
}
var CELL = 64;
function insert(grid, box) {
  const x0 = Math.floor(box.minX / CELL);
  const x1 = Math.floor(box.maxX / CELL);
  const y0 = Math.floor(box.minY / CELL);
  const y1 = Math.floor(box.maxY / CELL);
  for (let gx = x0; gx <= x1; gx++) {
    for (let gy = y0; gy <= y1; gy++) {
      const k = `${gx},${gy}`;
      let bucket = grid.get(k);
      if (!bucket) {
        bucket = [];
        grid.set(k, bucket);
      }
      bucket.push(box);
    }
  }
}
function collides(grid, box, gap) {
  const x0 = Math.floor((box.minX - gap) / CELL);
  const x1 = Math.floor((box.maxX + gap) / CELL);
  const y0 = Math.floor((box.minY - gap) / CELL);
  const y1 = Math.floor((box.maxY + gap) / CELL);
  for (let gx = x0; gx <= x1; gx++) {
    for (let gy = y0; gy <= y1; gy++) {
      const bucket = grid.get(`${gx},${gy}`);
      if (!bucket) continue;
      for (const other of bucket) {
        if (overlaps(box, other, gap)) return true;
      }
    }
  }
  return false;
}
function clusterObstacles(scene, project, { fontSize = 11 } = {}) {
  const out = [];
  for (const c2 of scene.clusters || []) {
    const p = project(c2.x, c2.y);
    const r = 10 + Math.min(30, Math.sqrt(Math.max(1, c2.count)) * 3.2);
    const text = `${c2.concept_name || c2.id || "(未归属概念)"} · ${c2.count}`;
    const w = textWidth(text, fontSize);
    const y2 = p.y + r + 13;
    out.push({
      minX: p.x - w / 2,
      minY: y2 - fontSize * 0.8,
      maxX: p.x + w / 2,
      maxY: y2 - fontSize * 0.8 + LABEL.lineHeight
    });
  }
  return out;
}
function layoutLabels(items, { width = 0, height = 0, fontSize = LABEL.fontSize, obstacles = [] } = {}) {
  const grid = /* @__PURE__ */ new Map();
  for (const o of obstacles) insert(grid, o);
  const ordered = items.filter((it) => it && Number.isFinite(it.x) && Number.isFinite(it.y)).map((it) => ({
    id: it.id,
    x: it.x,
    y: it.y,
    r: Number.isFinite(it.r) ? it.r : 5,
    degree: Number.isFinite(it.degree) ? it.degree : 0,
    text: String(it.text ?? it.id ?? "")
  })).map((it) => ({ ...it, w: textWidth(it.text, fontSize) })).sort((a2, b) => b.degree - a2.degree || (a2.id < b.id ? -1 : a2.id > b.id ? 1 : 0));
  const labels = [];
  const hidden = [];
  for (const it of ordered) {
    let placed = null;
    for (const ring of LABEL.rings) {
      for (const anchor of ["below", "above", "right", "left"]) {
        const c2 = candidate(it, anchor, ring, fontSize);
        if (width > 0 && (c2.minX < 1 || c2.maxX > width - 1)) continue;
        if (height > 0 && (c2.minY < 1 || c2.maxY > height - 1)) continue;
        if (collides(grid, c2, LABEL.gap)) continue;
        placed = c2;
        break;
      }
      if (placed) break;
    }
    if (!placed) {
      hidden.push(it.id);
      continue;
    }
    insert(grid, placed);
    labels.push({ id: it.id, x: placed.x, y: placed.y, anchor: placed.anchor, text: it.text });
  }
  return { labels, hidden, shown: labels.length, total: ordered.length };
}

// js/render.js
var SVG_NS = "http://www.w3.org/2000/svg";
var STYLE = {
  edgeColor: "#5c6472",
  edgeInvalidColor: "#8a94a6",
  edgePossiblySameColor: "#1d6fd8",
  matchedStroke: "#d32f2f",
  clusterFill: "#eef2f7",
  clusterStroke: "#8d9bb0",
  label: "#1f2933"
};
function render(host, scene, opts) {
  const { svg, canvas } = host;
  const useCanvas = opts.useCanvas;
  if (!useCanvas) {
    canvas.hidden = true;
    svg.hidden = false;
    const out2 = renderSVG(svg, scene, opts);
    return { renderer: "svg", ...out2 };
  }
  svg.hidden = true;
  canvas.hidden = false;
  const out = renderCanvas(canvas, scene, opts);
  return { renderer: "canvas", ...out };
}
function isPendingReview(e) {
  return Boolean(e.style.possiblySame) && !e.style.invalid;
}
function edgeEndpoints(a2, b, ra, rb) {
  const dx = b.x - a2.x;
  const dy = b.y - a2.y;
  const len = Math.hypot(dx, dy);
  if (!(len > 0.01)) return { x1: a2.x, y1: a2.y, x2: b.x, y2: b.y };
  const gap = 2;
  let t1 = (ra + gap) / len;
  let t2 = 1 - (rb + gap) / len;
  if (t2 - t1 < 0.08) {
    const m2 = Math.max(0, Math.min(1, (t1 + t2) / 2));
    t1 = Math.max(0, Math.min(1, m2 - 0.04));
    t2 = Math.max(0, Math.min(1, m2 + 0.04));
  }
  return {
    x1: a2.x + dx * t1,
    y1: a2.y + dy * t1,
    x2: a2.x + dx * t2,
    y2: a2.y + dy * t2
  };
}
function renderSVG(svg, scene, opts) {
  const { width, height } = opts;
  const project = makeProjector(scene, width, height);
  svg.setAttribute("viewBox", `0 0 ${width} ${height}`);
  svg.setAttribute("width", String(width));
  svg.setAttribute("height", String(height));
  while (svg.firstChild) svg.removeChild(svg.firstChild);
  const defs = el("defs", {});
  const pattern = el("pattern", {
    id: "draft-hatch",
    width: "6",
    height: "6",
    patternUnits: "userSpaceOnUse",
    patternTransform: "rotate(45)"
  });
  pattern.appendChild(el("rect", { width: "6", height: "6", fill: "transparent" }));
  pattern.appendChild(el("line", { x1: "0", y1: "0", x2: "0", y2: "6", stroke: "#20262e", "stroke-width": "2", opacity: "0.55" }));
  defs.appendChild(pattern);
  svg.appendChild(defs);
  const gEdges = el("g", { class: "edges" });
  const gClusters = el("g", { class: "clusters" });
  const gNodes = el("g", { class: "nodes" });
  const gLabels = el("g", { class: "labels" });
  svg.appendChild(gEdges);
  svg.appendChild(gClusters);
  svg.appendChild(gNodes);
  svg.appendChild(gLabels);
  const clusterPos = /* @__PURE__ */ new Map();
  for (const c2 of scene.clusters) {
    const p = project(c2.x, c2.y);
    const key = c2.id || c2.concept_id || "";
    clusterPos.set(key, p);
  }
  const pos = /* @__PURE__ */ new Map();
  for (const n of scene.nodes) pos.set(n.id, project(n.x, n.y));
  const nodeRadius2 = /* @__PURE__ */ new Map();
  for (const n of scene.nodes) nodeRadius2.set(n.id, n.r);
  const ordered = [
    ...scene.edges.filter((e) => !isPendingReview(e)),
    ...scene.edges.filter(isPendingReview)
  ];
  for (const e of ordered) {
    const a2 = pos.get(e.from);
    const b = pos.get(e.to);
    if (!a2 || !b) continue;
    const pending = isPendingReview(e);
    const seg = edgeEndpoints(a2, b, nodeRadius2.get(e.from) ?? 0, nodeRadius2.get(e.to) ?? 0);
    const line = el("line", {
      x1: seg.x1.toFixed(1),
      y1: seg.y1.toFixed(1),
      x2: seg.x2.toFixed(1),
      y2: seg.y2.toFixed(1),
      stroke: pending ? STYLE.edgePossiblySameColor : e.style.invalid ? STYLE.edgeInvalidColor : STYLE.edgeColor,
      "stroke-width": String(pending ? 2 : 1.2),
      "stroke-opacity": String(e.style.opacity),
      // 失效边画虚线 = 保留历史（不过滤）。
      "stroke-dasharray": e.style.dashed ? "4 3" : "none",
      "data-edge-id": e.id,
      "data-from": e.from,
      "data-to": e.to,
      "data-invalid": e.style.invalid ? "1" : "0",
      // data-possibly-same 保留服务端语义（关系名），class 才代表“现在可裁决”。
      "data-possibly-same": e.style.possiblySame ? "1" : "0",
      "data-pending": pending ? "1" : "0",
      class: `edge${pending ? " possibly-same" : ""}${e.style.invalid ? " invalidated" : ""}`
    });
    gEdges.appendChild(line);
  }
  const drillable = Boolean(scene.meta?.truncated);
  for (const c2 of scene.clusters) {
    const p = clusterPos.get(c2.id || c2.concept_id || "");
    if (!p) continue;
    const r = 10 + Math.min(30, Math.sqrt(Math.max(1, c2.count)) * 3.2);
    const g = el("g", {
      class: `cluster${drillable ? " drillable" : ""}`,
      "data-cluster-id": c2.concept_id || "",
      "data-cluster-name": c2.concept_name || c2.id || "",
      "data-count": String(c2.count),
      transform: `translate(${p.x.toFixed(1)},${p.y.toFixed(1)})`
    });
    if (drillable) {
      g.appendChild(el("circle", {
        class: "cluster-hit",
        r: r.toFixed(1),
        fill: "transparent",
        stroke: "none",
        "pointer-events": "auto",
        "data-cluster-id": c2.concept_id || ""
      }));
    }
    g.appendChild(el("circle", {
      r: r.toFixed(1),
      fill: c2.concept_status === "draft" ? "url(#draft-hatch)" : STYLE.clusterFill,
      stroke: c2.matched ? STYLE.matchedStroke : STYLE.clusterStroke,
      "stroke-width": c2.matched ? "2.5" : "1.5",
      "pointer-events": "none"
    }));
    const label = el("text", {
      "text-anchor": "middle",
      y: String(r + 13),
      fill: STYLE.label,
      "font-size": "11",
      class: "cluster-label"
    });
    label.textContent = `${c2.concept_name || c2.id || "(未归属概念)"} · ${c2.count}`;
    g.appendChild(label);
    gClusters.appendChild(g);
  }
  for (const ce of scene.clusterEdges || []) {
    const a2 = clusterPos.get(ce.from);
    const b = clusterPos.get(ce.to);
    if (!a2 || !b) continue;
    gClusters.appendChild(el("line", {
      x1: a2.x.toFixed(1),
      y1: a2.y.toFixed(1),
      x2: b.x.toFixed(1),
      y2: b.y.toFixed(1),
      stroke: STYLE.clusterStroke,
      "stroke-width": String(Math.min(4, 1 + Math.log2(Math.max(1, ce.count)))),
      "stroke-opacity": ce.invalidated ? "0.25" : "0.55",
      "stroke-dasharray": ce.invalidated ? "4 3" : "none",
      class: "cluster-edge"
    }));
  }
  for (const n of scene.nodes) {
    const p = pos.get(n.id);
    if (!p) continue;
    const g = el("g", {
      class: `node${n.matched ? " matched" : ""}`,
      transform: `translate(${p.x.toFixed(1)},${p.y.toFixed(1)})`,
      "data-node-id": n.id,
      "data-concept": n.key,
      "data-degree": String(n.degree)
    });
    g.appendChild(el("circle", {
      r: n.r.toFixed(1),
      fill: n.draft ? "url(#draft-hatch)" : conceptColor(n.key),
      stroke: n.matched ? STYLE.matchedStroke : "#ffffff",
      "stroke-width": n.matched ? "2.5" : "1.2"
    }));
    if (n.draft) {
      g.appendChild(el("circle", {
        r: (n.r + 2.5).toFixed(1),
        fill: "none",
        stroke: "#20262e",
        "stroke-width": "1",
        "stroke-dasharray": "2 2",
        opacity: "0.6"
      }));
    }
    gNodes.appendChild(g);
  }
  const labelItems = scene.nodes.filter((n) => pos.has(n.id)).map((n) => {
    const p = pos.get(n.id);
    return {
      id: n.id,
      x: p.x,
      y: p.y,
      r: n.r,
      degree: n.degree,
      text: n.canonical_name || n.id
    };
  });
  const placed = layoutLabels(labelItems, {
    width,
    height,
    obstacles: clusterObstacles(scene, project)
  });
  for (const lab of placed.labels) {
    const text = el("text", {
      x: lab.x.toFixed(1),
      y: lab.y.toFixed(1),
      "text-anchor": lab.anchor === "right" ? "start" : lab.anchor === "left" ? "end" : "middle",
      fill: STYLE.label,
      "font-size": "10",
      "data-label-anchor": lab.anchor,
      class: "node-label"
    });
    text.textContent = lab.text;
    gLabels.appendChild(text);
  }
  svg.setAttribute("data-labels-shown", String(placed.shown));
  svg.setAttribute("data-labels-hidden", String(placed.hidden.length));
  return { project, pos, clusterPos, labels: placed };
}
function el(name, attrs) {
  const node = document.createElementNS(SVG_NS, name);
  for (const [k, v] of Object.entries(attrs)) node.setAttribute(k, v);
  return node;
}
function renderCanvas(canvas, scene, opts) {
  const { width, height } = opts;
  const ratio = opts.devicePixelRatio || 1;
  canvas.width = Math.max(1, Math.floor(width * ratio));
  canvas.height = Math.max(1, Math.floor(height * ratio));
  canvas.style.width = `${width}px`;
  canvas.style.height = `${height}px`;
  const ctx = canvas.getContext("2d");
  ctx.setTransform(ratio, 0, 0, ratio, 0, 0);
  ctx.clearRect(0, 0, width, height);
  const project = makeProjector(scene, width, height);
  const pos = /* @__PURE__ */ new Map();
  for (const n of scene.nodes) pos.set(n.id, project(n.x, n.y));
  const nodeRadius2 = /* @__PURE__ */ new Map();
  for (const n of scene.nodes) nodeRadius2.set(n.id, n.r);
  const ordered = [
    ...scene.edges.filter((e) => !isPendingReview(e)),
    ...scene.edges.filter(isPendingReview)
  ];
  for (const e of ordered) {
    const a2 = pos.get(e.from);
    const b = pos.get(e.to);
    if (!a2 || !b) continue;
    const pending = isPendingReview(e);
    const seg = edgeEndpoints(a2, b, nodeRadius2.get(e.from) ?? 0, nodeRadius2.get(e.to) ?? 0);
    ctx.save();
    ctx.globalAlpha = e.style.opacity;
    ctx.strokeStyle = pending ? STYLE.edgePossiblySameColor : e.style.invalid ? STYLE.edgeInvalidColor : STYLE.edgeColor;
    ctx.lineWidth = pending ? 2 : 1;
    if (e.style.dashed) ctx.setLineDash([4, 3]);
    ctx.beginPath();
    ctx.moveTo(seg.x1, seg.y1);
    ctx.lineTo(seg.x2, seg.y2);
    ctx.stroke();
    ctx.restore();
  }
  const clusterPos = /* @__PURE__ */ new Map();
  for (const c2 of scene.clusters) {
    clusterPos.set(c2.id || c2.concept_id || "", project(c2.x, c2.y));
  }
  for (const ce of scene.clusterEdges || []) {
    const a2 = clusterPos.get(ce.from);
    const b = clusterPos.get(ce.to);
    if (!a2 || !b) continue;
    ctx.save();
    ctx.strokeStyle = STYLE.clusterStroke;
    ctx.globalAlpha = ce.invalidated ? 0.25 : 0.55;
    ctx.lineWidth = Math.min(4, 1 + Math.log2(Math.max(1, ce.count)));
    if (ce.invalidated) ctx.setLineDash([4, 3]);
    ctx.beginPath();
    ctx.moveTo(a2.x, a2.y);
    ctx.lineTo(b.x, b.y);
    ctx.stroke();
    ctx.restore();
  }
  for (const c2 of scene.clusters) {
    const p = project(c2.x, c2.y);
    const r = 10 + Math.min(30, Math.sqrt(Math.max(1, c2.count)) * 3.2);
    ctx.save();
    ctx.beginPath();
    ctx.arc(p.x, p.y, r, 0, Math.PI * 2);
    ctx.fillStyle = STYLE.clusterFill;
    ctx.fill();
    ctx.strokeStyle = c2.matched ? STYLE.matchedStroke : STYLE.clusterStroke;
    ctx.lineWidth = c2.matched ? 2.5 : 1.5;
    ctx.stroke();
    ctx.fillStyle = STYLE.label;
    ctx.font = "11px system-ui, sans-serif";
    ctx.textAlign = "center";
    ctx.fillText(`${c2.concept_name || c2.id || "(未归属概念)"} · ${c2.count}`, p.x, p.y + r + 13);
    ctx.restore();
  }
  for (const n of scene.nodes) {
    const p = pos.get(n.id);
    if (!p) continue;
    ctx.save();
    ctx.beginPath();
    ctx.arc(p.x, p.y, n.r, 0, Math.PI * 2);
    ctx.fillStyle = conceptColor(n.key);
    ctx.fill();
    ctx.strokeStyle = n.matched ? STYLE.matchedStroke : "#ffffff";
    ctx.lineWidth = n.matched ? 2.5 : 1.2;
    ctx.stroke();
    if (n.draft) {
      ctx.beginPath();
      ctx.arc(p.x, p.y, Math.max(2, n.r - 2), 0, Math.PI * 2);
      ctx.setLineDash([2, 2]);
      ctx.strokeStyle = "#20262e";
      ctx.lineWidth = 1;
      ctx.stroke();
    }
    ctx.restore();
  }
  return { project, pos, clusterPos };
}
function hitTest(scene, project, x2, y2) {
  let best = null;
  let bestD = Infinity;
  for (const n of scene.nodes) {
    const p = project(n.x, n.y);
    const d = Math.hypot(p.x - x2, p.y - y2);
    const hit = Math.max(6, n.r) + 4;
    if (d <= hit && d < bestD) {
      best = n;
      bestD = d;
    }
  }
  return best;
}
function hitTestCluster(scene, project, x2, y2) {
  let best = null;
  let bestD = Infinity;
  for (const c2 of scene.clusters) {
    const p = project(c2.x, c2.y);
    const d = Math.hypot(p.x - x2, p.y - y2);
    const hit = 10 + Math.min(30, Math.sqrt(Math.max(1, c2.count)) * 3.2) + 4;
    if (d <= hit && d < bestD) {
      best = c2;
      bestD = d;
    }
  }
  return best;
}

// node_modules/d3-quadtree/dist/d3-quadtree.mjs
function tree_add(d) {
  const x2 = +this._x.call(null, d), y2 = +this._y.call(null, d);
  return add(this.cover(x2, y2), x2, y2, d);
}
function add(tree, x2, y2, d) {
  if (isNaN(x2) || isNaN(y2)) return tree;
  var parent, node = tree._root, leaf = { data: d }, x0 = tree._x0, y0 = tree._y0, x1 = tree._x1, y1 = tree._y1, xm, ym, xp, yp, right, bottom, i, j;
  if (!node) return tree._root = leaf, tree;
  while (node.length) {
    if (right = x2 >= (xm = (x0 + x1) / 2)) x0 = xm;
    else x1 = xm;
    if (bottom = y2 >= (ym = (y0 + y1) / 2)) y0 = ym;
    else y1 = ym;
    if (parent = node, !(node = node[i = bottom << 1 | right])) return parent[i] = leaf, tree;
  }
  xp = +tree._x.call(null, node.data);
  yp = +tree._y.call(null, node.data);
  if (x2 === xp && y2 === yp) return leaf.next = node, parent ? parent[i] = leaf : tree._root = leaf, tree;
  do {
    parent = parent ? parent[i] = new Array(4) : tree._root = new Array(4);
    if (right = x2 >= (xm = (x0 + x1) / 2)) x0 = xm;
    else x1 = xm;
    if (bottom = y2 >= (ym = (y0 + y1) / 2)) y0 = ym;
    else y1 = ym;
  } while ((i = bottom << 1 | right) === (j = (yp >= ym) << 1 | xp >= xm));
  return parent[j] = node, parent[i] = leaf, tree;
}
function addAll(data) {
  if (!Array.isArray(data)) data = Array.from(data);
  const n = data.length;
  const xz = new Float64Array(n);
  const yz = new Float64Array(n);
  let x0 = Infinity, y0 = x0, x1 = -x0, y1 = x1;
  for (let i = 0, d, x2, y2; i < n; ++i) {
    if (isNaN(x2 = +this._x.call(null, d = data[i])) || isNaN(y2 = +this._y.call(null, d))) {
      xz[i] = NaN;
      continue;
    }
    xz[i] = x2;
    yz[i] = y2;
    if (x2 < x0) x0 = x2;
    if (x2 > x1) x1 = x2;
    if (y2 < y0) y0 = y2;
    if (y2 > y1) y1 = y2;
  }
  if (x0 > x1 || y0 > y1) return this;
  this.cover(x0, y0).cover(x1, y1);
  for (let i = 0; i < n; ++i) {
    add(this, xz[i], yz[i], data[i]);
  }
  return this;
}
function tree_cover(x2, y2) {
  if (isNaN(x2 = +x2) || isNaN(y2 = +y2)) return this;
  var x0 = this._x0, y0 = this._y0, x1 = this._x1, y1 = this._y1;
  if (isNaN(x0)) {
    x1 = (x0 = Math.floor(x2)) + 1;
    y1 = (y0 = Math.floor(y2)) + 1;
  } else {
    var z = x1 - x0 || 1, node = this._root, parent, i;
    while (x0 > x2 || x2 >= x1 || y0 > y2 || y2 >= y1) {
      i = (y2 < y0) << 1 | x2 < x0;
      parent = new Array(4), parent[i] = node, node = parent, z *= 2;
      switch (i) {
        case 0:
          x1 = x0 + z, y1 = y0 + z;
          break;
        case 1:
          x0 = x1 - z, y1 = y0 + z;
          break;
        case 2:
          x1 = x0 + z, y0 = y1 - z;
          break;
        case 3:
          x0 = x1 - z, y0 = y1 - z;
          break;
      }
    }
    if (this._root && this._root.length) this._root = node;
  }
  this._x0 = x0;
  this._y0 = y0;
  this._x1 = x1;
  this._y1 = y1;
  return this;
}
function tree_data() {
  var data = [];
  this.visit(function(node) {
    if (!node.length) do
      data.push(node.data);
    while (node = node.next);
  });
  return data;
}
function tree_extent(_) {
  return arguments.length ? this.cover(+_[0][0], +_[0][1]).cover(+_[1][0], +_[1][1]) : isNaN(this._x0) ? void 0 : [[this._x0, this._y0], [this._x1, this._y1]];
}
function Quad(node, x0, y0, x1, y1) {
  this.node = node;
  this.x0 = x0;
  this.y0 = y0;
  this.x1 = x1;
  this.y1 = y1;
}
function tree_find(x2, y2, radius) {
  var data, x0 = this._x0, y0 = this._y0, x1, y1, x22, y22, x3 = this._x1, y3 = this._y1, quads = [], node = this._root, q, i;
  if (node) quads.push(new Quad(node, x0, y0, x3, y3));
  if (radius == null) radius = Infinity;
  else {
    x0 = x2 - radius, y0 = y2 - radius;
    x3 = x2 + radius, y3 = y2 + radius;
    radius *= radius;
  }
  while (q = quads.pop()) {
    if (!(node = q.node) || (x1 = q.x0) > x3 || (y1 = q.y0) > y3 || (x22 = q.x1) < x0 || (y22 = q.y1) < y0) continue;
    if (node.length) {
      var xm = (x1 + x22) / 2, ym = (y1 + y22) / 2;
      quads.push(
        new Quad(node[3], xm, ym, x22, y22),
        new Quad(node[2], x1, ym, xm, y22),
        new Quad(node[1], xm, y1, x22, ym),
        new Quad(node[0], x1, y1, xm, ym)
      );
      if (i = (y2 >= ym) << 1 | x2 >= xm) {
        q = quads[quads.length - 1];
        quads[quads.length - 1] = quads[quads.length - 1 - i];
        quads[quads.length - 1 - i] = q;
      }
    } else {
      var dx = x2 - +this._x.call(null, node.data), dy = y2 - +this._y.call(null, node.data), d2 = dx * dx + dy * dy;
      if (d2 < radius) {
        var d = Math.sqrt(radius = d2);
        x0 = x2 - d, y0 = y2 - d;
        x3 = x2 + d, y3 = y2 + d;
        data = node.data;
      }
    }
  }
  return data;
}
function tree_remove(d) {
  if (isNaN(x2 = +this._x.call(null, d)) || isNaN(y2 = +this._y.call(null, d))) return this;
  var parent, node = this._root, retainer, previous, next, x0 = this._x0, y0 = this._y0, x1 = this._x1, y1 = this._y1, x2, y2, xm, ym, right, bottom, i, j;
  if (!node) return this;
  if (node.length) while (true) {
    if (right = x2 >= (xm = (x0 + x1) / 2)) x0 = xm;
    else x1 = xm;
    if (bottom = y2 >= (ym = (y0 + y1) / 2)) y0 = ym;
    else y1 = ym;
    if (!(parent = node, node = node[i = bottom << 1 | right])) return this;
    if (!node.length) break;
    if (parent[i + 1 & 3] || parent[i + 2 & 3] || parent[i + 3 & 3]) retainer = parent, j = i;
  }
  while (node.data !== d) if (!(previous = node, node = node.next)) return this;
  if (next = node.next) delete node.next;
  if (previous) return next ? previous.next = next : delete previous.next, this;
  if (!parent) return this._root = next, this;
  next ? parent[i] = next : delete parent[i];
  if ((node = parent[0] || parent[1] || parent[2] || parent[3]) && node === (parent[3] || parent[2] || parent[1] || parent[0]) && !node.length) {
    if (retainer) retainer[j] = node;
    else this._root = node;
  }
  return this;
}
function removeAll(data) {
  for (var i = 0, n = data.length; i < n; ++i) this.remove(data[i]);
  return this;
}
function tree_root() {
  return this._root;
}
function tree_size() {
  var size = 0;
  this.visit(function(node) {
    if (!node.length) do
      ++size;
    while (node = node.next);
  });
  return size;
}
function tree_visit(callback) {
  var quads = [], q, node = this._root, child, x0, y0, x1, y1;
  if (node) quads.push(new Quad(node, this._x0, this._y0, this._x1, this._y1));
  while (q = quads.pop()) {
    if (!callback(node = q.node, x0 = q.x0, y0 = q.y0, x1 = q.x1, y1 = q.y1) && node.length) {
      var xm = (x0 + x1) / 2, ym = (y0 + y1) / 2;
      if (child = node[3]) quads.push(new Quad(child, xm, ym, x1, y1));
      if (child = node[2]) quads.push(new Quad(child, x0, ym, xm, y1));
      if (child = node[1]) quads.push(new Quad(child, xm, y0, x1, ym));
      if (child = node[0]) quads.push(new Quad(child, x0, y0, xm, ym));
    }
  }
  return this;
}
function tree_visitAfter(callback) {
  var quads = [], next = [], q;
  if (this._root) quads.push(new Quad(this._root, this._x0, this._y0, this._x1, this._y1));
  while (q = quads.pop()) {
    var node = q.node;
    if (node.length) {
      var child, x0 = q.x0, y0 = q.y0, x1 = q.x1, y1 = q.y1, xm = (x0 + x1) / 2, ym = (y0 + y1) / 2;
      if (child = node[0]) quads.push(new Quad(child, x0, y0, xm, ym));
      if (child = node[1]) quads.push(new Quad(child, xm, y0, x1, ym));
      if (child = node[2]) quads.push(new Quad(child, x0, ym, xm, y1));
      if (child = node[3]) quads.push(new Quad(child, xm, ym, x1, y1));
    }
    next.push(q);
  }
  while (q = next.pop()) {
    callback(q.node, q.x0, q.y0, q.x1, q.y1);
  }
  return this;
}
function defaultX(d) {
  return d[0];
}
function tree_x(_) {
  return arguments.length ? (this._x = _, this) : this._x;
}
function defaultY(d) {
  return d[1];
}
function tree_y(_) {
  return arguments.length ? (this._y = _, this) : this._y;
}
function quadtree(nodes, x2, y2) {
  var tree = new Quadtree(x2 == null ? defaultX : x2, y2 == null ? defaultY : y2, NaN, NaN, NaN, NaN);
  return nodes == null ? tree : tree.addAll(nodes);
}
function Quadtree(x2, y2, x0, y0, x1, y1) {
  this._x = x2;
  this._y = y2;
  this._x0 = x0;
  this._y0 = y0;
  this._x1 = x1;
  this._y1 = y1;
  this._root = void 0;
}
function leaf_copy(leaf) {
  var copy = { data: leaf.data }, next = copy;
  while (leaf = leaf.next) next = next.next = { data: leaf.data };
  return copy;
}
var treeProto = quadtree.prototype = Quadtree.prototype;
treeProto.copy = function() {
  var copy = new Quadtree(this._x, this._y, this._x0, this._y0, this._x1, this._y1), node = this._root, nodes, child;
  if (!node) return copy;
  if (!node.length) return copy._root = leaf_copy(node), copy;
  nodes = [{ source: node, target: copy._root = new Array(4) }];
  while (node = nodes.pop()) {
    for (var i = 0; i < 4; ++i) {
      if (child = node.source[i]) {
        if (child.length) nodes.push({ source: child, target: node.target[i] = new Array(4) });
        else node.target[i] = leaf_copy(child);
      }
    }
  }
  return copy;
};
treeProto.add = tree_add;
treeProto.addAll = addAll;
treeProto.cover = tree_cover;
treeProto.data = tree_data;
treeProto.extent = tree_extent;
treeProto.find = tree_find;
treeProto.remove = tree_remove;
treeProto.removeAll = removeAll;
treeProto.root = tree_root;
treeProto.size = tree_size;
treeProto.visit = tree_visit;
treeProto.visitAfter = tree_visitAfter;
treeProto.x = tree_x;
treeProto.y = tree_y;

// node_modules/d3-dispatch/dist/d3-dispatch.mjs
var noop = { value: () => {
} };
function dispatch() {
  for (var i = 0, n = arguments.length, _ = {}, t; i < n; ++i) {
    if (!(t = arguments[i] + "") || t in _ || /[\s.]/.test(t)) throw new Error("illegal type: " + t);
    _[t] = [];
  }
  return new Dispatch(_);
}
function Dispatch(_) {
  this._ = _;
}
function parseTypenames(typenames, types) {
  return typenames.trim().split(/^|\s+/).map(function(t) {
    var name = "", i = t.indexOf(".");
    if (i >= 0) name = t.slice(i + 1), t = t.slice(0, i);
    if (t && !types.hasOwnProperty(t)) throw new Error("unknown type: " + t);
    return { type: t, name };
  });
}
Dispatch.prototype = dispatch.prototype = {
  constructor: Dispatch,
  on: function(typename, callback) {
    var _ = this._, T = parseTypenames(typename + "", _), t, i = -1, n = T.length;
    if (arguments.length < 2) {
      while (++i < n) if ((t = (typename = T[i]).type) && (t = get(_[t], typename.name))) return t;
      return;
    }
    if (callback != null && typeof callback !== "function") throw new Error("invalid callback: " + callback);
    while (++i < n) {
      if (t = (typename = T[i]).type) _[t] = set(_[t], typename.name, callback);
      else if (callback == null) for (t in _) _[t] = set(_[t], typename.name, null);
    }
    return this;
  },
  copy: function() {
    var copy = {}, _ = this._;
    for (var t in _) copy[t] = _[t].slice();
    return new Dispatch(copy);
  },
  call: function(type, that) {
    if ((n = arguments.length - 2) > 0) for (var args = new Array(n), i = 0, n, t; i < n; ++i) args[i] = arguments[i + 2];
    if (!this._.hasOwnProperty(type)) throw new Error("unknown type: " + type);
    for (t = this._[type], i = 0, n = t.length; i < n; ++i) t[i].value.apply(that, args);
  },
  apply: function(type, that, args) {
    if (!this._.hasOwnProperty(type)) throw new Error("unknown type: " + type);
    for (var t = this._[type], i = 0, n = t.length; i < n; ++i) t[i].value.apply(that, args);
  }
};
function get(type, name) {
  for (var i = 0, n = type.length, c2; i < n; ++i) {
    if ((c2 = type[i]).name === name) {
      return c2.value;
    }
  }
}
function set(type, name, callback) {
  for (var i = 0, n = type.length; i < n; ++i) {
    if (type[i].name === name) {
      type[i] = noop, type = type.slice(0, i).concat(type.slice(i + 1));
      break;
    }
  }
  if (callback != null) type.push({ name, value: callback });
  return type;
}

// node_modules/d3-timer/dist/d3-timer.mjs
var frame = 0;
var timeout$1 = 0;
var interval$1 = 0;
var pokeDelay = 1e3;
var taskHead;
var taskTail;
var clockLast = 0;
var clockNow = 0;
var clockSkew = 0;
var clock = typeof performance === "object" && performance.now ? performance : Date;
var setFrame = typeof window === "object" && window.requestAnimationFrame ? window.requestAnimationFrame.bind(window) : function(f) {
  setTimeout(f, 17);
};
function now() {
  return clockNow || (setFrame(clearNow), clockNow = clock.now() + clockSkew);
}
function clearNow() {
  clockNow = 0;
}
function Timer() {
  this._call = this._time = this._next = null;
}
Timer.prototype = timer.prototype = {
  constructor: Timer,
  restart: function(callback, delay, time) {
    if (typeof callback !== "function") throw new TypeError("callback is not a function");
    time = (time == null ? now() : +time) + (delay == null ? 0 : +delay);
    if (!this._next && taskTail !== this) {
      if (taskTail) taskTail._next = this;
      else taskHead = this;
      taskTail = this;
    }
    this._call = callback;
    this._time = time;
    sleep();
  },
  stop: function() {
    if (this._call) {
      this._call = null;
      this._time = Infinity;
      sleep();
    }
  }
};
function timer(callback, delay, time) {
  var t = new Timer();
  t.restart(callback, delay, time);
  return t;
}
function timerFlush() {
  now();
  ++frame;
  var t = taskHead, e;
  while (t) {
    if ((e = clockNow - t._time) >= 0) t._call.call(void 0, e);
    t = t._next;
  }
  --frame;
}
function wake() {
  clockNow = (clockLast = clock.now()) + clockSkew;
  frame = timeout$1 = 0;
  try {
    timerFlush();
  } finally {
    frame = 0;
    nap();
    clockNow = 0;
  }
}
function poke() {
  var now2 = clock.now(), delay = now2 - clockLast;
  if (delay > pokeDelay) clockSkew -= delay, clockLast = now2;
}
function nap() {
  var t0, t1 = taskHead, t2, time = Infinity;
  while (t1) {
    if (t1._call) {
      if (time > t1._time) time = t1._time;
      t0 = t1, t1 = t1._next;
    } else {
      t2 = t1._next, t1._next = null;
      t1 = t0 ? t0._next = t2 : taskHead = t2;
    }
  }
  taskTail = t0;
  sleep(time);
}
function sleep(time) {
  if (frame) return;
  if (timeout$1) timeout$1 = clearTimeout(timeout$1);
  var delay = time - clockNow;
  if (delay > 24) {
    if (time < Infinity) timeout$1 = setTimeout(wake, Math.ceil(time - clock.now() - clockSkew) + 1);
    if (interval$1) interval$1 = clearInterval(interval$1);
  } else {
    if (!interval$1) clockLast = clock.now(), interval$1 = setInterval(poke, pokeDelay);
    frame = 1, setFrame(wake);
  }
}

// node_modules/d3-force/dist/d3-force.mjs
function constant(x2) {
  return function() {
    return x2;
  };
}
function jiggle(random) {
  return (random() - 0.5) * 1e-6;
}
function x$2(d) {
  return d.x + d.vx;
}
function y$2(d) {
  return d.y + d.vy;
}
function collide(radius) {
  var nodes, radii, random, strength = 1, iterations = 1;
  if (typeof radius !== "function") radius = constant(radius == null ? 1 : +radius);
  function force() {
    var i, n = nodes.length, tree, node, xi, yi, ri, ri2;
    for (var k = 0; k < iterations; ++k) {
      tree = quadtree(nodes, x$2, y$2).visitAfter(prepare);
      for (i = 0; i < n; ++i) {
        node = nodes[i];
        ri = radii[node.index], ri2 = ri * ri;
        xi = node.x + node.vx;
        yi = node.y + node.vy;
        tree.visit(apply);
      }
    }
    function apply(quad, x0, y0, x1, y1) {
      var data = quad.data, rj = quad.r, r = ri + rj;
      if (data) {
        if (data.index > node.index) {
          var x2 = xi - data.x - data.vx, y2 = yi - data.y - data.vy, l = x2 * x2 + y2 * y2;
          if (l < r * r) {
            if (x2 === 0) x2 = jiggle(random), l += x2 * x2;
            if (y2 === 0) y2 = jiggle(random), l += y2 * y2;
            l = (r - (l = Math.sqrt(l))) / l * strength;
            node.vx += (x2 *= l) * (r = (rj *= rj) / (ri2 + rj));
            node.vy += (y2 *= l) * r;
            data.vx -= x2 * (r = 1 - r);
            data.vy -= y2 * r;
          }
        }
        return;
      }
      return x0 > xi + r || x1 < xi - r || y0 > yi + r || y1 < yi - r;
    }
  }
  function prepare(quad) {
    if (quad.data) return quad.r = radii[quad.data.index];
    for (var i = quad.r = 0; i < 4; ++i) {
      if (quad[i] && quad[i].r > quad.r) {
        quad.r = quad[i].r;
      }
    }
  }
  function initialize() {
    if (!nodes) return;
    var i, n = nodes.length, node;
    radii = new Array(n);
    for (i = 0; i < n; ++i) node = nodes[i], radii[node.index] = +radius(node, i, nodes);
  }
  force.initialize = function(_nodes, _random) {
    nodes = _nodes;
    random = _random;
    initialize();
  };
  force.iterations = function(_) {
    return arguments.length ? (iterations = +_, force) : iterations;
  };
  force.strength = function(_) {
    return arguments.length ? (strength = +_, force) : strength;
  };
  force.radius = function(_) {
    return arguments.length ? (radius = typeof _ === "function" ? _ : constant(+_), initialize(), force) : radius;
  };
  return force;
}
function index(d) {
  return d.index;
}
function find(nodeById, nodeId) {
  var node = nodeById.get(nodeId);
  if (!node) throw new Error("node not found: " + nodeId);
  return node;
}
function link(links) {
  var id = index, strength = defaultStrength, strengths, distance = constant(30), distances, nodes, count, bias, random, iterations = 1;
  if (links == null) links = [];
  function defaultStrength(link2) {
    return 1 / Math.min(count[link2.source.index], count[link2.target.index]);
  }
  function force(alpha) {
    for (var k = 0, n = links.length; k < iterations; ++k) {
      for (var i = 0, link2, source, target, x2, y2, l, b; i < n; ++i) {
        link2 = links[i], source = link2.source, target = link2.target;
        x2 = target.x + target.vx - source.x - source.vx || jiggle(random);
        y2 = target.y + target.vy - source.y - source.vy || jiggle(random);
        l = Math.sqrt(x2 * x2 + y2 * y2);
        l = (l - distances[i]) / l * alpha * strengths[i];
        x2 *= l, y2 *= l;
        target.vx -= x2 * (b = bias[i]);
        target.vy -= y2 * b;
        source.vx += x2 * (b = 1 - b);
        source.vy += y2 * b;
      }
    }
  }
  function initialize() {
    if (!nodes) return;
    var i, n = nodes.length, m2 = links.length, nodeById = new Map(nodes.map((d, i2) => [id(d, i2, nodes), d])), link2;
    for (i = 0, count = new Array(n); i < m2; ++i) {
      link2 = links[i], link2.index = i;
      if (typeof link2.source !== "object") link2.source = find(nodeById, link2.source);
      if (typeof link2.target !== "object") link2.target = find(nodeById, link2.target);
      count[link2.source.index] = (count[link2.source.index] || 0) + 1;
      count[link2.target.index] = (count[link2.target.index] || 0) + 1;
    }
    for (i = 0, bias = new Array(m2); i < m2; ++i) {
      link2 = links[i], bias[i] = count[link2.source.index] / (count[link2.source.index] + count[link2.target.index]);
    }
    strengths = new Array(m2), initializeStrength();
    distances = new Array(m2), initializeDistance();
  }
  function initializeStrength() {
    if (!nodes) return;
    for (var i = 0, n = links.length; i < n; ++i) {
      strengths[i] = +strength(links[i], i, links);
    }
  }
  function initializeDistance() {
    if (!nodes) return;
    for (var i = 0, n = links.length; i < n; ++i) {
      distances[i] = +distance(links[i], i, links);
    }
  }
  force.initialize = function(_nodes, _random) {
    nodes = _nodes;
    random = _random;
    initialize();
  };
  force.links = function(_) {
    return arguments.length ? (links = _, initialize(), force) : links;
  };
  force.id = function(_) {
    return arguments.length ? (id = _, force) : id;
  };
  force.iterations = function(_) {
    return arguments.length ? (iterations = +_, force) : iterations;
  };
  force.strength = function(_) {
    return arguments.length ? (strength = typeof _ === "function" ? _ : constant(+_), initializeStrength(), force) : strength;
  };
  force.distance = function(_) {
    return arguments.length ? (distance = typeof _ === "function" ? _ : constant(+_), initializeDistance(), force) : distance;
  };
  return force;
}
var a = 1664525;
var c = 1013904223;
var m = 4294967296;
function lcg() {
  let s = 1;
  return () => (s = (a * s + c) % m) / m;
}
function x$1(d) {
  return d.x;
}
function y$1(d) {
  return d.y;
}
var initialRadius = 10;
var initialAngle = Math.PI * (3 - Math.sqrt(5));
function simulation(nodes) {
  var simulation2, alpha = 1, alphaMin = 1e-3, alphaDecay = 1 - Math.pow(alphaMin, 1 / 300), alphaTarget = 0, velocityDecay = 0.6, forces = /* @__PURE__ */ new Map(), stepper = timer(step), event = dispatch("tick", "end"), random = lcg();
  if (nodes == null) nodes = [];
  function step() {
    tick();
    event.call("tick", simulation2);
    if (alpha < alphaMin) {
      stepper.stop();
      event.call("end", simulation2);
    }
  }
  function tick(iterations) {
    var i, n = nodes.length, node;
    if (iterations === void 0) iterations = 1;
    for (var k = 0; k < iterations; ++k) {
      alpha += (alphaTarget - alpha) * alphaDecay;
      forces.forEach(function(force) {
        force(alpha);
      });
      for (i = 0; i < n; ++i) {
        node = nodes[i];
        if (node.fx == null) node.x += node.vx *= velocityDecay;
        else node.x = node.fx, node.vx = 0;
        if (node.fy == null) node.y += node.vy *= velocityDecay;
        else node.y = node.fy, node.vy = 0;
      }
    }
    return simulation2;
  }
  function initializeNodes() {
    for (var i = 0, n = nodes.length, node; i < n; ++i) {
      node = nodes[i], node.index = i;
      if (node.fx != null) node.x = node.fx;
      if (node.fy != null) node.y = node.fy;
      if (isNaN(node.x) || isNaN(node.y)) {
        var radius = initialRadius * Math.sqrt(0.5 + i), angle = i * initialAngle;
        node.x = radius * Math.cos(angle);
        node.y = radius * Math.sin(angle);
      }
      if (isNaN(node.vx) || isNaN(node.vy)) {
        node.vx = node.vy = 0;
      }
    }
  }
  function initializeForce(force) {
    if (force.initialize) force.initialize(nodes, random);
    return force;
  }
  initializeNodes();
  return simulation2 = {
    tick,
    restart: function() {
      return stepper.restart(step), simulation2;
    },
    stop: function() {
      return stepper.stop(), simulation2;
    },
    nodes: function(_) {
      return arguments.length ? (nodes = _, initializeNodes(), forces.forEach(initializeForce), simulation2) : nodes;
    },
    alpha: function(_) {
      return arguments.length ? (alpha = +_, simulation2) : alpha;
    },
    alphaMin: function(_) {
      return arguments.length ? (alphaMin = +_, simulation2) : alphaMin;
    },
    alphaDecay: function(_) {
      return arguments.length ? (alphaDecay = +_, simulation2) : +alphaDecay;
    },
    alphaTarget: function(_) {
      return arguments.length ? (alphaTarget = +_, simulation2) : alphaTarget;
    },
    velocityDecay: function(_) {
      return arguments.length ? (velocityDecay = 1 - _, simulation2) : 1 - velocityDecay;
    },
    randomSource: function(_) {
      return arguments.length ? (random = _, forces.forEach(initializeForce), simulation2) : random;
    },
    force: function(name, _) {
      return arguments.length > 1 ? (_ == null ? forces.delete(name) : forces.set(name, initializeForce(_)), simulation2) : forces.get(name);
    },
    find: function(x2, y2, radius) {
      var i = 0, n = nodes.length, dx, dy, d2, node, closest;
      if (radius == null) radius = Infinity;
      else radius *= radius;
      for (i = 0; i < n; ++i) {
        node = nodes[i];
        dx = x2 - node.x;
        dy = y2 - node.y;
        d2 = dx * dx + dy * dy;
        if (d2 < radius) closest = node, radius = d2;
      }
      return closest;
    },
    on: function(name, _) {
      return arguments.length > 1 ? (event.on(name, _), simulation2) : event.on(name);
    }
  };
}
function manyBody() {
  var nodes, node, random, alpha, strength = constant(-30), strengths, distanceMin2 = 1, distanceMax2 = Infinity, theta2 = 0.81;
  function force(_) {
    var i, n = nodes.length, tree = quadtree(nodes, x$1, y$1).visitAfter(accumulate);
    for (alpha = _, i = 0; i < n; ++i) node = nodes[i], tree.visit(apply);
  }
  function initialize() {
    if (!nodes) return;
    var i, n = nodes.length, node2;
    strengths = new Array(n);
    for (i = 0; i < n; ++i) node2 = nodes[i], strengths[node2.index] = +strength(node2, i, nodes);
  }
  function accumulate(quad) {
    var strength2 = 0, q, c2, weight = 0, x2, y2, i;
    if (quad.length) {
      for (x2 = y2 = i = 0; i < 4; ++i) {
        if ((q = quad[i]) && (c2 = Math.abs(q.value))) {
          strength2 += q.value, weight += c2, x2 += c2 * q.x, y2 += c2 * q.y;
        }
      }
      quad.x = x2 / weight;
      quad.y = y2 / weight;
    } else {
      q = quad;
      q.x = q.data.x;
      q.y = q.data.y;
      do
        strength2 += strengths[q.data.index];
      while (q = q.next);
    }
    quad.value = strength2;
  }
  function apply(quad, x1, _, x2) {
    if (!quad.value) return true;
    var x3 = quad.x - node.x, y2 = quad.y - node.y, w = x2 - x1, l = x3 * x3 + y2 * y2;
    if (w * w / theta2 < l) {
      if (l < distanceMax2) {
        if (x3 === 0) x3 = jiggle(random), l += x3 * x3;
        if (y2 === 0) y2 = jiggle(random), l += y2 * y2;
        if (l < distanceMin2) l = Math.sqrt(distanceMin2 * l);
        node.vx += x3 * quad.value * alpha / l;
        node.vy += y2 * quad.value * alpha / l;
      }
      return true;
    } else if (quad.length || l >= distanceMax2) return;
    if (quad.data !== node || quad.next) {
      if (x3 === 0) x3 = jiggle(random), l += x3 * x3;
      if (y2 === 0) y2 = jiggle(random), l += y2 * y2;
      if (l < distanceMin2) l = Math.sqrt(distanceMin2 * l);
    }
    do
      if (quad.data !== node) {
        w = strengths[quad.data.index] * alpha / l;
        node.vx += x3 * w;
        node.vy += y2 * w;
      }
    while (quad = quad.next);
  }
  force.initialize = function(_nodes, _random) {
    nodes = _nodes;
    random = _random;
    initialize();
  };
  force.strength = function(_) {
    return arguments.length ? (strength = typeof _ === "function" ? _ : constant(+_), initialize(), force) : strength;
  };
  force.distanceMin = function(_) {
    return arguments.length ? (distanceMin2 = _ * _, force) : Math.sqrt(distanceMin2);
  };
  force.distanceMax = function(_) {
    return arguments.length ? (distanceMax2 = _ * _, force) : Math.sqrt(distanceMax2);
  };
  force.theta = function(_) {
    return arguments.length ? (theta2 = _ * _, force) : Math.sqrt(theta2);
  };
  return force;
}
function x(x2) {
  var strength = constant(0.1), nodes, strengths, xz;
  if (typeof x2 !== "function") x2 = constant(x2 == null ? 0 : +x2);
  function force(alpha) {
    for (var i = 0, n = nodes.length, node; i < n; ++i) {
      node = nodes[i], node.vx += (xz[i] - node.x) * strengths[i] * alpha;
    }
  }
  function initialize() {
    if (!nodes) return;
    var i, n = nodes.length;
    strengths = new Array(n);
    xz = new Array(n);
    for (i = 0; i < n; ++i) {
      strengths[i] = isNaN(xz[i] = +x2(nodes[i], i, nodes)) ? 0 : +strength(nodes[i], i, nodes);
    }
  }
  force.initialize = function(_) {
    nodes = _;
    initialize();
  };
  force.strength = function(_) {
    return arguments.length ? (strength = typeof _ === "function" ? _ : constant(+_), initialize(), force) : strength;
  };
  force.x = function(_) {
    return arguments.length ? (x2 = typeof _ === "function" ? _ : constant(+_), initialize(), force) : x2;
  };
  return force;
}
function y(y2) {
  var strength = constant(0.1), nodes, strengths, yz;
  if (typeof y2 !== "function") y2 = constant(y2 == null ? 0 : +y2);
  function force(alpha) {
    for (var i = 0, n = nodes.length, node; i < n; ++i) {
      node = nodes[i], node.vy += (yz[i] - node.y) * strengths[i] * alpha;
    }
  }
  function initialize() {
    if (!nodes) return;
    var i, n = nodes.length;
    strengths = new Array(n);
    yz = new Array(n);
    for (i = 0; i < n; ++i) {
      strengths[i] = isNaN(yz[i] = +y2(nodes[i], i, nodes)) ? 0 : +strength(nodes[i], i, nodes);
    }
  }
  force.initialize = function(_) {
    nodes = _;
    initialize();
  };
  force.strength = function(_) {
    return arguments.length ? (strength = typeof _ === "function" ? _ : constant(+_), initialize(), force) : strength;
  };
  force.y = function(_) {
    return arguments.length ? (y2 = typeof _ === "function" ? _ : constant(+_), initialize(), force) : y2;
  };
  return force;
}

// js/layout.js
function relayout(nodes, edges, { ticks = 220 } = {}) {
  const simNodes = nodes.map((n) => ({ id: n.id, x: n.x, y: n.y }));
  const byId = new Map(simNodes.map((n) => [n.id, n]));
  const links = edges.filter((e) => byId.has(e.from) && byId.has(e.to)).map((e) => ({ source: e.from, target: e.to }));
  const sim = simulation(simNodes).force("link", link(links).id((d) => d.id).distance(0.24).strength(0.35)).force("charge", manyBody().strength(-0.09).distanceMax(1.6)).force("collide", collide().radius(0.045)).force("x", x((d) => anchorOf(nodes, d.id).x).strength(0.06)).force("y", y((d) => anchorOf(nodes, d.id).y).strength(0.06)).stop();
  for (let i = 0; i < ticks; i++) sim.tick();
  const out = /* @__PURE__ */ new Map();
  for (const n of simNodes) {
    out.set(n.id, {
      x: round(n.x),
      y: round(n.y)
    });
  }
  return out;
}
function anchorOf(nodes, id) {
  const n = nodes.find((x2) => x2.id === id);
  return { x: n ? n.x : 0, y: n ? n.y : 0 };
}
function round(v) {
  return Math.round(v * 1e6) / 1e6;
}
function withLayout(scene, coords) {
  return {
    ...scene,
    layoutVersion: (scene.layoutVersion || 0) + 1,
    nodes: scene.nodes.map((n) => {
      const c2 = coords.get(n.id);
      return c2 ? { ...n, x: c2.x, y: c2.y } : n;
    })
  };
}

// js/reviewflow.js
var VERDICTS = Object.freeze(["same", "diff"]);
async function requestReview({ edgeId, verdict, confirm, send }) {
  if (!VERDICTS.includes(verdict)) {
    return { sent: false, reason: "cancelled" };
  }
  if (typeof confirm !== "function" || typeof send !== "function") {
    throw new TypeError("requestReview 需要 confirm 与 send 两个回调");
  }
  let ok = false;
  try {
    ok = await confirm({ edgeId, verdict });
  } catch {
    ok = false;
  }
  if (ok !== true) {
    return { sent: false, reason: "not_confirmed" };
  }
  const result = await send({ edgeId, verdict, confirmed: true });
  return { sent: true, reason: "sent", result };
}
function reviewPrompt(label, verdict) {
  if (verdict === "same") {
    return {
      title: "确认合并（同一实体）",
      text: `将把「${label}」合并为一个实体（Merge，可回溯重定向）。误合并是记忆系统最难恢复的错误，请确认。`
    };
  }
  return {
    title: "确认判为不同实体",
    text: `将把「${label}」记为 different（写 negative_SameAs），此后不再进入待审灰区。请确认。`
  };
}

// js/ui/dom.js
function h(tag, props = {}, children = []) {
  const el2 = document.createElement(tag);
  for (const [k, v] of Object.entries(props)) {
    if (v === null || v === void 0 || v === false) continue;
    if (k === "className") {
      el2.className = v;
    } else if (k === "textContent") {
      el2.textContent = String(v);
    } else if (k === "dataset") {
      for (const [dk, dv] of Object.entries(v)) el2.dataset[dk] = String(dv);
    } else if (k.startsWith("on") && typeof v === "function") {
      el2[k] = v;
    } else if (k === "hidden") {
      el2.hidden = Boolean(v);
    } else if (k === "value") {
      el2.value = String(v);
    } else {
      el2.setAttribute(k, String(v));
    }
  }
  append(el2, children);
  return el2;
}
function append(el2, children) {
  const list = Array.isArray(children) ? children : [children];
  for (const c2 of list) {
    if (c2 === null || c2 === void 0 || c2 === false) continue;
    if (Array.isArray(c2)) {
      append(el2, c2);
    } else if (typeof c2 === "string" || typeof c2 === "number") {
      el2.appendChild(document.createTextNode(String(c2)));
    } else {
      el2.appendChild(c2);
    }
  }
  return el2;
}
function fmt(n, digits = 3) {
  const v = Number(n);
  if (!Number.isFinite(v)) return "—";
  return v.toFixed(digits);
}
function fmtUnix(sec) {
  const v = Number(sec);
  if (!Number.isFinite(v)) return String(sec ?? "—");
  const iso = new Date(v * 1e3).toISOString();
  return iso.replace("T", " ").replace(".000Z", " UTC");
}

// js/ui/banner.js
var BANNER_KINDS = Object.freeze(["error", "warn", "info", "ok"]);
function banner(kind, text, { role = "status" } = {}) {
  const k = BANNER_KINDS.includes(kind) ? kind : "info";
  return h("div", { className: `banner ${k}`, role, textContent: String(text ?? "") });
}
function bannerHTML(kind, text) {
  return banner(kind, text).outerHTML;
}
function flash(container2, kind, text, { ms = 8e3 } = {}) {
  if (!container2) return null;
  const el2 = banner(kind, text);
  container2.appendChild(el2);
  if (typeof setTimeout === "function" && ms > 0) {
    setTimeout(() => el2.remove && el2.remove(), ms);
  }
  return el2;
}

// js/main.js
var MAX_SVG = 2e3;
var dom = {
  svg: document.getElementById("graph-svg"),
  canvas: document.getElementById("graph-canvas"),
  stage: document.getElementById("stage"),
  tooltip: document.getElementById("tooltip"),
  stageNote: document.getElementById("stage-note"),
  banners: document.getElementById("banners"),
  qInput: document.getElementById("q-input"),
  hopsInput: document.getElementById("hops-input"),
  budgetInput: document.getElementById("budget-input"),
  searchForm: document.getElementById("search-form"),
  overviewBtn: document.getElementById("overview-btn"),
  relayoutBtn: document.getElementById("relayout-btn"),
  copyBtn: document.getElementById("copy-btn"),
  modeBadge: document.getElementById("mode-badge"),
  conceptFilters: document.getElementById("concept-filters"),
  relationFilters: document.getElementById("relation-filters"),
  entityCard: document.getElementById("entity-card"),
  asOfRange: document.getElementById("asof-range"),
  asOfLabel: document.getElementById("asof-label"),
  asOfReset: document.getElementById("asof-reset"),
  confirmModal: document.getElementById("confirm-modal"),
  confirmTitle: document.getElementById("confirm-title"),
  confirmText: document.getElementById("confirm-text"),
  confirmOk: document.getElementById("confirm-ok"),
  confirmCancel: document.getElementById("confirm-cancel")
};
var state = {
  view: { focus: "", q: "", scope: "", hops: 1, budget: 200, asOf: null },
  raw: null,
  // 服务端原始响应
  scene: null,
  // 渲染用场景（过滤后）
  baseScene: null,
  // 未过滤场景（过滤选项的来源）
  conceptFilter: /* @__PURE__ */ new Set(),
  relationFilter: /* @__PURE__ */ new Set(),
  selectedId: "",
  project: null,
  tooltipEdge: "",
  errors: [],
  /** @type {Set<string>} 当前视图中待审的 possiblySame 边（裁决后移除） */
  reviewed: /* @__PURE__ */ new Set()
};
function currentFetchImpl() {
  return globalThis.fetch.bind(globalThis);
}
function syncURL(view, { replace = false } = {}) {
  const qs = toQuery(view);
  const url = `${location.pathname}${qs}`;
  if (replace) history.replaceState({ view }, "", url);
  else history.pushState({ view }, "", url);
}
async function loadView(view) {
  state.errors = [];
  syncURL(view, { replace: false });
  const { view: parsed, errors } = parseView(toQuery(view));
  if (errors.length) {
    state.errors = errors;
    state.scene = null;
    state.baseScene = null;
    renderBanners();
    renderStageNote("参数错误：" + errors.map((e) => e.message).join("；"));
    dom.entityCard.textContent = "参数有误，未发起请求。";
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
    state.errors = [{ field: err?.field || "", message: msg }];
    renderBanners();
    renderStageNote(`加载失败：${msg}`);
    return;
  }
  state.raw = resp;
  state.baseScene = buildScene(resp, { asOf: parsed.asOf });
  renderBanners();
  renderFilterPanel();
  renderTimeline();
  applyFiltersAndDraw();
}
function applyFiltersAndDraw() {
  if (!state.baseScene) return;
  const filtered = filterScene(state.baseScene, {
    concepts: state.conceptFilter,
    relations: state.relationFilter
  });
  state.scene = filtered;
  draw();
}
function stageSize() {
  const rect = dom.stage.getBoundingClientRect();
  return {
    width: Math.max(320, Math.floor(rect.width)),
    height: Math.max(240, Math.floor(rect.height)),
    devicePixelRatio: globalThis.devicePixelRatio || 1
  };
}
function draw() {
  if (!state.scene) return;
  const useCanvas = shouldUseCanvas(state.scene, MAX_SVG);
  const host = { svg: dom.svg, canvas: dom.canvas };
  let size = stageSize();
  let result = null;
  for (let i = 0; i < 3; i++) {
    result = render(host, state.scene, { ...size, useCanvas });
    const again = stageSize();
    if (again.width === size.width && again.height === size.height) break;
    size = again;
  }
  state.project = result.project;
  dom.stage.dataset.renderer = useCanvas ? "canvas" : "svg";
  dom.stage.dataset.nodes = String(state.scene.nodes.length);
  dom.stage.dataset.clusters = String(state.scene.clusters.length);
  bindInteractions(useCanvas);
  renderStageNote(buildStageNote());
  hideTooltip();
}
function buildStageNote() {
  const meta = state.scene.meta || {};
  const rendered = state.scene.nodes.length;
  const clusters = state.scene.clusters.length;
  if (!meta.truncated && !clusters) return "";
  const shown = `已折叠，未显示全部：node_count=${meta.node_count ?? "?"}，rendered_nodes=${meta.rendered_nodes ?? rendered}，clusters=${clusters}（团内共 ${meta.node_count ?? "?"} 个实体）`;
  return meta.truncated ? shown : `渲染 ${rendered} 个节点；按 concept 聚成 ${clusters} 个团（未截断）`;
}
function renderStageNote(text) {
  if (!text) {
    dom.stageNote.hidden = true;
    dom.stageNote.textContent = "";
    return;
  }
  dom.stageNote.hidden = false;
  dom.stageNote.textContent = text;
}
function renderBanners() {
  const parts = [];
  const meta = state.scene?.meta || state.raw?.meta || {};
  for (const e of state.errors) {
    parts.push(bannerHTML("error", `参数/请求错误${e.field ? `（${e.field}）` : ""}：${e.message}`));
  }
  if (Array.isArray(meta.degraded) && meta.degraded.length) {
    parts.push(bannerHTML("warn", `降级（meta.degraded，共 ${meta.degraded.length} 条）：${meta.degraded.join("；")}`));
  }
  if (meta.recall && Array.isArray(meta.recall.degraded) && meta.recall.degraded.length) {
    parts.push(bannerHTML("warn", `召回降级（recall.degraded）：${meta.recall.degraded.join("；")}`));
  }
  if (meta.recall && meta.recall.status && meta.recall.status !== "ok") {
    parts.push(bannerHTML("info", `召回状态：${meta.recall.status}（coverage=${meta.recall.coverage ?? "?"}，items=${meta.recall.items ?? "?"}）`));
  }
  if (meta.truncated) {
    parts.push(bannerHTML(
      "info",
      `已折叠，未显示全部：node_count=${meta.node_count}，rendered_nodes=${meta.rendered_nodes}，clusters=${meta.rendered_clusters ?? state.scene?.clusters.length ?? 0}，budget=${meta.budget}/${meta.budget_max}`
    ));
  }
  const reviewed = state.reviewed.size ? bannerHTML("ok", `本次已裁决 ${state.reviewed.size} 条 possiblySame 边（服务端为准，刷新后可见结果）`) : "";
  if (reviewed) parts.push(reviewed);
  dom.banners.innerHTML = parts.join("");
  dom.modeBadge.textContent = meta.mode ? `${meta.mode} · hops=${meta.hops ?? "-"} · budget=${meta.budget ?? "-"}` : "-";
}
function renderFilterPanel() {
  const scene = state.baseScene;
  if (!scene) return;
  const prevConceptKeys = new Set([...dom.conceptFilters.querySelectorAll("input")].map((i) => i.value));
  dom.conceptFilters.textContent = "";
  const options = conceptOptions(scene);
  for (const o of options) {
    const id = `cf-${o.key || "unclustered"}`;
    const label = document.createElement("label");
    label.className = "check";
    const cb = document.createElement("input");
    cb.type = "checkbox";
    cb.value = o.key;
    cb.id = id;
    cb.checked = state.conceptFilter.size ? state.conceptFilter.has(o.key) : false;
    cb.dataset.role = "concept";
    const sw = document.createElement("span");
    sw.className = "sw";
    sw.style.background = o.draft ? "repeating-linear-gradient(45deg,#20262e 0 2px,transparent 2px 5px)" : conceptColor(o.key);
    const txt = document.createElement("span");
    txt.textContent = `${o.label || "(未归属概念)"} (${o.count})`;
    label.append(cb, sw, txt);
    dom.conceptFilters.appendChild(label);
    if (prevConceptKeys.has(o.key) && state.conceptFilter.has(o.key)) cb.checked = true;
  }
  const prevRels = new Set([...dom.relationFilters.querySelectorAll("input")].map((i) => i.value));
  dom.relationFilters.textContent = "";
  for (const r of relationTypes(scene)) {
    const label = document.createElement("label");
    label.className = "check";
    const cb = document.createElement("input");
    cb.type = "checkbox";
    cb.value = r.relation;
    cb.dataset.role = "relation";
    cb.checked = state.relationFilter.size ? state.relationFilter.has(r.relation) : false;
    if (prevRels.has(r.relation) && state.relationFilter.has(r.relation)) cb.checked = true;
    const txt = document.createElement("span");
    txt.textContent = `${r.relation} (${r.count})`;
    label.append(cb, txt);
    dom.relationFilters.appendChild(label);
  }
}
function renderTimeline() {
  const range = state.scene ? sceneTimeRange(state.scene) : null;
  if (!range) {
    dom.asOfRange.disabled = true;
    dom.asOfLabel.textContent = "当前";
    return;
  }
  dom.asOfRange.disabled = false;
  dom.asOfRange.min = String(range.min);
  dom.asOfRange.max = String(range.max);
  const cur = state.view.asOf ?? range.max;
  dom.asOfRange.value = String(cur);
  dom.asOfLabel.textContent = state.view.asOf === null ? "当前" : new Date(Number(state.view.asOf) * 1e3).toISOString();
}
function bindInteractions(useCanvas) {
  const svgNodes = dom.svg.querySelectorAll("g.node");
  if (!useCanvas) {
    for (const g of svgNodes) {
      const id = g.getAttribute("data-node-id");
      g.addEventListener("click", (ev) => {
        ev.stopPropagation();
        selectNode(id);
      });
      g.addEventListener("dblclick", (ev) => {
        ev.stopPropagation();
        focusNode(id);
      });
      g.addEventListener("mouseenter", (ev) => showNodeTooltip(id, ev));
      g.addEventListener("mouseleave", hideTooltip);
    }
    for (const line of dom.svg.querySelectorAll("line.edge.possibly-same")) {
      const edgeId = line.getAttribute("data-edge-id");
      line.addEventListener("click", (ev) => {
        ev.stopPropagation();
        openReview(edgeId);
      });
      line.addEventListener("mouseenter", (ev) => showEdgeTooltip(edgeId, ev));
      line.addEventListener("mouseleave", hideTooltip);
    }
    for (const line of dom.svg.querySelectorAll("line.edge:not(.possibly-same)")) {
      const edgeId = line.getAttribute("data-edge-id");
      line.addEventListener("mouseenter", (ev) => showEdgeTooltip(edgeId, ev));
      line.addEventListener("mouseleave", hideTooltip);
    }
    for (const g of dom.svg.querySelectorAll("g.cluster.drillable")) {
      const conceptId = g.getAttribute("data-cluster-id");
      g.addEventListener("click", (ev) => {
        ev.stopPropagation();
        drillIntoCluster(conceptId);
      });
    }
  } else {
    const canvas = dom.canvas;
    let lastClickAt = 0;
    let lastClickId = "";
    canvas.onclick = (ev) => {
      const { x: x2, y: y2 } = localPoint(ev);
      const node = hitTest(state.scene, state.project, x2, y2);
      if (node) {
        const now2 = Date.now();
        if (lastClickId === node.id && now2 - lastClickAt < 350) {
          focusNode(node.id);
        } else {
          selectNode(node.id);
        }
        lastClickId = node.id;
        lastClickAt = now2;
        return;
      }
      const cluster = state.scene.meta?.truncated ? hitTestCluster(state.scene, state.project, x2, y2) : null;
      if (cluster) drillIntoCluster(cluster.concept_id || "");
    };
    canvas.onmousemove = (ev) => {
      const { x: x2, y: y2 } = localPoint(ev);
      const node = hitTest(state.scene, state.project, x2, y2);
      if (node) {
        showNodeTooltip(node.id, ev);
        return;
      }
      const edge = nearestEdge(x2, y2);
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
function nearestEdge(x2, y2) {
  let best = null;
  let bestD = 6;
  for (const e of state.scene.edges) {
    const a2 = state.scene.nodes.find((n) => n.id === e.from);
    const b = state.scene.nodes.find((n) => n.id === e.to);
    if (!a2 || !b) continue;
    const pa = state.project(a2.x, a2.y);
    const pb = state.project(b.x, b.y);
    const d = pointSegmentDistance(x2, y2, pa.x, pa.y, pb.x, pb.y);
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
async function selectNode(id) {
  state.selectedId = id;
  dom.entityCard.textContent = `加载实体卡 ${id}…`;
  try {
    const card = await fetchEntity(currentFetchImpl(), toEntityURL(id, state.view));
    dom.entityCard.textContent = "";
    dom.entityCard.appendChild(renderEntityCard(card));
  } catch (err) {
    dom.entityCard.textContent = `实体卡加载失败：${err instanceof ApiError ? err.message : err}`;
  }
}
function renderEntityCard(card) {
  const frag = document.createDocumentFragment();
  const h2 = document.createElement("h3");
  h2.textContent = card.canonical_name || card.id;
  frag.appendChild(h2);
  const recallName = card.canonical_name || card.id;
  if (recallName) {
    const link2 = document.createElement("p");
    link2.className = "hint";
    const a2 = document.createElement("a");
    a2.href = `/recall?q=${encodeURIComponent(recallName)}`;
    a2.textContent = `在 Recall Inspector 里查看「${recallName}」→`;
    link2.appendChild(a2);
    frag.appendChild(link2);
  }
  const meta = document.createElement("p");
  meta.className = "hint";
  meta.textContent = `id=${card.id} · scope=${card.scope}` + (card.concept ? ` · concept=${card.concept.name || card.concept.id}（${card.concept.status}）` : " · 无概念");
  frag.appendChild(meta);
  if (Array.isArray(card.meta?.degraded) && card.meta.degraded.length) {
    const d = document.createElement("p");
    d.className = "banner warn";
    d.textContent = `降级：${card.meta.degraded.join("；")}`;
    frag.appendChild(d);
  }
  if (Array.isArray(card.why) && card.why.length) {
    const hh = document.createElement("h4");
    hh.textContent = "为何生效 / 替换了什么";
    frag.appendChild(hh);
    const ul = document.createElement("ul");
    for (const w of card.why) {
      const li = document.createElement("li");
      li.textContent = w;
      ul.appendChild(li);
    }
    frag.appendChild(ul);
  }
  if (card.source) {
    const hh = document.createElement("h4");
    hh.textContent = "来源 episode";
    frag.appendChild(hh);
    const p = document.createElement("p");
    p.className = "episode";
    p.textContent = `${card.source.id} @ ${new Date(Number(card.source.at) * 1e3).toISOString()}` + (card.source.content ? `：${card.source.content}` : "");
    frag.appendChild(p);
  }
  if (Array.isArray(card.relations) && card.relations.length) {
    const hh = document.createElement("h4");
    hh.textContent = "关系";
    frag.appendChild(hh);
    for (const g of card.relations) {
      const box = document.createElement("div");
      box.className = "rel-group";
      const title = document.createElement("div");
      title.className = "rel-title";
      title.textContent = g.relation;
      box.appendChild(title);
      if (g.effective) box.appendChild(edgeRow(g.effective, "生效"));
      for (const r of g.replaced || []) box.appendChild(edgeRow(r, "已替换"));
      if (g.reason) {
        const p = document.createElement("p");
        p.className = "hint";
        p.textContent = g.reason;
        box.appendChild(p);
      }
      frag.appendChild(box);
    }
  }
  return frag;
}
function edgeRow(e, tag) {
  const row = document.createElement("div");
  row.className = "rel-row";
  const arrow = e.direction === "in" ? "←" : "→";
  row.textContent = `${tag} ${arrow} ${e.counterpart_name || e.counterpart}（${e.relation}，t_valid=${e.t_valid}${e.t_invalid ? `，t_invalid=${e.t_invalid}` : ""}${e.invalidated ? "，本次视角已失效" : ""}）`;
  return row;
}
function focusNode(id) {
  if (!id) return;
  loadView(focusView(state.view, id));
}
dom.searchForm.addEventListener("submit", (ev) => {
  ev.preventDefault();
  loadView(searchView(state.view, dom.qInput.value));
});
dom.overviewBtn.addEventListener("click", () => {
  dom.qInput.value = "";
  loadView(overviewView(state.view));
});
dom.hopsInput.addEventListener("change", () => {
  const n = Number(dom.hopsInput.value);
  if (!Number.isFinite(n) || n < 1) return;
  loadView({ ...state.view, hops: Math.floor(n) });
});
dom.budgetInput.addEventListener("change", () => {
  const n = Number(dom.budgetInput.value);
  if (!Number.isFinite(n) || n < 1) return;
  loadView({ ...state.view, budget: n > BUDGET_MAX ? n : Math.floor(n) });
});
dom.conceptFilters.addEventListener("change", (ev) => {
  if (ev.target?.dataset?.role !== "concept") return;
  toggleFilter(state.conceptFilter, ev.target.value, ev.target.checked);
  renderBanners();
  applyFiltersAndDraw();
  renderStageNote(buildStageNote());
});
dom.relationFilters.addEventListener("change", (ev) => {
  if (ev.target?.dataset?.role !== "relation") return;
  toggleFilter(state.relationFilter, ev.target.value, ev.target.checked);
  renderBanners();
  applyFiltersAndDraw();
  renderStageNote(buildStageNote());
});
function toggleFilter(set2, value, checked) {
  if (checked) set2.add(value);
  else set2.delete(value);
}
dom.asOfRange.addEventListener("input", () => {
  if (!state.baseScene) return;
  const n = Number(dom.asOfRange.value);
  state.view = { ...state.view, asOf: n };
  state.baseScene = applyAsOf({ ...state.baseScene, layoutVersion: state.baseScene.layoutVersion }, n);
  state.scene = filterScene(state.baseScene, {
    concepts: state.conceptFilter,
    relations: state.relationFilter
  });
  dom.asOfLabel.textContent = new Date(n * 1e3).toISOString();
  draw();
  syncURL(state.view, { replace: true });
});
dom.asOfReset.addEventListener("click", () => {
  state.view = { ...state.view, asOf: null };
  syncURL(state.view, { replace: true });
  loadView(state.view);
});
dom.relayoutBtn.addEventListener("click", () => {
  if (!state.scene) return;
  const coords = relayout(state.scene.nodes, state.scene.edges);
  state.scene = withLayout(state.scene, coords);
  state.baseScene = withLayout(state.baseScene, coords);
  draw();
  renderStageNote(`${buildStageNote()}（本地重排：坐标仅在本页生效，不写服务端）`.trim());
});
dom.copyBtn.addEventListener("click", async () => {
  const link2 = `${location.origin}${location.pathname}${toQuery(state.view)}`;
  try {
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(link2);
    } else {
      throw new Error("clipboard 不可用");
    }
    flashBanner("ok", `已复制当前视图链接：${link2}`);
  } catch {
    flashBanner("warn", `无法写入剪贴板，请手动复制：${link2}`);
  }
});
function flashBanner(kind, text) {
  flash(dom.banners, kind, text);
}
function drillIntoCluster(conceptId) {
  const candidate2 = state.baseScene?.nodes.find((n) => (n.concept_id || "") === (conceptId || ""));
  if (candidate2) {
    focusNode(candidate2.id);
    return;
  }
  flashBanner("info", `概念 ${conceptId || "(未归属概念)"} 在本次折叠里没有保留实体（折叠时只保留命中实体）；可提高 budget 或先搜索该概念下的实体。`);
}
function openReview(edgeId) {
  const edge = state.scene?.edges.find((e) => e.id === edgeId);
  if (!edge) return;
  if (!isPendingReview(edge)) return;
  const from = state.scene.nodes.find((n) => n.id === edge.from);
  const to = state.scene.nodes.find((n) => n.id === edge.to);
  const label = `${from?.canonical_name || edge.from} ⇄ ${to?.canonical_name || edge.to}`;
  chooseVerdict(label, edgeId);
}
function chooseVerdict(label, edgeId) {
  const wrap = document.createElement("div");
  wrap.className = "banner info verdict-picker";
  wrap.dataset.edgeId = edgeId;
  wrap.textContent = `待审边 ${label}：`;
  const same = document.createElement("button");
  same.type = "button";
  same.textContent = "同一";
  const diff = document.createElement("button");
  diff.type = "button";
  diff.textContent = "不同";
  const cancel = document.createElement("button");
  cancel.type = "button";
  cancel.textContent = "取消";
  const pick = (verdict) => {
    wrap.remove();
    if (!verdict) return;
    submitReview(edgeId, verdict, label);
  };
  same.onclick = () => pick("same");
  diff.onclick = () => pick("diff");
  cancel.onclick = () => pick("");
  wrap.append(same, diff, cancel);
  dom.banners.appendChild(wrap);
  return wrap;
}
async function submitReview(edgeId, verdict, label = edgeId) {
  try {
    const outcome = await requestReview({
      edgeId,
      verdict,
      confirm: () => {
        const p = reviewPrompt(label, verdict);
        dom.confirmTitle.textContent = p.title;
        dom.confirmText.textContent = p.text;
        return openConfirmModal();
      },
      send: ({ edgeId: id, verdict: v }) => postReview(currentFetchImpl(), { edgeId: id, verdict: v, confirmed: true })
    });
    if (!outcome.sent) {
      if (outcome.reason === "not_confirmed") flashBanner("info", "已取消：未发出裁决请求");
      return;
    }
    const res = outcome.result || {};
    state.reviewed.add(edgeId);
    const extra = [
      res.degraded?.length ? `降级：${res.degraded.join("；")}` : "",
      res.warnings?.length ? `提示：${res.warnings.join("；")}` : ""
    ].filter(Boolean).join(" ");
    flashBanner("ok", `裁决已提交：${verdict} → op=${res.op || "?"} status=${res.status || "?"}${extra ? `（${extra}）` : ""}`);
    renderBanners();
    loadView(state.view);
  } catch (err) {
    flashBanner("error", `裁决失败：${err instanceof ApiError ? err.message : err}`);
  }
}
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
    dom.confirmModal.onclick = (ev) => {
      if (ev.target === dom.confirmModal) done(false);
    };
  });
}
function showNodeTooltip(id, ev) {
  const n = state.scene?.nodes.find((x2) => x2.id === id);
  if (!n) return;
  dom.tooltip.hidden = false;
  dom.tooltip.textContent = `${n.canonical_name || n.id} · concept=${n.concept_name || n.key || "(无)"}（${n.concept_status || "?"}）· degree=${n.degree} · depth=${n.depth}` + (n.matched ? " · 本次命中" : "");
  moveTooltip(ev);
}
function showEdgeTooltip(edgeId, ev) {
  const e = state.scene?.edges.find((x2) => x2.id === edgeId);
  if (!e) return;
  dom.tooltipEdge = edgeId;
  dom.tooltip.hidden = false;
  dom.tooltip.textContent = `${e.relation} · ${e.from} → ${e.to} · weight=${e.weight} · t_valid=${e.t_valid}${e.t_invalid ? ` · t_invalid=${e.t_invalid}` : ""}` + (e.style.invalid ? " · 本次视角已失效（虚线保留）" : "") + // 已失效的边不得再自称“待审”：那句提示就是“同一/不同”按钮的入口。
  (isPendingReview(e) ? " · 待审（点击可裁决）" : "");
  moveTooltip(ev);
}
function moveTooltip(ev) {
  const rect = dom.stage.getBoundingClientRect();
  dom.tooltip.style.left = `${Math.min(ev.clientX - rect.left + 12, rect.width - 260)}px`;
  dom.tooltip.style.top = `${Math.max(8, ev.clientY - rect.top - 28)}px`;
}
function hideTooltip() {
  dom.tooltip.hidden = true;
  dom.tooltipEdge = "";
}
function bindGraphGlobalListeners() {
  window.addEventListener("popstate", () => {
    const { view, errors } = parseView(location.search);
    if (errors.length) {
      state.errors = errors;
      renderBanners();
      return;
    }
    loadView(view);
  });
  let resizeTimer = null;
  window.addEventListener("resize", () => {
    clearTimeout(resizeTimer);
    resizeTimer = setTimeout(() => draw(), 120);
  });
}
async function boot() {
  bindGraphGlobalListeners();
  const { view, errors } = parseView(location.search);
  if (errors.length) {
    state.errors = errors;
    renderBanners();
    renderStageNote("参数错误：" + errors.map((e) => e.message).join("；"));
    return;
  }
  dom.hopsInput.value = String(view.hops);
  dom.budgetInput.value = String(view.budget);
  dom.qInput.value = view.q;
  await loadView(view);
}

// js/ui/errorState.js
var ERROR_KINDS = Object.freeze(["network", "request", "param"]);
function classifyError(err) {
  const status = Number(err && err.status ? err.status : 0);
  const code = String(err && err.code || "");
  const field = String(err && err.field || "");
  if (field || code === "bad_request" || status === 400) return "param";
  if (status >= 500 || status === 0) return "network";
  if (code === "network") return "network";
  return "request";
}
function errorState({ kind, message, detail = "", onRetry, onFix } = {}) {
  const k = ERROR_KINDS.includes(kind) ? kind : "request";
  const wrap = h("div", {
    className: `error-state error-${k}`,
    "data-error-kind": k,
    role: "alert"
  });
  wrap.appendChild(h("p", { className: "error-title", textContent: titleFor(k) }));
  wrap.appendChild(h("p", { className: "error-message", textContent: String(message || "未知错误") }));
  if (detail) {
    wrap.appendChild(h("p", { className: "error-detail", textContent: String(detail) }));
  }
  const actions = h("div", { className: "error-actions" });
  if (k !== "param" && typeof onRetry === "function") {
    actions.appendChild(h("button", {
      type: "button",
      className: "error-action",
      onclick: onRetry,
      textContent: "重试"
    }));
  }
  if (typeof onFix === "function") {
    actions.appendChild(h("button", {
      type: "button",
      className: "error-action",
      onclick: onFix,
      textContent: "修改参数"
    }));
  }
  if (actions.children && actions.children.length) wrap.appendChild(actions);
  return wrap;
}
function titleFor(kind) {
  switch (kind) {
    case "network":
      return "连不上服务";
    case "param":
      return "参数有误";
    default:
      return "请求失败";
  }
}

// js/recallstate.js
var K_MAX = 50;
var K_DEFAULT = 10;
var SCOPES = Object.freeze(["user", "agent", "project", "global"]);
var KINDS = Object.freeze(["semantic", "episodic", "procedural", "decision"]);
var LANES = Object.freeze(["lexical", "vector", "graph"]);
var INT_RE2 = /^-?\d+$/;
function splitList(raw) {
  return String(raw || "").split(",").map((s) => s.trim()).filter(Boolean);
}
function parseRecallView(search) {
  const params = new URLSearchParams(String(search || "").replace(/^\?/, ""));
  const errors = [];
  const view = {
    q: (params.get("q") || "").trim(),
    scope: (params.get("scope") || "").trim(),
    kinds: [],
    lanes: [],
    asOf: null,
    k: K_DEFAULT,
    item: (params.get("item") || "").trim()
  };
  if (view.scope && !SCOPES.includes(view.scope)) {
    errors.push({
      field: "scope",
      message: `scope 必须是 ${SCOPES.join(" / ")} 之一，收到 ${JSON.stringify(view.scope)}`
    });
  }
  for (const k of splitList(params.get("kinds"))) {
    if (!KINDS.includes(k)) {
      errors.push({
        field: "kinds",
        message: `kinds 只能是 ${KINDS.join(" / ")}，收到 ${JSON.stringify(k)}`
      });
      continue;
    }
    view.kinds.push(k);
  }
  for (const l of splitList(params.get("lanes"))) {
    if (!LANES.includes(l)) {
      errors.push({
        field: "lanes",
        message: `lanes 只能是 ${LANES.join(" / ")}，收到 ${JSON.stringify(l)}`
      });
      continue;
    }
    view.lanes.push(l);
  }
  const kRaw = params.get("k");
  if (kRaw !== null && kRaw !== "") {
    if (!INT_RE2.test(kRaw)) {
      errors.push({ field: "k", message: `k 必须是整数，收到 ${JSON.stringify(kRaw)}` });
    } else {
      const n = Number(kRaw);
      if (n < 1) {
        errors.push({ field: "k", message: `k 必须 >= 1，收到 ${n}` });
      } else if (n > K_MAX) {
        errors.push({
          field: "k",
          message: `k 不能超过硬上限 ${K_MAX}，收到 ${n}（不会静默夹取）`
        });
      } else {
        view.k = n;
      }
    }
  }
  const asOfRaw = params.get("as_of");
  if (asOfRaw !== null && asOfRaw !== "") {
    if (!INT_RE2.test(asOfRaw)) {
      errors.push({
        field: "as_of",
        message: `as_of 必须是 unix 秒（整数），收到 ${JSON.stringify(asOfRaw)}`
      });
    } else {
      view.asOf = Number(asOfRaw);
    }
  }
  return { view, errors };
}
function toRecallQuery(view) {
  const params = new URLSearchParams();
  if (view?.q) params.set("q", view.q);
  if (view?.scope) params.set("scope", view.scope);
  if (view?.kinds?.length) params.set("kinds", view.kinds.join(","));
  if (view?.lanes?.length) params.set("lanes", view.lanes.join(","));
  if (view?.asOf !== null && view?.asOf !== void 0) params.set("as_of", String(view.asOf));
  if (view?.k !== null && view?.k !== void 0 && view.k !== K_DEFAULT) {
    params.set("k", String(view.k));
  }
  if (view?.item) params.set("item", view.item);
  const qs = params.toString();
  return qs ? `?${qs}` : "";
}
function toRecallURL(view) {
  const apiView = { ...view, item: "" };
  const qs = toRecallQuery(apiView);
  return `/api/recall${qs}${qs ? "&" : "?"}explain=1`;
}
function toggleList(view, field, value) {
  const cur = Array.isArray(view?.[field]) ? view[field] : [];
  const next = cur.includes(value) ? cur.filter((v) => v !== value) : [...cur, value];
  const order = field === "kinds" ? KINDS : LANES;
  return { ...view, [field]: order.filter((v) => next.includes(v)), item: "" };
}

// js/ui/empty.js
var EMPTY_KINDS = Object.freeze(["idle", "noData", "noCapability"]);
function emptyState({ kind = "idle", title, reason = "", suggestions = [] } = {}) {
  const k = EMPTY_KINDS.includes(kind) ? kind : "idle";
  const wrap = h("div", { className: `empty empty-${k}`, "data-empty-kind": k, role: "status" });
  wrap.appendChild(h("p", { className: "empty-title", textContent: title || "这里还没有内容" }));
  if (reason) {
    wrap.appendChild(h("p", { className: "empty-reason", textContent: reason }));
  }
  const actions = suggestions.filter((s) => s && s.label);
  if (actions.length) {
    const list = h("div", { className: "empty-actions" });
    for (const s of actions) {
      if (s.href) {
        list.appendChild(h("a", {
          className: "empty-action",
          href: s.href,
          // 站内链接不整页刷新：交给调用方的客户端路由处理。
          onclick: typeof s.onClick === "function" ? s.onClick : void 0,
          textContent: s.label
        }));
      } else {
        list.appendChild(h("button", {
          type: "button",
          className: "empty-action",
          onclick: typeof s.onClick === "function" ? s.onClick : void 0,
          textContent: s.label
        }));
      }
    }
    wrap.appendChild(list);
  }
  return wrap;
}
var EXAMPLE_QUERIES = Object.freeze(["acme", "Alice", "deploy"]);
function exampleSuggestions(onPick, queries = EXAMPLE_QUERIES) {
  return queries.map((q) => ({
    label: `试试「${q}」`,
    onClick: () => {
      if (typeof onPick === "function") onPick(q);
    }
  }));
}

// js/ui/skeleton.js
function skeleton({ rows = 3, label = "正在加载", variant = "list" } = {}) {
  const n = Math.max(1, Math.min(20, Number(rows) || 3));
  const wrap = h("div", {
    className: `skeleton skeleton-${variant}`,
    "aria-busy": "true",
    "aria-label": String(label || "正在加载"),
    role: "status",
    "data-skeleton-variant": variant
  });
  for (let i = 0; i < n; i += 1) {
    wrap.appendChild(h("div", { className: "skeleton-row", "aria-hidden": "true" }));
  }
  return wrap;
}

// js/ui/chart.js
function linePath(points) {
  if (!Array.isArray(points) || points.length === 0) return "";
  return points.map((p, i) => `${i === 0 ? "M" : "L"}${round2(p.x)} ${round2(p.y)}`).join(" ");
}
function round2(v) {
  const n = Number(v);
  return Number.isFinite(n) ? Number(n.toFixed(2)) : 0;
}
function sparkline({
  values = [],
  width = 160,
  height = 36,
  label = "图表",
  min,
  max,
  className = ""
} = {}) {
  const nums = (Array.isArray(values) ? values : []).map(Number).filter((v) => Number.isFinite(v));
  const svg = h("svg", {
    className: `spark ${className}`.trim(),
    viewBox: `0 0 ${width} ${height}`,
    width: String(width),
    height: String(height),
    role: "img",
    "aria-label": String(label || "图表")
  });
  if (nums.length === 0) return svg;
  const lo = Number.isFinite(min) ? Number(min) : Math.min(...nums);
  const hi = Number.isFinite(max) ? Number(max) : Math.max(...nums);
  const span = hi - lo || 1;
  const pad = 2;
  const innerW = width - pad * 2;
  const innerH = height - pad * 2;
  const pts = nums.map((v, i) => ({
    x: pad + (nums.length === 1 ? innerW / 2 : i / (nums.length - 1) * innerW),
    y: pad + innerH - (v - lo) / span * innerH
  }));
  svg.appendChild(h("path", {
    className: "spark-line",
    d: linePath(pts),
    fill: "none",
    "stroke-width": "1.5"
  }));
  return svg;
}

// js/recallpage.js
var SCORE_EPS = 1e-3;
function scoreBreakdown(item) {
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
    consistent: Number.isFinite(delta) && delta <= SCORE_EPS
  };
}
function planSummary(resp) {
  const plan = resp?.plan || {};
  const hasPlan = resp?.plan !== null && resp?.plan !== void 0;
  return {
    query: plan.query ?? resp?.query ?? "",
    shape: plan.shape ?? "",
    shapeReason: plan.shape_reason ?? "",
    coverage: Number(plan.coverage ?? resp?.coverage ?? NaN),
    thetaCoverage: Number(plan.theta_coverage ?? resp?.theta_coverage ?? NaN),
    hops: Number(plan.hops ?? 0),
    anchors: Array.isArray(plan.anchors) ? plan.anchors : [],
    matched: Array.isArray(plan.matched) ? plan.matched : [],
    unmatched: Array.isArray(plan.unmatched) ? plan.unmatched : [],
    relations: Array.isArray(plan.relations) ? plan.relations : [],
    steps: Array.isArray(plan.steps) ? plan.steps : [],
    fromPlan: hasPlan
  };
}
function laneRows(resp) {
  const lanes = Array.isArray(resp?.plan?.lanes) ? resp.plan.lanes : [];
  const items = Array.isArray(resp?.items) ? resp.items : [];
  return lanes.map((lp) => {
    const hits = [];
    for (const it of items) {
      for (const c2 of it.contributions || []) {
        if (c2.lane !== lp.lane) continue;
        hits.push({
          memoryId: it.id,
          rank: Number(c2.rank),
          weight: Number(c2.weight),
          value: Number(c2.value)
        });
      }
    }
    hits.sort((a2, b) => a2.rank - b.rank || String(a2.memoryId).localeCompare(String(b.memoryId)));
    return {
      lane: lp.lane,
      weight: Number(lp.weight),
      budget: Number(lp.budget),
      selected: Boolean(lp.selected),
      executed: Boolean(lp.executed),
      // reason 与 skip_reason 至少有一个要能显示（否则读者不知道它为何执行/被跳过）。
      reason: lp.reason || "",
      skipReason: lp.skip_reason || "",
      hits
    };
  });
}
function droppedRows(resp) {
  const list = Array.isArray(resp?.plan?.dropped) ? resp.plan.dropped : Array.isArray(resp?.dropped) ? resp.dropped : [];
  return list.map((d) => ({
    id: d.id || "",
    reason: d.reason || "",
    score: Number(d.score)
  }));
}
function degradedLines(resp) {
  const out = [];
  const push = (v) => {
    const s = String(v ?? "").trim();
    if (s) out.push(s);
  };
  for (const d of resp?.degraded || []) push(d);
  for (const d of resp?.meta?.degraded || []) push(d);
  return out;
}
function asOfBanner(resp, view) {
  const asOf = resp?.as_of ?? view?.asOf ?? null;
  if (asOf === null || asOf === void 0) return null;
  return `你正在看 ${fmtUnix(asOf)}（as_of=${asOf}）的历史视角，不是当前状态`;
}
function isEmptyQuery(view) {
  return !String(view?.q || "").trim();
}
function noCapabilityReason(resp) {
  const caps = resp?.meta?.capabilities;
  if (!caps) return null;
  if (caps.recall_plan) return null;
  const reasons = Array.isArray(caps.reasons) ? caps.reasons : [];
  const hit = reasons.find((r) => String(r).startsWith("recall_plan"));
  return hit || "recall_plan 能力不可用";
}
var COVERAGE_RANGE = Object.freeze({ min: 0, max: 1 });
function kv(pairs) {
  const dl = h("dl", { className: "kv" });
  for (const [k, v, num] of pairs) {
    dl.appendChild(h("dt", { textContent: k }));
    dl.appendChild(h("dd", { className: num ? "num" : "", textContent: String(v ?? "—") }));
  }
  return dl;
}
function renderPlanSection(resp) {
  const p = planSummary(resp);
  const section = h("section", { className: "recall-section", "data-section": "plan" });
  section.appendChild(h("h2", { textContent: "plan（召回规划）" }));
  const cov = h("div", { className: "chips" });
  cov.appendChild(h("span", { className: "chip", textContent: `coverage ${fmt(p.coverage)}` }));
  cov.appendChild(h("span", { className: "chip", textContent: `θ ${fmt(p.thetaCoverage)}` }));
  cov.appendChild(sparkline({
    values: [p.coverage, p.thetaCoverage],
    width: 120,
    height: 22,
    min: COVERAGE_RANGE.min,
    max: COVERAGE_RANGE.max,
    label: `coverage ${fmt(p.coverage)}，门槛 ${fmt(p.thetaCoverage)}`
  }));
  section.appendChild(kv([
    ["query", p.query || "—"],
    ["shape", p.shape || "—"],
    ["shape_reason", p.shapeReason || "—"],
    ["coverage", fmt(p.coverage), true],
    ["theta_coverage", fmt(p.thetaCoverage), true],
    ["hops", String(p.hops)],
    ["anchors", p.anchors.length ? p.anchors.join(", ") : "（无实体锚点）"],
    ["matched", String(p.matched.length), true],
    ["unmatched", String(p.unmatched.length), true]
  ]));
  if (!p.fromPlan) {
    section.appendChild(h("p", { className: "hint", textContent: "注意：响应里没有 plan 对象，上面取自顶层字段。" }));
  }
  section.appendChild(h("h3", { textContent: `未对齐槽位（${p.unmatched.length}）` }));
  if (p.unmatched.length === 0) {
    section.appendChild(h("p", { className: "hint", textContent: "全部槽位都已对齐。" }));
  } else {
    const table = h("table", { className: "table", "data-table": "unmatched" });
    const head = h("tr", {});
    for (const [label, cls] of [["kind", ""], ["text", ""], ["required", "num"], ["weight", "num"]]) {
      head.appendChild(h("th", { className: cls, textContent: label }));
    }
    table.appendChild(h("thead", {}, [head]));
    const body = h("tbody", {});
    for (const s of p.unmatched) {
      body.appendChild(h("tr", {}, [
        h("td", { textContent: s.kind || "" }),
        h("td", { textContent: s.text || "" }),
        h("td", { className: "num", textContent: s.required ? "必需" : "可选" }),
        h("td", { className: "num", textContent: fmt(s.weight) })
      ]));
    }
    table.appendChild(body);
    section.appendChild(table);
  }
  if (p.relations.length) {
    section.appendChild(h("h3", { textContent: `关系跳数（${p.relations.length}）` }));
    const table = h("table", { className: "table", "data-table": "relations" });
    const head = h("tr", {});
    for (const [label, cls] of [["relation", ""], ["transitive", ""], ["max_hops", "num"], ["weight", "num"], ["origin", ""]]) {
      head.appendChild(h("th", { className: cls, textContent: label }));
    }
    table.appendChild(h("thead", {}, [head]));
    const body = h("tbody", {});
    for (const r of p.relations) {
      body.appendChild(h("tr", {}, [
        h("td", { textContent: r.relation || "" }),
        h("td", { textContent: r.transitive ? "传递" : "非传递" }),
        h("td", { className: "num", textContent: String(r.max_hops ?? "") }),
        h("td", { className: "num", textContent: fmt(r.weight) }),
        h("td", { textContent: r.origin || "" })
      ]));
    }
    table.appendChild(body);
    section.appendChild(table);
  }
  return section;
}
function renderLaneSection(resp, { onToggle } = {}) {
  const rows = laneRows(resp);
  const section = h("section", { className: "recall-section", "data-section": "lanes" });
  section.appendChild(h("h2", { textContent: `lane（${rows.length} 条通道的规划与贡献）` }));
  if (rows.length === 0) {
    section.appendChild(h("p", { className: "hint", textContent: "响应里没有 lane 规划。" }));
    return section;
  }
  const table = h("table", { className: "table", "data-table": "lanes" });
  const head = h("tr", {});
  for (const [label, cls] of [
    ["lane", ""],
    ["weight", "num"],
    ["budget", "num"],
    ["selected", "num"],
    ["executed", "num"],
    ["reason / skip_reason", ""]
  ]) {
    head.appendChild(h("th", { className: cls, textContent: label }));
  }
  table.appendChild(h("thead", {}, [head]));
  const body = h("tbody", {});
  for (const r of rows) {
    const tr = h("tr", { className: "lane-row", "data-lane": r.lane });
    const btn = h("button", {
      type: "button",
      className: "row-button",
      "aria-expanded": "false",
      textContent: r.lane
    });
    tr.appendChild(h("td", {}, [btn]));
    tr.appendChild(h("td", { className: "num", textContent: fmt(r.weight) }));
    tr.appendChild(h("td", { className: "num", textContent: String(r.budget) }));
    tr.appendChild(h("td", { className: `num lane-flag ${r.selected ? "flag-yes" : "flag-no"}`, textContent: r.selected ? "是" : "否" }));
    tr.appendChild(h("td", { className: `num lane-flag ${r.executed ? "flag-yes" : "flag-no"}`, textContent: r.executed ? "是" : "否" }));
    tr.appendChild(h("td", { textContent: r.reason || (r.skipReason ? `跳过：${r.skipReason}` : "—") }));
    body.appendChild(tr);
    const detail = h("tr", { className: "lane-detail", hidden: true, "data-lane-hits": r.lane });
    const cell = h("td", { colspan: "6" });
    if (r.hits.length === 0) {
      cell.appendChild(h("p", {
        className: "hint",
        textContent: r.executed ? "这一路被执行，但对最终结果没有贡献（没有条目在它上面得分）。" : "这一路没有执行，因此没有贡献。"
      }));
    } else {
      const hitTable = h("table", { className: "table", "data-table": "lane-hits" });
      const hh = h("tr", {});
      for (const [label, cls] of [["memory", ""], ["rank", "num"], ["weight", "num"], ["value", "num"]]) {
        hh.appendChild(h("th", { className: cls, textContent: label }));
      }
      hitTable.appendChild(h("thead", {}, [hh]));
      const hb = h("tbody", {});
      for (const hit of r.hits) {
        hb.appendChild(h("tr", {}, [
          h("td", { textContent: hit.memoryId }),
          h("td", { className: "num", textContent: String(hit.rank) }),
          h("td", { className: "num", textContent: fmt(hit.weight) }),
          h("td", { className: "num", textContent: fmt(hit.value) })
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
      tr.classList.toggle("open", open);
      btn.setAttribute("aria-expanded", open ? "true" : "false");
      if (typeof onToggle === "function") onToggle(r.lane, open);
    };
    btn.onclick = toggle;
  }
  table.appendChild(body);
  section.appendChild(table);
  return section;
}
function renderDetail(resp, itemId, { onClose } = {}) {
  const wrap = h("div", { className: "recall-detail", "data-section": "detail" });
  const head = h("div", { className: "drawer-head" });
  head.appendChild(h("span", { textContent: "分数分解" }));
  if (typeof onClose === "function") {
    head.appendChild(h("button", { type: "button", onclick: onClose, textContent: "关闭" }));
  }
  wrap.appendChild(head);
  const item = (resp?.items || []).find((it) => it.id === itemId);
  if (!item) {
    wrap.appendChild(h("p", {
      className: "hint",
      textContent: itemId ? `结果 ${itemId} 不在本次响应里（可能已被预算切断）。` : "点左侧任意一条结果，这里显示它的分数分解。"
    }));
    return wrap;
  }
  const b = scoreBreakdown(item);
  wrap.appendChild(h("h2", { textContent: item.id }));
  const box = h("div", { className: "score-box", "data-section": "score" });
  const formula = h("p", {
    className: "score-formula",
    textContent: `score = fusion_score × concept_boost × recency_factor = ${fmt(b.fusion)} × ${fmt(b.boost)} × ${fmt(b.recency)} = ${fmt(b.product)}`
  });
  box.appendChild(formula);
  const finalRow = h("div", { className: "score-final" });
  finalRow.appendChild(h("span", { textContent: fmt(b.score) }));
  finalRow.appendChild(h("span", { className: "unit", textContent: "score（后端）" }));
  finalRow.appendChild(h("span", {
    className: `score-check ${b.consistent ? "ok" : "bad"}`,
    "data-consistent": b.consistent ? "true" : "false",
    textContent: b.consistent ? `✓ 三因子相乘与 score 一致（差 ${fmt(b.delta)} ≤ ${SCORE_EPS}）` : `✗ 三因子相乘 = ${fmt(b.product)}，与 score = ${fmt(b.score)} 差 ${fmt(b.delta)}（> ${SCORE_EPS}）`
  }));
  box.appendChild(finalRow);
  box.appendChild(kv([
    ["fusion_score", fmt(b.fusion), true],
    ["concept_boost", fmt(b.boost), true],
    ["recency_factor", fmt(b.recency), true],
    ["三因子相乘", fmt(b.product), true]
  ]));
  wrap.appendChild(box);
  const adj = Array.isArray(item.adjustments) ? item.adjustments : [];
  wrap.appendChild(h("h3", { textContent: `调整明细（${adj.length}）` }));
  if (adj.length === 0) {
    wrap.appendChild(h("p", { className: "hint", textContent: "没有调整记录。" }));
  } else {
    const ul = h("ul", { className: "plain-list", "data-list": "adjustments" });
    for (const a2 of adj) {
      ul.appendChild(h("li", {
        textContent: `${a2.kind} ×${fmt(a2.factor)} — ${a2.reason || "（无说明）"}`
      }));
    }
    wrap.appendChild(ul);
  }
  const contrib = Array.isArray(item.contributions) ? item.contributions : [];
  wrap.appendChild(h("h3", { textContent: `分通道贡献（${contrib.length}）` }));
  if (contrib.length === 0) {
    wrap.appendChild(h("p", { className: "hint", textContent: "没有通道贡献记录。" }));
  } else {
    const table = h("table", { className: "table", "data-table": "contributions" });
    const hh = h("tr", {});
    for (const [label, cls] of [["lane", ""], ["rank", "num"], ["weight", "num"], ["value", "num"]]) {
      hh.appendChild(h("th", { className: cls, textContent: label }));
    }
    table.appendChild(h("thead", {}, [hh]));
    const tb = h("tbody", {});
    for (const c2 of contrib) {
      tb.appendChild(h("tr", {}, [
        h("td", { textContent: c2.lane || "" }),
        h("td", { className: "num", textContent: String(c2.rank ?? "") }),
        h("td", { className: "num", textContent: fmt(c2.weight) }),
        h("td", { className: "num", textContent: fmt(c2.value) })
      ]));
    }
    table.appendChild(tb);
    wrap.appendChild(table);
  }
  const ev = Array.isArray(item.graph_evidence) ? item.graph_evidence : [];
  wrap.appendChild(h("h3", { textContent: `图证据（${ev.length}）` }));
  if (ev.length === 0) {
    wrap.appendChild(h("p", { className: "hint", textContent: "这条结果不是经由 graph lane 召回的，没有边证据。" }));
  } else {
    const ul = h("ul", { className: "plain-list", "data-list": "graph-evidence" });
    for (const e of ev) {
      ul.appendChild(h("li", {
        textContent: `${e.from} -[${e.relation}]-> ${e.to}（anchor=${e.anchor}，depth=${e.depth}，linked_by=${e.linked_by}，edge=${e.edge_id}）`
      }));
    }
    wrap.appendChild(ul);
  }
  wrap.appendChild(h("h3", { textContent: "记忆正文" }));
  wrap.appendChild(h("p", { className: "result-content", textContent: item.content || "（空）" }));
  return wrap;
}
function renderResultList(resp, { selectedId, onSelect } = {}) {
  const items = Array.isArray(resp?.items) ? resp.items : [];
  const section = h("section", { className: "recall-section", "data-section": "items" });
  section.appendChild(h("h2", { textContent: `结果（${items.length}）` }));
  if (items.length === 0) {
    section.appendChild(h("p", {
      className: "hint",
      textContent: "本次召回没有返回条目：见上方 plan 的 unmatched / missing 与降级横幅。"
    }));
    return section;
  }
  const table = h("table", { className: "table", "data-table": "items" });
  const head = h("tr", {});
  for (const [label, cls] of [["score", "num"], ["记忆", ""], ["kind", ""], ["概念", ""]]) {
    head.appendChild(h("th", { className: cls, textContent: label }));
  }
  table.appendChild(h("thead", {}, [head]));
  const body = h("tbody", {});
  for (const it of items) {
    const tr = h("tr", {
      className: `result-row${it.id === selectedId ? " selected" : ""}`,
      "data-item-id": it.id
    });
    const btn = h("button", {
      type: "button",
      className: "row-button",
      textContent: fmt(it.score)
    });
    btn.onclick = () => {
      if (typeof onSelect === "function") onSelect(it.id);
    };
    tr.appendChild(h("td", { className: "num" }, [h("span", { className: "result-score" }, [btn])]));
    tr.appendChild(h("td", {}, [
      h("div", { className: "result-content", textContent: it.content || "（空）" }),
      h("div", {
        className: "result-meta",
        textContent: `id=${it.id} · v${it.version} · scope=${it.scope}` + (it.source_episode ? ` · src=${it.source_episode}` : "")
      })
    ]));
    tr.appendChild(h("td", { textContent: it.kind || "" }));
    tr.appendChild(h("td", { textContent: it.concept_id || "—" }));
    body.appendChild(tr);
  }
  table.appendChild(body);
  section.appendChild(table);
  return section;
}
function renderDroppedSection(resp) {
  const rows = droppedRows(resp);
  const section = h("section", { className: "recall-section", "data-section": "dropped" });
  section.appendChild(h("h2", { textContent: `被拦候选（${rows.length}）` }));
  if (rows.length === 0) {
    section.appendChild(h("p", { className: "hint", textContent: "没有被拦截的候选。" }));
    return section;
  }
  const table = h("table", { className: "table", "data-table": "dropped" });
  const head = h("tr", {});
  for (const [label, cls] of [["id", ""], ["reason", ""], ["score", "num"]]) {
    head.appendChild(h("th", { className: cls, textContent: label }));
  }
  table.appendChild(h("thead", {}, [head]));
  const body = h("tbody", {});
  for (const d of rows) {
    body.appendChild(h("tr", {}, [
      h("td", { textContent: d.id }),
      h("td", { textContent: d.reason }),
      h("td", { className: "num", textContent: fmt(d.score) })
    ]));
  }
  table.appendChild(body);
  section.appendChild(table);
  return section;
}
function renderMissingSection(resp) {
  const missing = Array.isArray(resp?.missing) ? resp.missing : [];
  if (missing.length === 0) return null;
  const section = h("section", { className: "recall-section", "data-section": "missing" });
  section.appendChild(h("h2", { textContent: `缺什么（${missing.length}）` }));
  const ul = h("ul", { className: "plain-list", "data-list": "missing" });
  for (const m2 of missing) {
    ul.appendChild(h("li", {
      textContent: `[${m2.kind}] ${m2.text}：${m2.reason}` + (m2.suggestion ? `（建议：${m2.suggestion}）` : "")
    }));
  }
  section.appendChild(ul);
  return section;
}
function renderDegradedBanners(resp) {
  const lines = degradedLines(resp);
  if (lines.length === 0) return [];
  return [banner("warn", `降级（共 ${lines.length} 条，本次召回结果不完整）：${lines.join("；")}`)];
}
function renderAsOfBanner(resp, view, { onBackToNow } = {}) {
  const text = asOfBanner(resp, view);
  if (!text) return null;
  const el2 = h("div", { className: "asof-banner", role: "status", "data-section": "asof" });
  el2.appendChild(h("span", { textContent: text }));
  el2.appendChild(h("button", {
    type: "button",
    onclick: () => {
      if (typeof onBackToNow === "function") onBackToNow();
    },
    textContent: "回到现在"
  }));
  return el2;
}
function renderEmptyState({ onPick } = {}) {
  return emptyState({
    kind: "idle",
    title: "还没有查询",
    reason: "Recall Inspector 需要一个问题：输入关键词后，这里会给出召回规划、每条结果的分数分解，以及被拦下的候选。",
    suggestions: exampleSuggestions(onPick)
  });
}
function renderNoCapability(reason) {
  return emptyState({
    kind: "noCapability",
    title: "这个后端不支持召回解释",
    reason: reason || "meta.capabilities.recall_plan = false",
    suggestions: []
  });
}
function renderLoading() {
  return skeleton({ rows: 6, label: "正在加载召回解释" });
}
function renderError({ kind, message, detail, onRetry, onFix }) {
  return errorState({ kind, message, detail, onRetry, onFix });
}
function renderFilters(view, { onChange } = {}) {
  const wrap = h("div", { className: "chips", "data-section": "filters" });
  const fire = (next) => {
    if (typeof onChange === "function") onChange(next);
  };
  const scope = h("select", { "aria-label": "scope" });
  scope.appendChild(h("option", { value: "", textContent: "scope：默认（user）" }));
  for (const s of SCOPES) {
    scope.appendChild(h("option", { value: s, textContent: s, selected: view.scope === s ? "selected" : false }));
  }
  scope.onchange = () => fire({ ...view, scope: scope.value, item: "" });
  wrap.appendChild(scope);
  for (const kind of KINDS) {
    const on = (view.kinds || []).includes(kind);
    wrap.appendChild(h("button", {
      type: "button",
      className: on ? "chip on" : "chip",
      "aria-pressed": on ? "true" : "false",
      textContent: kind,
      onclick: () => fire(toggleList(view, "kinds", kind))
    }));
  }
  for (const lane of LANES) {
    const on = (view.lanes || []).includes(lane);
    wrap.appendChild(h("button", {
      type: "button",
      className: on ? "chip on" : "chip",
      "aria-pressed": on ? "true" : "false",
      textContent: `lane:${lane}`,
      onclick: () => fire(toggleList(view, "lanes", lane))
    }));
  }
  const k = h("input", {
    type: "number",
    min: "1",
    max: String(K_MAX),
    step: "1",
    value: String(view.k ?? ""),
    "aria-label": "k（返回条数）"
  });
  k.onchange = () => {
    const n = Number(k.value);
    fire({ ...view, k: Number.isFinite(n) ? n : k.value, item: "" });
  };
  wrap.appendChild(k);
  return wrap;
}
function renderRecallPageView(snapshot, handlers = {}) {
  const { view = {}, status = "idle", resp = null, errors = [], error = null } = snapshot || {};
  const root = h("div", { className: "recall-body", "data-status": status });
  const asof = renderAsOfBanner(resp, view, handlers);
  if (asof) root.appendChild(asof);
  const banners = h("div", { className: "recall-banners" });
  for (const e of errors) {
    banners.appendChild(banner("error", `参数错误（${e.field}）：${e.message}`));
  }
  if (resp) {
    for (const b of renderDegradedBanners(resp)) banners.appendChild(b);
    const st = resp.status;
    if (st && st !== "ok") {
      banners.appendChild(banner("info", `召回状态：${st}（coverage=${fmt(resp.coverage)}）`));
    }
  }
  root.appendChild(banners);
  if (status === "loading") {
    root.appendChild(renderLoading());
    return root;
  }
  if (errors.length) {
    root.appendChild(renderError({
      kind: "param",
      message: errors.map((e) => e.message).join("；"),
      detail: `出错参数：${errors.map((e) => e.field).join(", ")}`,
      onFix: handlers.onFixParams
    }));
    return root;
  }
  if (status === "error") {
    root.appendChild(renderError({
      kind: error?.kind || "request",
      message: error?.message || "未知错误",
      detail: error?.detail || "",
      onRetry: handlers.onRetry
    }));
    return root;
  }
  if (isEmptyQuery(view)) {
    root.appendChild(renderEmptyState({ onPick: (q) => {
      if (typeof handlers.onQuery === "function") handlers.onQuery(q);
    } }));
    return root;
  }
  const noCap = noCapabilityReason(resp);
  if (noCap) {
    root.appendChild(renderNoCapability(noCap));
    return root;
  }
  if (!resp) {
    root.appendChild(renderLoading());
    return root;
  }
  root.appendChild(renderFilters(view, { onChange: handlers.onViewChange }));
  const layout = h("div", { className: "recall-layout" });
  const list = h("div", { className: "recall-list" });
  list.appendChild(renderPlanSection(resp));
  list.appendChild(renderLaneSection(resp));
  const missing = renderMissingSection(resp);
  if (missing) list.appendChild(missing);
  list.appendChild(renderResultList(resp, {
    selectedId: view.item || "",
    onSelect: (id) => {
      if (typeof handlers.onSelectItem === "function") handlers.onSelectItem(id);
    }
  }));
  list.appendChild(renderDroppedSection(resp));
  layout.appendChild(list);
  const detail = renderDetail(resp, view.item || "", {
    onClose: () => {
      if (typeof handlers.onSelectItem === "function") handlers.onSelectItem("");
    }
  });
  layout.appendChild(detail);
  root.appendChild(layout);
  return root;
}

// js/recallmain.js
var recallState = {
  view: { q: "", scope: "", kinds: [], lanes: [], asOf: null, k: 10, item: "" },
  status: "idle",
  // idle | loading | ready | error
  resp: null,
  errors: [],
  error: null,
  // 请求序号：并发请求（快速连打）时只有最新一次的结果能落到页面上，
  // 否则慢请求后到会覆盖快请求的新结果 —— 表现为"页面显示的是上一次的答案"。
  seq: 0
};
var container = null;
function mountRecallPage(el2) {
  container = el2;
  return container;
}
function currentFetchImpl2() {
  return globalThis.fetch.bind(globalThis);
}
function syncRecallURL(view, { replace = false } = {}) {
  const qs = toRecallQuery(view);
  const url = `/recall${qs}`;
  if (replace) history.replaceState({ view }, "", url);
  else history.pushState({ view }, "", url);
}
function render2() {
  if (!container) return;
  container.textContent = "";
  container.appendChild(renderRecallPageView({
    view: recallState.view,
    status: recallState.status,
    resp: recallState.resp,
    errors: recallState.errors,
    error: recallState.error
  }, {
    onSelectItem: (id) => {
      const next = { ...recallState.view, item: id || "" };
      recallState.view = next;
      syncRecallURL(next, { replace: true });
      render2();
    },
    onViewChange: (next) => {
      loadRecall(next);
    },
    onQuery: (q) => {
      loadRecall({ ...recallState.view, q, item: "" });
    },
    onBackToNow: () => {
      loadRecall({ ...recallState.view, asOf: null, item: "" });
    },
    onRetry: () => {
      loadRecall(recallState.view);
    },
    onFixParams: () => {
      loadRecall({ q: recallState.view.q, scope: "", kinds: [], lanes: [], asOf: null, k: 10, item: "" });
    }
  }));
}
async function loadRecall(view) {
  const { view: parsed, errors } = parseRecallView(toRecallQuery(view));
  recallState.view = parsed;
  recallState.errors = errors;
  recallState.error = null;
  syncRecallURL(parsed, { replace: false });
  if (errors.length) {
    recallState.status = "ready";
    recallState.resp = null;
    render2();
    return;
  }
  if (!parsed.q) {
    recallState.status = "idle";
    recallState.resp = null;
    render2();
    return;
  }
  recallState.status = "loading";
  render2();
  const mySeq = ++recallState.seq;
  let resp;
  try {
    resp = await fetchRecall(currentFetchImpl2(), toRecallURL(parsed));
  } catch (err) {
    if (mySeq !== recallState.seq) return;
    recallState.status = "error";
    recallState.resp = null;
    recallState.error = {
      kind: classifyError(err),
      message: err instanceof ApiError ? err.message : String(err),
      detail: err?.field ? `出错参数：${err.field}` : ""
    };
    render2();
    return;
  }
  if (mySeq !== recallState.seq) return;
  recallState.resp = resp;
  recallState.status = "ready";
  render2();
}
async function bootRecall(el2) {
  mountRecallPage(el2);
  document.body.classList.add("route-recall");
  const { view, errors } = parseRecallView(location.search);
  if (errors.length) {
    recallState.view = view;
    recallState.errors = errors;
    recallState.status = "ready";
    render2();
    return;
  }
  await loadRecall(view);
}

// js/entry.js
function hideAllPages() {
  for (const id of ["page-recall", "page-generic"]) {
    const el2 = document.getElementById(id);
    if (el2) el2.hidden = true;
  }
  document.body.classList.remove("route-recall", "route-generic");
}
function renderGeneric(container2, { page, path }) {
  if (!container2) return;
  container2.textContent = "";
  const wrap = h("div", { className: "page-missing" });
  if (page === "missing") {
    wrap.appendChild(h("h1", { textContent: "页面不存在" }));
    wrap.appendChild(h("p", { textContent: `地址 ${path} 没有对应的页面。` }));
  } else {
    wrap.appendChild(h("h1", { textContent: PAGE_TITLES[page] || page }));
    wrap.appendChild(h("p", { textContent: notImplementedMessage(page) }));
  }
  const nav = h("div", { className: "chips" });
  nav.appendChild(h("a", { className: "empty-action", href: "/", textContent: "← Graph View" }));
  nav.appendChild(h("a", { className: "empty-action", href: "/recall?q=acme", textContent: "Recall Inspector →" }));
  wrap.appendChild(nav);
  container2.appendChild(wrap);
  container2.hidden = false;
  document.body.classList.add("route-generic");
}
async function dispatch2(pathname) {
  const { page, path } = matchRoute(pathname);
  hideAllPages();
  if (page === "graph") {
    document.title = "Gmem · Graph View";
    await boot();
    return { page, path };
  }
  if (page === "recall") {
    document.title = "Gmem · Recall Inspector";
    const el2 = document.getElementById("page-recall");
    if (el2) el2.hidden = false;
    await bootRecall(document.getElementById("recall-root"));
    return { page, path };
  }
  renderGeneric(document.getElementById("page-generic"), { page, path });
  if (!isImplemented(page)) {
    document.title = `Gmem · ${PAGE_TITLES[page] || page}`;
  }
  return { page, path };
}
if (typeof document !== "undefined" && typeof location !== "undefined") {
  dispatch2(location.pathname);
  window.addEventListener("popstate", () => {
    dispatch2(location.pathname);
  });
  document.addEventListener("click", (ev) => {
    if (ev.defaultPrevented || ev.button !== 0 || ev.metaKey || ev.ctrlKey || ev.shiftKey || ev.altKey) return;
    const a2 = ev.target && ev.target.closest ? ev.target.closest("a[href]") : null;
    if (!a2) return;
    const href = a2.getAttribute("href");
    if (!href || !href.startsWith("/") || href.startsWith("//")) return;
    if (a2.target && a2.target !== "_self") return;
    ev.preventDefault();
    history.pushState({}, "", href);
    dispatch2(location.pathname);
  });
}
export {
  dispatch2 as dispatch
};
//# sourceMappingURL=app.js.map
