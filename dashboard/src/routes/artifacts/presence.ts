/**
 * What this reader tells an artifact's room: where they are and whether they
 * are editing, and that they went idle.
 *
 * Active is input in the last two minutes in a visible tab; a hidden tab, or
 * two minutes without input, is idle, and the next input says where the reader
 * is again. Who is speaking is never this module's to say: the engine shows
 * the seat the socket's credential resolved to.
 */

import { useCallback, useEffect, useRef } from "react";
import type { PresenceActivity, PresenceFocus } from "~/protocol/index.ts";
import type { ArtifactRoom } from "~/lib/artifacts/room.ts";
import type { ArtifactPeople } from "~/lib/artifacts/people.ts";

/** How long without input a reader stays active (5.4 of the plan): two minutes. */
export const IDLE_AFTER_MS = 2 * 60_000;

/** The reader's place, as a screen inside the page reports it. */
export interface PresenceReport {
  /** Where the reader is now: a cell, a region, or nothing more than the version on show. */
  at: (focus: Omit<PresenceFocus, "version"> | null, editing: boolean) => void;
}

export function useArtifactPresence(
  room: ArtifactRoom,
  _people: ArtifactPeople,
  version: number,
): PresenceReport {
  const place = useRef<{ focus: Omit<PresenceFocus, "version"> | null; editing: boolean }>({
    focus: null,
    editing: false,
  });
  const idle = useRef(false);
  const timer = useRef<ReturnType<typeof setTimeout> | 0>(0);
  const { say } = room;
  const joined = room.state.kind === "joined";

  const speak = useCallback(
    (activity: PresenceActivity) => {
      if (!joined) return;
      say(activity, { ...(place.current.focus ?? {}), ...(version > 0 ? { version } : {}) });
    },
    [say, version, joined],
  );

  const last = useRef(0);
  const active = useCallback(() => {
    // A POINTER MOVES MANY TIMES A SECOND: the clock is reset at most once a
    // second, which is far inside the two minutes it measures.
    const now = Date.now();
    if (!idle.current && now - last.current < 1000) return;
    last.current = now;
    clearTimeout(timer.current);
    timer.current = setTimeout(() => {
      idle.current = true;
      speak("idle");
    }, IDLE_AFTER_MS);
    if (idle.current) {
      idle.current = false;
      speak(place.current.editing ? "editing" : "viewing");
    }
  }, [speak]);

  useEffect(() => {
    speak(place.current.editing ? "editing" : "viewing");
    active();
    const onVisibility = () => {
      if (document.visibilityState === "hidden") {
        idle.current = true;
        speak("idle");
      } else {
        active();
      }
    };
    // INPUT IS A POINTER, A WHEEL OR FOCUS MOVING, and every move the
    // reader makes inside a sheet reports where they are (`at`), which counts
    // too. No key is listened for at the window: the frame's one keymap owns
    // those (`app/keymap.ts`).
    const events = ["pointerdown", "pointermove", "wheel", "focusin"] as const;
    for (const name of events)
      window.addEventListener(name, active, { capture: true, passive: true });
    document.addEventListener("visibilitychange", onVisibility);
    return () => {
      clearTimeout(timer.current);
      for (const name of events) window.removeEventListener(name, active, { capture: true });
      document.removeEventListener("visibilitychange", onVisibility);
    };
  }, [speak, active]);

  const at = useCallback(
    (focus: Omit<PresenceFocus, "version"> | null, editing: boolean) => {
      place.current = { focus, editing };
      idle.current = false;
      active();
      speak(editing ? "editing" : "viewing");
    },
    [speak, active],
  );
  return { at };
}
