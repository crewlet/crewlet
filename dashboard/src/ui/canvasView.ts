/**
 * The view a tree canvas is drawn at when a SCREEN chooses it rather than the
 * reader: the legibility floor, the gestures a screen makes to reach it, and
 * the first view held at it.
 *
 * # Two charts, one floor
 *
 * The org chart (`routes/agents/OrgChart.tsx`) and the chart Edit org draws
 * (`routes/org/builder/CanvasView.tsx`) are two drawings of one organization,
 * and both are fitted by the kit's canvas on their first layout. A fit goes
 * as far as the company is wide: Nimbus's seven seats put the org chart at
 * 54% beside a peek at 1280 and the builder's chart at 57% at 1440, where a
 * card's name was eight pixels and its caption seven — a chart drawn whole and
 * read by nobody. The org chart grew a floor; the builder opened on the 57%
 * chart for another release, because the floor lived in the org chart's own
 * file. It lives here so the two cannot disagree about what "legible" is.
 *
 * # Why the view is moved by the READER'S gestures
 *
 * The kit's tree canvas passes out only `focusNode`, `focusRegion` and
 * `restoreView` from the canvas it wraps — no fit, no zoom and no event when
 * the view changes. So the view is read the way a reader meets it — the
 * viewport is the group announced as a canvas, and the layer it moves is its
 * one child — and moved by the two things a reader does to it:
 *
 * - the FIT is the canvas's own Fit control, found by the label a screen gives
 *   it ([CANVAS_LABELS]) and pressed, so there is exactly one answer to
 *   "fitted": the kit's;
 * - the ZOOM is the reader's zoom gesture — ctrl and the wheel over a point —
 *   made ONCE, with the delta the kit's published wheel arithmetic
 *   (`CANVAS_WHEEL_ZOOM_PER_PIXEL`) turns into exactly the zoom wanted, about
 *   exactly the point that leaves the node the reader is on where it belongs.
 *
 * It used to be the Zoom in button, pressed until the view reached the floor.
 * That can only land on the kit's steps, and it zooms about the canvas's
 * centre: the builder came to rest at 0.57 × 1.2³ ≈ 0.99, which read "99%" at
 * 1440 and "101%" at 1280 beside the other chart's 100%, and the crop was
 * wherever the middle of the fit happened to be — a sliver of one card at one
 * edge, the root off centre, the company's two people off screen. A kit that
 * hands out a view setter and a view-change event is a change to
 * [canvasControls] and nothing else.
 *
 * Every gesture is flushed, so the view it moved to can be read before the
 * next one decides anything.
 */

import { useEffect, useRef } from "react";
import { flushSync } from "react-dom";
import {
  CANVAS_FIT_PADDING,
  CANVAS_REVEAL_PADDING,
  CANVAS_WHEEL_ZOOM_PER_PIXEL,
  type CanvasPoint,
  type CanvasRect,
  type CanvasSize,
  type CanvasView,
  type TreeInput,
} from "@crewlethq/ui";

/**
 * The smallest zoom a screen draws a tree canvas at ON ITS OWN, and the zoom
 * it draws one at when the fit would go below it.
 *
 * A card's smallest text a reader is meant to read is `--font-size-xs`, 12px;
 * at 0.85 it is drawn at 10.2px, and below it it stops being text. A chart
 * that fits above this is fitted whole; one that would not is drawn AT this —
 * exactly this, so the readout says 85% rather than whichever step of the
 * zoom buttons came next — with the node the reader is on placed in it. Only
 * a Fit the READER presses goes below it — that is a request for the whole
 * company, legible or not.
 */
export const LEGIBLE_ZOOM = 0.85;

/**
 * The label a screen gives the canvas's own Fit control — and so how
 * [canvasControls] finds it to press. Handed to `TreeCanvas`'s `labels` by
 * every screen that holds a floor, so a kit that renamed its default would
 * move nothing here.
 */
export const CANVAS_LABELS = { fit: "Fit to view" } as const;

/** What [canvasControls] offers. */
export interface CanvasControls {
  /** The view as the canvas draws it: the transform of the layer it pans. */
  view(): string | null;
  /** That view as numbers, or null before there is one. */
  current(): CanvasView | null;
  /** The viewport's size, or null before it is laid out. */
  size(): CanvasSize | null;
  /** Where the card holding node `id` is drawn in the world, or null. */
  place(id: string): CanvasRect | null;
  /** The world box every placed card is drawn inside, or null with none. */
  bounds(): CanvasRect | null;
  /** Press Fit. */
  fit(): void;
  /** Zoom by `factor` about `at` (viewport pixels): ctrl and the wheel there. */
  zoomAbout(factor: number, at: CanvasPoint): void;
}

