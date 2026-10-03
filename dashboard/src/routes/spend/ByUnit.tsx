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

import { useMemo } from "react";
import { BarList, Card, DATA_COLORS, Skeleton } from "@crewlethq/ui";
import { href } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { fmtCount, plural } from "~/lib/format.ts";
import { indexOrg, unitByKey, unitPath } from "~/lib/seats.ts";
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
  // A BAND IS A UNIT'S KEY, labelled by the engine with its name — two units
  // may share a name, so a band keyed on one pooled them — and it LINKS TO A
  // TEAM ONLY WHERE THE CHART HOLDS ONE BY THAT KEY: the root seats' band is
  // "no unit" and the residual is several. A key a unit has since given up
  // still resolves to it (`unitByKey`), and the link opens it by the key it
  // answers to now.
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  return (
    <Card className="spend-card spend-list">
      <div className="spend-card-head">
        <h2 className="spend-title">By team</h2>
        {words && <span className="spend-caption">{words}</span>}
      </div>
      <QueryState
        error={units.error}
        refusal={units.refusal}
        detail={units.detail ?? undefined}
        loading={false}
      >
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
              label: b.other ? "Everyone else" : b.label || b.group,
              sub: b.other
                ? plural(b.folded, "team")
                : b.seats
                  ? plural(b.seats, "agent")
                  : undefined,
              value: b.total_tokens,
              display: fmtCount(b.total_tokens),
              href: teamHref(b.other ? null : unitByKey(index, b.group)),
              color: DATA_COLORS[0],
            }))}
          />
        )}
      </QueryState>
    </Card>
  );
}

/** A team's page, where the chart holds the unit a band names. */
function teamHref(unit: Parameters<typeof unitPath>[0] | null): string | undefined {
  return unit ? href(unitPath(unit)) : undefined;
}
