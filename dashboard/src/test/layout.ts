/**
 * What the layout suites (`*.layout.test.tsx`, `vitest.layout.config.ts`)
 * share: the app's stylesheets, in `main.tsx`'s order and placed where a built
 * page places them, and the measurement of what a reader would have to scroll
 * sideways to see.
 *
 * THE APP'S SHEET COMES LAST, as in a built page: the build moves `main.tsx`'s
 * sheets (the kit's among them) to the end of the body (`vite.config.ts`'s
 * `appSheetsToBodyEnd`), after every lazy screen's sheet in the head, so a
 * screen's rule loses to an app rule of equal specificity. The dev server
 * injects in import order, the other way round, so a suite would pass on what
 * a reader never sees. A screen's own sheet is imported by the suite (or the
 * module it mounts) after this one, and lands in the head, before the app's.
 */
import "@crewlethq/tokens/css";
import "@crewlethq/tokens/css/themes";
import "@crewlethq/tokens/css/themes/all";
import "@crewlethq/tokens/css/density";
import "@crewlethq/tokens/css/fonts";
import "@crewlethq/tokens/css/base";
import "@crewlethq/tokens/css/brand-scope";
import "virtual:crewlet-design-system.css";
import "~/styles/tokens.css";
import "~/styles/base.css";
import "~/styles/components.css";
import "~/styles/frame.css";
import "~/styles/screens.css";

const appSheets = new Set(document.querySelectorAll("style[data-vite-dev-id]"));
for (const sheet of appSheets) document.body.append(sheet);
// A SCREEN'S SHEET, loaded later, goes to the head, before the app's: the dev
// server inserts it after the last sheet it inserted, which is now the app's.
new MutationObserver((records) => {
  for (const record of records) {
    for (const node of record.addedNodes) {
      if (node instanceof HTMLStyleElement && node.dataset.viteDevId && !appSheets.has(node)) {
        document.head.append(node);
      }
    }
  }
}).observe(document.body, { childList: true });

const REPLACED = new Set([
  "IMG",
  "VIDEO",
  "CANVAS",
  "IFRAME",
  "INPUT",
  "SELECT",
  "TEXTAREA",
  "BUTTON",
]);
const label = (el: Element) => `${el.tagName.toLowerCase()}.${[...el.classList].join(".")}`;

/** True when `el`, or an ancestor below `box`, scrolls or clips its own overflow. */
function ownScroller(el: Element | null, box: Element): boolean {
  for (let up = el; up && up !== box; up = up.parentElement) {
    if (getComputedStyle(up).overflowX !== "visible") return true;
  }
  return false;
}

/**
 * A glyph, or a box that is not a run of text, past `box`'s padding edge and
 * outside a scroller of its own. A run of text is measured by its glyphs: a
 * browser leaves a broken chip's cloned end inset out of line breaking.
 */
export function overflowing(box: Element): string[] {
  const right = box.getBoundingClientRect().left + box.clientLeft + box.clientWidth + 0.5;
  const found: string[] = [];
  for (const el of box.querySelectorAll("*")) {
    const run =
      getComputedStyle(el).display === "inline" &&
      !(el instanceof SVGElement) &&
      !REPLACED.has(el.tagName);
    if (!run && el.getBoundingClientRect().right > right && !ownScroller(el.parentElement, box)) {
      found.push(label(el));
    }
  }
  const texts = document.createTreeWalker(box, NodeFilter.SHOW_TEXT);
  for (let text = texts.nextNode(); text; text = texts.nextNode()) {
    const range = document.createRange();
    range.selectNodeContents(text);
    const past = [...range.getClientRects()].some((rect) => rect.width > 0 && rect.right > right);
    if (past && !ownScroller(text.parentElement, box))
      found.push(`text in ${label(text.parentElement!)}`);
  }
  return found;
}

const scrolls = (el: Element) => ["auto", "scroll"].includes(getComputedStyle(el).overflowX);

/**
 * What a reader would scroll to see past `box`: the box, if it scrolls, and
 * the nearest scroller around it, up to the document. A box that clips holds
 * its own overflow, and what one that does not scroll spills reaches that one.
 */
export function readerScrollers(box: Element): Element[] {
  let around = box.parentElement;
  while (around && !scrolls(around)) around = around.parentElement;
  return [...(scrolls(box) ? [box] : []), around ?? document.scrollingElement!];
}

/** Each scroller around `box` a reader could move sideways: what scrolls wider than it shows. */
export function sidewaysScrollers(box: Element): string[] {
  return readerScrollers(box)
    .filter((scroller) => scroller.scrollWidth > scroller.clientWidth)
    .map((scroller) => `${label(scroller)} ${scroller.scrollWidth}/${scroller.clientWidth}`);
}
