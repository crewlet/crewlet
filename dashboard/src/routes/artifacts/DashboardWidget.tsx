/**
 * One widget of a dashboard: its figure, drawn by the design system's own
 * primitive for its type from the rows its dataset answered for this reader,
 * inside the frame that cites the dataset and when its rows were true.
 *
 * A widget over a dataset the reader may not read draws no figure: the frame
 * says why, in the engine's words, and no row of it ever reached this tab.
 */

import { useRef, type ReactNode } from "react";
import {
  ActivityStrip,
  BarList,
  ChartEmpty,
  DataTable,
  EmptyValue,
  downloadBlob,
  Legend,
  elementToPng,
  ElementToPngError,
  Menu,
  Meter,
  Note,
  StackedBar,
  StackedColumns,
  StatCard,
  TimeSeries,
  toCsv,
  useToast,
  WidgetFrame,
  type DatasetStatus,
  type DataTableColumn,
} from "@crewlethq/ui";
import { EllipsisGlyph } from "@crewlethq/icons/glyphs";
import type {
  DashboardBoardDataset,
  DashboardColumn,
  DashboardReading,
  DashboardRow,
  DashboardValue,
  DashboardWidget as Widget,
} from "~/protocol/index.ts";
import {
  allDays,
  cellText,
  chartTimeOf,
  csvOf,
  effectiveAgg,
  exportName,
  figureFormat,
  fold,
  foldSeries,
  formatDay,
  formatNumber,
  scaleFormat,
  tableLayout,
  wordsOf,
} from "~/lib/artifacts/dashboard.ts";
import { tableMeasure } from "~/lib/artifacts/tableMeasure.ts";

/** The widget types drawn as a chart, which a reader may save as an image. */
const CHARTS = new Set<Widget["type"]>(["timeseries", "columns", "activity", "bars", "share"]);

/**
 * What a figure says when its dataset answered no rows: one phrase for every
 * widget kind, a table's included, where the board said "No rows." under one
 * figure and "Nothing recorded in this window" under the next.
 *
 * WORDED BY WHETHER THE SOURCE HAS A WINDOW. A spend source reads a span of
 * company days (`windowed` in the catalogue), so its empty answer is nothing
 * in that window; every other source reads what is true now (open work, the
 * agents, an artifact's own rows), where "this window" names a window the
 * reader cannot find.
 */
export function nothingSaid(dataset: DashboardBoardDataset | undefined): string {
  return dataset?.engine?.windowed ? "Nothing in this window" : "Nothing to show";
}

/** Why a chart could not be saved as an image, in a reader's words. */
const PNG_REFUSALS: Record<ElementToPngError["reason"], string> = {
  unsupported: "This browser cannot save this chart as an image.",
  tainted: "This browser blocked saving this chart as an image.",
  empty: "The chart has nothing drawn to save yet.",
};

export interface DashboardWidgetProps {
  boardTitle: string;
  widget: Widget;
  dataset: DashboardBoardDataset | undefined;
  answer: DashboardReading | null;
  status: DatasetStatus;
  detail: string | undefined;
  asOf: string | undefined;
  onCite: (() => void) | undefined;
  onFocus: () => void;
  /**
   * The widget stands in a row of others read across (a board wider than a
   * phone): a stat's and a meter's frame keep their title's two lines, so
   * every number of the row starts on one line.
   */
  inRow?: boolean;
  /**
   * The frame's own controls in place of the export menu: a builder's Add,
   * where the figure is a suggestion and not yet a board's.
   */
  actions?: ReactNode;
}

