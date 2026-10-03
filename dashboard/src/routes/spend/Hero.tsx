/**
 * The Spend hero: the window's tokens, the change against the window before,
 * how far back the history reaches, and three tiles — the monthly budget, the
 * prompt cache's share of the input, and the median task.
 *
 * Each tile is its own file (`BudgetTile`, `CacheTile`, `TaskMedianTile`)
 * because each answers a different question from a different source, and says
 * when it has no answer rather than drawing a zero.
 */

import { Card, EmptyValue, Skeleton } from "@crewlethq/ui";
import { changeVsPrevious, changeWords } from "./model.ts";
import { BudgetTile } from "./BudgetTile.tsx";
import { CacheTile } from "./CacheTile.tsx";
import { TaskMedianTile } from "./TaskMedianTile.tsx";
import { companyDateLabel, fmtCount, fmtExact } from "~/lib/format.ts";
import type { QueryResult } from "~/lib/useQuery.ts";
import type { OrgBudget, Rollup, TokenSeries } from "~/protocol/types.ts";

export function Hero({
  rollup,
  loading,
  words,
  named,
  prior,
  budget,
  raises,
}: {
  rollup: Rollup | null;
  loading: boolean;
  /** The window in words, from the answer ("last 30 days"), or null before it. */
  words: string | null;
  /** Whether the window is a named range, which the comparison names alike. */
  named: boolean;
  prior: QueryResult<TokenSeries>;
  /** `null` until a node has reported the counters. */
  budget: OrgBudget | null;
  /** The reader may set a ceiling — holds `config:write`. */
  raises: boolean;
}) {
  const total = rollup?.totals.total_tokens;
  return (
    <Card className="spend-card spend-hero">
      <div className="spend-hero-figure">
        <span className="spend-label">{words ? `Tokens · ${words}` : "Tokens"}</span>
        {rollup ? (
          <span className="spend-hero-number" title={`${fmtExact(total)} tokens`}>
            {fmtCount(total)}
          </span>
        ) : loading ? (
          <Skeleton width="10rem" height="3.25rem" />
        ) : (
          <span className="spend-hero-number">
            <EmptyValue label="This window has not answered" />
          </span>
        )}
        {rollup && <Comparison current={total ?? 0} prior={prior} named={named} rollup={rollup} />}
        {rollup?.horizon && (
          // THE FLOOR, SAID. A window reaches back as far as the usage domain
          // keeps, and one past it is refused rather than drawn short — so the
          // reader is told where the history ends before they ask past it.
          <span className="spend-caption">
            History from {companyDateLabel(rollup.horizon.floor)} · {rollup.horizon.days} days, on
            every node alike
          </span>
        )}
      </div>
      <BudgetTile budget={budget} raises={raises} />
      <div className="spend-tiles">
        <CacheTile totals={rollup?.totals ?? null} loading={loading} words={words} />
        <TaskMedianTile since={rollup?.since} until={rollup?.until} words={words} />
      </div>
    </Card>
  );
}

/**
 * "+18% vs previous 30 days" — the window against the one before it, as the
 * ENGINE cut it (`previous=true`): subtracting here would cut the two windows
 * on this browser's clock, and a comparison of two different weeks reports a
 * change nobody made.
 */
function Comparison({
  current,
  prior,
  named,
  rollup,
}: {
  current: number;
  prior: QueryResult<TokenSeries>;
  named: boolean;
  rollup: Rollup;
}) {
  const before = named && rollup.days ? `previous ${rollup.days} days` : "the window before";
  if (prior.error) {
    // THE WINDOW BEFORE A LONG ENOUGH ONE STARTS PAST THE HISTORY, and the
    // engine refuses it naming why — which is what the reader is told.
    return (
      <span className="spend-hero-change" title={prior.detail ?? undefined}>
        No comparison — {prior.detail ?? "the window before this one could not be read"}
      </span>
    );
  }
  if (!prior.data) return <span className="spend-hero-change">&nbsp;</span>;
  const change = changeVsPrevious(current, prior.data.totals.total_tokens);
  if (change === null) {
    return (
      <span className="spend-hero-change">
        <span className="spend-muted">Nothing was spent in the {before}</span>
      </span>
    );
  }
  return (
    <span className="spend-hero-change">
      {changeWords(change)} <span className="spend-muted">vs {before}</span>
    </span>
  );
}