/** `translate(12px, -4px) scale(0.5)` as a view; a transform with no scale is actual size. */
function parseView(transform: string): CanvasView | null {
  const m = /translate\((-?[\d.e-]+)px,\s*(-?[\d.e-]+)px\)(?:\s*scale\(([\d.e-]+)\))?/.exec(
    transform,
  );
  if (!m) return null;
  return { x: Number(m[1]), y: Number(m[2]), k: m[3] === undefined ? 1 : Number(m[3]) };
}

/**
 * Where a card is drawn in the world: the kit places each of the tree's own
 * children with a translate in world units, and its box is a layout size,
 * which the canvas's scale does not change. A card the layout has not placed
 * yet has no translate, and is nowhere.
 */
function cardBox(card: HTMLElement): CanvasRect | null {
  const at = parseView(card.style.transform);
  if (!at || card.offsetWidth <= 0) return null;
  return { x: at.x, y: at.y, width: card.offsetWidth, height: card.offsetHeight };
}

/**
 * The canvas's own controls and gestures under `host`, and its view as it
 * draws it — see the module note. `pressing` is raised while a press or a
 * gesture is this screen's own, so a screen that listens for the READER's
 * presses can tell the two apart.
 */
export function canvasControls(
  host: HTMLElement,
  pressing: { current: boolean } = { current: false },
): CanvasControls {
  const viewport = () =>
    host.querySelector<HTMLElement>('[role="group"][aria-roledescription="canvas"]');
  const layer = () => viewport()?.firstElementChild as HTMLElement | null;
  const own = (act: () => void) => {
    pressing.current = true;
    try {
      flushSync(act);
    } finally {
      pressing.current = false;
    }
  };
  return {
    view: () => layer()?.style.transform ?? null,
    current: () => parseView(layer()?.style.transform ?? ""),
    size: () => {
      const el = viewport();
      if (!el || el.clientWidth <= 0 || el.clientHeight <= 0) return null;
      return { width: el.clientWidth, height: el.clientHeight };
    },
    place: (id) => {
      // THE TREE ITEM BY THE ID THE KIT STAMPS ON IT (`data-tree-id`, part of
      // its item props), compared rather than put in a selector, since a key
      // is the engine's and may hold a character a selector would read.
      const item = [...host.querySelectorAll("[data-tree-id]")].find(
        (el) => el.getAttribute("data-tree-id") === id,
      );
      const card = item?.closest<HTMLElement>('[role="tree"] > *');
      return card ? cardBox(card) : null;
    },
    bounds: () => {
      let box: CanvasRect | null = null;
      for (const card of host.querySelectorAll<HTMLElement>('[role="tree"] > *')) {
        const at = cardBox(card);
        if (!at) continue;
        if (!box) {
          box = at;
          continue;
        }
        const right = Math.max(box.x + box.width, at.x + at.width);
        const bottom = Math.max(box.y + box.height, at.y + at.height);
        const x = Math.min(box.x, at.x);
        const y = Math.min(box.y, at.y);
        box = { x, y, width: right - x, height: bottom - y };
      }
      return box;
    },
    fit: () => {
      const button = host.querySelector<HTMLButtonElement>(
        `button[aria-label="${CANVAS_LABELS.fit}"]`,
      );
      if (button) own(() => button.click());
    },
    zoomAbout: (factor, at) => {
      const el = viewport();
      if (!el || !(factor > 0)) return;
      const box = el.getBoundingClientRect();
      // THE DELTA THE KIT'S OWN ARITHMETIC TURNS INTO `factor`:
      // `wheelZoomFactor(dy)` is 2^(−dy · CANVAS_WHEEL_ZOOM_PER_PIXEL).
      const deltaY = -Math.log2(factor) / CANVAS_WHEEL_ZOOM_PER_PIXEL;
      own(() =>
        el.dispatchEvent(
          new WheelEvent("wheel", {
            bubbles: true,
            cancelable: true,
            ctrlKey: true,
            deltaMode: 0,
            deltaY,
            clientX: box.left + at.x,
            clientY: box.top + at.y,
          }),
        ),
      );
    },
  };
}

/**
 * What [holdLegible] did.
 *
 * - `fitted`: the view was at or above the floor, and nothing moved;
 * - `anchored`: it was drawn at the floor with the anchor placed in it;
 * - `centred`: it was drawn at the floor about the canvas's centre, because
 *   there was no anchor or its card is not laid out yet (a node inside a
 *   closed parent) — the caller reveals it.
 */
export type Hold = "fitted" | "anchored" | "centred";

