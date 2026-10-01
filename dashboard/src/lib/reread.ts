/**
 * The one timer a screen's hand-rolled REST read asks itself again on — and,
 * beside it, the one event: the socket coming back (`useRereadOnReconnect`).
 *
 * `useQuery` asks the socket and arms its own next ask. A screen reading over
 * REST — the Integrations screen's `/setup` reads, the credential listing —
 * keeps its own loader, and each had its own idea of when to ask again: an
 * interval started on one cadence that went on firing on it whatever the next
 * answer said, or nothing at all, so a `503` whose `Retry-After` said two
 * seconds was asked a minute later or never. The wait after an answer is that
 * ANSWER's to decide — a pass still running, one that ended, the engine's own
 * hint on a `503` it wrote (`restRetryMs`), which at zero is "not on a timer"
 * — so each answer arms the next ask here, replacing whatever an earlier one
 * armed.
 *
 * ARMED WHERE THE ANSWER LANDS, never through state and an effect: the next
 * ask is a consequence of the answer rather than of rendering it, and armed by
 * an effect it waited for React to commit the answer before its clock even
 * started.
 */

import { useCallback, useEffect, useMemo, useRef } from "react";
import { useConnection } from "./store-hooks.ts";

export interface Reread {
  /** Ask `read` again after `ms`, replacing whatever was armed; `null` arms nothing. */
  readonly after: (ms: number | null, read: () => void) => void;
  /** Ask nothing more until the next `after` — a read that is starting now. */
  readonly cancel: () => void;
}

/** The timer, disarmed when the screen goes: a read nobody will render is waste. */
export function useReread(): Reread {
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const cancel = useCallback(() => {
    clearTimeout(timer.current);
    timer.current = undefined;
  }, []);
  useEffect(() => cancel, [cancel]);
  const after = useCallback(
    (ms: number | null, read: () => void) => {
      cancel();
      if (ms !== null) timer.current = setTimeout(read, ms);
    },
    [cancel],
  );
  return useMemo(() => ({ after, cancel }), [after, cancel]);
}

/**
 * Ask `read` again when the live socket comes back after being down — the
 * REST twin of `useQuery`'s re-ask on a reconnect.
 *
 * WHAT MAKES A `closed` BANNER TRUE on a screen that reads over REST. A read
 * that never reached the engine is drawn as `closed`, whose sentence says the
 * screen reads again once the socket is back ([restFailure]); a REST loader
 * has no socket of its own, so without this nothing read again until somebody
 * reloaded or changed tabs. The socket reconnecting is the moment the engine
 * is reachable again — the two share the origin — and an answer from before
 * it is about an engine that has since moved, which is why `useQuery`
 * re-asks there by default.
 *
 * A TRANSITION, not a state: only a socket that was down and is back asks,
 * never one that was connected all along, and a change of `read` asks
 * nothing on its own.
 */
export function useRereadOnReconnect(read: () => void, enabled = true): void {
  const { connected } = useConnection();
  const was = useRef(connected);
  useEffect(() => {
    if (enabled && connected && !was.current) read();
    was.current = connected;
  }, [connected, enabled, read]);
}
