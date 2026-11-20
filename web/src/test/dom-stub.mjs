// 最小 DOM 打桩（供 ui/ 组件与 Recall 页的单测使用）。
//
// 只实现 ui/dom.js 与 recallpage.js 真正用到的那一小片 API：
// createElement / createTextNode / appendChild / textContent / setAttribute /
// dataset / classList / hidden / remove / value。
//
// 刻意**不**实现 innerHTML：真实代码里凡是拼 HTML 的地方都该被看见，
// 打桩替它兜底反而会掩盖"用了 innerHTML"这种安全回退。
class FakeNode {
  constructor(tagName) {
    this.tagName = tagName;
    this.nodeType = tagName === '#text' ? 3 : 1;
    this.attributes = new Map();
    this.children = [];
    this.dataset = {};
    this.style = {};
    this.hidden = false;
    this.value = '';
    this.className = '';
    this._text = '';
    this.parentNode = null;
    const self = this;
    this.classList = {
      _set: new Set(),
      add(...cs) {
        for (const c of cs) this._set.add(c);
        self.className = [...this._set].join(' ');
      },
      remove(...cs) {
        for (const c of cs) this._set.delete(c);
        self.className = [...this._set].join(' ');
      },
      contains(c) {
        return this._set.has(c) || String(self.className).split(/\s+/).includes(c);
      },
      toggle(c, force) {
        const on = force === undefined ? !this.contains(c) : Boolean(force);
        if (on) this.add(c);
        else this.remove(c);
        return on;
      },
    };
  }

  get textContent() {
    if (this.children.length === 0) return this._text;
    return this.children.map((c) => c.textContent).join('');
  }

  set textContent(v) {
    // 与真实 DOM 一致：写 textContent 会**清空**已有子节点。
    this._text = String(v ?? '');
    this.children = [];
  }

  appendChild(child) {
    if (child && child.parentNode) child.parentNode.removeChild(child);
    if (child) child.parentNode = this;
    this.children.push(child);
    return child;
  }

  removeChild(child) {
    const i = this.children.indexOf(child);
    if (i >= 0) this.children.splice(i, 1);
    if (child) child.parentNode = null;
    return child;
  }

  remove() {
    if (this.parentNode) this.parentNode.removeChild(this);
  }

  setAttribute(k, v) {
    this.attributes.set(k, String(v));
  }

  getAttribute(k) {
    return this.attributes.has(k) ? this.attributes.get(k) : null;
  }

  hasAttribute(k) {
    return this.attributes.has(k);
  }

  addEventListener() {}

  closest() {
    return null;
  }

  // all 深度优先收集（含自身），模拟 querySelectorAll 的用途。
  all(pred, out = []) {
    if (pred(this)) out.push(this);
    for (const c of this.children) c.all(pred, out);
    return out;
  }
}

// installDom 把 document 装到 globalThis 上，并返回工厂函数。
export function installDom() {
  const doc = {
    createElement: (tag) => new FakeNode(tag),
    createTextNode: (text) => {
      const n = new FakeNode('#text');
      n._text = String(text ?? '');
      return n;
    },
  };
  globalThis.document = doc;
  return doc;
}

// textOf 汇总一棵子树里的全部可见文本（断言"页面上出现了哪些字"）。
export function textOf(el) {
  return el.all(() => true).map((n) => n._text).filter(Boolean).join(' | ');
}

// findAll 深度优先查找满足条件的节点。
export function findAll(el, pred) {
  return el.all(pred);
}

export { FakeNode };
