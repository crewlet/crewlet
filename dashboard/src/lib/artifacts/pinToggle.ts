/**
 * The press every pin surface makes: a row's menu and an artifact page's.
 * It lives apart from the listing (`pins.ts`), which the sidebar reads; see
 * there for why.
 */

import { useCallback, useState } from "react";
import { useToast } from "@crewlethq/ui";
import { useKeyedGesture } from "../keyedWrite.ts";
import { pinArtifact } from "./api.ts";
import { announcePinsChanged } from "./pins.ts";

/** What a pin menu item says, the same on a row and on a page. */
export const PIN_WORDS = {
  pin: "Pin to your sidebar",
  unpin: "Unpin from your sidebar",
  pinned: "Pinned to your sidebar",
  unpinned: "Removed from your sidebar",
  couldNotPin: "Could not pin it",
  couldNotUnpin: "Could not unpin it",
} as const;

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
