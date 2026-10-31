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
 * and everything else asks this function. It imports nothing but `nav.ts`
 * and the artifacts' kind words `nav.ts` already reads, which is what lets
 * `lib/` and a Node test call it.
 *
 * # The sign-in screens resolve too, and to no workspace
 *
 * Signing in, redeeming an invitation, spending a password reset link and
 * enrolling a second factor are drawn OUTSIDE the frame (`FRAMELESS` in `nav.ts`), and they are resolved FIRST,
 * before any workspace is asked: their heads are reserved for them, and a
 * `#/login?next=…` link is a link to a page like any other, which the link
 * gates hold. What they lack is a workspace — `workspace` is `""` — because
 * nothing in the frame draws them, so a recent, a star or a breadcrumb never
 * names one.
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

import { isMakeKind, type MakeKind } from "~/lib/artifacts/words.ts";
import {
  framelessOf,
  RESERVED_SEGMENTS,
  workspaceOf,
  type FramelessRoute,
  type Workspace,
} from "./nav.ts";

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
 * The places under `#/chatlet` that are not a room, each a reserved word. A
 * room is addressed by its uuid ([ID_SHAPE]), so no word here can ever be
 * read as a room, and no room can be shadowed by one.
 */
export const CHAT_VIEWS = [
  "activity",
  "threads",
  "drafts",
  "saved",
  "search",
  "browse",
  "new",
] as const;
export type ChatView = (typeof CHAT_VIEWS)[number];
/**
 * A Doclet container key: whatever the engine keeps, which is UPPERCASED on
 * every path that writes one (`doclet.Store` uppercases the key a page is
 * created in and the key `EnsureContainer` makes), so it never holds a
 * lowercase letter. Looser than [KEY_SHAPE] on purpose — a unit's `space:` may
 * carry a dash or start with a digit — and exactly as strict where it matters:
 * every reserved segment is lowercase, so `#/doclet/skills` can never be
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

/**
 * The Artifacts gallery's sections: All (its landing, everything the reader
 * can open), Yours and Shared with you, each one of the scopes the engine's
 * listing answers by (`artifactsdomain.ListScope`).
 */
export type ArtifactSection = "all" | "yours" | "shared";

const ARTIFACT_SECTIONS: readonly ArtifactSection[] = ["yours", "shared"];

/**
 * The sections an earlier gallery had, and the section each now lands on: its
 * own artifacts are Yours, and what the whole company or an agent shared is
 * among All. The gallery rewrites such an address in place
 * (`Navigator.replace`), so a link pasted before the change still opens the
 * nearest list and the address bar says the current one.
 */
export const LEGACY_ARTIFACT_SECTIONS: Readonly<Record<string, ArtifactSection>> = {
  mine: "yours",
  company: "all",
  agents: "all",
};

/** A gallery section's address. */
export function artifactSectionPath(section: ArtifactSection): string[] {
  return section === "all" ? ["artifacts"] : ["artifacts", section];
}

/**
 * An artifact's own address in the dashboard, the one `artifactsRoute` reads.
 * It lives here rather than beside the artifacts' REST calls
 * (`lib/artifacts/api.ts`) because the frame draws it (a pinned artifact in
 * the sidebar, a palette hit): a bundler places a module whole, so the frame
 * importing one function from that module carried every artifact write and
 * read into the entry.
 */
export function artifactPath(id: string): string[] {
  return ["artifacts", id];
}

/**
 * The Folders section's address: the company's top-level folders the reader
 * can see, then what is in none of them. Here for the reason [artifactPath]
 * is: an artifact's page and its Details link to it.
 */
export function artifactFoldersPath(): string[] {
  return ["artifacts", "folders"];
}

/**
 * A folder's page, by its id, never by its path: the id the engine minted
 * survives a rename and a move, so a link pasted in chat still opens it.
 * Here for the reason [artifactPath] is.
 */
export function artifactFolderPath(id: string): string[] {
  return ["artifacts", "folders", id];
}

