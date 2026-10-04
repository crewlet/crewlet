/**
 * The order somebody means to work in — theirs, or a lead's for them — and
 * the one place it is changed from the dashboard.
 *
 * # The order is the content
 *
 * The rows are drawn IN THE STORED ORDER, never re-sorted, and NUMBERED: the
 * order is what somebody decided, and a decision nothing on screen shows is one
 * the reader cannot act on. Every other section of My work is a SET somebody
 * has a claim on; this one is a SEQUENCE, and drawn as an ordinary run of rows
 * it reads exactly like the Watching list beside it.
 *
 * # A drag is a write, made as the reader
 *
 * Dropping a row somewhere else — or `Alt` with Up or Down on a focused row,
 * because a pointer-only reorder is a write a keyboard reader cannot make —
 * sends `set_priorities` as the person signed in (ADR-0024). Three
 * rules make it honest:
 *
 *  - THE WHOLE STORED LIST, NOT THE ROWS. The rows are the open entries and at
 *    most twenty of them; the list the write replaces is the person's own
 *    record, and everything the reader cannot see keeps its place around the
 *    row that moved (`reorder.ts`).
 *  - CONDITIONAL ON THE RECORD'S VERSION (`if_match`). A reorder made from a
 *    screen that read an older order is refused rather than putting that order
 *    back over somebody else's — "Changed by somebody else since you opened
 *    it", and the screen reads the record again.
 *  - CONFIRMED, NOT OPTIMISTIC. The row is drawn at its new place with a
 *    pending mark, and the ENGINE's answer decides: a refusal puts it back and
 *    says why; an applied reorder is read again at the position it landed.
 *
 * AND WHO MAY is the screen's to say (`MyWork.tsx`): your own queue, or — on
 * somebody else's day — a queue in your line. Anything else is held with the
 * sentence that says so, through the same write access every control asks.
 */

import { useEffect, useMemo, useState, type DragEvent, type KeyboardEvent } from "react";
import { EmptyState } from "@crewlethq/ui";
import { RefusalNote } from "~/components/WriteButton.tsx";
import { WorkRow, type RowChrome } from "~/components/work.tsx";
import { useAct } from "~/lib/useAct.ts";
import { plural } from "~/lib/format.ts";
import type { WriteAccess } from "~/lib/useWriteAccess.ts";
import type { WorkSummary } from "~/protocol/index.ts";
import { dropPlace, moveTo, sameOrder, stepPlace, type Place } from "./reorder.ts";

/** The person's own record, as far as a reorder needs it. */
export interface StoredOrder {
  /** The whole stored list, ids, in its order — `work_person.priorities`. */
  ids: readonly string[];
  /** The record's version — what the write is conditional on. */
  version: number;
}

