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

// makeProjector 构造 世界→屏幕 的等比映射（padding 像素内边距）。
export function makeProjector(scene, width, height, padding = 48) {
  const b = bounds(scene);
  const w = Math.max(1, width - padding * 2);
  const h = Math.max(1, height - padding * 2);
  const scale = Math.min(w / (b.maxX - b.minX), h / (b.maxY - b.minY));
  const cx = (b.minX + b.maxX) / 2;
  const cy = (b.minY + b.maxY) / 2;
  const project = (x, y) => ({
    x: width / 2 + (x - cx) * scale,
    y: height / 2 + (y - cy) * scale,
  });
  project.scale = scale;
  return project;
}

// shouldUseCanvas 是渲染分档判定（TD §9：SVG ≤ 2k，超过切 Canvas）。
//
// 判决依据是**实际要画的图元数**（节点 + 折叠团），不是 node_count：
// overview 下 node_count 可能上千但 rendered_nodes 为 0（全进 clusters），
// 那时画的是几个团，用 Canvas 反而看不清标签。
export function shouldUseCanvas(scene, maxSvg = 2000) {
  return scene.nodes.length + scene.clusters.length > maxSvg;
}
