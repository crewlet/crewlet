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
  DataTable,
  downloadBlob,
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
  DashboardDatasetAnswer,
  DashboardRow,
  DashboardValue,
  DashboardWidget as Widget,
} from "~/protocol/index.ts";
import {
  csvOf,
  effectiveAgg,
  exportName,
  fold,
  formatNumber,
  instantOf,
} from "~/lib/artifacts/dashboard.ts";

/** The widget types drawn as a chart, which a reader may save as an image. */
const CHARTS = new Set<Widget["type"]>(["timeseries", "columns", "activity", "bars", "share"]);

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
  answer: DashboardDatasetAnswer | null;
  status: DatasetStatus;
  detail: string | undefined;
  asOf: string | undefined;
  onCite: (() => void) | undefined;
  onFocus: () => void;
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
}: DashboardWidgetProps) {
  const toast = useToast();
  const figure = useRef<HTMLDivElement>(null);
  const title = widget.title ?? "";

  if (widget.type === "note") {
    return (
      <Note tone="none" onFocus={onFocus} tabIndex={-1}>
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
  const actions = ready ? (
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
      onFocus={onFocus}
      data-widget={widget.id}
    >
      <div ref={figure} className="dashboard-figure">
        {ready ? <Figure widget={widget} columns={ready.columns} rows={ready.rows} /> : null}
      </div>
    </WidgetFrame>
  );
}

/** The figure one widget type draws from its rows. */
function Figure({
  widget: w,
  columns,
  rows,
}: {
  widget: Widget;
  columns: readonly DashboardColumn[];
  rows: readonly DashboardRow[];
}): ReactNode {
  const column = (key: string | undefined) => columns.find((c) => c.key === key);
  const label = (key: string | undefined) => column(key)?.label ?? key ?? "";
  const num = (v: DashboardValue) => formatNumber(v, w.format, w.unit);
  const title = w.title ?? "";
  switch (w.type) {
    case "stat": {
      const agg = effectiveAgg(w);
      const value = fold(agg, column(w.value)?.type ?? "number", rows, w.value ?? "");
      return <StatCard flush label={w.value ? label(w.value) : "Rows"} value={num(value)} />;
    }
    case "meter": {
      const agg = effectiveAgg(w);
      const value = fold(agg, "number", rows, w.value ?? "");
      const max = w.max ?? fold(agg, "number", rows, w.max_field ?? "");
      const v = typeof value === "number" ? value : 0;
      const m = typeof max === "number" && max > 0 ? max : 1;
      return (
        <Meter
          value={v}
          max={m}
          label={title}
          hideLabel
          valueText={`${num(value)} of ${num(max)}`}
        />
      );
    }
    case "timeseries":
    case "columns": {
      const { series, buckets, from, to } = seriesOf(w, rows, label);
      if (w.type === "timeseries") {
        return (
          <TimeSeries
            label={title}
            from={from}
            to={to}
            format={(v) => num(v)}
            series={series.map((s) => ({
              id: s.id,
              name: s.name,
              points: buckets.map((b) => ({ t: b.t, v: b.values[s.id] ?? 0 })),
            }))}
          />
        );
      }
      return (
        <StackedColumns label={title} series={series} buckets={buckets} format={(v) => num(v)} />
      );
    }
    case "activity": {
      const y = w.y?.[0] ?? "";
      return (
        <ActivityStrip
          label={title}
          buckets={timed(rows, w.x ?? "").map((r) => ({
            t: instantOf(r[w.x ?? ""] ?? null),
            v: numberOr0(r[y]),
          }))}
        />
      );
    }
    case "bars":
      return (
        <BarList
          limit={w.limit ?? 10}
          data={rows.map((r, i) => ({
            id: String(i),
            label: String(r[w.label ?? ""] ?? ""),
            value: numberOr0(r[w.value ?? ""]),
            display: num(r[w.value ?? ""] ?? null),
          }))}
        />
      );
    case "share":
      return (
        <StackedBar
          format={(v) => num(v)}
          segments={rows.slice(0, w.limit ?? 8).map((r, i) => ({
            id: String(i),
            label: String(r[w.label ?? ""] ?? ""),
            value: numberOr0(r[w.value ?? ""]),
          }))}
        />
      );
    case "table": {
      const shown = w.columns ?? columns.map((c) => c.key);
      const table: Record<string, DataTableColumn> = {};
      for (const key of shown) {
        const c = column(key);
        table[key] = {
          label: label(key),
          ...(c?.type === "number" ? { align: "right" as const } : {}),
          render: (_row, value) => cell(value as DashboardValue, c),
        };
      }
      return (
        <DataTable
          variant="compact"
          columns={table}
          data={rows.slice(0, w.limit ?? 100).map((r, i) => ({ ...r, __row: i }))}
          getRowKey={(r) => Number(r.__row)}
          emptyMessage="No rows."
        />
      );
    }
  }
  return null;
}

/** A table cell's words. */
function cell(v: DashboardValue, c: DashboardColumn | undefined): ReactNode {
  if (v === null) return "";
  if (typeof v === "boolean") return v ? "Yes" : "No";
  if (c?.type === "number" && typeof v === "number") return formatNumber(v, "number");
  return String(v);
}

function numberOr0(v: DashboardValue | undefined): number {
  return typeof v === "number" && Number.isFinite(v) ? v : 0;
}

/** Rows with an instant at `x`, oldest first. */
function timed(rows: readonly DashboardRow[], x: string): DashboardRow[] {
  return rows
    .filter((r) => !Number.isNaN(instantOf(r[x] ?? null)))
    .sort((a, b) => instantOf(a[x] ?? null) - instantOf(b[x] ?? null));
}

/** A chart over time: one series per `y`, or one per value of `by` over its one `y`. */
function seriesOf(
  w: Widget,
  rows: readonly DashboardRow[],
  label: (key: string) => string,
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
    const t = instantOf(r[x] ?? null);
    const values = byT.get(t) ?? {};
    if (w.by) {
      const band = String(r[w.by] ?? "");
      names.set(band, band);
      values[band] = (values[band] ?? 0) + numberOr0(r[ys[0] ?? ""]);
    } else {
      for (const y of ys) {
        names.set(y, label(y));
        values[y] = (values[y] ?? 0) + numberOr0(r[y]);
      }
    }
    byT.set(t, values);
  }
  const buckets = [...byT.entries()].map(([t, values]) => ({ t, values }));
  const ts = buckets.map((b) => b.t);
  return {
    series: [...names.entries()].map(([id, name]) => ({ id, name })),
    buckets,
    from: ts.length ? Math.min(...ts) : 0,
    to: ts.length ? Math.max(...ts) : 0,
  };
}
