/**
 * The reader's artifact pins: one listing every surface reads, one press every
 * surface makes, and one signal that a press landed.
 *
 * A pin is the reader's own record, kept by the engine across devices. It is
 * drawn in three places at once: the sidebar's Pinned group, the head of For
 * you, and the Pin item of a row's or a page's menu. A press in one of them
 * used to leave the other two showing the state from before it until their
 * next poll, so a pinned row sat in the main list unmarked for a minute and
 * the sidebar promised by the menu never moved. Every press now goes through
 * [usePinToggle], which tells every reader of [usePinsChanged] to ask again.
 */

import { useCallback, useEffect, useRef, useState } from "react";
import { useToast } from "@crewlethq/ui";
import { useKeyedGesture } from "../keyedWrite.ts";
import { useQuery } from "../useQuery.ts";
import { pinArtifact } from "./api.ts";

/** The most pins the sidebar lists, and the page's own check reads. */
export const PINS_LIMIT = 200;

/** What a pin list is asked as: every artifact the reader can open that they pinned. */
export const PINS_QUERY = { scope: "all", pinned: true, limit: PINS_LIMIT } as const;

/** What a pin menu item says, the same on a row and on a page. */
export const PIN_WORDS = {
  pin: "Pin to your sidebar",
  unpin: "Unpin from your sidebar",
  pinned: "Pinned to your sidebar",
  unpinned: "Removed from your sidebar",
  couldNotPin: "Could not pin it",
  couldNotUnpin: "Could not unpin it",
} as const;

const listeners = new Set<() => void>();

/** Tells every pin reader a pin changed. */
export function announcePinsChanged(): void {
  for (const listener of [...listeners]) listener();
}

/** Calls `refetch` whenever a pin lands anywhere in this tab. */
export function usePinsChanged(refetch: () => void): void {
  const latest = useRef(refetch);
  latest.current = refetch;
  useEffect(() => {
    const listener = () => latest.current();
    listeners.add(listener);
    return () => void listeners.delete(listener);
  }, []);
}

/** The reader's pins, kept current by every press in this tab. */
export function usePins(enabled = true) {
  const pins = useQuery("artifacts", PINS_QUERY, { enabled, pollMs: 120_000 });
  usePinsChanged(pins.refetch);
  return pins;
}

/**
 * Pin or unpin one artifact. `initial` is what the caller's listing says; the
 * reader's own press overrides it until that listing says otherwise.
 */
export function usePinToggle(id: string, initial: boolean) {
  const toast = useToast();
  const gesture = useKeyedGesture();
  const [held, setHeld] = useState<{ from: boolean; to: boolean } | null>(null);
  // A FRESH LISTING WINS once it agrees with the press, or says something new.
  const pinned = held && held.from === initial ? held.to : initial;
  const toggle = useCallback(async () => {
    const next = !pinned;
    const answer = await gesture.run(pinArtifact(id, next));
    if (answer?.kind === "done") {
      setHeld({ from: initial, to: next });
      toast.ok(next ? PIN_WORDS.pinned : PIN_WORDS.unpinned);
      announcePinsChanged();
    } else if (answer) {
      toast.show({
        variant: "danger",
        title: next ? PIN_WORDS.couldNotPin : PIN_WORDS.couldNotUnpin,
        message: answer.text,
      });
    }
  }, [pinned, gesture, id, initial, toast]);
  return { pinned, toggle, busy: gesture.busy, label: pinned ? PIN_WORDS.unpin : PIN_WORDS.pin };
}
