/**
 * One weekly report (`#/advisor/reports/{id}`): the week's numbers beside the
 * week before's, what the week opened, resolved, applied and dismissed, the
 * open findings that cost the most, and the model's summary of those numbers
 * where the Advisor has a model.
 */

import { AttentionList, DescriptionList, Text } from "@crewlethq/ui";
import { QueryState } from "~/components/common.tsx";
import { usePageLabels } from "~/app/Shell.tsx";
import { useNow } from "~/lib/clock.ts";
import { fmtDateTime } from "~/lib/format.ts";
import { marked } from "~/ui/Problems.tsx";
import { useReport } from "./api.ts";
import { freshness } from "./rows.tsx";
import { factValue, toneOf } from "./words.ts";

export function ReportPage({ id }: { id: string }) {
  const read = useReport(id);
  const report = read.data;
  const now = useNow();
  const title = report ? `Week of ${fmtDateTime(report.period.from)}` : "";
  usePageLabels(report ? { [id]: title } : {});
  return (
    <QueryState error={read.code} refusal={read.refusal} loading={read.loading && !report}>
      {report ? (
        <article className="col gap-4" aria-label={title}>
          <Text as="p" tone="secondary">
            {fmtDateTime(report.period.from)} to {fmtDateTime(report.period.to)}, written{" "}
            {fmtDateTime(report.written_at)}.
          </Text>
          {report.summary ? <Text as="p">{marked(report.summary)}</Text> : null}
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
          <section className="col gap-1" aria-labelledby="advisor-week-moved">
            <Text as="h3" variant="label" id="advisor-week-moved">
              What the week moved
            </Text>
            <DescriptionList
              items={[
                ["Findings opened", report.opened.toLocaleString()],
                ["Resolved", report.resolved.toLocaleString()],
                ["Applied", report.applied.toLocaleString()],
                ["Dismissed", report.dismissed.toLocaleString()],
              ]}
            />
          </section>
          {report.top.length > 0 ? (
            <AttentionList
              items={report.top.map((f) => ({
                key: f.id,
                tone: toneOf(f.severity),
                subject: f.subject.name || undefined,
                title: marked(f.title),
                meta: freshness(f, now).join(" · "),
              }))}
              labels={{ title: "What cost the most" }}
            />
          ) : null}
        </article>
      ) : null}
    </QueryState>
  );
}
