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

import { BarList, Card, DATA_COLORS, EmptyState, Skeleton } from "@crewlethq/ui";
import { usedByParts, usedByWords } from "./model.ts";
import { fmtCount } from "~/lib/format.ts";
import { UNSPENT_CALLS, callsWords, spentOnly } from "~/lib/spend.ts";
import { useSeatBadgeOf } from "~/lib/seats.ts";
import type { Rollup } from "~/protocol/types.ts";

export function ByModel({
  rollup,
  loading,
  words,
}: {
  rollup: Rollup | null;
  loading: boolean;
  /** The window, where nothing else on the screen names it (a Home widget). */
  words?: string | null;
}) {
  const badgeOf = useSeatBadgeOf();
  // WHERE TOKENS WENT, so an entry whose calls spent nothing is no row of it
  // (`spentOnly`); the empty state says those calls were made.
  const rows = spentOnly(rollup?.by_provider ?? []);
  const calls = rollup?.totals.calls ?? 0;
  return (
    <Card className="spend-card spend-list" padding="none">
      <Card.Header subtitlePlacement="below" subtitle={words ?? undefined}>
        <Card.Title>By model</Card.Title>
      </Card.Header>
      <Card.Body padding="md">
        {loading && !rollup ? (
          <Skeleton height="4.5rem" />
        ) : rollup?.entries_withheld ? (
          <EmptyState
            size="inline"
            title="Shown to admins"
            description="Which of the company's model entries served the calls is its configuration, which an admin reads."
          />
        ) : rows.length === 0 ? (
          // ONE EMPTY STATE FOR EVERY EMPTY CARD ON THE PAGE: a ranked list's
          // bare sentence beside the tables' and the chart's empty states read
          // as two kinds of answer to one question. AND IT COUNTS THE CALLS
          // MODELS COUNTS: "no model served a call" over a window of 887
          // refused calls contradicted Settings › Models & Fleets, which counts them.
          calls > 0 ? (
            <EmptyState
              size="inline"
              title="No model spent tokens in this window"
              description={`${callsWords(calls, 0)}. ${UNSPENT_CALLS}`}
            />
          ) : (
            <EmptyState
              size="inline"
              title="No model was called in this window"
              description="A model entry appears once a call is made to it."
            />
          )
        ) : (
          <BarList
            className="spend-bars"
            layout="beside"
            limit={6}
            moreLabel={(n) => `${n} more ${n === 1 ? "entry" : "entries"}`}
            data={rows.map((p) => {
              const used = usedByWords(p.seats, p.seats_total, (h) => badgeOf(h).name);
              const parts = usedByParts(p.seats, p.seats_total, (h) => badgeOf(h).name);
              return {
                id: p.provider_key,
                label: <span title={p.models.join(", ")}>{p.provider_key}</span>,
                // THE NAMES GIVE WAY, THE COUNT NEVER DOES: the column is one
                // line, and cut as one sentence it lost "and 4 more" first,
                // three names then read as the whole list. The whole sentence
                // is the hover.
                sub: used ? (
                  <span className="spend-used" title={`Used by ${used}`}>
                    <span className="truncate">{parts.names}</span>
                    {parts.more > 0 && (
                      <span className="spend-used-more">and {parts.more} more</span>
                    )}
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
      </Card.Body>
    </Card>
  );
}
