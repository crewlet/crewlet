/**
 * A budget window in words, on the screens that draw windows.
 *
 * APART FROM `budget.ts`, which the frame reads through the attention queue: it
 * imports types alone, so what these words need of the date helpers stays out of
 * the first paint.
 */

import type { BudgetWindow } from "~/protocol/types.ts";
import { companyDateLabel, companyMonthLabel, relTime } from "./format.ts";
import { dayLabelIn } from "./range.ts";

/** A window's heading, over its figure or its bar: "Today", "This week". */
export const PERIOD_HEADING: Readonly<Record<BudgetWindow["period"], string>> = {
  day: "Today",
  week: "This week",
  month: "This month",
};

/**
 * The name a window's span carries: `Sep 29`, `Week 40`, `September`.
 *
 * ALL THREE FROM THE WINDOW'S LABEL, which the engine cut on the company's
 * clock: naming one reads no clock here, so a company zone this browser
 * cannot read names its windows as every other does (`companyMonthLabel`
 * says why the month's first instant would not).
 */
export function windowName(w: BudgetWindow): string {
  if (w.period === "day") return companyDateLabel(w.window);
  if (w.period === "week") return `Week ${w.window.slice(-2).replace(/^0/, "")}`;
  return companyMonthLabel(w.window);
}

/**
 * "Last refused a call 12m ago": the gate's own stamp, where it has one. LAST,
 * because the counter moves it to every refusal it records; "since" claimed
 * the window had been refusing for less time than it had. A full window that
 * has refused nothing yet says it is full.
 */
export function refusedWords(w: BudgetWindow, now: number): string {
  return w.refused_at
    ? `Last refused a call ${relTime(w.refused_at, now)}`
    : "No further charge fits";
}

/**
 * When a window turns over, on the company's clock: "today at midnight", or
 * the company date it turns over on ("on Nov 9"); undefined for a reset the
 * answer does not state.
 */
export function resetWords(w: BudgetWindow, zone: string | undefined): string | undefined {
  const at = Date.parse(w.resets_at);
  if (!Number.isFinite(at)) return undefined;
  return w.period === "day" ? "today at midnight" : `on ${companyDateLabel(dayLabelIn(at, zone))}`;
}
