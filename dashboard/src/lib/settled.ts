/**
 * A list that does not rearrange itself under a reader.
 *
 * The problem this exists for: on a live screen, rows arrive while somebody is
 * reading one. Splicing a new phase in at the top pushes everything down by a
 * card, so the paragraph they were mid-sentence in moves — and on a busy
 * company that happens every few seconds, which makes the page unusable rather
 * than merely busy. It is the same complaint as a chat that scrolls when you
 * are reading history.
 *
 * The rule is the one the round ledger already follows: THE PAGE MOVES ONLY
 * WHEN THE READER IS NOT READING. At the top of the scroller, new rows merge
 * straight in — that is a reader watching the feed, and holding rows back from
 * them would look broken. Scrolled down, they are held and counted, and the
 * reader merges them when they choose.
 *
 * Deliberately keyed on identity rather than on array position: a phase that
 * UPDATES in place (a live round landing, a phase completing) is not a new row
 * and must never be held back, or the row a reader is watching would freeze
 * while the rest of the page moved on.
 *
 * The same rule reaches ACROSS the two lists a live screen renders, which is
 * what `shownElsewhere` is for. A running turn lives in its own region above
 * the settled list; when it finishes it leaves that region and arrives here
 * with a key this hook has never admitted, so a reader scrolled into the
 * transcript they were following had it replaced by "1 new turn finished while
 * you were reading — show". The row was on their screen a moment earlier: it is
 * not new to them, whatever list it was in.
 */

import { useCallback, useEffect, useRef, useState } from "react";

/** How near the top counts as "watching the feed". */
export const TOP_SLACK_PX = 24;

export interface Settled<T> {
  /** What to render: the held snapshot, plus in-place updates to it. */
  items: T[];
  /** How many rows are waiting to be merged. */
  pending: number;
  /** Merge them and return to the top. */
  flush: () => void;
}

export function useSettled<T>(
  items: T[],
  keyOf: (item: T) => string,
  /**
   * Keys the screen is rendering somewhere ELSE right now — its live region.
   * Accumulated, because by the time a row arrives here it has already left
   * that list: the key is passed on the renders BEFORE the move, and this hook
   * has to still know it on the render after.
   */
  shownElsewhere: readonly string[] = [],
): Settled<T> {
  // The keys the reader has been shown. A ref, not state: it is a record of
  // what has been rendered, and writing it must not itself cause a render.
  const shown = useRef<Set<string> | null>(null);
  const [, bump] = useState(0);

  if (shown.current === null) {
    shown.current = new Set(items.map(keyOf));
  }

  const admitted = shown.current;
  // Written during render, which is safe here for the one reason it usually is
  // not: adding to a set is idempotent and order-independent, so a render React
  // discards and repeats — StrictMode's double invoke, a re-render for another
  // slice — leaves exactly the same set behind. Doing it in an effect instead
  // would show the "new rows" button for the one frame between the move and the
  // effect, which is the flicker this is here to remove.
  for (const key of shownElsewhere) admitted.add(key);

  const visible = items.filter((item) => admitted.has(keyOf(item)));
  const held = items.filter((item) => !admitted.has(keyOf(item)));

  const flush = useCallback(() => {
    for (const item of items) admitted.add(keyOf(item));
    document.querySelector(".screen")?.scrollTo({ top: 0, behavior: "smooth" });
    bump((n) => n + 1);
  }, [items, keyOf, admitted]);

  // Admit new rows while the reader is at the top. In an effect rather than
  // during render because it reads layout and writes the record — doing that
  // in the render body makes the result depend on how often React renders.
  useEffect(() => {
    if (held.length === 0) return;
    const scroller = document.querySelector(".screen");
    const atTop = !scroller || scroller.scrollTop <= TOP_SLACK_PX;
    if (!atTop) return;
    for (const item of held) admitted.add(keyOf(item));
    bump((n) => n + 1);
  });

  return { items: visible, pending: held.length, flush };
}
