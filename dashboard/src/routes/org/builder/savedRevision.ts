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

/** Records a save. */
export function recordSavedRevision(saved: SavedRevision): void {
  current = saved;
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
