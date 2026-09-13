/**
 * The pieces every tracker surface draws.
 *
 * A board card, a list row, a calendar chip, a peek panel, an item page, My
 * work and a sprint's own breakdown all render the same task, and before this
 * file existed each of them rendered it slightly differently: the board showed
 * a key and a title, the list showed six columns, My work showed four, and
 * only one of the three knew a task could be blocked. Every one of those is
 * the same object, so it gets one renderer and the screens choose the density.
 *
 * NONE OF THESE NEEDS A ROUTER. A card takes an `href` and an `onOpen`, so it
 * renders in a test — and in a peek panel, and in a sprint report — without a
 * routing context being the price of drawing one.
 */

import type { MouseEvent, ReactNode } from "react";
import { Avatar, Badge, cx, type Tone } from "~/ui/primitives.tsx";
import { Icon } from "~/ui/Icon.tsx";
import { fmtDateCompact, fmtDateTime, relTime } from "~/lib/format.ts";
import {
  fmtMinutes,
  PRIORITY_TONE,
  statusLabel,
  STATUS_TONE,
  typeIcon,
  typeName,
} from "~/lib/work.ts";
import type { WorkIncomplete, WorkStatusDef, WorkSummary, WorkTypeDef } from "~/protocol/index.ts";

export interface RowChrome {
  /** What a handle is called. The chart's, so a row shows a person's name. */
  seatName?: (handle: string) => string;
  types?: WorkTypeDef[];
  statuses?: WorkStatusDef[];
}

/** The task type as its mark, with the company's own word for it on hover. */
export function TypeIcon({ type, types }: { type?: string; types?: WorkTypeDef[] }) {
  const name = typeName(type, types) || "Untyped";
  return (
    <span className="work-type" title={name}>
      <Icon name={typeIcon(type)} size="sm" />
      <span className="sr-only">{name}</span>
    </span>
  );
}

/**
 * The priority, as a shape first.
 *
 * COLOUR IS NEVER THE ONLY CARRIER — the glyph differs per step, so the scale
 * reads under any vision and in a screenshot — and the two ends are the only
 * ones that take a status tone at all: `normal` and `low` are the ordinary
 * case, and tinting them would spend the caution and critical hues on the
 * fact that somebody filed a task.
 */
export function PriorityMark({ priority, word }: { priority?: string; word?: boolean }) {
  // NOTHING IS DRAWN FOR THE DEFAULT. `normal` is what a task gets when
  // nobody said, so it is most of the board, and a mark on every card is a
  // mark that says nothing — it buries the four cards that ARE urgent under
  // forty that are not. `word` is the one caller that still wants it: a
  // properties panel is answering "what is this set to", where "normal" is
  // the answer and a blank is a gap.
  if (!priority || priority === "none") return null;
  if (priority === "normal" && !word) return null;
  const glyph =
    priority === "urgent"
      ? "chevronsUp"
      : priority === "high"
        ? "chevronUp"
        : priority === "low"
          ? "chevronDown"
          : "minus";
  return (
    <span className="work-prio" data-priority={priority} title={`${priority} priority`}>
      <Icon name={glyph} size="xs" />
      {word ? <span>{priority}</span> : <span className="sr-only">{priority} priority</span>}
    </span>
  );
}

/** A status, in the project's own word and the tone its state carries. */
export function StatusBadge({ status, defs }: { status: string; defs?: WorkStatusDef[] }) {
  return (
    <Badge tone={(STATUS_TONE[status] ?? "neutral") as Tone} dot>
      {statusLabel(status, defs)}
    </Badge>
  );
}

/**
 * Who holds a task, or the fact that nobody does.
 *
 * UNASSIGNED IS A STATE AND THE ONE WORTH SEEING: an unassigned item routes to
 * the project's lead, and a project with no lead routes to nobody at all. It
 * is drawn as an empty dashed square rather than as a blank, so a board column
 * of unclaimed work reads as unclaimed rather than as a rendering that failed.
 */
export function Assignee({
  handle,
  seatName,
  size = "sm",
  name: showName,
}: {
  handle?: string;
  seatName?: (handle: string) => string;
  size?: "sm" | "md";
  name?: boolean;
}) {
  if (!handle) {
    return (
      <span className="work-nobody" title="Unassigned">
        <Icon name="user" size="xs" />
        <span className="sr-only">Unassigned</span>
        {showName && <span className="muted">Unassigned</span>}
      </span>
    );
  }
  const label = seatName?.(handle) ?? handle;
  return (
    <span className="row gap-1" title={label}>
      <Avatar name={label} size={size} />
      {showName && <span className="truncate">{label}</span>}
    </span>
  );
}

