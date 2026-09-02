/**
 * The chart kit.
 *
 * Hand-rolled SVG rather than a charting library, for one reason that decides
 * it: every mark here has to read tokens, in two themes, at the same contrast
 * floors the palette suite measures. A library's theming surface is a second
 * design system to keep in step with this one, and the four shapes this
 * product actually needs are ~40 lines each.
 *
 * The shapes, and when each is right:
 *
 *  - `BarList`   — a ranked comparison. The default, and what "by model", "by
 *                  seat", "by phase" all want: a sorted horizontal bar list
 *                  reads exactly as fast as it is long, and the labels sit on
 *                  a straight left edge instead of rotated under an axis.
 *  - `StackedBar`— one whole split into parts. Always with its legend.
 *  - `TimeSeries`— a quantity over time, when TIME is the question.
 *  - `Sparkline` — a shape beside a number, never on its own.
 *
 * There is no pie chart. An angle is the hardest encoding to compare and every
 * question a pie would answer here is a `StackedBar` plus a legend.
 */

import { useId, type ReactNode } from "react";
import { cx } from "./primitives.tsx";

/** The five data hues plus the residual bucket, in their fixed order. */
export const VIZ = [
  "var(--viz-1)",
  "var(--viz-2)",
  "var(--viz-3)",
  "var(--viz-4)",
  "var(--viz-5)",
] as const;
export const VIZ_OTHER = "var(--viz-other)";

/** The colour of series `i`, with everything past the fifth as the residual. */
export function vizColor(i: number): string {
  return VIZ[i] ?? VIZ_OTHER;
}

/** Phase is its own fixed identity, not a viz slot — see tokens.css. */
export function phaseColor(phase: string): string {
  switch ((phase || "").toLowerCase()) {
    case "onboarding":
      return "var(--phase-onboarding)";
    case "execute":
      return "var(--phase-execute)";
    case "review":
      return "var(--phase-review)";
    default:
      return "var(--viz-other)";
  }
}

export function Legend({ items }: { items: { label: ReactNode; color: string }[] }) {
  return (
    <div className="legend">
      {items.map((s, i) => (
        <span key={i}>
          <i className="swatch" style={{ background: s.color }} />
          <span className="truncate">{s.label}</span>
        </span>
      ))}
    </div>
  );
}

// ---------------------------------------------------------------------------

export interface BarDatum {
  label: ReactNode;
  /** Sorting and width both use this. */
  value: number;
  /** What the number reads as — "412k", "1.2 s", "38%". */
  display?: ReactNode;
  color?: string;
  onClick?: () => void;
  sub?: ReactNode;
}

/**
 * A ranked horizontal bar list.
 *
 * Bars are drawn as a fraction of the LARGEST value, not of the total, because
 * the question is "how does this compare to the biggest one". A total-relative
 * bar makes every row in a long tail an invisible sliver.
 */
export function BarList({
  data,
  max,
  limit,
  emptyLabel = "Nothing recorded in this window",
}: {
  data: BarDatum[];
  max?: number;
  limit?: number;
  emptyLabel?: ReactNode;
}) {
  const shown = limit ? data.slice(0, limit) : data;
  const top = max ?? Math.max(1, ...data.map((d) => d.value));
  if (!shown.length) return <div className="t-caption">{emptyLabel}</div>;
  return (
    <div className="col" style={{ gap: "var(--space-2)" }}>
      {shown.map((d, i) => {
        const pct = top > 0 ? Math.max(1.5, (d.value / top) * 100) : 0;
        const Row = d.onClick ? "button" : "div";
        return (
          <Row
            key={i}
            className={cx("col", d.onClick && "clickable")}
            style={{
              gap: 3,
              textAlign: "left",
              cursor: d.onClick ? "pointer" : undefined,
              width: "100%",
            }}
            onClick={d.onClick}
          >
            <div className="row" style={{ gap: "var(--space-2)" }}>
              <span className="truncate t-cell" style={{ flex: 1 }}>
                {d.label}
              </span>
              <span className="t-cell t-num" style={{ color: "var(--text-secondary)" }}>
                {d.display ?? d.value.toLocaleString()}
              </span>
            </div>
            <div className="meter-track" style={{ height: 5 }}>
              <div
                style={{
                  height: "100%",
                  width: `${pct}%`,
                  background: d.color ?? vizColor(i),
                  borderRadius: "var(--r-full)",
                }}
              />
            </div>
            {d.sub && <span className="t-caption truncate">{d.sub}</span>}
          </Row>
        );
      })}
      {limit && data.length > limit && (
        <span className="t-caption">
          and {data.length - limit} more — the list is ranked, so this is the tail
        </span>
      )}
    </div>
  );
}

// ---------------------------------------------------------------------------

/** One whole split into parts. Never rendered without its legend. */
export function StackedBar({
  segments,
  height = 6,
}: {
  segments: { label: string; value: number; color?: string }[];
  height?: number;
}) {
  const total = segments.reduce((n, s) => n + s.value, 0);
  if (total <= 0) return <div className="stack" style={{ height }} />;
  return (
    <div className="stack" style={{ height }}>
      {segments.map((s, i) => (
        <span
          key={s.label}
          style={{ width: `${(s.value / total) * 100}%`, background: s.color ?? vizColor(i) }}
          title={`${s.label}: ${s.value.toLocaleString()}`}
        />
      ))}
    </div>
  );
}

// ---------------------------------------------------------------------------

