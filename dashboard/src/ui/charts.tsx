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
  /**
   * Where this bar GOES.
   *
   * A bar that navigates is a link, and a link has to be middle-clickable: a
   * button with an onClick opens nothing in a tab, which on a chart of
   * projects is the gesture a reader makes most. `onClick` remains for a bar
   * that changes the chart rather than leaving it.
   */
  href?: string;
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
        // A ZERO DRAWS NOTHING. The 1.5% floor is there so a value that
        // is merely tiny beside the largest still has a visible mark —
        // but applied to zero it draws the same sliver, and "nobody
        // delivered anything" then looks exactly like "somebody
        // delivered a little". The number beside the bar is what carries
        // a zero; the bar is what carries the proportion.
        const pct = top > 0 && d.value > 0 ? Math.max(1.5, (d.value / top) * 100) : 0;
        const Row = d.href ? "a" : d.onClick ? "button" : "div";
        const interactive = !!(d.href || d.onClick);
        return (
          <Row
            key={i}
            className={cx("col", interactive && "clickable")}
            style={{
              gap: 3,
              textAlign: "left",
              cursor: interactive ? "pointer" : undefined,
              width: "100%",
              color: "inherit",
              textDecoration: "none",
            }}
            href={d.href}
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
                className="bar-fill"
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
  if (total <= 0) return <div className="stackbar" style={{ height }} />;
  return (
    <div className="stackbar" style={{ height }}>
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
  series: {
    name: string;
    points: SeriesPoint[];
    color?: string;
    /**
     * Whether the line carries an area under it.
     *
     * DEFAULTS TO "ONLY WHEN THERE IS ONE SERIES", which is this kit's own
     * stated rule and which it could not express until now: three translucent
     * areas stacked on one chart blend into a fourth colour nobody chose, and
     * the reader cannot tell which of them is on top. One series keeps its
     * fill, because a lone line over a lot of white is a thinner claim than
     * the quantity deserves.
     */
    fill?: boolean;
    /**
     * A REFERENCE rather than a measurement — an ideal, a target, a cap.
     *
     * Drawn as a dash so it cannot be read as data: a straight solid line
     * among measured ones is indistinguishable from a series that happened to
     * be linear, which is the one thing a reference line must never look
     * like.
     */
    dashed?: boolean;
  }[];
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
          const filled = s.fill ?? series.length === 1;
          return (
            <g key={s.name}>
              {filled && (
                <>
                  <defs>
                    <linearGradient id={`${id}-${i}`} x1="0" y1="0" x2="0" y2="1">
                      <stop offset="0%" stopColor={color} stopOpacity="0.22" />
                      <stop offset="100%" stopColor={color} stopOpacity="0" />
                    </linearGradient>
                  </defs>
                  <polygon points={area} fill={`url(#${id}-${i})`} />
                </>
              )}
              <polyline
                points={line}
                fill="none"
                stroke={color}
                strokeWidth={1.75}
                strokeDasharray={s.dashed ? "6 5" : undefined}
                vectorEffect="non-scaling-stroke"
                strokeLinejoin="round"
                strokeLinecap="round"
              />
            </g>
          );
        })}
      </svg>
      <div className="row" style={{ justifyContent: "space-between" }}>
        <span className="t-caption">{formatEdge(from, span)}</span>
        <span className="t-caption">peak {format(peak)}</span>
        <span className="t-caption">{formatEdge(to, span)}</span>
      </div>
    </figure>
  );
}

/**
 * The two ends of the x axis, at the resolution the WINDOW has.
 *
 * A clock on a window of weeks is noise — "09:00:00" under both ends of a
 * fortnight says the same nothing twice — and a bare date on a window of
 * minutes cannot tell the two ends apart at all. So the span decides, at the
 * one boundary where the answer changes: a day.
 */
function formatEdge(at: number, span: number): string {
  const date = new Date(at);
  if (span >= 24 * 60 * 60 * 1000) {
    return date.toLocaleDateString(undefined, { month: "short", day: "numeric" });
  }
  return date.toLocaleString();
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

// ---------------------------------------------------------------------------

/**
 * A quantity over time, SPLIT INTO BANDS — one column per bucket.
 *
 * COLUMNS RATHER THAN STACKED AREAS, and this kit already says why one level
 * up: three translucent areas over one chart blend into a fourth colour nobody
 * chose, and the reader cannot tell which is on top. A column is opaque, its
 * segments abut, and the boundary between two bands is a line rather than a
 * mixture.
 *
 * The x domain is the WINDOW, and here that is simply the bucket list: the
 * engine returns every bucket in the range including the empty ones, so a
 * quiet hour is a gap of full height rather than a column the chart squeezed
 * out. Drawing from the data alone is what made a quiet company look busy.
 *
 * A GHOST is the same window one period earlier, drawn as a dashed mark AT ITS
 * OWN HEIGHT — never as a second set of bands, which would double the colours
 * for a comparison that only needs a height. Over the column rather than
 * behind it, so "the previous period was smaller" and "there is no previous
 * period" do not render identically.
 */
export function StackedTimeSeries({
  buckets,
  bands,
  ghost,
  height = 160,
  format = (n) => n.toLocaleString(),
  label,
}: {
  buckets: { at: string; total: number; parts: { key: string; value: number }[] }[];
  bands: { key: string; label: string; color: string }[];
  ghost?: number[];
  height?: number;
  format?: (n: number) => string;
  label?: (at: string) => string;
}) {
  const colors = new Map(bands.map((b) => [b.key, b.color]));
  // The peak spans the ghost too, so the two periods are drawn against ONE
  // scale. Scaled separately, a week that cost half as much would draw the
  // same height as the week it is being compared with.
  const peak = Math.max(1, ...buckets.map((b) => b.total), ...(ghost ?? []));
  return (
    <div className="stackseries" style={{ height }}>
      {buckets.map((b, i) => {
        const prior = ghost?.[i] ?? 0;
        return (
          <div className="stackseries-col" key={b.at}>
            {prior > 0 && (
              <div
                className="stackseries-ghost"
                style={{ height: `${(prior / peak) * 100}%` }}
                title={`previous period: ${format(prior)}`}
              />
            )}
            <div
              className="stackseries-stack"
              style={{ height: `${(b.total / peak) * 100}%` }}
              title={`${label?.(b.at) ?? b.at} — ${format(b.total)}`}
            >
              {b.parts.map((p) => (
                <span
                  key={p.key}
                  style={{
                    // Of the COLUMN, not of the peak: the column's own
                    // height already carries the magnitude, and scaling
                    // the segments again would leave a stack whose parts
                    // do not fill it.
                    height: `${(p.value / Math.max(1, b.total)) * 100}%`,
                    background: colors.get(p.key) ?? VIZ_OTHER,
                  }}
                />
              ))}
            </div>
          </div>
        );
      })}
    </div>
  );
}
