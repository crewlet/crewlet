/**
 * Reading a REST answer, as a hook — the one loader for every read the socket
 * has no question for.
 *
 * `useQuery` is the loader for what the query registry answers. What it does
 * not answer is guarded and REST-only — `/secrets`, `/config/references`,
 * `/setup/integrations` and its passes, the identity directory under `/iam` —
 * and four screens each read those with a loader of their own: a `generation`
 * ref, an unmount effect bumping it, an `async` body that checked the ref
 * three times, and a mapping from a `RestError` to what the screen should
 * say. Two of them had already been fixed once for writing an answer nobody
 * was waiting for any more.
 *
 * ONE IMPLEMENTATION, and it answers the questions each copy answered
 * differently:
 *
 * - **An answer belongs to the read that asked for it.** Every read takes a
 *   generation and an `AbortController`; a newer read, a changed key or an
 *   unmount supersedes it, aborts its request and drops whatever it answers.
 * - **An answer belongs to its KEY.** A read whose key changed reports
 *   nothing until the new key answers, rather than showing one object's
 *   answer under another's name.
 * - **A refusal replaces what is on screen; a request that never reached the
 *   engine does not.** An engine that answered 401, 404 or 500 said something
 *   about the read, and the last answer beside that is a guarded half still
 *   drawn after the engine refused it. Status 0 — a dropped connection, the
 *   deadline — says nothing about the answer, so the last one stays with the
 *   error beside it.
 * - **A retry is the engine's to schedule.** A `503` the engine wrote with a
 *   `Retry-After` is read again, quietly, after exactly that wait — bounded at
 *   `RETRY_AFTER_MAX_MS`, so a derived backlog estimate of minutes does not
 *   park the screen. One without it is not read again on its own, because a
 *   503 with no hint is a node that no wait repairs (no keyring, a full log)
 *   and re-asking it only repeats it.
 * - **A socket that came back is a company that moved.** Where the shell's
 *   client is present, a reconnect re-reads quietly, for the reason
 *   `useQuery` re-asks on one — and it is what makes the `closed` sentence a
 *   REST refusal renders true. A sign-in re-dials the socket, so a screen
 *   that was refused before it is read again once it lands.
 */

import { useCallback, useContext, useEffect, useRef, useState } from "react";
import { ClientContext } from "./store-hooks.ts";
import {
  isAbort,
  RestError,
  retryAfterMs,
  type LogRefusal,
  type QueryRefusal,
} from "~/protocol/index.ts";
import type { QueryErrorCode } from "~/contract/errors.ts";

export interface RestOptions {
  /**
   * Re-read every N ms, quietly. For an answer something else moves while the
   * screen is open — a pass that is running — and for nothing a person
   * changes from this screen, which re-reads by `reload` instead.
   */
  pollMs?: number;
  /**
   * Re-read, quietly, when the tab comes back — for an answer a person
   * changes somewhere else (a third-party app, another tab). See `useQuery`'s
   * option of the same name for why `visibilitychange` rather than focus.
   */
  refetchOnFocus?: boolean;
}

export interface RestResult<T> {
  /** The last answer to THIS key, or null before one arrived or after a
   *  refusal replaced it. */
  data: T | null;
  /** Why the latest read did not answer, or null when it did. */
  error: RestError | null;
  /** The error as the code `QueryState` renders a sentence for. */
  code: QueryErrorCode | null;
  /**
   * What the refusal said beyond its code — the rule and the grants that
   * would admit the reader, or a `503`'s state-log refusal and whether
   * waiting changes it — in the shape `QueryState` renders from
   * ([RestError.refusal]). Null beside no error.
   */
  refusal: QueryRefusal | LogRefusal | null;
  /**
   * True while a LOUD read is in flight: the first one, one for a new key,
   * and every `reload()` not marked quiet. A quiet re-read —
   * a poll tick, the tab coming back, a hinted retry — keeps what is on
   * screen, because blanking a table into its skeleton to say nothing new
   * takes the row somebody was reading out from under them.
   */
  loading: boolean;
  /**
   * Read again now. Resolves when this read has settled, however it settled,
   * so a caller can sequence on it; it never rejects.
   */
  reload: (options?: { quiet?: boolean }) => Promise<void>;
}

/**
 * A refusal as the code `QueryState` is keyed on.
 *
 * THE BANNER IS A TABLE OVER `QueryErrorCode`, never a place for prose: a
 * screen once handed it a sentence, the lookup missed, and a 401 rendered the
 * red "a code this build does not know" banner instead of the auth-gated one.
 *
 * `unavailable` ONLY FOR A `503` THE ENGINE WROTE ([RestError.retryHint]),
 * whose refusal says whether this loader asks again on its own and when —
 * and at zero that waiting will not change it. Status 0 is `closed`: nothing
 * refused it, and the read runs again when the socket does. A screen whose
 * 404 means something more particular (a surface that is not registered)
 * says so over this.
 */
export function restErrorCode(err: RestError | null): QueryErrorCode | null {
  if (!err) return null;
  if (err.unauthorized) return "unauthorized";
  if (err.status === 0) return "closed";
  if (err.status === 400) return "bad_params";
  if (err.status === 404) return "not_found";
  if (err.retryHint !== null) return "unavailable";
  return "query_failed";
}

/**
 * Any rejection as a `RestError`.
 *
 * A read function that threw something else failed to make sense of an
 * answer it was given — a transform over a body shaped unlike the type —
 * which is the answer being unreadable, the same thing `rest.ts` reports for a
 * body that is not JSON.
 */
