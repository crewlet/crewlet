/**
 * The last revision this tab saved from the org builder, for as long as the
 * tab lives.
 *
 * A SAVE IS NOT AN APPLY. The engine answers a write once the revision is
 * stored and activated; each node applies it on its own reconcile tick, and
 * may refuse it. Until this node has applied it, the org projection every
 * read lens draws from still describes the previous revision, and the lens
 * the operator switches to right after saving has to say so. The Builder is
 * unmounted by that switch, so what it saved is kept here, outside any one
 * screen, rather than in the Builder's own state.
 *
 * In memory only: a reload starts without it, and the Fleet screen remains
 * the place to read where every node stands.
 */

import { useSyncExternalStore } from "react";

export interface SavedRevision {
  readonly revisionId: string;
  /**
   * The revision the save was built on, which is the saved revision's parent;
   * `null` for the company's first revision. What the save changed is the
   * saved revision against this one: against the active revision, a save
   * that is active now differs from nothing.
   */
  readonly parentRevisionId: string | null;
  /**
   * The epoch the write activated, from its answer. `null` when the save's
   * answer was lost and the write was found afterwards by reading the
   * revision history, which carries no epoch.
   */
  readonly epoch: number | null;
}

let current: SavedRevision | null = null;
const listeners = new Set<() => void>();

function emit(): void {
  for (const listener of listeners) listener();
}

/**
 * Records a save.
 *
 * ONE REVISION RECORDED TWICE KEEPS WHAT WAS KNOWN OF IT. A save's own answer
 * records it with its epoch, and a later visit of the same page that settles
 * the same save from the revision history (`useSave.resume`) records it again
 * with none. The epoch is what the strip matches a node's applied epoch
 * against, so the second record keeps the first one's rather than erasing it.
 */
export function recordSavedRevision(saved: SavedRevision): void {
  current =
    saved.epoch === null && current?.revisionId === saved.revisionId
      ? { ...saved, epoch: current.epoch }
      : saved;
  emit();
}

/** Forgets the last save, once the operator dismisses its status. */
export function clearSavedRevision(): void {
  if (current === null) return;
  current = null;
  emit();
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

/** The last save of this tab, or `null`. */
export function useSavedRevision(): SavedRevision | null {
  return useSyncExternalStore(
    subscribe,
    () => current,
    () => null,
  );
}
