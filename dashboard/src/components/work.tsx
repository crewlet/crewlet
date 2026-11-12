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

import { useLayoutEffect, useRef, useState, type HTMLAttributes } from "react";
import { Callout, Tag, cx, peekClickHandler } from "@crewlethq/ui";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import { CalendarGlyph, CircleAlertGlyph, UserGlyph } from "@crewlethq/icons/glyphs";
// STILL OURS: a task TYPE's mark is named by the company's own type table as a
// value, and uilet's glyphs are components. The name -> drawing lookup stays in
// `~/ui/Icon.tsx`, which is where one change moves every caller at once.
import { NamedGlyph } from "@crewlethq/icons/glyphs/registry";
import { uiletTone } from "~/ui/primitives.tsx";
import { fmtCount, fmtDateCompact, fmtDateTime, fmtExact, humanize, plural } from "~/lib/format.ts";
import { useClock } from "~/lib/clock.ts";
import { shortAge } from "~/lib/seats.ts";
import { GENERATION_STRIDE } from "~/contract/positions.ts";
// TYPE ONLY: these pieces render with no provider above them, so the chart
// reaches them as resolvers on the chrome and never as a module they import.
import type { AgentAvatar } from "~/lib/avatar.ts";
import type { AvatarStatus } from "@crewlethq/ui";
import type { CardLive, SeatKind } from "~/lib/seats.ts";
import {
  DEFAULT_TYPE,
  fmtMinutes,
  statusLabel,
  statusShape,
  STATUS_TONE,
  typeIcon,
  typeName,
  readsBlocked,
} from "~/lib/work.ts";
import type { WorkIncomplete, WorkStatusDef, WorkSummary, WorkTypeDef } from "~/protocol/index.ts";

export interface RowChrome {
  /** What a handle is called. The chart's, so a row shows a person's name. */
  seatName?: (handle: string) => string;
  /**
   * Which KIND of seat a handle is — the chart's again, and the other half of
   * drawing a person.
   *
   * An identity badge has exactly one variant and it is structural: its
   * OUTLINE, a circle for a person and a squircle for an agent. Without this
   * the compact cells could not draw it at all, so one person was a circle in
   * a column that happened to be handed a kind and a squircle in the column
   * beside it — on the same grid, over the same row.
   *
   * `undefined` is a seat the chart does not hold, and it takes the kit's
   * default, the agent's squircle: the engine runs agents, and a person is
   * always declared. Both resolvers come from
   * [seatResolvers], so a builder cannot thread the name and drop this.
   */
  seatKind?: (handle: string) => SeatKind | undefined;
  /**
   * How an agent is drawn: its character and colour, the chart's third
   * answer, and from [seatResolvers] with the other two for the same reason.
   * `undefined` for a seat the chart does not hold, which keeps its initials.
   */
  seatAvatar?: (handle: string) => AgentAvatar | null | undefined;
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
      <NamedGlyph name={typeIcon(type)} size="sm" />
      {!decorative && <span className="sr-only">{name}</span>}
    </span>
  );
}

/** How many of the three bars a priority fills. `urgent` is not a fourth bar. */
const PRIORITY_LEVEL: Record<string, number> = { low: 1, normal: 2, high: 3 };

/**
 * The priority, as a shape first.
 *
 * THE SIGNAL BARS, which is how the approved Board draws it: three rising
 * bars filled to the step — one for `low`, two for `normal`, three for `high`
 * — and `urgent` as the alert mark rather than a fourth bar, because urgent is
 * not "more high", it is "stop and look". A level is read by COUNTING, so the
 * scale reads under any vision and in a screenshot with no colour spent on it
 * at all. It was a chevron pair once, and a chevron in a list is the
 * expand/collapse control: a column of them read as rows that fold.
 *
 * The bars are drawn by the stylesheet (`.work-prio-bars`) rather than as a
 * glyph: the kit vendors no signal mark, and a level meter is a data mark —
 * three boxes and a count — rather than a drawing with a name.
 *
 * ONLY `urgent` TAKES A STATUS TONE. `high` is a step on the scale; tinting it
 * would spend the caution hue on the fact that somebody filed a task.
 */
