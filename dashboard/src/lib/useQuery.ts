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
import { windowEdges, type Window } from "./range.ts";
import {
  queryErrorCode,
  queryFailure,
  unavailableRetryMs,
  type LogRefusal,
  type QueryErrorCode,
  type QueryMap,
  type QueryName,
  type QueryRefusal,
} from "~/protocol/index.ts";

export interface QueryResult<T> {
  data: T | null;
  /** True only before the FIRST answer. A poll keeps the last answer on
   *  screen — replacing a rendered table with a skeleton every 30 seconds is
   *  how a polled screen becomes unreadable. */
  loading: boolean;
  /** The engine's machine-readable code (`unauthorized`, `unavailable`,
   *  `timeout`, …), or null. Typed as the protocol's own union, so a screen
   *  comparing it against a code the engine does not send fails the
   *  typecheck. */
  error: QueryErrorCode | null;
  /**
   * Why an `unauthorized` answer was refused — the rule, and the grants any
   * one of which would have admitted the reader — or, for an `unavailable`
   * one, the state log's refusal behind it and whether asking this node again
   * can change it; null otherwise. Pass it to `QueryState` beside `error`,
   * which is what lets the banner say what would change the answer rather than
   * only that there was one.
   */
  refusal: QueryRefusal | LogRefusal | null;
  /**
   * The two instants a question with a `window` was LAST ASKED over — null
   * before its first ask, and always for a question without one.
   *
   * For what a screen narrows BESIDE the answer. The audit reads four
   * sources and only the tracker's takes a window; the other three are cut to
   * it in the browser, and cut to these edges rather than to a pair of its own
   * they cannot disagree with the engine about where "the last seven days"
   * begins.
   */
  asked: AskedWindow | null;
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

/**
 * A wall-clock window a question is asked over, and the two parameters its
 * edges are written to.
 *
 * THE EDGES ARE COMPUTED WHEN THE QUESTION IS ASKED — the first ask, every
 * poll, a refetch, a reconnect — and the question is KEYED ON THE WINDOW
 * rather than on its edges. A named range's edges ARE the clock: computed at
 * render they were a fresh pair of instants on every tick of `useNow`, so the
 * key changed once a second and a list meant to be polled once a minute was
 * asked once a second instead — the audit asked the tracker for its feed one,
 * two, three, four times over three ticks. A test that moved its clock a
 * minute in one `act` re-keyed the question sixty times in a row, which React
 * reports as "Maximum update depth exceeded". Keyed on `7d`, a second passing
 * is not a new question: the poll is what asks again, over the seven days
 * ending when it asks.
 */
export interface QueryWindow {
  /** The window, as the screen holds it. */
  over: Window;
  /** The parameter the inclusive start is written to — `from` on the tracker's feed. */
  since: string;
  /** The parameter the end is written to. */
  until: string;
}

/** Two instants a question was asked over, RFC3339. */
export interface AskedWindow {
  since: string;
  until: string;
}

export interface QueryOptions {
  /** Skip the query entirely — for a screen whose parameter is not chosen yet. */
  enabled?: boolean;
  /**
   * Re-ask every N ms. Only for answers with NO push behind them; anything the
   * projection pushes must not be polled on top of it.
   *
   * AN `unavailable` ANSWER REPLACES THE NEXT TICK with the engine's own hint
   * (`unavailableRetryMs`), sooner or later than the poll, and a hint of zero
   * stops the poll until something a person does asks again — see `useQuery`.
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
  /**
   * Ask again shortly after an `inbox_changed` frame for this seat.
   *
   * FOR THE ANSWERS A COMMITTED TRACKER RECORD MOVES — the inbox, and what is
   * read beside it. The frame reaches only a socket watching the seat (the
   * shell watches the viewer's own), so on any other seat this never fires
   * and the poll is what keeps the screen current, exactly as before there
   * was a push. Keep the poll either way: a frame lost to backpressure or to
   * a reconnect is not re-sent, and the poll is what bounds how stale that
   * leaves the screen.
   */
  refetchOnInboxOf?: string;
  /** Ask over a wall-clock window whose edges the ask computes — see [QueryWindow]. */
  window?: QueryWindow;
}

/**
 * How long after an `inbox_changed` frame the answer is asked for, collecting
 * any frame that lands in the meantime into the same ask.
 *
 * A BOUND, NOT A DEBOUNCE: the first frame starts the clock and later ones
 * join it, so a steady run of pushes still produces an answer every half
 * second rather than none until the run stops. Half a second, because the
 * engine pushes once per APPLIED BATCH and a bulk gesture — a person moving a
 * column of items, a triage seat filing a set — lands as a run of batches over
 * a few hundred milliseconds; collected, the run is one read rather than one
 * per batch, which matters against a per-person budget of four questions in
 * flight at once. Next to the sixty-second poll this replaces, the wait is
 * invisible.
 */
export const INBOX_SETTLE_MS = 500;

export function useQuery<K extends QueryName>(
  what: K,
  params?: Record<string, unknown>,
  options: QueryOptions = {},
): QueryResult<QueryMap[K]> {
  const { socket, store } = useClient();
  const { connected } = useConnection();
  const {
    enabled = true,
    pollMs,
    refetchOnReconnect = true,
    refetchOnFocus = false,
    refetchOnInboxOf = "",
    window: over,
  } = options;

  const [state, setState] = useState<{
    data: QueryMap[K] | null;
    loading: boolean;
    error: QueryErrorCode | null;
    refusal: QueryRefusal | LogRefusal | null;
    asked: AskedWindow | null;
  }>({ data: null, loading: enabled, error: null, refusal: null, asked: null });

  // The params object is a fresh literal on every render, so it cannot be a
  // dependency. Its serialisation can.
  const key = JSON.stringify(params ?? {});
  // AND THE WINDOW BY WHAT WAS CHOSEN — `7d`, or a reader's two instants —
  // never by the edges an ask computes from it. See [QueryWindow].
  const windowKey = over ? JSON.stringify([over.over, over.since, over.until]) : "";

  // Which generation of the effect is allowed to write state. A ref rather
  // than a captured boolean so a poll tick started by an earlier generation
  // cannot resurrect itself.
  const generation = useRef(0);

  // A COUNTER RATHER THAN A CALLBACK holding the query, so an explicit ask
  // goes through exactly the same path as a poll tick: one implementation of
  // "what does the engine say", and a refetch that cannot drift from it.
  const [refetches, setRefetches] = useState(0);
  const refetch = useCallback(() => setRefetches((n) => n + 1), []);

  useEffect(() => {
    if (!enabled) {
      setState({ data: null, loading: false, error: null, refusal: null, asked: null });
      return;
    }
    const mine = ++generation.current;
    let timer: ReturnType<typeof setTimeout> | 0 = 0;

    const run = async (): Promise<void> => {
      // When this answer is asked again: the screen's own poll, unless the
      // engine said otherwise. `null` is never on a timer.
      let next: number | null = pollMs !== undefined && pollMs > 0 ? pollMs : null;
      const asking = JSON.parse(key) as Record<string, unknown>;
      if (windowKey !== "") {
        // THE EDGES OF THIS ASK, read off the clock now — on the first ask and
        // on every poll alike, which is what makes a minute's poll ask over
        // the window ending a minute later rather than the one the screen
        // rendered with.
        const [chosen, sinceParam, untilParam] = JSON.parse(windowKey) as [Window, string, string];
        const { since, until } = windowEdges(chosen, Date.now());
        asking[sinceParam] = since;
        asking[untilParam] = until;
        setState((prev) => ({ ...prev, asked: { since, until } }));
      }
      try {
        const data = await socket.query(what, asking);
        if (generation.current !== mine) return;
        setState((prev) => ({
          data,
          loading: false,
          error: null,
          refusal: null,
          asked: prev.asked,
        }));
      } catch (err) {
        if (generation.current !== mine) return;
        // A socket rejection always carries a code; anything else that
        // threw is a failure nobody explained, which is `query_failed`.
        const code = queryErrorCode(err instanceof Error ? err.message : null) ?? "query_failed";
        const { refusal } = queryFailure(err);
        // AN `unavailable` ANSWER IS ASKED AGAIN WHEN THE ENGINE SAID, and
        // that replaces the poll's next tick in both directions. Sooner,
        // because a minute-long poll would leave a recovered node looking
        // broken for most of that minute; later, because a node that said
        // "twenty seconds" refuses a five-second poll every time it asks. And
        // NOT AT ALL when the hint is zero — a full log, a record this node
        // cannot decode, a barrier its broker refused: each is refused the
        // same until an operator acts, so the poll stops too, the banner
        // says the node refused and what would change it, and a reconnect, a
        // refetch or the screen's next mount asks again.
        if (code === "unavailable") next = unavailableRetryMs(refusal);
        setState((prev) => ({
          // KEEP the last good answer. A screen that blanks on one failed poll
          // tells the reader less than one that shows the last reading and
          // says when it was taken.
          data: prev.data,
          loading: false,
          error: code,
          refusal,
          asked: prev.asked,
        }));
      } finally {
        if (generation.current === mine && next !== null) {
          timer = setTimeout(() => void run(), next);
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
  }, [socket, what, key, windowKey, enabled, pollMs, refetches, refetchOnReconnect && connected]);

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

  // A SEAT'S INBOX MOVING ASKS AGAIN, within INBOX_SETTLE_MS.
  //
  // Subscribed to the store directly rather than through `useSlice`, because
  // nothing here renders the counter: a re-render per frame would be work
  // done to reach a timer. The count is compared against the one this effect
  // started from, so a frame for ANOTHER seat — which moves the same slice —
  // asks nothing.
  useEffect(() => {
    if (!enabled || refetchOnInboxOf === "") return;
    let seen = store.state.inboxMoves[refetchOnInboxOf] ?? 0;
    let timer: ReturnType<typeof setTimeout> | 0 = 0;
    const unsubscribe = store.subscribe(["inboxMoves"], () => {
      const now = store.state.inboxMoves[refetchOnInboxOf] ?? 0;
      if (now === seen) return;
      seen = now;
      if (timer) return; // already collecting
      timer = setTimeout(() => {
        timer = 0;
        refetch();
      }, INBOX_SETTLE_MS);
    });
    return () => {
      unsubscribe();
      clearTimeout(timer);
    };
  }, [store, enabled, refetchOnInboxOf, refetch]);

  return { ...state, refetch };
}