export function DashboardWidget({
  boardTitle,
  widget,
  dataset,
  answer,
  status,
  detail,
  asOf,
  onCite,
  onFocus,
  inRow = false,
  actions: own,
}: DashboardWidgetProps) {
  const toast = useToast();
  const figure = useRef<HTMLDivElement>(null);
  const title = widget.title ?? "";

  if (widget.type === "note") {
    return (
      <Note
        tone="none"
        heading={title || undefined}
        className="dashboard-note"
        onFocus={onFocus}
        tabIndex={-1}
      >
        {(widget.text ?? "").split(/\n{2,}/).map((para, i) => (
          <p key={i}>{para}</p>
        ))}
      </Note>
    );
  }

  const ready = answer?.status === "ready" && answer.table ? answer.table : null;
  const saveCsv = () => {
    if (!answer || answer.status !== "ready") return;
    const { header, rows } = csvOf(answer);
    downloadBlob(
      new Blob([toCsv(header, rows)], { type: "text/csv;charset=utf-8" }),
      exportName(boardTitle, widget.id, "csv"),
    );
  };
  const savePng = async () => {
    if (!figure.current) return;
    try {
      const blob = await elementToPng(figure.current);
      downloadBlob(blob, exportName(boardTitle, widget.id, "png"));
    } catch (err) {
      toast.show({
        variant: "warning",
        title: "The image was not saved",
        message:
          err instanceof ElementToPngError ? PNG_REFUSALS[err.reason] : PNG_REFUSALS.unsupported,
      });
    }
  };
  const actions =
    own !== undefined ? (
      own
    ) : ready ? (
      <Menu
        label={`Export ${title || "this widget"}`}
        icon={<EllipsisGlyph size="sm" />}
        align="end"
        items={[
          { key: "csv", label: "Download CSV", onSelect: saveCsv },
          ...(CHARTS.has(widget.type)
            ? [{ key: "png", label: "Download PNG", onSelect: () => void savePng() }]
            : []),
        ]}
      />
    ) : undefined;

  return (
    <WidgetFrame
      title={title}
      source={{ label: dataset?.title ?? widget.dataset ?? "", status, asOf, detail }}
      onCite={onCite}
      actions={actions}
      // A TABLE REACHES THE FRAME'S SIDES, its row rules meeting the border
      // and its first column under the title, as a table in any card does.
      flush={widget.type === "table"}
      holdTitle={inRow && (widget.type === "stat" || widget.type === "meter")}
      onFocus={onFocus}
      data-widget={widget.id}
    >
      <div ref={figure} className="dashboard-figure" data-figure={widget.type}>
        {ready ? (
          <Figure
            widget={widget}
            columns={ready.columns}
            rows={ready.rows}
            datasetTitle={dataset?.title ?? widget.dataset ?? ""}
            empty={nothingSaid(dataset)}
          />
        ) : null}
      </div>
    </WidgetFrame>
  );
}

