/**
 * The tracker's own field names, exactly as `tracker.TaskDeltas` writes them
 * into a history row's `fields` bag.
 *
 * THE NAMES ARE THE ENGINE'S, NOT THE RAIL'S. A property is labelled "Due" and
 * recorded as `due`; "Estimate" is `estimate` and not `estimate_minutes`, which
 * is what the task row calls the same number; "Watching" is `watchers` and
 * "Routes to" is `routing_unit`. A key that names no field produces no
 * attribution and no error — the line simply never appears — so the list is
 * declared once here, `lib/attribution.ts`'s `ChangeField` makes a typo a
 * compile failure, and `internal/tracker/attribution_test.go` holds it against
 * `TaskDeltas` so a field added or renamed in Go cannot leave this silently
 * short.
 *
 * ONE NAME PER ROW THE RAIL DRAWS, and no more: the four people-and-routing
 * names below joined the list when `TaskDeltas` started comparing them, which
 * is what turned "who added this watcher" from unanswerable into a walk over
 * the log the page already holds. A delta the rail has no row for — `parent`,
 * `archived`, `body`, the checklists — is deliberately absent rather than
 * declared unused, because the gate reports the engine's unattributed fields
 * and a name here with nowhere to render is a row somebody thinks exists.
 */
export const CHANGE_FIELDS = [
  "title",
  "status",
  "assignee",
  "reporter",
  "collaborators",
  "watchers",
  "priority",
  "project",
  "type",
  "tags",
  "due",
  "start",
  "estimate",
  "points",
  "routing_unit",
] as const;
