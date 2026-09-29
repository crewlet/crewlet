/**
 * The route table, as ONE pure function from a path to what it addresses.
 *
 * # Why a resolver rather than a switch in the component tree
 *
 * Five things need to know whether a path is a page and which one: the screen
 * dispatch, the breadcrumb, the recents and stars a reader keeps (a stored
 * path that stopped resolving must be dropped rather than drawn as a row that
 * leads to Not Found), the palette's "Go to", and the tests that hold the
 * design doc's route table against the code. The dispatch used to be a nested
 * `switch` in `App.tsx`, which meant the other four each carried an
 * approximation of it — the breadcrumb had its own idea of which tails were
 * valid, and the doc gate had to give up and read three other tables instead,
 * because the switch spelled a route three different ways.
 *
 * So the table is data here, `App.tsx` maps a resolved screen to a component,
 * and everything else asks this function. It imports nothing but `nav.ts`,
 * which is what lets `lib/` and a Node test call it.
 *
 * # What decides a segment
 *
 * Its POSITION and its SHAPE, never a lookup: a route resolves before any
 * answer arrives. Lowercase words are reserved (`nav.ts` `RESERVED_SEGMENTS`);
 * a project or container key is uppercase ([KEY_SHAPE], [CONTAINER_SHAPE]); an
 * item is its key, `KEY-n`, or its id, a uuid ([ID_SHAPE]) — and a segment
 * that is neither is Not Found rather than an item nobody minted. Where two
 * shapes share a segment — a tool page and a tool origin filter — the tail's
 * LENGTH discriminates rather than the word, because a tool name is a third
 * party's string and `servers` is a legal one.
 */

import { RESERVED_SEGMENTS, workspaceOf, type Workspace } from "./nav.ts";

/** A project or container key: uppercase, as the engine mints it. */
export const KEY_SHAPE = /^[A-Z][A-Z0-9_]*$/;
/** An item key: a project key, a dash, a number. */
export const ITEM_KEY_SHAPE = /^[A-Z][A-Z0-9_]*-\d+$/;
/**
 * An id the engine minted: a uuid, as `uuid.UUID` prints it. Every object the
 * engine names by id is one, so a segment in an id's position that is not is
 * nothing the engine could answer for.
 */
export const ID_SHAPE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
/**
 * A knowledge container key: whatever the engine keeps, which is UPPERCASED on
 * every path that writes one (`pages.Store` uppercases the key a page is
 * created in and the key `EnsureContainer` makes), so it never holds a
 * lowercase letter. Looser than [KEY_SHAPE] on purpose — a unit's `space:` may
 * carry a dash or start with a digit — and exactly as strict where it matters:
 * every reserved segment is lowercase, so `#/knowledge/skills` can never be
 * read as a container called `skills`.
 */
export const CONTAINER_SHAPE = /^[^a-z]+$/;

/** My work's sections below its queue. */
export type MeSection =
  | "queue"
  | "asked-of-me"
  | "asked-by-me"
  | "unblocked"
  | "collaborating"
  | "watching"
  | "checklist";

const ME_SECTIONS: readonly MeSection[] = [
  "asked-of-me",
  "asked-by-me",
  "unblocked",
  "collaborating",
  "watching",
  "checklist",
];

