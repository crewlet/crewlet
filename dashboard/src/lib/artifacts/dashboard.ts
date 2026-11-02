/**
 * A dashboard board, as screens read it: the folds a stat draws, how a number
 * is written, which datasets stand on which, how fresh an answer is, and the
 * words the Sources rail describes each dataset with.
 *
 * THE FOLD IS THE ENGINE'S (`dashspec.Fold`), ported rather than re-imagined:
 * a stat over a dataset and the calculated dataset a board computes from the
 * same rows must give one number, so the two follow one rule, held by a suite
 * over the same rows the engine's own suite folds.
 */

import { DASHBOARD_MIN_REFRESH_SECONDS } from "~/contract/artifacts.ts";
import type {
  DashboardAgg,
  DashboardArg,
  DashboardBoardDataset,
  DashboardColumnType,
  DashboardDataset,
  DashboardDatasetAnswer,
  DashboardReading,
  DashboardFreshness,
  DashboardNumberFormat,
  DashboardParam,
  DashboardRow,
  DashboardStep,
  DashboardValue,
  DashboardWidget,
} from "~/protocol/index.ts";
import { DATA_COLORS, EMPTY_VALUE, type DatasetStatus } from "@crewlethq/ui";
import { fmtCount, fmtExact, plural } from "~/lib/format.ts";
import { dateTimeFormat, numberFormat, type DateShape } from "~/lib/formatters.ts";
import { browserZone, zone } from "~/lib/prefs.ts";
import { axisLabel } from "~/lib/work.ts";
import { stateWord, type SeatState } from "~/lib/seats.ts";
import type { DashboardColumn } from "~/protocol/index.ts";

/** A day (`YYYY-MM-DD`) or an instant as milliseconds, NaN for anything else. */
export function instantOf(v: DashboardValue): number {
  if (typeof v !== "string") return Number.NaN;
  return Date.parse(/^\d{4}-\d{2}-\d{2}$/.test(v) ? `${v}T00:00:00Z` : v);
}

/** A calendar day, `YYYY-MM-DD`, with no time and no zone. */
const DAY = /^(\d{4})-(\d{2})-(\d{2})$/;

/**
 * Where a value falls on a chart's time axis, in milliseconds: a calendar day
 * at its MIDNIGHT IN THE READER'S ZONE, an instant as itself, NaN for anything
 * else.
 *
 * NOT [instantOf], which orders days as the engine does (midnight UTC): drawn
 * at that instant, a day east of Greenwich is placed, and labelled, on the
 * evening before it. A day has no time to convert; it is the day it names
 * wherever the reader is.
 */
export function chartTimeOf(v: DashboardValue): number {
  if (typeof v !== "string") return Number.NaN;
  const day = DAY.exec(v);
  if (day) return new Date(Number(day[1]), Number(day[2]) - 1, Number(day[3])).getTime();
  return Date.parse(v);
}

/** Whether every value a chart's time axis is drawn from is a calendar day. */
export function allDays(values: readonly DashboardValue[]): boolean {
  return values.length > 0 && values.every((v) => typeof v === "string" && DAY.test(v));
}

/**
 * A day on a chart's axis or in its tooltip: the date, never a time of day,
 * and the year only when it is not this one. Three dates under a plot that
 * each said "2026" ran into each other on a widget a third of a board wide,
 * and the year a reader is in goes without saying.
 */
export function formatDay(at: number, now = Date.now()): string {
  const day = new Date(at);
  // THE BROWSER'S ZONE, the one [chartTimeOf] builds a day's midnight in and
  // `getFullYear` reads the year in, so the label is the day the value named.
  return dateTimeFormat(
    undefined,
    browserZone(),
    day.getFullYear() === new Date(now).getFullYear() ? DAY_MONTH : DAY_MONTH_YEAR,
  ).format(day);
}

const DAY_MONTH: DateShape = Object.freeze({ day: "numeric", month: "short" });
const DAY_MONTH_YEAR: DateShape = Object.freeze({ ...DAY_MONTH, year: "numeric" });

/**
 * A value in its column's own words: a tracker status, status group or
 * priority as Worklet names it, an agent's activity as the Agents page does, and
 * any other value as its text. The id is what a filter compared; this is what
 * a reader is shown.
 */
