/**
 * "Tasks completed" — deliveries per company day over the last fortnight,
 * from the engine's own series (`work_flow`).
 *
 * TODAY IS THE ACCENT: the kit's accent means where the reader is, and on a
 * trend that is the present — the Sparkline's "now" point, drawn as a column.
 * Every earlier day is the residual neutral, because the columns are one
 * quantity and a hue per day would be colour spent on nothing.
 *
 * ONE SERIES, AND THE ACCENT IS A COLUMN'S PAINT (`.home-chart` in
 * screens.css, on the last column — the series always ends today). Drawn as
 * two series of which each day filled one, the split leaked into the
 * tooltip: every column read "Completed 0 · Completed today 3 · Total 3",
 * a series nobody measured beside the one they did.
 */

import { Card, DATA_COLOR_OTHER, StackedColumns } from "@crewlethq/ui";
import { ChartColumnGlyph } from "@crewlethq/icons/glyphs";
import { companyDateLabel } from "~/lib/format.ts";
import { WORKLET, needsWords } from "~/lib/access.ts";
import type { QueryResult } from "~/lib/useQuery.ts";
import type { WorkFlowAnswer } from "~/protocol/index.ts";
import { WidgetQuery, gapsOf } from "./board/kit.tsx";

/** A fortnight: two weeks side by side, enough to see a week-on-week shape. */
export const CHART_DAYS = 14;

const SERIES = [{ id: "completed", name: "Completed", color: DATA_COLOR_OTHER }] as const;

export function CompletedChart({
  flow,
  locked,
}: {
  flow: QueryResult<WorkFlowAnswer>;
  /** The reader holds no Worklet, so the series was never asked. */
  locked: boolean;
}) {
  const points = flow.data?.points?.slice(-CHART_DAYS) ?? [];
  const week = points.slice(-7).reduce((n, p) => n + p.completed, 0);
  const last = points.length - 1;
  const buckets = points.map((p) => ({
    t: Date.parse(p.start),
    values: { completed: p.completed },
  }));
  // THE LABEL IS THE WINDOW'S OWN (`2026-09-22`), the company's date, drawn
  // as a date rather than re-derived from the instant on this browser's clock.
  const labels = new Map(
    points.map((p, i) => [Date.parse(p.start), i === last ? "Today" : companyDateLabel(p.window)]),
  );
  return (
    // THE KIT'S HEAD, as every other card on Home wears: a head of its own drew
    // this title at 16px beside the 14px every sibling's title takes.
    <Card padding="none" className="home-card">
      <Card.Header
        icon={<ChartColumnGlyph size="sm" />}
        subtitle={`Per day · last ${CHART_DAYS} days`}
        actions={
          points.length > 0 ? (
            <span className="home-chart-figure">
              {week.toLocaleString()}
              <span className="home-chart-figure-unit"> in 7 days</span>
            </span>
          ) : undefined
        }
      >
        <Card.Title as="h3">Tasks completed</Card.Title>
      </Card.Header>
      <Card.Body className="home-chart">
        {locked ? (
          <p className="home-card-note">{needsWords("Worklet's history", [WORKLET])}</p>
        ) : (
          <WidgetQuery
            error={flow.error}
            refusal={flow.refusal}
            loading={flow.loading && !flow.data}
            gaps={gapsOf([flow.data])}
          >
            <StackedColumns
              series={SERIES}
              buckets={buckets}
              legend="none"
              height="9.5rem"
              label="Tasks completed per company day, the last fourteen days"
              format={(v) => Math.round(v).toLocaleString()}
              formatTime={(at) => labels.get(at) ?? ""}
            />
          </WidgetQuery>
        )}
      </Card.Body>
    </Card>
  );
}
