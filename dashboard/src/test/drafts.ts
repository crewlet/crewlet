/**
 * A draft's branded state for a suite (`app/assistant/draft.ts`).
 *
 * A screen mints a write only from the branded state it holds its draft in,
 * and a suite that tests a spec, a field or an action without rendering the
 * screen holds none. So the suites take theirs from here, cast as the
 * screen's own would be minted: the draft gate refuses that cast in a module
 * that ships, and reads neither this file nor a suite.
 */

import type { DraftDispatch, DraftWrite } from "~/app/assistant/draft.ts";

/** A write that hands each change to `apply`, as a screen's branded state would take it. */
export function heldWrite<D>(apply: (next: D) => void = () => {}): DraftWrite<D> {
  return apply as DraftWrite<D>;
}

/** A dispatch that hands each action to `dispatch`, as a screen's branded reducer would take it. */
export function heldDispatch<A>(dispatch: (action: A) => void = () => {}): DraftDispatch<A> {
  return dispatch as DraftDispatch<A>;
}
