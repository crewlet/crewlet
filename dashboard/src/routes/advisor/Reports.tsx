/**
 * The Advisor's weekly reports (`#/advisor/reports`), as a timeline of weeks:
 * one card a week, the newest first, each its week's days, when it was
 * written and four figures beside the week before (turns, tokens, the share
 * of turns that changed nothing, and the findings it opened with what else
 * moved), opening its week's report (`report.ts` reads each card).
 *
 * A FIRST WEEK SAYS SO ONCE, in its head, and its figures carry no change.
 * No report yet is said with when one is written.
 */

import { useState } from "react";
import {
  Button,
  Callout,
  Card,
  EMPTY_VALUE,
  EmptyState,
  Skeleton,
  StatCard,
  StatGroup,
  Tag,
  Text,
} from "@crewlethq/ui";
import { QueryState } from "~/components/common.tsx";
import { href } from "~/app/router.tsx";
import { ago } from "~/lib/ago.ts";
import { useNow } from "~/lib/clock.ts";
import type { AdvisorReportSummary } from "~/protocol/advisorFindings.ts";
import { useReports } from "./api.ts";
import { deltaOf } from "./Numbers.tsx";
import { pagedOn } from "./paged.ts";
import { reportFigure, weekCards } from "./report.ts";
import type { ViewStat } from "./view.ts";
import { weekWords } from "./words.ts";

/**
 * One figure of a week, with its change against the week before: its parts
 * where it has them ("535 of 3,116 turns"), else what it is set beside.
 */
export function WeekTile({
  stat,
  flush = false,
  compared = true,
}: {
  stat: ViewStat;
  flush?: boolean;
  /** Say what a change is set beside; false where the page says it once for every figure. */
  compared?: boolean;
}) {
  const shown = stat.value === null ? null : reportFigure(stat.value, stat.unit);
  const delta = deltaOf(stat);
  const sub =
    shown === null
      ? "Not read this week"
      : (stat.detail ?? (delta && compared ? "vs the week before" : undefined));
  return (
    <StatCard
      label={stat.label}
      value={shown?.value ?? EMPTY_VALUE}
      {...(shown?.unit ? { unit: shown.unit } : {})}
      {...(delta ? { delta } : {})}
      {...(sub ? { sub } : {})}
      flush={flush}
      data-stat={stat.key}
    />
  );
}

/** The weekly reports, newest first, a card each, older ones a press away. */
export function Reports() {
  const [cursor, setCursor] = useState<string | null>(null);
  const [held, setHeld] = useState<AdvisorReportSummary[]>([]);
  const reports = useReports(cursor);
  const now = useNow();
  const rows = pagedOn(held, reports.data?.reports, (r) => r.week);
  const cards = weekCards(rows);
  // SAID ONCE, and only where a figure is set beside the week before.
  const compared = cards.some((c) => c.figures.some((f) => f.was !== null));
  return (
    <QueryState
      error={reports.error}
      detail={reports.detail}
      refusal={reports.refusal}
      loading={reports.loading && reports.data === null && held.length === 0}
      placeholder={<Skeleton variant="grid" rows={2} columns={1} label="Loading reports" />}
    >
      {rows.length === 0 && reports.data ? (
        <EmptyState
          size="compact"
          title="No report yet"
          description="The Advisor writes a report every Monday morning: the week's numbers against the week before, and what it found."
        />
      ) : (
        <>
          {compared ? (
            <Text as="p" variant="caption" tone="secondary">
              Each week against the week before it.
            </Text>
          ) : null}
          <ol className="advisor-weeks" aria-label="Weekly reports">
            {cards.map((c) => (
              <li key={c.week}>
                <Card
                  href={href(["advisor", "reports", c.week])}
                  padding="none"
                  aria-label={`${weekWords(c.week)}, ${c.days}`}
                >
                  <div className="advisor-week__head">
                    <Card.Title>{c.title}</Card.Title>
                    <Text as="span" tone="secondary">
                      {c.days}
                    </Text>
                    {c.first ? (
                      <Tag size="sm" variant="neutral">
                        First week measured
                      </Tag>
                    ) : null}
                    <Text
                      as="span"
                      variant="caption"
                      tone="secondary"
                      className="advisor-week__written"
                    >
                      Written {ago(c.writtenAt, now)}
                    </Text>
                  </div>
                  <StatGroup columns={4} flush>
                    {c.figures.map((f) => (
                      <WeekTile key={f.key} stat={f} compared={false} />
                    ))}
                  </StatGroup>
                </Card>
              </li>
            ))}
          </ol>
        </>
      )}
      {reports.data?.next_cursor ? (
        <Button
          size="small"
          variant="ghost"
          onClick={() => {
            setHeld(rows);
            setCursor(reports.data!.next_cursor);
          }}
        >
          Show older reports
        </Button>
      ) : null}
      {reports.error && held.length > 0 ? (
        <Callout variant="warning">Older reports could not be read. Try again in a moment.</Callout>
      ) : null}
    </QueryState>
  );
}
