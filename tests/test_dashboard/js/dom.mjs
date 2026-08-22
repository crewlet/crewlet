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
  // `api.js` reads `location.origin` at module scope, so importing it
  // needs one to exist. Kept here rather than in each suite: a global
  // the browser always provides belongs with the rest of the fake
  // browser, not sprinkled across the tests that trip over it.
  if (!globalThis.location) globalThis.location = { origin: "http://localhost" };
  return { document, root };
}

/**
 * Install a session history, a location that writes to it, and a window
 * that reports scroll — enough to test a router.
 *
 * The dashboard's router had no test at all, and the defects that cost
 * were not parsing bugs: a redirect that PUSHED made Back re-run the
 * redirect for ever, sub-sections that REPLACED made Back leave the room
 * entirely, and every mount reset the scroll so Back landed at the top of
 * a list the reader had scrolled halfway down. None of those is visible
 * in a parse; all of them are visible in a stack of entries.
 *
 * Modelled faithfully on the two rules that produce those bugs:
 *   - assigning `location.hash` PUSHES an entry whose state is null;
 *   - `history.replaceState` overwrites the current entry in place and
 *     does NOT fire `hashchange`.
 */
export function installHistory(initial = "#/") {
  const entries = [{ url: initial, state: null }];
  let index = 0;
  const listeners = { hashchange: [], popstate: [], scroll: [] };
  let scrollY = 0;

  const fire = (type) => {
    for (const fn of [...listeners[type]]) fn({ type });
  };

  const location = {
    origin: "http://localhost",
    get hash() {
      return entries[index].url;
    },
    set hash(value) {
      const url = value.startsWith("#") ? value : `#${value}`;
      if (url === entries[index].url) return;
      entries.length = index + 1;
      entries.push({ url, state: null });
      index += 1;
      fire("hashchange");
    },
  };

  const history = {
    scrollRestoration: "auto",
    get state() {
      return entries[index].state;
    },
    get length() {
      return entries.length;
    },
    pushState(state, _title, url) {
      entries.length = index + 1;
      entries.push({ url: url ?? entries[index].url, state });
      index += 1;
    },
    replaceState(state, _title, url) {
      entries[index] = { url: url ?? entries[index].url, state };
    },
    go(delta) {
      const next = Math.min(entries.length - 1, Math.max(0, index + delta));
      if (next === index) return;
      index = next;
      fire("popstate");
      fire("hashchange");
    },
    back() {
      this.go(-1);
    },
    forward() {
      this.go(1);
    },
  };

  globalThis.location = location;
  globalThis.history = history;
  // Router code says `window.addEventListener`, not the bare global. In
  // a browser they are the same object; here they have to be made so, or
  // every listener the router registers goes somewhere nothing fires.
  globalThis.window = globalThis;
  globalThis.dispatchEvent = (event) => {
    for (const fn of [...(listeners[event.type] || [])]) fn(event);
    return true;
  };
  globalThis.addEventListener = (type, fn) => {
    (listeners[type] || (listeners[type] = [])).push(fn);
  };
  globalThis.removeEventListener = (type, fn) => {
    const list = listeners[type];
    if (!list) return;
    const at = list.indexOf(fn);
    if (at >= 0) list.splice(at, 1);
  };
  globalThis.scrollTo = (_x, y) => {
    scrollY = typeof y === "number" ? y : (y && y.top) || 0;
  };
  Object.defineProperty(globalThis, "scrollY", { get: () => scrollY, configurable: true });
  globalThis.requestAnimationFrame = (fn) => setTimeout(() => fn(Date.now()), 0);
  globalThis.cancelAnimationFrame = (id) => clearTimeout(id);
  globalThis.HashChangeEvent = class {
    constructor(type) {
      this.type = type;
    }
  };

  return {
    location,
    history,
    /** Every URL in the stack, oldest first, with a caret on the current. */
    stack: () => entries.map((e, i) => (i === index ? `>${e.url}` : ` ${e.url}`)),
    urls: () => entries.map((e) => e.url),
    index: () => index,
    setScroll: (y) => {
      scrollY = y;
    },
    scroll: () => scrollY,
  };
}
