/**
 * "Daily tokens by phase": one column per company day, stacked into the
 * engine's bands — by default its FOUR phase bands (`tokens.PhaseBand`:
 * execute and the executor's launches are Execute, review is Review, delegated workers
 * are Workers, and Auxiliary is everything that is not the turn's own work:
 * every call to the seat's auxiliary model — the turn-start context, every
 * compaction rewrite, the reflection workers, the background learning passes,
 * a person's answered question — the round-cap judge and the first-turn
 * onboarding), or split by model, provider, seat, unit or worker, where the
 * engine names the top four and folds the rest into one residual band.
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

import { Card, SegmentedControl, StackedColumns } from "@crewlethq/ui";
import { QueryState } from "~/components/common.tsx";
import { bandsOf, unbandedTokens } from "~/lib/spend.ts";
import { companyDateLabel, fmtCount, fmtExact } from "~/lib/format.ts";
import { dayLabelIn } from "~/lib/range.ts";

import type { QueryResult } from "~/lib/useQuery.ts";
import type { TokenSeries } from "~/protocol/types.ts";

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
  groups,
  onGroup,
  zone,
}: {
  series: QueryResult<TokenSeries>;
  group: string;
  /** The splits this reader may ask for (`GROUPS`, less an admin's alone). */
  groups: readonly { value: string; label: string }[];
  onGroup: (group: string) => void;
  zone: string | undefined;
}) {
  const data = series.data;
  // THE GROUPING THE ANSWER WAS BUILT WITH, not the control's: while a new
  // split is in flight the old answer is still on screen, and its legend must
  // name its own bands.
  const drawn = data?.group ?? group;
  // THE ENGINE'S LABEL FOR EVERY BAND (`bandsOf`): a seat band is keyed by
  // its agent id and labelled by the engine with its current name, so there
  // is no chart lookup to make here.
  const bands = bandsOf(data, drawn);
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
        refusal={series.refusal}
        detail={series.detail}
        loading={series.loading && !data}
        empty={
          data && data.totals.calls === 0
            ? {
                title: "No model calls in this window",
                hint: "Widen the window.",
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
                {drawn !== "phase" && (
                  <span className="spend-caption">
                    {drawn === "worker"
                      ? "Background work only"
                      : "The four biggest, and everything else as one band"}
                  </span>
                )}
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
            // THE PLOT TAKES THE ROW'S HEIGHT. Drawn at a fixed 12rem beside a
            // taller hero, it stopped short and the split control stood up to
            // 68px under the dates; filling, the figure flexes in this card and
            // the plot grows into the spare height, never under that 12rem.
            height="fill"
            minHeight="12rem"
            format={(v) => fmtCount(v)}
            formatTime={(at) => labels.get(at) ?? ""}
          />
        )}
      </QueryState>
      <div className="spend-chart-foot">
        <SegmentedControl
          semantics="radio"
          activate="manual"
          label="Split by"
          size="sm"
          value={group}
          onValueChange={onGroup}
          options={groups.map((g) => ({ value: g.value, label: g.label }))}
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
