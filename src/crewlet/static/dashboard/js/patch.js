// In-place DOM patching — the reason this dashboard stops flickering.
//
// Every view used to render with `root.innerHTML = markup`, and the
// store re-rendered the active view on *every* envelope: once per LLM
// round while a turn was running, plus a health tick every five seconds
// when nothing was happening at all. Replacing innerHTML throws away
// every node and builds new ones, which:
//
//   * restarted the container's entrance animation, so the page visibly
//     strobed rather than updated — the "it refreshes multiple times"
//     report;
//   * reset scroll position inside any scrollable panel;
//   * dropped text selection, focus, and hover state mid-read;
//   * discarded expanded rows and re-collapsed them.
//
// `patch` takes the same markup those views already produce and walks it
// against the live DOM, touching only what actually differs. A row whose
// text has not changed is not written to, so the browser does no layout
// for it and nothing about it visually resets.
//
// Lists are matched by an explicit `data-k` key rather than by position,
// so an event arriving at the top of a feed inserts one node instead of
// rewriting every row below it. A keyed node that is genuinely new is
// tagged `.is-entering` for one frame, which is how arrival animation
// happens now: on the row that arrived, not on the whole screen.

// How long the entrance marker stays on a newly inserted keyed node.
// Long enough for the CSS arrival animation to run to completion
// (styles/base.css `row-in`, 260ms) with a frame of slack, short enough
// that a node is never still marked when its next update lands.
const ENTER_MS = 320;

// Attributes that must never be patched from markup because the live
// value is user state, not render state: replacing them would fight the
// person typing or dragging.
const LIVE_PROPS = new Set(["value", "checked", "selected"]);

// A fresh <template> per parse rather than one reused across calls.
// Reuse looks like the obvious optimisation and is a portability trap:
// `content` is a live fragment in a browser but a cached one in some DOM
// implementations, so a reused template silently keeps re-parsing to the
// FIRST markup it was ever given and every later patch becomes a no-op.
// Creating the element costs microseconds against the DOM work that
// follows, and it keeps this module importable — and testable — outside
// a browser.
function parse(markup) {
  const template = document.createElement("template");
  template.innerHTML = markup;
  return template.content;
}

/**
 * Patch `parent`'s children to match `markup`, in place.
 * Returns `parent`.
 */
export function patch(parent, markup) {
  if (!parent) return parent;
  patchChildren(parent, parse(markup));
  return parent;
}

function keyOf(node) {
  return node.nodeType === 1 ? node.getAttribute("data-k") : null;
}

function patchChildren(oldParent, newParent) {
  const oldNodes = [...oldParent.childNodes];
  const newNodes = [...newParent.childNodes];

  // Index the existing keyed children so a reordered or newly-prepended
  // row reuses its own node instead of overwriting whichever node
  // happened to sit at that index.
  const keyed = new Map();
  for (const node of oldNodes) {
    const key = keyOf(node);
    if (key !== null && !keyed.has(key)) keyed.set(key, node);
  }

  // Every node the new markup accounted for, whether reused or created.
  // Removal is "everything that survived and was not claimed", which is
  // the only rule that stays correct when two siblings share a key: the
  // second one can never be matched (the index holds the first), so a
  // rule that removes only *indexed* leftovers would leave it in place
  // AND insert a replacement — leaking one node per render, forever.
  // Views should not emit a duplicate key, but a key derived from data
  // can collide, and a slow leak is a bad way to find out.
  const claimed = new Set();

  let cursor = oldParent.firstChild;
  for (const wanted of newNodes) {
    const key = keyOf(wanted);
    let match = null;

    if (key !== null) {
      const candidate = keyed.get(key);
      if (candidate !== undefined && !claimed.has(candidate)) {
        match = candidate;
        keyed.delete(key);
      }
    } else if (
      cursor &&
      keyOf(cursor) === null &&
      !claimed.has(cursor) &&
      compatible(cursor, wanted)
    ) {
      // Unkeyed nodes match positionally, but only against a compatible
      // node — swapping a <div> for a <span> in place would leave the
      // wrong element type carrying the right attributes.
      match = cursor;
    }

    if (match === null) {
      const fresh = wanted.cloneNode(true);
      oldParent.insertBefore(fresh, cursor);
      claimed.add(fresh);
      if (key !== null) markEntering(fresh);
      continue;
    }

    claimed.add(match);
    if (match !== cursor) {
      oldParent.insertBefore(match, cursor);
    } else {
      cursor = cursor.nextSibling;
    }
    patchNode(match, wanted);
  }

  for (const node of oldNodes) {
    if (!claimed.has(node) && node.parentNode === oldParent) node.remove();
  }
}

function compatible(a, b) {
  if (a.nodeType !== b.nodeType) return false;
  if (a.nodeType !== 1) return true;
  return a.tagName === b.tagName;
}

function patchNode(oldNode, newNode) {
  if (!compatible(oldNode, newNode)) {
    oldNode.replaceWith(newNode.cloneNode(true));
    return;
  }
  if (oldNode.nodeType !== 1) {
    // Text / comment: write only on a real difference, so an unchanged
    // label keeps any selection the reader has on it.
    if (oldNode.nodeValue !== newNode.nodeValue) {
      oldNode.nodeValue = newNode.nodeValue;
    }
    return;
  }
  patchAttrs(oldNode, newNode);
  patchChildren(oldNode, newNode);
}

function patchAttrs(oldEl, newEl) {
  // The entrance marker is patch-owned, never render-owned, so it has to
  // survive an update that lands mid-animation — including one whose
  // markup carries no `class` at all, which the removal sweep below
  // would otherwise strip along with the class attribute.
  const entering = oldEl.classList.contains("is-entering");

  const wanted = newEl.attributes;
  for (let i = 0; i < wanted.length; i++) {
    const { name, value } = wanted[i];
    if (LIVE_PROPS.has(name) && oldEl === document.activeElement) continue;
    if (name === "class") {
      const next = entering ? `${value} is-entering` : value;
      if (oldEl.getAttribute("class") !== next) oldEl.setAttribute("class", next);
      continue;
    }
    if (oldEl.getAttribute(name) !== value) oldEl.setAttribute(name, value);
  }

  // Remove attributes the new markup dropped.
  const existing = oldEl.attributes;
  for (let i = existing.length - 1; i >= 0; i--) {
    const name = existing[i].name;
    if (!newEl.hasAttribute(name)) oldEl.removeAttribute(name);
  }
  if (entering) oldEl.classList.add("is-entering");
}

function markEntering(node) {
  if (node.nodeType !== 1) return;
  node.classList.add("is-entering");
  setTimeout(() => node.classList.remove("is-entering"), ENTER_MS);
}
