/**
 * Who set each property, from the object's own change log.
 *
 * # The one thing this product can say that a tracker cannot
 *
 * Every write in this engine is attributed to somebody — a seat, a person, an
 * operator token — and carries the turn it happened in. So a properties rail
 * here can say `set by ada · 3h ago · turn ↗` on the row, where Jira, Linear
 * and ClickUp can only put an actor on a feed entry and leave the reader to
 * match it up. `ObjectHeader.Fact` and `PropertiesRail.Property` were both
 * built to render that line and NOTHING EVER PASSED ONE: the field was
 * declared, the markup was written, the class was in the stylesheet, and the
 * producer was never written — so the product's own differentiator rendered on
 * zero rows.
 *
 * # It is a derivation, not a wire field
 *
 * The engine already answers it. `work_item` returns `history[]`, each entry
 * carrying `actor`, `actor_kind`, `turn_id`, `at` and a `fields` bag keyed on
 * the property names the rail renders — `status`, `assignee`, `due`, `points`,
 * `priority`, `start`, `title`, `type` — and the item screen already reads it
 * for the History tab. So this is a walk over data the page has in hand, not a
 * query, and it costs nothing to render.
 *
 * # The window is why an absent answer is an answer
 *
 * `history` is the newest `DetailHistoryDefault` (50) changes, `log_seq DESC`.
 * A property whose last change fell out of that window has no attribution HERE
 * — and the honest response is no line at all. Naming the oldest entry in the
 * window would read as "ada set this" when what happened is "ada made the
 * oldest change we can still see", which is a fact about the page size.
 *
 * That is also why a `created` entry attributes every field it carries: a
 * create genuinely sets them, and for most tasks it is inside the window,
 * which is what makes the line appear at all on a task nobody has edited.
 */

import type { SetBy } from "~/app/frame/ObjectHeader.tsx";
import type { WorkChange } from "~/protocol/index.ts";

/** The author kinds the wire carries, as `SetBy` spells them. */
const KINDS = new Set(["agent", "human", "operator", "system"]);

/**
 * The tracker's own field names, exactly as `tracker.TaskDeltas` writes them
 * into a history row's `fields` bag.
 *
 * THE NAMES ARE THE ENGINE'S, NOT THE RAIL'S. A property is labelled "Due" and
 * recorded as `due`; "Estimate" is `estimate` and not `estimate_minutes`, which
 * is what the task row calls the same number. A key that names no field
 * produces no attribution and no error — the line simply never appears — so
 * the list is declared once here, the type makes a typo a compile failure, and
 * `internal/tracker/attribution_test.go` holds it against `TaskDeltas` so a
 * field added or renamed in Go cannot leave this silently short.
 */
export const CHANGE_FIELDS = [
  "title",
  "status",
  "assignee",
  "priority",
  "project",
  "type",
  "tags",
  "due",
  "start",
  "estimate",
  "points",
] as const;

export type ChangeField = (typeof CHANGE_FIELDS)[number];

/**
 * The latest change to each named property, keyed on the property's own name.
 *
 * NEWEST FIRST IS THE INPUT ORDER, so the first entry naming a field wins and
 * nothing has to compare instants. The server orders on `log_seq`, which is a
 * position on one stream rather than a clock, so it is the ordering that holds
 * when two nodes wrote in the same second.
 */
export function attribution(history: readonly WorkChange[] | undefined): Map<string, SetBy> {
  const out = new Map<string, SetBy>();
  for (const change of history ?? []) {
    // A change with no actor attributes nothing: the line's whole content is
    // who, and "set by" with nobody after it is worse than silence.
    if (!change.actor) continue;
    for (const field of Object.keys(change.fields ?? {})) {
      if (out.has(field)) continue;
      out.set(field, {
        actor: change.actor,
        actorKind: KINDS.has(change.actor_kind ?? "")
          ? (change.actor_kind as SetBy["actorKind"])
          : undefined,
        turnId: change.turn_id || undefined,
        at: change.at,
      });
    }
  }
  return out;
}