export interface SeriesPoint {
  /** Bucket start, epoch ms. */
  t: number;
  v: number;
}

/**
 * A quantity over time.
 *
 * The x domain is the WINDOW, not the data: a series that only covers the last
 * ten minutes of a 24-hour window must be drawn in the last twentieth of the
 * chart, not stretched across it. Stretching is how a quiet company came to
 * look busy.
 */
export function TimeSeries({
  series,
  from,
  to,
  height = 120,
  label,
  format = (n) => n.toLocaleString(),
}: {
  series: { name: string; points: SeriesPoint[]; color?: string }[];
  from: number;
  to: number;
  height?: number;
  label?: ReactNode;
  format?: (n: number) => string;
}) {
  const id = useId();
  const W = 1000;
  const H = height;
  const padB = 18;
  const padT = 6;
  const span = Math.max(1, to - from);
  const peak = Math.max(1, ...series.flatMap((s) => s.points.map((p) => p.v)));
  const x = (t: number) => ((t - from) / span) * W;
  const y = (v: number) => padT + (1 - v / peak) * (H - padT - padB);

  return (
    <figure style={{ margin: 0 }}>
      <svg
        className="chart"
        viewBox={`0 0 ${W} ${H}`}
        preserveAspectRatio="none"
        style={{ height, width: "100%" }}
        role="img"
        aria-label={typeof label === "string" ? label : "time series"}
      >
        {[0.25, 0.5, 0.75].map((f) => (
          <line key={f} className="grid-line" x1={0} x2={W} y1={y(peak * f)} y2={y(peak * f)} />
        ))}
        <line className="axis-line" x1={0} x2={W} y1={H - padB} y2={H - padB} />
        {series.map((s, i) => {
          const color = s.color ?? vizColor(i);
          const pts = [...s.points].sort((a, b) => a.t - b.t);
          if (!pts.length) return null;
          const line = pts.map((p) => `${x(p.t).toFixed(2)},${y(p.v).toFixed(2)}`).join(" ");
          const area = `${x(pts[0]!.t).toFixed(2)},${H - padB} ${line} ${x(
            pts[pts.length - 1]!.t,
          ).toFixed(2)},${H - padB}`;
          return (
            <g key={s.name}>
              <defs>
                <linearGradient id={`${id}-${i}`} x1="0" y1="0" x2="0" y2="1">
                  <stop offset="0%" stopColor={color} stopOpacity="0.22" />
                  <stop offset="100%" stopColor={color} stopOpacity="0" />
                </linearGradient>
              </defs>
              <polygon points={area} fill={`url(#${id}-${i})`} />
              <polyline
                points={line}
                fill="none"
                stroke={color}
                strokeWidth={1.75}
                vectorEffect="non-scaling-stroke"
                strokeLinejoin="round"
                strokeLinecap="round"
              />
            </g>
          );
        })}
      </svg>
      <div className="row" style={{ justifyContent: "space-between" }}>
        <span className="t-caption">{new Date(from).toLocaleString()}</span>
        <span className="t-caption">peak {format(peak)}</span>
        <span className="t-caption">{new Date(to).toLocaleString()}</span>
      </div>
    </figure>
  );
}

// ---------------------------------------------------------------------------

/** A shape beside a number. Never on its own — it has no scale of its own. */
export function Sparkline({
  values,
  color = "var(--accent)",
  height = 28,
}: {
  values: number[];
  color?: string;
  height?: number;
}) {
  if (values.length < 2) return <div style={{ height }} />;
  const W = 200;
  const peak = Math.max(1, ...values);
  const step = W / (values.length - 1);
  const pts = values
    .map(
      (v, i) => `${(i * step).toFixed(2)},${(height - (v / peak) * (height - 2) - 1).toFixed(2)}`,
    )
    .join(" ");
  return (
    <svg
      className="spark"
      viewBox={`0 0 ${W} ${height}`}
      preserveAspectRatio="none"
      style={{ height }}
      aria-hidden="true"
    >
      <polyline
        points={pts}
        fill="none"
        stroke={color}
        strokeWidth={1.5}
        vectorEffect="non-scaling-stroke"
        strokeLinejoin="round"
        strokeLinecap="round"
      />
    </svg>
  );
}

/**
 * A fixed-width activity strip: one cell per bucket, oldest → newest.
 *
 * Keyed by BUCKET, never by index. The board this replaces keyed 60 cells
 * `p0..p59` over a window recomputed from the clock on every render, so at each
 * minute roll every cell's content shifted one position left and the whole
 * strip was rewritten. A time-anchored key moves exactly one node.
 */
export function ActivityStrip({
  buckets,
  color = "var(--accent)",
  height = 26,
  title,
}: {
  buckets: { t: number; v: number }[];
  color?: string;
  height?: number;
  title?: (b: { t: number; v: number }) => string;
}) {
  const peak = Math.max(1, ...buckets.map((b) => b.v));
  return (
    <div className="row" style={{ gap: 2, height, alignItems: "flex-end" }}>
      {buckets.map((b) => (
        <div
          key={b.t}
          title={title?.(b)}
          style={{
            flex: 1,
            minWidth: 2,
            height: b.v > 0 ? `${Math.max(12, (b.v / peak) * 100)}%` : "2px",
            background: b.v > 0 ? color : "var(--surface-inset)",
            borderRadius: 1,
            opacity: b.v > 0 ? 0.35 + 0.65 * (b.v / peak) : 1,
          }}
        />
      ))}
    </div>
  );
}
