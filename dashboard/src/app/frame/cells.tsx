/**
 * The typed cells every grid in the product draws.
 *
 * ONE FUNCTION PER CELL TYPE, because the same value must look the same
 * wherever it appears: an absent number wears the same mark on the spend table
 * and on the sprint report, a date drops its year in both places or in
 * neither, and a seat is an avatar and a name rather than a handle on one
 * screen and a display name on the next.
 *
 * Nothing here fetches, and nothing here decides what a value MEANS — a status
 * glyph's tone comes from the status vocabulary, and these render what they
 * are handed.
 *
 * # Nothing here restates a format either
 *
 * `lib/format.ts` owns how a number, a duration and an instant are SPELLED,
 * and these compose it. They did not: `DurationCell` wrote "500ms" where
 * `fmtDuration` writes "500 ms", and `TokenCell` abbreviated five thousand as
 * "5.0k" where `fmtCount` writes "5,000". Two rules for one value is the
 * shape `textcut` was created to end on the engine side, and here it was
 * worse than a drift — it was a TRAP: adopting a cell would have silently
 * changed every figure in the column, so the module built to make the product
 * consistent could not be adopted without making it inconsistent.
 *
 * What a cell adds over calling the formatter directly is the part a
 * formatter cannot have: an ABSENT value renders as [EmptyValue], which says
 * what kind of absence it is, and the value wears the tabular face so a column
 * of them lines up.
 *
 * # And the mark is the design system's, not one of our own
 *
 * There was a local `Dash` here: an em dash in a `.cell-dash` span with a
 * `title`. `@crewlethq/ui` ships [EmptyValue] for exactly this and draws an EN
 * dash — its own doc says the em dash is a mark "this design system does not
 * use anywhere" — so the trash screen showed both at once, a 12px em dash in
 * the table's DUE column beside a 6px en dash in the activity feed's object
 * column, two glyphs for one fact in one viewport. Newer screens (People,
 * Company, LiveNow, Trace, the org builder's table) had already adopted
 * [EmptyValue]; the migration simply stopped halfway and nothing said so.
 *
 * The `title` went with it, and that is a gain rather than a cost: a `title`
 * on a `<span>` is a hover tooltip no keyboard reaches and no screen reader is
 * required to announce, so on the surface where the distinction actually
 * matters — a column where absent and zero are different facts — the
 * distinction was reachable only with a mouse. [EmptyValue]'s `label` is read
 * in place of the dash.
 */

import type { ReactNode } from "react";
import { href } from "../router.tsx";
import { Avatar, cx, EmptyValue, Tag } from "@crewlethq/ui";
// A `TextCell`'s mark is named by whichever screen draws the column, so the
// name→drawing lookup stays in `~/ui/Icon.tsx` — one change there moves every
// caller onto uilet's glyphs at once.
import { Mark, type MarkName } from "~/ui/glyph.tsx";
import { type Tone } from "~/ui/primitives.tsx";
import { fmtCount, fmtDateTime, fmtDuration, relTime } from "~/lib/format.ts";

/** An identifier — a key, a handle, an id. Monospaced, and usually a link. */
export function KeyCell({ value, path }: { value: string; path?: string[] }) {
  if (!value) return <EmptyValue label="Not set" />;
  return path ? (
    <a className="mono t-link" href={href(path)}>
      {value}
    </a>
  ) : (
    <span className="mono">{value}</span>
  );
}

/**
 * A number, tabular, a marked absence when there is none.
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
  if (value == null) return <EmptyValue label="Nothing recorded" />;
  return (
    <span className="t-num" title={title}>
      {fmtCount(value)}
      {suffix}
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
  if (!at) return <EmptyValue label="Never" />;
  return (
    <span title={fmtDateTime(at)} className="cell-date">
      {relTime(at, now)}
    </span>
  );
}

/**
 * A seat: the identity badge and the name, linking to the seat's page.
 *
 * ONE BADGE FOR ONE SEAT. This drew its own `.seat-mark` — a 22px circle
 * holding a robot or a person glyph — while the board, the list, the roster
 * and every chip drew `@crewlethq/ui`'s `Avatar`, a rounded square of
 * initials. So the same engineer was "FE" on the board and an identical
 * generic robot on Search and on a goal's Owners panel, and a reader scanning
 * two surfaces for one person had nothing to scan FOR: every agent's mark was
 * the same drawing.
 *
 * The distinction the local mark carried is not lost, because the design
 * system carries it: `dashed` is documented there as "a HUMAN seat: the engine
 * does not run it", which is precisely the structural fact the dashed ring
 * meant here. What IS lost is a picture of a robot, and that was the half
 * saying nothing — it drew the KIND, which one glance at the roster gives, in
 * the slot that should have been saying WHO.
 *
 * `decorative`, because the name is printed immediately beside the badge:
 * without it the row reads "Ada Lovelace avatar, Ada Lovelace".
 */
