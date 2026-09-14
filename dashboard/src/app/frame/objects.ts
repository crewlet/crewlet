/**
 * What an object IS to the frame: how it is addressed, what it is called, and
 * how to get to its page.
 *
 * # One address grammar for every kind
 *
 * `peek={kind}:{id}` is the frame's own query key, and it is the only way the
 * detail rail is opened. One key, one component, one rule — a plain click
 * peeks, a modifier-click opens the page — so a list of any kind can open a
 * peek on any other kind without either knowing about the other. The tracker
 * had `item=` and nothing else did, which is why a board could peek a task and
 * nothing in the product could peek a seat, a node or a turn.
 *
 * # The id is opaque to the frame
 *
 * A work item is a key or a uuid, a seat is a handle, a sprint is
 * `{KEY}/{n}`, a page is `{CONTAINER}/{Title}`. The frame parses only the
 * FIRST colon, so every one of those survives being carried in a query value —
 * and each kind's own `pathOf` is what turns it back into a route.
 */

export type ObjectKind =
  | "item"
  | "project"
  | "sprint"
  | "goal"
  | "seat"
  | "unit"
  | "page"
  | "container"
  | "turn"
  | "run"
  | "event"
  | "node"
  | "schedule"
  | "channel"
  | "tool"
  | "integration"
  | "credential"
  | "revision"
  | "notice";

export interface ObjectRef {
  kind: ObjectKind;
  id: string;
}

/** The `peek=` value for one object. */
export function refToken(ref: ObjectRef): string {
  return `${ref.kind}:${ref.id}`;
}

/**
 * Read a `peek=` value, or null.
 *
 * SPLIT ON THE FIRST COLON ONLY. A sprint is `sprint:ENG/3` and a page is
 * `page:ENG/Deploy runbook` — an id carrying its own separators is the common
 * case rather than the exception, and splitting on every colon would make a
 * title containing one unaddressable.
 */
export function parseRef(token: string | null | undefined): ObjectRef | null {
  if (!token) return null;
  const cut = token.indexOf(":");
  if (cut <= 0) return null;
  const kind = token.slice(0, cut) as ObjectKind;
  const id = token.slice(cut + 1);
  if (!id || !KINDS[kind]) return null;
  return { kind, id };
}

export interface KindSpec {
  /** What this kind is called in a header eyebrow and a palette row. */
  label: string;
  /** The object's own page. */
  pathOf: (id: string) => string[];
  /** Ids that carry their own separators, split for the route. */
  mono?: boolean;
}

/**
 * Every kind the frame can address.
 *
 * A kind missing from here cannot be peeked, which is deliberate: `parseRef`
 * refuses an unknown one rather than opening an empty rail, so a hand-edited
 * URL lands on the page with the rail closed instead of on a spinner that
 * never resolves.
 */
export const KINDS: Record<ObjectKind, KindSpec> = {
  item: { label: "Item", pathOf: (id) => ["work", id], mono: true },
  project: { label: "Project", pathOf: (id) => ["work", id], mono: true },
  sprint: {
    label: "Sprint",
    // `ENG/3` → `#/work/ENG/sprints/3`.
    pathOf: (id) => {
      const [key, number] = id.split("/");
      return ["work", key ?? "", "sprints", number ?? ""];
    },
    mono: true,
  },
  goal: { label: "Goal", pathOf: (id) => ["goals", id] },
  seat: { label: "Seat", pathOf: (id) => ["company", "people", id], mono: true },
  unit: { label: "Unit", pathOf: (id) => ["company", "units", id] },
  page: {
    // `ENG/Deploy runbook` → `#/knowledge/ENG/Deploy runbook`, each segment
    // encoded by the router.
    label: "Page",
    pathOf: (id) => {
      const cut = id.indexOf("/");
      if (cut < 0) return ["knowledge", id];
      return ["knowledge", id.slice(0, cut), id.slice(cut + 1)];
    },
  },
  container: { label: "Container", pathOf: (id) => ["knowledge", id], mono: true },
  turn: { label: "Turn", pathOf: (id) => ["activity", "turns", id], mono: true },
  run: { label: "Coding run", pathOf: (id) => ["activity", "runs", id], mono: true },
  event: { label: "Event", pathOf: (id) => ["activity", "events", id], mono: true },
  node: { label: "Node", pathOf: (id) => ["admin", "fleet", id], mono: true },
  schedule: {
    // `role/ceo/standup` — three segments, and the name may contain none of
    // them because the engine slugs it.
    label: "Schedule",
    pathOf: (id) => ["activity", "schedules", ...id.split("/")],
  },
  channel: { label: "Channel", pathOf: (id) => ["activity", "a2a", id], mono: true },
  tool: { label: "Tool", pathOf: (id) => ["admin", "tools", id], mono: true },
  integration: { label: "Integration", pathOf: (id) => ["admin", "integrations", id] },
  credential: { label: "Credential", pathOf: (id) => ["admin", "credentials", id], mono: true },
  revision: { label: "Revision", pathOf: (id) => ["admin", "config", "revisions", id], mono: true },
  // A NOTICE HAS NO PAGE OF ITS OWN. It is a row in somebody's inbox naming a
  // change to something else, so its "page" is the inbox that holds it — and
  // the peek is where the notice itself is read.
  notice: { label: "Notice", pathOf: () => ["inbox"] },
};

/** The route for an object. */
export function pathOf(ref: ObjectRef): string[] {
  return KINDS[ref.kind].pathOf(ref.id);
}
