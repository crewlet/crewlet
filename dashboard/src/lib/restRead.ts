/**
 * Reading a REST answer, as a hook — `useQuery`'s twin for the reads no
 * socket question answers: the Integrations screen's setup listing, its pass
 * history and one pass, the credential listing with its reference index, and
 * the org chart's guarded reads (`./chartReads.ts`).
 *
 * FIVE HAND-WRITTEN LOADERS became this, and each had got a different case
 * wrong. Every one carried its own generation counter, its own loading flag,
 * its own mapping of a failure and its own idea of when to ask again, and the
 * case none of them could express was the one that stood longest: a request
 * NO ANSWER CAME BACK TO while the live socket stayed up — thirty seconds on a
 * slow engine, one request dropped on the way. It was drawn `closed`, whose
 * banner says the socket went away and the screen reads again once it is
 * back; the socket never went away, so a read nobody polls — the credential
 * listing, one pass nobody was following — was read again only on a reload.
 * One implementation, so the failure has one code (`unanswered`), one
 * sentence, and one schedule.
 *
 * WHAT IT KEEPS, from `useQuery`:
 *
 * - THE LAST ANSWER, through any failure that is not a refusal on authority. A
 *   screen that blanks on one failed re-read tells the reader less than one
 *   that keeps the last reading and says the re-read failed. A reader REFUSED
 *   is shown nothing they were refused, so that one answer is dropped.
 * - THE ANSWER BELONGS TO ITS KEY. A read asked for another key starts from
 *   nothing, because what one pass's read held is not a reading of the next.
 * - EVERY FAILURE MAPPED ONCE (`restFailure`), into the pair `QueryState`
 *   renders, so no screen draws a failure as nothing.
 * - EVERY NEXT ASK ARMED BY THE ANSWER THAT LANDED (`lib/reread.ts`), at the
 *   wait `restRetryMs` decides from the same mapping: the engine's own hint on
 *   a `503` it wrote, never on a timer at its zero; a backoff from one second
 *   to thirty for a read nobody answered; and otherwise the screen's own
 *   cadence for what it holds. So the banner and the timer never disagree
 *   about which failure this is.
 * - A READ AGAIN WHEN THE SOCKET COMES BACK (`useRereadOnReconnect`), and,
 *   where asked for, when the tab does: an answer from before either is about
 *   an engine that has since moved.
 *
 * AND WHAT A SUPERSEDED READ MAY DO: nothing. A read that a newer one, a
 * change of key or the screen going has overtaken writes no state and arms no
 * timer, and its request is aborted, since nobody is left to hand its answer
 * to. Whichever landed last used to be what a screen held; and a read that
 * outlived its screen set state against a torn-down document, which in a test
 * run is an unhandled error from React's own dispatch after every case has
 * passed — timing, so it appeared on CI and not on a laptop.
 */

import { useCallback, useEffect, useRef, useState } from "react";
import { useReread, useRereadOnReconnect } from "./reread.ts";
import { restFailure, restRetryMs, type RestFailure } from "~/protocol/index.ts";

export interface RestReadOptions<T> {
  /**
   * Ask nothing, and hold nothing — for a read whose subject is not chosen
   * yet: a peek opened on no name, a panel with no kind to ask about. A read
   * disabled while one is in flight writes nothing and arms nothing when it
   * lands. Default true.
   */
  enabled?: boolean;
  /**
   * When the screen asks again on its own, given the answer it holds (`null`
   * before the first, and after a refusal on authority): milliseconds, or
   * `null` for not on a timer. Default: never.
   *
   * OF THE ANSWER HELD, not of the last read, because a failure says nothing
   * about what it failed to read: a pass that was running when it was last
   * read is still followed through a failed re-read the engine gave no hint
   * for, exactly as the poll it replaces would have.
   */
  cadence?: (held: T | null) => number | null;
  /**
   * Ask again when the tab becomes visible after being hidden — for an answer
   * a person changes somewhere else, as `useQuery`'s option of the same name
   * says. Default false.
   */
  refetchOnFocus?: boolean;
}

export interface RestRead<T> {
  /** The last answer — kept through a failure; see the module doc. */
  data: T | null;
  /**
   * True until the first read has answered, HOWEVER it answered — a failure
   * is a state the screen renders, waiting is not — and while a
   * `refetch(true)` is in flight. A read nobody asked for never sets it.
   */
  loading: boolean;
  /** The last read's failure in `QueryState`'s terms, or null once one answers. */
  failure: RestFailure | null;
  /**
   * The last read's failure as it was thrown, or null: for what only the
   * screen reads off it — the grants a refusal named, the route's own
   * meaning of a `404`.
   */
  error: unknown;
  /**
   * Ask again now. `blank` holds `loading` until it answers, for a read a
   * person's own write asked for, whose answer the screen must not be drawn
   * from half of; anything else asks quietly, keeping what is on screen.
   */
  refetch: (blank?: boolean) => void;
}

/**
 * What the hook holds, and THE QUESTION IT HOLDS IT FOR: the key it was read
 * under, or null for a read that is disabled.
 *
 * STAMPED, because state an effect writes is a render late. The key's effect
 * resets what is held, but only after the render that first carries the new
 * key — and that render handed back the LAST key's answer, not loading, under
 * the new one: the last tool's passes under the next tool's header, a closed
 * rail still holding the credentials it had read, and a rail opened again
 * drawn as a finished read of nothing. A reading whose question is not the one
 * asked now is never returned ([useRestRead] hands back a fresh one in its
 * place), so that render cannot exist.
 */
