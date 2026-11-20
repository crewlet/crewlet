/**
 * The company's numbers at the top of the Advisor's page: a strip of figures
 * and the charts over the range the reader picked (`view.ts` says what each
 * is and where it is read from).
 *
 * A FIGURE NAMES WHAT EXPLAINS IT: where an open finding accounts for a
 * figure, its second line links to that finding ("Why"), so the reader goes
 * from the number to the change that moves it in one press.
 *
 * ONE RENDERER FOR EVERY CHART, by its kind: a series over days is a line
 * (`TimeSeries`), columns across labelled values are the kit's
 * `StackedColumns`, and bars down (long labels, such as what woke a turn) are
 * its `BarList`. Each is named by what it shows, so a reader who cannot see
 * one is told its window and its peak.
 */

import { useMemo, type ReactNode } from "react";
import {
  BarList,
  Card,
  EMPTY_VALUE,
  SegmentedControl,
  StackedColumns,
  StatCard,
  StatGroup,
  Text,
  TimeSeries,
  type BarDatum,
  type StatCardDelta,
} from "@crewlethq/ui";
import { href } from "~/app/router.tsx";
import { fmtCount, plural } from "~/lib/format.ts";
import { dateTimeFormat } from "~/lib/formatters.ts";
import { zone } from "~/lib/prefs.ts";
import { useSeatBadgeOf } from "~/lib/seats.ts";
import { useQuery } from "~/lib/useQuery.ts";
import type { AdvisorKeptFinding } from "~/protocol/advisorFindings.ts";
import { useReport, useReports } from "./api.ts";
import {
  RANGES,
  explainer,
  snapshotOf,
  type RangeDays,
  type StatUnit,
  type ViewChart,
  type ViewSnapshot,
  type ViewStat,
} from "./view.ts";

/** The range picker. */
export function RangePicker({
  days,
  onChange,
}: {
  days: RangeDays;
  onChange: (days: RangeDays) => void;
}) {
  return (
    <SegmentedControl<`${RangeDays}`>
      label="Range"
      semantics="radio"
      size="sm"
      value={`${days}`}
      options={RANGES.map((d) => ({ value: `${d}` as `${RangeDays}`, label: `${d} days` }))}
      onValueChange={(v) => onChange(Number(v) as RangeDays)}
    />
  );
}

/** The page's numbers over `days`, from the questions the company's other screens ask. */
export function useSnapshot(days: RangeDays, findings: readonly AdvisorKeptFinding[]) {
  const flow = useQuery("work_flow", { bucket: "day", points: days });
  const workload = useQuery("work_workload", undefined, { pollMs: 60_000 });
  const outcomes = useQuery("turn_outcomes", { days });
  const previous = useQuery("turn_outcomes", { days, previous: true });
  const reports = useReports();
  const newest = reports.data?.reports[0]?.week ?? null;
  const report = useReport(newest ?? "", newest !== null);
  const badgeOf = useSeatBadgeOf();
  const snapshot = useMemo(
    () =>
      snapshotOf(
        {
          flow: flow.data,
          workload: workload.data,
          outcomes: outcomes.data?.rows ?? null,
          previous: previous.data?.rows ?? null,
          report: report.data,
          findings,
        },
        days,
        (handle) => badgeOf(handle).name,
      ),
    [flow.data, workload.data, outcomes.data, previous.data, report.data, findings, days, badgeOf],
  );
  const loading: Record<string, boolean> = {
    open_items: flow.loading && !flow.data,
    stuck_hours:
      (reports.loading && !reports.data) || (newest !== null && report.loading && !report.data),
    capacity_lost_hours: false,
    wasted_turns_percent: outcomes.loading && !outcomes.data,
    tokens_per_turn: outcomes.loading && !outcomes.data,
  };
  return { snapshot, loading };
}

