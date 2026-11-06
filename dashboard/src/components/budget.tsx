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

import { Meter, VisuallyHidden } from "@crewlethq/ui";
import {
  PERIOD_ADJECTIVE,
  PERIOD_HEADING,
  refusedWords,
  resetWords,
  windowName,
} from "~/lib/budget.ts";
import { useNow } from "~/lib/clock.ts";
import { fmtCount, fmtExact } from "~/lib/format.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import type { BudgetWindow } from "~/protocol/types.ts";

/**
 * Every capped window of one scope, as bars.
 *
 * WORDED AS COMPANY › BUDGETS WORDS THE SAME WINDOWS, through the same
 * helpers: "Today · Nov 6" over the bar, "Last refused a call 36m ago" and
 * "Resets today at midnight" under a full one. The wire's window key
 * (`2026-W45`) and its ISO stamps are values, never words.
 *
 * `whose` is the possessive the accessible name starts with ("The company's",
 * "Lead's"), so each bar is heard as whose ceiling and which window it is; the
 * page it sits on already says whose, so it is not drawn.
 */
export function WindowMeters({
  windows,
  whose,
}: {
  windows: readonly BudgetWindow[];
  whose: string;
}) {
  const now = useNow();
  const zone = useOrg()?.timezone;
  return (
    <div className="col gap-3">
      {windows.map((w) => {
        const resets = resetWords(w, zone);
        return (
          <div key={w.period} className="col gap-1">
            <Meter
              value={w.used}
              max={w.limit ?? 0}
              state={w.state}
              label={
                <>
                  {PERIOD_HEADING[w.period]} <span className="muted">{windowName(w)}</span>
                  <VisuallyHidden>
                    , {whose} {PERIOD_ADJECTIVE[w.period]} token budget
                  </VisuallyHidden>
                </>
              }
              valueText={`${fmtExact(w.used)} of ${fmtExact(w.limit ?? 0)} tokens`}
              hint={`${fmtCount(w.used)} / ${fmtCount(w.limit ?? 0)}`}
            />
            {w.state === "refusing" && (
              // THE GATE'S OWN WORD WHERE IT HAS ONE, and a full window that
              // has refused nothing yet says it is full.
              <p className="t-caption">
                {refusedWords(w, now)}.{resets ? ` Resets ${resets}.` : ""}
              </p>
            )}
          </div>
        );
      })}
    </div>
  );
}