/**
 * The org builder's address, Org chart › Edit org: the one place the chart,
 * the seats, the charter and the policies are written, which every page that
 * reads them offers as its way to change them. Here for the reason
 * [artifactPath] is: the frame's pages link to it.
 */
export const ORG_EDIT_PATH: readonly string[] = ["company", "org", "edit"];

/**
 * Company › Principles, where the mission, the vision and the standing
 * policies are read and, by an administrator, written in place
 * (`routes/settings/principles/edit/address.ts` builds its edit address). Here
 * for the reason [artifactPath] is: the org builder links to it.
 */
export const PRINCIPLES_PATH: readonly string[] = ["company", "principles"];

/** Settings › Models, where a model is added, signed in and tested: the page a screen sends a reader to give the company a model. */
export const MODELS_PATH: readonly string[] = ["settings", "models"];

/**
 * Where a new artifact of a kind is made: each kind has its own page
 * (`routes/artifacts/new/`), because writing a doc, laying out a deck and
 * asking an agent for a dashboard are three different gestures. Here for the
 * reason [artifactPath] is: the frame links to it.
 */
export function newArtifactPath(kind: MakeKind): string[] {
  return ["artifacts", "new", kind];
}

/**
 * The screens drawn outside the frame. An invitation's and a reset link's one
 * segment is `<id>.<secret>`, kept whole: the screen itself says when that is
 * not a whole link — a link cut short by a mail client is the ordinary way it
 * arrives wrong, and "no such screen" would send the person looking for a
 * different page rather than for the rest of this one.
 */
export type SignInScreen =
  | { screen: "login" }
  | { screen: "enrol" }
  | { screen: "invite"; link: string }
  | { screen: "reset"; link: string };

/** Every screen the frame draws, with what its path names. */
export type FramedScreen =
  | { screen: "home" }
  | { screen: "inbox" }
  | { screen: "me"; section: MeSection }
  | { screen: "worklet-projects" }
  | { screen: "worklet-tasks" }
  | { screen: "worklet-views"; id?: string }
  | { screen: "worklet-history" }
  | { screen: "worklet-search" }
  | { screen: "project"; key: string }
  | { screen: "item"; id: string }
  | { screen: "principles" }
  | { screen: "brand" }
  | { screen: "brand-setup" }
  | { screen: "org-chart" }
  | { screen: "roster" }
  | { screen: "teams"; unit?: string }
  | { screen: "org-edit" }
  | { screen: "seat"; handle: string }
  | { screen: "schedules"; scope: string[] }
  | { screen: "integrations"; kind?: string }
  | { screen: "budgets" }
  | { screen: "chatlet"; id?: string }
  | { screen: "chatlet-mentions" }
  | { screen: "chatlet-search" }
  | { screen: "live" }
  | { screen: "turns" }
  | { screen: "turn"; id: string }
  | { screen: "runs"; id?: string }
  | { screen: "a2a"; id?: string }
  | { screen: "trace"; id: string }
  | { screen: "events" }
  | { screen: "event"; id: string }
  | { screen: "doclet" }
  | { screen: "container"; key: string }
  | { screen: "page"; id: string }
  | { screen: "new-page" }
  | { screen: "skills" }
  | { screen: "diaries" }
  | { screen: "diary"; handle: string }
  | { screen: "artifacts"; section: ArtifactSection }
  | { screen: "artifact-folders" }
  | { screen: "artifact-folder"; id: string }
  | { screen: "artifact"; id: string }
  /** A kind's own create page, or, with no kind, the address that redirects to one. */
  | { screen: "new-artifact"; kind: MakeKind | null }
  | { screen: "models"; id?: string }
  | { screen: "mcp-tools"; server?: string; tool?: string }
  | { screen: "secrets"; name?: string }
  | { screen: "limits" }
  | { screen: "spend" }
  | { screen: "spend-tasks" }
  | { screen: "people" }
  | { screen: "service-accounts" }
  | { screen: "tokens" }
  | { screen: "sign-in" }
  | { screen: "audit" }
  | { screen: "nodes"; node?: string }
  | { screen: "config"; revisions: boolean; revision?: string }
  | { screen: "backups"; domain?: string }
  | { screen: "storage" }
  | { screen: "account" };

