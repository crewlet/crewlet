/**
 * A week's report as its page and the list of weeks draw it, read off the
 * report the engine kept (`advisor_report`, `advisor_reports`).
 *
 * THE HEADLINE IS FOUR FIGURES: the week's turns, its tokens, the share of
 * turns that changed nothing (with their count beside it) and the hours work
 * waited for a seat, each beside the week before. A figure the week did not
 * measure is said so, never drawn as a zero.
 *
 * A FIRST WEEK IS SAID ONCE: a report with nothing the week before was
 * measured for reads as the first, and its figures carry no change, rather
 * than a column of "not measured" beside every number.
 *
 * THE WEEK ACROSS ITS DAYS where the report kept them: turns split by
 * whether they committed anything, and tokens, each with a line naming the
 * busiest day. A report kept before its days were draws no chart.
 */

import type { StatCardDelta } from "@crewlethq/ui";
import { fmtCount, plural } from "~/lib/format.ts";
import { dateTimeFormat } from "~/lib/formatters.ts";
import type {
  AdvisorChart,
  AdvisorMetric,
  AdvisorReport,
  AdvisorReportSummary,
} from "~/protocol/advisorFindings.ts";
import { deltaOf } from "./Numbers.tsx";
import { amount, type StatUnit, type ViewStat } from "./view.ts";
import { factValue } from "./words.ts";

/** A number in a few characters: in full under ten thousand, else three figures and k, M or B. */
export function compact(n: number): string {
  if (Math.abs(n) < 10_000) return fmtCount(amount(n));
  const steps = [
    [1e3, "k"],
    [1e6, "M"],
    [1e9, "B"],
  ] as const;
  let at = steps.findLastIndex(([size]) => Math.abs(n) >= size);
  let shown = Number((n / steps[at]![0]).toPrecision(3));
  // 999.6k IS 1M: a value that rounds up to a thousand of its step takes the next.
  if (Math.abs(shown) >= 1000 && at < steps.length - 1) {
    at += 1;
    shown = Number((n / steps[at]![0]).toPrecision(3));
  }
  return `${shown}${steps[at]![1]}`;
}

/** A figure of a report in its unit, as its tile draws it: the number and the word after it. */
export function reportFigure(value: number, unit: StatUnit): { value: string; unit?: string } {
  switch (unit) {
    case "percent":
      return { value: `${amount(value)}%` };
    case "hours":
      return { value: fmtCount(amount(value)), unit: "h" };
    case "minutes":
      return { value: fmtCount(amount(value)), unit: "min" };
    case "tokens":
      return { value: compact(value), unit: "tokens" };
    case "turns":
      return { value: compact(value), unit: value === 1 ? "turn" : "turns" };
    case "items":
      return { value: compact(value), unit: value === 1 ? "item" : "items" };
    default:
      return { value: compact(value) };
  }
}

const find = (metrics: readonly AdvisorMetric[], key: string) =>
  metrics.find((m) => m.key === key) ?? null;

/** A share of turns in whole points, where the turns are known and some ran. */
const share = (part: number | null | undefined, whole: number | null | undefined) =>
  part == null || whole == null || whole <= 0 ? null : Math.round((part / whole) * 100);

/** A metric as a figure, or the figure not measured. */
function figureOf(
  key: string,
  label: string,
  unit: StatUnit,
  metric: AdvisorMetric | null,
  better: ViewStat["better"],
): ViewStat {
  return {
    key,
    label,
    value: metric?.value ?? null,
    unit: metric?.unit ?? unit,
    was: metric?.was ?? null,
    better,
  };
}

/** The keys the headline figures stand for: listed once, never again under the rest. */
function headlineKeys(metrics: readonly AdvisorMetric[]): string[] {
  const wait = find(metrics, "wait_h") ? "wait_h" : find(metrics, "pickup_p50") ? "pickup_p50" : "";
  return ["turns", "turns_nothing", "tokens", wait].filter(Boolean);
}

/** A week's four headline figures, from its metrics (a report's, or a list's headline). */
export function weekFigures(metrics: readonly AdvisorMetric[]): ViewStat[] {
  const turns = find(metrics, "turns");
  const nothing = find(metrics, "turns_nothing");
  const wait = find(metrics, "wait_h");
  const pickup = wait ? null : find(metrics, "pickup_p50");
  return [
    figureOf("turns", "Turns", "turns", turns, null),
    figureOf("tokens", "Tokens", "tokens", find(metrics, "tokens"), "down"),
    {
      key: "changed_nothing",
      label: "Changed nothing",
      value: share(nothing?.value, turns?.value),
      unit: "percent",
      was: share(nothing?.was, turns?.was),
      better: "down",
      ...(nothing && turns
        ? { detail: `${fmtCount(nothing.value)} of ${plural(turns.value, "turn")}` }
        : {}),
    },
    pickup
      ? figureOf("pickup_p50", "Pickup wait, median", "minutes", pickup, "down")
      : figureOf("wait_h", "Hours work waited", "hours", wait, "down"),
  ];
}

/** Whether a week is the first measured: it has numbers, and none of the week before's. */
export function firstWeek(metrics: readonly AdvisorMetric[]): boolean {
  return metrics.length > 0 && metrics.every((m) => m.was === null);
}

/** One of the rest of a week's numbers, with its change against the week before. */
export interface OtherRow {
  key: string;
  label: string;
  value: string;
  change: StatCardDelta | null;
}