export function PriorityMark({ priority, word }: { priority?: string; word?: boolean }) {
  // EVERY STEP ON THE SCALE IS DRAWN, `normal` included — two of three bars,
  // as the approved Board draws it on every card. Blank for `normal` meant a
  // list whose priority column was mostly empty, where "two bars" never
  // appeared and a blank could not be told from a value this build did not
  // receive. The urgent cards still stand out: they are the only alert mark
  // in a column of bars. `none` (the engine's "nobody said") and an absent
  // value are not steps, and draw nothing.
  if (!priority || priority === "none") return null;
  const level = PRIORITY_LEVEL[priority];
  return (
    <span className="work-prio" data-priority={priority} title={`${priority} priority`}>
      {/* A value this build has no step for draws no mark rather than a
          guessed one — its word still says what it is. */}
      {priority === "urgent" ? (
        <CircleAlertGlyph size="sm" />
      ) : level !== undefined ? (
        <span className="work-prio-bars" data-level={level} aria-hidden="true">
          <i />
          <i />
          <i />
        </span>
      ) : null}
      {/* THE WORD AS A LABEL READS IT — "Urgent", like every other value in a
          rail — while the attribute and the title keep the engine's slug. */}
      {word ? (
        <span>{humanize(priority)}</span>
      ) : (
        <span className="sr-only">{priority} priority</span>
      )}
    </span>
  );
}

/**
 * A status as a mark, in the shape its state is drawn in ([STATUS_SHAPE]) —
 * the approved artboards' 14-unit drawings, so a half-filled ring is somebody
 * on it and a filled check is delivered, whatever the hue.
 *
 * DRAWN HERE RATHER THAN FROM THE KIT'S GLYPHS, which have a ring and a
 * ring-with-check and nothing between: two shapes for six states is what drew
 * in progress and in review as the same empty ring. The tone is the status's
 * own (`STATUS_TONE`) and reaches the drawing as `currentColor`; the check is
 * cut out in the surface colour so it reads on both themes.
 * Decorative: every caller prints the status's word beside it.
 */