/**
 * Draw the view at exactly [LEGIBLE_ZOOM] where it is below it, with the node
 * `anchor` names placed in it — in ONE gesture, so there is no intermediate
 * view and no zoom but the floor.
 *
 * WHERE THE ANCHOR GOES — the selected node, or with none the chart's root:
 *
 * - ACROSS, centred on it. Zoomed about the canvas's centre, Nimbus's root sat
 *   141px left of centre with the ranks under it cut at both edges.
 * - DOWN, where the kit's own fit puts a chart (`fitView`): the whole of it in
 *   the middle when it is shorter than the room, read from its top edge when
 *   it is not — and then pulled just far enough to show the anchor, with the
 *   kit's reveal padding, if that left it off the canvas. Centring a leaf down
 *   the canvas too cut the ranks above it off the top of a chart that had room
 *   to show all of them.
 *
 * The point the gesture is made about is solved for, not guessed: a zoom by
 * `s` about `p` draws a view offset `o` at `p − (p − o)·s`, so the offset that
 * places the anchor is reached about `p = (o′ − s·o) / (1 − s)`.
 */
export function holdLegible(controls: CanvasControls, anchor: string | null): Hold {
  const view = controls.current();
  const size = controls.size();
  if (!view || !size || view.k >= LEGIBLE_ZOOM) return "fitted";
  const s = LEGIBLE_ZOOM / view.k;
  const box = anchor !== null ? controls.place(anchor) : null;
  if (!box) {
    controls.zoomAbout(s, { x: size.width / 2, y: size.height / 2 });
    return "centred";
  }
  const k = LEGIBLE_ZOOM;
  const x = size.width / 2 - (box.x + box.width / 2) * k;
  const all = controls.bounds() ?? box;
  const room = size.height - 2 * CANVAS_FIT_PADDING;
  let y =
    all.height * k <= room
      ? (size.height - all.height * k) / 2 - all.y * k
      : CANVAS_FIT_PADDING - all.y * k;
  const top = y + box.y * k;
  const bottom = top + box.height * k;
  if (top < CANVAS_REVEAL_PADDING) y += CANVAS_REVEAL_PADDING - top;
  else if (bottom > size.height - CANVAS_REVEAL_PADDING) {
    y -= bottom - (size.height - CANVAS_REVEAL_PADDING);
  }
  controls.zoomAbout(s, { x: (x - s * view.x) / (1 - s), y: (y - s * view.y) / (1 - s) });
  return "anchored";
}

/**
 * The root a chart is read from: of a forest's roots, the one with the most
 * nodes under it. A forest's first root is whoever the data listed first — a
 * person with no manager, drawn at the far left of the reporting chart — and
 * the organization hangs from the other one.
 */
export function largestRoot(roots: readonly TreeInput[]): string | null {
  const size = (n: TreeInput): number =>
    1 + (n.children ?? []).reduce<number>((sum, c) => sum + size(c), 0);
  let best: { id: string; nodes: number } | null = null;
  for (const n of roots) {
    const nodes = size(n);
    if (!best || nodes > best.nodes) best = { id: n.id, nodes };
  }
  return best?.id ?? null;
}

/** Whether the kit's canvas under `host` has drawn its first layout and fit. */
export function canvasReady(host: HTMLElement): boolean {
  return host.querySelector("[data-ready]")?.getAttribute("data-ready") === "true";
}

/**
 * The canvas's own first fit, held at [LEGIBLE_ZOOM] where it fell below it
 * with the node `anchor()` names placed in it ([holdLegible]) — and `reveal`
 * asked to bring that node into view where it could not be placed.
 *
 * ONCE PER CANVAS, on the frame after the canvas says it is ready, so the fit
 * it is ready with has been drawn. Nothing after that is this hook's: the kit
 * never moves a reader's view on its own after its first fit, and neither does
 * this.
 *
 * The org chart does more — it fits again when its width moves under a peek —
 * and keeps that in its own `useChartView`, which reads the same floor.
 */
export function useLegibleFirstView(
  host: HTMLElement | null,
  anchor: () => string | null,
  reveal: () => void,
): void {
  const latest = useRef({ anchor, reveal });
  latest.current = { anchor, reveal };
  useEffect(() => {
    if (!host) return;
    let placed = false;
    let frame = 0;
    const place = () => {
      if (placed || !canvasReady(host)) return;
      placed = true;
      watch.disconnect();
      frame = requestAnimationFrame(() => {
        const hold = holdLegible(canvasControls(host), latest.current.anchor());
        if (hold === "centred") latest.current.reveal();
      });
    };
    const watch = new MutationObserver(place);
    watch.observe(host, { attributes: true, attributeFilter: ["data-ready"], subtree: true });
    place();
    return () => {
      watch.disconnect();
      cancelAnimationFrame(frame);
    };
  }, [host]);
}