/** Every screen the dashboard draws. */
export type Screen = FramedScreen | SignInScreen;

export type ScreenName = Screen["screen"];

/**
 * A path that addresses a page. The discriminant is `resolved`, deliberately
 * not `kind`: a screen's own parameters are spread beside it, and one of them
 * IS a `kind` (an integration's), which a `kind` discriminant would silently
 * overwrite, `#/company/integrations/github` would resolve to an
 * integration called "screen".
 */
export type Resolved =
  | (FramedScreen & { resolved: true; workspace: Workspace })
  | (SignInScreen & { resolved: true; workspace: "" });

/** A path the product has no page for, and a sentence saying which. */
export interface Unresolved {
  resolved: false;
  /** The workspace the head names, or "" when it names none. */
  workspace: Workspace | "";
  /** What was asked for, in words: `“views/x/y” under Worklet`. */
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
  const outside = framelessOf(path);
  if (outside !== null) return signIn(outside, rest);
  const screen = (s: FramedScreen): Resolved => ({
    ...s,
    resolved: true,
    workspace: workspaceOf(path) as Workspace,
  });
  // THE ROW THE ADDRESS IS IN, by the longest prefix it owns (`nav.ts`), so a
  // page nothing resolves under Company is Not Found under the row a reader
  // was in: `#/company/org/x` under the Org chart, not under Principles.
  const under = (label: string) => `“${rest.join("/")}” under ${label}`;
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
    case "company":
      return company(rest, screen, under);
    case "worklet":
      return worklet(rest, screen, under);
    case "chatlet":
      return chatlet(rest, screen, under);
    case "live":
      return live(rest, screen, under);
    case "doclet":
      return doclet(rest, screen, under);
    case "artifacts":
      return artifactsRoute(rest, screen, under);
    case "settings":
      return settings(rest, screen, under);
    case "account":
      return rest.length ? missing("account", under("Account")) : screen({ screen: "account" });
    default:
      return missing("", `the screen “${head}”`);
  }
}

type Make = (s: FramedScreen) => Resolved;

/**
 * A sign-in screen: exactly the tail its address has, and anything longer is
 * an address the product does not have — except an invitation and a reset
 * link, whose screens answer a link that is not whole.
 */
function signIn(route: FramelessRoute, rest: string[]): Route {
  const make = (s: SignInScreen): Resolved => ({ ...s, resolved: true, workspace: "" });
  switch (route) {
    case "login":
      return rest.length
        ? missing("", `“${rest.join("/")}” under sign-in`)
        : make({ screen: "login" });
    case "enrol":
      return rest.length
        ? missing("", `“${rest.join("/")}” under enrolment`)
        : make({ screen: "enrol" });
    case "invite":
    case "reset":
      return make({ screen: route, link: rest.length === 1 ? (rest[0] ?? "") : "" });
  }
}
type Under = (label: string) => string;

function worklet(rest: string[], screen: Make, under: Under): Route {
  const [first, second] = rest;
  // WORKLET OPENS ON ITS PROJECTS: the landing is All projects' first lens,
  // and the company's task list is the lens beside it.
  if (first === undefined) return screen({ screen: "worklet-projects" });
  if (first === "views") {
    if (rest.length > 2) return missing("worklet", under("Worklet"));
    return screen(second ? { screen: "worklet-views", id: second } : { screen: "worklet-views" });
  }
  // THE WORKSPACE-LEVEL LISTS TAKE NO TAIL. Each answers a question about the
  // whole company, so there is nothing under them to address: a project has
  // its own route, and a change is a record on the item it changed.
  if (first === "tasks" || first === "history" || first === "search") {
    if (rest.length > 1) return missing("worklet", under("Worklet"));
    return screen({
      screen:
        first === "tasks"
          ? "worklet-tasks"
          : first === "history"
            ? "worklet-history"
            : "worklet-search",
    });
  }
  if (rest.length > 1) return missing("worklet", under("Worklet"));
  // A PROJECT OR AN ITEM, decided by the SHAPE of the key rather than by a
  // lookup, so the route resolves before any answer arrives. A key or an id
  // BOTH resolve to an item, because the reader has whichever they were shown.
  if (KEY_SHAPE.test(first)) return screen({ screen: "project", key: first });
  if (ITEM_KEY_SHAPE.test(first) || ID_SHAPE.test(first)) {
    return screen({ screen: "item", id: first });
  }
  // AND ANYTHING ELSE IS NOTHING HERE. Read as an item id, a word (a reserved
  // one for a section Worklet does not have, `turns`, or one for a section
  // still to come) asked Worklet for an item called that and drew its refusal
  // under a trail naming a page that does not exist.
  return missing("worklet", under("Worklet"));
}

