/**
 * Spend › Overview's turns that changed nothing: per agent and what woke it,
 * how many of its turns committed nothing (no work moved and no words written),
 * what they spent, and who can make them happen less.
 */

import { useMemo } from "react";
import { CircleDashedGlyph } from "@crewlethq/icons/glyphs";
import { Card, Link, TableNumberCell } from "@crewlethq/ui";
import { href } from "~/app/router.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { QueryState } from "~/components/common.tsx";
import { fmtCount } from "~/lib/format.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { wakeWords } from "~/lib/wakes.ts";
import type { TurnOutcomeRow } from "~/protocol/turnOutcomes.ts";

export function NothingTurns({
  params,
  words,
}: {
  params: Record<string, unknown>;
  words: string | null;
}) {
  const answer = useQuery("turn_outcomes", params);
  // THE COSTLIEST FIRST: a row is worth reading for what its turns spent.
  const rows = useMemo(
    () =>
      (answer.data?.rows ?? [])
        .filter((r) => r.nothing > 0)
        .sort((a, b) => b.nothing_tokens - a.nothing_tokens),
    [answer.data],
  );
  return (
    <Card className="spend-card" padding="none">
      <Card.Header
        icon={<CircleDashedGlyph size="sm" />}
        subtitle={words ?? undefined}
        subtitlePlacement="below"
        count={rows.length}
      >
        <Card.Title>Turns that changed nothing</Card.Title>
      </Card.Header>
      <QueryState
        error={answer.error}
        refusal={answer.refusal}
        detail={answer.detail}
        loading={answer.loading && !answer.data}
      >
        <DataGrid<TurnOutcomeRow>
          name="nothing-turns"
          aria-label="Turns that changed nothing, by agent and what woke it"
          rows={rows}
          rowKey={(r) => `${r.agent_id}/${r.wake}`}
          empty={{
            title: "Every turn changed something",
            hint: "A turn that moves no work and writes no words shows up here, by what woke it.",
            icon: "circle-check",
          }}
          columns={[
            { key: "seat", header: "Agent", sortValue: (r) => r.seat, cell: (r) => r.seat },
            {
              key: "wake",
              header: "Woken by",
              sortValue: (r) => wakeWords(r.wake, r.seat).label,
              cell: (r) => <span className="truncate">{wakeWords(r.wake, r.seat).label}</span>,
            },
            {
              key: "nothing",
              header: "Changed nothing",
              align: "right",
              shrink: true,
              sortValue: (r) => r.nothing / r.turns,
              cell: (r) => (
                <span title={`${fmtCount(r.nothing)} of ${fmtCount(r.turns)} turns`}>
                  {fmtCount(r.nothing)} of {fmtCount(r.turns)} (
                  {Math.round((100 * r.nothing) / r.turns)}%)
                </span>
              ),
            },
            {
              key: "tokens",
              header: "Tokens",
              align: "right",
              shrink: true,
              sortValue: (r) => r.nothing_tokens,
              cell: (r) => <TableNumberCell value={r.nothing_tokens} format={fmtCount} />,
            },
            {
              key: "who",
              header: "Who can change it",
              drop: 1,
              cell: (r) => {
                const w = wakeWords(r.wake, r.seat);
                return w.path.length > 0 ? (
                  <Link href={href(w.path)} size="inherit">
                    {w.who}
                  </Link>
                ) : (
                  w.who
                );
              },
            },
          ]}
        />
      </QueryState>
    </Card>
  );
}