/** The figure one widget type draws from its rows. */
function Figure({
  widget: w,
  columns,
  rows,
  datasetTitle,
  empty,
}: {
  widget: Widget;
  columns: readonly DashboardColumn[];
  rows: readonly DashboardRow[];
  /** What the figure counts when it names no column: its dataset. */
  datasetTitle: string;
  /** What it says over no rows (`nothingSaid`). */
  empty: string;
}): ReactNode {
  const column = (key: string | undefined) => columns.find((c) => c.key === key);
  const label = (key: string | undefined) => column(key)?.label ?? key ?? "";
  const num = (v: DashboardValue) => formatNumber(v, w.format, w.unit);
  const title = w.title ?? "";
  // A tile's number at a glance, its unit beside it at the tile's quieter step.
  const figure = (v: DashboardValue) => formatNumber(v, figureFormat(w));
  switch (w.type) {
    case "stat": {
      const agg = effectiveAgg(w);
      const value = fold(agg, column(w.value)?.type ?? "number", rows, w.value ?? "");
      // THE FRAME'S TITLE NAMES THE NUMBER, so the tile's own label (the
      // column it folds) is said to a screen reader and not drawn under it.
      return (
        <StatCard
          flush
          hideLabel
          label={w.value ? label(w.value) : datasetTitle}
          value={figure(value)}
          unit={value === null ? undefined : w.unit}
        />
      );
    }
    case "meter": {
      const agg = effectiveAgg(w);
      const value = fold(agg, "number", rows, w.value ?? "");
      const max = w.max ?? fold(agg, "number", rows, w.max_field ?? "");
      const v = typeof value === "number" ? value : 0;
      const m = typeof max === "number" && max > 0 ? max : 1;
      const of = (x: DashboardValue) => formatNumber(x, figureFormat(w), w.unit);
      const named = w.value ? label(w.value) : datasetTitle;
      // A METER IS A STAT WITH ITS BAR: the number at the tile's display
      // step, what it is out of on the quiet line under it (beside it, a
      // tile two columns wide cut the maximum to "of 10…"), and the bar under
      // both. The tile is the bar's words drawn; a screen reader hears them
      // once, from the meter.
      return (
        <div className="dashboard-meter">
          <StatCard
            flush
            hideLabel
            aria-hidden
            label={named}
            value={figure(value)}
            unit={value === null ? undefined : w.unit}
            sub={`of ${of(max)}`}
          />
          <Meter
            value={v}
            max={m}
            hideLabel
            label={named}
            valueText={`${of(value)} of ${of(max)}`}
            // THE DESIGN SYSTEM'S RAMP: a meter is a number against a limit,
            // and the bar says so as it nears it (warning from three quarters,
            // danger at the limit), which is what a board's author writes a
            // meter for ("the failure meter turns red at five").
          />
        </div>
      );
    }
    case "timeseries":
    case "columns": {
      const { series, buckets, from, to } = seriesOf(w, rows, label, column(w.by));
      const days = allDays(rows.map((r) => r[w.x ?? ""] ?? null));
      const time = days ? { formatTime: (at: number) => formatDay(at) } : {};
      // ONE NAMED COLUMN IS NAMED BY THE TITLE ("Deploys per day"), so it
      // draws no key; a split names its bands nowhere else, so it keeps one
      // even when it answered a single band.
      const legend = !w.by && series.length === 1 ? ("none" as const) : ("below" as const);
      if (w.type === "timeseries") {
        return (
          <TimeSeries
            label={title}
            from={from}
            to={to}
            format={(v) => num(v)}
            emptyLabel={empty}
            legend={legend}
            {...time}
            series={series.map((s) => ({
              id: s.id,
              name: s.name,
              points: buckets.map((b) => ({ t: b.t, v: b.values[s.id] ?? 0 })),
            }))}
          />
        );
      }
      return (
        <StackedColumns
          label={title}
          series={series}
          buckets={buckets}
          format={(v) => num(v)}
          formatScale={scaleFormat(w)}
          emptyLabel={empty}
          legend={legend}
          {...time}
        />
      );
    }
    case "activity": {
      const y = w.y?.[0] ?? "";
      const days = allDays(rows.map((r) => r[w.x ?? ""] ?? null));
      return (
        <ActivityStrip
          label={title}
          emptyLabel={empty}
          reading={{
            formatTime: days ? (at) => formatDay(at) : (at) => new Date(at).toLocaleString(),
            format: (v) => num(v),
          }}
          buckets={timed(rows, w.x ?? "").map((r) => ({
            t: chartTimeOf(r[w.x ?? ""] ?? null),
            v: numberOr0(r[y]),
          }))}
        />
      );
    }
    case "bars":
      // AN EMPTY LIST STANDS IN ITS PLOT'S MIDDLE, as every empty chart on
      // the board does, not where its first row would have begun.
      if (rows.length === 0) return <ChartEmpty>{empty}</ChartEmpty>;
      return (
        <BarList
          emptyLabel={empty}
          limit={w.limit ?? 10}
          data={rows.map((r, i) => ({
            id: String(i),
            label: wordsOf(column(w.label), r[w.label ?? ""] ?? null),
            value: numberOr0(r[w.value ?? ""]),
            display: num(r[w.value ?? ""] ?? null),
          }))}
        />
      );
    case "share": {
      // A SHARE IS NEVER DRAWN WITHOUT ITS LEGEND: the bar names none of its
      // parts, so the legend, keyed from the same list, says which part is
      // which and how much it holds.
      const parts = shareParts(
        rows.slice(0, w.limit ?? 8).map((r, i) => ({
          id: String(i),
          label: wordsOf(column(w.label), r[w.label ?? ""] ?? null),
          value: numberOr0(r[w.value ?? ""]),
        })),
      ).map((p) => ({ ...p, shown: num(p.value) }));
      if (!parts.some((p) => p.value > 0)) {
        return <StackedBar format={(v) => num(v)} segments={[]} emptyLabel={empty} />;
      }
      return (
        <div className="dashboard-share">
          <StackedBar
            format={(v) => num(v)}
            segments={parts.map(({ id, label, value }) => ({ id, label, value }))}
          />
          <Legend items={parts.map((p) => ({ id: p.id, label: p.label, value: p.shown }))} />
        </div>
      );
    }
    case "table":
      return (
        <RowsTable
          columns={columns}
          shown={w.columns ?? columns.map((c) => c.key)}
          rows={rows.slice(0, w.limit ?? 100)}
          empty={empty}
        />
      );
  }
  return null;
}

/** A table cell's words. */
function numberOr0(v: DashboardValue | undefined): number {
  return typeof v === "number" && Number.isFinite(v) ? v : 0;
}

/**
 * A dataset's rows as a table: a widget's, and the Sources rail's preview of
 * a dataset, drawn by one rule so the two never read one row two ways.
 */
