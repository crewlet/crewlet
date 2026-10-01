/**
 * Every `act`, every wait and every event a suite makes, bound to the case
 * that made it. Imported by tests only.
 *
 * # A case that times out is failed, not stopped
 *
 * Its function is a promise nothing can cancel. Vitest fails it and starts
 * the next case, and the function goes on running beside the cases after it,
 * waking at whatever it was waiting for — and three things it does then reach
 * the next case:
 *
 * - AN `act`. React keeps ONE act scope count for the process and restores on
 *   exit whatever count it found on entry, so two cases' async scopes
 *   interleaving leave it raised for good, and every later render in the file
 *   is queued and never flushed. One timeout read as the rest of the file
 *   failing. A scope the late case already had open when its time ran out
 *   does the same from the other side: the next case's acts nest inside it.
 * - A WAIT (`findBy*`, `waitFor`). It polls `document`, which by then holds
 *   the next case's page, so it can be satisfied by an element that case drew
 *   and hand it to a case that already failed. And the library runs every
 *   wait with `IS_REACT_ACT_ENVIRONMENT` set aside and puts back what it found,
 *   which is the act scope's restore-on-exit hazard again.
 * - AN EVENT (`fireEvent`), dispatched inside the library's `act` onto
 *   whatever the late case found — the next case's controls.
 *
 * # Which case is asking is the async context the call runs in
 *
 * `setup.ts` runs every case — its `beforeEach`, its body, its `afterEach` and
 * its `onTestFinished` hooks — inside [runCase], which gives it a [Case] held
 * in an `AsyncLocalStorage`. Node carries that store through every `await`,
 * promise reaction and timer the case's own code schedules, and it carries the
 * store of the case that SCHEDULED the work, not of the case running when the
 * work wakes. So a late case's continuation still reads its own, ended case,
 * however long after its time it resumes, while the case running then reads
 * its own. That is the question a function every case shares could not answer
 * before — the reason this module's flush used to be handed to each case
 * rather than imported — and it answers it for the waits and events no suite
 * could be handed: the library's own `findBy*`, `waitFor` and `fireEvent`,
 * through its two hooks (`asyncWrapper`, `eventWrapper`), which
 * [bindTestingLibrary] wraps once for the whole run.
 *
 * What a context cannot fix is a VALUE found in a variable every case shares,
 * which is why the builder's testkit still hands each case the lens it
 * mounted: a late `settle()` read the last mount's lens and settled the next
 * case's, whatever case it believed it was.
 *
 * # What a case's end does
 *
 * Its [Case] is closed after its last hook has run, before the next case
 * begins. Every act scope and every wait it still has out is ended — each one
 * races the case's end, so a scope waiting on something that will never come
 * is closed rather than left open beside the next case — INNERMOST FIRST,
 * each unwound before the next is ended, because each puts back on exit what
 * it found on entry and only the reverse of the order they opened puts back
 * the case's own. So React's scope count and the act environment are back to
 * what the next case expects before it starts. Every act, wait or
 * event the case asks for after that is REFUSED before it opens, naming why
 * ([caseEnded]). A suite therefore reaches `act` only through this module,
 * which `inCase.source.test.ts` holds it to.
 *
 * What is not ended: the library's poll behind a wait that was refused goes
 * on to its own one-second deadline, but its answer goes nowhere and it opens
 * no scope; and a late case can still read the page or `render` into it,
 * neither of which leaves a scope open past its own call.
 */

import { AsyncLocalStorage } from "node:async_hooks";
import { act as libraryAct, configure, getConfig } from "@testing-library/react";

/**
 * The refusal a late case meets: it names what was asked and why, because a
 * timed-out case's second error is read next to its first.
 */
function caseEnded(what: string): Error {
  return new Error(
    `${what}: the case that asked has ended — this is that case, still running after its time ran out`,
  );
}

/**
 * One act scope or wait a case has out: the promise its case's end resolves,
 * which the scope or wait races, and how it is counted out.
 */
interface Out {
  /** Resolves when the case's end reaches this one — and never before. */
  readonly ended: Promise<void>;
  /** Counts `work` as what this is until it settles, and hands it back. */
  track<T>(work: Promise<T>): Promise<T>;
  /** Takes it off the case's list: it turned out not to be out at all. */
  drop(): void;
}

/** What a case's end holds of one [Out]. */
interface Held {
  end: () => void;
  settled: Promise<unknown>;
}

/** One case's lifetime, as every act, wait and event it makes sees it. */
class Case {
  #open = true;
  /** What the case has out, in the order it opened them. */
  readonly #out: Held[] = [];

