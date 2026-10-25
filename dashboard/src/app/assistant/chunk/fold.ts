/**
 * Which supporting reads the person opened, for this page's session only.
 *
 * A READ IS FOLDED BY DEFAULT: the answer is the model's prose, and the card
 * a read built is one line the person opens when they want the rows. What
 * they opened stays open while the page is loaded (across the drawer closing
 * and a conversation opened again), and is forgotten on a reload: nothing is
 * written to storage, because which card was open is not worth keeping.
 */

import { useCallback, useSyncExternalStore } from "react";

const opened = new Set<string>();
const listeners = new Set<() => void>();

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

/** Opens or folds one card, by its id, and tells every card that reads it. */
export function setCardOpen(cardId: string, open: boolean): void {
  if (opened.has(cardId) === open) return;
  if (open) opened.add(cardId);
  else opened.delete(cardId);
  for (const listener of listeners) listener();
}

/** Whether one card is open, and the setter its fold calls. */
export function useCardOpen(cardId: string): [boolean, (open: boolean) => void] {
  const open = useSyncExternalStore(
    subscribe,
    () => opened.has(cardId),
    () => false,
  );
  const set = useCallback((next: boolean) => setCardOpen(cardId, next), [cardId]);
  return [open, set];
}

/** Forgets every opened card. For tests. */
export function resetCardFolds(): void {
  opened.clear();
  for (const listener of listeners) listener();
}
