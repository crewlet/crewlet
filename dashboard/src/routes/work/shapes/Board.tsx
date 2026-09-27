/**
 * The board shape: one lane per value of the axis, cards inside them — and
 * the one shape a reader can rearrange by hand.
 *
 * A LANE IS A COLUMN OF THE SHEET AND A CARD IS AN OBJECT ON IT — a heading
 * and a run of cards on the page's own ground, never a recessed box; the card
 * is the one place this product spends elevation. `styles/screens.css` carries
 * that reasoning, and why a lane is sized from the board's width so the board
 * shows whole lanes and [BoardPager] names the rest.
 *
 * A COLUMN'S COUNT IS OVER THE WHOLE SET and its rows are a slice, so a column
 * of four hundred says four hundred and hands back fifty. The rest are
 * reachable rather than merely counted: the lane's foot is a link to the list
 * narrowed to that lane.
 *
 * # A drag is a write, made as the reader
 *
 * Dropping a card calls `place_work_item` as the person the token is bound to
 * (ADR-0024): into another lane is a status change and a place in the order,
 * within a lane it is a place alone. Three rules make it honest:
 *
 *  - ONLY UNDER THE MANUAL ORDER. A board sorted by due date is not in an
 *    order a drag can change; a card dropped there would jump back to where
 *    its date puts it, which reads as a refused move that was not refused.
 *  - CONFIRMED, NOT OPTIMISTIC. The card is drawn where it was dropped with a
 *    pending mark, and the ENGINE's answer decides: a refusal snaps it back
 *    and says why in the engine's words; an applied move is re-read at the
 *    position it landed.
 *  - A LANE CHANGE THAT LANDED STAYS. The move is two records — the status on
 *    the task, then its place in the project's order — and when the second is
 *    refused after the first landed, the answer says `placed: false`. The card
 *    is then in its new lane at whatever place the engine keeps it, and the
 *    sentence says the place was not taken; snapping it back across lanes
 *    would draw a status that is no longer true.
 *
 * AND A KEYBOARD MOVES IT TOO: `Alt` with an arrow on a focused card is the
 * same gesture ([keyboardDrop]), because a pointer-only drag is a write a
 * keyboard reader cannot make.
 */

import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type DragEvent,
  type KeyboardEvent,
  type RefObject,
} from "react";
import { createPortal } from "react-dom";
import { Button, Callout, Menu, cx } from "@crewlethq/ui";
import {
  ChevronDownGlyph,
  ChevronLeftGlyph,
  ChevronRightGlyph,
  EllipsisGlyph,
  EyeOffGlyph,
  ListGlyph,
} from "@crewlethq/icons/glyphs";
import { WorkCard, type CardFact, type CardWaiting, type RowChrome } from "~/components/work.tsx";
import { RefusalNote } from "~/components/WriteButton.tsx";
import { useAct } from "~/lib/useAct.ts";
import type { WriteAccess } from "~/lib/useWriteAccess.ts";
import type { CardLive, SeatRing } from "~/lib/seats.ts";
import { plural } from "~/lib/format.ts";
import { GroupMark, headingOf } from "./group.tsx";
import { keyboardDrop, moveFor, withDrop, type Drop } from "./boardMove.ts";
import type { WorkGroup, WorkProjectDetail, WorkSummary } from "~/protocol/index.ts";

/** What the board knows about the seats and runs on its cards, keyed by task key or handle. */
export interface BoardFacts {
  /** The turn running on a task, by task key. */
  live: ReadonlyMap<string, CardLive>;
  /** A coding run on a task parked waiting on the reader, by task key. */
  waiting: ReadonlyMap<string, CardWaiting>;
  /** A holder's state ring, by handle. */
  ring: (handle: string) => SeatRing | undefined;
}

const NO_FACTS: BoardFacts = { live: new Map(), waiting: new Map(), ring: () => undefined };