export function wordsOf(column: DashboardColumn | undefined, v: DashboardValue): string {
  if (v === null) return "";
  const text = String(v);
  switch (column?.words) {
    case "tracker_status":
      return axisLabel("status", text);
    case "tracker_status_group":
      return axisLabel("status_group", text);
    case "tracker_priority":
      return axisLabel("priority", text);
    case "agent_activity":
      return stateWord(text as SeatState);
    case undefined:
      return text;
  }
}

/**
 * A table cell's words, the one rule a widget's table, its column widths and
 * the Sources rail's preview all read: none (and no priority) as nothing,
 * which a table draws as the empty mark, a boolean as Yes or
 * No, a number grouped to two decimals, a date as a reader writes one (a day
 * as `Sep 1`, an instant with its time), and any other value in its column's
 * own words. A date drawn as the engine stores it (`2026-09-01`,
 * `2026-09-01T10:00:00Z`) is a machine's spelling of a fact a reader reads.
 */
export function cellText(column: DashboardColumn | undefined, v: DashboardValue): string {
  if (v === null || v === "") return "";
  // A priority of none is no priority: drawn as the absence it is, as an
  // empty due date is, never as the word "None".
  if (column?.words === "tracker_priority" && v === "none") return "";
  if (typeof v === "boolean") return v ? "Yes" : "No";
  if (column?.type === "number" && typeof v === "number") return formatNumber(v, "number");
  if (column?.type === "date" && typeof v === "string") {
    const at = chartTimeOf(v);
    if (!Number.isNaN(at)) {
      return DAY.test(v) ? formatDay(at) : dateTimeFormat(undefined, zone(), INSTANT).format(at);
    }
  }
  return wordsOf(column, v);
}

const INSTANT: DateShape = Object.freeze({ dateStyle: "medium", timeStyle: "short" });

/**
 * Orders two values of one column type: none before every value, numbers
 * numerically, dates chronologically, false before true, text by code unit.
 * The engine's `compare`.
 */
export function compareValues(
  t: DashboardColumnType,
  a: DashboardValue,
  b: DashboardValue,
): number {
  if (a === null && b === null) return 0;
  if (a === null) return -1;
  if (b === null) return 1;
  switch (t) {
    case "number":
      return Math.sign(Number(a) - Number(b));
    case "date": {
      const x = instantOf(a);
      const y = instantOf(b);
      if (!Number.isNaN(x) && !Number.isNaN(y)) return Math.sign(x - y);
      break;
    }
    case "boolean":
      return a === b ? 0 : a ? 1 : -1;
  }
  const x = String(a);
  const y = String(b);
  return x < y ? -1 : x > y ? 1 : 0;
}

/**
 * `agg` over `column` of `rows`, a column of type `t`: a count is a number of
 * rows (every row when no column is named), a sum of no rows is zero, and
 * every other fold over no rows, or over rows none of which holds a value, is
 * none. The engine's `Fold`.
 */
export function fold(
  agg: DashboardAgg,
  t: DashboardColumnType,
  rows: readonly DashboardRow[],
  column: string,
): DashboardValue {
  if (agg === "count") {
    return rows.filter((r) => column === "" || (r[column] ?? null) !== null).length;
  }
  if (agg === "sum" && rows.length === 0) return 0;
  const present = rows
    .map((r) => r[column] ?? null)
    .filter((v): v is Exclude<DashboardValue, null> => v !== null);
  if (present.length === 0) return null;
  switch (agg) {
    case "first":
      return present[0]!;
    case "last":
      return present[present.length - 1]!;
    case "sum":
    case "avg": {
      let sum = 0;
      for (const v of present) sum += typeof v === "number" ? v : 0;
      return agg === "avg" ? sum / present.length : sum;
    }
    case "min":
    case "max": {
      let best = present[0]!;
      for (const v of present.slice(1)) {
        const c = compareValues(t, v, best);
        if ((agg === "min" && c < 0) || (agg === "max" && c > 0)) best = v;
      }
      return best;
    }
  }
}

/** The fold a stat or a meter takes: its own, else the last row's value (`Widget.EffectiveAgg`). */
export function effectiveAgg(w: Pick<DashboardWidget, "agg">): DashboardAgg {
  return w.agg ?? "last";
}

/**
 * A number as a widget writes it: `number` grouped to two decimals, `integer`
 * grouped and rounded, `percent` a ratio written out of a hundred, `compact`
 * abbreviated past ten thousand. A unit follows with a space.
 */