export function StatusMark({ status }: { status: string }) {
  const tone = STATUS_TONE[status] ?? "neutral";
  const shape = statusShape(status);
  return (
    <span className={cx("work-status-mark", tone)} aria-hidden="true" data-shape={shape}>
      <svg viewBox="0 0 14 14" width="14" height="14" focusable="false">
        {shape === "check" || shape === "disc" ? (
          <circle cx="7" cy="7" r="6.2" fill="currentColor" />
        ) : (
          <circle cx="7" cy="7" r="5.5" fill="none" stroke="currentColor" strokeWidth="1.5" />
        )}
        {shape === "half" && <path d="M7 7V3.5A3.5 3.5 0 0 1 7 10.5z" fill="currentColor" />}
        {shape === "most" && <path d="M7 7V3.5A3.5 3.5 0 1 1 3.5 7z" fill="currentColor" />}
        {shape === "check" && (
          <path
            d="M4.4 7.2 6.2 9l3.4-3.6"
            className="work-status-cut"
            fill="none"
            strokeWidth="1.6"
            strokeLinecap="round"
            strokeLinejoin="round"
          />
        )}
        {shape === "struck" && (
          <path
            d="M4.5 9.5 9.5 4.5"
            stroke="currentColor"
            strokeWidth="1.5"
            strokeLinecap="round"
          />
        )}
      </svg>
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
  seatKind,
  seatAvatar,
  size = "sm",
  name: showName,
  status,
}: {
  handle?: string;
  seatName?: (handle: string) => string;
  seatKind?: (handle: string) => SeatKind | undefined;
  seatAvatar?: (handle: string) => AgentAvatar | null | undefined;
  size?: "sm" | "md";
  name?: boolean;
  /**
   * The holder's STATE, as the glyph on their badge's corner (`lib/seats.ts`
   * `statusOf`). Never the only carrier: the badge says it in its name where
   * it names itself, and the caller says it in words beside it.
   */
  status?: AvatarStatus | undefined;
}) {
  if (!handle) {
    // THE SQUARE IS A FIXED 20px BOX, so the word goes BESIDE it, never in
    // it: printed inside, "Unassigned" ran 44px past the box's edge, and a
    // grid measuring whether a column cuts its values short read that as a
    // squeezed Assignee column and dropped every droppable column in the
    // table to make room for a word no width could fit.
    const square = (
      <span className="work-nobody" title="Unassigned">
        <UserGlyph size="xs" />
        {!showName && <span className="sr-only">Unassigned</span>}
      </span>
    );
    if (!showName) return square;
    return (
      <span className="row gap-1" title="Unassigned">
        {square}
        <span className="muted truncate">Unassigned</span>
      </span>
    );
  }
  const label = seatName?.(handle) ?? handle;
  return (
    <span className="row gap-1" title={label}>
      {/* `decorative` ONLY WHERE THE NAME IS PRINTED BESIDE IT. Ours was
          `aria-hidden` either way and leant on a `title` on the wrapper, which
          a screen reader is free to ignore — so the four columns that draw the
          badge alone announced the assignee as nothing at all.

          AND THE OUTLINE IS THE ONE VARIANT A BADGE HAS: `linkedSeat` drew it
          from the kind it was handed and this cell could not be handed one,
          so the same human seat was drawn two ways on the two column sets of
          ONE grid. A kind the chart does not hold takes the kit's default,
          the agent's squircle. */}
      <SeatAvatar
        name={label}
        size={size}
        kind={seatKind?.(handle) === "human" ? "human" : "agent"}
        avatar={seatAvatar?.(handle)}
        decorative={showName}
        status={status}
      />
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
export function DueMark({ due, overdue }: { due?: string; overdue?: boolean }) {
  // ITS OWN CLOCK, re-rendering this mark alone and only when its words change.
  const label = useClock((now) => (due ? fmtDateCompact(due, now) : ""));
  if (!due) return null;
  return (
    // THE FULL INSTANT IS ON THE TITLE, always. What is DRAWN is the
    // compact form, because a column of dates repeating one year on every
    // row is how the one date not in this year goes unnoticed.
    <span className={cx("work-due", overdue && "overdue")} title={fmtDateTime(due)}>
      <CalendarGlyph size="xs" />
      {label}
      {overdue && <span className="sr-only">, overdue</span>}
    </span>
  );
}

/**
 * The mark for a notice that ASKS something of its recipient.
 *
 * ONE MARK, because there were two. The engine writes ONE fact —
 * `worklet.Reason.Addressed`, a wake that obliges its seat to answer rather than
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
      title={many ? "asked something, rather than merely informed" : "Asks for an answer"}
    >
      {/* ONE TEXT NODE, so a DOM query can match the pill's own label rather
          than a fragment of it. */}
      {many ? `${count} asked` : "asks"}
    </Tag>
  );
}

/**
 * What a card's foot draws, and whether it has anything to draw at all.
 *
 * EVERY MARK DECIDES ITS OWN ABSENCE by rendering nothing, so a card on a task
 * nobody has touched — no priority, no labels, no date, nothing spent — has
 * no foot rather than an empty band under its title. The predicate restates
 * each mark's own emptiness rule from the same fields, which is the price of
 * asking the question BEFORE rendering: a container cannot ask a child that
 * drew nothing whether it did. A mark that gains a field adds it here.
 *
 * THE ASSIGNEE IS NOT A FOOT MARK ANY MORE. It is the card's top-right, beside
 * its key, as the approved board draws it — so the dashed "nobody" square is
 * always there to say a task is unclaimed, and never floats alone in a foot.
 */
