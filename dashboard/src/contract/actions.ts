/**
 * Every change the dashboard makes, as the tool the engine runs for it.
 *
 * The dashboard writes through ONE route — `POST /operator/act/{tool}`, as the
 * person the presented token is bound to (ADR-0024) — and the tools behind it
 * are the operator catalogue's, the same implementations a person's own
 * assistant calls. This table is what the dashboard is allowed to send there:
 * `protocol/act.ts` takes only a key of it, and only the arguments its row
 * names.
 *
 * ONE ROW PER TOOL A CONTROL PRESSES, and no other: `app/source.test.ts` holds
 * the keys to the `useAct("…")` literals outside the suites, both ways, so a
 * row lands with the control that sends it rather than ahead of it.
 *
 * Each row is:
 *
 *  - `args` — the arguments a screen may pass. Every one is a property of the
 *    tool's own schema, and every property the schema REQUIRES is here;
 *  - `domain` — the log the write lands in, whose position raises this tab's
 *    read floor for that domain (`protocol/session.ts`), or `null` for a
 *    write that lands in no log a question reads;
 *  - `refreshes` — questions OUTSIDE that domain's session set
 *    (`contract/domains.ts`) that the write also moves, asked again without a
 *    floor because they cannot take one;
 *  - `scope` — `person` for a change any bound person makes to the company's
 *    work in their own name, `company` for one that rewrites what every seat's
 *    tracker means (a project's settings, the catalogue), which a screen
 *    offers only where it says so.
 *
 * HELD AGAINST THE REAL CATALOGUE by `internal/api/operator`'s
 * `TestEveryActionTheDashboardTakesIsOneTheActTransportServes`: every tool is
 * served by the act transport and is not a read, every argument is one its
 * schema takes, every required one is present, and every scope is the
 * engine's. A renamed argument in Go is a red build here rather than a button
 * that is refused `invalid` the first time somebody presses it.
 */
export const ACTIONS = {
  update_work_item: {
    args: ["item", "assignee", "reason", "status", "priority", "if_match"],
    domain: "tracker",
    refreshes: ["work_search"],
    scope: "person",
  },
  restore_work_item: {
    args: ["item"],
    domain: "tracker",
    refreshes: ["work_search"],
    scope: "person",
  },
  set_pins: {
    args: ["views", "favorites"],
    domain: "tracker",
    refreshes: [],
    scope: "person",
  },
  mark_inbox: {
    args: ["read", "unread", "snooze", "unsnooze", "read_through"],
    domain: "tracker",
    refreshes: [],
    scope: "person",
  },
} as const;
