/**
 * The company's numbers at the top of the Advisor's page: a strip of figures
 * and the charts over the range the reader picked (`view.ts` says what each
 * is and where it is read from).
 *
 * A FIGURE NAMES WHAT EXPLAINS IT: where an open finding accounts for a
 * figure, its second line links to that finding ("Why"), so the reader goes
 * from the number to the change that moves it in one press.
 *
 * ONE RENDERER FOR EVERY CHART, by its shape (`chartShape`): NAMES GO DOWN A
 * LIST, DAYS GO ACROSS. A chart over seats, entries or reasons is the kit's
 * `BarList`, each row its full name and split by its series; one series over
 * days is a line (`TimeSeries`) and several are `StackedColumns`, whose first,
 * middle and last days are short enough to stand under the plot. Each is
 * named by what it shows, so a reader who cannot see one is told its window
 * and its peak.
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
import { useQuery } from "~/lib/useQuery.ts";
import type { AdvisorChart, AdvisorKeptFinding } from "~/protocol/advisorFindings.ts";
import {
  RANGES,
  amount,
  amountWords,
  chartColumns,
  chartShape,
  explainer,
  snapshotView,
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

/** The page's numbers over `days`, as the engine states them (`advisor_snapshot`). */
export function useSnapshot(days: RangeDays) {
  const answer = useQuery("advisor_snapshot", { range: `${days}d` }, { pollMs: SNAPSHOT_POLL_MS });
  const snapshot = useMemo(() => snapshotView(answer.data, days), [answer.data, days]);
  return { snapshot, loading: answer.loading && !answer.data, answer };
}

/**
 * How often the numbers are read: the engine computes a snapshot on read and
 * keeps it a few minutes, so a read a minute shows the next one within its life.
 */
export const SNAPSHOT_POLL_MS = 60_000;

/** A figure in its unit, as its tile draws it: the number and the word after it. */
export function figure(value: number, unit: StatUnit): { value: string; unit?: string } {
  const said = fmtCount(amount(value));
  switch (unit) {
    case "percent":
      return { value: `${said}%` };
    case "hours":
      return { value: said, unit: "h" };
    case "minutes":
      return { value: said, unit: "min" };
    case "tokens":
      return { value: said, unit: "tokens" };
    case "items":
      return { value: said, unit: value === 1 ? "item" : "items" };
    case "turns":
      return { value: said, unit: value === 1 ? "turn" : "turns" };
    default:
      return { value: said };
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
  // A SHARE MOVES IN POINTS, a count by how much of itself, and a change
  // from nothing by itself in its unit.
  const said =
    stat.unit === "percent"
      ? `${sign}${fmtCount(amount(Math.abs(diff)))} pts`
      : stat.was > 0
        ? `${sign}${Math.round((Math.abs(diff) / stat.was) * 100)}%`
        : `${sign}${amountWords(Math.abs(diff), stat.unit)}`;
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
  const compared =
    shown === null
      ? "Not measured yet"
      : (stat.detail ?? (delta ? `vs previous ${days} days` : ""));
  const sub: ReactNode = (
    <>
      {compared}
      {why ? (
        <>
          {compared ? " · " : ""}
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
 * The strip of figures, one row of four, the kit's widest, which folds two by
 * two on a phone and leaves no tile alone on a row (`view.ts` chooses them).
 */
export function StatsStrip({
  snapshot,
  findings,
  loading,
}: {
  snapshot: ViewSnapshot;
  findings: readonly AdvisorKeptFinding[];
  loading: boolean;
}) {
  return (
    <StatGroup columns={4} aria-label="The company's numbers">
      {snapshot.stats.map((s) => (
        <Tile
          key={s.key}
          stat={s}
          days={snapshot.days}
          why={explainer(s.key, findings)}
          loading={loading}
        />
      ))}
    </StatGroup>
  );
}

/** A day, in the zone every instant here is drawn in, as a chart's axis says it. */
const dayWords = (at: number) =>
  dateTimeFormat(undefined, zone(), { month: "short", day: "numeric" }).format(at);

/** A chart's value in its unit, in a few characters. */
const valueWords = (unit: StatUnit) => (v: number) => amountWords(v, unit);

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
  const body = (
    <>
      <ChartBody chart={chart} from={from} to={to} />
      {chart.note ? (
        <Text as="p" variant="caption" tone="secondary">
          {chart.note}
        </Text>
      ) : null}
    </>
  );
  // A FIGURE HEADS ITS CHART AT THE CARD'S OWN EDGE: the tile brings its own
  // inset, so the card adds none, and the chart under it is inset as much,
  // which lines the figure up with every other card's title.
  return stat ? (
    <Card padding="none" className="advisor-chart" data-chart={chart.id}>
      <Tile stat={{ ...stat, label: chart.title }} days={days} why={why} loading={false} flush />
      <Card.Body padding="md" className="advisor-chart__body">
        {body}
      </Card.Body>
    </Card>
  ) : (
    <Card padding="md" className="advisor-chart" data-chart={chart.id}>
      <Card.Title truncate>{chart.title}</Card.Title>
      {body}
    </Card>
  );
}

/**
 * A chart over names as the kit's bar list draws it: a row per name, RANKED,
 * the most first, split by its series where there are several, and every
 * number, a part's as the row's, in the chart's unit.
 */
export function barRows(chart: AdvisorChart): BarDatum[] {
  const format = valueWords(chart.unit);
  const several = chart.series.length > 1;
  // A SHARE OF THE WHOLE where the engine drew bars of one kind ("47 of 47"
  // changed nothing); the row's whole where a chart over names is split.
  const share = chart.kind === "bars" && several;
  return chart.x.labels
    .map((label, i): BarDatum => {
      const parts = chart.series.map((s) => {
        const value = s.values[i] ?? 0;
        return { id: s.id, label: s.label, value, display: format(value) };
      });
      const value = several ? parts.reduce((n, p) => n + p.value, 0) : (parts[0]?.value ?? 0);
      return {
        id: chart.x.keys[i] ?? `${chart.id}:${i}`,
        label,
        value,
        display: share ? `${fmtCount(parts[0]!.value)} of ${format(value)}` : format(value),
        ...(several ? { parts } : {}),
      };
    })
    .sort((a, b) => b.value - a.value);
}

function ChartBody({ chart, from, to }: { chart: ViewChart; from: number; to: number }) {
  const format = valueWords(chart.unit);
  const shape = chartShape(chart);
  if (shape === "line" && chart.at) {
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
  if (shape === "bars") {
    return <BarList data={barRows(chart)} limit={5} emptyLabel="Nothing to show in the range" />;
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
      data-columns={chartColumns(snapshot.charts.length)}
      aria-label={`The last ${plural(snapshot.days, "day")} in charts`}
    >
      {snapshot.charts.map((c) => {
        const stat = snapshot.charted.find((s) => s.key === c.id) ?? null;
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