function hasFootMarks(row: WorkSummary, omit: ReadonlySet<CardFact>): boolean {
  return Boolean(
    (!omit.has("priority") && row.priority && row.priority !== "none") ||
    readsBlocked(row) ||
    (!omit.has("due") && row.due) ||
    (!omit.has("labels") && row.tags?.length) ||
    row.dependents_count ||
    row.open_asks ||
    (!omit.has("tokens") && row.spend?.tokens),
  );
}

/**
 * The facts a board card draws that a reader may put away — the Display
 * menu's "Card shows" (`card_hide=`).
 *
 * THE MARKS A READER MAY NOT PUT AWAY are the ones a board is triaged by: the
 * key (the card's address), the holder, the title, and the STATES — blocked,
 * "blocks N", open asks and the live band. Each of those says something is
 * wrong or happening now, and a board that could hide them would be a board
 * reporting a quiet project that is not one. What is offered is the
 * DESCRIPTIVE half: how big, how urgent, which categories, when, and what it
 * has cost in tokens.
 *
 * ONE DECLARATION, read by the menu's checkboxes and by the card, so the menu
 * cannot offer a fact the card does not draw.
 */
export const CARD_FACTS = [
  { key: "size", label: "Size" },
  { key: "priority", label: "Priority" },
  { key: "labels", label: "Labels" },
  { key: "due", label: "Due date" },
  { key: "tokens", label: "Tokens" },
] as const;

export type CardFact = (typeof CARD_FACTS)[number]["key"];

const NO_OMIT: ReadonlySet<CardFact> = new Set();
const NO_TAGS: readonly string[] = [];

/**
 * `card_hide=` read: the facts put away, keeping only the ones a card draws —
 * a key a later build retired, on an address somebody kept, is ignored rather
 * than hiding nothing under a name nobody can see.
 */
export function cardHidden(raw: string): ReadonlySet<CardFact> {
  if (!raw) return NO_OMIT;
  const known = new Set<string>(CARD_FACTS.map((f) => f.key));
  return new Set(raw.split(",").filter((k): k is CardFact => known.has(k)));
}

/** `card_hide=` written: in declaration order, so a click never reorders the key. */
export function cardHideParam(hidden: ReadonlySet<CardFact>): string {
  return CARD_FACTS.filter((f) => hidden.has(f.key))
    .map((f) => f.key)
    .join(",");
}

/**
 * How many of a card's labels fit on its mark row beside everything else.
 *
 * THE LABELS GIVE WAY FIRST, and to a "+N": the token count stays right-aligned
 * on the row and the marks a reader triages by — priority, blocked, due,
 * "blocks N", open asks — stay whole, because a label is the one mark that is
 * a category rather than a state. The row used to wrap instead, and the count
 * then stood on a line of its own under a full row of chips.
 *
 * `room` is the row's width less every mark that is not a label, and the gaps
 * between those; each label costs its width and a gap, and so does the "+N".
 */
export function fitTags(
  room: number,
  widths: readonly number[],
  plus: number,
  gap: number,
): number {
  const all = widths.reduce((sum, w) => sum + w + gap, 0);
  if (all <= room) return widths.length;
  let used = plus + gap;
  let fit = 0;
  for (const w of widths) {
    if (used + w + gap > room) break;
    used += w + gap;
    fit += 1;
  }
  return fit;
}

/**
 * A card's mark row: the marks, the labels that fit, and the spend in tokens
 * at its far end — see [fitTags].
 *
 * EVERY LABEL IS RENDERED and the ones that do not fit are taken out of the
 * flow rather than out of the tree, so each keeps a width to measure: a label
 * that came back after a resize would otherwise have no size to decide it by.
 */
