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

import { useEffect, useRef, useState } from "react";
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
}

export function useQuery<K extends QueryName>(
  what: K,
  params?: Record<string, unknown>,
  options: QueryOptions = {},
): QueryResult<QueryMap[K]> {
  const { socket } = useClient();
  const { connected } = useConnection();
  const { enabled = true, pollMs, refetchOnReconnect = true } = options;

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
  }, [socket, what, key, enabled, pollMs, refetchOnReconnect && connected]);

  return state;
}
