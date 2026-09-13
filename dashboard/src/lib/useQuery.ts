/**
 * Asking the engine something, as a hook.
 *
 * The dashboard this replaces hand-wrote this ten times — a `data` variable, a
 * `loadError`, a `disposed` flag, an `async load()` and a `destroy()` — and one
 * of the ten forgot the flag, so its in-flight answer landed after the screen
 * had gone and re-rendered whatever had replaced it. It also hand-rolled four
 * pollers on four different intervals, one of them a bare literal with its
 * rationale in a comment and its interval repeated as a string elsewhere.
 *
 * One implementation. The cancellation is structural rather than remembered,
 * every poll interval is named and justified at the call site, and a query
 * re-runs when the socket comes back because an answer taken before a
 * reconnect is an answer about a company that has since moved.
 */

import { useCallback, useEffect, useRef, useState } from "react";
import { useClient, useConnection } from "./store-hooks.ts";
import type { QueryMap, QueryName } from "~/protocol/index.ts";

export interface QueryResult<T> {
  data: T | null;
  /** True only before the FIRST answer. A poll keeps the last answer on
   *  screen — replacing a rendered table with a skeleton every 30 seconds is
   *  how a polled screen becomes unreadable. */
  loading: boolean;
  /** The engine's machine-readable code (`unauthorized`, `no_event_store`,
   *  `timeout`, …), or null. */
  error: string | null;
  /**
   * Ask again now.
   *
   * For the moment a screen KNOWS the answer has changed, which no poll
   * interval can be short enough to cover: a write this screen just made
   * lands at the engine before the next tick, so the row it changed sat
   * showing the state it had before the button was pressed.
   *
   * It does not blank what is on screen. See `loading`.
   */
  refetch: () => void;
}

export interface QueryOptions {
  /** Skip the query entirely — for a screen whose parameter is not chosen yet. */
  enabled?: boolean;
  /**
   * Re-ask every N ms. Only for answers with NO push behind them; anything the
   * projection pushes must not be polled on top of it.
   */
  pollMs?: number;
  /** Ask again when the socket reconnects. Default true. */
  refetchOnReconnect?: boolean;
  /**
   * Ask again when the tab becomes visible after being hidden.
   *
   * FOR THE ANSWERS A PERSON CHANGES SOMEWHERE ELSE. Setting an integration
   * up means leaving for the third-party app, doing something there, and
   * coming back — and coming back is the strongest signal in the system that
   * the answer may have moved, stronger than any interval. Without it the
   * screen holds whatever it read before they left until its poll comes
   * round: at a minute's cadence an operator who finished installing a
   * GitHub App in eight seconds returned to a card still asking them to
   * install it, and reloaded the page to find out why.
   *
   * Default off, because most answers are not changed from outside this
   * screen and a tab switch is not a reason to re-ask them.
   */
  refetchOnFocus?: boolean;
}

export function useQuery<K extends QueryName>(
  what: K,
  params?: Record<string, unknown>,
  options: QueryOptions = {},
): QueryResult<QueryMap[K]> {
  const { socket } = useClient();
  const { connected } = useConnection();
  const { enabled = true, pollMs, refetchOnReconnect = true, refetchOnFocus = false } = options;

  const [state, setState] = useState<{
    data: QueryMap[K] | null;
    loading: boolean;
    error: string | null;
  }>({ data: null, loading: enabled, error: null });

  // The params object is a fresh literal on every render, so it cannot be a
  // dependency. Its serialisation can.
  const key = JSON.stringify(params ?? {});

  // Which generation of the effect is allowed to write state. A ref rather
  // than a captured boolean so a poll tick started by an earlier generation
  // cannot resurrect itself.
  const generation = useRef(0);

  // A COUNTER RATHER THAN A CALLBACK holding the query, so an explicit ask
  // goes through exactly the same path as a poll tick: one implementation of
  // "what does the engine say", and a refetch that cannot drift from it.
  const [asked, setAsked] = useState(0);
  const refetch = useCallback(() => setAsked((n) => n + 1), []);

  useEffect(() => {
    if (!enabled) {
      setState({ data: null, loading: false, error: null });
      return;
    }
    const mine = ++generation.current;
    let timer: ReturnType<typeof setTimeout> | 0 = 0;

    const run = async (): Promise<void> => {
      try {
        const data = await socket.query(what, JSON.parse(key) as Record<string, unknown>);
        if (generation.current !== mine) return;
        setState({ data, loading: false, error: null });
      } catch (err) {
        if (generation.current !== mine) return;
        setState((prev) => ({
          // KEEP the last good answer. A screen that blanks on one failed poll
          // tells the reader less than one that shows the last reading and
          // says when it was taken.
          data: prev.data,
          loading: false,
          error: err instanceof Error ? err.message : "query_failed",
        }));
      } finally {
        if (generation.current === mine && pollMs) {
          timer = setTimeout(() => void run(), pollMs);
        }
      }
    };

    setState((prev) => ({ ...prev, loading: prev.data === null }));
    void run();

    return () => {
      generation.current++;
      clearTimeout(timer);
    };
    // `connected` is a dependency only when the caller wants a reconnect to
    // re-ask; including it unconditionally would re-run every query on every
    // socket blip.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [socket, what, key, enabled, pollMs, asked, refetchOnReconnect && connected]);

  // A TAB COMING BACK ASKS AGAIN.
  //
  // `visibilitychange` rather than window focus: focus fires for a click
  // back into a window that was never hidden, which is several re-asks a
  // minute for somebody switching between this screen and their editor,
  // while this fires only for a tab that was actually away — which is
  // exactly the round trip to a third-party app that this is for.
  //
  // Its own effect, so it does not join the dependency list above and
  // re-run the query on every toggle of the flag.
  useEffect(() => {
    if (!enabled || !refetchOnFocus) return;
    const wake = () => {
      if (document.visibilityState === "visible") refetch();
    };
    document.addEventListener("visibilitychange", wake);
    return () => document.removeEventListener("visibilitychange", wake);
  }, [enabled, refetchOnFocus, refetch]);

  return { ...state, refetch };
}
