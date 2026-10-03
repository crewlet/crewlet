/**
 * The typed cells every grid in the product draws.
 *
 * ONE FUNCTION PER CELL TYPE, because the same value must look the same
 * wherever it appears: an absent number wears the same mark on the spend table
 * and on the activity feed, a date drops its year in both places or in
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
import { cx, EmptyValue, Tag } from "@crewlethq/ui";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
// A `TextCell`'s mark is named by whichever screen draws the column, so the
// name→drawing lookup is `~/ui/glyph.tsx`'s — one change there moves every
// caller at once.
import type { GlyphName } from "@crewlethq/icons/glyphs";
import { Mark } from "~/ui/glyph.tsx";
import { type Tone } from "~/ui/primitives.tsx";
import { useClockReading } from "~/lib/clock.ts";
import { fmtCount, fmtDateTime, fmtDuration, relTime } from "~/lib/format.ts";
import { handleLabel, type SeatKind } from "~/lib/seats.ts";
import { workItemLabel } from "~/lib/turns.ts";
import type { WorkItemRef } from "~/protocol/index.ts";

/**
 * An identifier — a key, a handle, an id. Monospaced, and usually a link.
 *
 * ONE LINE, cut with its whole value on the title. An identifier is one token
 * and carries no space to break at, so a long one — a conversation key is
 * `work:task:` and a uuid — wrapped at whatever character the column ran out
 * on, stood the row two lines tall and printed a key nobody could search for.
 *
 * `text` is what the row PRINTS where that is shorter than the value — a
 * conversation key with its uuid cut to a head (`conversationLabel`) — and the
 * value itself is still the title, so nothing is lost to the cut.
 */
export function KeyCell({ value, path, text }: { value: string; path?: string[]; text?: string }) {
  if (!value) return <EmptyValue label="Not set" />;
  return path ? (
    <a className="mono t-link key-cell" href={href(path)} title={value}>
      {text ?? value}
    </a>
  ) : (
    <span className="mono key-cell" title={value}>
      {text ?? value}
    </span>
  );
}

/**
 * What a turn did, and the work item it was on: the "What it did" cell of
 * every list of turns.
 *
 * THE SUMMARY IS WHAT KEEPS ITS WIDTH. The key beside it is one token and must
 * not wrap, but it is capped (`.turn-what-key`) rather than never shrinking:
 * an uncapped key took the whole cell whenever it was long, and on a phone's
 * stacked row the summary beside it was laid out one letter per line — a
 * single row 31,000px tall. Each half is cut on one line with its whole text
 * on its title.
 */
export function TurnWhatCell({
  summary,
  item,
  doing,
}: {
  summary?: string | undefined;
  item?: WorkItemRef | null | undefined;
  /** What the turn is doing now, for one still running (`runningNow`): said
   *  where a settled turn's summary goes, since a running turn has none yet. */
  doing?: string | undefined;
}) {
  const label = item ? workItemLabel(item) : null;
  return (
    <span className="turn-what">
      {/* A CLAMP, ONE LINE WIDE, rather than a nowrap cut: it is the same
          ellipsis on a table row, and it lets a phone's stacked card give the
          summary a second line (`frame.css`) without a second rule. */}
      <span className="turn-what-summary clamp" title={summary || doing || undefined}>
        {summary || (doing ? doing : <span className="muted">no summary recorded</span>)}
      </span>
      {label && (
        <span className="mono t-caption item-key turn-what-key" title={label.title}>
          {label.text}
        </span>
      )}
    </span>
  );
}

/**
 * A turn's figure that a turn still running does not have yet — its iteration
 * count, its tokens, how long it took. Each is written as the turn completes,
 * and a list that drew the row's zeros said a running turn had run no rounds
 * and spent nothing.
 */
