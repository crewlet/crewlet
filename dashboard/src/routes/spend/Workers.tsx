/**
 * "Background workers": the duties that spend tokens outside any seat's turn
 * — reflection, summarisation, the embedding pass — from the rollup's
 * `by_worker`. Said separately because no agent's row carries them, so a
 * reader summing the agents would otherwise find the company's total larger
 * than the sum and nothing to say why.
 */

import { BarList, Card, DATA_COLORS, Skeleton } from "@crewlethq/ui";
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
        <h2 className="spend-title">Background workers</h2>
        {words && <span className="spend-caption">{words}</span>}
      </div>
      {loading && !rollup ? (
        <Skeleton height="4.5rem" />
      ) : (
        <BarList
          layout="beside"
          emptyLabel="No background worker spent tokens in this window."
          data={rows
            .slice()
            .sort((a, b) => b.total_tokens - a.total_tokens)
            .map((w) => ({
              id: w.worker,
              label: w.worker,
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