export function SeatCell({
  handle,
  name,
  kind,
}: {
  handle?: string | null;
  name?: string;
  kind?: "agent" | "human" | string;
}) {
  if (!handle) return <EmptyValue label="Nobody" />;
  return (
    <a className="cell-seat" href={href(["company", "people", handle])} title={`@${handle}`}>
      <Avatar
        name={name || handle}
        size="xs"
        variant={kind === "human" ? "dashed" : "solid"}
        decorative
      />
      <span className="truncate">{name || handle}</span>
    </a>
  );
}

/**
 * A state glyph with its word — never colour alone.
 *
 * OURS, BECAUSE THE GLYPH IS THE CALLER'S. `StatusDot` draws one filled 6px
 * mark per tone, and half of what this column says is the HOLLOW counterpart:
 * `●` against `○` is how Fleet says "running the active revision" against "no
 * apply reported", and how Retention says whether the trim still waits for a
 * node. There is no unfilled state in the tone set and no way to hand one in,
 * so a port would have had to spend a second colour on the negative case —
 * which is colour carrying identity, the one thing the tone rule forbids.
 *
 * Nor is it a `Tag`: a tag is a pill, and this is a mark beside prose inside a
 * table cell that already has a column heading naming it.
 */
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
  if (ms == null) return <EmptyValue label="Not measured" />;
  return <span className="t-num">{fmtDuration(ms)}</span>;
}

/** Tokens, abbreviated the way `fmtCount` abbreviates every other count. */
export function TokenCell({ value }: { value?: number | null }) {
  if (value == null) return <EmptyValue label="Nothing recorded" />;
  return <span className="t-num">{fmtCount(value)}</span>;
}

/** Tags, capped with a count rather than wrapping a row to three lines. */
export function TagsCell({ tags, max = 3 }: { tags?: string[] | null; max?: number }) {
  const list = tags ?? [];
  if (list.length === 0) return <EmptyValue label="No tags" />;
  const shown = list.slice(0, max);
  const rest = list.length - shown.length;
  return (
    <span className="row gap-1 wrap">
      {/* NEUTRAL AND OUTLINE: a tag names a thing, and uilet's tone doc draws
          the same line ours does — identity takes no colour. `outline` is
          their `appearance`, which is what our `outline` prop was. */}
      {shown.map((tag) => (
        <Tag key={tag} appearance="outline" size="xs">
          {tag}
        </Tag>
      ))}
      {rest > 0 && (
        <span className="t-caption" title={list.slice(max).join(", ")}>
          +{rest}
        </span>
      )}
    </span>
  );
}

/**
 * A small proportion bar, for a cell that is a fraction of something.
 *
 * OURS, BECAUSE A COLUMN OF BARS HAS TO LINE UP. This is a fixed 64px bar
 * drawn inline inside a grid cell, and uilet's `Meter` is a block flex column
 * that takes the width it is given: dropped into the Budgets column it would
 * be 120px on that screen and something else on the next, so two bars at the
 * same fraction would be different lengths and the column would stop being
 * readable at a glance. `Meter` publishes no width, no intrinsic size and no
 * inline form — its `compact` size only drops the legend a type step, and the
 * legend is hidden here anyway.
 *
 * What their Meter has that ours does not is `role="meter"` with
 * `aria-valuenow`/`valuemin`/`valuemax`, where ours is a `role="img"` named by
 * its label. That is worth having and is the thing to take from it if `Meter`
 * ever grows a fixed-width form.
 */
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
  if (!(max > 0)) return <EmptyValue label="Nothing to measure against" />;
  const pct = Math.max(0, Math.min(100, (used / max) * 100));
  return (
    <span className="cell-meter" title={label} role="img" aria-label={label}>
      <span className={cx("cell-meter-fill", tone)} style={{ width: `${pct}%` }} />
    </span>
  );
}

/**
 * Plain text with a leading mark, truncated.
 *
 * TWO SPELLINGS OF ONE SLOT, because a COLUMN's mark and a ROW's mark are
 * different facts. `icon` is a NAME, and it is right when every row of the
 * column wears the same drawing — the seat column's `memory`, a node's `dns`, a
 * page's `description`. There the header carries the meaning and the mark is
 * decoration, which is exactly what `Mark` renders it as: `aria-hidden`, with no
 * word of its own.
 *
 * `mark` is a rendered node, for the case a name cannot serve: a drawing that
 * VARIES PER ROW is the only place that row states the fact, so it has to bring
 * its own hover word and its own accessible name with it. The tracker's
 * `TypeIcon` is what forced it — a bug, an epic and a spike are three drawings
 * AND three words, none of them in the header — and the screen that could not
 * reach it drew a hardcoded tick on every ranked hit instead, which reads as
 * "done" one column from a status saying otherwise.
 *
 * Exactly one of the two, enforced by the type rather than by a rule: a cell
 * given both would draw the column's answer over the row's.
 */
export function TextCell({
  children,
  icon,
  mark,
}: { children: ReactNode } & (
  { icon?: MarkName; mark?: never } | { icon?: never; mark: ReactNode }
)) {
  return (
    <span className="row" style={{ gap: 6, minWidth: 0 }}>
      {mark ?? (icon && <Mark name={icon} size="xs" />)}
      <span className="truncate">{children}</span>
    </span>
  );
}
