/**
 * The typed cells every grid in the product draws.
 *
 * ONE FUNCTION PER CELL TYPE, because the same value must look the same
 * wherever it appears: an absent number is an em dash on the spend table and
 * on the sprint report, a date drops its year in both places or in neither,
 * and a seat is an avatar and a name rather than a handle on one screen and a
 * display name on the next.
 *
 * Nothing here fetches, and nothing here decides what a value MEANS — a status
 * glyph's tone comes from the status vocabulary, and these render what they
 * are handed.
 */

import type { ReactNode } from "react";
import { href } from "../router.tsx";
import { Icon, type IconName } from "~/ui/Icon.tsx";
import { Badge, cx, type Tone } from "~/ui/primitives.tsx";
import { fmtDateTime, relTime } from "~/lib/format.ts";

/** An identifier — a key, a handle, an id. Monospaced, and usually a link. */
export function KeyCell({ value, path }: { value: string; path?: string[] }) {
  if (!value) return <Dash />;
  return path ? (
    <a className="mono t-link" href={href(path)}>
      {value}
    </a>
  ) : (
    <span className="mono">{value}</span>
  );
}

/**
 * A number, tabular, em dash when absent.
 *
 * ABSENT IS NOT ZERO, and this is the whole reason the cell exists: "nothing
 * is estimated" and "everything is estimated at nothing" are different facts,
 * and a grid that renders both as `0` makes the first invisible.
 */
export function NumberCell({
  value,
  suffix,
  title,
}: {
  value: number | null | undefined;
  suffix?: string;
  title?: string;
}) {
  if (value == null) return <Dash title="nothing recorded" />;
  return (
    <span className="t-num" title={title}>
      {value.toLocaleString()}
      {suffix}
    </span>
  );
}

/** An em dash that says what it means when hovered. */
export function Dash({ title = "not set" }: { title?: string }) {
  return (
    <span className="cell-dash" title={title}>
      —
    </span>
  );
}

/**
 * A date.
 *
 * Absolute in the title, relative in the cell: a grid scanned for "what moved
 * today" is read in relative time, and the exact instant is what somebody
 * needs once they have found the row.
 */
export function DateCell({ at, now }: { at?: string | null; now: number }) {
  if (!at) return <Dash title="never" />;
  return (
    <span title={fmtDateTime(at)} className="cell-date">
      {relTime(at, now)}
    </span>
  );
}

/** A seat: the avatar mark and the name, linking to the seat's page. */
export function SeatCell({
  handle,
  name,
  kind,
}: {
  handle?: string | null;
  name?: string;
  kind?: "agent" | "human" | string;
}) {
  if (!handle) return <Dash title="nobody" />;
  return (
    <a className="cell-seat" href={href(["company", "people", handle])} title={`@${handle}`}>
      <span className={cx("seat-mark", kind === "human" && "human")} aria-hidden="true">
        <Icon name={kind === "human" ? "user" : "cpu"} size="xs" />
      </span>
      <span className="truncate">{name || handle}</span>
    </a>
  );
}

/** A state glyph with its word — never colour alone. */
export function StatusCell({
  glyph,
  label,
  tone,
  title,
}: {
  glyph: string;
  label: string;
  tone: Tone;
  title?: string;
}) {
  return (
    <span className={cx("cell-status", tone)} title={title ?? label}>
      <span className="cell-glyph" aria-hidden="true">
        {glyph}
      </span>
      <span className="truncate">{label}</span>
    </span>
  );
}

/** A duration in milliseconds, rendered at the coarsest honest unit. */
export function DurationCell({ ms }: { ms?: number | null }) {
  if (ms == null) return <Dash title="not measured" />;
  if (ms < 1_000) return <span className="t-num">{Math.round(ms)}ms</span>;
  if (ms < 60_000) return <span className="t-num">{(ms / 1_000).toFixed(1)}s</span>;
  const minutes = Math.floor(ms / 60_000);
  const seconds = Math.round((ms % 60_000) / 1_000);
  if (minutes < 60) return <span className="t-num">{`${minutes}m ${seconds}s`}</span>;
  return <span className="t-num">{`${Math.floor(minutes / 60)}h ${minutes % 60}m`}</span>;
}

/** Tokens, abbreviated the way a spend table is read. */
export function TokenCell({ value }: { value?: number | null }) {
  if (value == null) return <Dash title="nothing recorded" />;
  if (value < 1_000) return <span className="t-num">{value}</span>;
  if (value < 1_000_000) return <span className="t-num">{(value / 1_000).toFixed(1)}k</span>;
  return <span className="t-num">{(value / 1_000_000).toFixed(2)}M</span>;
}

/** Tags, capped with a count rather than wrapping a row to three lines. */
export function TagsCell({ tags, max = 3 }: { tags?: string[] | null; max?: number }) {
  const list = tags ?? [];
  if (list.length === 0) return <Dash title="no tags" />;
  const shown = list.slice(0, max);
  const rest = list.length - shown.length;
  return (
    <span className="row gap-1 wrap">
      {shown.map((tag) => (
        <Badge key={tag} outline>
          {tag}
        </Badge>
      ))}
      {rest > 0 && (
        <span className="t-caption" title={list.slice(max).join(", ")}>
          +{rest}
        </span>
      )}
    </span>
  );
}

/** A small proportion bar, for a cell that is a fraction of something. */
export function MeterCell({
  used,
  max,
  label,
  tone = "accent",
}: {
  used: number;
  max: number;
  label: string;
  tone?: Tone;
}) {
  if (!(max > 0)) return <Dash title="nothing to measure against" />;
  const pct = Math.max(0, Math.min(100, (used / max) * 100));
  return (
    <span className="cell-meter" title={label} role="img" aria-label={label}>
      <span className={cx("cell-meter-fill", tone)} style={{ width: `${pct}%` }} />
    </span>
  );
}

/** Plain text with an icon, truncated. */
export function TextCell({ children, icon }: { children: ReactNode; icon?: IconName }) {
  return (
    <span className="row" style={{ gap: 6, minWidth: 0 }}>
      {icon && <Icon name={icon} size="xs" />}
      <span className="truncate">{children}</span>
    </span>
  );
}
