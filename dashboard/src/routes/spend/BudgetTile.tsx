/**
 * "This month": the company's monthly token budget, as the budget gate counts
 * it — the fleet's shared counter for the current calendar month on the
 * company's clock, against its ceiling, in the ENGINE's state.
 *
 * NOT THE WINDOW ABOVE IT. The hero's figure is the window a reader chose; a
 * budget is a calendar window of its own (ADR-0019), so this tile says "this
 * month" and when it resets, and never divides one into the other.
 *
 * NO THRESHOLD HERE. The bar's colour is the window's `state` as the engine
 * judged it beside the counter (`near` at `BudgetNearFraction`, `refusing`
 * when the gate turned a charge away or none fits) — the kit meter is handed
 * that verdict and derives nothing.
 */

import { Meter, Skeleton } from "@crewlethq/ui";
import { href } from "~/app/router.tsx";
import { windowOf } from "~/lib/budget.ts";
import { companyDateLabel, fmtCount, fmtExact } from "~/lib/format.ts";
import { dayLabelIn } from "~/lib/range.ts";
import type { OrgBudget } from "~/protocol/types.ts";

export function BudgetTile({
  budget,
  raises,
}: {
  /** `null` until a node has reported the counters: see {@link OrgBudget}. */
  budget: OrgBudget | null;
  /**
   * The reader may set a ceiling: they hold `config:write`, the grant every
   * ceiling write takes — a grant check, never "is this an operator", which
   * every signed-in person now is.
   */
  raises: boolean;
}) {
  // NOT KNOWN IS NOT NONE. Before the first report nobody has read the
  // counter, and saying "No monthly budget" then — with "Set one" — told the
  // founder of a capped company, for the first seconds after every engine
  // start, to set a ceiling it already had. So the tile waits, and says so.
  if (budget === null) {
    return (
      <div
        className="spend-budget"
        role="status"
        aria-label="This month's budget: not reported yet"
      >
        <span className="spend-label">This month</span>
        <Skeleton width="100%" height="0.5rem" />
        <span className="spend-caption spend-muted">Waiting for the engine's first reading</span>
      </div>
    );
  }
  const month = windowOf(budget.org.windows, "month");
  if (!month || month.limit === undefined) {
    return (
      <div className="spend-budget spend-budget-none">
        <span className="spend-muted">No monthly budget</span>
        {/* WHERE A CEILING IS SET, for the one reader who can set it. Anybody
            else is told there is none, which is the whole of what they can
            act on. */}
        {raises && (
          <a className="t-link" href={href(["spend", "budgets"])}>
            Set one
          </a>
        )}
      </div>
    );
  }
  // THE COMPANY'S DATE the window turns over on — its first instant is the
  // next month's midnight on the company's clock, which on a reader's own
  // clock may be the evening before.
  const resets = companyDateLabel(dayLabelIn(Date.parse(month.resets_at), budget.timezone));
  return (
    <div className="spend-budget">
      <Meter
        value={month.used}
        max={month.limit}
        state={month.state}
        label="This month"
        valueText={`${fmtExact(month.used)} of ${fmtExact(month.limit)} tokens of the monthly budget, ${
          month.state === "refusing"
            ? "refusing charges"
            : month.state === "near"
              ? "nearly spent"
              : "within the ceiling"
        }, resets ${resets}`}
        hint={`${fmtCount(month.used)} of ${fmtCount(month.limit)} · resets ${resets}`}
      />
      {month.state === "refusing" && (
        <span className="spend-caption spend-refusing">
          Refusing charges — turns wait until {resets}, or until the ceiling is raised.
        </span>
      )}
    </div>
  );
}
