/**
 * The testing library, bound to the case that uses it — every `act`, render,
 * wait and event a suite makes — and the case-bound [poll] beside them.
 * Imported by tests only; `cases.ts` is the case's lifetime this binds to,
 * and says why a case that times out reaches the next one at all.
 *
 * # The one door to the library
 *
 * A suite imports the library from here and from nowhere else, which
 * `inCase.source.test.ts` holds it to. So loading the library at all loads
 * this module, which binds it as it loads, and there is no suite whose
 * waits are bound only if something else remembered to load the binding.
 *
 * - [act], the library's own, refused once its case has ended and an async
 *   scope ended with its case.
 * - [render], [renderHook] and what they hand back, and [cleanup], refused
 *   once their case has ended. Each opens and closes its own `act`, so none
 *   leaves a scope open — but each moves the page, and the page by then is
 *   the next case's.
 * - The library's waits (`findBy*`, `waitFor`) and events (`fireEvent`),
 *   through its two hooks, `asyncWrapper` and `eventWrapper`, set once as
 *   this module loads ([bindTestingLibrary]) — a wait's restated, so every
 *   step of it ends with its case ([waitUntilEnded]).
 *
 * What is not ended: the library's poll behind a wait that was refused goes
 * on to its own one-second deadline, but its answer goes nowhere and it opens
 * no scope; and a late case can still READ the page, which moves nothing.
 *
 * Nor is what a late case writes to the page's globals ITSELF — the fake
 * clock through `vi`, `location.hash`, a stub — and that is a boundary, not
 * an oversight. Every act, wait and poll here rejects at its case's end, so
 * a late case is thrown out at the first of them it awaits. What of its own
 * it can still run is the `catch` or `finally` that throw passes through,
 * and whatever follows an await of something none of them is — a promise it
 * made, a stubbed answer. No hook of the library's or Vitest's carries a
 * write there, and `location`'s members cannot be wrapped at all: jsdom
 * defines them on the object itself, unconfigurable, as the platform's
 * `[LegacyUnforgeable]` requires.
 */

import {
  act as libraryAct,
  cleanup as libraryCleanup,
  configure,
  getConfig,
  render as libraryRender,
  renderHook as libraryRenderHook,
} from "@testing-library/react";
import { caseEnded, caseOf, isThenable } from "./cases.ts";

// THE REST OF THE LIBRARY, AS IT IS: its queries move nothing, and its waits
// and events are bound through its own hooks below. Named rather than `export
// *`, which would hand out `configure` — one call of which unbinds them all.
export {
  fireEvent,
  getConfig,
  screen,
  waitFor,
  waitForElementToBeRemoved,
  within,
} from "@testing-library/react";
export { poll, type PollOptions } from "./cases.ts";

/** Refuses `what` to a case that has ended; outside every case, nothing. */
function refuseLate(what: string): void {
  caseOf(what)?.refuse(what);
}

/**
 * The library's `render`, refused to a case that has ended — and the
 * `rerender` and `unmount` it hands back, likewise.
 *
 * A LATE RENDER DRAWS INTO THE NEXT CASE'S PAGE. It opens and closes its own
 * `act` in one turn, so it leaves no scope open, which is why it was first
 * left unbound; but the page it mounts goes into the document the next case
 * is reading, whose queries then find two of whatever both drew — and a
 * render made while the next case is inside an `act` of its own is queued
 * into that case's scope and lands with its renders.
 */
export const render = ((...args: Parameters<typeof libraryRender>) => {
  refuseLate("render");
  const page = libraryRender(...args);
  return {
    ...page,
    rerender: (ui: Parameters<typeof page.rerender>[0]) => {
      refuseLate("rerender");
      page.rerender(ui);
    },
    unmount: () => {
      refuseLate("unmount");
      page.unmount();
    },
  };
}) as typeof libraryRender;

