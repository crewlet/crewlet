/**
 * A case's lifetime, as everything it does sees it — and the one wait that
 * needs no testing library to bind. Imported by tests only; suites reach it
 * through `inCase.ts`, which binds the library's `act`, renders, waits and
 * events to what this module keeps.
 *
 * # A case that times out is failed, not stopped
 *
 * Its function is a promise nothing can cancel. Vitest fails it and starts
 * the next case, and the function goes on running beside the cases after it,
 * waking at whatever it was waiting for — and five things it does then reach
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
 * - A RENDER OR A CLEANUP (`render`, `renderHook`, `rerender`, `unmount`,
 *   `cleanup`), each in an `act` of the library's own that opens and closes
 *   in one turn — but a render draws into the next case's page, and a
 *   cleanup unmounts every page the library mounted, the next case's too.
 * - A POLL (`vi.waitFor`), which before every look advances whatever fake
 *   clock is installed — by then the next case's, whose timers it fires — and
 *   looks at the next case's page. Vitest's own cannot be bound, so a suite
 *   polls through [poll] instead.
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
 * its own ([caseOf]). That is the question a function every case shares could
 * not answer before — the reason the flush used to be handed to each case
 * rather than imported — and it answers it for the waits and events no suite
 * could be handed: the library's own `findBy*`, `waitFor` and `fireEvent`,
 * which `inCase.ts` binds through the library's two hooks.
 *
 * What a context cannot fix is a VALUE found in a variable every case shares,
 * which is why the builder's testkit still hands each case the lens it
 * mounted: a late `settle()` read the last mount's lens and settled the next
 * case's, whatever case it believed it was.
 *
 * # What a case's end does
 *
 * Its [Case] is closed after its last hook has run, before the next case
 * begins. Every act scope, wait and poll it still has out is ended — each one
 * races the case's end, so a scope waiting on something that will never come
 * is closed rather than left open beside the next case — INNERMOST FIRST,
 * each unwound before the next is ended, because each puts back on exit what
 * it found on entry and only the reverse of the order they opened puts back
 * the case's own. So React's scope count and the act environment are back to
 * what the next case expects before it starts. Every act, render, cleanup,
 * wait, event or poll the case asks for after that is REFUSED before it
 * opens, naming why ([caseEnded]).
 *
 * # Why this is not `inCase.ts`
 *
 * Every file runs [runCase], and a file with no document — a suite of pure
 * functions, `@vitest-environment node` — has no page for the library to
 * wait on or dispatch to. Kept with the library binding, the lifecycle loaded
 * `@testing-library/react` and `react-dom` into every such file's setup,
 * which measured about 200 ms a file for nothing. Here it loads neither; the
 * binding is loaded by the import that gives a suite the library, the only
 * door to it there is.
 */

import { AsyncLocalStorage } from "node:async_hooks";
import { vi } from "vitest";

/**
 * The refusal a late case meets: it names what was asked and why, because a
 * timed-out case's second error is read next to its first.
 */
export function caseEnded(what: string): Error {
  return new Error(
    `${what}: the case that asked has ended — this is that case, still running after its time ran out`,
  );
}

/**
 * One act scope, wait or poll a case has out: the promise its case's end
 * resolves, which the work races, and how it is counted out.
 */
export interface Out {
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

/** One case's lifetime, as every act, wait, event and poll it makes sees it. */
export class Case {
  #open = true;
  /** What the case has out, in the order it opened them. */
  readonly #out: Held[] = [];

  /** Throws once the case has ended. */
  refuse(what: string): void {
    if (!this.#open) throw caseEnded(what);
  }

  /**
   * Puts one act scope, wait or poll on the case's list, BEFORE it opens — so
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
export function caseOf(what: string): Case | null {
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

export function isThenable(value: unknown): value is PromiseLike<unknown> {
  return (
    value !== null &&
    (typeof value === "object" || typeof value === "function") &&
    typeof (value as { then?: unknown }).then === "function"
  );
}

/**
 * The host's own timers, taken as this module loads — before any case can
 * fake them — because a [poll] runs on real time whatever clock the case
 * holds, as Vitest's own does.
 */
const real = {
  setTimeout: globalThis.setTimeout.bind(globalThis),
  clearTimeout: globalThis.clearTimeout.bind(globalThis),
  setInterval: globalThis.setInterval.bind(globalThis),
  clearInterval: globalThis.clearInterval.bind(globalThis),
};

/** How long a [poll] goes on asking, and how often: `vi.waitFor`'s own options and defaults. */
export type PollOptions = number | { timeout?: number; interval?: number };

/**
 * Vitest's `vi.waitFor`, for the case whose async context calls it: asks
 * `check` at once and every `interval` until it returns — or resolves —
 * without throwing, and fails with its last error once `timeout` has passed.
 * Refused once that case has ended, and ended with it.
 *
 * `vi.waitFor` CANNOT BE BOUND FROM OUTSIDE, which is why this is a poll of
 * its own rather than a wrapper. Before every look it advances whatever fake
 * clock is installed by one interval, and it goes on looking on real timers
 * after its case has ended — so a case that timed out while one was out
 * moved the NEXT case's clock, every interval, firing the timers that case
 * had armed outside anything it did, and looked at that case's page. Nothing
 * reaches its timers to stop it, and refusing inside `check` comes after the
 * clock has moved. This one moves the clock the same way, so a case that
 * holds the timers reads exactly as it did — and stops before its next move
 * when its case ends.
 */
export function poll<T>(check: () => T | PromiseLike<T>, options: PollOptions = {}): Promise<T> {
  const { timeout = 1000, interval = 50 } =
    typeof options === "number" ? { timeout: options } : options;
  let life: Case | null;
  try {
    life = caseOf("poll");
    life?.refuse("poll");
  } catch (refusal) {
    return Promise.reject(refusal);
  }
  const out = life?.open();
  const work = new Promise<T>((resolve, reject) => {
    let done = false;
    /** Whether an answer `check` gave as a promise is still out: one at a time, as Vitest asks. */
    let asking = false;
    let lastError: unknown;
    const finish = (settle: () => void): void => {
      if (done) return;
      done = true;
      real.clearInterval(ticker);
      real.clearTimeout(deadline);
      settle();
    };
    const look = (): void => {
      if (done) return;
      try {
        // Ended already, though its end has not reached this poll yet — the
        // case is unwinding what it opened after it. It looks no more.
        life?.refuse("poll");
      } catch (refusal) {
        finish(() => reject(refusal));
        return;
      }
      // VITEST'S ORDER: the clock moves before each look, so a case that holds
      // the timers sees what they were holding back.
      if (vi.isFakeTimers()) vi.advanceTimersByTime(interval);
      if (asking) return;
      try {
        const value = check();
        if (!isThenable(value)) {
          finish(() => resolve(value));
          return;
        }
        asking = true;
        value.then(
          (answer) => finish(() => resolve(answer as T)),
          (error: unknown) => {
            asking = false;
            lastError = error;
          },
        );
      } catch (error) {
        lastError = error;
      }
    };
    void out?.ended.then(() => finish(() => reject(caseEnded("poll"))));
    const ticker = real.setInterval(look, interval);
    const deadline = real.setTimeout(
      () => finish(() => reject(lastError ?? new Error(`poll: nothing answered in ${timeout} ms`))),
      timeout,
    );
    look();
  });
  return out ? out.track(work) : work;
}
