/**
 * Links pressed while Home is being arranged open in a new tab.
 *
 * AN UNSAVED BOARD IS NOT LEFT FOR A LINK. A widget's rows, a preview in the
 * add sheet and the page's own sentence are all links, and following one here
 * would leave the editor: the leave guard would ask, and either answer costs
 * the reader something (their draft, or the page they wanted). A new tab
 * gives them both, so while arranging, every plain press on a link opens one.
 *
 * THE LINK ITSELF OPENS IT, as a link with `target="_blank"` does, rather than
 * `window.open`, which a sandboxed frame may refuse. The target is set for the
 * press and taken back after it, so the same link behaves as written once
 * editing ends. A press the reader already aimed somewhere (a modifier key, a
 * middle button, a link that opens a tab of its own) is left as it is.
 */

import type { MouseEvent } from "react";

export function openLinksInNewTab(event: MouseEvent<HTMLElement>): void {
  if (event.defaultPrevented || event.button !== 0) return;
  if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
  const link = (event.target as Element | null)?.closest?.("a[href]");
  if (!(link instanceof HTMLAnchorElement)) return;
  if (link.target === "_blank" || link.hasAttribute("download")) return;
  const was = { target: link.getAttribute("target"), rel: link.getAttribute("rel") };
  link.target = "_blank";
  link.rel = "noopener noreferrer";
  window.setTimeout(() => {
    if (was.target === null) link.removeAttribute("target");
    else link.setAttribute("target", was.target);
    if (was.rel === null) link.removeAttribute("rel");
    else link.setAttribute("rel", was.rel);
  }, 0);
}
