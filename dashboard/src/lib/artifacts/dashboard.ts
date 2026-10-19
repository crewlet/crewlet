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
  DashboardFreshness,
  DashboardNumberFormat,
  DashboardRow,
  DashboardStep,
  DashboardValue,
  DashboardWidget,
} from "~/protocol/index.ts";
import { EMPTY_VALUE, type DatasetStatus } from "@crewlethq/ui";
import { fmtCount, fmtExact, plural } from "~/lib/format.ts";

/** A day (`YYYY-MM-DD`) or an instant as milliseconds, NaN for anything else. */
export function instantOf(v: DashboardValue): number {
  if (typeof v !== "string") return Number.NaN;
  return Date.parse(/^\d{4}-\d{2}-\d{2}$/.test(v) ? `${v}T00:00:00Z` : v);
}

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
 * `agg` over `column` of `rows`, a column of type `t`: none where no row holds
 * a value, except a count, which is a number of rows (every row when no column
 * is named). The engine's `Fold`.
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
      text = `${(v * 100).toLocaleString(undefined, { maximumFractionDigits: 1 })}%`;
      break;
    case "compact":
      text = fmtCount(v);
      break;
    default:
      text = v.toLocaleString(undefined, { maximumFractionDigits: 2 });
  }
  return unit ? `${text} ${unit}` : text;
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

/** How a dataset is kept current, in the rail's words. */
export function refreshWords(f: DashboardFreshness): string {
  switch (f.mode) {
    case "live":
      return "Live";
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

function argText(a: DashboardArg): string {
  return typeof a === "object" ? `the ${a.param} filter` : String(a);
}

/** One calculation step in words. */
export function stepWords(st: DashboardStep): string {
  switch (st.op) {
    case "filter":
      return `keep rows where ${(st.where ?? [])
        .map((w) => `${w.column} ${w.cmp}${w.value === undefined ? "" : ` ${argText(w.value)}`}`)
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

/** What a dataset reads, in the rail's words: a source and its arguments, a collection, a file, or the steps. */
export function definitionWords(d: DashboardDataset): string {
  switch (d.source) {
    case "engine": {
      const args = Object.entries(d.engine?.params ?? {}).map(([k, v]) => `${k} ${argText(v)}`);
      return args.length ? `${d.engine?.name}, ${args.join(", ")}` : (d.engine?.name ?? "");
    }
    case "store":
      return `The ${d.store?.collection} collection of this dashboard's data`;
    case "file":
      return `The file ${d.file?.path} of this version`;
    case "calc":
      return [`From ${d.calc?.from}`, ...(d.calc?.steps ?? []).map(stepWords)].join("; ");
  }
}

/** A rows count as the rail says it. */
export function rowsWords(answer: DashboardDatasetAnswer | null | undefined): number | undefined {
  return answer?.status === "ready" ? (answer.table?.rows.length ?? 0) : undefined;
}

/** A dataset's rows as CSV cells, in its columns' order, each value as the spreadsheet should hold it. */
export function csvOf(answer: DashboardDatasetAnswer): {
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
