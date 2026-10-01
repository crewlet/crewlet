/**
 * A value a screen derives, keeping the objects it derived last time.
 *
 * Every answer that reaches a screen is already shared with the one it
 * replaced (`~/protocol/share.ts`), so a poll that changed one row hands back
 * the old object for every other. A screen that BUILDS its rows from an answer
 * — the audit folds four sources into one shape — undoes that: its derivation
 * runs again whenever any source moves and makes every row a new object, so a
 * poll that changed one tracker commit drew every row of all four. Passed
 * through this hook, the rows nothing changed are the objects drawn last time.
 */

import { useLayoutEffect, useRef } from "react";
import { share } from "~/protocol/index.ts";

/**
 * `value`, shared with what the last committed render of this hook returned.
 *
 * AGAINST THE COMMITTED VALUE, kept in a layout effect rather than written
 * during render: a render React throws away must not become the value the
 * next one is compared with. Sharing is correct against any earlier value —
 * it only decides which deep-equal object is returned — so this is about
 * keeping the identity the screen actually drew, not about correctness.
 */
export function useShared<T>(value: T): T {
  const held = useRef<T>(value);
  const shared = share(held.current, value);
  useLayoutEffect(() => {
    held.current = shared;
  });
  return shared;
}
