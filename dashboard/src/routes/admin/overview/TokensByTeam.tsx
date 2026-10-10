/**
 * "Tokens by team" — what each unit's seats spent over the window, from the
 * engine's own series grouped by unit (`token_series{group: unit}`).
 *
 * TOKENS, NEVER MONEY (rule 19): the engine's buckets carry no price this
 * dashboard declares. The rows are the kit's ranked bars in the design's
 * `beside` register — the name and its seat count, a bar, the figure at its
 * end — in the first data hue, because the bars are one quantity.
 */

import { BarList, Card, DATA_COLORS } from "@crewlethq/ui";
import { href } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { fmtCount, plural } from "~/lib/format.ts";
import type { QueryResult } from "~/lib/useQuery.ts";
import type { TokenSeries } from "~/protocol/index.ts";
import type { HomeRange } from "./model.ts";

/** How many teams the card ranks before the rest are counted. */
export const TEAM_ROWS = 5;

export function TokensByTeam({
  spend,
  range,
}: {
  spend: QueryResult<TokenSeries>;
  range: HomeRange;
}) {
  const data = spend.data?.totals ? spend.data : null;
  const bands = (data?.by_group ?? []).filter((b) => b.total_tokens > 0);
  const window = range.days === 1 ? "Today" : `Last ${range.words}`;
  return (
    <Card className="home-card home-chart">
      <div className="home-chart-head">
        <div className="home-chart-title">
          <h3 className="home-card-title">Tokens by team</h3>
          <span className="home-card-caption">
            {data ? `${window} · ${fmtCount(data.totals.total_tokens)} total` : window}
          </span>
        </div>
        <a className="t-link" href={href(["spend"], { group: "unit" })}>
          Spend
        </a>
      </div>
      <QueryState
        error={spend.error}
        loading={spend.loading && !data}
        empty={
          data && bands.length === 0
            ? {
                title: "No tokens spent in this window",
                hint: "A team's bar appears once one of its seats makes a model call.",
              }
            : undefined
        }
      >
        <BarList
          layout="beside"
          limit={TEAM_ROWS}
          moreLabel={(n) => `${plural(n, "more team")}`}
          data={bands.map((b) => ({
            id: b.group || "other",
            label: b.other ? "Everyone else" : b.group,
            sub: b.other
              ? plural(b.folded, "team")
              : b.seats
                ? plural(b.seats, "agent")
                : undefined,
            value: b.total_tokens,
            display: fmtCount(b.total_tokens),
            // ONE HUE FOR ONE QUANTITY: a hue per team would read as the
            // teams being different kinds of thing.
            color: DATA_COLORS[0],
          }))}
        />
      </QueryState>
    </Card>
  );
}