/**
 * Company's rows, each under `#/company`: Principles, at its own address and
 * at the bare one, which is the Company landing, then Brand, the Org chart,
 * Schedules, Integrations and Budgets.
 */
function company(rest: string[], screen: Make, under: Under): Route {
  const [first, ...tail] = rest;
  if (first === undefined) return screen({ screen: "principles" });
  const none = (row: Workspace, label: string) => missing(row, under(label));
  switch (first) {
    case "principles":
      return tail.length ? none("company", "Principles") : screen({ screen: "principles" });
    case "brand":
      // ONE PAGE UNDER IT, the brand's setup: a full page of its own, never a
      // dialog over the Brand page.
      if (tail.length === 0) return screen({ screen: "brand" });
      if (tail.length === 1 && tail[0] === "setup") return screen({ screen: "brand-setup" });
      return none("brand", "Brand");
    case "org":
      return org(tail, screen, under);
    case "schedules":
      // NONE OR ALL THREE: `{scope_type}/{scope_id}/{name}` names one
      // schedule, and a partial tail names nothing.
      if (tail.length !== 0 && tail.length !== 3) return none("schedules", "Schedules");
      return screen({ screen: "schedules", scope: tail });
    case "integrations":
      // ONE SEGMENT UNDER IT, an integration's kind: its own page.
      if (tail.length > 1) return none("integrations", "Integrations");
      return screen(
        tail[0] ? { screen: "integrations", kind: tail[0] } : { screen: "integrations" },
      );
    case "budgets":
      return tail.length ? none("budgets", "Budgets") : screen({ screen: "budgets" });
    default:
      return none("company", "Company");
  }
}

/** The Org chart's sections, and every seat's page. */
function org(rest: string[], screen: Make, under: Under): Route {
  const [first, ...tail] = rest;
  const none = (hint?: string) => missing("org", under("Org chart"), hint);
  if (first === undefined) return screen({ screen: "org-chart" });
  switch (first) {
    case "roster":
      return tail.length ? none() : screen({ screen: "roster" });
    case "edit":
      return tail.length ? none() : screen({ screen: "org-edit" });
    case "teams":
      if (tail.length > 1) return none();
      return screen(tail[0] ? { screen: "teams", unit: tail[0] } : { screen: "teams" });
    case "seats":
      // HANDLES LIVE ONLY HERE, which is what lets a handle equal a reserved
      // word: `#/company/org/seats/roster` is a seat called roster.
      if (tail.length !== 1 || !tail[0]) {
        return none("a seat is opened from the roster or the org chart");
      }
      return screen({ screen: "seat", handle: tail[0] });
    default:
      return none();
  }
}

