/**
 * "Tokens by team": what each unit's seats spent over the window, from the
 * engine's own series grouped by unit (`token_series{group: unit}`).
 *
 * TOKENS, NEVER MONEY (rule 19): the engine's buckets carry no price this
 * dashboard declares. The rows are the kit's ranked bars in the design's
 * `beside` register: the name and its seat count, a bar, the figure at its
 * end.
 *
 * EACH TEAM'S BAR IS SPLIT BY SEAT. The engine answers every unit band with
 * the seats that spent in it (`parts`), from the same cells that made the
 * band, so the bar keeps the team's length and ranks as it did whole, and is
 * divided by seat, keyed under the bar with each seat's name and tokens. The
 * kit holds four data hues apart, so a team of more than four seats is drawn
 * as its three biggest and the rest as one part in the residual hue, as the
 * fifth series of every chart is. The residual ROW (every team past the
 * card's cap) is one bar in that residual hue: its parts would be seats from
 * teams the card does not name.
 */

import { BarList, Card, DATA_COLOR_OTHER, DATA_COLORS, type BarPart } from "@crewlethq/ui";
import { href } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { fmtCount, plural } from "~/lib/format.ts";
import { useSeatBadgeOf } from "~/lib/seats.ts";
import type { QueryResult } from "~/lib/useQuery.ts";
import type { SeriesPart, TokenSeries } from "~/protocol/index.ts";
import type { HomeRange } from "./model.ts";

/** How many teams the card ranks before the rest are counted. */
export const TEAM_ROWS = 5;

/** The part a team's smaller seats are drawn as. Never a seat's key: a handle has no colon. */
const REST_PART = "rest:";

export function TokensByTeam({
  spend,
  range,
}: {
  spend: QueryResult<TokenSeries>;
  range: HomeRange;
}) {
  const badgeOf = useSeatBadgeOf();
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
            // ONE HUE FOR A BAR THAT IS NOT SPLIT, the first data hue for a
            // team the engine answered without its seats and the residual
            // hue for the residual row, which is several teams.
            color: b.other ? DATA_COLOR_OTHER : DATA_COLORS[0],
            ...(b.other || !b.parts?.length
              ? {}
              : { parts: seatParts(b.parts, (key) => badgeOf(key).name) }),
          }))}
        />
      </QueryState>
    </Card>
  );
}

/**
 * A team's seats as the parts of its bar, biggest first: every seat while the
 * data hues hold them apart, else the three biggest and the rest as one part
 * in the residual hue. Each seat is named as the chart names it today, by its
 * handle, and by the key the engine answered where it has none.
 */
export function seatParts(
  parts: readonly SeriesPart[],
  nameOf: (key: string) => string,
): BarPart[] {
  const named = parts.length > DATA_COLORS.length ? parts.slice(0, DATA_COLORS.length - 1) : parts;
  const rest = parts.slice(named.length);
  const out: BarPart[] = named.map((part) => ({
    id: part.group,
    label: nameOf(part.handle || part.group),
    value: part.total_tokens,
    display: fmtCount(part.total_tokens),
  }));
  if (rest.length) {
    const value = rest.reduce((sum, part) => sum + part.total_tokens, 0);
    out.push({
      id: REST_PART,
      label: plural(rest.length, "more agent"),
      value,
      display: fmtCount(value),
      color: DATA_COLOR_OTHER,
    });
  }
  return out;
}