/** The library's `renderHook`, refused to a case that has ended, as [render] is. */
export const renderHook = ((...args: Parameters<typeof libraryRenderHook>) => {
  refuseLate("renderHook");
  const hook = libraryRenderHook(...args);
  return {
    ...hook,
    rerender: (props?: Parameters<typeof hook.rerender>[0]) => {
      refuseLate("rerender");
      hook.rerender(props);
    },
    unmount: () => {
      refuseLate("unmount");
      hook.unmount();
    },
  };
}) as typeof libraryRenderHook;

/**
 * The library's `cleanup`, refused to a case that has ended: it unmounts
 * EVERY page the library mounted, so a late one took the next case's page
 * out from under it.
 */
export function cleanup(): void {
  refuseLate("cleanup");
  libraryCleanup();
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

/** Where React reads whether it runs under `act`, which every wait sets aside. */
const reactGlobals = globalThis as { IS_REACT_ACT_ENVIRONMENT?: unknown };

/**
 * The library's own wrapper around a wait, for a wait a case makes, with
 * each step it can be held at ended by `end`, its case's end. It sets the act
 * environment aside, waits, lets one turn of the page's timer pass so what
 * the wait set off lands before the environment is put back, and puts it
 * back.
 *
 * RESTATED RATHER THAN WRAPPED, because that turn of the timer is the one
 * step a race around the wait does not reach, and it can be held. The
 * library takes `setTimeout` as the page has it, which under Vitest's fake
 * timers is a fake one nothing moves — the library advances Jest's clock
 * there and no other. So a wait that FOUND what it looked for sat in that
 * turn until its case ran out of time, and the case's end could not unwind
 * it: the act environment stayed set aside, and the end, which ends each
 * scope only once the one opened inside it has unwound, waited on it for
 * ever and ended none of the scopes outside it, so the next case's renders
 * were queued into a scope nobody closed.
 *
 * The two steps end differently, because they are different facts. A wait
 * still looking when its case ends is REFUSED: what it would find by then is
 * the next case's. A wait that already found is not refused — what it found
 * was the case's own — so the end only cuts its turn short, and the wait
 * hands back what it found as the library would have; whatever the late
 * case asks for next is refused there. Refused here instead, it pre-empted
 * the refusal a harness wait gives in its own words (the builder's testkit
 * wakes its waits with the lens's end, and they land in this turn).
 *
 * Everything else is the library's to the letter, so a live case waits
 * exactly as it did — the same timer, fake or not, the same environment set
 * aside and put back. The one branch left out advances Jest's fake clock,
 * and no suite here runs under Jest.
 */
async function waitUntilEnded<T>(wait: () => Promise<T>, end: Promise<void>): Promise<T> {
  const refused = end.then(() => {
    throw caseEnded("findBy/waitFor");
  });
  const before = reactGlobals.IS_REACT_ACT_ENVIRONMENT;
  reactGlobals.IS_REACT_ACT_ENVIRONMENT = false;
  try {
    const found = await Promise.race([wait(), refused]);
    await Promise.race([
      new Promise<void>((resolve) => {
        setTimeout(resolve, 0);
      }),
      end,
    ]);
    return found;
  } finally {
    reactGlobals.IS_REACT_ACT_ENVIRONMENT = before;
  }
}

/**
 * Binds the library's waits and events to the case whose async context makes
 * them. Run once, as this module loads — after the library, which it imports,
 * has configured itself.
 *
 * A WAIT IN A CASE ENDS WITH ITS CASE AT EVERY STEP ([waitUntilEnded]), so
 * when the case ends the wait unwinds — it puts back the act environment it
 * set aside — before the next case begins, and a wait still looking is
 * handed the refusal rather than an element the next case drew. A wait
 * outside every case is the library's own.
 */
function bindTestingLibrary(): void {
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
      return out.track(waitUntilEnded(wait, out.ended));
    },
    eventWrapper: (dispatch) => {
      caseOf("fireEvent")?.refuse("fireEvent");
      return dispatchAsLibrary(dispatch);
    },
  });
}

bindTestingLibrary();