/**
 * A due date, and whether it has passed.
 *
 * THE ROW'S OWN `overdue` FLAG rather than a comparison of our own: the server
 * derives it against the company's day start, and a browser re-deriving it
 * from its own midnight is how one screen shows a task as overdue and another
 * does not.
 */
export function DueMark({ due, overdue, now }: { due?: string; overdue?: boolean; now: number }) {
  if (!due) return null;
  return (
    // THE FULL INSTANT IS ON THE TITLE, always. What is DRAWN is the
    // compact form, because a column of dates repeating one year on every
    // row is how the one date not in this year goes unnoticed.
    <span className={cx("work-due", overdue && "overdue")} title={fmtDateTime(due)}>
      <Icon name="calendar" size="xs" />
      {fmtDateCompact(due, now)}
      {overdue && <span className="sr-only">— overdue</span>}
    </span>
  );
}

/** The size a task carries, in whichever measure it was given. */
export function SizeMark({ points, minutes }: { points?: number; minutes?: number }) {
  if (points) return <span className="work-size">{points} pts</span>;
  if (minutes) return <span className="work-size">{fmtMinutes(minutes)}</span>;
  return null;
}

/**
 * Open a task without leaving the screen — unless the reader asked to.
 *
 * A CARD IS A REAL ANCHOR, so middle-click and ⌘-click open the item's own
 * page in a tab, which is what anybody with a tracker open expects. A plain
 * click is intercepted and opens the peek instead, because the board is the
 * place the reader is and sending them away from it to read one title is the
 * navigation every tracker learned not to make.
 */
function openHandler(onOpen?: () => void) {
  if (!onOpen) return undefined;
  return (event: MouseEvent) => {
    if (event.defaultPrevented) return;
    if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    if (event.button !== 0) return;
    event.preventDefault();
    onOpen();
  };
}

/**
 * One task as a board card.
 *
 * The three rows are fixed — identity, title, facts — so a column of cards
 * scans down its own columns rather than as a stack of paragraphs, and a card
 * that gains a due date does not reshuffle the two above it.
 */
export function BoardCard({
  row,
  href,
  onOpen,
  selected,
  now,
  chrome = {},
  showSprint,
}: {
  row: WorkSummary;
  href: string;
  onOpen?: () => void;
  selected?: boolean;
  now: number;
  chrome?: RowChrome;
  showSprint?: boolean;
}) {
  return (
    <a
      className={cx("work-card", selected && "selected")}
      data-blocked={row.blocked ? "true" : undefined}
      href={href}
      onClick={openHandler(onOpen)}
    >
      <div className="work-card-top">
        <TypeIcon type={row.type} types={chrome.types} />
        <span className="work-key mono">{row.key}</span>
        <span className="spacer" />
        <PriorityMark priority={row.priority} />
      </div>
      <div className="work-card-title">{row.title}</div>
      <div className="work-card-foot">
        {row.blocked && (
          <Badge tone="critical" outline>
            Blocked
          </Badge>
        )}
        <DueMark due={row.due} overdue={row.overdue} now={now} />
        <SizeMark points={row.points} minutes={row.estimate_min} />
        {showSprint && row.sprint !== undefined && <span className="work-size">S{row.sprint}</span>}
        <span className="spacer" />
        <Assignee handle={row.assignee} seatName={chrome.seatName} />
      </div>
    </a>
  );
}

/**
 * One task as a row.
 *
 * The compact shape, used wherever a task appears inside something else: a
 * subtask list, a person's day, a calendar day opened out. It is a GRID rather
 * than a flex row so a run of them lines up column by column — the same reason
 * the event feed is one.
 */