export function formatNumber(
  v: DashboardValue,
  format: DashboardNumberFormat | undefined,
  unit?: string,
): string {
  if (typeof v !== "number" || !Number.isFinite(v)) return v === null ? EMPTY_VALUE : String(v);
  let text: string;
  switch (format) {
    case "integer":
      text = fmtExact(Math.round(v));
      break;
    case "percent":
      text = `${numberFormat(undefined, ONE_PLACE).format(v * 100)}%`;
      break;
    case "compact":
      text = fmtCount(v);
      break;
    default:
      text = numberFormat(undefined, TWO_PLACES).format(v);
  }
  return unit ? `${text} ${unit}` : text;
}

const ONE_PLACE: Intl.NumberFormatOptions = Object.freeze({ maximumFractionDigits: 1 });
const TWO_PLACES: Intl.NumberFormatOptions = Object.freeze({ maximumFractionDigits: 2 });

/**
 * The format a figure's headline number is written in: the author's, else
 * `compact`. A stat or a meter is read at a glance, and written to every digit
 * (`123,456,789.12`) it was cut short in its tile with an ellipsis, losing the
 * digits that said how big it was; a table cell keeps every digit.
 */
export function figureFormat(w: Pick<DashboardWidget, "format">): DashboardNumberFormat {
  return w.format ?? "compact";
}

/**
 * How a chart's scale is written: as a ratio for a `percent` widget, else
 * shortened from a thousand up, and never with the unit. A scale is read at a
 * glance against its lines (`7.5k`, not `7,500 USD` on every one), and every
 * line of one scale is written the same way: shortened only past ten
 * thousand, as a figure is, one axis read `10k` over `7,500`. The tooltip
 * reads each value in full, unit and all.
 */
export function scaleFormat(w: Pick<DashboardWidget, "format">): (v: number) => string {
  if (w.format === "percent") return (v) => formatNumber(v, "percent");
  return (v) => {
    const abs = Math.abs(v);
    if (abs < 1000) return numberFormat(undefined, TWO_PLACES).format(v);
    const [n, suffix] =
      abs < 1_000_000
        ? [v / 1000, "k"]
        : abs < 1_000_000_000
          ? [v / 1_000_000, "M"]
          : [v / 1e9, "B"];
    return `${numberFormat(undefined, ONE_PLACE).format(n)}${suffix}`;
  };
}

/** The data hues a chart can tell apart, the design system's ramp. */
const RAMP = DATA_COLORS.length;

/** The series every part past the ramp folds into. */
export const OTHER_SERIES = "\u0000other";

/**
 * A chart's series, held to the hues a reader can tell apart: every series,
 * when there are no more than the ramp's hues and one residual; else the
 * first of them, in the order given, and one "Other" that the rest fold into
 * (`fold` names the series a value is summed under).
 *
 * WHY: the design system has four data hues and a neutral for the rest
 * (`dataColor`), and a board may draw six series (eight parts of a share).
 * Past the fifth, every series took the same neutral, so a legend of six
 * named two of them in one grey and a reader could not tell which was which.
 * Folded, each named series keeps a hue of its own and the grey means what it
 * means everywhere else in the product: the rest. The rows are untouched, so
 * the CSV and the dataset's preview still carry every series.
 */
export function foldSeries(series: readonly { id: string; name: string }[]): {
  series: { id: string; name: string }[];
  fold: (id: string) => string;
} {
  if (series.length <= RAMP + 1) return { series: [...series], fold: (id) => id };
  const kept = series.slice(0, RAMP);
  const ids = new Set(kept.map((s) => s.id));
  return {
    series: [...kept, { id: OTHER_SERIES, name: "Other" }],
    fold: (id) => (ids.has(id) ? id : OTHER_SERIES),
  };
}

/**
 * The datasets that stand on any of `ids`, through every calculation, the ids
 * themselves included: what refreshes when a source does.
 */
export function withDependents(
  datasets: readonly Pick<DashboardBoardDataset, "id" | "freshness">[],
  ids: Iterable<string>,
): Set<string> {
  const out = new Set(ids);
  let grew = true;
  while (grew) {
    grew = false;
    for (const d of datasets) {
      if (out.has(d.id)) continue;
      if ((d.freshness.inputs ?? []).some((input) => out.has(input))) {
        out.add(d.id);
        grew = true;
      }
    }
  }
  return out;
}