export function Board({
  groups,
  axis,
  chrome,
  detail,
  now,
  selected,
  hrefOf,
  onOpen,
  onOverflow,
  overflowHref,
  facts = NO_FACTS,
  hidden,
  onHide,
  movable,
  finishedLanes,
  weekly,
  laneSlot,
  cardOmit,
}: {
  groups: WorkGroup[];
  axis: string;
  chrome: RowChrome;
  detail?: WorkProjectDetail | null;
  now: number;
  selected?: string;
  hrefOf: (row: WorkSummary) => string;
  onOpen: (row: WorkSummary) => void;
  onOverflow: (axis: string, key: string) => void;
  /** Where the column footer GOES — the same pairing as `hrefOf`/`onOpen`,
   *  and for the same reason: the screen owns the address, because only it
   *  knows which project and which filters the reader is looking at. */
  overflowHref: (axis: string, key: string) => string;
  facts?: BoardFacts;
  /** The lanes the reader put away (`hide=`), by key. */
  hidden?: ReadonlySet<string>;
  /** Put one lane away. Absent where the screen offers no way to. */
  onHide?: (key: string) => void;
  /**
   * Whether the order on screen is one a drag can change — the manual order —
   * and so whether a card is draggable at all. The WRITE's own access is the
   * board's to read; this is the screen's half of the question.
   */
  movable?: boolean;
  /** The lanes holding finished work, by key — where "N more" is this week's. */
  finishedLanes?: ReadonlySet<string>;
  /** The scope is Recent, so a finished lane holds this week's work alone. */
  weekly?: boolean;
  /**
   * Where the lanes out of view are named — a slot at the end of the screen's
   * own bar. Named there, they cost the lanes no row of their own above them;
   * absent, the board names them above its lanes.
   */
  laneSlot?: HTMLElement | null;
  /** The card facts the reader put away (`card_hide=`) — see [CARD_FACTS]. */
  cardOmit?: ReadonlySet<CardFact>;
}) {
  const move = useAct("place_work_item");
  // THE DROP IN FLIGHT, drawn until the engine answers — and the UNPLACED one,
  // held after an answer that changed the lane and refused the place, until
  // the re-read that answer fires arrives with the card where it now is.
  const [drop, setDrop] = useState<{ drop: Drop; settled: boolean; against: WorkGroup[] } | null>(
    null,
  );
  const [unplaced, setUnplaced] = useState("");
  const [dragging, setDragging] = useState("");
  // A DRAG TRIED UNDER AN ORDER A DRAG CANNOT CHANGE. The reason is said
  // then — when somebody reaches for the gesture — rather than on a line above
  // every board, where it cost a row on each visit to explain a gesture most
  // visits never make.
  const [tried, setTried] = useState(false);
  const [over, setOver] = useState<{ lane: string; above: string } | null>(null);
  const scroller = useRef<HTMLDivElement>(null);

  // A SETTLED DROP GOES WHEN THE ANSWER IT WAS DRAWN OVER DOES: the write's
  // own re-read is what redraws the card, so the overlay is dropped on the
  // first new answer rather than on a timer that could beat it.
  useEffect(() => {
    if (drop?.settled && drop.against !== groups) setDrop(null);
  }, [groups, drop]);

  const shown = useMemo(() => groups.filter((g) => !hidden?.has(g.key)), [groups, hidden]);
  const drawn = useMemo(() => withDrop(shown, drop?.drop ?? null), [shown, drop]);
  const out = useLanesOutOfView(scroller, drawn.length);

  const canDrag = Boolean(movable) && move.access.can && !move.busy;

  const send = async (next: Drop) => {
    const args = moveFor(next, shown, axis);
    if (!args) return;
    setUnplaced("");
    setDrop({ drop: next, settled: false, against: groups });
    const result = await move.run(args, { done: `Moved ${args.item}` });
    if (!result || result.kind === "refused" || result.kind === "unknown") {
      // SNAPPED BACK: the engine refused it, or nobody can say it landed —
      // either way what is drawn is the answer the board last had, and the
      // refusal (or the toast) says why.
      setDrop(null);
      return;
    }
    const receipt = (result.receipt ?? {}) as { placed?: boolean; unplaced?: string };
    if (receipt.placed === false) {
      setUnplaced(
        receipt.unplaced ||
          `${args.item} moved lanes, but the engine could not give it that place in the order.`,
      );
    }
    setDrop((prev) => (prev ? { ...prev, settled: true } : prev));
  };

  if (drawn.length === 0) return null;
  const ctx = {
    statuses: detail?.statuses,
    types: chrome.types,
    tags: detail?.tags,
    seatName: chrome.seatName,
  };
  const tagName = (slug: string) => detail?.tags?.find((t) => t.slug === slug)?.label ?? slug;
  const laneName = (group: WorkGroup) => headingOf(axis, group, ctx);
  const note = moveNote(move.access);
  const orderNote = tried && !movable && move.access.can ? MANUAL_ORDER_NOTE : "";

  const dragProps = (row: WorkSummary) =>
    !canDrag
      ? move.access.can && !movable
        ? {
            // THE GESTURE IS CAUGHT AND ANSWERED rather than let through: a
            // card is a link, and a link dragged with nothing to catch it is
            // the browser's own drag of its address — the card lifting and
            // landing nowhere, which reads as a board that is broken.
            onDragStart: (e: DragEvent<HTMLAnchorElement>) => {
              e.preventDefault();
              setTried(true);
            },
            onKeyDown: (e: KeyboardEvent<HTMLAnchorElement>) => {
              if (!e.altKey || !ARROWS[e.key]) return;
              e.preventDefault();
              setTried(true);
            },
          }
        : undefined
      : {
          draggable: true,
          "aria-describedby": "work-board-move-hint",
          onDragStart: (e: DragEvent<HTMLAnchorElement>) => {
            e.dataTransfer.effectAllowed = "move";
            e.dataTransfer.setData("text/plain", row.key);
            setDragging(row.key);
          },
          onDragEnd: () => {
            setDragging("");
            setOver(null);
          },
          onKeyDown: (e: KeyboardEvent<HTMLAnchorElement>) => {
            if (!e.altKey) return;
            const direction = ARROWS[e.key];
            if (!direction) return;
            e.preventDefault();
            const next = keyboardDrop(row.key, shown, direction);
            if (next) void send(next);
          },
        };

  /** Where in a lane the pointer is: above the card it is over, by halves. */
  const overLane = (lane: WorkGroup, e: DragEvent<HTMLElement>) => {
    if (!dragging) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = "move";
    const cards = [...e.currentTarget.querySelectorAll<HTMLElement>("[data-card]")];
    const below = cards.find((el) => {
      const box = el.getBoundingClientRect();
      return e.clientY < box.top + box.height / 2;
    });
    const above = below?.dataset.card ?? "";
    if (over?.lane !== lane.key || over.above !== above) setOver({ lane: lane.key, above });
  };

  const pager = (
    <BoardPager
      before={out.before.map((i) => laneName(drawn[i]!))}
      after={out.after.map((i) => laneName(drawn[i]!))}
      onPage={(direction) => pageLanes(scroller.current, direction)}
    />
  );

  return (
    <>
      {canDrag && (
        <span id="work-board-move-hint" className="sr-only">
          Drag to move, or press Alt with an arrow key: up and down to reorder, left and right to
          change lane.
        </span>
      )}
      <RefusalNote write={move} />
      {unplaced && (
        <Callout
          variant="warning"
          action={
            <Button size="small" variant="ghost" onClick={() => setUnplaced("")}>
              Dismiss
            </Button>
          }
        >
          {unplaced}
        </Callout>
      )}
      {orderNote && (
        <Callout
          variant="info"
          action={
            <Button size="small" variant="ghost" onClick={() => setTried(false)}>
              Dismiss
            </Button>
          }
        >
          {orderNote}
        </Callout>
      )}
      {laneSlot
        ? createPortal(pager, laneSlot)
        : (note !== "" || out.before.length > 0 || out.after.length > 0) && (
            <div className="work-board-head">
              {note && <p className="t-caption work-write-note">{note}</p>}
              {pager}
            </div>
          )}
      {laneSlot && note && <p className="t-caption work-write-note">{note}</p>}
      <div
        className="work-board-frame"
        data-more-start={out.before.length ? "true" : undefined}
        data-more-end={out.after.length ? "true" : undefined}
      >
        <div
          className="work-board"
          ref={scroller}
          onScroll={out.measure}
          data-dragging={dragging ? "true" : undefined}
        >
          {drawn.map((group) => {
            const finished = finishedLanes?.has(group.key) ?? false;
            const more = group.count - group.rows.length;
            const label = headingOf(axis, group, ctx);
            return (
              <section
                className={cx("work-col", over?.lane === group.key && "drop-target")}
                key={group.key || "—"}
                aria-label={`${label}, ${plural(group.count, "task")}`}
                onDragOver={(e) => overLane(group, e)}
                onDragLeave={(e) => {
                  if (!e.currentTarget.contains(e.relatedTarget as Node | null)) setOver(null);
                }}
                onDrop={(e) => {
                  e.preventDefault();
                  const key = e.dataTransfer.getData("text/plain") || dragging;
                  const above = over?.lane === group.key ? over.above : "";
                  setDragging("");
                  setOver(null);
                  if (key) void send({ key, lane: group.key, above: above === key ? "" : above });
                }}
              >
                <header className="work-col-head">
                  <GroupMark axis={axis} groupKey={group.key} chrome={chrome} />
                  <span className="work-col-name truncate">{label}</span>
                  <span className="work-col-count">{group.count}</span>
                  <span className="spacer" />
                  {onHide && (
                    <Menu
                      label={`${label} lane options`}
                      icon={<EllipsisGlyph size="sm" />}
                      align="end"
                      items={[
                        {
                          key: "list",
                          label: "Open as a list",
                          icon: <ListGlyph size="sm" />,
                          onSelect: () => onOverflow(axis, group.key),
                        },
                        {
                          key: "hide",
                          label: "Hide this lane",
                          icon: <EyeOffGlyph size="sm" />,
                          onSelect: () => onHide(group.key),
                        },
                      ]}
                    />
                  )}
                </header>
                <div className="work-col-body">
                  {group.rows.map((row) => (
                    <div
                      key={row.id}
                      className="work-col-slot"
                      data-card={row.key}
                      data-drop-above={
                        over?.lane === group.key && over.above === row.key ? "true" : undefined
                      }
                    >
                      <WorkCard
                        row={row}
                        now={now}
                        chrome={chrome}
                        href={hrefOf(row)}
                        selected={selected === row.key}
                        onOpen={() => onOpen(row)}
                        live={facts.live.get(row.key)}
                        waiting={facts.waiting.get(row.key)}
                        ring={row.assignee ? facts.ring(row.assignee) : undefined}
                        pending={drop?.drop.key === row.key && !drop.settled}
                        tagName={tagName}
                        drag={dragProps(row)}
                        omit={cardOmit}
                      />
                    </div>
                  ))}
                  {/* A DECLARED LANE WITH NOTHING IN IT. The engine pads a closed
                    axis to every column its own predicate admits, which is what
                    makes a board the WORKFLOW rather than the occupied part of
                    it. A heading over nothing at all reads as rows that failed
                    to arrive. */}
                  {group.rows.length === 0 && <div className="work-col-empty">Nothing here</div>}
                  {/* THE REST OF THE LANE, as the list narrowed to it. "This week"
                    on a finished lane under Recent, because that is all the lane
                    holds there — the week's deliveries, not the company's. */}
                  {more > 0 && (
                    <a
                      className="work-col-more"
                      href={overflowHref(axis, group.key)}
                      onClick={(e) => {
                        e.preventDefault();
                        onOverflow(axis, group.key);
                      }}
                    >
                      <ChevronDownGlyph size="xs" aria-hidden="true" />
                      {finished && weekly ? `${more} more this week` : `${more} more`}
                    </a>
                  )}
                </div>
              </section>
            );
          })}
        </div>
      </div>
    </>
  );
}

