// Minimal DOM helpers + event delegation.

export const $ = (sel, root = document) => root.querySelector(sel);
export const $$ = (sel, root = document) => [...root.querySelectorAll(sel)];

// Tag template that just concatenates — interpolate pre-escaped strings
// (via esc()) or numbers. Keeps render functions readable as HTML.
export function html(strings, ...values) {
  let out = strings[0];
  for (let i = 0; i < values.length; i++) {
    const v = values[i];
    out += (Array.isArray(v) ? v.join("") : v == null ? "" : v) + strings[i + 1];
  }
  return out;
}

export function setHTML(el, markup) {
  if (el) el.innerHTML = markup;
  return el;
}

// Event delegation: one listener per (root, type); dispatch by matching
// the closest element carrying `data-{type}-action` or `data-action`.
export function delegate(root, type, handler) {
  root.addEventListener(type, (ev) => {
    const target = ev.target.closest("[data-action]");
    if (target && root.contains(target)) {
      handler(target.dataset.action, target, ev);
    }
  });
}

export function copyToClipboard(text) {
  if (navigator.clipboard?.writeText) {
    return navigator.clipboard.writeText(text);
  }
  const ta = document.createElement("textarea");
  ta.value = text;
  ta.style.position = "fixed";
  ta.style.opacity = "0";
  document.body.appendChild(ta);
  ta.select();
  try {
    document.execCommand("copy");
  } finally {
    ta.remove();
  }
  return Promise.resolve();
}

let toastTimer = 0;
export function toast(message, kind = "ok") {
  const wrap = $("#toasts");
  if (!wrap) return;
  const el = document.createElement("div");
  el.className = "toast " + kind;
  el.textContent = message;
  wrap.appendChild(el);
  clearTimeout(toastTimer);
  setTimeout(() => el.remove(), 2600);
}