/** The interval a dataset refreshes on, in milliseconds, never under the engine's floor; 0 for none. */
export function intervalMs(f: DashboardFreshness): number {
  if (!f.every_seconds) return 0;
  return Math.max(f.every_seconds, DASHBOARD_MIN_REFRESH_SECONDS) * 1000;
}

/** What one dataset is, on the board and in this tab. */
export interface DatasetState {
  answer: DashboardDatasetAnswer | null;
  loading: boolean;
  /** A read that failed for a reason the engine did not answer as a dataset status. */
  failure: string | null;
  /** When this tab last received an answer, by its own clock. */
  received: number;
}

/**
 * What a figure says when its rows could not be read for a reason the engine
 * did not answer as the dataset's own status: a node that could not reach
 * the domain, or a route that answered nothing. One sentence of a reader's,
 * never the route's detail, which names a path and a node rather than
 * anything a person can act on. A board's datasets and New dashboard's
 * previews both say it.
 */
export const ROWS_UNREAD = "These rows could not be read right now. Refresh to try again.";

/** A dataset's state as the design system draws it. */
export function datasetStatus(
  d: Pick<DashboardBoardDataset, "source" | "freshness">,
  s: DatasetState | undefined,
  now: number,
): DatasetStatus {
  if (!s || (!s.answer && s.loading)) return "loading";
  if (s.failure) return "failed";
  switch (s.answer?.status) {
    case "refused":
      return "refused";
    case "unanswered":
      return "failed";
    case "ready": {
      const every = intervalMs(d.freshness);
      if (every > 0 && now - s.received > 2 * every) return "stale";
      return d.freshness.mode === "live" ? "live" : "ready";
    }
  }
  return "loading";
}

/**
 * How a dataset is kept current, in the rail's words. A live store says how,
 * not "Live" again: the line above it already says the rows are live.
 */
export function refreshWords(f: DashboardFreshness): string {
  switch (f.mode) {
    case "live":
      return "As its rows are written";
    case "signal":
      return f.every_seconds ? `On change, and every ${minutes(f.every_seconds)}` : "On change";
    case "interval":
      return `Every ${minutes(f.every_seconds ?? DASHBOARD_MIN_REFRESH_SECONDS)}`;
    case "manual":
      return "When the board opens or is refreshed";
    case "fixed":
      return "Fixed with this version";
    case "derived":
      return "When its inputs change";
  }
}

function minutes(seconds: number): string {
  const [n, unit] =
    seconds % 3600 === 0
      ? [seconds / 3600, "hour"]
      : seconds % 60 === 0
        ? [seconds / 60, "minute"]
        : [seconds, "second"];
  return n === 1 ? unit : plural(n, unit);
}

/** The board's filters, by key, which a bound argument is named by. */
type ParamLabels = ReadonlyMap<string, string>;

/** An argument in words: a literal as written, a binding as the filter a reader sees ("the Service filter"). */
function argText(a: DashboardArg, params: ParamLabels): string {
  return typeof a === "object" ? `the ${params.get(a.param) ?? a.param} filter` : String(a);
}

/** A filter's comparison in words, as a sentence reads it rather than as the spec spells it. */
const CMP_WORDS: Record<string, string> = {
  eq: "is",
  ne: "is not",
  lt: "is under",
  le: "is at most",
  gt: "is over",
  ge: "is at least",
  present: "has a value",
  absent: "has no value",
};

/** One calculation step in words. */
export function stepWords(st: DashboardStep, params: ParamLabels = new Map()): string {
  switch (st.op) {
    case "filter":
      return `keep rows where ${(st.where ?? [])
        .map(
          (w) =>
            `${w.column} ${CMP_WORDS[w.cmp] ?? w.cmp}${w.value === undefined ? "" : ` ${argText(w.value, params)}`}`,
        )
        .join(" and ")}`;
    case "group":
      return `group by ${(st.by ?? []).join(", ") || "all rows"}: ${(st.aggregates ?? [])
        .map((a) => `${a.column} = ${a.agg}${a.of ? `(${a.of})` : ""}`)
        .join(", ")}`;
    case "compute":
      return `${st.column} = ${st.fn}(${[st.left, st.right].filter((o) => o !== undefined).join(", ")})`;
    case "join":
      return `join ${st.with} on ${st.on}, adding ${(st.columns ?? []).join(", ")}`;
    case "sort":
      return `sort by ${(st.order ?? []).map((o) => `${o.column}${o.desc ? " descending" : ""}`).join(", ")}`;
    case "limit":
      return `keep the first ${st.count}`;
    case "select":
      return `keep ${(st.columns ?? []).join(", ")}`;
  }
}

