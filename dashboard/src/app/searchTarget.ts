/**
 * The search box a screen owns, so `/` can find it.
 *
 * A BARE `/` MEANS "SEARCH WHAT I AM LOOKING AT". On a screen that draws a
 * search box of its own — the seat filter, the event log's search, the tools
 * catalogue, the knowledge base — that is the box, and opening the command
 * palette instead took the reader away from the screen they meant to search.
 * On a screen with no box there is nothing else it could mean, so it opens the
 * palette there. The frame cannot tell which, so the screen says: it registers
 * its box with [useSearchTarget], and the frame's `/` asks [focusSearchTarget]
 * first.
 *
 * THE MOST RECENTLY MOUNTED TARGET WINS, which is the one on top: a screen's
 * box mounts after the frame, and a search that opens over a screen mounts
 * after it. An unmounted screen takes its registration with it, so a box on a
 * screen the reader left is never focused behind the one they are on.
 */

import { useEffect, useRef, type RefObject } from "react";

/** Either the box itself, or what opens it, for a search drawn only on demand. */
export type SearchTarget = RefObject<HTMLElement | null> | (() => void);

const targets: { get: () => SearchTarget }[] = [];

/**
 * Register this screen's search. `enabled: false` withdraws it without
 * unmounting — a box drawn only in one state of the screen.
 */
export function useSearchTarget(target: SearchTarget, enabled = true): void {
  // THE LATEST TARGET IS READ THROUGH A REF, so a caller passing a fresh
  // closure every render registers once rather than on every render.
  const held = useRef(target);
  held.current = target;
  useEffect(() => {
    if (!enabled) return;
    const entry = { get: () => held.current };
    targets.push(entry);
    return () => {
      const at = targets.indexOf(entry);
      if (at >= 0) targets.splice(at, 1);
    };
  }, [enabled]);
}

/**
 * Focus the search on top, if there is one a reader can reach. Answers
 * whether it did, so the frame opens the palette when it did not.
 */
export function focusSearchTarget(): boolean {
  for (let i = targets.length - 1; i >= 0; i--) {
    const target = targets[i]!.get();
    if (typeof target === "function") {
      target();
      return true;
    }
    const el = target.current;
    // A BOX THAT IS NOT IN THE DOCUMENT OR CANNOT TAKE THE KEYBOARD is
    // skipped rather than claimed — claiming it would swallow the `/` and
    // do nothing, which is worse than the palette.
    if (!el || !el.isConnected || (el as HTMLInputElement).disabled) continue;
    el.focus();
    if (el instanceof HTMLInputElement) el.select();
    return document.activeElement === el;
  }
  return false;
}