export function WorkRow({
  row,
  href,
  onOpen,
  selected,
  now,
  chrome = {},
}: {
  row: WorkSummary;
  href: string;
  onOpen?: () => void;
  selected?: boolean;
  now: number;
  chrome?: RowChrome;
}) {
  return (
    <a className={cx("work-row", selected && "selected")} href={href} onClick={openHandler(onOpen)}>
      <TypeIcon type={row.type} types={chrome.types} />
      <span className="work-key mono">{row.key}</span>
      <span className="work-row-title truncate">
        {row.title}
        {row.blocked && (
          <Badge tone="critical" outline>
            Blocked
          </Badge>
        )}
      </span>
      {/* EVERY CELL EXISTS EVEN WHEN ITS VALUE DOES NOT. The marks below
          each render nothing for an absent value — which is right on a
          CARD, where they sit in inline flow — but this row is a grid,
          and a child that disappears takes its track with it and pulls
          every column after it one place left. So the row owns the cells
          and the marks only decide what goes in them: an unestimated,
          undated, unassigned task still lines its status up with the
          task above it. */}
      <span className="work-cell work-cell-status">
        <StatusBadge status={row.status} defs={chrome.statuses} />
      </span>
      <span className="work-cell work-cell-prio">
        <PriorityMark priority={row.priority} />
      </span>
      <span className="work-cell work-cell-due">
        <DueMark due={row.due} overdue={row.overdue} now={now} />
      </span>
      <span className="work-cell work-cell-who">
        <Assignee handle={row.assignee} seatName={chrome.seatName} />
      </span>
      <span className="work-row-when" title={fmtDateTime(row.updated)}>
        {relTime(row.updated, now)}
      </span>
    </a>
  );
}

/** A stack of rows under a heading, absent when it holds nothing. */
export function RowList({
  rows,
  now,
  chrome,
  hrefOf,
  onOpen,
  selected,
}: {
  rows: WorkSummary[];
  now: number;
  chrome?: RowChrome;
  hrefOf: (row: WorkSummary) => string;
  onOpen?: (row: WorkSummary) => void;
  selected?: string;
}) {
  return (
    <div className="work-rows">
      {rows.map((row) => (
        <WorkRow
          key={row.id}
          row={row}
          now={now}
          chrome={chrome}
          href={hrefOf(row)}
          selected={selected === row.key}
          onOpen={onOpen ? () => onOpen(row) : undefined}
        />
      ))}
    </div>
  );
}

/** The coverage half every tracker answer carries. */
export interface CoverageFacts {
  read_level?: string;
  complete?: boolean;
  log_seq?: number;
  applied_through?: number;
  incomplete?: WorkIncomplete;
}

/**
 * How stale an answer may be, and what it could not account for.
 *
 * # Two different facts, and a screen that shows only one lies
 *
 * `read_level` says how FRESH the answer is. `complete` says whether it could
 * account for everything it was asked about: a node holding records this build
 * cannot decode has rows that may be missing, rows that should have left and
 * may still be present, and totals computed over the incomplete set.
 *
 * A screen that renders the freshness badge and swallows the coverage flag is
 * worse than a stale tile, because a person reads "a moment ago" and concludes
 * the board is right. So the incomplete line is a BANNER above the rows rather
 * than a badge beside them, it names the count and the scope, and it says the
 * remedy — which is a build that can read the records, not a refresh.
 */
export function Coverage({ answer }: { answer?: CoverageFacts | null }) {
  if (!answer) return null;
  const behind =
    answer.applied_through !== undefined &&
    answer.log_seq !== undefined &&
    answer.applied_through < answer.log_seq;

  return (
    <>
      {answer.complete === false && (
        <div className="banner caution">
          <Icon name="alert" size="sm" />
          <span style={{ flex: 1, minWidth: 0 }}>
            <strong>
              This answer is incomplete
              {answer.incomplete
                ? ` — ${answer.incomplete.records} record(s) this build cannot read`
                : ""}
            </strong>{" "}
            Rows may be missing, rows that should have gone may still be here, and the counts were
            computed over what is shown.
            {answer.incomplete?.scope?.length
              ? ` Affected: ${answer.incomplete.scope.join(", ")}.`
              : ""}
            {answer.incomplete
              ? ` Record version ${answer.incomplete.version}, from sequence ${answer.incomplete.from.seq} — a build that can read it is what resolves this, not a refresh.`
              : ""}
          </span>
        </div>
      )}
      {(answer.read_level || behind) && (
        <span className="work-coverage">
          {answer.read_level && (
            <Badge outline title="How fresh this answer is, as the engine actually served it">
              {answer.read_level}
            </Badge>
          )}
          {/* APPLIED_THROUGH BESIDE SEQ, so a node holding something it cannot
              apply is visible as its own state rather than as lag. */}
          {behind && (
            <Badge tone="caution" outline>
              applied through {answer.applied_through} of {answer.log_seq}
            </Badge>
          )}
        </span>
      )}
    </>
  );
}

/** A labelled fact, in the one shape the head and the properties panel share. */
export function Fact({
  label,
  children,
  icon,
}: {
  label: ReactNode;
  children: ReactNode;
  icon?: ReactNode;
}) {
  return (
    <div className="work-fact">
      <div className="work-fact-label">{label}</div>
      <div className="work-fact-value truncate">
        {icon}
        {children}
      </div>
    </div>
  );
}
