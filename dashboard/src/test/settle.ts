/**
 * Waiting for what a render started to finish, rather than counting ticks or
 * sleeping.
 *
 * A screen asks its reads on mounting, and a suite's stub answers each at
 * once; what a case then asserts is drawn a few microtask hops later. The
 * suites waited for it three ways, and only one of them was sound:
 *
 * - AN AWAITED `act` IS. React's `act` (19.3) flushes the work it queued when
 *   its callback settles and then checks the queue again at a task boundary
 *   (`setImmediate`, from Node's own `timers`, which no faked clock touches),
 *   over and over until a check finds it empty; and the event loop runs every
 *   queued microtask, and every one those queue, before that boundary. So an
 *   awaited act returns only once React has no work left that a microtask
 *   chain could have queued — which made the ticks the suites counted INSIDE
 *   one (four, six, eight, twelve, twenty) dead weight: the count never
 *   decided anything.
 * - TICKS COUNTED OUTSIDE `act` were a guess at how many hops a chain takes:
 *   right until a hook gains an `await`, and then a case asserting an absence
 *   passes on a screen that has not drawn anything yet.
 * - SLEEPS of 10 to 1100 ms on the real clock were a guess at the machine's
 *   load, which is what a gate sharing its machine with `go build` and
 *   `go vet` turned red.
 *
 * Both helpers here wait at a TASK BOUNDARY inside `act`, and say what for.
 * That is an ordering guarantee, not a sleep: nothing here waits on time, and
 * a loaded machine only makes the task fire later. It covers work that is all
 * microtasks — a stub's answers, a resolved promise's continuations, a
 * MutationObserver's records, a stubbed `fetch`'s body read from a string —
 * and the renders it causes; [settled] also covers a request a stub was asked
 * from a task of its own, by waiting until no count moves across a boundary.
 * A timer is not covered: a case drives it on a faked clock, or waits for
 * what it draws.
 */

import { act } from "@testing-library/react";
import { vi } from "vitest";

import { WAIT_BOUND_MS } from "./bounds.ts";

/**
 * The runtime's own timer, taken when this module loads, before any case can
 * install a fake one: the boundary is the event loop's, and a faked clock
 * would never run it.
 */
const nextTask = globalThis.setTimeout.bind(globalThis);

/**
 * Runs, inside `act`, every microtask queued now and every one they queue in
 * turn, and the renders they cause.
 */
export async function afterMicrotasks(): Promise<void> {
  await act(() => new Promise<void>((resolve) => nextTask(resolve, 0)));
}

/**
 * Waits until nothing a pass ran asks for more: every microtask chain started
 * has run, and none of the counts given — how many requests a stub has been
 * asked — moved across the last boundary, so no render asked for an answer
 * that would be one more pass. With no count, one boundary.
 */
export async function settled(...asked: (() => number)[]): Promise<void> {
  const counts = () => asked.map((count) => count()).join(",");
  for (let seen: string | null = null; seen !== counts();) {
    seen = counts();
    await afterMicrotasks();
  }
}

/**
 * Vitest's own wait, held to the suite's wait bound: looks at `condition`
 * until it returns without throwing, and answers what it returned.
 *
 * THE WAIT FOR A CASE ON A FAKED CLOCK. Testing Library's `waitFor` arms its
 * polls and its bound on whatever `setTimeout` is installed, so on a faked
 * one it never looks again and never gives up; `vi.waitFor` keeps the
 * runtime's own timers, and moves a faked clock on by its interval at every
 * look.
 *
 * NEVER CALLED BARE. `vi.waitFor` gives up after one second unless told
 * otherwise, which is a budget beside the suite's that nobody here chose.
 * With React's tasks landing late, as on a loaded machine, the hook suites
 * that waited with it took 1.5 to 1.9 s to draw an answer their stub had
 * already given, and the default failed them. `budgets.test.ts` refuses
 * `vi.waitFor`, `vi.waitUntil` and `expect.poll` anywhere but here.
 */
export function eventually<T>(condition: () => T | Promise<T>): Promise<T> {
  return vi.waitFor(condition, { timeout: WAIT_BOUND_MS });
}
