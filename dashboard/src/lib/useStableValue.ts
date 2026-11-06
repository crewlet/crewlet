/**
 * A reading that keeps its identity while it says the same thing.
 *
 * A poll, a focus re-read or a recheck hands back a NEW answer every time, the
 * same in every field, and everything keyed on it by identity (a memo, a
 * `memo` component) is drawn again for nothing a person can see: an idle
 * access table redrew every switch and picker on each re-read. This hands
 * back the value held while the new one is `sameValue` to it, as
 * [useStableList] does for a list's rows.
 */

import { useRef } from "react";
import { sameValue } from "~/protocol/equal.ts";

export function useStableValue<T>(value: T): T {
  const held = useRef(value);
  // WRITTEN DURING RENDER, as a cache: what it holds is a function of the
  // values drawn, so a render React discards computes the same answer again.
  if (held.current !== value && !sameValue(held.current, value)) held.current = value;
  return held.current;
}
