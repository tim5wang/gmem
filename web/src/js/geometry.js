// 坐标换算：世界坐标（服务端给的 ~1 量级）→ 屏幕像素。
//
// 抽成纯函数是为了让渲染分档、折叠团位置这些几何结论可以被 node --test 覆盖，
// 不必起浏览器。

// bounds 返回场景内容的包围盒（含半径，避免边缘节点被裁掉）。
export function bounds(scene) {
  let minX = Infinity;
  let minY = Infinity;
  let maxX = -Infinity;
  let maxY = -Infinity;
  const consider = (x, y, r) => {
    if (!Number.isFinite(x) || !Number.isFinite(y)) return;
    if (x - r < minX) minX = x - r;
    if (y - r < minY) minY = y - r;
    if (x + r > maxX) maxX = x + r;
    if (y + r > maxY) maxY = y + r;
  };
  for (const n of scene.nodes) consider(n.x, n.y, 0.02);
  for (const c of scene.clusters) consider(c.x, c.y, 0.09);
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

// clusterPixelRadius 是团圆在**屏幕上的**半径（像素）：只吃 count，与缩放无关。
//
// 单独抽出来的理由：视口适配必须知道"内容真正画多大"。旧实现只按世界坐标的
// 固定半径（节点 0.02、团 0.09）估算包围盒，而团圆实际有 10~40px、节点最大
// 22px —— 缩放一大，真实图形就会越过估算边界，于是内容贴边甚至被裁掉，
// 而留白全堆在另一侧（用户看到的"左上方大片空白"）。
export function clusterPixelRadius(count) {
  return 10 + Math.min(30, Math.sqrt(Math.max(1, count)) * 3.2);
}

// drawExtent 返回所有图元中最大的**屏幕**半径（像素），用于给缩放留出余量。
export function drawExtent(scene) {
  let r = 0;
  for (const n of scene.nodes) {
    const nr = Number.isFinite(n.r) ? n.r : 5;
    if (nr > r) r = nr;
  }
  for (const c of scene.clusters || []) {
    const cr = clusterPixelRadius(c.count);
    if (cr > r) r = cr;
  }
  // 描边/红描边是画在圆外的（2.5px），多留 3px 免得压到 padding 边上。
  return r + 3;
}

// makeProjector 构造 世界→屏幕 的等比映射（padding 像素内边距）。
//
// 两个不变量（都由 geometry.test.mjs 钉死）：
//  1. **等比**：x、y 用同一个 scale，图形不变形；
//  2. **居中且不越界**：内容按世界包围盒中心对齐视口中心，并且把
//     drawExtent(scene)（图元自己的像素半径）从可用宽高里扣掉，
//     因此边缘图元整个落在画布内 —— 左右/上下留白自然对称。
//
// 为什么不拉伸到"填满视口"：非等比缩放会把圆压成椭圆、把团心几何关系改掉，
// 视觉编码（大小 = degree）随之失真。宽视口下圆形内容两侧必然留白，这是
// 等比映射的代价，不是缺陷；能改进的是"别把内容顶到边上、别一侧紧一侧空"。
export function makeProjector(scene, width, height, padding = 48) {
  const b = bounds(scene);
  const dx = b.maxX - b.minX;
  const dy = b.maxY - b.minY;
  // 可用区域先扣 padding，再扣图元最大像素半径（它不随缩放变化）。
  const pad = Math.max(0, padding);
  const ext = Math.min(drawExtent(scene), Math.min(width, height) / 4);
  const w = Math.max(1, width - pad * 2 - ext * 2);
  const h = Math.max(1, height - pad * 2 - ext * 2);
  const scale = Math.min(w / dx, h / dy);
  const cx = (b.minX + b.maxX) / 2;
  const cy = (b.minY + b.maxY) / 2;
  const project = (x, y) => ({
    x: width / 2 + (x - cx) * scale,
    y: height / 2 + (y - cy) * scale,
  });
  project.scale = scale;
  return project;
}

// contentBox 返回内容在屏幕上的实际包围盒（含图元半径）。
//
// 与 bounds 的区别：这是**画完真的占了多少像素**，用于断言"居中/不越界/填充率"。
export function contentBox(scene, project) {
  let minX = Infinity;
  let minY = Infinity;
  let maxX = -Infinity;
  let maxY = -Infinity;
  const consider = (x, y, r) => {
    if (x - r < minX) minX = x - r;
    if (y - r < minY) minY = y - r;
    if (x + r > maxX) maxX = x + r;
    if (y + r > maxY) maxY = y + r;
  };
  for (const n of scene.nodes) {
    const p = project(n.x, n.y);
    if (!Number.isFinite(p.x) || !Number.isFinite(p.y)) continue;
    consider(p.x, p.y, Number.isFinite(n.r) ? n.r : 5);
  }
  for (const c of scene.clusters || []) {
    const p = project(c.x, c.y);
    if (!Number.isFinite(p.x) || !Number.isFinite(p.y)) continue;
    consider(p.x, p.y, clusterPixelRadius(c.count));
  }
  if (!Number.isFinite(minX)) return null;
  return { minX, minY, maxX, maxY, width: maxX - minX, height: maxY - minY };
}

// shouldUseCanvas 是渲染分档判定（TD §9：SVG ≤ 2k，超过切 Canvas）。
//
// 判决依据是**实际要画的图元数**（节点 + 折叠团），不是 node_count：
// overview 下 node_count 可能上千但 rendered_nodes 为 0（全进 clusters），
// 那时画的是几个团，用 Canvas 反而看不清标签。
export function shouldUseCanvas(scene, maxSvg = 2000) {
  return scene.nodes.length + scene.clusters.length > maxSvg;
}