function CardFoot({
  row,
  tagName,
  omit,
}: {
  row: WorkSummary;
  tagName?: (slug: string) => string;
  omit: ReadonlySet<CardFact>;
}) {
  const foot = useRef<HTMLDivElement>(null);
  const tags = omit.has("labels") ? NO_TAGS : (row.tags ?? NO_TAGS);
  const [fit, setFit] = useState(tags.length);
  const tagKey = tags.join(",");
  useLayoutEffect(() => {
    const el = foot.current;
    if (!el) return;
    const measure = () => {
      const items = [...el.children] as HTMLElement[];
      const gap = parseFloat(getComputedStyle(el).columnGap) || 0;
      const fixed = items.filter((c) => !c.dataset.tag && !c.dataset.more);
      const room =
        el.clientWidth -
        fixed.reduce((sum, c) => sum + (c.classList.contains("spacer") ? 0 : c.offsetWidth), 0) -
        gap * Math.max(0, fixed.length - 1);
      const widths = items.filter((c) => c.dataset.tag).map((c) => c.offsetWidth);
      const plus = items.find((c) => c.dataset.more)?.offsetWidth ?? 0;
      const next = el.clientWidth > 0 ? fitTags(room, widths, plus, gap) : widths.length;
      setFit((prev) => (prev === next ? prev : next));
    };
    measure();
    if (typeof ResizeObserver === "undefined") return;
    const watch = new ResizeObserver(measure);
    watch.observe(el);
    return () => watch.disconnect();
  }, [tagKey]);
  const shown = Math.min(fit, tags.length);
  const rest = tags.slice(shown).map((slug) => tagName?.(slug) ?? slug);
  return (
    <div className="work-card-foot" ref={foot}>
      {!omit.has("priority") && <PriorityMark priority={row.priority} />}
      {readsBlocked(row) && (
        <Tag size="xs" variant="danger" appearance="outline">
          Blocked
        </Tag>
      )}
      {tags.map((slug, i) => (
        <Tag
          key={slug}
          size="xs"
          appearance="outline"
          data-tag="true"
          className={i >= shown ? "work-card-tag-over" : undefined}
          aria-hidden={i >= shown || undefined}
        >
          {tagName?.(slug) ?? slug}
        </Tag>
      ))}
      {/* THE LABELS THAT DID NOT FIT, counted — and named in the title and to
          a screen reader, so collapsing them hides nothing. Always rendered,
          so it has a width to plan with; out of the flow while nothing is
          hidden. */}
      <Tag
        size="xs"
        appearance="outline"
        data-more="true"
        className={rest.length ? undefined : "work-card-tag-over"}
        aria-hidden={rest.length ? undefined : true}
        title={rest.length ? rest.join(", ") : undefined}
      >
        <span aria-hidden="true">{`+${Math.max(rest.length, 1)}`}</span>
        {rest.length ? (
          <span className="sr-only">{`${plural(rest.length, "more label")}: ${rest.join(", ")}`}</span>
        ) : null}
      </Tag>
      {!omit.has("due") && <DueMark due={row.due} overdue={row.overdue} />}
      {/* WHAT THIS TASK HOLDS UP — the live tasks waiting on it, which is
          the other end of `blocked`: the reason to pick THIS card first. */}
      {row.dependents_count ? (
        <Tag size="xs" variant="danger">{`blocks ${row.dependents_count}`}</Tag>
      ) : null}
      {row.open_asks ? <AsksTag count={row.open_asks} /> : null}
      <span className="spacer" />
      {!omit.has("tokens") && row.spend?.tokens ? (
        <span className="work-card-tokens mono" title={`${fmtExact(row.spend.tokens)} tokens`}>
          {fmtCount(row.spend.tokens)}
          <span className="sr-only"> tokens</span>
        </span>
      ) : null}
    </div>
  );
}

/** An executor parked on this task that is waiting on the reader. */
export interface CardWaiting {
  /** When the executor's box began being held for an answer. */
  since?: string;
}

/**
 * One task as a board card.
 *
 * FOUR ROWS, EACH FIXED: identity (type, key, size, holder), the title in two
 * lines, the facts, and — only while something is happening on it — one band
 * saying what. A column of cards then scans down its own columns rather than as
 * a stack of paragraphs, and a card that gains a due date does not reshuffle
 * the rows above it.
 *
 * THE BAND IS A STATE, so it is the one part of a card with a hue: blue while
 * a seat's turn is on the task, amber while an executor on it is parked
 * waiting for the READER — never for somebody else, whose waiting executor is
 * a fact on the task page rather than a call to this person. Both say it in
 * words, because a tint alone is invisible to a reader who does not see it.
 *
 * TOKENS, NEVER MONEY. What the task has cost is the tokens its turns were
 * charged (`spend.tokens`), in the product's one compact count.
 */
