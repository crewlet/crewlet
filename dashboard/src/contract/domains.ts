/**
 * Which questions a write can be read back through.
 *
 * A write through `/operator/act` answers with the POSITION its record landed
 * at in its domain's log, and a read that names that position as its floor
 * (`read_level=session&min_position=…`) waits until this node has applied it —
 * so the screen that pressed the button never shows the state from before the
 * press. That is only true of a question that READS a floor: one that ignores
 * the key answers from whatever this node holds, and a refetch of it after a
 * write is the optimistic guess this dashboard refuses to make.
 *
 * So the list is the ENGINE'S, per domain, and held both ways by
 * `internal/api/queries`' `TestEverySessionQueryTakesAFreshnessFloor`: every
 * kind here is asked there with a bare `read_level=session` (refused — no
 * floor named) and with a floor (served at `session`), and every question the
 * engine serves at a floor is listed here under the domain whose log it
 * reads. A kind missing from here is a screen that stays stale after its own
 * write; a kind listed that takes no floor is a screen that believes it waited
 * and did not.
 */
export const SESSION_QUERIES = {
  tracker: [
    "work_items",
    "work_item",
    "work_comments",
    "work_item_turns",
    "work_views",
    "work_saved_views",
    "work_catalogue",
    "work_person",
    "work_projects",
    "work_project",
    "work_workload",
    "work_activity",
    "work_my_work",
    "work_inbox",
    "work_routing",
    "work_flow",
    "company_feed",
    "decisions",
  ],
  pages: ["pages", "page", "containers", "page_activity", "page_revision"],
} as const;
