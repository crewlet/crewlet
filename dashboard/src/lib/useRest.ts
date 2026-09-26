/**
 * Reading a REST answer, as a hook — the one loader for every read the socket
 * has no question for.
 *
 * `useQuery` is the loader for what the query registry answers. What it does
 * not answer is guarded and REST-only — `/secrets`, `/config/references`,
 * `/setup/integrations` and its passes — and four screens each read those with
 * a loader of their own: a `generation` ref, an unmount effect bumping it, an
 * `async` body that checked the ref three times, a `useEffect` over
 * `onTokenChanged`, and a mapping from a `RestError` to what the screen should
 * say. Two of them had already been fixed once for writing an answer nobody
 * was waiting for any more, and one of them — the Audit screen's credential
 * read — never re-read on a token change at all, so an operator who set a
 * token there saw the credential rows only after navigating away and back.
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
 * - **A retry is the engine's to schedule.** A refusal carrying `Retry-After`
 *   is read again, quietly, after exactly that wait; one without it is not
 *   read again on its own, because a 503 with no hint is a node that no wait
 *   repairs (no keyring, no surface) and re-asking it only repeats it.
 * - **A new credential is a new question.** A read re-runs whenever the
 *   operator token changes, because every route read here is guarded and the
 *   refusal a screen is showing is usually the one the new token answers.
 * - **A socket that came back is a company that moved.** Where the shell's
 *   client is present, a reconnect re-reads quietly, for the reason
 *   `useQuery` re-asks on one — and it is what makes the `closed` sentence a
 *   REST refusal renders true.
 */

import { useCallback, useContext, useEffect, useRef, useState } from "react";
import { ClientContext } from "./store-hooks.ts";
import { isAbort, onTokenChanged, RestError } from "~/protocol/index.ts";
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
   * True while a LOUD read is in flight: the first one, one for a new key, a
   * token change, and every `reload()` not marked quiet. A quiet re-read —
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
 * `unavailable` ONLY WITH A HINT, because that code's sentence promises the
 * screen asks again on its own, and this loader does so only when the engine
 * said when. Status 0 is `closed`: nothing refused it, and the read runs again
 * when the socket does. A screen whose 404 means something more particular (a
 * surface that is not registered) says so over this.
 */
export function restErrorCode(err: RestError | null): QueryErrorCode | null {
  if (!err) return null;
  if (err.unauthorized) return "unauthorized";
  if (err.status === 0) return "closed";
  if (err.status === 400) return "bad_params";
  if (err.status === 404) return "not_found";
  if (err.status === 503 && err.retryAfterSeconds !== null) return "unavailable";
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
 * Read `read` under `key`, and again whenever the key, the token or the
 * connection says the answer may have moved.
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
      if (error.retryAfterSeconds !== null) {
        // THE ENGINE'S WAIT, and never less than a second: zero is "now",
        // which against a node that just said it cannot answer is a loop.
        retry.current = setTimeout(
          () => {
            if (generation.current === mine) void run(true);
          },
          Math.max(error.retryAfterSeconds, 1) * 1000,
        );
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

  useEffect(() => onTokenChanged(() => void run(false)), [run]);

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
    loading: current ? state.loading : key !== null,
    reload,
  };
}