/** How long a card's run has gone on, on a clock of its own. */
function CardAge({ since }: { since?: string }) {
  return <span className="work-card-age">{useClock((now) => shortAge(since, now))}</span>;
}

export function WorkCard({
  row,
  href,
  onOpen,
  selected,
  chrome = {},
  live,
  waiting,
  status,
  pending,
  tagName,
  drag,
  omit = NO_OMIT,
}: {
  row: WorkSummary;
  href: string;
  onOpen?: () => void;
  selected?: boolean;
  chrome?: RowChrome;
  /** The descriptive facts the reader put away — see [CARD_FACTS]. */
  omit?: ReadonlySet<CardFact>;
  /** The turn running on this task, where one is. */
  live?: CardLive | null;
  /** An executor on this task parked waiting on the reader. */
  waiting?: CardWaiting | null;
  /** The holder's state, as the glyph on their badge, where the engine reports one. */
  status?: AvatarStatus | undefined;
  /** A move of this card is in flight and the engine has not answered. */
  pending?: boolean;
  /** A tag slug in the project's own words. */
  tagName?: (slug: string) => string;
  /** What makes the card draggable, where the reader may move it. */
  drag?: HTMLAttributes<HTMLAnchorElement>;
}) {
  return (
    <a
      className={cx("work-card", selected && "selected")}
      data-blocked={readsBlocked(row) ? "true" : undefined}
      data-pending={pending ? "true" : undefined}
      aria-busy={pending || undefined}
      href={href}
      onClick={peekClickHandler(onOpen)}
      {...drag}
    >
      <div className="work-card-top">
        {/* THE EXCEPTIONS ONLY — see [DEFAULT_TYPE]. */}
        {row.type && row.type !== DEFAULT_TYPE ? (
          <TypeIcon type={row.type} types={chrome.types} />
        ) : null}
        <span className="work-key mono">{row.key}</span>
        {/* THE SIZE, in whichever measure it was given — "1 pt", "5 pts", "2h". */}
        {!omit.has("size") && (row.points || row.estimate_min) ? (
          <span className="work-card-points">
            {row.points ? plural(row.points, "pt") : fmtMinutes(row.estimate_min!)}
          </span>
        ) : null}
        <span className="spacer" />
        <Assignee
          handle={row.assignee}
          seatName={chrome.seatName}
          seatKind={chrome.seatKind}
          seatAvatar={chrome.seatAvatar}
          status={status}
        />
      </div>
      <div className="work-card-title clamp">{row.title}</div>
      {hasFootMarks(row, omit) && <CardFoot row={row} tagName={tagName} omit={omit} />}
      {live ? (
        <div className="work-card-live" data-tone="working">
          <i className="dot info" aria-hidden="true" />
          <span className="truncate">
            {chrome.seatName?.(live.handle) ?? live.handle} · {live.doing}
          </span>
          <CardAge since={live.since} />
        </div>
      ) : waiting ? (
        <div className="work-card-live" data-tone="needs">
          <CircleAlertGlyph size="xs" aria-hidden="true" />
          <span className="truncate">Waiting on you · executor parked</span>
          <CardAge since={waiting.since} />
        </div>
      ) : null}
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
 * EXPORTED, because every task list's title cell draws it
 * (`components/workGrid.tsx`): one sentence rather than a copy per list.
 */
export function blockedBy(row: WorkSummary, keyOf?: (id: string) => string | undefined): string {
  // `open` IS THE FLAG THE EDGE CARRIES, and `false` is a settled fact rather
  // than a missing one — the blocker has finished. An edge that says nothing is
  // counted as open, because `blocked` on the row is exactly "some entry here
  // is open" and the two must not disagree.
  const held = (row.waiting_on ?? []).filter((edge) => edge.open !== false);
  if (held.length === 0) return "Blocked";
  // A WAIT NOTHING MOVES IS NAMED FIRST: it is the one that never ends by itself.
  const dead = held.find((edge) => edge.dead);
  if (dead) {
    const nobody = `Nobody is reviewing ${pullName(dead.id)}`;
    return held.length > 1 ? `${nobody} +${held.length - 1}` : nobody;
  }
  const named = held.map((edge) => keyOf?.(edge.id)).find(Boolean);
  if (!named) return held.length > 1 ? `Blocked · ${held.length}` : "Blocked";
  return held.length > 1 ? `Blocked · ${named} +${held.length - 1}` : `Blocked · ${named}`;
}

/** A pull request's address as a person names it, `PR #42`, or the address itself. */
function pullName(address: string): string {
  const number = /\/pull\/(\d+)(?:[/?#]|$)/.exec(address)?.[1];
  return number ? `PR #${number}` : address;
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
 * What an answer whose `complete` is false says, wherever it is drawn: the
 * state bar's banner and a panel's own callout are one sentence, so they
 * cannot drift apart.
 */
export const INCOMPLETE_NOTE =
  "Some records need a newer engine version to read, so this may be incomplete.";

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
 * than a badge beside them, and it names the remedy: a newer engine that can
 * read the records, not a refresh.
 */
export function Coverage({ answer }: { answer?: CoverageFacts | null }) {
  if (!answer) return null;
  return (
    <>
      {answer.complete === false && (
        // The severity stays a WARNING rather than a danger: the answer is
        // usable, it just cannot account for everything, which is precisely
        // what the sentence says.
        <Callout variant="warning">{INCOMPLETE_NOTE}</Callout>
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
  const behind = appliedThrough(answer);
  const level = answer.read_level ?? "";
  const odd = oddLevel(level);
  if (!odd && behind === null) return null;
  return (
    <span className="work-coverage">
      {odd && (
        <Tag
          variant="warning"
          appearance="outline"
          size="xs"
          title="How current this is can’t be measured right now."
        >
          age unknown
        </Tag>
      )}
      {/* APPLIED_THROUGH BESIDE SEQ, so a node holding something it cannot
          apply is visible as its own state rather than as lag. NEUTRAL: a
          read level and an apply position are facts about the answer, not
          states of it, and lag alone is never an alarm. */}
      {behind !== null && (
        <Tag appearance="outline" size="xs" title="This node holds records it has not applied yet">
          {behind}
        </Tag>
      )}
    </span>
  );
}

/**
 * A packed log position as a person reads it: the sequence alone in the first
 * generation, where every log starts and almost all stay, and `generation:seq`
 * past it, so a re-anchored log says which generation it is in.
 *
 * `applied_through` and `log_seq` are packed — (generation × 2^40) + seq, by
 * the engine's own stride ([GENERATION_STRIDE]) — so they ORDER correctly with
 * a plain `<` across a re-anchor, and that comparison stays on the packed
 * values. What a person reads is not packed: printed raw, every position after
 * a log's first re-anchor was a thirteen-digit number ("applied through
 * 1099511627817 of 1099511627832") that names neither the generation nor how
 * far behind the node is.
 */
export function positionWords(packed: number): string {
  const generation = Math.floor(packed / GENERATION_STRIDE);
  const seq = packed - generation * GENERATION_STRIDE;
  return generation === 0 ? `${seq}` : `${generation}:${seq}`;
}

/** "applied through 41 of 88" for an answer whose node is behind its log, or null. */
export function appliedThrough(facts: CoverageFacts | null | undefined): string | null {
  if (facts?.applied_through === undefined || facts.log_seq === undefined) return null;
  return facts.applied_through < facts.log_seq
    ? `applied through ${positionWords(facts.applied_through)} of ${positionWords(facts.log_seq)}`
    : null;
}
