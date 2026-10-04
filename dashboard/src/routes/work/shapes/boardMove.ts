/**
 * A board drag, as values: where a card was dropped, what `place_work_item` is
 * sent for it, and how the board is drawn while the engine decides.
 *
 * PURE, over the answer's own lanes, for the reason `lib/work.ts` gives for its
 * own arithmetic: a rule that can only be exercised by dragging a card in a
 * browser is a rule nobody re-measures, and every judgement here has a wrong
 * form that moves a different task — a neighbour from another project, a lane
 * change on an axis that is not the status, a place computed from a card that
 * is itself the one being moved.
 *
 * # Neighbours, never positions
 *
 * The engine mints the card's new rank inside its own write, between the
 * neighbour it was dropped beside and the one beyond it, as the board stands
 * when the move lands (`internal/agent/builtin/workplace.go`). So a drop names
 * ONE neighbour — `before` the card it went above, or `after` the last card of
 * a lane it went to the bottom of — and never an index, which would be a claim
 * about a board somebody else may have rearranged a second ago.
 */

import type { WorkGroup, WorkSummary } from "~/protocol/index.ts";

/** Where a card was let go: the lane, and the card it went above, if any. */
export interface Drop {
  /** The card being moved. */
  key: string;
  /** The lane it was dropped into, by the lane's own key. */
  lane: string;
  /**
   * The card it was dropped directly ABOVE, or "" for the bottom of the lane.
   * Never the moved card itself.
   */
  above: string;
}

/** The arguments `place_work_item` is sent for a drop, or null for a drop that moves nothing. */
export interface MoveArgs {
  item: string;
  before?: string;
  after?: string;
  status?: string;
  if_match: number;
}

/**
 * What a drop asks the engine to do, or null where it asks nothing.
 *
 * THE LANE CHANGES ONLY ON THE STATUS AXIS. `place_work_item`'s lane is a
 * status (`status=`), so a card dragged between two ASSIGNEE lanes would be a
 * reassignment the tool does not make; such a drop reorders within its own
 * lane or moves nothing. A board on any other axis is still reorderable, lane
 * by lane.
 *
 * THE NEIGHBOUR MUST SHARE THE CARD'S PROJECT, because a rank is an order
 * within one project's board: on the company-wide board a lane mixes projects,
 * and a card from another one is no neighbour. With none left, a lane change
 * is still a status change, and a reorder is nothing.
 */
export function moveFor(drop: Drop, groups: readonly WorkGroup[], axis: string): MoveArgs | null {
  const from = groups.find((g) => g.rows.some((r) => r.key === drop.key));
  const card = from?.rows.find((r) => r.key === drop.key);
  const to = groups.find((g) => g.key === drop.lane);
  if (!from || !card || !to) return null;
  const crossing = from.key !== to.key;
  if (crossing && axis !== "status") return null;

  const lane = to.rows.filter((r) => r.key !== card.key && r.project === card.project);
  const out: MoveArgs = { item: card.key, if_match: card.version };
  if (crossing) out.status = to.key;

  const above = drop.above ? lane.find((r) => r.key === drop.above) : undefined;
  if (above) {
    out.before = above.key;
  } else if (drop.above && !crossing) {
    // DROPPED ABOVE A CARD OF ANOTHER PROJECT: no neighbour this card can be
    // ranked against, and within its own lane nothing else changes.
    return null;
  } else if (!drop.above && lane.length > 0) {
    out.after = lane[lane.length - 1]!.key;
  }

  // A DROP WHERE IT ALREADY WAS moves nothing, and the engine would say so as
  // a refusal ("as sent it moves nothing") — so it is not sent.
  if (!crossing) {
    const rows = from.rows.filter((r) => r.project === card.project);
    const at = rows.findIndex((r) => r.key === card.key);
    if (out.before && rows[at + 1]?.key === out.before) return null;
    if (out.after && rows[at - 1]?.key === out.after && at === rows.length - 1) return null;
    if (!out.before && !out.after) return null;
  }
  return out;
}

/**
 * The drop a KEYBOARD move is: `Alt` with an arrow, on a focused card.
 *
 * Up and down step past the neighbour of the same project in the same lane;
 * left and right carry the card to the top of the lane beside it. The same [moveFor] decides what
 * it sends, so a key and a pointer are one gesture with two inputs.
 */
export function keyboardDrop(
  key: string,
  groups: readonly WorkGroup[],
  direction: "up" | "down" | "left" | "right",
): Drop | null {
  const at = groups.findIndex((g) => g.rows.some((r) => r.key === key));
  if (at < 0) return null;
  const lane = groups[at]!;
  // UP AND DOWN STEP OVER THE CARD'S OWN PROJECT, because a rank is an order
  // within one project ([moveFor]): on the company's board a lane interleaves
  // projects, and a step onto a card of another one would be a move with no
  // neighbour to place it against — a key press that did nothing.
  const project = lane.rows.find((r) => r.key === key)?.project;
  const own = lane.rows.filter((r) => r.project === project);
  const i = own.findIndex((r) => r.key === key);
  if (direction === "up") {
    const prev = own[i - 1];
    return prev ? { key, lane: lane.key, above: prev.key } : null;
  }
  if (direction === "down") {
    const next = own[i + 1];
    if (!next) return null;
    return { key, lane: lane.key, above: own[i + 2]?.key ?? "" };
  }
  const beside = groups[direction === "left" ? at - 1 : at + 1];
  if (!beside) return null;
  return { key, lane: beside.key, above: beside.rows[0]?.key ?? "" };
}

/**
 * The lanes as they are drawn while a move is in flight: the card lifted out
 * of its lane and set where it was dropped.
 *
 * DRAWN, NOT ASSUMED. The card carries a pending mark until the engine
 * answers, and the answer — never this — is what the board is redrawn from:
 * a refusal puts it back where it was, and an applied move is re-read at the
 * position the write landed.
 *
 * THE COUNTS TRAVEL WITH THE CARD. A lane's count is the engine's number for
 * the whole lane, and moving one card out of it and into another changes both
 * numbers by exactly one — whatever the lane holds beyond its loaded slice. A
 * card drawn in In progress under a heading still reading the old tally is two
 * claims on one screen that disagree, and the tally then jumping a second later
 * reads as a second change nobody made. Because this is a function of the
 * drop, a refusal that clears the drop restores both numbers with the card.
 */
export function withDrop(groups: readonly WorkGroup[], drop: Drop | null): WorkGroup[] {
  if (!drop) return groups as WorkGroup[];
  let card: WorkSummary | undefined;
  for (const g of groups) card ??= g.rows.find((r) => r.key === drop.key);
  if (!card) return groups as WorkGroup[];
  const moved = card;
  const from = groups.find((g) => g.rows.some((r) => r.key === drop.key))!.key;
  const crossing = from !== drop.lane;
  return groups.map((g) => {
    const rows = g.rows.filter((r) => r.key !== drop.key);
    if (g.key !== drop.lane) {
      if (rows.length === g.rows.length) return g;
      return { ...g, rows, count: crossing ? Math.max(0, g.count - 1) : g.count };
    }
    const at = drop.above ? rows.findIndex((r) => r.key === drop.above) : -1;
    const next = [...rows];
    next.splice(at < 0 ? next.length : at, 0, moved);
    return { ...g, rows: next, count: crossing ? g.count + 1 : g.count };
  });
}
