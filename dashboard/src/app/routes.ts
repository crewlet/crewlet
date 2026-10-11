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
 * # Two shells, one table
 *
 * The person shell's heads are its workspaces (`today`, `inbox`, `me`,
 * `work`, `team`, `knowledge`), and the console's one head is `admin`, under
 * which its own workspaces are the second segment. Which shell a path is in
 * is `nav.ts`'s [shellOf] — the first segment — and nothing here decides it
 * again.
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
 * every reserved segment is lowercase, so a lowercase word under Knowledge can
 * never be read as a container.
 */
export const CONTAINER_SHAPE = /^[^a-z]+$/;

/** My work's sections below its queue. */
export type MeSection =
  | "queue"
  | "asked-of-me"
  | "asked-by-me"
  | "handed-off"
  | "ready"
  | "helping"
  | "following"
  | "checklist";

const ME_SECTIONS: readonly MeSection[] = [
  "asked-of-me",
  "asked-by-me",
  "handed-off",
  "ready",
  "helping",
  "following",
  "checklist",
];

/** Every screen the dashboard draws, with what its path names. */
export type Screen =
  // The landing: `#/`, which the viewer's own answer turns into an address.
  | { screen: "landing" }
  // The person shell.
  | { screen: "today" }
  | { screen: "inbox" }
  | { screen: "me"; section: MeSection }
  | { screen: "work" }
  | { screen: "work-projects" }
  | { screen: "work-views"; id?: string }
  | { screen: "work-search" }
  | { screen: "project"; key: string }
  | { screen: "item"; id: string }
  | { screen: "org-chart" }
  | { screen: "people" }
  | { screen: "units"; unit?: string }
  | { screen: "about" }
  | { screen: "seat"; handle: string }
  | { screen: "knowledge" }
  | { screen: "container"; key: string }
  | { screen: "page"; id: string }
  // The admin console.
  | { screen: "overview" }
  | { screen: "live" }
  | { screen: "turns" }
  | { screen: "turn"; id: string }
  | { screen: "runs"; id?: string }
  | { screen: "a2a"; id?: string }
  | { screen: "trace"; id: string }
  | { screen: "events" }
  | { screen: "event"; id: string }
  | { screen: "seats" }
  | { screen: "seat-setup"; handle: string }
  | { screen: "schedules"; scope: string[] }
  | { screen: "skills" }
  | { screen: "memory" }
  | { screen: "diary"; handle: string }
  | { screen: "page-reads"; id: string }
  | { screen: "spend" }
  | { screen: "spend-tasks" }
  | { screen: "budgets" }
  | { screen: "charter" }
  | { screen: "access" }
  | { screen: "org-edit" }
  | { screen: "audit" }
  | { screen: "changes" }
  | { screen: "integrations"; kind?: string }
  | { screen: "tools"; server?: string; tool?: string }
  | { screen: "models"; id?: string }
  | { screen: "secrets"; name?: string }
  | { screen: "nodes"; node?: string }
  | { screen: "config"; revisions: boolean; revision?: string }
  | { screen: "backups"; domain?: string };

export type ScreenName = Screen["screen"];

/**
 * A path that addresses a page. The discriminant is `resolved`, deliberately
 * not `kind`: a screen's own parameters are spread beside it, and one of them
 * IS a `kind` (an integration's), which a `kind` discriminant would silently
 * overwrite — `#/admin/connect/integrations/github` would resolve to an
 * integration called "screen".
 *
 * `workspace` is "" on the landing alone: `#/` is no workspace's until the
 * viewer has said whose it is.
 */