/** Every number of a week the headline does not show, in the report's order. */
export function otherRows(metrics: readonly AdvisorMetric[]): OtherRow[] {
  const shown = new Set(headlineKeys(metrics));
  return metrics
    .filter((m) => !shown.has(m.key))
    .map((m) => ({
      key: m.key,
      label: m.label,
      value: factValue(m.value, m.unit),
      change:
        m.was === null
          ? null
          : m.was === m.value
            ? { value: "same", polarity: "neutral" }
            : (deltaOf({ ...m, better: "down" }) ?? null),
    }));
}

/** A company day as a chart's axis names it: "Mon 9". */
function dayLabel(day: string): string {
  const at = Date.parse(`${day}T12:00:00Z`);
  if (!Number.isFinite(at)) return day;
  const weekday = dateTimeFormat(undefined, "UTC", { weekday: "short" }).format(at);
  return `${weekday} ${new Date(at).getUTCDate()}`;
}

/** The week across its days: turns by whether they committed anything, and tokens. */
export function dayCharts(report: Pick<AdvisorReport, "days">): AdvisorChart[] {
  const days = report.days;
  if (days.length === 0) return [];
  const x = { kind: "day", keys: days.map((d) => d.day), labels: days.map((d) => dayLabel(d.day)) };
  const busiest = days.reduce((best, d) => (d.turns > best.turns ? d : best), days[0]!);
  const costliest = days.reduce((best, d) => (d.tokens > best.tokens ? d : best), days[0]!);
  return [
    {
      id: "turns_by_day",
      title: "Turns each day",
      unit: "turns",
      kind: "stacked_columns",
      x,
      series: [
        {
          id: "moved",
          label: "Committed something",
          values: days.map((d) => d.turns - d.turns_nothing),
        },
        { id: "nothing", label: "Changed nothing", values: days.map((d) => d.turns_nothing) },
      ],
      note:
        busiest.turns > 0
          ? `The busiest day was ${dayLabel(busiest.day)}: ${plural(busiest.turns, "turn")}, ${fmtCount(busiest.turns_nothing)} of them changed nothing.`
          : "No turn ran this week.",
    },
    {
      id: "tokens_by_day",
      title: "Tokens each day",
      unit: "tokens",
      kind: "stacked_columns",
      x,
      series: [{ id: "tokens", label: "Tokens", values: days.map((d) => d.tokens) }],
      note:
        costliest.tokens > 0
          ? `The most was spent on ${dayLabel(costliest.day)}: ${fmtCount(costliest.tokens)} tokens.`
          : null,
    },
  ];
}

const DAY_MS = 86_400_000;

/** The Monday an ISO week starts on, as a UTC instant, or null for a week not in ISO form. */
function mondayOf(week: string): number | null {
  const match = /^(\d{4})-W(\d{2})$/.exec(week);
  if (!match) return null;
  // WEEK 1 is the one holding January 4th, and every ISO week starts on a Monday.
  const jan4 = Date.UTC(Number(match[1]), 0, 4);
  const fromMonday = (new Date(jan4).getUTCDay() + 6) % 7;
  return jan4 - fromMonday * DAY_MS + (Number(match[2]) - 1) * 7 * DAY_MS;
}

/** A week as its days in short: "Nov 9 to 15, 2026", the month and year said only where they change. */
export function weekShort(week: string): string {
  const monday = mondayOf(week);
  if (monday === null) return week;
  const sunday = monday + 6 * DAY_MS;
  const [a, b] = [new Date(monday), new Date(sunday)];
  const month = (at: number) => dateTimeFormat(undefined, "UTC", { month: "short" }).format(at);
  if (a.getUTCFullYear() !== b.getUTCFullYear()) {
    return `${month(monday)} ${a.getUTCDate()}, ${a.getUTCFullYear()} to ${month(sunday)} ${b.getUTCDate()}, ${b.getUTCFullYear()}`;
  }
  const end =
    a.getUTCMonth() === b.getUTCMonth()
      ? `${b.getUTCDate()}`
      : `${month(sunday)} ${b.getUTCDate()}`;
  return `${month(monday)} ${a.getUTCDate()} to ${end}, ${b.getUTCFullYear()}`;
}

/** One week as the list of weeks draws it. */
export interface WeekCard {
  week: string;
  /** "Week 46". */
  title: string;
  /** "Nov 9 to 15, 2026". */
  days: string;
  writtenAt: string;
  /** Nothing the week before was measured for. */
  first: boolean;
  /** Its turns, tokens, the share that changed nothing, and the findings it opened. */
  figures: ViewStat[];
}

/**
 * The weeks as the list draws them, the newest first. The findings a week
 * opened stand beside the week before's where the list holds that week (the
 * next one, ending as this one starts); its numbers carry the week before's
 * from the report itself.
 */
export function weekCards(rows: readonly AdvisorReportSummary[]): WeekCard[] {
  return rows.map((r, i) => {
    const before = rows[i + 1];
    const adjacent = before !== undefined && before.period.to === r.period.from;
    const [turns, tokens, nothing] = weekFigures(r.headline);
    const number = /-W(\d{2})$/.exec(r.week);
    return {
      week: r.week,
      title: number ? `Week ${Number(number[1])}` : r.week,
      days: weekShort(r.week),
      writtenAt: r.written_at,
      first: firstWeek(r.headline),
      figures: [
        turns!,
        tokens!,
        nothing!,
        {
          key: "opened",
          label: "Findings opened",
          value: r.opened,
          unit: "count",
          was: adjacent ? before.opened : null,
          better: "down",
          detail: `${fmtCount(r.resolved)} resolved · ${fmtCount(r.applied)} applied · ${fmtCount(r.dismissed)} dismissed`,
        },
      ],
    };
  });
}