/**
 * WHY THE CARDS DO NOT MOVE FOR THIS READER, said once above the lanes — and
 * nothing where they do.
 *
 * A WRITE IS NEVER HIDDEN, and a drag is a write with no button to disable: a
 * card that will not lift reads as a board that is broken. So a reader the
 * engine will not move cards for is told why in the sentence every other
 * control uses (`lib/useWriteAccess.ts`). A writer looking at an order a drag
 * cannot change is told which order can when they TRY — [MANUAL_ORDER_NOTE] —
 * and in the Sort control's title, rather than on a line on every visit.
 */
function moveNote(access: WriteAccess): string {
  return access.can ? "" : `Moving cards is off. ${access.reason}`;
}

/** What a drag tried outside the manual order is answered with. */
export const MANUAL_ORDER_NOTE =
  "Cards move in the manual order — choose Manual under Sort to drag them.";

/**
 * Which lanes are not wholly in view, by index, on each side.
 *
 * WHOLLY, because a lane cut at the scroller's edge is a lane whose cards a
 * reader cannot read — and a pager that stayed quiet about it would be as
 * silent as the board it was added to. A pixel of slack absorbs the rounding
 * a fractional lane width leaves at the far edge.
 */
export function lanesOutOfView(
  lanes: readonly { left: number; width: number }[],
  scrollLeft: number,
  viewWidth: number,
): { before: number[]; after: number[] } {
  const before: number[] = [];
  const after: number[] = [];
  lanes.forEach((lane, i) => {
    if (lane.left < scrollLeft - 1) before.push(i);
    else if (lane.left + lane.width > scrollLeft + viewWidth + 1) after.push(i);
  });
  return { before, after };
}