/**
 * What a dataset reads, in the rail's words: a source and its arguments, a
 * collection, a file, or the steps. A filter is named as the reader sees it,
 * by its label, and an argument bound to one says so: "scope from the Whose
 * work filter", where the key alone read "scope the scope filter".
 */
export function definitionWords(
  d: DashboardDataset,
  boardParams: readonly Pick<DashboardParam, "key" | "label">[] = [],
): string {
  const params: ParamLabels = new Map(boardParams.map((p) => [p.key, p.label]));
  switch (d.source) {
    case "engine": {
      const args = Object.entries(d.engine?.params ?? {}).map(([k, v]) =>
        typeof v === "object" ? `${k} from ${argText(v, params)}` : `${k} ${argText(v, params)}`,
      );
      return args.length ? `${d.engine?.name}, ${args.join(", ")}` : (d.engine?.name ?? "");
    }
    case "store":
      return `The ${d.store?.collection} collection of this dashboard's data`;
    case "file":
      return `The file ${d.file?.path} of this version`;
    case "calc":
      return [
        `From ${d.calc?.from}`,
        ...(d.calc?.steps ?? []).map((st) => stepWords(st, params)),
      ].join("; ");
  }
}

/** A rows count as the rail says it. */
export function rowsWords(answer: DashboardReading | null | undefined): number | undefined {
  return answer?.status === "ready" ? (answer.table?.rows.length ?? 0) : undefined;
}

/** A dataset's rows as CSV cells, in its columns' order, each value as the spreadsheet should hold it. */
export function csvOf(answer: DashboardReading): {
  header: string[];
  rows: DashboardValue[][];
} {
  const columns = answer.table?.columns ?? [];
  return {
    header: columns.map((c) => c.key),
    rows: (answer.table?.rows ?? []).map((r) => columns.map((c) => r[c.key] ?? null)),
  };
}

/** The filename a widget's export is saved under. */
export function exportName(board: string, widget: string, ext: "csv" | "png"): string {
  const stem = `${board}-${widget}`
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");
  return `${stem || "dashboard"}.${ext}`;
}

/**
 * The narrowest a widget is drawn, in pixels: room for a title of a word or
 * two beside the widget's menu, and a figure under it.
 */
const WIDGET_MIN_PX = 120;

/** The spans a narrowed widget snaps to: those that tile a row of twelve evenly. */
const EVEN_SPANS = [1, 2, 3, 4, 6, 12];

/**
 * The columns a widget spans on a board `width` pixels wide with `gap`
 * between columns: its own `w`, unless that is narrower than [WIDGET_MIN_PX],
 * when it takes the fewest columns of an even span that are not.
 *
 * WHY: a board is laid out at whatever width its author saw, and read at every
 * other. Two columns of a board that has lost its Sources rail's width to the
 * page (a laptop) were 105px, and a widget's title broke inside its words
 * ("Empt|y", "Budg|et"). Widened to an even span, a row of narrow widgets still
 * tiles its rows (six of two become four of three).
 */
export function boardSpan(w: number, width: number, gap: number, columns = 12): number {
  if (width <= 0) return w;
  const column = (width - gap * (columns - 1)) / columns;
  const drawn = w * column + (w - 1) * gap;
  if (drawn >= WIDGET_MIN_PX) return w;
  const need = Math.ceil((WIDGET_MIN_PX + gap) / (column + gap));
  return EVEN_SPANS.find((span) => span >= need) ?? columns;
}

/** Every widget of a board, a group's children after it, with the group each sits in. */
export function eachWidget(
  widgets: readonly DashboardWidget[],
): { widget: DashboardWidget; group?: string }[] {
  const out: { widget: DashboardWidget; group?: string }[] = [];
  for (const w of widgets) {
    out.push({ widget: w });
    for (const c of w.children ?? []) out.push({ widget: c, group: w.id });
  }
  return out;
}

/**
 * How wide a run of a widget table's text is drawn, in pixels: a `heading`
 * (set in capitals and tracked open, as the compact table's header is) or a
 * value. The board measures with the fonts the table is drawn in (see
 * `tableMeasure.ts`); [estimateTableText] stands in where nothing is drawn.
 */
export type TableMeasure = (text: string, heading: boolean) => number;