export type Resolved = Screen & { resolved: true; workspace: Workspace | "" };

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
  // THE EMPTY FRAGMENT IS THE LANDING, and which page that is depends on who
  // is reading — `nav.ts`'s `landingFor`, which the dispatch asks once the
  // viewer has answered. A resolver that picked one here would pick it before
  // any answer arrived, for everybody.
  if (head === undefined) return { resolved: true, workspace: "", screen: "landing" };
  const workspace = workspaceOf(path);
  const screen = (s: Screen): Resolved => ({ ...s, resolved: true, workspace });
  if (head === "admin") return admin(rest, screen);
  const under = (label: string) => `“${rest.join("/")}” under ${label}`;
  switch (head) {
    case "today":
      return rest.length ? missing("today", under("Today")) : screen({ screen: "today" });
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
    case "team":
      return team(rest, screen, under);
    case "knowledge":
      return knowledge(rest, screen, under);
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
  // its own route.
  if (first === "projects" || first === "search") {
    if (rest.length > 1) return missing("work", under("Work"));
    return screen({ screen: first === "projects" ? "work-projects" : "work-search" });
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

function team(rest: string[], screen: Make, under: Under): Route {
  const [first, ...tail] = rest;
  if (first === undefined) return screen({ screen: "org-chart" });
  switch (first) {
    case "people":
      return tail.length ? missing("team", under("Team")) : screen({ screen: "people" });
    case "about":
      return tail.length ? missing("team", under("Team")) : screen({ screen: "about" });
    case "units":
      if (tail.length > 1) return missing("team", under("Team"));
      return screen(tail[0] ? { screen: "units", unit: tail[0] } : { screen: "units" });
    case "seats":
      // HANDLES LIVE ONLY HERE, which is what lets a handle equal a reserved
      // word: `#/team/seats/people` is a seat called people. A handle under
      // `team/` itself would make every seat called People, Units or About
      // unreachable by address.
      if (tail.length !== 1 || !tail[0]) {
        return missing("team", under("Team"), "a seat is opened from the org chart or the people");
      }
      return screen({ screen: "seat", handle: tail[0] });
    default:
      return missing("team", under("Team"));
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
  // A CONTAINER BY ITS SHAPE, like a project: a lowercase segment here is a
  // reserved word or nothing, and never a container the engine could have
  // minted — so a knowledge section that has not landed yet answers Not Found
  // rather than a container screen asking the engine for a key it cannot hold.
  if (reserved(first) || tail.length > 0 || !CONTAINER_SHAPE.test(first)) {
    return missing("knowledge", under("Knowledge"));
  }
  return screen({ screen: "container", key: first });
}

/** The console: `#/admin`, and its workspaces as the second segment. */
function admin(rest: string[], screen: Make): Route {
  const [workspace, ...tail] = rest;
  if (workspace === undefined) return screen({ screen: "overview" });
  const under = (label: string) => `“${tail.join("/")}” under ${label}`;
  switch (workspace) {
    case "live":
      return live(tail, screen, under);
    case "agents":
      return agents(tail, screen, under);
    case "spend":
      if (tail.length === 0) return screen({ screen: "spend" });
      // A CLOSED SET, so an unknown tail is Not Found rather than the spend
      // screen: `#/admin/spend/budget` — the obvious typo — drawing the spend
      // tables under a trail reading "Spend / budget" names three different
      // things.
      if (tail.length === 1 && tail[0] === "budgets") return screen({ screen: "budgets" });
      if (tail.length === 1 && tail[0] === "tasks") return screen({ screen: "spend-tasks" });
      return missing("spend", under("Spend"));
    case "company":
      return company(tail, screen, under);
    case "connect":
      return connect(tail, screen, under);
    case "engine":
      return engine(tail, screen, under);
    default:
      return missing("", `the console screen “${rest.join("/")}”`);
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

function agents(rest: string[], screen: Make, under: Under): Route {
  const [first, ...tail] = rest;
  if (first === undefined) return screen({ screen: "seats" });
  switch (first) {
    case "schedules":
      // NONE OR ALL THREE: `{scope_type}/{scope_id}/{name}` names one
      // schedule, and a partial tail names nothing.
      if (tail.length !== 0 && tail.length !== 3) return missing("setup", under("Agent setup"));
      return screen({ screen: "schedules", scope: tail });
    case "skills":
      return tail.length ? missing("setup", under("Agent setup")) : screen({ screen: "skills" });
    case "memory":
      // EVERY AGENT'S MEMORY, and one agent's, by its handle — which lives
      // here as it does under `seats/`, so a handle may equal a reserved word.
      if (tail.length === 0) return screen({ screen: "memory" });
      if (tail.length === 1 && tail[0]) return screen({ screen: "diary", handle: tail[0] });
      return missing("setup", under("Agent setup"), "an agent's memory is opened from the list");
    case "seats":
      if (tail.length !== 1 || !tail[0]) {
        return missing("setup", under("Agent setup"), "a seat is opened from the seats list");
      }
      return screen({ screen: "seat-setup", handle: tail[0] });
    case "pages":
      // A PAGE HAS NO LIST HERE, like a trace: it is opened from the page, or
      // from a tool skill's row, both of which already hold its id.
      if (tail.length !== 1 || !tail[0]) {
        return missing(
          "setup",
          under("Agent setup"),
          "a page is opened from the page itself or from Agent skills",
        );
      }
      return screen({ screen: "page-reads", id: tail[0] });
    default:
      return missing("setup", under("Agent setup"));
  }
}

function company(rest: string[], screen: Make, under: Under): Route {
  const [first, ...tail] = rest;
  if (first === undefined) return screen({ screen: "charter" });
  // EVERY COMPANY SECTION TAKES NO TAIL: a person's page is their seat, a
  // change is a record on what it changed, and the builder's selection is its
  // own query.
  if (tail.length > 0) return missing("company", under("Company"));
  switch (first) {
    case "people":
      return screen({ screen: "access" });
    case "org":
      return screen({ screen: "org-edit" });
    case "audit":
      return screen({ screen: "audit" });
    case "changes":
      return screen({ screen: "changes" });
    default:
      return missing("company", under("Company"));
  }
}

function connect(rest: string[], screen: Make, under: Under): Route {
  const [first, ...tail] = rest;
  if (first === undefined) return screen({ screen: "integrations" });
  const none = () => missing("connect", under("Connect"));
  switch (first) {
    case "integrations":
      // ONE INTEGRATION'S PAGE. The list is Connect's own landing, so a bare
      // `integrations` is not a second address for it.
      if (tail.length !== 1 || !tail[0]) {
        return missing(
          "connect",
          under("Connect"),
          "an integration is opened from Connect's list of them",
        );
      }
      return screen({ screen: "integrations", kind: tail[0] });
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
    case "models":
      // ONE SEGMENT UNDER IT, a provider entry's config key — what a seat's
      // `llm:` names.
      if (tail.length > 1) return none();
      return screen(tail[0] ? { screen: "models", id: tail[0] } : { screen: "models" });
    case "secrets":
      if (tail.length > 1) return none();
      return screen(tail[0] ? { screen: "secrets", name: tail[0] } : { screen: "secrets" });
    default:
      return none();
  }
}

function engine(rest: string[], screen: Make, under: Under): Route {
  const [first, ...tail] = rest;
  if (first === undefined) return screen({ screen: "nodes" });
  const none = () => missing("engine", under("Engine"));
  switch (first) {
    case "nodes":
      // ONE NODE'S PAGE; the list is Engine's own landing.
      if (tail.length !== 1 || !tail[0]) {
        return missing("engine", under("Engine"), "a node is opened from Engine's list of them");
      }
      return screen({ screen: "nodes", node: tail[0] });
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
    default:
      return none();
  }
}