/** A figure in its unit, as its tile draws it: the number and the word after it. */
export function figure(value: number, unit: StatUnit): { value: string; unit?: string } {
  switch (unit) {
    case "percent":
      return { value: `${value}%` };
    case "hours":
      return { value: fmtCount(value), unit: "h" };
    case "minutes":
      return { value: fmtCount(value), unit: "min" };
    case "tokens":
      return { value: fmtCount(value), unit: "tokens" };
    case "items":
      return { value: fmtCount(value), unit: value === 1 ? "item" : "items" };
    case "turns":
      return { value: fmtCount(value), unit: value === 1 ? "turn" : "turns" };
    case "count":
      return { value: fmtCount(value) };
  }
}

/** The change against the previous range, in the ink of whether it is the one wanted. */
export function deltaOf(stat: ViewStat): StatCardDelta | undefined {
  if (stat.value === null || stat.was === null) return undefined;
  const diff = stat.value - stat.was;
  const polarity: StatCardDelta["polarity"] =
    diff === 0 || stat.better === null
      ? "neutral"
      : diff > 0 === (stat.better === "up")
        ? "good"
        : "bad";
  const sign = diff > 0 ? "+" : diff < 0 ? "−" : "±";
  // A SHARE MOVES IN POINTS, and a count by how much of itself.
  const said =
    stat.unit === "percent"
      ? `${sign}${Math.abs(diff)} pts`
      : stat.was > 0
        ? `${sign}${Math.round((Math.abs(diff) / stat.was) * 100)}%`
        : `${sign}${fmtCount(Math.abs(diff))}`;
  return { value: said, polarity };
}

/** One figure, with its change and the finding that explains it. */
function Tile({
  stat,
  days,
  why,
  loading,
  flush = false,
}: {
  stat: ViewStat;
  days: RangeDays;
  why: AdvisorKeptFinding | null;
  loading: boolean;
  /** Drawn in a card of its own (a chart's head): no surface or edge of its own. */
  flush?: boolean;
}) {
  const shown = stat.value === null ? null : figure(stat.value, stat.unit);
  const delta = deltaOf(stat);
  const words =
    shown === null
      ? "Not measured yet"
      : delta
        ? (stat.window ?? `vs previous ${days} days`)
        : (stat.window ?? "");
  const sub: ReactNode = (
    <>
      {words}
      {why ? (
        <>
          {words ? " · " : ""}
          <a
            className="t-link"
            href={href(["advisor", "findings", why.id])}
            aria-label={`Why: ${why.title}`}
          >
            Why
          </a>
        </>
      ) : null}
    </>
  );
  return (
    <StatCard
      label={stat.label}
      value={shown?.value ?? EMPTY_VALUE}
      {...(shown?.unit ? { unit: shown.unit } : {})}
      {...(delta ? { delta } : {})}
      sub={sub}
      loading={loading}
      flush={flush}
      data-stat={stat.key}
    />
  );
}

/**
 * The ids of the charts that carry a figure of their own: a stat whose key is
 * a chart's id (`tokens_per_turn`) is drawn on that chart, its breakdown
 * beside it, and not in the strip.
 */
const charted = (snapshot: ViewSnapshot) => new Set(snapshot.charts.map((c) => c.id));

/**
 * The strip of figures, one row. FOUR A ROW at most, the kit's widest row,
 * which folds two by two on a phone and leaves no tile alone on a row.
 */
export function StatsStrip({
  snapshot,
  findings,
  loading,
}: {
  snapshot: ViewSnapshot;
  findings: readonly AdvisorKeptFinding[];
  loading: Record<string, boolean>;
}) {
  const onCharts = charted(snapshot);
  const shown = snapshot.stats.filter((s) => !onCharts.has(s.key));
  const n = shown.length;
  return (
    <StatGroup
      columns={n % 3 === 0 && n % 4 !== 0 ? 3 : n === 2 ? 2 : 4}
      aria-label="The company's numbers"
    >
      {shown.map((s) => (
        <Tile
          key={s.key}
          stat={s}
          days={snapshot.days}
          why={explainer(s.key, findings)}
          loading={!!loading[s.key]}
        />
      ))}
    </StatGroup>
  );
}