/** Every screen the dashboard draws, with what its path names. */
export type Screen =
  | { screen: "home" }
  | { screen: "inbox" }
  | { screen: "me"; section: MeSection }
  | { screen: "work" }
  | { screen: "work-projects" }
  | { screen: "work-views"; id?: string }
  | { screen: "work-history" }
  | { screen: "work-search" }
  | { screen: "project"; key: string }
  | { screen: "item"; id: string }
  | { screen: "org-chart" }
  | { screen: "roster" }
  | { screen: "teams"; unit?: string }
  | { screen: "schedules"; scope: string[] }
  | { screen: "org-edit" }
  | { screen: "seat"; handle: string }
  | { screen: "live" }
  | { screen: "turns" }
  | { screen: "turn"; id: string }
  | { screen: "runs"; id?: string }
  | { screen: "a2a"; id?: string }
  | { screen: "trace"; id: string }
  | { screen: "events" }
  | { screen: "event"; id: string }
  | { screen: "knowledge" }
  | { screen: "container"; key: string }
  | { screen: "page"; id: string }
  | { screen: "skills" }
  | { screen: "spend" }
  | { screen: "budgets" }
  | { screen: "general" }
  | { screen: "integrations"; kind?: string }
  | { screen: "tools"; server?: string; tool?: string }
  | { screen: "secrets"; name?: string }
  | { screen: "nodes"; node?: string }
  | { screen: "config"; revisions: boolean; revision?: string }
  | { screen: "backups"; domain?: string }
  | { screen: "audit" };

export type ScreenName = Screen["screen"];

/**
 * A path that addresses a page. The discriminant is `resolved`, deliberately
 * not `kind`: a screen's own parameters are spread beside it, and one of them
 * IS a `kind` (an integration's), which a `kind` discriminant would silently
 * overwrite — `#/settings/integrations/github` would resolve to an
 * integration called "screen".
 */
export type Resolved = Screen & { resolved: true; workspace: Workspace };

/** A path the product has no page for, and a sentence saying which. */
export interface Unresolved {
  resolved: false;
  /** The workspace the head names, or "" when it names none. */
  workspace: Workspace | "";
  /** What was asked for, in words: `“views/x/y” under Work`. */
  what: string;
  /** Why there is nothing here, where the product knows. */
  hint?: string;
}

export type Route = Resolved | Unresolved;

function missing(workspace: Workspace | "", what: string, hint?: string): Unresolved {
  return hint ? { resolved: false, workspace, what, hint } : { resolved: false, workspace, what };
}

const reserved = (segment: string): boolean => RESERVED_SEGMENTS.includes(segment);

/** Whether a path addresses a page. */
export function resolves(path: string[]): boolean {
  return resolve(path).resolved;
}

/** What a path addresses. */
export function resolve(path: string[]): Route {
  const [head, ...rest] = path;
  // THE EMPTY FRAGMENT IS HOME, which is the landing screen: what a person
  // opening this wants to know first is whether anything needs them, and Home
  // answers that for the company as well as for them.
  if (head === undefined) return { resolved: true, workspace: "home", screen: "home" };
  const under = (label: string) => `“${rest.join("/")}” under ${label}`;
  const screen = (s: Screen): Resolved => ({
    ...s,
    resolved: true,
    workspace: workspaceOf(path) as Workspace,
  });
  switch (head) {
    case "home":
      return rest.length ? missing("home", under("Home")) : screen({ screen: "home" });
    case "inbox":
      return rest.length ? missing("inbox", under("Inbox")) : screen({ screen: "inbox" });
    case "me": {
      if (rest.length === 0) return screen({ screen: "me", section: "queue" });
      const [section] = rest;
      if (rest.length === 1 && ME_SECTIONS.includes(section as MeSection)) {
        return screen({ screen: "me", section: section as MeSection });
      }
      return missing("me", under("My work"));
    }
    case "work":
      return work(rest, screen, under);
    case "agents":
      return agents(rest, screen, under);
    case "live":
      return live(rest, screen, under);
    case "knowledge":
      return knowledge(rest, screen, under);
    case "spend":
      if (rest.length === 0) return screen({ screen: "spend" });
      // A CLOSED SET, so an unknown tail is Not Found rather than the spend
      // screen: `#/spend/budget` — the obvious typo — drawing the spend tables
      // under a trail reading "Spend / budget" names three different things.
      if (rest.length === 1 && rest[0] === "budgets") return screen({ screen: "budgets" });
      return missing("spend", under("Spend"));
    case "settings":
      return settings(rest, screen, under);
    default:
      return missing("", `the screen “${head}”`);
  }
}

