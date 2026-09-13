/**
 * Reading a REST answer, as a hook.
 *
 * The socket's questions go through [useQuery]. A handful of answers exist
 * only over REST (the guarded `/config`, `/secrets` and `/setup` reads), and
 * each screen that needed one hand-rolled the same loader: a generation counter
 * so the read that answers last cannot overwrite the read that was asked last,
 * a re-read when the operator token changes, a re-read when the tab comes back.
 * The Integrations screen's copy carried a comment saying useQuery's doc argues
 * against a second hand-rolled loader, which is the argument for this one.
 *
 * ONE IMPLEMENTATION, with the rules the copies each learned:
 *
 *  - THE READ THAT ANSWERS LAST IS NOT THE READ THAT WAS ASKED LAST. Mount, a
 *    token change, the tab becoming visible and an explicit reload can all
 *    start one while another is in flight. A superseded read is ABORTED, not
 *    merely ignored: it holds a connection and, on a slow engine, the thirty
 *    seconds of [REQUEST_TIMEOUT_MS] for an answer nobody will read.
 *  - A TOKEN CHANGE IS A RE-READ. A guarded read refused for want of a token
 *    has to recover on the same screen once one is set, or the banner asking
 *    for it is a banner nothing can dismiss.
 *  - A REFUSAL REPLACES THE ANSWER; AN UNREACHABLE ENGINE DOES NOT. What the
 *    engine refuses (a 401 after a token was cleared, a 404 once a revision is
 *    gone) is a new fact about the resource, and keeping the old body beside it
 *    would draw content the current credential cannot read. A request that
 *    never reached the engine (status 0) says nothing about the resource, so
 *    the last answer stays on screen with the error beside it, as [useQuery]
 *    keeps its last good answer through a failed poll.
 */

import { useCallback, useEffect, useRef, useState } from "react";
import { isAbort, onTokenChanged, rest, RestError } from "~/protocol/index.ts";

export interface RestResult<T> {
  /** The last body the engine answered with, or null. See the module doc for when it is kept. */
  data: T | null;
  /** The entity-tag that came with `data`, verbatim. */
  etag: string | null;
  /**
   * The status of the latest SETTLED read: the engine's own status, 0 when it
   * could not be reached, null before anything has settled.
   */
  status: number | null;
  /** Why the latest settled read failed, or null when it did not. */
  error: RestError | null;
  /**
   * True until the first read settles, and again while a read somebody asked
   * for is in flight. A quiet re-read (the tab coming back) leaves it false,
   * because blanking what is on screen each time a reader switches tabs would
   * be the screen reporting an absence that is not there.
   */
  loading: boolean;
  /** Read again now. `quiet` keeps what is on screen without the loading state. */
  reload: (quiet?: boolean) => void;
}

export interface RestOptions {
  /** Skip the read entirely, for a screen whose inputs are not ready. Default true. */
  enabled?: boolean;
  /**
   * Read again, quietly, when the tab becomes visible after being hidden.
   *
   * For the answers a person changes somewhere else: an app installed at a
   * code host in another tab, a revision written by a colleague. Default off,
   * because most answers are not changed from outside the screen.
   */
  refetchOnFocus?: boolean;
}

interface State<T> {
  data: T | null;
  etag: string | null;
  status: number | null;
  error: RestError | null;
  loading: boolean;
}

export function useRest<T>(path: string, options: RestOptions = {}): RestResult<T> {
  const { enabled = true, refetchOnFocus = false } = options;
  const [state, setState] = useState<State<T>>({
    data: null,
    etag: null,
    status: null,
    error: null,
    loading: enabled,
  });

  // Which read is allowed to write state, and the controller that cancels it.
  // Refs rather than effect-local variables, because reload() starts reads
  // from outside any one effect run.
  const generation = useRef(0);
  const inFlight = useRef<AbortController | null>(null);

  const read = useCallback(
    (quiet: boolean) => {
      inFlight.current?.abort();
      const mine = ++generation.current;
      const controller = new AbortController();
      inFlight.current = controller;
      if (!quiet) setState((prev) => ({ ...prev, loading: true }));
      void (async () => {
        try {
          const answer = await rest.request("GET", path, { signal: controller.signal });
          if (generation.current !== mine) return;
          setState({
            data: answer.body as T,
            etag: answer.etag,
            status: answer.status,
            error: null,
            loading: false,
          });
        } catch (err) {
          if (generation.current !== mine || isAbort(err)) return;
          const refusal =
            err instanceof RestError
              ? err
              : new RestError(0, {
                  error: "unreachable",
                  detail: err instanceof Error ? err.message : String(err),
                });
          setState((prev) =>
            refusal.status === 0
              ? { ...prev, status: 0, error: refusal, loading: false }
              : { data: null, etag: null, status: refusal.status, error: refusal, loading: false },
          );
        } finally {
          if (inFlight.current === controller) inFlight.current = null;
        }
      })();
    },
    [path],
  );

  const reload = useCallback((quiet = false) => read(quiet), [read]);

  useEffect(() => {
    if (!enabled) {
      generation.current++;
      inFlight.current?.abort();
      setState({ data: null, etag: null, status: null, error: null, loading: false });
      return;
    }
    read(false);
    return () => {
      // An unmounted screen, or a path that changed, has no use for the
      // answer: a stale generation drops it, and the abort frees the
      // connection it was holding.
      generation.current++;
      inFlight.current?.abort();
    };
  }, [enabled, read]);

  useEffect(() => {
    if (!enabled) return;
    return onTokenChanged(() => read(false));
  }, [enabled, read]);

  // `visibilitychange` rather than window focus: focus fires for a click back
  // into a window that was never hidden, which is several re-reads a minute
  // for somebody moving between this screen and their editor.
  useEffect(() => {
    if (!enabled || !refetchOnFocus) return;
    const wake = () => {
      if (document.visibilityState === "visible") read(true);
    };
    document.addEventListener("visibilitychange", wake);
    return () => document.removeEventListener("visibilitychange", wake);
  }, [enabled, refetchOnFocus, read]);

  return { ...state, reload };
}
