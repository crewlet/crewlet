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
 *   through its two hooks, `asyncWrapper` and `eventWrapper`, wrapped once
 *   as this module loads ([bindTestingLibrary]).
 *
 * What is not ended: the library's poll behind a wait that was refused goes
 * on to its own one-second deadline, but its answer goes nowhere and it opens
 * no scope; and a late case can still READ the page, which moves nothing.
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
