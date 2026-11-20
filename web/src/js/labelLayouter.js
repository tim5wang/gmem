// 确定性标签避让：节点标签不再"每个都无条件画在节点下方"，而是按重要度排序、
// 逐个找空位，实在摆不下就不画。
//
// # 为什么需要它
//
// 原实现（render.js 的 node-label）对每个节点都画一行文字在圆心正下方 -11px 处，
// 没有任何避让。团内节点本身就是密集的，结果是标签互相压叠（"Atlas Design Doc"
// 压在 "Atlas" 上、"Alice W." 压在 "Alice" 上）—— 压叠之后两行字都读不出来，
// 比少画一行更糟。
//
// # 为什么不"只画前 N 个"或"只在悬停时显示"
//
// 那等于把信息藏起来，图上就只剩一堆没有名字的点，使用者要靠逐个悬停才能读图。
// 这里的取值是：按 **degree 降序**（重要节点优先保住名字，平局用 id 保证确定性）
// 逐个尝试上/下/右/左四个方位，冲突才隐藏；隐藏比例有断言兜底（≤40%）。
//
// # 纯函数
//
// 输入只有"节点屏幕坐标 + 半径 + 文本 + 画布尺寸"，输出只有坐标列表。
// 没有 Math.random、没有 DOM 测量、没有时间 —— 同一份数据每次画出来完全一致，
// 这样 node --test 才能把"标签不重叠"变成断言而不是靠肉眼。
//
// 只有团徽位置例外地来自 geometry.js：那里是"团徽让位"的同一份确定性实现，
// 障碍框必须按**实际画出来的位置**算，否则节点标签会去压真正画出去的那行团名。
import { placeClusterBadges } from './geometry.js';

// LABEL 是与 render.js 共用的字号/估算常量。
//
// 文本宽度靠**估算**而不是 DOM 测量：measureText 需要真实布局引擎，一旦用了，
// 这个函数就不再是纯函数，也没法在 node --test 里跑。误差（把西文按 0.6em、
// CJK 按 1.75em 计）方向是"略微偏大"，宁可少放一个标签也不压叠。
export const LABEL = {
  fontSize: 10,
  lineHeight: 12,
  // CJK / 全角字符按 1.75em 计（10px 字号 → 17.5px/字）；
  latinWidth: 0.6,
  cjkWidth: 1.75,
  // 文本框之间额外留 2px，避免"刚好贴边"看起来仍像压叠。
  gap: 2,
  // 候选方位的外扩倍数：先贴近节点，贴不下再往外挪一档（仍是同样四个方位）。
  rings: [1, 1.9],
};

// textWidth 估算字符串的渲染宽度（像素）。
export function textWidth(text, fontSize = LABEL.fontSize) {
  let w = 0;
  for (const ch of text) {
    const code = ch.codePointAt(0);
    // 0x2E80 以下是拉丁/标点，按比例宽度；以上按全角宽。
    w += (code > 0x2e80 ? LABEL.cjkWidth : LABEL.latinWidth) * fontSize;
  }
  return w;
}

// candidate 返回某个方位的文本框（屏幕坐标，轴对齐包围盒）。
//
// anchor 决定 render.js 的 text-anchor：
//   below / above → 'middle'（水平居中于节点）
//   right / left  → 'start' / 'end'
// 返回值里的 x/y 是**文本基线的落点**，box 是估算包围盒。
function candidate(item, anchor, ring, fontSize) {
  const h = LABEL.lineHeight;
  const ascent = fontSize * 0.8;
  const descent = fontSize * 0.2;
  const w = item.w;
  const gapPx = (LABEL.gap + 2) * ring;
  let x;
  let y;
  if (anchor === 'below') {
    x = item.x;
    y = item.y + item.r + LABEL.fontSize * 0.9 + gapPx;
  } else if (anchor === 'above') {
    x = item.x;
    y = item.y - item.r - gapPx;
  } else if (anchor === 'right') {
    x = item.x + item.r + gapPx + 2;
    y = item.y + LABEL.fontSize * 0.35;
  } else {
    x = item.x - item.r - gapPx - 2;
    y = item.y + LABEL.fontSize * 0.35;
  }
  let minX;
  if (anchor === 'below' || anchor === 'above') {
    minX = x - w / 2;
  } else if (anchor === 'right') {
    minX = x;
  } else {
    minX = x - w;
  }
  return {
    x,
    y,
    anchor,
    minX,
    minY: y - ascent,
    maxX: minX + w,
    maxY: y - ascent + h,
  };
}

