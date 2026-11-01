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
 *
 * A QUESTION READ FROM SEVERAL LOGS IS LISTED UNDER EACH — the company feed is
 * the tracker's work and the knowledge base's pages — and its read names one
 * `min_position` per listed domain this tab has written (`protocol/session.ts`),
 * which the engine holds each half of the answer to. Listed under one, the
 * half on the other log would be drawn from before the tab's own write there.
 *
 * `code_search` IS DELIBERATELY ABSENT: it answers at the tip it names — each
 * repository searched at the commit its git node holds, named in the answer's
 * `tips`, and a hit's file read at that commit — so it takes no floor, and the
 * engine refuses `read_level=session` and `min_position` on it
 * (`internal/api/queries.TestCodeSearchIsNotASessionQuery`). Listed here, a
 * code write would scatter a search to every git node only to be refused.
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
    "inbox",
  ],
  pages: ["pages", "page", "containers", "page_activity", "page_revision", "company_feed"],
  code: [
    "code_repos",
    "code_repo",
    "code_pushes",
    "code_changes",
    "code_change",
    "code_change_routing",
    "code_reviews",
    "code_refs",
    "code_tree",
    "code_blob",
    "code_log",
    "code_blame",
    "code_diff",
    "code_compare",
    "code_commit",
    "code_queue",
    "code_checks",
    "code_check",
    "code_check_log",
    "inbox",
  ],
} as const;
