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

import { BarList, Card, DATA_COLORS, Skeleton } from "@crewlethq/ui";
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
    <Card className="spend-card spend-list">
      <div className="spend-card-head">
        <h2 className="spend-title">Auxiliary model</h2>
        {words && <span className="spend-caption">{words}</span>}
      </div>
      {loading && !rollup ? (
        <Skeleton height="4.5rem" />
      ) : (
        <BarList
          layout="beside"
          emptyLabel="The auxiliary model spent no tokens in this window."
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
    </Card>
  );
}