// overlaps 判定两个包围盒是否相交（含 gap 的矩形扩张）。
function overlaps(a, b, gap) {
  return (
    a.minX - gap < b.maxX &&
    b.minX - gap < a.maxX &&
    a.minY - gap < b.maxY &&
    b.minY - gap < a.maxY
  );
}

// 空间哈希：把已摆放的矩形按格子分桶，查询只看邻域 9 格。
//
// 为什么要它：3000 个节点的密集图，逐对比较是 O(n²)（900 万次），
// 而验收要求"3000 节点下标签避让 ≤ 200ms"。分桶后每次查询是常数级。
const CELL = 64;

function cellKey(x, y) {
  return `${Math.floor(x / CELL)},${Math.floor(y / CELL)}`;
}

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

// obstaclesOf 把"已经画在图上的文字"（团标签）变成避让障碍。
//
// 团标签是团名 + 计数，比单个节点名更重要（折叠视图里它是唯一的解释），
// 因此先占位、节点标签绕开它。
//
// 位置必须与 render.js 实际画的位置**同口径**：团徽在概览下会被让位到节点云外侧
// （placeClusterBadges），如果障碍框仍按"团心 + 圆下 13px"估算，节点标签就会去压
// 真正画出来的那行字。所以这里直接复用同一个布置函数。
export function clusterObstacles(scene, project, { fontSize = 11, width = 0, height = 0 } = {}) {
  const badges = placeClusterBadges(scene, project, { width, height });
  const out = [];
  for (const c of scene.clusters || []) {
    const key = c.id || c.concept_id || '';
    const b = badges.get(key);
    if (!b) continue;
    const text = `${c.concept_name || c.id || '(未归属概念)'} · ${c.count}`;
    const w = textWidth(text, fontSize);
    out.push({
      minX: b.x - w / 2,
      minY: b.labelY - fontSize * 0.8,
      maxX: b.x + w / 2,
      maxY: b.labelY - fontSize * 0.8 + LABEL.lineHeight,
    });
    // 团徽圆盘本身也是障碍：节点标签压在圆盘上同样读不出。
    out.push({
      minX: b.x - b.r,
      minY: b.y - b.r,
      maxX: b.x + b.r,
      maxY: b.y + b.r,
    });
  }
  return out;
}

// layoutLabels 是避让入口。
//
// items: [{ id, x, y, r, text, degree }] —— 屏幕坐标
// opts:  { width, height, fontSize, obstacles }
//
// 返回 { labels: [{ id, x, y, anchor, text }], hidden: [id], shown: n, total: n }
// 输出的 labels 顺序 = 摆放顺序（degree 降序），渲染层直接按它画即可。
export function layoutLabels(items, { width = 0, height = 0, fontSize = LABEL.fontSize, obstacles = [] } = {}) {
  const grid = new Map();
  for (const o of obstacles) insert(grid, o);

  // 重要性排序：degree 降序，平局用 id 升序 —— 完全确定，不依赖输入顺序。
  const ordered = items
    .filter((it) => it && Number.isFinite(it.x) && Number.isFinite(it.y))
    .map((it) => ({
      id: it.id,
      x: it.x,
      y: it.y,
      r: Number.isFinite(it.r) ? it.r : 5,
      degree: Number.isFinite(it.degree) ? it.degree : 0,
      text: String(it.text ?? it.id ?? ''),
    }))
    .map((it) => ({ ...it, w: textWidth(it.text, fontSize) }))
    .sort((a, b) => (b.degree - a.degree) || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));

  const labels = [];
  const hidden = [];
  for (const it of ordered) {
    let placed = null;
    for (const ring of LABEL.rings) {
      for (const anchor of ['below', 'above', 'right', 'left']) {
        const c = candidate(it, anchor, ring, fontSize);
        // 出画布的候选位也算冲突：宁可换方位，也不要画出被 viewBox 切掉半个字。
        if (width > 0 && (c.minX < 1 || c.maxX > width - 1)) continue;
        if (height > 0 && (c.minY < 1 || c.maxY > height - 1)) continue;
        if (collides(grid, c, LABEL.gap)) continue;
        placed = c;
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