/**
 * The lanes out of view on the board a ref holds, re-measured on a scroll, a
 * resize and a change in how many lanes there are.
 */
function useLanesOutOfView(
  ref: RefObject<HTMLDivElement | null>,
  count: number,
): { before: number[]; after: number[]; measure: () => void } {
  const [out, setOut] = useState<{ before: number[]; after: number[] }>({ before: [], after: [] });
  const measure = useCallback(() => {
    const board = ref.current;
    if (!board) return;
    const lanes = [...board.children].map((el) => {
      const lane = el as HTMLElement;
      return { left: lane.offsetLeft - board.offsetLeft, width: lane.offsetWidth };
    });
    const next = lanesOutOfView(lanes, board.scrollLeft, board.clientWidth);
    // THE SAME ANSWER IS NOT A NEW STATE: a scroll fires every frame, and a
    // fresh array each time would re-render every card on the board with it.
    setOut((prev) =>
      prev.before.join() === next.before.join() && prev.after.join() === next.after.join()
        ? prev
        : next,
    );
  }, [ref]);
  useLayoutEffect(measure, [measure, count]);
  useEffect(() => {
    const board = ref.current;
    if (!board || typeof ResizeObserver === "undefined") return;
    const watch = new ResizeObserver(measure);
    watch.observe(board);
    return () => watch.disconnect();
  }, [ref, measure]);
  return { ...out, measure };
}

