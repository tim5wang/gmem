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
  let h = 5381;
  for (let i = 0; i < str.length; i++) {
    h = (h << 5) + h + str.charCodeAt(i) >>> 0;
  }
  return h >>> 0;
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
function makeProjector(scene, width, height, padding = 48) {
  const b = bounds(scene);
  const w = Math.max(1, width - padding * 2);
  const h = Math.max(1, height - padding * 2);
  const scale = Math.min(w / (b.maxX - b.minX), h / (b.maxY - b.minY));
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
    renderSVG(svg, scene, opts);
    return "svg";
  }
  svg.hidden = true;
  canvas.hidden = false;
  renderCanvas(canvas, scene, opts);
  return "canvas";
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
  for (const e of scene.edges) {
    const a2 = pos.get(e.from);
    const b = pos.get(e.to);
    if (!a2 || !b) continue;
    const line = el("line", {
      x1: a2.x.toFixed(1),
      y1: a2.y.toFixed(1),
      x2: b.x.toFixed(1),
      y2: b.y.toFixed(1),
      stroke: e.style.possiblySame ? STYLE.edgePossiblySameColor : e.style.invalid ? STYLE.edgeInvalidColor : STYLE.edgeColor,
      "stroke-width": String(e.style.possiblySame ? 2 : 1.2),
      "stroke-opacity": String(e.style.opacity),
      // 失效边画虚线 = 保留历史（不过滤）。
      "stroke-dasharray": e.style.dashed ? "4 3" : "none",
      "data-edge-id": e.id,
      "data-from": e.from,
      "data-to": e.to,
      "data-invalid": e.style.invalid ? "1" : "0",
      "data-possibly-same": e.style.possiblySame ? "1" : "0",
      class: `edge${e.style.possiblySame ? " possibly-same" : ""}${e.style.invalid ? " invalidated" : ""}`
    });
    gEdges.appendChild(line);
  }
  for (const c2 of scene.clusters) {
    const p = clusterPos.get(c2.id || c2.concept_id || "");
    if (!p) continue;
    const r = 10 + Math.min(30, Math.sqrt(Math.max(1, c2.count)) * 3.2);
    const g = el("g", {
      class: "cluster",
      "data-cluster-id": c2.concept_id || "",
      "data-cluster-name": c2.concept_name || c2.id || "",
      "data-count": String(c2.count),
      transform: `translate(${p.x.toFixed(1)},${p.y.toFixed(1)})`
    });
    g.appendChild(el("circle", {
      r: r.toFixed(1),
      fill: c2.concept_status === "draft" ? "url(#draft-hatch)" : STYLE.clusterFill,
      stroke: c2.matched ? STYLE.matchedStroke : STYLE.clusterStroke,
      "stroke-width": c2.matched ? "2.5" : "1.5"
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
    const text = el("text", {
      x: p.x.toFixed(1),
      y: (p.y + n.r + 11).toFixed(1),
      "text-anchor": "middle",
      fill: STYLE.label,
      "font-size": "10",
      class: "node-label"
    });
    text.textContent = n.canonical_name || n.id;
    gLabels.appendChild(text);
  }
  return { project, pos, clusterPos };
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
  for (const e of scene.edges) {
    const a2 = pos.get(e.from);
    const b = pos.get(e.to);
    if (!a2 || !b) continue;
    ctx.save();
    ctx.globalAlpha = e.style.opacity;
    ctx.strokeStyle = e.style.possiblySame ? STYLE.edgePossiblySameColor : e.style.invalid ? STYLE.edgeInvalidColor : STYLE.edgeColor;
    ctx.lineWidth = e.style.possiblySame ? 2 : 1;
    if (e.style.dashed) ctx.setLineDash([4, 3]);
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
  return { project, pos };
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
  applyFiltersAndDraw();
  renderBanners();
  renderFilterPanel();
  renderTimeline();
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
  const size = stageSize();
  const useCanvas = shouldUseCanvas(state.scene, MAX_SVG);
  const host = { svg: dom.svg, canvas: dom.canvas };
  const result = render(host, state.scene, { ...size, useCanvas });
  state.project = (x2, y2) => result.project(x2, y2);
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
    parts.push(banner("error", `参数/请求错误${e.field ? `（${e.field}）` : ""}：${e.message}`));
  }
  if (Array.isArray(meta.degraded) && meta.degraded.length) {
    parts.push(banner("warn", `降级（meta.degraded，共 ${meta.degraded.length} 条）：${meta.degraded.join("；")}`));
  }
  if (meta.recall && Array.isArray(meta.recall.degraded) && meta.recall.degraded.length) {
    parts.push(banner("warn", `召回降级（recall.degraded）：${meta.recall.degraded.join("；")}`));
  }
  if (meta.recall && meta.recall.status && meta.recall.status !== "ok") {
    parts.push(banner("info", `召回状态：${meta.recall.status}（coverage=${meta.recall.coverage ?? "?"}，items=${meta.recall.items ?? "?"}）`));
  }
  if (meta.truncated) {
    parts.push(banner(
      "info",
      `已折叠，未显示全部：node_count=${meta.node_count}，rendered_nodes=${meta.rendered_nodes}，clusters=${meta.rendered_clusters ?? state.scene?.clusters.length ?? 0}，budget=${meta.budget}/${meta.budget_max}`
    ));
  }
  const reviewed = state.reviewed.size ? banner("ok", `本次已裁决 ${state.reviewed.size} 条 possiblySame 边（服务端为准，刷新后可见结果）`) : "";
  if (reviewed) parts.push(reviewed);
  dom.banners.innerHTML = parts.join("");
  dom.modeBadge.textContent = meta.mode ? `${meta.mode} · hops=${meta.hops ?? "-"} · budget=${meta.budget ?? "-"}` : "-";
}
function banner(kind, text) {
  const div = document.createElement("div");
  div.className = `banner ${kind}`;
  div.textContent = text;
  return div.outerHTML;
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
    for (const g of dom.svg.querySelectorAll("g.cluster")) {
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
      const cluster = hitTestCluster(state.scene, state.project, x2, y2);
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
  const h = document.createElement("h3");
  h.textContent = card.canonical_name || card.id;
  frag.appendChild(h);
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
  applyFiltersAndDraw();
  renderBanners();
  renderStageNote(buildStageNote());
});
dom.relationFilters.addEventListener("change", (ev) => {
  if (ev.target?.dataset?.role !== "relation") return;
  toggleFilter(state.relationFilter, ev.target.value, ev.target.checked);
  applyFiltersAndDraw();
  renderBanners();
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
  const div = document.createElement("div");
  div.className = `banner ${kind}`;
  div.textContent = text;
  dom.banners.appendChild(div);
  setTimeout(() => div.remove(), 8e3);
}
function drillIntoCluster(conceptId) {
  const candidate = state.baseScene?.nodes.find((n) => (n.concept_id || "") === (conceptId || ""));
  if (candidate) {
    focusNode(candidate.id);
    return;
  }
  flashBanner("info", `概念 ${conceptId || "(未归属概念)"} 在本次折叠里没有保留实体（折叠时只保留命中实体）；可提高 budget 或先搜索该概念下的实体。`);
}
function openReview(edgeId) {
  const edge = state.scene?.edges.find((e) => e.id === edgeId);
  if (!edge) return;
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
function confirmThenRun({ title, text, onConfirm }) {
  dom.confirmTitle.textContent = title;
  dom.confirmText.textContent = text;
  openConfirmModal().then((ok) => {
    if (ok === true) onConfirm();
  });
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
  dom.tooltip.textContent = `${e.relation} · ${e.from} → ${e.to} · weight=${e.weight} · t_valid=${e.t_valid}${e.t_invalid ? ` · t_invalid=${e.t_invalid}` : ""}` + (e.style.invalid ? " · 本次视角已失效（虚线保留）" : "") + (e.style.possiblySame ? " · 待审（点击可裁决）" : "");
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
window.addEventListener("popstate", () => {
  const { view, errors } = parseView(location.search);
  if (errors.length) {
    state.errors = errors;
    renderBanners();
    return;
  }
  loadView(view);
});
var resizeTimer = null;
window.addEventListener("resize", () => {
  clearTimeout(resizeTimer);
  resizeTimer = setTimeout(() => draw(), 120);
});
async function boot() {
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
if (typeof document !== "undefined" && document.getElementById("graph-svg")) {
  boot();
}
export {
  applyFiltersAndDraw,
  boot,
  buildStageNote,
  chooseVerdict,
  confirmThenRun,
  loadView,
  openReview,
  state,
  submitReview
};
//# sourceMappingURL=app.js.map
