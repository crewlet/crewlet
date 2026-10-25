/**
 * What the open conversation is doing, for the Ask button while the drawer is
 * closed.
 *
 * A reader who closes the drawer mid turn still has an answer coming, or a
 * change waiting on them, and the button they will reopen it with is where
 * they look. So the drawer (a chunk) WRITES one word here as its turn moves,
 * and the button (in the entry) READS it: the button never imports the chunk,
 * which would put the whole drawer in the first paint.
 *
 * ONE WORD, NOT THE TURN. The button draws a dot and says why in its name;
 * anything more is the drawer's to show.
 */

import { useSyncExternalStore } from "react";

/** Where the open conversation stands, as the Ask button tells it. */
export type AssistantActivity =
  /** Nothing is running and nothing waits on the reader. */
  | "idle"
  /** A turn is thinking, working or applying a change. */
  | "working"
  /** A change waits on the reader's approval. */
  | "waiting";

let current: AssistantActivity = "idle";
const listeners = new Set<() => void>();

/** Say where the open conversation stands; the drawer calls this as its turn moves. */
export function setAssistantActivity(next: AssistantActivity): void {
  if (next === current) return;
  current = next;
  for (const listener of listeners) listener();
}

/** Where the open conversation stands now. */
export function assistantActivity(): AssistantActivity {
  return current;
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

/** Where the open conversation stands, as a value a component re-renders on. */
export function useAssistantActivity(): AssistantActivity {
  return useSyncExternalStore(subscribe, assistantActivity, () => "idle");
}
