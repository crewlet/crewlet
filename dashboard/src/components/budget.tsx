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
 * own; every bar here is handed the window's `state` itself, so a
 * window reads the same here as in the attention queue and on the Budgets
 * screen, and the fraction is never a second opinion beside it. A window that
 * refused a round reads past its ceiling, because the refused round is counted
 * (the vendor billed it), and its figures say so rather than the ceiling.
 */

import { Meter } from "@crewlethq/ui";
import { PERIOD_ADJECTIVE, PERIOD_WORDS, turnsOverWords, windowName } from "~/lib/budget.ts";
import { useNow } from "~/lib/clock.ts";
import { fmtCount, fmtExact, relTime } from "~/lib/format.ts";
import type { BudgetWindow } from "~/protocol/types.ts";

/**
 * Every capped window of one scope, as bars.
 *
 * `whose` is the possessive the labels start with — "The company's",
 * "Lead's" — so each bar's accessible name says whose ceiling and which
 * window it is. `zone` is the company's clock, which every window is cut on
 * and turns over by.
 *
 * EVERY VALUE AS THE BUDGETS SCREEN SAYS IT. The labels and the refusing
 * caption printed the engine's own values — the window's label (`2026-W41`),
 * `refused_at` and `resets_at` as raw ISO instants — so a seat's page named
 * one window three ways from the screen its ceiling is raised on.
 */
export function WindowMeters({
  windows,
  whose,
  zone,
}: {
  windows: readonly BudgetWindow[];
  whose: string;
  zone: string | undefined;
}) {
  const now = useNow();
  return (
    <div className="col gap-3">
      {windows.map((w) => {
        const when = turnsOverWords(w, zone);
        return (
          <div key={w.period} className="col gap-1">
            <Meter
              value={w.used}
              max={w.limit ?? 0}
              state={w.state}
              label={`${whose} ${PERIOD_ADJECTIVE[w.period]} token budget, ${windowName(w)}`}
              valueText={`${fmtExact(w.used)} of ${fmtExact(w.limit ?? 0)} tokens ${PERIOD_WORDS[w.period]}`}
              hint={`${fmtCount(w.used)} / ${fmtCount(w.limit ?? 0)}`}
            />
            {w.state === "refusing" && (
              // THE GATE'S OWN WORD WHERE IT HAS ONE. The stamp is when a
              // call was LAST turned away — the counter moves it to every
              // refusal it records — and a window with no room left for a
              // single token refuses the next one whatever its size, and
              // says so before any has been.
              <p className="t-caption">
                {w.refused_at
                  ? `Last refused a call ${relTime(w.refused_at, now)}.`
                  : "No further charge fits, so turns are declined at the gate."}{" "}
                {`Room comes back when the ${w.period} turns over${when ? ` ${when}` : ""}, or when the ceiling is raised.`}
              </p>
            )}
          </div>
        );
      })}
    </div>
  );
}
