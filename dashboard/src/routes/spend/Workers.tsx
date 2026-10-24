/**
 * "Auxiliary model": what the seats' cheap model (`llm_auxiliary`) spent, by
 * what each call was FOR — the rollup's `by_worker`, which the engine files
 * every `auxiliary_spend` record under by its purpose: the turn-start memory
 * filter, knowledge query and episode summary, every compaction rewrite
 * (`condense_<kind>`), the reflection workers after a turn, the background
 * passes, and a person's answered question.
 *
 * A BREAKDOWN, NOT AN ADDITION: every one of these calls is already inside
 * the row of the seat — or the person — it was made for, under the Auxiliary
 * band. Said separately because no other card can say WHICH machinery spent
 * it, and that is the question a reader of a large auxiliary share has.
 *
 * Embedding calls are not here: they are metered nowhere, by design — see
 * docs/guides/budgets-and-spend.md.
 */

import { WrenchGlyph } from "@crewlethq/icons/glyphs";
import { BarList, Card, DATA_COLORS, EmptyState, Skeleton } from "@crewlethq/ui";
import { purposeLabel } from "~/lib/auxiliary.ts";
import { fmtCount, plural } from "~/lib/format.ts";
import type { Rollup } from "~/protocol/types.ts";

export function Workers({
  rollup,
  loading,
  words,
}: {
  rollup: Rollup | null;
  loading: boolean;
  words: string | null;
}) {
  const rows = (rollup?.by_worker ?? []).filter((w) => w.total_tokens > 0);
  return (
    <Card className="spend-card spend-list" padding="none">
      <Card.Header
        icon={<WrenchGlyph size="sm" />}
        subtitle={words ?? undefined}
        subtitlePlacement="below"
      >
        <Card.Title>Auxiliary model</Card.Title>
      </Card.Header>
      <Card.Body padding="md">
        {loading && !rollup ? (
          <Skeleton height="4.5rem" />
        ) : rows.length === 0 ? (
          // ONE EMPTY STATE FOR EVERY EMPTY CARD ON THE PAGE: a ranked list's
          // bare sentence beside the tables' and the chart's empty states read
          // as two kinds of answer to one question.
          <EmptyState
            size="compact"
            title="The auxiliary model spent no tokens in this window"
            description="Its calls appear here once it makes them."
          />
        ) : (
          <BarList
            className="spend-bars"
            layout="beside"
            data={rows
              .slice()
              .sort((a, b) => b.total_tokens - a.total_tokens)
              .map((w) => ({
                id: w.worker,
                label: purposeLabel(w.worker),
                sub: plural(w.calls, "call"),
                value: w.total_tokens,
                display: fmtCount(w.total_tokens),
                color: DATA_COLORS[0],
              }))}
          />
        )}
      </Card.Body>
    </Card>
  );
}