function chatlet(rest: string[], screen: Make, under: Under): Route {
  const [first, ...tail] = rest;
  if (first === undefined) return screen({ screen: "chatlet" });
  // THE TWO SECTIONS THAT ARE NOT A ROOM are reserved words, and a tail under
  // either is nothing: neither list has an object of its own to address.
  if (first === "mentions" || first === "search") {
    if (tail.length) return missing("chatlet", under("Chatlet"));
    return screen({ screen: first === "mentions" ? "chatlet-mentions" : "chatlet-search" });
  }
  // THE ROOM IS THE ONLY THING UNDER `chatlet` WITH AN ID — the uuid the engine
  // minted for it, a direct conversation's derived from who is in it — and
  // never its NAME. A name is what a person types, so `#/chatlet/general` is the
  // address somebody guesses, and read as a room it would ask the engine for
  // one under an id nobody minted. The thread is deliberately NOT a segment
  // either: it is `?thread=` on the room you are already in, because a thread
  // is one level deep and has no life away from its room. Both guesses get the
  // sentence that says how a room and a thread are reached.
  if (tail.length || !ID_SHAPE.test(first)) {
    return missing(
      "chatlet",
      under("Chatlet"),
      "a room is opened from your rooms, a mention or a search, and a thread from its room",
    );
  }
  return screen({ screen: "chatlet", id: first });
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
        return missing("live", under("Live"), "Open a trace from a turn, run or event");
      }
      return screen({ screen: "trace", id });
    default:
      return missing("live", under("Live"));
  }
}

function doclet(rest: string[], screen: Make, under: Under): Route {
  const [first, ...tail] = rest;
  if (first === undefined) return screen({ screen: "doclet" });
  // ONE ADDRESS PER PAGE, BY ITS ID. A page was addressed by its container
  // and title, and a title changes on rename — so every link anybody had
  // copied broke the day somebody fixed a heading.
  if (first === "pages") {
    const [id] = tail;
    if (tail.length !== 1 || id === undefined || !ID_SHAPE.test(id)) {
      return missing("doclet", under("Doclet"), "a page is opened from a search or its container");
    }
    return screen({ screen: "page", id });
  }
  // A NEW PAGE, written in the composer. Where it is filed rides the query
  // (`?space=…&parent=…`), because the composer lets the writer change it.
  if (first === "new" && tail.length === 0) return screen({ screen: "new-page" });
  // THE SKILLS AGENTS ARE GIVEN, a list that links to each skill's one page.
  if (first === "skills" && tail.length === 0) return screen({ screen: "skills" });
  // EVERY AGENT'S DIARY, and one agent's, by its handle — which lives here as
  // it does under `seats/`, so a handle may equal a reserved word.
  if (first === "diaries") {
    if (tail.length === 0) return screen({ screen: "diaries" });
    if (tail.length === 1 && tail[0]) return screen({ screen: "diary", handle: tail[0] });
    return missing("doclet", under("Doclet"), "a diary is opened from the diaries list");
  }
  // A CONTAINER BY ITS SHAPE, like a project: a lowercase segment here is a
  // reserved word or nothing, and never a container the engine could have
  // minted, so a Doclet section that has not landed yet answers Not Found
  // rather than a container screen asking the engine for a key it cannot hold.
  if (reserved(first) || tail.length > 0 || !CONTAINER_SHAPE.test(first)) {
    return missing("doclet", under("Doclet"));
  }
  return screen({ screen: "container", key: first });
}

