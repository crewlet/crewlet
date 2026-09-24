/**
 * Reading the engine's budget windows.
 *
 * NO THRESHOLD LIVES HERE. Every window arrives with the engine's own `state`
 * (`ok`, `near`, `refusing`), computed once beside the counter it describes,
 * and the screens used to hold three thresholds of their own — 75% on the
 * Budgets table, 90% in the attention queue and the kit meter's own — so one
 * window read as healthy, nearly spent and full at once depending on where it
 * was drawn. What is left for the client is WHICH window to name and how to
 * colour a state, both of which are presentation.
 */

import type { BudgetState } from "~/contract/spend.ts";
import type { BudgetWindow } from "~/protocol/types.ts";

/** How a period reads in front of "token budget": "the company's daily token
 *  budget". */
export const PERIOD_ADJECTIVE: Readonly<Record<BudgetWindow["period"], string>> = {
  day: "daily",
  week: "weekly",
  month: "monthly",
};

/**
 * The window in `state` a scope waits on longest: the one that turns over
 * LAST, the longer period where two turn over together.
 *
 * THE ENGINE'S TIE-BREAK (`coord.Outlasts`), which is how the budget gate
 * names the window it refused in and how a parked seat decides when to wake:
 * naming an earlier window would tell a reader the scope has room again at a
 * moment the gate will still refuse it. The windows arrive in day, week, month
 * order, so the later of two equal instants is the longer period.
 */
export function waitedOn(
  windows: readonly BudgetWindow[] | undefined,
  state: BudgetState,
): BudgetWindow | undefined {
  let out: BudgetWindow | undefined;
  for (const w of windows ?? []) {
    if (w.state !== state) continue;
    if (!out || Date.parse(w.resets_at) >= Date.parse(out.resets_at)) out = w;
  }
  return out;
}

/** The colour a window's state is drawn in. Colour carries state and nothing
 *  else, so the three states are three tones and no fraction picks one — and
 *  `ok` is NEUTRAL rather than the accent, which means where the reader is: a
 *  window with room is a number, not a state. */
export function stateTone(state: BudgetState): "neutral" | "caution" | "critical" {
  return state === "refusing" ? "critical" : state === "near" ? "caution" : "neutral";
}

/** The window of `period` in a scope's list, if the list states one. */
export function windowOf(
  windows: readonly BudgetWindow[] | undefined,
  period: BudgetWindow["period"],
): BudgetWindow | undefined {
  return windows?.find((w) => w.period === period);
}
