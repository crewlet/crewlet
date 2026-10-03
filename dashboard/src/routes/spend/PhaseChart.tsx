/**
 * "Daily tokens by phase": one column per company day, stacked into the
 * engine's bands — by default its FOUR phase bands (`tokens.PhaseBand`:
 * execute and a coding run are Execute, review is Review, delegated workers
 * are Workers, the learning workers, the judge and onboarding are Auxiliary),
 * or split by model, provider, seat, unit or worker, where the engine names
 * the top four and folds the rest into one residual band.
 *
 * THE BUCKETING IS THE ENGINE'S. The usage domain holds whole company days and
 * the browser holds none of them, so an axis folded here would be a guess;
 * `token_series` answers every day of the window, the empty ones included, so
 * a quiet weekend is a gap rather than a column the chart squeezed out.
 *
 * A PHASE BAND KEEPS ITS COLOUR. `contract/spend.ts` `BANDS` gives each band
 * its data hue, gated against `tokens.Bands`, so Execute is the same blue
 * whichever band was biggest this month. Any other split takes the data ramp
 * by rank, with the residual in the residual's own neutral.
 */

import { Card, StackedColumns } from "@crewlethq/ui";
import { QueryState } from "~/components/common.tsx";
import { GROUPS } from "~/contract/spend.ts";
import { bandsOf, unbandedTokens } from "~/lib/spend.ts";
import { companyDateLabel, fmtCount, fmtExact } from "~/lib/format.ts";
import { dayLabelIn } from "~/lib/range.ts";
import { useSeatBadgeOf } from "~/lib/seats.ts";
import type { QueryResult } from "~/lib/useQuery.ts";
import type { TokenSeries } from "~/protocol/types.ts";
// OURS, AND DELIBERATELY: the kit's `SegmentedControl` commits the option the
// arrows land on, and every commit here re-asks `token_series` — six queries
// and six history entries for one sweep across the dimensions.
import { Segmented } from "~/ui/primitives.tsx";

/** What each split is called in the card's title: "Daily tokens by phase". */
const GROUP_NOUN: Record<string, string> = {
  phase: "phase",
  model: "model",
  provider: "provider",
  seat: "agent",
  unit: "team",
  worker: "background worker",
};

export function PhaseChart({
  series,
  group,
  onGroup,
  zone,
}: {
  series: QueryResult<TokenSeries>;
  group: string;
  onGroup: (group: string) => void;
  zone: string | undefined;
}) {
  const data = series.data;
  // THE GROUPING THE ANSWER WAS BUILT WITH, not the control's: while a new
  // split is in flight the old answer is still on screen, and its legend must
  // name its own bands.
  const drawn = data?.group ?? group;
  const badgeOf = useSeatBadgeOf();
  const bands = bandsOf(data, drawn, (handle) => badgeOf(handle).name);
  const today = dayLabelIn(Date.now(), zone);
  const labels = new Map(
    (data?.series ?? []).map((p) => [
      Date.parse(p.at),
      p.window === today ? "Today" : companyDateLabel(p.window),
    ]),
  );
  const buckets = (data?.series ?? []).map((p) => ({
    t: Date.parse(p.at),
    values: Object.fromEntries([
      ...bands.filter((b) => b.key !== "").map((b) => [b.key, p.groups[b.key]?.total_tokens ?? 0]),
      ["", p.other.total_tokens],
    ]),
  }));
  const unbanded = unbandedTokens(data);
  const noun = GROUP_NOUN[drawn] ?? drawn;

  return (
    <Card className="spend-card spend-chart">
      <QueryState
        error={series.error}
        detail={series.detail}
        loading={series.loading && !data}
        empty={
          data && data.totals.calls === 0
            ? {
                title: "No model calls in this window",
                hint: "Nothing ran, or nothing that ran reports tokens. Widen the window, or check that the seats you expect are running.",
              }
            : undefined
        }
      >
        {data && (
          <StackedColumns
            label={`Tokens per company day by ${noun}, ${data.from} to ${data.to}`}
            legend="head"
            head={
              <div className="spend-chart-head">
                <h2 className="spend-title">Daily tokens by {noun}</h2>
                <span className="spend-caption">
                  {drawn === "phase"
                    ? "Execute does the work; review, workers and auxiliary calls are the overhead"
                    : drawn === "worker"
                      ? "Background duties only — every other call is outside these bands"
                      : "The four biggest, and everything else as one band"}
                </span>
              </div>
            }
            // EVERY BAND THE ENGINE NAMED, in its order: a band's colour is
            // its place here. By phase that is always the FOUR, a band the
            // window never spent in answered at zero and kept in the legend
            // — the sentence above names all four, and a legend that lost
            // Workers in a quiet month disagreed with it. Its zero parts draw
            // nothing. Any other split lists only what spent.
            series={bands.map((b) => ({ id: b.key, name: b.label, color: b.color }))}
            buckets={buckets}
            height="12rem"
            format={(v) => fmtCount(v)}
            formatTime={(at) => labels.get(at) ?? ""}
          />
        )}
      </QueryState>
      <div className="spend-chart-foot">
        <Segmented
          ariaLabel="Split by"
          size="sm"
          value={group}
          onChange={onGroup}
          options={GROUPS.map((g) => ({ value: g.value, label: g.label }))}
        />
        {unbanded > 0 && data && (
          // THE GAP IS SAID OUT LOUD. Splitting by worker leaves out every
          // call that is not a worker's, and bands that sum to less than the
          // window's total otherwise read as spend that went missing.
          <span className="spend-caption">
            {fmtCount(unbanded)} of {fmtExact(data.totals.total_tokens)} tokens fall under no {noun}
          </span>
        )}
      </div>
    </Card>
  );
}
