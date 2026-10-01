/**
 * Every `act`, every wait and every event a suite makes, bound to the case
 * that made it — and the case-bound [poll] beside them. Imported by tests
 * only; `cases.ts` is the case's lifetime this binds to, and says why a case
 * that times out reaches the next one at all.
 *
 * # What is bound here
 *
 * - [act], the library's own, refused once its case has ended and an async
 *   scope ended with its case. A suite reaches `act` only through this module,
 *   which `inCase.source.test.ts` holds it to.
 * - The library's waits (`findBy*`, `waitFor`) and events (`fireEvent`),
 *   through its two hooks, `asyncWrapper` and `eventWrapper`, wrapped once
 *   as this module loads ([bindTestingLibrary]). `setup.ts` loads it for every
 *   file with a document, so a suite that never imports it — that only
 *   renders and waits — is bound all the same.
 *
 * What is not ended: the library's poll behind a wait that was refused goes
 * on to its own one-second deadline, but its answer goes nowhere and it opens
 * no scope; and a late case can still read the page or `render` into it,
 * neither of which leaves a scope open past its own call.
 */

import { act as libraryAct, configure, getConfig } from "@testing-library/react";
import { caseEnded, caseOf, isThenable } from "./cases.ts";

export { poll, type PollOptions } from "./cases.ts";

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
 * them. Run once, as this module loads — after the library, which it imports,
 * has configured itself.
 *
 * A WAIT RACES ITS CASE'S END INSIDE THE LIBRARY'S OWN WRAPPER, so when the
 * case ends the wrapper unwinds — it puts back the act environment it set
 * aside — before the next case begins, and the case is handed the refusal
 * rather than an element the next case drew.
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

bindTestingLibrary();
