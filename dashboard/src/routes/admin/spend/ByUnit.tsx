/**
 * "By team": each unit's seats' tokens over the window, from the engine's own
 * series grouped by unit (`token_series{group: unit}`) — only a seat's DIRECT
 * unit, so a department and the team inside it are never counted twice, and
 * the seats at the root of the chart are their own "no unit" row.
 *
 * ASKED FOR EVERY TEAM (`groups` at the engine's own ceiling), because this is
 * a ranking and not a chart: the chart's four-and-a-residual is right for
 * bands and wrong for a list whose fifth row is somebody's whole team.
 */

import { BarList, Card, DATA_COLORS, Skeleton } from "@crewlethq/ui";
import { href } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { fmtCount, plural } from "~/lib/format.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";

/** `tokens.MaxSeriesGroups`: the most bands the engine names in one answer. */
export const UNIT_GROUPS = 20;

export function ByUnit({
  params,
  words,
}: {
  params: Record<string, unknown>;
  words: string | null;
}) {
  const units = useQuery("token_series", {
    ...params,
    group: "unit",
    bucket: "day",
    groups: UNIT_GROUPS,
  });
  const rows = (units.data?.by_group ?? []).filter((b) => b.total_tokens > 0);
  // A BAND LINKS TO A TEAM ONLY WHERE THE CHART HAS ONE BY THAT NAME: the
  // root seats' band is "no unit", the residual is several, and a unit renamed
  // since the window still names its old days.
  const org = useOrg();
  const teams = new Set((org?.units ?? []).map((u) => u.name));
  return (
    <Card className="spend-card spend-list">
      <div className="spend-card-head">
        <h2 className="spend-title">By team</h2>
        {words && <span className="spend-caption">{words}</span>}
      </div>
      <QueryState error={units.error} detail={units.detail} loading={false}>
        {units.loading && !units.data ? (
          <Skeleton height="4.5rem" />
        ) : (
          <BarList
            layout="beside"
            limit={8}
            moreLabel={(n) => plural(n, "more team")}
            emptyLabel="No team's seats spent tokens in this window."
            data={rows.map((b) => ({
              id: b.group || "other",
              label: b.other ? "Everyone else" : b.group,
              sub: b.other
                ? plural(b.folded, "team")
                : b.seats
                  ? plural(b.seats, "agent")
                  : undefined,
              value: b.total_tokens,
              display: fmtCount(b.total_tokens),
              href: !b.other && teams.has(b.group) ? href(["agents", "teams", b.group]) : undefined,
              color: DATA_COLORS[0],
            }))}
          />
        )}
      </QueryState>
    </Card>
  );
}