  /** Throws once the case has ended. */
  refuse(what: string): void {
    if (!this.#open) throw caseEnded(what);
  }

  /**
   * Puts one act scope or wait on the case's list, BEFORE it opens — so
   * whatever it opens inside itself (an act inside an act, a wait inside a
   * scope) is placed after it, and ended before it.
   */
  open(): Out {
    let end: () => void = () => {};
    const ended = new Promise<void>((resolve) => {
      end = resolve;
    });
    const held: Held = { end, settled: Promise.resolve() };
    this.#out.push(held);
    const drop = () => {
      const at = this.#out.indexOf(held);
      if (at >= 0) this.#out.splice(at, 1);
    };
    return {
      ended,
      track: (work) => {
        held.settled = work.then(drop, drop);
        return work;
      },
      drop,
    };
  }

  /**
   * Ends the case: refuses what it asks from now on, and ends what it has out
   * INNERMOST FIRST, each unwound before the next is ended.
   *
   * Each of these restores on exit what it found on entry — React's act scope
   * count, and the act environment a wait sets aside — so they unwind
   * correctly only in the reverse of the order they opened. Ended all at once,
   * they unwound in the order they had begun waiting for the end, which is
   * not the order they opened: an act opened after an `await` inside another
   * began waiting second, so the outer scope was popped first, React reported
   * overlapping act calls and put its count back to what the INNER scope had
   * found — raised — and the next case's renders were queued and never
   * flushed; a wait inside a scope put back the act environment the scope had
   * set, over the one the case began with.
   */
  async close(): Promise<void> {
    this.#open = false;
    for (let held = this.#out.at(-1); held; held = this.#out.at(-1)) {
      held.end();
      await held.settled;
      // Settling drops it already; an entry whose work never settled through
      // [Out.track] is dropped here, or the loop would end it for ever.
      const at = this.#out.indexOf(held);
      if (at >= 0) this.#out.splice(at, 1);
    }
  }
}

const cases = new AsyncLocalStorage<Case>();

/**
 * The case running now, as the RUNNER sees it — never what a call is bound
 * to, which is its async context's. Kept only to tell a call that belongs to
 * no case because none is running (a module loading, a `beforeAll`) from one
 * that lost its case's context while one is, which would bind nothing and say
 * nothing.
 */
let running: Case | null = null;

/**
 * The case a call belongs to: the one whose async context it runs in, or
 * `null` outside every case.
 */
function caseOf(what: string): Case | null {
  const own = cases.getStore();
  if (own) return own;
  if (running) {
    throw new Error(
      `${what}: called while a case runs but from none's async context, so nothing would end it with its case — reach it from the case, or from a hook the case runs`,
    );
  }
  return null;
}

/**
 * Runs one case — `run` is the runner's, from `aroundEach` in `setup.ts` —
 * inside its own [Case], and closes the case once its last hook has run.
 */
export async function runCase(run: () => Promise<void>): Promise<void> {
  const life = new Case();
  running = life;
  try {
    await cases.run(life, run);
  } finally {
    running = null;
    await life.close();
  }
}

function isThenable(value: unknown): value is PromiseLike<unknown> {
  return (
    value !== null &&
    (typeof value === "object" || typeof value === "function") &&
    typeof (value as { then?: unknown }).then === "function"
  );
}

function boundAct(body: () => unknown): unknown {
  const life = caseOf("act");
  if (!life) return libraryAct(body as () => void);
  // REFUSED BEFORE IT OPENS, so a late case's body never runs: that body is
  // the move — a timer advanced, an answer released — that would reach the
  // next case's page.
  life.refuse("act");
  // ON THE LIST BEFORE IT OPENS, so an act its body opens is placed after it.
  const out = life.open();
  let scoped = false;
  let opened: unknown;
  try {
    opened = libraryAct((() => {
      const value = body();
      if (!isThenable(value)) return value;
      scoped = true;
      // AN ASYNC SCOPE RACES ITS CASE'S END: one still waiting when the case
      // ends would otherwise stay open beside the next case's.
      return Promise.race([value, out.ended]);
    }) as () => Promise<unknown>);
  } catch (thrown) {
    out.drop();
    throw thrown;
  }
  // A synchronous body has opened, run and closed its scope already, in one
  // turn nothing else can interleave with.
  if (!scoped) {
    out.drop();
    return opened;
  }
  return out
    .track(
      (async () => {
        return await opened;
      })(),
    )
    .then((value) => {
      // A scope the case's end closed resolved with nothing; the late case is
      // refused here rather than handed that nothing as its answer.
      life.refuse("act");
      return value;
    });
}

/**
 * `act`, for the case whose async context calls it: the library's own, refused
 * once that case has ended, and an async scope ended with its case.
 *
 * Typed as the library's, so a suite's calls read and check exactly as they
 * did.
 */
export const act = boundAct as typeof libraryAct;

/**
 * Lets every answer the page has out land, and renders what they change.
 *
 * `step`, when given, is taken first in an `act` of its own — the move that
 * makes the page ask again, such as advancing the timers a case holds. It is
 * the flush's rather than the caller's so it is REFUSED with it: a late case
 * that advanced the timers would fire the ones the next case armed.
 *
 * It is the commonest act a suite makes — five suites each kept a copy, an
 * `await act(async () => {})` — so it is here once.
 */
export async function answered(step?: () => void): Promise<void> {
  if (step) act(step);
  await act(async () => {});
}

/**
 * Binds the library's waits and events to the case whose async context makes
 * them. Called once, by `setup.ts`, after the library has configured itself.
 *
 * A WAIT RACES ITS CASE'S END INSIDE THE LIBRARY'S OWN WRAPPER, so when the
 * case ends the wrapper unwinds — it puts back the act environment it set
 * aside — before the next case begins, and the case is handed the refusal
 * rather than an element the next case drew.
 */
export function bindTestingLibrary(): void {
  const { asyncWrapper: waitAsLibrary, eventWrapper: dispatchAsLibrary } = getConfig();
  configure({
    asyncWrapper: (wait) => {
      const life = caseOf("findBy/waitFor");
      if (!life) return waitAsLibrary(wait);
      try {
        life.refuse("findBy/waitFor");
      } catch (refusal) {
        return Promise.reject(refusal);
      }
      const out = life.open();
      const ended = out.ended.then(() => {
        throw caseEnded("findBy/waitFor");
      });
      return out.track(waitAsLibrary(() => Promise.race([wait(), ended])));
    },
    eventWrapper: (dispatch) => {
      caseOf("fireEvent")?.refuse("fireEvent");
      return dispatchAsLibrary(dispatch);
    },
  });
}
