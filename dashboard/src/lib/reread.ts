/**
 * The one timer a REST read asks itself again on — and, beside it, the one
 * event: the socket coming back (`useRereadOnReconnect`). Both are the
 * machinery under `useRestRead` (`./restRead.ts`), which is what a screen
 * reading over REST uses; nothing else should need either.
 *
 * Each screen reading over REST used to keep its own loader with its own idea
 * of when to ask again: an interval started on one cadence that went on firing
 * on it whatever the next answer said, or nothing at all, so a `503` whose
 * `Retry-After` said two seconds was asked a minute later or never. The wait
 * after an answer is that ANSWER's to decide — a pass still running, one that
 * ended, the engine's own hint on a `503` it wrote (`restRetryMs`), which at
 * zero is "not on a timer" — so each answer arms the next ask here, replacing
 * whatever an earlier one armed.
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
 * The socket reconnecting is the moment the engine is reachable again — the
 * two share the origin — and an answer from before it is about an engine that
 * has since moved, which is why `useQuery` re-asks there by default. A REST
 * read that nobody answered asks again on its own backoff as well
 * (`restRetryMs`), because the socket is routinely up the whole time such a
 * read fails and this event then never comes; this is what asks at ONCE when
 * it does, rather than at the end of a wait that may have grown to thirty
 * seconds.
 *
 * A TRANSITION, not a state: only a socket that was down and is back asks,
 * never one that was connected all along, and a change of `read` asks
 * nothing on its own.
 *
 * AND A RE-CONNECT, NOT THE FIRST CONNECT. A page's socket opens a moment
 * after its screens' first REST reads went out over HTTP, which needs no
 * socket — so a socket first coming up under a mounted read is no news about
 * the engine, and asking on it asked every guarded read twice per page load.
 * The one exception is a read that never REACHED the engine (`unreached`,
 * asked when the socket comes up): the socket arriving is then the first sign
 * that it can.
 */
export function useRereadOnReconnect(
  read: () => void,
  enabled = true,
  unreached: () => boolean = () => false,
): void {
  const { connected } = useConnection();
  const was = useRef(connected);
  // WHETHER THIS MOUNT HAS SEEN THE SOCKET UP, which is what tells a
  // reconnect from the first connect.
  const seen = useRef(connected);
  const unreachedRef = useRef(unreached);
  useEffect(() => {
    unreachedRef.current = unreached;
  });
  useEffect(() => {
    if (enabled && connected && !was.current && (seen.current || unreachedRef.current())) read();
    was.current = connected;
    if (connected) seen.current = true;
  }, [connected, enabled, read]);
}