export function PriorityQueue({
  rows,
  stored,
  whose,
  they,
  theirs,
  total,
  now,
  chrome,
  hrefOf,
}: {
  /** The open entries, at most a page, in the stored order (`work_my_work`). */
  rows: readonly WorkSummary[];
  /** The record the write replaces, or undefined while it has not answered. */
  stored: StoredOrder | undefined;
  /** Whose queue this is, by handle — what the write names. */
  whose: string;
  /** "you" on the reader's own day, "them" on anybody else's. */
  they: string;
  /** The person's name on somebody else's day, and absent on the reader's own. */
  theirs?: string;
  /** How many open entries the list holds in full, where the engine said. */
  total?: number;
  now: number;
  chrome: RowChrome;
  hrefOf: (row: WorkSummary) => string;
}) {
  const write = useAct("set_priorities");
  // THE MOVE IN FLIGHT, drawn until the engine answers — and, once it has
  // answered with a landed write, until the re-read it fired arrives with the
  // row where it now is. Held against the rows it was drawn over, so the first
  // new answer is what retires it rather than a timer that could beat it.
  const [moving, setMoving] = useState<{
    id: string;
    place: Place;
    settled: boolean;
    against: readonly WorkSummary[];
  } | null>(null);
  const [dragging, setDragging] = useState("");
  // WHERE A DROP WOULD LAND: above this row, "" after the last one, and null
  // while nothing is being dragged over the list.
  const [over, setOver] = useState<string | null>(null);

  useEffect(() => {
    if (moving?.settled && moving.against !== rows) setMoving(null);
  }, [rows, moving]);

  const ids = useMemo(() => rows.map((r) => r.id), [rows]);
  // THE ROWS AS THEY ARE DRAWN: the stored order, with a move in flight laid
  // over it at the place it was dropped.
  const drawn = useMemo(() => {
    if (!moving) return rows;
    const order = moveTo(ids, moving.id, moving.place);
    if (!order) return rows;
    const byID = new Map(rows.map((r) => [r.id, r]));
    return order.map((id) => byID.get(id)!);
  }, [rows, ids, moving]);
  const drawnIDs = useMemo(() => drawn.map((r) => r.id), [drawn]);

  if (rows.length === 0) {
    return (
      <EmptyState
        size="compact"
        title={`Nothing is at the top of ${they === "you" ? "your" : "their"} list`}
        description={
          they === "you"
            ? "Your priorities are the tasks you mean to do first, in order. Your assistant puts them in place with set_priorities, and so can somebody you report to — once they are, this is where you reorder them."
            : "Their priorities are the tasks they mean to do first, in order — set by them, or by somebody in their line. Nothing is on the list yet."
        }
      />
    );
  }

  const blocked = holdOf(write.access, stored, rows.length);
  // WHETHER THIS READER MAY REORDER THE LIST, and whether a row can be lifted
  // RIGHT NOW. Two facts: a move in flight holds the next one back, but the
  // list is still one this reader rearranges — so its rows keep their grips
  // and its place track keeps its width. Gated on the second alone, every
  // grip left the rows while the write was out and the whole list stepped
  // 18px left, then back when it landed.
  const reorderable = blocked === "";
  const canMove = reorderable && !write.busy;

  const send = async (id: string, place: Place | null) => {
    if (!place || !stored || !canMove) return;
    const next = moveTo(stored.ids, id, place);
    if (!next || sameOrder(next, stored.ids)) return;
    const row = rows.find((r) => r.id === id);
    const at = (moveTo(ids, id, place) ?? ids).indexOf(id) + 1;
    setMoving({ id, place, settled: false, against: rows });
    const result = await write.run(
      { handle: whose, items: next, if_match: stored.version },
      { done: `Moved ${row?.key ?? "the task"} to place ${at}` },
    );
    if (!result || result.kind === "refused" || result.kind === "unknown") {
      // PUT BACK: the engine refused it, or nobody can say it landed —
      // either way what is drawn is the order the screen last read, and the
      // refusal (or the toast) says why.
      setMoving(null);
      return;
    }
    setMoving((prev) => (prev ? { ...prev, settled: true } : prev));
  };

  const dragProps = (row: WorkSummary) =>
    reorderable
      ? {
          draggable: canMove,
          "aria-describedby": "me-priorities-move-hint",
          onDragStart: (e: DragEvent<HTMLAnchorElement>) => {
            if (!canMove) return;
            e.dataTransfer.effectAllowed = "move";
            e.dataTransfer.setData("text/plain", row.id);
            setDragging(row.id);
          },
          onDragEnd: () => {
            setDragging("");
            setOver(null);
          },
          onKeyDown: (e: KeyboardEvent<HTMLAnchorElement>) => {
            if (!e.altKey || (e.key !== "ArrowUp" && e.key !== "ArrowDown")) return;
            e.preventDefault();
            void send(row.id, stepPlace(drawnIDs, row.id, e.key === "ArrowUp"));
          },
        }
      : undefined;

  /**
   * Which row the pointer is above, by halves — or "" below the last one.
   *
   * THE ROWS ARE THE LIST'S OWN CHILDREN, IN DRAWN ORDER, and are found as
   * that rather than through a wrapper carrying an id: a row is a SUBGRID of
   * the list's tracks, and any element between the two takes the tracks away
   * from it.
   */
  const overRows = (e: DragEvent<HTMLElement>) => {
    if (!dragging) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = "move";
    const cells = [...e.currentTarget.querySelectorAll<HTMLElement>(":scope > .work-row")];
    const below = cells.findIndex((el) => {
      const box = el.getBoundingClientRect();
      return e.clientY < box.top + box.height / 2;
    });
    const above = below >= 0 ? (drawn[below]?.id ?? "") : "";
    if (over !== above) setOver(above);
  };

  const hidden = typeof total === "number" && total > rows.length ? total - rows.length : 0;
  const last = drawn.at(-1)?.id;
  return (
    <div className="col gap-2">
      {/* WHAT THIS LIST COUNTS, before its first row — see [listLine]. */}
      <p className="t-caption">{listLine({ rows, total, whose, they })}</p>
      {reorderable && (
        <span id="me-priorities-move-hint" className="sr-only">
          Drag to change its place, or press Alt with the up or down arrow.
        </span>
      )}
      {/* WHY THE ROWS DO NOT MOVE, said once above them — a drag is a write
          with no button to disable, and rows that will not lift read as a
          list that is broken. Nothing where they do. */}
      {blocked && <p className="t-caption work-write-note">{`Reordering is off. ${blocked}`}</p>}
      {/* AND WHAT A LEAD'S REORDER DOES, said before it is made: the engine
          stamps the queue with who ordered it and wakes the person to take it
          up, so a reorder of somebody else's list is an instruction to them
          rather than a private arrangement. */}
      {!blocked && theirs && (
        <p className="t-caption work-write-note">
          {`A reorder here is stamped on ${theirs}’s queue with your name, and ${theirs} is told what is now first.`}
        </p>
      )}
      <RefusalNote write={write} />
      <div className="work-list">
        <div
          className="work-rows"
          data-ordinals="true"
          data-reorder={reorderable ? "true" : undefined}
          data-dragging={dragging ? "true" : undefined}
          onDragOver={overRows}
          onDragLeave={(e) => {
            if (!e.currentTarget.contains(e.relatedTarget as Node | null)) setOver(null);
          }}
          onDrop={(e) => {
            e.preventDefault();
            const id = e.dataTransfer.getData("text/plain") || dragging;
            const target = over;
            setDragging("");
            setOver(null);
            // NOWHERE MEASURED IS NOWHERE TO GO: a drop with no drag-over
            // before it (a file dropped in from outside) moves nothing.
            if (!id || target === null) return;
            void send(id, dropPlace(drawnIDs, id, target === "" ? null : target));
          }}
        >
          {drawn.map((row, at) => (
            <WorkRow
              key={row.id}
              row={row}
              now={now}
              chrome={chrome}
              href={hrefOf(row)}
              ordinal={at + 1}
              drag={dragProps(row)}
              pending={moving?.id === row.id && !moving.settled}
              drop={
                dragging && over === row.id && row.id !== dragging
                  ? "above"
                  : dragging && over === "" && row.id === last && row.id !== dragging
                    ? "below"
                    : undefined
              }
            />
          ))}
        </div>
      </div>
      {/* A PAGE IS A PAGE: the rows are the open entries up to the engine's
          block bound, and the rest of the list keeps its places around any row
          moved here — `reorder.ts`. */}
      {hidden > 0 && (
        <p className="t-caption">
          {`The first ${plural(rows.length, "open entry", "open entries")} of ${total}. The other ${hidden} keep their places when one of these moves.`}
        </p>
      )}
    </div>
  );
}