function asRestError(err: unknown): RestError {
  if (err instanceof RestError) return err;
  return new RestError(502, {
    error: "unreadable_body",
    detail: err instanceof Error ? err.message : String(err),
  });
}

interface State<T> {
  key: string | null;
  data: T | null;
  error: RestError | null;
  loading: boolean;
}

/**
 * Read `read` under `key`, and again whenever the key or the connection says
 * the answer may have moved.
 *
 * `key` names the answer — the path, or the paths — and `null` reads nothing:
 * the hook's `enabled`. `read` is called with the signal that supersedes it
 * and may compose several requests; it is taken fresh on every read, so it
 * need not be memoised, and the KEY rather than its identity is what starts a
 * new one.
 */
export function useRest<T>(
  key: string | null,
  read: (signal: AbortSignal) => Promise<T>,
  options: RestOptions = {},
): RestResult<T> {
  const { pollMs, refetchOnFocus = false } = options;
  const [state, setState] = useState<State<T>>({
    key,
    data: null,
    error: null,
    loading: key !== null,
  });

  const readRef = useRef(read);
  readRef.current = read;
  const keyRef = useRef(key);
  keyRef.current = key;

  // WHICH READ MAY WRITE STATE. A ref rather than a captured flag, so a
  // superseded read — or a retry timer an earlier read armed — cannot write.
  const generation = useRef(0);
  const controller = useRef<AbortController | null>(null);
  const retry = useRef<ReturnType<typeof setTimeout> | 0>(0);
  // Whether the latest read ended without reaching the engine (status 0).
  const unreached = useRef(false);

  const run = useCallback(async (quiet: boolean): Promise<void> => {
    const mine = ++generation.current;
    controller.current?.abort();
    clearTimeout(retry.current);
    const current = keyRef.current;
    if (current === null) {
      controller.current = null;
      setState({ key: null, data: null, error: null, loading: false });
      return;
    }
    const abort = new AbortController();
    controller.current = abort;
    setState((prev) =>
      prev.key === current
        ? quiet
          ? prev
          : { ...prev, loading: true }
        : // A NEW KEY SHOWS NOTHING OF THE OLD ONE, loudly whatever asked.
          { key: current, data: null, error: null, loading: true },
    );
    try {
      const data = await readRef.current(abort.signal);
      if (generation.current !== mine) return;
      unreached.current = false;
      setState({ key: current, data, error: null, loading: false });
    } catch (err) {
      if (generation.current !== mine || isAbort(err)) return;
      const error = asRestError(err);
      unreached.current = error.status === 0;
      setState((prev) => ({
        key: current,
        // A REFUSAL REPLACES; A REQUEST THAT NEVER ARRIVED KEEPS.
        data: error.status === 0 && prev.key === current ? prev.data : null,
        error,
        loading: false,
      }));
      // THE ENGINE'S WAIT, read the one way every retry here reads it: a
      // zero — no `Retry-After` on its 503 — is never on a timer.
      const wait = error.retryHint === null ? null : retryAfterMs(error.retryHint);
      if (wait !== null) {
        retry.current = setTimeout(() => {
          if (generation.current === mine) void run(true);
        }, wait);
      }
    }
  }, []);

  const reload = useCallback((opts?: { quiet?: boolean }) => run(opts?.quiet ?? false), [run]);

  // THE KEY IS THE QUESTION. A new one is read at once, loudly.
  useEffect(() => {
    void run(false);
  }, [key, run]);

  // AN UNMOUNTED SCREEN HAS NOTHING TO WRITE INTO, and a request it can no
  // longer read is one to stop rather than one to finish.
  useEffect(
    () => () => {
      generation.current++;
      controller.current?.abort();
      clearTimeout(retry.current);
    },
    [],
  );

  useEffect(() => {
    if (key === null || !pollMs) return;
    const timer = setInterval(() => void run(true), pollMs);
    return () => clearInterval(timer);
  }, [key, pollMs, run]);

  useEffect(() => {
    if (key === null || !refetchOnFocus) return;
    const wake = () => {
      if (document.visibilityState === "visible") void run(true);
    };
    document.addEventListener("visibilitychange", wake);
    return () => document.removeEventListener("visibilitychange", wake);
  }, [key, refetchOnFocus, run]);

  // THE CONTEXT, NOT `useClient`: a dialog or a screen rendered outside the
  // shell (its own tests among them) has no socket, and a loader that threw
  // there would make every REST read depend on a transport it does not use.
  const client = useContext(ClientContext);
  useEffect(() => {
    if (!client || key === null) return;
    const { store } = client;
    let was = store.state.connected;
    // A RECONNECT, NOT THE FIRST CONNECT. The page's socket opens a moment
    // after its first REST read went out, and re-reading on that would ask
    // every guarded route twice per page load for nothing — unless the read
    // never reached the engine, in which case the socket arriving is the
    // first sign that it can.
    let seen = was;
    return store.subscribe(["health"], () => {
      const now = store.state.connected;
      if (now && !was && (seen || unreached.current)) void run(true);
      if (now) seen = true;
      was = now;
    });
  }, [client, key, run]);

  const current = state.key === key;
  const error = current ? state.error : null;
  return {
    data: current ? state.data : null,
    error,
    code: restErrorCode(error),
    refusal: error?.refusal ?? null,
    loading: current ? state.loading : key !== null,
    reload,
  };
}
