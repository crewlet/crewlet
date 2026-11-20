/**
 * One weekly report (`#/advisor/reports/{week}`), read as a report:
 *
 * - its head: the week, its days, when it was written, the weeks either side,
 *   and, for the first week measured, that it is one;
 * - the model's summary where the Advisor has a model;
 * - the week's four headline figures, each beside the week before
 *   (`report.ts` chooses them);
 * - the week across its days, where the report kept them: turns by whether
 *   they committed anything, and tokens;
 * - what the week moved, in a strip: findings opened, resolved, applied and
 *   dismissed;
 * - what cost the most, as the open findings' rows, each opening its page;
 * - every other number of the week, each with its change.
 *
 * A source the week could not read is said, since its numbers are left out.
 * The report is kept as it was written: a finding it names reads as it did
 * that Monday, and its page says what became of it since.
 */

import {
  AttentionList,
  ButtonLink,
  Callout,
  Card,
  DescriptionList,
  Link,
  StatGroup,
  Tag,
  Text,
} from "@crewlethq/ui";
import { QueryState } from "~/components/common.tsx";
import { usePageLabels } from "~/app/Shell.tsx";
import { href } from "~/app/router.tsx";
import { fmtCount, fmtDateTime } from "~/lib/format.ts";
import { unreadNames } from "~/lib/advisorUnread.ts";
import type { AdvisorReport } from "~/protocol/advisorFindings.ts";
import { marked } from "~/ui/Problems.tsx";
import { useReport, useReports } from "./api.ts";
import { ChartCard } from "./Numbers.tsx";
import { dayCharts, firstWeek, otherRows, weekFigures } from "./report.ts";
import { WeekTile } from "./Reports.tsx";
import { chartView } from "./view.ts";
import { reportSubject, toneOf, weekSpan, weekWords } from "./words.ts";

export function ReportPage({ week }: { week: string }) {
  const read = useReport(week);
  const report = read.data;
  const title = weekWords(week);
  usePageLabels(report ? { [week]: title } : {});
  return (
    <QueryState
      error={read.error}
      refusal={read.refusal}
      detail={read.detail}
      loading={read.loading && !report}
    >
      {report ? <Report report={report} title={title} /> : null}
    </QueryState>
  );
}

function Report({ report, title }: { report: AdvisorReport; title: string }) {
  const first = firstWeek(report.metrics);
  const charts = dayCharts(report).map(chartView);
  const others = otherRows(report.metrics);
  const from = Date.parse(report.period.from);
  const to = Date.parse(report.period.to);
  return (
    <article className="advisor-report" aria-label={title}>
      <header className="advisor-report__head">
        <div className="advisor-report__title">
          <Text as="h2" variant="heading">
            {title}
          </Text>
          <Text as="p" tone="secondary">
            {weekSpan(report.week) ??
              `${fmtDateTime(report.period.from)} to ${fmtDateTime(report.period.to)}`}
            . Written {fmtDateTime(report.written_at)}.
          </Text>
          {first ? (
            <Text as="p" tone="secondary">
              <Tag size="sm" variant="neutral">
                First week measured
              </Tag>{" "}
              Next week's report sets its numbers beside these.
            </Text>
          ) : null}
        </div>
        <Neighbours week={report.week} />
      </header>
      {report.unread.length > 0 ? (
        <Callout variant="warning" title="Some numbers are left out">
          The Advisor couldn't read {unreadNames(report.unread)} for this week, so the numbers drawn
          from them are not in this report.
        </Callout>
      ) : null}
      {report.summary ? (
        <Card padding="md">
          <Card.Title>The Advisor's summary</Card.Title>
          <Text as="p">{marked(report.summary)}</Text>
        </Card>
      ) : null}
      {report.metrics.length > 0 ? (
        <StatGroup columns={4} aria-label="The week's numbers">
          {weekFigures(report.metrics).map((f) => (
            <WeekTile key={f.key} stat={f} />
          ))}
        </StatGroup>
      ) : null}
      {charts.length > 0 ? (
        <div className="advisor-charts" data-columns={charts.length} aria-label="The week by day">
          {charts.map((c) => (
            <ChartCard key={c.id} chart={c} stat={null} days={7} why={null} from={from} to={to} />
          ))}
        </div>
      ) : null}
      <section className="advisor-report__section" aria-labelledby="advisor-week-moved">
        <Text as="h3" variant="label" id="advisor-week-moved">
          What the week moved
        </Text>
        <ul className="advisor-moved">
          {(
            [
              ["Findings opened", report.opened],
              ["Resolved", report.resolved],
              ["Applied", report.applied],
              ["Dismissed", report.dismissed],
            ] as const
          ).map(([word, n]) => (
            <li key={word}>
              <span className="advisor-moved__count">{fmtCount(n)}</span> {word}
            </li>
          ))}
        </ul>
      </section>
      {report.top.length > 0 ? (
        <AttentionList
          items={report.top.map((f) => ({
            key: f.id,
            tone: toneOf(f.severity),
            subject: f.subject ? reportSubject(f.subject) : undefined,
            title: (
              <Link href={href(["advisor", "findings", f.id])} size="body">
                {marked(f.title)}
              </Link>
            ),
          }))}
          labels={{ title: "What cost the most" }}
        />
      ) : (
        <Card padding="md">
          <Card.Title>What cost the most</Card.Title>
          <Text as="p" tone="secondary">
            Nothing open was costing the company when this report was written.
          </Text>
        </Card>
      )}
      {others.length > 0 ? (
        <section className="advisor-report__section" aria-labelledby="advisor-week-numbers">
          <Text as="h3" variant="label" id="advisor-week-numbers">
            The week's other numbers
          </Text>
          <DescriptionList
            dense
            items={others.map((r) => [
              r.label,
              <span key={r.key} className="advisor-report__number">
                {r.value}
                {r.change ? (
                  <span className={`advisor-change advisor-change--${r.change.polarity}`}>
                    {r.change.value === "same" ? "same as the week before" : r.change.value}
                  </span>
                ) : null}
              </span>,
            ])}
          />
        </section>
      ) : null}
    </article>
  );
}

/** The weeks either side of this one that have a report, from the list of weeks. */
function Neighbours({ week }: { week: string }) {
  const weeks = useReports().data?.reports ?? [];
  const at = weeks.findIndex((r) => r.week === week);
  if (at < 0) return null;
  const newer = weeks[at - 1];
  const older = weeks[at + 1];
  if (!newer && !older) return null;
  return (
    <nav className="advisor-report__weeks" aria-label="Other weeks">
      {older ? (
        <ButtonLink size="small" variant="ghost" href={href(["advisor", "reports", older.week])}>
          {weekWords(older.week)}
        </ButtonLink>
      ) : null}
      {newer ? (
        <ButtonLink size="small" variant="ghost" href={href(["advisor", "reports", newer.week])}>
          {weekWords(newer.week)}
        </ButtonLink>
      ) : null}
    </nav>
  );
}