type Make = (s: Screen) => Resolved;
type Under = (label: string) => string;

function work(rest: string[], screen: Make, under: Under): Route {
  const [first, second] = rest;
  if (first === undefined) return screen({ screen: "work" });
  if (first === "views") {
    if (rest.length > 2) return missing("work", under("Work"));
    return screen(second ? { screen: "work-views", id: second } : { screen: "work-views" });
  }
  // THE WORKSPACE-LEVEL LISTS TAKE NO TAIL. Each answers a question about the
  // whole company, so there is nothing under them to address: a project has
  // its own route, and a change is a record on the item it changed.
  if (first === "projects" || first === "history" || first === "search") {
    if (rest.length > 1) return missing("work", under("Work"));
    return screen({
      screen:
        first === "projects"
          ? "work-projects"
          : first === "history"
            ? "work-history"
            : "work-search",
    });
  }
  if (rest.length > 1) return missing("work", under("Work"));
  // A PROJECT OR AN ITEM, decided by the SHAPE of the key rather than by a
  // lookup, so the route resolves before any answer arrives. A key or an id
  // BOTH resolve to an item, because the reader has whichever they were shown.
  if (KEY_SHAPE.test(first)) return screen({ screen: "project", key: first });
  if (ITEM_KEY_SHAPE.test(first) || ID_SHAPE.test(first)) {
    return screen({ screen: "item", id: first });
  }
  // AND ANYTHING ELSE IS NOTHING HERE. Read as an item id, a word — a reserved
  // one for a section Work does not have (`turns`), or one for a section still
  // to come — asked the tracker for an item called that and drew its refusal
  // under a trail naming a page that does not exist.
  return missing("work", under("Work"));
}

function agents(rest: string[], screen: Make, under: Under): Route {
  const [first, ...tail] = rest;
  if (first === undefined) return screen({ screen: "org-chart" });
  switch (first) {
    case "roster":
      return tail.length ? missing("agents", under("Agents")) : screen({ screen: "roster" });
    case "edit":
      return tail.length ? missing("agents", under("Agents")) : screen({ screen: "org-edit" });
    case "teams":
      if (tail.length > 1) return missing("agents", under("Agents"));
      return screen(tail[0] ? { screen: "teams", unit: tail[0] } : { screen: "teams" });
    case "schedules":
      // NONE OR ALL THREE: `{scope_type}/{scope_id}/{name}` names one
      // schedule, and a partial tail names nothing.
      if (tail.length !== 0 && tail.length !== 3) return missing("agents", under("Agents"));
      return screen({ screen: "schedules", scope: tail });
    case "seats":
      // HANDLES LIVE ONLY HERE, which is what lets a handle equal a reserved
      // word: `#/agents/seats/roster` is a seat called roster.
      if (tail.length !== 1 || !tail[0]) {
        return missing(
          "agents",
          under("Agents"),
          "a seat is opened from the roster or the org chart",
        );
      }
      return screen({ screen: "seat", handle: tail[0] });
    default:
      return missing("agents", under("Agents"));
  }
}

function live(rest: string[], screen: Make, under: Under): Route {
  const [first, ...tail] = rest;
  if (first === undefined) return screen({ screen: "live" });
  if (tail.length > 1) return missing("live", under("Live"));
  const id = tail[0];
  switch (first) {
    case "turns":
      return screen(id ? { screen: "turn", id } : { screen: "turns" });
    case "runs":
      return screen(id ? { screen: "runs", id } : { screen: "runs" });
    case "a2a":
      return screen(id ? { screen: "a2a", id } : { screen: "a2a" });
    case "events":
      return screen(id ? { screen: "event", id } : { screen: "events" });
    case "traces":
      // A TRACE HAS NO LIST, only a page: nothing enumerates traces, and every
      // way in is a link that already holds the id. This used to draw the
      // turns list under a trail naming traces, which is two claims at once
      // and neither of them the address.
      if (!id) {
        return missing(
          "live",
          under("Live"),
          "a trace has no list — open one from a turn, a run or an event",
        );
      }
      return screen({ screen: "trace", id });
    default:
      return missing("live", under("Live"));
  }
}

