/**
 * One weekly report (`#/advisor/reports/{week}`): the week's numbers beside
 * the week before's, what the week opened, resolved, applied and dismissed,
 * the open findings that cost the most, each opening its page, and the
 * model's summary of those numbers where the Advisor has a model. A source
 * the week could not read is said, since its numbers are left out.
 *
 * The report is kept as it was written: a finding it names reads as it did
 * that Monday, and its page says what became of it since.
 */

import { AttentionList, Callout, DescriptionList, Link, Text } from "@crewlethq/ui";
import { QueryState } from "~/components/common.tsx";
import { usePageLabels } from "~/app/Shell.tsx";
import { href } from "~/app/router.tsx";
import { fmtCount, fmtDateTime } from "~/lib/format.ts";
import { unreadNames } from "~/lib/advisorUnread.ts";
import { marked } from "~/ui/Problems.tsx";
import { useReport } from "./api.ts";
import { factValue, reportSubject, toneOf, weekSpan, weekWords } from "./words.ts";

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
      {report ? (
        <article className="col gap-4" aria-label={title}>
          <Text as="p" tone="secondary">
            {weekSpan(report.week) ??
              `${fmtDateTime(report.period.from)} to ${fmtDateTime(report.period.to)}`}
            , written {fmtDateTime(report.written_at)}.
          </Text>
          {report.unread.length > 0 ? (
            <Callout variant="warning" title="Some numbers are left out">
              The Advisor couldn't read {unreadNames(report.unread)} for this week, so the numbers
              drawn from them are not in this report.
            </Callout>
          ) : null}
          {report.summary ? <Text as="p">{marked(report.summary)}</Text> : null}
          {report.metrics.length > 0 ? (
            <table className="advisor-report__metrics">
              <caption className="t-label">The week against the week before</caption>
              <thead>
                <tr>
                  <th scope="col">Measure</th>
                  <th scope="col">This week</th>
                  <th scope="col">The week before</th>
                </tr>
              </thead>
              <tbody>
                {report.metrics.map((m) => (
                  <tr key={m.key}>
                    <th scope="row">{m.label}</th>
                    <td>{factValue(m.value, m.unit)}</td>
                    <td>{m.was === null ? "Not measured" : factValue(m.was, m.unit)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          ) : null}
          <section className="col gap-1" aria-labelledby="advisor-week-moved">
            <Text as="h3" variant="label" id="advisor-week-moved">
              What the week moved
            </Text>
            <DescriptionList
              items={[
                ["Findings opened", fmtCount(report.opened)],
                ["Resolved", fmtCount(report.resolved)],
                ["Applied", fmtCount(report.applied)],
                ["Dismissed", fmtCount(report.dismissed)],
              ]}
            />
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
          ) : null}
        </article>
      ) : null}
    </QueryState>
  );
}
