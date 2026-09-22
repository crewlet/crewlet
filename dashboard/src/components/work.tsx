/**
 * The pieces every tracker surface draws.
 *
 * A board card, a list row, a calendar chip, a peek panel, an item page, My
 * work and a peek panel all render the same task, and before this
 * file existed each of them rendered it slightly differently: the board showed
 * a key and a title, the list showed six columns, My work showed four, and
 * only one of the three knew a task could be blocked. Every one of those is
 * the same object, so it gets one renderer and the screens choose the density.
 *
 * NONE OF THESE NEEDS A ROUTER. A card takes an `href` and an `onOpen`, so it
 * renders in a test — and in a peek panel — without a
 * routing context being the price of drawing one.
 */

import type { ReactNode } from "react";
import { Avatar, Callout, Card, Tag, cx } from "@crewlethq/ui";
import {
  ArrowUpwardGlyph,
  CalendarTodayGlyph,
  KeyboardArrowDownGlyph,
  KeyboardArrowUpGlyph,
  PersonGlyph,
  RemoveGlyph,
} from "@crewlethq/icons/glyphs";
// STILL OURS: a task TYPE's mark is named by the company's own type table as a
// value, and uilet's glyphs are components. The name -> drawing lookup stays in
// `~/ui/Icon.tsx`, which is where one change moves every caller at once.
import { Mark } from "~/ui/glyph.tsx";
import { uiletTone } from "~/ui/primitives.tsx";
import { rowPeekHandler } from "~/app/frame/DetailRail.tsx";
import { fmtDateCompact, fmtDateTime, relTime } from "~/lib/format.ts";
import { fmtMinutes, statusLabel, STATUS_TONE, typeIcon, typeName } from "~/lib/work.ts";
import type { WorkIncomplete, WorkStatusDef, WorkSummary, WorkTypeDef } from "~/protocol/index.ts";

export interface RowChrome {
  /** What a handle is called. The chart's, so a row shows a person's name. */
  seatName?: (handle: string) => string;
  types?: WorkTypeDef[];
  statuses?: WorkStatusDef[];
}

/**
 * The task type as its mark, with the company's own word for it on hover.
 *
 * `decorative` ONLY WHERE THE NAME IS PRINTED BESIDE IT — the rule [Assignee]
 * keeps for the avatar, and for the same reason: the mark always read its
 * name aloud, so a properties row that printed "Task" after it announced
 * "Task Task". A card, a row and a column cell draw the mark alone and keep
 * the hidden name; the two places that print the word say so.
 */
