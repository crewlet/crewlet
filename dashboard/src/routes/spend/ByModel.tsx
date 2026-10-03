/**
 * "By model": which of the company's configured provider entries
 * (`providers.llm.<key>`) served the window's calls, the models each answered
 * with, and who leaned on it — the engine's `by_provider`.
 *
 * AN ENTRY, NOT A MODEL NAME. A fallback chain serves several models under one
 * key and one model can sit under several keys, so "which entry carried the
 * load" is its own question, and the one a ceiling or a credential is written
 * against. The models it answered with are named under it, from what each
 * completion reported.
 *
 * "USED BY" NAMES THE TOP THREE and counts the rest: the engine sends the
 * three biggest seats and how many there were (`seats_total`), so a row never
 * reads three names as the whole list — and the count is drawn apart from the
 * names, so cutting the names to the column never cuts the count.
 */

import { BarList, Card, Skeleton } from "@crewlethq/ui";
import { usedByParts, usedByWords } from "./model.ts";
import { fmtCount } from "~/lib/format.ts";
import { useSeatBadgeOf } from "~/lib/seats.ts";
import type { Rollup } from "~/protocol/types.ts";
import { DATA_COLORS } from "@crewlethq/ui";

export function ByModel({
  rollup,
  loading,
  words,
}: {
  rollup: Rollup | null;
  loading: boolean;
  words: string | null;
}) {
  const badgeOf = useSeatBadgeOf();
  const rows = (rollup?.by_provider ?? []).filter((p) => p.total_tokens > 0);
  return (
    <Card className="spend-card spend-list">
      <div className="spend-card-head">
        <h2 className="spend-title">By model</h2>
        {words && <span className="spend-caption">{words}</span>}
      </div>
      {loading && !rollup ? (
        <Skeleton height="4.5rem" />
      ) : (
        <BarList
          className="spend-models"
          layout="beside"
          limit={6}
          moreLabel={(n) => `${n} more ${n === 1 ? "entry" : "entries"}`}
          emptyLabel="No provider entry served a call in this window."
          data={rows.map((p) => {
            const used = usedByWords(p.seats, p.seats_total, (h) => badgeOf(h).name);
            const parts = usedByParts(p.seats, p.seats_total, (h) => badgeOf(h).name);
            return {
              id: p.provider_key,
              label: <span title={p.models.join(", ")}>{p.provider_key}</span>,
              // THE NAMES GIVE WAY, THE COUNT NEVER DOES: the column is one
              // line, and cut as one sentence it lost "and 4 more" first —
              // three names then read as the whole list. The whole sentence
              // is the hover.
              sub: used ? (
                <span className="spend-used" title={`Used by ${used}`}>
                  <span className="truncate">{parts.names}</span>
                  {parts.more > 0 && <span className="spend-used-more">and {parts.more} more</span>}
                </span>
              ) : (
                <span title={p.models.join(", ")}>{p.models.join(", ")}</span>
              ),
              value: p.total_tokens,
              display: fmtCount(p.total_tokens),
              // ONE HUE FOR ONE QUANTITY: a hue per entry would read as the
              // entries being different kinds of thing.
              color: DATA_COLORS[0],
            };
          })}
        />
      )}
    </Card>
  );
}