function knowledge(rest: string[], screen: Make, under: Under): Route {
  const [first, ...tail] = rest;
  if (first === undefined) return screen({ screen: "knowledge" });
  // ONE ADDRESS PER PAGE, BY ITS ID. A page was addressed by its container
  // and title, and a title changes on rename — so every link anybody had
  // copied broke the day somebody fixed a heading.
  if (first === "pages") {
    const [id] = tail;
    if (tail.length !== 1 || id === undefined || !ID_SHAPE.test(id)) {
      return missing(
        "knowledge",
        under("Knowledge"),
        "a page is opened from a search or its container",
      );
    }
    return screen({ screen: "page", id });
  }
  // THE SKILLS AGENTS ARE GIVEN, a list that links to each skill's one page.
  if (first === "skills" && tail.length === 0) return screen({ screen: "skills" });
  // A CONTAINER BY ITS SHAPE, like a project: a lowercase segment here is a
  // reserved word or nothing, and never a container the engine could have
  // minted — so a knowledge section that has not landed yet answers Not Found
  // rather than a container screen asking the engine for a key it cannot hold.
  if (reserved(first) || tail.length > 0 || !CONTAINER_SHAPE.test(first)) {
    return missing("knowledge", under("Knowledge"));
  }
  return screen({ screen: "container", key: first });
}

function settings(rest: string[], screen: Make, under: Under): Route {
  const [first, ...tail] = rest;
  if (first === undefined) return screen({ screen: "general" });
  const none = () => missing("settings", under("Settings"));
  switch (first) {
    case "integrations":
      if (tail.length > 1) return none();
      return screen(
        tail[0] ? { screen: "integrations", kind: tail[0] } : { screen: "integrations" },
      );
    case "tools": {
      // TWO SHAPES UNDER ONE SEGMENT, discriminated on LENGTH: `servers/{name}`
      // is a filter on the catalogue's origin and a bare `{name}` is one tool.
      // A tool name is a third-party MCP server's string, so reading
      // `tail[0] === "servers"` alone would take the page away from a tool
      // actually called that.
      if (tail.length === 0) return screen({ screen: "tools" });
      if (tail.length === 1 && tail[0]) return screen({ screen: "tools", tool: tail[0] });
      if (tail.length === 2 && tail[0] === "servers" && tail[1]) {
        return screen({ screen: "tools", server: tail[1] });
      }
      return none();
    }
    case "secrets":
      if (tail.length > 1) return none();
      return screen(tail[0] ? { screen: "secrets", name: tail[0] } : { screen: "secrets" });
    case "nodes":
      if (tail.length > 1) return none();
      return screen(tail[0] ? { screen: "nodes", node: tail[0] } : { screen: "nodes" });
    case "config":
      if (tail.length === 0) return screen({ screen: "config", revisions: false });
      if (tail[0] === "revisions" && tail.length <= 2) {
        return screen(
          tail[1]
            ? { screen: "config", revisions: true, revision: tail[1] }
            : { screen: "config", revisions: true },
        );
      }
      return none();
    case "backups":
      // DOMAINS LIVE ONLY HERE, one segment under it. The fleet page used to
      // hold both a node and a domain under one segment and tell them apart by
      // the tail's length, because a node id is an operator's string and
      // `domains` a legal one.
      if (tail.length > 1) return none();
      return screen(tail[0] ? { screen: "backups", domain: tail[0] } : { screen: "backups" });
    case "audit":
      // NO TAIL: every row has a page of its own somewhere else.
      return tail.length ? none() : screen({ screen: "audit" });
    default:
      return none();
  }
}
