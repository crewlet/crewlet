/**
 * The live token meters of one scope: a bar per capped calendar window.
 *
 * ONE BAR PER WINDOW, because a scope capped by the day and by the month has
 * two ceilings and a single bar can only be drawn against one of them — the
 * meter this replaced drew whichever window was closest to its ceiling, so the
 * bar's scale jumped between a day's and a month's as the two moved.
 *
 * THE COLOUR IS THE ENGINE'S STATE, never the fill. The kit's `Meter` derives
 * a tone from the fraction when it is given none, which is a threshold of its
 * own; every bar here is handed the tone its window's `state` names, so a
 * window reads the same here as in the attention queue and on the Budgets
 * screen. A refusing window below its ceiling is the case that matters: a
 * refused charge increments nothing, so the fraction alone would draw the one
 * window the gate is turning turns away as the calmest bar on the screen.
 */

import { Meter, type MeterTone } from "@crewlethq/ui";
import type { BudgetState } from "~/contract/spend.ts";
import { PERIOD_ADJECTIVE } from "~/lib/budget.ts";
import { fmtCount, fmtExact } from "~/lib/format.ts";
import type { BudgetWindow } from "~/protocol/types.ts";

/** The kit tone each engine state is drawn in: `ok` is neutral, because the
 *  accent means where the reader is and a window with room is not a state. */
const TONE: Readonly<Record<BudgetState, MeterTone>> = {
  ok: "neutral",
  near: "warning",
  refusing: "danger",
};

/**
 * Every capped window of one scope, as bars.
 *
 * `whose` is the possessive the labels start with — "The company's",
 * "Lead's" — so each bar's accessible name says whose ceiling and which
 * window it is.
 */
export function WindowMeters({
  windows,
  whose,
}: {
  windows: readonly BudgetWindow[];
  whose: string;
}) {
  return (
    <div className="col gap-3">
      {windows.map((w) => (
        <div key={w.period} className="col gap-1">
          <Meter
            value={w.used}
            max={w.limit ?? 0}
            tone={TONE[w.state]}
            label={`${whose} ${PERIOD_ADJECTIVE[w.period]} token budget, ${w.window}`}
            valueText={`${fmtExact(w.used)} of ${fmtExact(w.limit ?? 0)} tokens`}
            hint={`${fmtCount(w.used)} / ${fmtCount(w.limit ?? 0)}`}
          />
          {w.state === "refusing" && (
            // THE GATE'S OWN WORD WHERE IT HAS ONE. The stamp is when a
            // charge was actually turned away; a window with no room left
            // for a single token refuses the next one whatever its size,
            // and says so before any has been.
            <p className="t-caption">
              {w.refused_at
                ? `Refusing charges since ${w.refused_at}.`
                : "No further charge fits, so turns are declined at the gate."}{" "}
              Room comes back when {w.window} turns over at {w.resets_at}, or when the ceiling is
              raised.
            </p>
          )}
        </div>
      ))}
    </div>
  );
}