export function RowsTable({
  columns,
  shown,
  rows,
  empty,
}: {
  columns: readonly DashboardColumn[];
  /** The columns drawn, by key, in order. */
  shown: readonly string[];
  /** The rows drawn, already cut. */
  rows: readonly DashboardRow[];
  /** What it says over no rows (`nothingSaid`). */
  empty: string;
}) {
  const column = (key: string) => columns.find((c) => c.key === key);
  const label = (key: string) => column(key)?.label ?? key;
  const { widths, flex, flexMin } = tableLayout(
    shown.map((key) => ({ key, label: label(key), column: column(key) })),
    rows,
    tableMeasure(),
  );
  const table: Record<string, DataTableColumn> = {};
  for (const key of shown) {
    const c = column(key);
    table[key] = {
      label: label(key),
      // HELD, BUT FOR THE PROSE: a held `width` the table leaves alone, so a
      // short column keeps its words whole; the one column given a
      // `defaultWidth` is fitted to the room the others leave (see
      // tableLayout).
      ...(key === flex
        ? { defaultWidth: widths[key] ?? 0, minWidth: flexMin }
        : { width: widths[key] ?? 0 }),
      // THE ROWS ARE IN THEIR DATASET'S ORDER: the dataset sorts and cuts
      // them ("sort by day descending; keep the first 8"), and sorting what
      // is left by another column would read as an order over every row.
      sortable: false,
      ...(c?.type === "number" ? { align: "right" as const } : {}),
      // AN ABSENT VALUE IS THE EMPTY MARK, the dash a stat draws for one,
      // never a blank cell (which reads as a value nobody looked at) or a
      // word for it ("None").
      render: (_row, value) => {
        const text = cellText(c, value as DashboardValue);
        return text === "" ? <EmptyValue label="No value" /> : text;
      },
    };
  }
  return (
    <DataTable
      variant="compact"
      // NO CHROME ROW: the rows are already cut to what is drawn, so a pager
      // and a settings control over two rows were a band of controls with
      // nothing to do.
      paginated={false}
      showSettings={false}
      // NEVER BROKEN MID-WORD: a value too long for its column ends in an
      // ellipsis, and only the prose column is ever narrower than what it
      // holds; a table wider than its box scrolls sideways.
      defaultWrapLines={false}
      // NO RESIZE HANDLES: the widths are the content's, and a width dragged
      // in a read-only figure was kept nowhere; the handles stood in the
      // header as a row of 37px rules between headings.
      resizable={false}
      className="dashboard-rows"
      columns={table}
      data={rows.map((r, i) => ({ ...r, __row: i }))}
      getRowKey={(r) => Number(r.__row)}
      emptyMessage={empty}
    />
  );
}

/** A share's parts, held to the hues a reader can tell apart, the rest summed as "Other" (see foldSeries). */
function shareParts(
  parts: readonly { id: string; label: string; value: number }[],
): { id: string; label: string; value: number }[] {
  const { series, fold: into } = foldSeries(parts.map((p) => ({ id: p.id, name: p.label })));
  const sum = new Map<string, number>();
  for (const p of parts) sum.set(into(p.id), (sum.get(into(p.id)) ?? 0) + p.value);
  return series.map((s) => ({ id: s.id, label: s.name, value: sum.get(s.id) ?? 0 }));
}

/** Rows with a place on the time axis at `x`, oldest first. */
function timed(rows: readonly DashboardRow[], x: string): DashboardRow[] {
  return rows
    .filter((r) => !Number.isNaN(chartTimeOf(r[x] ?? null)))
    .sort((a, b) => chartTimeOf(a[x] ?? null) - chartTimeOf(b[x] ?? null));
}

/** A chart over time: one series per `y`, or one per value of `by` over its one `y`. */
function seriesOf(
  w: Widget,
  rows: readonly DashboardRow[],
  label: (key: string) => string,
  byColumn: DashboardColumn | undefined,
): {
  series: { id: string; name: string }[];
  buckets: { t: number; values: Record<string, number> }[];
  from: number;
  to: number;
} {
  const x = w.x ?? "";
  const ys = w.y ?? [];
  const byT = new Map<number, Record<string, number>>();
  const names = new Map<string, string>();
  for (const r of timed(rows, x)) {
    const t = chartTimeOf(r[x] ?? null);
    const values = byT.get(t) ?? {};
    if (w.by) {
      const band = String(r[w.by] ?? "");
      names.set(band, wordsOf(byColumn, r[w.by] ?? null));
      values[band] = (values[band] ?? 0) + numberOr0(r[ys[0] ?? ""]);
    } else {
      for (const y of ys) {
        names.set(y, label(y));
        values[y] = (values[y] ?? 0) + numberOr0(r[y]);
      }
    }
    byT.set(t, values);
  }
  const all = [...names.entries()].map(([id, name]) => ({ id, name }));
  // A split's series are its values in no order of their own, so the ones
  // kept are the biggest over the window; columns named by the author keep
  // the author's order.
  const totals = new Map<string, number>();
  for (const values of byT.values()) {
    for (const [id, v] of Object.entries(values))
      totals.set(id, (totals.get(id) ?? 0) + Math.abs(v));
  }
  const ranked = w.by
    ? [...all].sort((a, b) => (totals.get(b.id) ?? 0) - (totals.get(a.id) ?? 0))
    : all;
  const { series, fold: into } = foldSeries(ranked);
  const buckets = [...byT.entries()].map(([t, values]) => {
    const folded: Record<string, number> = {};
    for (const [id, v] of Object.entries(values)) folded[into(id)] = (folded[into(id)] ?? 0) + v;
    return { t, values: folded };
  });
  const ts = buckets.map((b) => b.t);
  return {
    series,
    buckets,
    from: ts.length ? Math.min(...ts) : 0,
    to: ts.length ? Math.max(...ts) : 0,
  };
}