/**
 * An estimate of [TableMeasure], from a character's average width on the
 * compact step (a heading's capitals wider), for a suite and for a browser
 * that cannot measure. The board does not size by it: an average reserved too
 * much for lowercase words and too little for capitals, so one table both ran
 * past its widget and cut `DEP-1050` to `DEP-105…`.
 */
export const estimateTableText: TableMeasure = (text, heading) =>
  text.length * (heading ? 8.5 : 7.5);

/**
 * A cell's padding: the compact table's 12px each side and 4px of air. The
 * table is flush in its widget, so its first and last columns stand on the
 * frame's 16px instead of the cell's 12px on their outer side (TABLE_EDGE_PX
 * more). A heading carries no sort control (a widget's rows are in the order
 * its dataset gives them), so its padding is a cell's.
 */
const TABLE_CELL_PAD_PX = 28;
const TABLE_EDGE_PX = 4;

/** The narrowest and widest a table column starts at, in pixels. */
const TABLE_MIN_PX = 72;
const TABLE_MAX_PX = 320;

/**
 * The narrowest the prose column is fitted to, about twenty characters: what
 * still reads as the start of a title. Past it the table scrolls sideways.
 */
const TABLE_PROSE_MIN_PX = 150;

/** How a dashboard table's columns are laid out: see [tableLayout]. */
export interface TableLayout {
  /** Each column's width from what it holds. */
  widths: Record<string, number>;
  /** The one column that takes the table's leftover room, or none. */
  flex: string | undefined;
  /** The narrowest the flexing column is fitted to. */
  flexMin: number;
}

/**
 * Each column's width, from what it holds: the wider of its heading and its
 * longest value as `measure` says each is drawn, at least [TABLE_MIN_PX] and at
 * most [TABLE_MAX_PX]; and the one column that gives or takes the room left
 * in the widget.
 *
 * WHY A TABLE IS SIZED BY ITS CONTENT: a column the design system's compact
 * table may fit is shrunk to its box, so on a narrow widget a heading is cut
 * to its first letters and a value is broken mid-word (`succeed|ed`). Held at
 * what it holds (a column's `width`), a column keeps its words whole.
 *
 * WHY ONE COLUMN FLEXES: held at their content every column, a table either
 * stopped short of its widget's side or ran past it and scrolled. The column
 * of prose (a free text column whose values are words with spaces between
 * them, the longest such, a title) is the one a reader reads by its start, so
 * it alone is fitted: it takes what is left of the widget's width, and gives
 * up its tail behind an ellipsis when there is less, down to
 * [TABLE_PROSE_MIN_PX], while every other column
 * (a key, a version, a state, a date, a number) keeps its value whole. A table
 * whose held columns alone are wider than its widget scrolls sideways.
 */
export function tableLayout(
  shown: readonly { key: string; label: string; column: DashboardColumn | undefined }[],
  rows: readonly DashboardRow[],
  measure: TableMeasure = estimateTableText,
): TableLayout {
  const widths: Record<string, number> = {};
  let flex: string | undefined;
  let flexChars = 0;
  for (const [at, { key, label, column }] of shown.entries()) {
    const edge = (at === 0 ? TABLE_EDGE_PX : 0) + (at === shown.length - 1 ? TABLE_EDGE_PX : 0);
    let chars = 0;
    let drawn = 0;
    let spaced = false;
    for (const r of rows) {
      const v = r[key] ?? null;
      const text = cellText(column, v);
      chars = Math.max(chars, text.length);
      drawn = Math.max(drawn, measure(text, false));
      spaced ||= /\s/.test(text.trim());
    }
    const head = Math.ceil(measure(label.toUpperCase(), true)) + TABLE_CELL_PAD_PX + edge;
    const body = Math.ceil(drawn) + TABLE_CELL_PAD_PX + edge;
    widths[key] = Math.round(Math.min(TABLE_MAX_PX, Math.max(TABLE_MIN_PX, head, body)));
    // Prose is free text in words: a column of a closed vocabulary is a
    // state, and one value with no space in it an identifier, each read
    // whole, whatever its length.
    const prose = (column?.type ?? "text") === "text" && !column?.words && spaced;
    if (prose && chars > flexChars) {
      flex = key;
      flexChars = chars;
    }
  }
  const flexMin = flex ? Math.min(widths[flex] ?? 0, TABLE_PROSE_MIN_PX) : 0;
  return { widths, flex, flexMin };
}