function artifactsRoute(rest: string[], screen: Make, under: Under): Route {
  const [first, ...tail] = rest;
  if (first === undefined) return screen({ screen: "artifacts", section: "all" });
  // A NEW ARTIFACT IS MADE ON ITS KIND'S OWN PAGE (`#/artifacts/new/doc`).
  // The bare address, which earlier links carry with `?kind=`, resolves too,
  // and the frame rewrites it in place to the kind's page or the gallery.
  if (first === "new") {
    if (tail.length === 0) return screen({ screen: "new-artifact", kind: null });
    const [kind, ...more] = tail;
    if (more.length === 0 && kind !== undefined && isMakeKind(kind)) {
      return screen({ screen: "new-artifact", kind });
    }
    return missing(
      "artifacts",
      under("Artifacts"),
      kind === "brand" && more.length === 0
        ? "the company's brand is set up on Company › Brand"
        : "a new artifact is a doc, slides, a sheet, a design, a dashboard or an HTML page",
    );
  }
  // THE FOLDERS: the section, and one folder by the id the engine minted
  // (`#/artifacts/folders/<id>`), never its path, so a rename or a move
  // breaks no link. A word there names nothing the engine could have minted.
  if (first === "folders") {
    if (tail.length === 0) return screen({ screen: "artifact-folders" });
    const [id, ...more] = tail;
    if (more.length === 0 && id !== undefined && ID_SHAPE.test(id)) {
      return screen({ screen: "artifact-folder", id });
    }
    return missing(
      "artifacts",
      under("Artifacts"),
      "a folder is opened from Folders, an artifact's details or a link somebody shared",
    );
  }
  // NOTHING IS ADDRESSED UNDER AN ARTIFACT: its tabs, its version and its
  // compare are queries on its own address (`tab=`, `version=`, `against=`),
  // so a link survives a reload and a pasted address in chat.
  if (tail.length > 0) return missing("artifacts", under("Artifacts"));
  if (ARTIFACT_SECTIONS.includes(first as ArtifactSection)) {
    return screen({ screen: "artifacts", section: first as ArtifactSection });
  }
  const moved = Object.hasOwn(LEGACY_ARTIFACT_SECTIONS, first)
    ? LEGACY_ARTIFACT_SECTIONS[first]
    : undefined;
  if (moved) return screen({ screen: "artifacts", section: moved });
  // AN ARTIFACT BY ITS ID, the uuid the engine derived from the operation
  // that created it; a word here names nothing the engine could have minted.
  if (ID_SHAPE.test(first)) return screen({ screen: "artifact", id: first });
  return missing(
    "artifacts",
    under("Artifacts"),
    "an artifact is opened from the gallery, a task or a link somebody shared",
  );
}

/**
 * Settings' sections. The bare `#/settings` names none of them: the Settings
 * row lands on its first section, Models, and an address is one page.
 */
function settings(rest: string[], screen: Make, under: Under): Route {
  const [first, ...tail] = rest;
  const none = () => missing("settings", under("Settings"));
  if (first === undefined) {
    return missing("settings", "Settings itself", "Settings opens on Models");
  }
  // THE SECTIONS THAT TAKE NO TAIL: every row they list has a page of its
  // own somewhere else, or none.
  const bare: Partial<Record<string, FramedScreen>> = {
    limits: { screen: "limits" },
    people: { screen: "people" },
    "service-accounts": { screen: "service-accounts" },
    tokens: { screen: "tokens" },
    "sign-in": { screen: "sign-in" },
    audit: { screen: "audit" },
    storage: { screen: "storage" },
  };
  const plain = Object.hasOwn(bare, first) ? bare[first] : undefined;
  if (plain) return tail.length ? none() : screen(plain);
  switch (first) {
    case "models":
      // ONE SEGMENT UNDER IT, a provider entry's config key: what a seat's
      // `llm:` names.
      if (tail.length > 1) return none();
      return screen(tail[0] ? { screen: "models", id: tail[0] } : { screen: "models" });
    case "mcp-tools": {
      // TWO SHAPES UNDER ONE SEGMENT, discriminated on LENGTH: `servers/{name}`
      // is a filter on the catalogue's origin and a bare `{name}` is one tool.
      // A tool name is a third-party MCP server's string, so reading
      // `tail[0] === "servers"` alone would take the page away from a tool
      // actually called that.
      if (tail.length === 0) return screen({ screen: "mcp-tools" });
      if (tail.length === 1 && tail[0]) return screen({ screen: "mcp-tools", tool: tail[0] });
      if (tail.length === 2 && tail[0] === "servers" && tail[1]) {
        return screen({ screen: "mcp-tools", server: tail[1] });
      }
      return none();
    }
    case "secrets":
      if (tail.length > 1) return none();
      return screen(tail[0] ? { screen: "secrets", name: tail[0] } : { screen: "secrets" });
    case "spend":
      // A CLOSED SET, so an unknown tail is Not Found rather than the spend
      // screen: `#/settings/spend/task`, the obvious typo, drawing the spend
      // tables under a trail naming a page that does not exist.
      if (tail.length === 0) return screen({ screen: "spend" });
      if (tail.length === 1 && tail[0] === "tasks") return screen({ screen: "spend-tasks" });
      return none();
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
    default:
      return none();
  }
}
