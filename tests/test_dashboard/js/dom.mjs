// A minimal DOM, enough to exercise the dashboard's patcher.
//
// The dashboard is a zero-build ES-module app: no bundler, no npm, no
// node_modules. Its patcher is nonetheless load-bearing — it decides
// which nodes survive a re-render, which is the whole reason the page
// stopped rebuilding itself on every websocket envelope — so it needs
// real tests. Vendoring the handful of DOM surfaces it touches keeps
// that possible without introducing a JavaScript toolchain to a Python
// project, and without a network install in CI.
//
// This models the algorithm's contract, not a browser: parsing is
// deliberately simple (well-formed markup, no entities beyond the few
// the dashboard emits, no scripts). Anything the patcher does not use is
// absent on purpose rather than half-implemented.

const VOID_TAGS = new Set([
  "area",
  "base",
  "br",
  "col",
  "embed",
  "hr",
  "img",
  "input",
  "link",
  "meta",
  "source",
  "track",
  "wbr",
]);

const ENTITIES = {
  "&amp;": "&",
  "&lt;": "<",
  "&gt;": ">",
  "&quot;": '"',
  "&#39;": "'",
};

function decode(text) {
  return text.replace(/&(amp|lt|gt|quot|#39);/g, (m) => ENTITIES[m] ?? m);
}

class ClassList {
  constructor(el) {
    this.el = el;
  }
  _list() {
    return (this.el.getAttribute("class") || "").split(/\s+/).filter(Boolean);
  }
  contains(name) {
    return this._list().includes(name);
  }
  add(name) {
    const list = this._list();
    if (!list.includes(name)) {
      list.push(name);
      this.el.setAttribute("class", list.join(" "));
    }
  }
  remove(name) {
    this.el.setAttribute("class", this._list().filter((c) => c !== name).join(" "));
  }
}

class DomNode {
  constructor(nodeType) {
    this.nodeType = nodeType;
    this.childNodes = [];
    this.parentNode = null;
    this.nodeValue = null;
  }

  get firstChild() {
    return this.childNodes[0] || null;
  }

  get nextSibling() {
    if (!this.parentNode) return null;
    const kids = this.parentNode.childNodes;
    return kids[kids.indexOf(this) + 1] || null;
  }

  get children() {
    return this.childNodes.filter((n) => n.nodeType === 1);
  }

  get textContent() {
    if (this.nodeType !== 1) return this.nodeValue || "";
    return this.childNodes.map((n) => n.textContent).join("");
  }

  insertBefore(node, ref) {
    if (node.parentNode) node.parentNode.removeChild(node);
    const at = ref ? this.childNodes.indexOf(ref) : this.childNodes.length;
    this.childNodes.splice(at < 0 ? this.childNodes.length : at, 0, node);
    node.parentNode = this;
    return node;
  }

  appendChild(node) {
    return this.insertBefore(node, null);
  }

  removeChild(node) {
    const at = this.childNodes.indexOf(node);
    if (at >= 0) this.childNodes.splice(at, 1);
    node.parentNode = null;
    return node;
  }

  remove() {
    if (this.parentNode) this.parentNode.removeChild(this);
  }

  replaceWith(node) {
    if (!this.parentNode) return;
    this.parentNode.insertBefore(node, this);
    this.remove();
  }

  cloneNode() {
    if (this.nodeType !== 1) {
      const copy = new DomNode(this.nodeType);
      copy.nodeValue = this.nodeValue;
      return copy;
    }
    const copy = new Element(this.tagName);
    for (const { name, value } of this.attributes) copy.setAttribute(name, value);
    for (const kid of this.childNodes) copy.appendChild(kid.cloneNode(true));
    return copy;
  }
}

class Element extends DomNode {
  constructor(tagName) {
    super(1);
    this.tagName = tagName.toUpperCase();
    this._attrs = new Map();
    this.classList = new ClassList(this);
  }

  get attributes() {
    return [...this._attrs].map(([name, value]) => ({ name, value }));
  }

  getAttribute(name) {
    return this._attrs.has(name) ? this._attrs.get(name) : null;
  }
  setAttribute(name, value) {
    this._attrs.set(name, String(value));
  }
  removeAttribute(name) {
    this._attrs.delete(name);
  }
  hasAttribute(name) {
    return this._attrs.has(name);
  }

  set innerHTML(markup) {
    this.childNodes = [];
    for (const node of parseFragment(markup)) this.appendChild(node);
  }

  get innerHTML() {
    return this.childNodes.map(serialize).join("");
  }

  get content() {
    // <template>.content — a fresh fragment view of the parsed children,
    // matching the browser closely enough for the patcher's use.
    const fragment = new DomNode(11);
    fragment.childNodes = this.childNodes;
    for (const kid of this.childNodes) kid.parentNode = fragment;
    return fragment;
  }

  querySelector(selector) {
    const match = /^\[([^=\]]+)="([^"]*)"\]$/.exec(selector);
    const byClass = /^\.([\w-]+)$/.exec(selector);
    const walk = (node) => {
      for (const kid of node.childNodes) {
        if (kid.nodeType === 1) {
          if (match && kid.getAttribute(match[1]) === match[2]) return kid;
          if (byClass && kid.classList.contains(byClass[1])) return kid;
          const found = walk(kid);
          if (found) return found;
        }
      }
      return null;
    };
    return walk(this);
  }
}

function serialize(node) {
  if (node.nodeType !== 1) return node.nodeValue || "";
  const attrs = node.attributes
    .map(({ name, value }) => ` ${name}="${value}"`)
    .join("");
  const tag = node.tagName.toLowerCase();
  if (VOID_TAGS.has(tag)) return `<${tag}${attrs}>`;
  return `<${tag}${attrs}>${node.childNodes.map(serialize).join("")}</${tag}>`;
}

// A small, forgiving parser: tags, attributes, text. Enough for the
// markup the dashboard's render functions produce.
function parseFragment(markup) {
  const root = new DomNode(11);
  let cursor = root;
  let index = 0;
  const source = String(markup);

  while (index < source.length) {
    const next = source.indexOf("<", index);
    if (next < 0) {
      addText(cursor, source.slice(index));
      break;
    }
    if (next > index) addText(cursor, source.slice(index, next));
    const close = source.indexOf(">", next);
    if (close < 0) break;
    const raw = source.slice(next + 1, close).trim();
    index = close + 1;

    if (raw.startsWith("/")) {
      if (cursor.parentNode) cursor = cursor.parentNode;
      continue;
    }
    const selfClosing = raw.endsWith("/");
    const body = selfClosing ? raw.slice(0, -1).trim() : raw;
    const space = body.search(/\s/);
    const tagName = (space < 0 ? body : body.slice(0, space)).toLowerCase();
    const el = new Element(tagName);
    if (space >= 0) {
      for (const [, name, quoted, bare] of body
        .slice(space)
        .matchAll(/([\w:-]+)(?:=(?:"([^"]*)"|([^\s"]+)))?/g)) {
        el.setAttribute(name, decode(quoted ?? bare ?? ""));
      }
    }
    cursor.appendChild(el);
    if (!selfClosing && !VOID_TAGS.has(tagName)) cursor = el;
  }
  return [...root.childNodes];
}

function addText(parent, text) {
  if (!text) return;
  const node = new DomNode(3);
  node.nodeValue = decode(text);
  parent.appendChild(node);
}

/** Install a document object with a single `#root` element. */
export function installDom() {
  const root = new Element("div");
  root.setAttribute("id", "root");
  const document = {
    createElement: (tag) => new Element(tag),
    activeElement: null,
  };
  globalThis.document = document;
  return { document, root };
}
