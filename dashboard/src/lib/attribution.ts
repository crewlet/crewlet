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
 * the tracker's own field names — every one [CHANGE_FIELDS] names — and
 * the item screen already reads it for the History tab. So this is a walk over
 * data the page has in hand, not a query, and it costs nothing to render.
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
import { CHANGE_FIELDS } from "~/contract/attribution.ts";

/** The author kinds the wire carries, as `SetBy` spells them. */
const KINDS = new Set(["agent", "human", "operator", "system"]);

/** One of the names [CHANGE_FIELDS] declares — a typo is a compile failure. */
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
    for (const [field, delta] of Object.entries(change.fields ?? {})) {
      if (out.has(field)) continue;
      out.set(field, {
        actor: change.actor,
        actorKind: KINDS.has(change.actor_kind ?? "")
          ? (change.actor_kind as SetBy["actorKind"])
          : undefined,
        turnId: change.turn_id || undefined,
        at: change.at,
        cleared: emptied(delta),
      });
    }
  }
  return out;
}

/**
 * Whether this change took the field's value AWAY.
 *
 * THE ONE THING THAT LICENSES PROVENANCE UNDER A BLANK. A rail draws no "set
 * by" under a value that is not there — it would claim a record of somebody
 * setting nothing — but a change that EMPTIED the field is a record the log
 * genuinely holds, and it reads "cleared by". Without this the two are
 * indistinguishable here and the honest rail has to drop both.
 *
 * `tracker.Delta` carries `From` and `To` with no `omitempty`
 * (`internal/tracker/mutation.go`), so every delta this engine has written has
 * both keys and an emptied field is `to: ""`. The defensive arm below is for a
 * payload this build cannot type-check, which is a smaller claim: a delta
 * whose shape is unreadable says nothing about a clearing either way.
 */
function emptied(delta: unknown): boolean {
  if (!delta || typeof delta !== "object") return false;
  if (!("to" in delta)) return false;
  const to = (delta as { to?: unknown }).to;
  return to === "" || to === null || to === undefined;
}