export function TypeIcon({
  type,
  types,
  decorative,
}: {
  type?: string;
  types?: WorkTypeDef[];
  decorative?: boolean;
}) {
  const name = typeName(type, types) || "Untyped";
  return (
    <span className="work-type" title={name} aria-hidden={decorative || undefined}>
      <Mark name={typeIcon(type)} size="sm" />
      {!decorative && <span className="sr-only">{name}</span>}
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
  // FOUR STEPS, FOUR SHAPES — the scale has to read without its colour, which
  // is what this mark is for. uilet vendors no double chevron, so the top step
  // takes the SOLID ARROW against high's chevron rather than two chevrons
  // against one: a different drawing, and still a different drawing per step,
  // which is the property that was load-bearing.
  const Glyph =
    priority === "urgent"
      ? ArrowUpwardGlyph
      : priority === "high"
        ? KeyboardArrowUpGlyph
        : priority === "low"
          ? KeyboardArrowDownGlyph
          : RemoveGlyph;
  return (
    <span className="work-prio" data-priority={priority} title={`${priority} priority`}>
      <Glyph size="xs" />
      {word ? <span>{priority}</span> : <span className="sr-only">{priority} priority</span>}
    </span>
  );
}

/** A status, in the project's own word and the tone its state carries. */
export function StatusBadge({ status, defs }: { status: string; defs?: WorkStatusDef[] }) {
  return (
    <Tag variant={uiletTone(STATUS_TONE[status] ?? "neutral")} dot>
      {statusLabel(status, defs)}
    </Tag>
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
        <PersonGlyph size="xs" />
        <span className="sr-only">Unassigned</span>
        {showName && <span className="muted">Unassigned</span>}
      </span>
    );
  }
  const label = seatName?.(handle) ?? handle;
  return (
    <span className="row gap-1" title={label}>
      {/* `decorative` ONLY WHERE THE NAME IS PRINTED BESIDE IT. Ours was
          `aria-hidden` either way and leant on a `title` on the wrapper, which
          a screen reader is free to ignore — so the four columns that draw the
          badge alone announced the assignee as nothing at all. */}
      <Avatar name={label} size={size} decorative={showName} />
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
      <CalendarTodayGlyph size="xs" />
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
 * The mark for a notice that ASKS something of its recipient.
 *
 * ONE MARK, because there were two. The engine writes ONE fact —
 * `tracker.Reason.Addressed`, a wake that obliges its seat to answer rather than
 * absorb — and two screens drew it: My work as a caution-toned "asks" beside a
 * neutral reason chip, the item's Woke tab as a TINT ON the reason chip. Two
 * marks for one fact is two facts to a reader, and folding it into the reason
 * meant "assignee that asked" and "assignee that informed" were the same word in
 * two grounds.
 *
 * AND THE GROUND IT WAS FOLDED INTO IS NOT AVAILABLE. The accent means where the
 * READER is — the active nav row, the primary button, the focus ring, the on
 * filter — and nothing else; every other fact is neutral and carried by its word
 * (docs/reference/dashboard-design.md, and uilet's own tone contract). Not a
 * preference: `variant="brand"` resolves to `--color-brand-accent-soft` under
 * `--color-brand-accent-ink`, which is the pair
 * `.crewlet-filter-chip[aria-pressed='true']` takes, so the Woke panel's summary
 * chip and one row's reason pill rendered the lavender ground of a switched-on
 * filter — in a panel whose other half IS a selection list.
 * `styles/tone.test.ts` is what keeps it there now.
 *
 * CAUTION RATHER THAN INFO. The tone has to mark the notice that WANTS
 * something, and `info` is the tone of the half that does not.
 */
export function AsksTag({ count }: { count?: number }) {
  const many = count !== undefined;
  return (
    <Tag
      variant="warning"
      title={
        many
          ? "asked something, rather than merely informed"
          : "this asks something of its recipient, rather than informing them"
      }
    >
      {/* ONE TEXT NODE, so a DOM query can match the pill's own label rather
          than a fragment of it. */}
      {many ? `${count} asked` : "asks"}
    </Tag>
  );
}

/**
 * Whether a card's foot has a single mark to draw.
 *
 * EVERY MARK DECIDES ITS OWN ABSENCE by rendering nothing — every mark but
 * one. [Assignee] draws a dashed "nobody" square instead, deliberately, and
 * that square's job is to hold a COLUMN OPEN on `.work-row`, which is a grid:
 * a cell that disappeared there would take its track with it and pull every
 * later column one place left. A card is inline flow and has no track to hold,
 * so on a task nobody has touched — no priority, no due date, no estimate, not
 * blocked, unassigned — the foot came out as that ghost and nothing else,
 * floated on its own under the title, reading as a control somebody could
 * press rather than as the absence of five facts.
 *
 * So the foot is drawn only when something goes in it, and the ghost stays on
 * rows. The predicate restates each mark's own emptiness rule from the same
 * fields, which is the one copy of it and the price of asking the question
 * BEFORE rendering: a container cannot ask a child that drew nothing whether
 * it did. A mark that gains a field is a mark that adds it here.
 */
function hasFootMarks(row: WorkSummary): boolean {
  return Boolean(row.blocked || row.due || row.points || row.estimate_min || row.assignee);
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
}: {
  row: WorkSummary;
  href: string;
  onOpen?: () => void;
  selected?: boolean;
  now: number;
  chrome?: RowChrome;
}) {
  return (
    <a
      className={cx("work-card", selected && "selected")}
      data-blocked={row.blocked ? "true" : undefined}
      href={href}
      onClick={rowPeekHandler(onOpen)}
    >
      <div className="work-card-top">
        <TypeIcon type={row.type} types={chrome.types} />
        <span className="work-key mono">{row.key}</span>
        <span className="spacer" />
        <PriorityMark priority={row.priority} />
      </div>
      <div className="work-card-title clamp">{row.title}</div>
      {hasFootMarks(row) && (
        <div className="work-card-foot">
          {row.blocked && (
            <Tag variant="danger" appearance="outline">
              Blocked
            </Tag>
          )}
          <DueMark due={row.due} overdue={row.overdue} now={now} />
          <SizeMark points={row.points} minutes={row.estimate_min} />
          <span className="spacer" />
          <Assignee handle={row.assignee} seatName={chrome.seatName} />
        </div>
      )}
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
  keyOf,
}: {
  row: WorkSummary;
  href: string;
  onOpen?: () => void;
  selected?: boolean;
  now: number;
  chrome?: RowChrome;
  /** A blocker's KEY from its id, where this list holds its row — see
   *  [blockedBy]. */
  keyOf?: (id: string) => string | undefined;
}) {
  return (
    <a
      className={cx("work-row", selected && "selected")}
      href={href}
      onClick={rowPeekHandler(onOpen)}
    >
      {/* EVERY CELL EXISTS EVEN WHEN ITS VALUE DOES NOT. The marks below each
          render nothing for an absent value — which is right on a CARD, where
          they sit in inline flow — but this row is a grid, and a child that
          disappears takes its track with it and pulls every column after it one
          place left. So the row owns the cells and the marks only decide what
          goes in them: an unestimated, undated, unassigned task still lines its
          status up with the task above it. */}
      <span className="work-cell work-cell-prio">
        <PriorityMark priority={row.priority} />
      </span>
      <span className="work-key mono">{row.key}</span>
      <span className="work-cell work-cell-status">
        <StatusBadge status={row.status} defs={chrome.statuses} />
      </span>
      <span className="work-row-title truncate">
        {row.title}
        {/* WHICH TASK HOLDS THIS ONE UP, not merely that something does. The
            row carries the edges, so the badge names the blocker a reader can
            go to — and it names the first of them rather than all, because a
            row is one line and a task waiting on four is still one fact. */}
        {row.blocked && (
          <Tag variant="danger" appearance="outline">
            {blockedBy(row, keyOf)}
          </Tag>
        )}
      </span>
      <span className="work-cell work-cell-due">
        <DueMark due={row.due} overdue={row.overdue} now={now} />
      </span>
      <span className="work-cell work-cell-type">
        <TypeIcon type={row.type} types={chrome.types} />
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

/**
 * What a blocked task is waiting on, in the room one line has.
 *
 * A bare "Blocked" made every blocked task look alike on a list whose whole
 * purpose is telling them apart, and the row already carries the edges
 * (`waiting_on`) — so this names how many hold it up, and WHICH where it can.
 *
 * AN EDGE CARRIES AN ID AND NOT A KEY, deliberately: it is drawn between two
 * rows on one page and `WorkSummary.id` is what they are matched on. So a
 * blocker the caller's own filter excluded is an id this list holds no row for,
 * and the honest rendering is the count rather than an invented key. That is
 * also why the resolver is the LIST's — only a list knows which rows it has.
 *
 * EXPORTED, because there are two lists now: this row, which every embedded
 * task list draws, and the work screen's grid, whose compact column set draws
 * the same badge in its title cell. One sentence rather than two, for the
 * reason every other mark in this file is shared — the second copy is the one
 * that stops saying `Blocked · ENG-4 +2` the day somebody changes this one.
 */
export function blockedBy(row: WorkSummary, keyOf?: (id: string) => string | undefined): string {
  // `open` IS THE FLAG THE EDGE CARRIES, and `false` is a settled fact rather
  // than a missing one — the blocker has finished. An edge that says nothing is
  // counted as open, because `blocked` on the row is exactly "some entry here
  // is open" and the two must not disagree.
  const held = (row.waiting_on ?? []).filter((edge) => edge.open !== false);
  if (held.length === 0) return "Blocked";
  const named = held.map((edge) => keyOf?.(edge.id)).find(Boolean);
  if (!named) return held.length > 1 ? `Blocked · ${held.length}` : "Blocked";
  return held.length > 1 ? `Blocked · ${named} +${held.length - 1}` : `Blocked · ${named}`;
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
  // THE LIST IS WHAT CAN RESOLVE AN EDGE. A dependency names the blocking
  // task's id, and the key a reader recognises is on that task's own row — so
  // only a component holding the rows can turn one into the other. Built once
  // per list rather than per row, because a lookup rebuilt inside the map is
  // quadratic over a page of a hundred.
  const keys = new Map(rows.map((row) => [row.id, row.key]));
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
          keyOf={(id) => keys.get(id)}
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
        // `title` IS the bold lead-in this banner hand-rolled with a `strong`,
        // and the `flex: 1` span goes with it: a Callout's content column
        // already owns that. The severity stays a WARNING rather than a
        // danger — the answer is usable, it just cannot account for
        // everything, which is precisely what the sentence says.
        <Callout
          variant="warning"
          title={`This answer is incomplete${
            answer.incomplete
              ? ` — ${answer.incomplete.records} record(s) this build cannot read`
              : ""
          }`}
        >
          Rows may be missing, rows that should have gone may still be here, and the counts were
          computed over what is shown.
          {answer.incomplete?.scope?.length
            ? ` Affected: ${answer.incomplete.scope.join(", ")}.`
            : ""}
          {answer.incomplete
            ? ` Record version ${answer.incomplete.version}, from sequence ${answer.incomplete.from.seq} — a build that can read it is what resolves this, not a refresh.`
            : ""}
        </Callout>
      )}
      <CoverageTags answer={answer} />
    </>
  );
}

/**
 * THE LEVELS A DASHBOARD ANSWER MAY BE SERVED AT AND SAY NOTHING ABOUT.
 *
 * `stale` is this surface's OWN default — `internal/statelog`'s
 * `ReadLevelDefaults` says so, and gives the reason: a screen polls, so a
 * barrier per poll would buy a freshness nobody reads. `session` and
 * `linearizable` are stronger still. None of the three is news.
 *
 * LISTED RATHER THAN EXCLUDED, so an unknown level is SHOWN. This list is a
 * second copy of a value the engine owns, and the day a fifth level arrives
 * the honest failure is a word a reader asks about — not a level quietly
 * rendered as the ordinary path.
 */
const ORDINARY_LEVELS = ["stale", "session", "linearizable"];

/** Whether a served level is one this surface did NOT expect. */
export function oddLevel(level: string | undefined): boolean {
  return !!level && !ORDINARY_LEVELS.includes(level);
}

/**
 * How fresh an answer is, and how far behind the node that served it —
 * ONE IMPLEMENTATION, because there were two.
 *
 * `StateBar` drew the page's answer as an `xs` outline chip under the page
 * bar; this component drew a panel's as a default-size one inside the screen's
 * own toolbar. Same fact, two sizes, two places — which on Inbox and My work,
 * both reading a page-level answer, put it in two different corners of the
 * chrome one click apart.
 *
 * AND THE ORDINARY PATH DRAWS NOTHING, which is the rule `ServedLevelBanner`
 * already states one screen over: a badge that always drew the level put the
 * word `stale` — this surface's own default — beside every healthy answer on
 * every screen in the product, bare, with no age and no measure of how far
 * behind. The one case that matters then arrives as a CHANGED WORD in a chip
 * nobody reads any more. So a level is drawn only when it is not one of the
 * three a dashboard expects, and the lag is drawn only when there is one.
 */
export function CoverageTags({ answer }: { answer?: CoverageFacts | null }) {
  if (!answer) return null;
  const behind =
    answer.applied_through !== undefined &&
    answer.log_seq !== undefined &&
    answer.applied_through < answer.log_seq;
  const level = answer.read_level ?? "";
  const odd = oddLevel(level);
  if (!odd && !behind) return null;
  return (
    <span className="work-coverage">
      {odd && (
        <Tag
          variant="warning"
          appearance="outline"
          size="xs"
          title={`This node could not measure its own distance from the log, so this answer is a coherent point in its order with no statement about age (read level ${level})`}
        >
          age unknown
        </Tag>
      )}
      {/* APPLIED_THROUGH BESIDE SEQ, so a node holding something it cannot
          apply is visible as its own state rather than as lag. NEUTRAL: a
          read level and an apply position are facts about the answer, not
          states of it, and lag alone is never an alarm. */}
      {behind && (
        <Tag appearance="outline" size="xs" title="This node holds records it has not applied yet">
          applied through {answer.applied_through} of {answer.log_seq}
        </Tag>
      )}
    </span>
  );
}

/**
 * A stack of tasks under a heading, as a card.
 *
 * THE SCREEN CHOOSES THE FRAME AND THIS IS ONE OF THEM. A seat's page stacks
 * several of these among other cards, so a heading and a count are what tell
 * one block from the next; My work draws the same rows under a TAB, where a
 * card inside a tab panel is a second boundary around a thing that already has
 * one. Both render [RowList], so a task looks the same wherever it appears.
 *
 * ABSENT WHEN IT HOLDS NOTHING, which is right for a stack: a page of seven
 * "nothing here" panels buries the one that has something. A tab cannot do
 * that — it would take its own name off the strip — which is why My work draws
 * an empty state instead of this.
 */
export function TaskBlock({
  title,
  hint,
  rows,
  now,
  chrome,
  hrefOf,
}: {
  title: string;
  hint?: string;
  rows: WorkSummary[];
  now: number;
  chrome?: RowChrome;
  /** Where a row goes. The screen owns the address. */
  hrefOf: (row: WorkSummary) => string;
}) {
  if (rows.length === 0) return null;
  return (
    <Card padding="none">
      <Card.Header subtitle={hint} count={rows.length}>
        <Card.Title>{title}</Card.Title>
      </Card.Header>
      <RowList rows={rows} now={now} chrome={chrome} hrefOf={hrefOf} />
    </Card>
  );
}