/**
 * What the priorities list holds, and how it differs from the Queue's count.
 *
 * THE LIST IS NOT THE QUEUE. It sits under the Queue tab, as the order the
 * queue is to be worked in, but it is a list somebody WROTE — the person, or a
 * lead for them — and it can name any task: one a colleague holds that this
 * person has to see through, as readily as one of their own. The Queue's count
 * is the work ASSIGNED to them. So the tab said 2, the list drew 5, the
 * sidebar said something else again, and nothing on the screen said which
 * figure counted what. This line says it, with the split drawn from the rows
 * themselves: how many of them are assigned to this person — the only ones the
 * Queue's count takes in — rather than who holds the rest, which each row's
 * own avatar already says (and "somebody else" would be wrong about a task
 * nobody holds).
 *
 * THE SPLIT IS OVER WHAT IS DRAWN. The rows are a page of at most twenty open
 * entries under an exact total, and whose each hidden entry is is not on the
 * wire — so past a page the split says "of the N shown" rather than claiming
 * the whole list.
 */
export function listLine({
  rows,
  total,
  whose,
  they,
}: {
  rows: readonly WorkSummary[];
  total?: number;
  whose: string;
  they: string;
}): string {
  const all = typeof total === "number" && total >= rows.length ? total : rows.length;
  const head = `${plural(all, "open task")} on ${they === "you" ? "your" : "their"} list, in the order to work them`;
  const held = rows.filter((r) => r.assignee === whose).length;
  const paged = rows.length < all;
  if (held === rows.length && !paged) {
    return `${head} — ${all === 1 ? "it is" : "all"} assigned to ${they}.`;
  }
  const split = paged
    ? `${held} of the ${rows.length} shown ${held === 1 ? "is" : "are"} assigned to ${they}`
    : held === 0
      ? `none assigned to ${they}`
      : `${held} of them assigned to ${they}`;
  const queue = held === rows.length ? "" : ` The Queue counts only the work assigned to ${they}.`;
  return paged ? `${head}. ${capitalize(split)}.${queue}` : `${head} — ${split}.${queue}`;
}

function capitalize(s: string): string {
  return s.charAt(0).toUpperCase() + s.slice(1);
}

/**
 * Why a reorder cannot be made here, or "" where it can.
 *
 * THE WRITE'S OWN ACCESS FIRST — offline, nobody signed in, a token bound to
 * nobody, somebody else's queue — because that is the sentence every other
 * control on the screen is showing. Then the two this list adds: the record
 * the write is conditional on has not answered, and a list of one has no
 * order to change.
 */
export function holdOf(
  access: WriteAccess,
  stored: StoredOrder | undefined,
  count: number,
): string {
  if (!access.can) return access.reason;
  if (!stored) return "Reading the order as it is stored before it can be changed.";
  if (count < 2) return "One task has no order to change.";
  return "";
}
