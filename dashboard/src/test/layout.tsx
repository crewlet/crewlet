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
import type { ReactNode } from "react";
import { vi } from "vitest";
import { Announcer, LayerHost, ToastProvider } from "@crewlethq/ui";
import { Router } from "~/app/router.tsx";
import { FrameReadings } from "~/app/Shell.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { healthFrame } from "~/test/health.ts";
import { withDerived } from "~/test/org.ts";

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

export interface Glyph {
  c: string;
  left: number;
  right: number;
  bottom: number;
}

/** Every glyph a reader sees in `el`, in order: what only a screen reader hears is left out. */
export function glyphs(el: Element): Glyph[] {
  const out: Glyph[] = [];
  const walk = document.createTreeWalker(el, NodeFilter.SHOW_TEXT);
  for (let node = walk.nextNode(); node; node = walk.nextNode()) {
    if (node.parentElement?.closest(".crewlet-inline-ref__said, .crewlet-visually-hidden"))
      continue;
    const text = node.textContent ?? "";
    for (let i = 0; i < text.length; i++) {
      const range = document.createRange();
      range.setStart(node, i);
      range.setEnd(node, i + 1);
      const rect = range.getClientRects()[0];
      if (rect && rect.width > 0) {
        out.push({ c: text[i]!, left: rect.left, right: rect.right, bottom: rect.bottom });
      }
    }
  }
  return out;
}

export const sameLine = (a: Glyph, b: Glyph) => Math.abs(a.bottom - b.bottom) < 6;

/** Each "(" whose next glyph, or ")" whose glyph before, sits on another line. */
export function strandedPunctuation(el: Element): string[] {
  const all = glyphs(el).filter((g) => g.c.trim());
  const out: string[] = [];
  all.forEach((g, i) => {
    if (g.c === "(" && all[i + 1] && !sameLine(g, all[i + 1]!))
      out.push(`"(" before "${all[i + 1]!.c}"`);
    if (g.c === ")" && all[i - 1] && !sameLine(all[i - 1]!, g))
      out.push(`")" after "${all[i - 1]!.c}"`);
  });
  return out;
}

/** The words of `el` by line: runs of glyphs between spaces, a run cut where a line breaks. */
export function lines(el: Element): Glyph[][][] {
  const words: Glyph[][] = [];
  let word: Glyph[] | null = null;
  for (const g of glyphs(el)) {
    if (!g.c.trim()) word = null;
    else if (word && sameLine(word[0]!, g)) word.push(g);
    else words.push((word = [g]));
  }
  const rows: Glyph[][][] = [];
  for (const w of words) {
    const row = rows.find((r) => sameLine(r[0]![0]!, w[0]!));
    if (row) row.push(w);
    else rows.push([w]);
  }
  return rows;
}

/** Where `el`'s first line left room for the next two words: what a balanced title does. */
export function firstLineRoom(el: Element): number | null {
  const rows = lines(el);
  if (rows.length < 2) return null;
  const span = (w: Glyph[] | undefined) => (w ? w.at(-1)!.right - w[0]!.left : 0);
  const gap = el.getBoundingClientRect().right - rows[0]!.at(-1)!.at(-1)!.right;
  const [next, after] = rows[1]!;
  const space = 6;
  return gap >= span(next) + space + span(after) + space ? Math.round(gap) : null;
}

export class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

/** Every read a layout case's screen asks, answered from `answers`, around `children`. */
export function LayoutClient({
  answers,
  org,
  children,
}: {
  answers: Record<string, unknown>;
  org: Record<string, unknown>;
  children: ReactNode;
}) {
  const store = new Store();
  store.applyHealth(healthFrame({ nodes: 1 }));
  store.applyOrg(withDerived(org) as never);
  store.applySeats([] as never);
  const socket = new LiveSocket(store);
  socket.query = ((what: string) => Promise.resolve(answers[what] ?? {})) as typeof socket.query;
  return (
    <ToastProvider>
      <Announcer label="Announcements" />
      <LayerHost>
        <ClientContext.Provider value={{ store, socket }}>
          <FrameReadings>
            <Router>{children}</Router>
          </FrameReadings>
        </ClientContext.Provider>
      </LayerHost>
    </ToastProvider>
  );
}

/** An engine that takes every write: the act route answers each tool as applied. */
export function actsApplied(): ReturnType<typeof vi.fn> {
  return vi.fn(async (url: string) => {
    const tool = decodeURIComponent(String(url).split("/operator/act/")[1] ?? "");
    return new Response(
      JSON.stringify({
        tool,
        outcome: "applied",
        position: "CREWLET_TRACKER_LOG@1:99",
        receipt: {},
      }),
      { status: 200, headers: { "Content-Type": "application/json" } },
    );
  });
}
