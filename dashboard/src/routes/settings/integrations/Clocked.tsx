/**
 * A relative time printed as a leaf of the shared clock.
 *
 * A pane that read [useNow] re-rendered everything under it every second (an
 * access table of switches and pickers) for a few "5m ago" strings. A leaf
 * selects its own words through [useClock], so it re-renders only when they
 * change and nothing around it re-renders at all.
 */

import { useClock } from "~/lib/clock.ts";

export function Clocked({ text }: { text: (now: number) => string }) {
  return <>{useClock(text)}</>;
}