interface Reading<T> {
  question: string | null;
  data: T | null;
  loading: boolean;
  failure: RestFailure | null;
  error: unknown;
}

/** What a read holds before `question` has answered: nothing, and waiting if asked. */
function fresh<T>(question: string | null): Reading<T> {
  return { question, data: null, loading: question !== null, failure: null, error: null };
}

/**
 * Read `read` under `key`, and keep reading it as its answers say.
 *
 * `key` NAMES THE QUESTION and is what a change of question is detected by —
 * the paths a read asks, its kind and id — because `read` is a fresh closure
 * every render. The latest `read` and `cadence` are always the ones asked.
 */
export function useRestRead<T>(
  key: string,
  read: (signal: AbortSignal) => Promise<T>,
  options: RestReadOptions<T> = {},
): RestRead<T> {
  const { enabled = true, cadence, refetchOnFocus = false } = options;
  // THE QUESTION ASKED NOW, which every reading is held against.
  const question = enabled ? key : null;
  const [reading, setReading] = useState<Reading<T>>(() => fresh(question));
  const reread = useReread();

  // THE LATEST CLOSURES, so the identity of an inline function never starts a
  // read of its own: the key does.
  const readRef = useRef(read);
  const cadenceRef = useRef(cadence);
  useEffect(() => {
    readRef.current = read;
    cadenceRef.current = cadence;
  });

  // Which read may still write: anything that supersedes one moves it.
  const generation = useRef(0);
  const inFlight = useRef<AbortController | null>(null);
  // What the screen holds, for the cadence a failure keeps; and how many reads
  // in a row nobody answered, for the backoff.
  const held = useRef<T | null>(null);
  const unanswered = useRef(0);
  // The question the key's effect last started, which every answer is stamped
  // with: an answer the generation still lets write is always this one's.
  const asking = useRef<string | null>(question);

  const ask = useCallback(
    (blank: boolean) => {
      const mine = ++generation.current;
      const about = asking.current;
      reread.cancel();
      inFlight.current?.abort();
      const controller = new AbortController();
      inFlight.current = controller;
      if (blank) setReading((prev) => (prev.loading ? prev : { ...prev, loading: true }));
      void (async () => {
        let next: number | null;
        try {
          const data = await readRef.current(controller.signal);
          if (generation.current !== mine) return;
          held.current = data;
          unanswered.current = 0;
          setReading({ question: about, data, loading: false, failure: null, error: null });
          next = cadenceRef.current?.(data) ?? null;
        } catch (err) {
          if (generation.current !== mine) return;
          const failure = restFailure(err);
          // A READER REFUSED IS SHOWN NOTHING THEY WERE REFUSED, and every
          // other failure keeps the last reading.
          const refused = failure.error === "unauthorized";
          if (refused) held.current = null;
          unanswered.current = failure.error === "unanswered" ? unanswered.current + 1 : 0;
          setReading((prev) => ({
            question: about,
            data: refused ? null : prev.data,
            loading: false,
            failure,
            error: err,
          }));
          next = restRetryMs(err, {
            cadence: cadenceRef.current?.(held.current) ?? null,
            unanswered: unanswered.current,
          });
        }
        reread.after(next, () => ask(false));
      })();
    },
    [reread],
  );

  // THE KEY'S OWN ANSWER: asked when the read mounts and whenever its question
  // changes, starting from nothing — and nothing at all while it is disabled.
  // The cleanup supersedes whatever is in flight or armed, for the next key
  // and for a screen that went.
  useEffect(() => {
    held.current = null;
    unanswered.current = 0;
    asking.current = question;
    setReading((prev) => (prev.question === question ? prev : fresh(question)));
    if (question === null) return;
    ask(false);
    return () => {
      generation.current++;
      inFlight.current?.abort();
      inFlight.current = null;
      reread.cancel();
    };
  }, [question, ask, reread]);

  // THE SOCKET COMING BACK reads again, quietly, as `useQuery` re-asks.
  const quietly = useCallback(() => ask(false), [ask]);
  useRereadOnReconnect(quietly, enabled);

  // AND, where asked for, THE TAB COMING BACK. `visibilitychange` rather than
  // focus, for the reason `useQuery` gives: it fires for a tab that was away,
  // not for a click back into a window that never was.
  useEffect(() => {
    if (!enabled || !refetchOnFocus) return;
    const wake = () => {
      if (document.visibilityState === "visible") ask(false);
    };
    document.addEventListener("visibilitychange", wake);
    return () => document.removeEventListener("visibilitychange", wake);
  }, [enabled, refetchOnFocus, ask]);

  const refetch = useCallback(
    (blank = false) => {
      if (enabled) ask(blank);
    },
    [enabled, ask],
  );

  // NEVER ANOTHER QUESTION'S READING: until the key's effect has caught up,
  // the question asked now holds nothing yet. See [Reading].
  const shown = reading.question === question ? reading : fresh<T>(question);
  return {
    data: shown.data,
    loading: shown.loading,
    failure: shown.failure,
    error: shown.error,
    refetch,
  };
}
