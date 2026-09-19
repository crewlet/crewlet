/**
 * The two charts uilet has no shape for, and the phase ramp.
 *
 * Hand-rolled SVG rather than a charting library, for one reason that decides
 * it: every mark here has to read tokens, in two themes, at the same contrast
 * floors the palette suite measures. A library's theming surface is a second
 * design system to keep in step with this one.
 *
 * WHAT IS LEFT, AND WHY IT IS STILL OURS. `Charts` covers the ranked list, the
 * legend, the single stacked bar, the activity strip, the sparkline and a line
 * series, and every screen draws those from the package now. Three things had
 * no peer there:
 *
 *  - `TimeSeries`  — a quantity over time where a series can be a REFERENCE
 *                    rather than a measurement. Theirs fills every series and
 *                    has no dashed stroke, so a reference line would arrive as
 *                    a third solid line under a third translucent wash.
 *  - `StackedTimeSeries`
 *                  — a column per bucket, SPLIT INTO BANDS, with the previous
 *                    period drawn over it as a ghost. Theirs is a line series
 *                    or one stacked bar; this is neither.
 *  - `phaseColor`  — uilet publishes `--color-phase-*` as tokens and no
 *                    function that picks one from a phase name. Its data ramp
 *                    is deliberately NOT that: a chart hue means "this
 *                    series", never "execute".
 *
 * THE DATA RAMP IS uilet's. `dataColor` and `DATA_COLOR_OTHER` are the same
 * five-plus-residual vocabulary this file used to spell as `--viz-*`, measured
 * by the package's own palette suite, and a colour named in two places is a
 * colour that drifts.
 */

import { useId, type ReactNode } from "react";
import { DATA_COLOR_OTHER, dataColor } from "@crewlethq/ui";

/**
 * Phase is its own fixed identity, not a slot on the data ramp.
 *
 * Three phases, three tokens, and the ramp is deliberately not consulted: a
 * data hue says "this series" and would mean a different phase on every chart
 * that happened to order its bands differently.
 *
 * ANYTHING ELSE IS THE RESIDUAL, and it is uilet's residual rather than a
 * colour of our own — a phase this build has never heard of is exactly "the
 * rest", which is the one thing that neutral is for. Returning a token we no
 * longer publish would draw it in nothing at all.
 */
export function phaseColor(phase: string): string {
  switch ((phase || "").toLowerCase()) {
    case "onboarding":
      return "var(--phase-onboarding)";
    case "execute":
      return "var(--phase-execute)";
    case "review":
      return "var(--phase-review)";
    default:
      return DATA_COLOR_OTHER;
  }
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
          // A SERIES WITH NO COLOUR OF ITS OWN TAKES ITS PLACE ON THE RAMP,
          // and the ramp is uilet's. Left pointing at a token this product no
          // longer publishes, `stroke` would resolve to nothing and the line
          // would be drawn in transparent — a series that is present, counted
          // in the peak, and invisible.
          const color = s.color ?? dataColor(i);
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
                    // A BAND THE LEGEND DOES NOT NAME IS THE RESIDUAL, in
                    // the residual's own neutral. It must never fall through
                    // to nothing: a transparent segment still takes its share
                    // of the column, so the stack would be short by exactly
                    // the amount nobody can see.
                    background: colors.get(p.key) ?? DATA_COLOR_OTHER,
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
