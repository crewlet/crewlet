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

import { UsersGlyph } from "@crewlethq/icons/glyphs";
import { useMemo } from "react";
import { BarList, Card, DATA_COLORS, EmptyState, Skeleton } from "@crewlethq/ui";
import { href } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { fmtCount, plural } from "~/lib/format.ts";
import { indexOrg, unitByKey, unitRoute, type Unit } from "~/lib/seats.ts";
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
  // "no unit" and the residual is several.
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  return (
    <Card className="spend-card spend-list" padding="none">
      <Card.Header icon={<UsersGlyph size="sm" />} subtitle={words ?? undefined}>
        <Card.Title>By team</Card.Title>
      </Card.Header>
      <Card.Body padding="md">
        <QueryState
          error={units.error}
          refusal={units.refusal}
          detail={units.detail}
          loading={false}
        >
          {units.loading && !units.data ? (
            <Skeleton height="4.5rem" />
          ) : rows.length === 0 ? (
            // ONE EMPTY STATE FOR EVERY EMPTY CARD ON THE PAGE: a ranked list's
            // bare sentence beside the tables' and the chart's empty states read
            // as two kinds of answer to one question.
            <EmptyState
              size="compact"
              title="No team's seats spent tokens in this window"
              description="A team appears once its seats spend tokens."
            />
          ) : (
            <BarList
              className="spend-bars"
              layout="beside"
              limit={8}
              moreLabel={(n) => plural(n, "more team")}
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
      </Card.Body>
    </Card>
  );
}

/** A team's page, where the chart holds the unit a band names. */
function teamHref(unit: Unit | null): string | undefined {
  return unit ? href(unitRoute(unit)) : undefined;
}
