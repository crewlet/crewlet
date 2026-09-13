/**
 * The rendered size of each card, measured by the browser, for a layout to use.
 *
 * WHY IT EXISTS. A card's height depends on what it holds (a unit stacks its
 * seats), its spacing scales with the density setting, and its text follows
 * the reader's own browser font size. A layout fed sizes copied into
 * TypeScript as constants would overlap cards in compact or comfortable
 * density and at any font size but the one somebody tested. So the cards are
 * rendered at their real width (a token), measured, and the measured sizes are
 * what the layout runs on.
 *
 * THE RULES IT KEEPS.
 *
 * - ONE ResizeObserver for every card, not one per card: a chart of a few
 *   hundred cards should cost one observer and one batched update per frame.
 * - A SIZE CHANGE IS APPLIED BEFORE PAINT. The observer reports after layout
 *   and before paint, and the update is flushed synchronously there, so a card
 *   that grows (a font finished loading, density changed, an operation added a
 *   seat) is never painted overlapping its neighbour for a frame.
 * - SUB-PIXEL NOISE IS NOT A CHANGE. Fractional re-measurements under half a
 *   pixel do not trigger a relayout, which would otherwise jitter a chart
 *   whose text renders at fractional sizes.
 * - NOTHING IS LAID OUT BEFORE IT IS MEASURED. `measured(ids)` says whether
 *   every card a layout needs has a size yet; until it does the caller shows
 *   nothing and fits nothing.
 * - THE ACTED-ON NODE STAYS PUT. `useLayoutAnchor` moves the view before paint
 *   so the node the operator just touched keeps its screen position when a
 *   relayout moves it in the world.
 *
 * Sizes are border-box, unscaled by any transform: a card inside a zoomed
 * canvas reports the size it is laid out at, which is the size a layout
 * needs. A size is kept after its card unmounts, so a card that returns (an
 * undo) is placed at its last size until the observer reports it again.
 */

import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";
import { flushSync } from "react-dom";
import type { Point, Size } from "./viewport.ts";

/** Changes smaller than this, in CSS pixels, are measurement noise rather than a new size. */
export const SIZE_EPSILON = 0.5;

export interface MeasuredSizes {
  /** The ref callback for the element drawn for `id`. Stable for the life of the id. */
  measure: (id: string) => (el: HTMLElement | null) => void;
  /** Every size seen so far, by id. A new map whenever any size changes. */
  sizes: ReadonlyMap<string, Size>;
  /** Whether every one of `ids` has been measured at least once. */
  measured: (ids: Iterable<string>) => boolean;
}

function sizeOf(entry: ResizeObserverEntry): Size {
  const box = entry.borderBoxSize?.[0];
  if (box) return { width: box.inlineSize, height: box.blockSize };
  const el = entry.target as HTMLElement;
  return { width: el.offsetWidth, height: el.offsetHeight };
}

export function useMeasuredSizes(): MeasuredSizes {
  const [sizes, setSizes] = useState<ReadonlyMap<string, Size>>(() => new Map());
  const current = useRef(sizes);
  current.current = sizes;
  const observer = useRef<ResizeObserver | null>(null);
  const idOf = useRef(new WeakMap<Element, string>());
  const callbacks = useRef(new Map<string, (el: HTMLElement | null) => void>());
  const elements = useRef(new Map<string, HTMLElement>());

  const onResize = useCallback((entries: ResizeObserverEntry[]) => {
    let next: Map<string, Size> | null = null;
    for (const entry of entries) {
      const id = idOf.current.get(entry.target);
      if (id === undefined) continue;
      const size = sizeOf(entry);
      const was = (next ?? current.current).get(id);
      if (
        was &&
        Math.abs(was.width - size.width) < SIZE_EPSILON &&
        Math.abs(was.height - size.height) < SIZE_EPSILON
      ) {
        continue;
      }
      next ??= new Map(current.current);
      next.set(id, size);
    }
    if (!next) return;
    const changed = next;
    current.current = changed;
    // Before paint: see the module doc.
    flushSync(() => setSizes(changed));
  }, []);

  const observe = useCallback(
    (el: HTMLElement) => {
      if (typeof ResizeObserver === "undefined") return;
      observer.current ??= new ResizeObserver(onResize);
      observer.current.observe(el);
    },
    [onResize],
  );

  const measure = useCallback(
    (id: string) => {
      let callback = callbacks.current.get(id);
      if (callback) return callback;
      callback = (el: HTMLElement | null) => {
        const previous = elements.current.get(id);
        if (previous && previous !== el) {
          observer.current?.unobserve(previous);
          idOf.current.delete(previous);
          elements.current.delete(id);
        }
        if (!el) return;
        elements.current.set(id, el);
        idOf.current.set(el, id);
        observe(el);
      };
      callbacks.current.set(id, callback);
      return callback;
    },
    [observe],
  );

  useEffect(
    () => () => {
      observer.current?.disconnect();
      observer.current = null;
    },
    [],
  );

  const measured = useCallback(
    (ids: Iterable<string>) => {
      for (const id of ids) if (!sizes.has(id)) return false;
      return true;
    },
    [sizes],
  );

  return { measure, sizes, measured };
}

/**
 * Keeps the acted-on node still on screen when a new layout moves it.
 *
 * `positions` is each node's world position in the current layout (null
 * while there is none); `anchorId` is the node the operator just acted on.
 * When a new layout moves that node, `keep(before, after)` runs before paint
 * with its old and new position; a canvas's `anchor` is exactly that. A node
 * that did not exist in the previous layout has no position to keep, so the
 * caller anchors something that did (a new seat's unit, a deleted seat's
 * neighbour).
 */
export function useLayoutAnchor(
  positions: ReadonlyMap<string, Point> | null,
  anchorId: string | null,
  keep: (before: Point, after: Point) => void,
): void {
  const previous = useRef(positions);
  const keepNow = useRef(keep);
  keepNow.current = keep;
  useLayoutEffect(() => {
    const was = previous.current;
    previous.current = positions;
    if (!was || !positions || anchorId === null || was === positions) return;
    const before = was.get(anchorId);
    const after = positions.get(anchorId);
    if (before && after && (before.x !== after.x || before.y !== after.y)) {
      keepNow.current(before, after);
    }
  }, [positions, anchorId]);
}
