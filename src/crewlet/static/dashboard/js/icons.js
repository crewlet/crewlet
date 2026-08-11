// Icon helper — references the inline <symbol> sprite in index.html.

export function icon(id, cls = "") {
  return `<span class="ic ${cls}"><svg><use href="#i-${id}"/></svg></span>`;
}
