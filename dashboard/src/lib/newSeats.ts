/**
 * The seats this tab's last save added, for as long as the engine may not have
 * placed them yet.
 *
 * A SAVE IS NOT AN APPLY (`AfterSaveStrip.tsx`). The engine activates a revision
 * and reports its new seat at once, as stopped and unplaced, until a node has
 * applied the revision and claimed the seat. Read as that seat's standing, a
 * reader who had just added it was told it had failed: Stopped, a red No node,
 * and one more in the company's problem counts, settling by itself seconds
 * later. So for that long the seat is a seat with no state yet, which is what
 * it is. Past that, an unplaced seat is what it says, and the reader is told.
 *
 * TAB-LIVED, as the saved revision the strip follows is: only this tab knows
 * what it just added, and a reload reads the engine's word.
 */

import { useSyncExternalStore } from "react";

/**
 * Two of the engine's 15 second reconcile ticks (`configplane.ReconcileInterval`):
 * one for a node to apply the revision and one for it to claim the seat.
 */
export const NEW_SEAT_SETTLE_MS = 30_000;

const EMPTY: ReadonlySet<string> = new Set();
let held: ReadonlySet<string> = EMPTY;
const listeners = new Set<() => void>();

function publish(next: ReadonlySet<string>): void {
  held = next;
  for (const listener of listeners) listener();
}

/** Holds these handles as new seats until the engine has had time to place them. */
export function holdNewSeats(handles: readonly string[]): void {
  if (handles.length === 0) return;
  publish(new Set([...held, ...handles]));
  setTimeout(() => {
    const left = new Set(held);
    for (const handle of handles) left.delete(handle);
    publish(left.size === 0 ? EMPTY : left);
  }, NEW_SEAT_SETTLE_MS);
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

/** The seats held as new now. */
export function useNewSeats(): ReadonlySet<string> {
  return useSyncExternalStore(
    subscribe,
    () => held,
    () => EMPTY,
  );
}