export function UnsettledCell() {
  return <EmptyValue label="Not settled — the turn has not ended" />;
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
 *
 * IT READS THE CLOCK ITSELF, and takes no `now`. A `now` handed in from the
 * screen made every column that drew one a function of the clock, so the
 * screen rebuilt its columns once a second and the grid rendered every row
 * again — the audit's hundred rows, every second, to move the few that read
 * "12s ago". Subscribed here, a tick reaches this span alone, and only when
 * its words change.
 */
export function DateCell({ at }: { at?: string | null }) {
  const text = useClockReading((now) => relTime(at, now));
  if (!at) return <EmptyValue label="Never" />;
  return (
    <span title={fmtDateTime(at)} className="cell-date">
      {text}
    </span>
  );
}

/**
 * Any time-relative words a cell shows, read off the clock by the cell itself.
 *
 * [DateCell]'s reason, for the readings that are not "how long ago": "in 4m"
 * to a schedule's next run, a due date that drops its year when it is this
 * year's. The words are the caller's; that the clock reaches them here rather
 * than through the column above is this component's.
 */
export function ClockText({ read }: { read: (now: number) => string }) {
  return <>{useClockReading(read)}</>;
}

/**
 * A seat: the identity badge and the name, linking to the seat's page.
 *
 * ONE BADGE FOR ONE SEAT. This drew its own `.seat-mark` — a 22px circle
 * holding a robot or a person glyph — while the board, the list, the roster
 * and every chip drew `@crewlethq/ui`'s `Avatar`, a rounded square of
 * initials. So the same engineer was "FE" on the board and an identical
 * generic robot on Search and on a project's Lead panel, and a reader scanning
 * two surfaces for one person had nothing to scan FOR: every agent's mark was
 * the same drawing.
 *
 * The distinction the local mark carried is not lost, because the design
 * system carries it: the badge's `kind` draws a person as a circle and an
 * agent as a squircle, which is precisely the structural fact the old mark
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
  title,
}: {
  handle?: string | null;
  name?: string;
  /**
   * What the link's tooltip says, where the handle is not the whole story —
   * an operator's write names the person AND the token they wrote through.
   */
  title?: string;
  /**
   * The seat's kind, which decides the badge's one variant.
   *
   * THE NAMED TYPE, because this said `"agent" | "human" | string` — a union
   * that collapses to `string`, so it accepted any word and a caller passing
   * an author kind the chart never mints type-checked and drew a solid disc
   * for ever. `undefined` is a handle the chart does not hold; the kit's
   * badge has no third outline, so it takes the kit's default. A screen that
   * holds its writers' recorded kinds resolves them first (`kindWithAuthors`
   * in lib/seats.ts), which is what draws an operator as the person they are.
   */
  kind?: SeatKind;
}) {
  if (!handle) return <EmptyValue label="Nobody" />;
  return (
    <a
      className="cell-seat"
      href={href(["agents", "seats", handle])}
      title={title ?? handleLabel(handle)}
    >
      <SeatAvatar
        name={name || handle}
        size="xs"
        kind={kind === "human" ? "human" : "agent"}
        decorative
      />
      <span className="truncate">{name || handle}</span>
    </a>
  );
}

/**
 * A seat named inside a row that is ALREADY a link: its badge and its name,
 * and no anchor of its own.
 *
 * [SeatCell] is a link to the seat's page, and a grid whose rows are links
 * (the turns list, a fleet's leases, the spend tables) cannot nest one — an
 * anchor inside an anchor is markup no browser agrees about. Those columns
 * drew `TextCell icon="cpu"` instead: a chip glyph where every other surface
 * identifies a seat by its badge, so the same seat was a squircle on Work and
 * a processor on Activity. This is the badge, without the link.
 */
export function SeatLabel({ name, kind }: { name: string; kind?: SeatKind }) {
  return (
    <span className="cell-seat" title={name}>
      <SeatAvatar name={name} size="xs" kind={kind === "human" ? "human" : "agent"} decorative />
      <span className="truncate">{name}</span>
    </span>
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
  { icon?: GlyphName; mark?: never } | { icon?: never; mark: ReactNode }
)) {
  return (
    <span className="row" style={{ gap: 6, minWidth: 0 }}>
      {mark ?? (icon && <Mark name={icon} size="xs" />)}
      <span className="truncate">{children}</span>
    </span>
  );
}