/** A day, in the zone every instant here is drawn in, as a chart's axis says it. */
const dayWords = (at: number) =>
  dateTimeFormat(undefined, zone(), { month: "short", day: "numeric" }).format(at);

/** A chart's value in its unit, in a few characters. */
const valueWords = (unit: StatUnit) => (v: number) =>
  unit === "percent" ? `${v}%` : unit === "hours" ? `${fmtCount(v)} h` : fmtCount(v);

/** One chart, by its kind, headed by its figure where the snapshot has one by its id. */
export function ChartCard({
  chart,
  stat,
  days,
  why,
  from,
  to,
}: {
  chart: ViewChart;
  stat: ViewStat | null;
  days: RangeDays;
  why: AdvisorKeptFinding | null;
  from: number;
  to: number;
}) {
  return (
    <Card padding="md" className="advisor-chart" data-chart={chart.id}>
      {stat ? (
        <Tile stat={{ ...stat, label: chart.title }} days={days} why={why} loading={false} flush />
      ) : (
        <Card.Title>{chart.title}</Card.Title>
      )}
      <ChartBody chart={chart} from={from} to={to} />
      {chart.note ? (
        <Text as="p" variant="caption" tone="secondary">
          {chart.note}
        </Text>
      ) : null}
    </Card>
  );
}

function ChartBody({ chart, from, to }: { chart: ViewChart; from: number; to: number }) {
  const format = valueWords(chart.unit);
  if (chart.kind === "columns" && chart.x.kind === "day" && chart.at) {
    const at = chart.at;
    return (
      <TimeSeries
        label={chart.title}
        from={from}
        to={to}
        height="7rem"
        format={format}
        formatTime={dayWords}
        emptyLabel="Nothing recorded in the range"
        series={chart.series.map((s) => ({
          id: s.id,
          name: s.label,
          points: s.values.flatMap((v, i) => (v === null ? [] : [{ t: at[i]!, v }])),
        }))}
      />
    );
  }
  if (chart.kind === "bars") {
    const several = chart.series.length > 1;
    const data: BarDatum[] = chart.x.labels.map((label, i) => {
      const parts = chart.series.map((s) => ({
        id: s.id,
        label: s.label,
        value: s.values[i] ?? 0,
      }));
      const value = several ? parts.reduce((n, p) => n + p.value, 0) : (parts[0]?.value ?? 0);
      return {
        id: chart.x.keys[i] ?? `${chart.id}:${i}`,
        label,
        value,
        display: several ? `${fmtCount(parts[0]!.value)} of ${format(value)}` : format(value),
        ...(several ? { parts } : {}),
      };
    });
    return <BarList data={data} limit={8} emptyLabel="Nothing to show in the range" />;
  }
  return (
    <StackedColumns
      label={chart.title}
      height="7rem"
      legend="below"
      format={format}
      formatScale={format}
      formatTime={(i) => chart.x.labels[i] ?? ""}
      emptyLabel="Nothing to show in the range"
      series={chart.series.map((s) => ({ id: s.id, name: s.label }))}
      buckets={chart.x.labels.map((_, i) => ({
        t: i,
        values: Object.fromEntries(chart.series.map((s) => [s.id, s.values[i] ?? 0])),
      }))}
    />
  );
}

/** Every chart of the range, in a grid. */
export function Charts({
  snapshot,
  findings,
  now,
}: {
  snapshot: ViewSnapshot;
  findings: readonly AdvisorKeptFinding[];
  now: number;
}) {
  const from = now - snapshot.days * 86_400_000;
  return (
    <div
      className="advisor-charts"
      aria-label={`The last ${plural(snapshot.days, "day")} in charts`}
    >
      {snapshot.charts.map((c) => {
        const stat = snapshot.stats.find((s) => s.key === c.id) ?? null;
        return (
          <ChartCard
            key={c.id}
            chart={c}
            stat={stat}
            days={snapshot.days}
            why={stat ? explainer(stat.key, findings) : null}
            from={from}
            to={now}
          />
        );
      })}
    </div>
  );
}