/** Scroll the board by one view, toward the side asked for. */
function pageLanes(board: HTMLDivElement | null, direction: "before" | "after") {
  // BY THE VIEW'S WIDTH, and the snap lands it on a lane's start: whole lanes
  // before a page and whole lanes after it. The smoothness is the sheet's, so
  // a reader who asked for reduced motion gets the jump.
  board?.scrollBy({ left: (direction === "after" ? 1 : -1) * board.clientWidth });
}

/**
 * THE LANES OUT OF VIEW, NAMED, above the board.
 *
 * A board wider than its window is the normal case — a Recent board has six
 * lanes — and its horizontal scrollbar sits under its tallest lane, usually
 * under the fold, so a reader had nothing on screen to say Cancelled and Closed
 * existed. Each side says how many lanes are past it and is a button that
 * brings them into view.
 *
 * ONE PHRASING AT EVERY WIDTH — "2 more lanes ›", "‹ 1 earlier lane". It used
 * to print the lane names when there were two or fewer, so the same control
 * read "Cancelled, Closed ›" at 1440 and "3 more lanes ›" at 1280, and a bare
 * list of status words did not say it reveals lanes. The names are in the
 * accessible name and the tooltip, where a reader asking which lanes finds
 * them.
 */
export function BoardPager({
  before,
  after,
  onPage,
}: {
  before: string[];
  after: string[];
  onPage: (direction: "before" | "after") => void;
}) {
  if (before.length === 0 && after.length === 0) return null;
  return (
    <span className="work-board-pager" role="group" aria-label="Lanes out of view">
      {before.length > 0 && (
        <Button
          size="small"
          variant="ghost"
          leadingIcon={<ChevronLeftGlyph size="sm" />}
          title={before.join(", ")}
          aria-label={`Show ${plural(before.length, "earlier lane")}: ${before.join(", ")}`}
          onClick={() => onPage("before")}
        >
          {plural(before.length, "earlier lane")}
        </Button>
      )}
      {after.length > 0 && (
        <Button
          size="small"
          variant="ghost"
          trailingIcon={<ChevronRightGlyph size="sm" />}
          title={after.join(", ")}
          aria-label={`Show ${plural(after.length, "more lane")}: ${after.join(", ")}`}
          onClick={() => onPage("after")}
        >
          {plural(after.length, "more lane")}
        </Button>
      )}
    </span>
  );
}

const ARROWS: Record<string, "up" | "down" | "left" | "right"> = {
  ArrowUp: "up",
  ArrowDown: "down",
  ArrowLeft: "left",
  ArrowRight: "right",
};
