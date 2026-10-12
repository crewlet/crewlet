/**
 * The information architecture: ONE sidebar, and three nouns that keep it one.
 *
 * # One sidebar, not a rail and a tree
 *
 * The dashboard had two columns of navigation: an 80px rail of eight
 * workspaces, and beside it a sidebar holding the open workspace's own tree —
 * its units, its containers, its nodes and domains. Two levels, because six
 * trees would not fit in one list. What it cost was that every workspace had a
 * different second column, the reader's place moved between two columns on
 * every navigation, and the rail spent 97px of every screen on eight icons.
 *
 * The approved design keeps the trees and moves them: a workspace's own
 * SECTIONS are paths drawn as tabs in the page header (Settings draws them as
 * a column, because its own row in WORKSPACES holds more sections, and
 * cross-links to the reader's Account and to Budgets, in its groups, than a
 * tab strip can — a row nobody reads to the end of), and its
 * OBJECTS — a unit, a container, a node — are rows on the section that lists
 * them. What is left for the sidebar is the part every workspace shares: where
 * you can go, and the few objects this reader keeps coming back to.
 *
 * # The three nouns, stated once and asserted in router.test.ts
 *
 *   - A SIDEBAR ROW is a workspace, or a live shortcut to a pinned view, a
 *     starred page or a running turn. A project is not one: it is a scope of
 *     Work, switched where the trail names it.
 *   - A SECTION is a PATH segment inside a workspace, drawn by one renderer:
 *     `tabs` in the page header, `lenses` beside Work's scope in the trail,
 *     `column` for Settings, or `none` where the workspace's own screens lead
 *     to it (Knowledge's landing).
 *   - An OBJECT TAB is a `tab=` query on an object's own page, bound to 1–9.
 *
 * Nothing is two of those. A section is never a sidebar row and never a
 * query; an object tab is never a path. A filter — a reason, a scope, a
 * status — is none of the three and lives in the page's own bar.
 *
 * # The order is the argument
 *
 * You first — what needs you, what reached you, what is yours — then the
 * company's work, its people, what is running, what it knows and what it
 * spent, and the machine last. A founder opening this meets their own
 * decisions first and the engine last, because the company is the product and
 * the engine is what runs it.
 */

import type { GlyphName } from "@crewlethq/icons/glyphs";
import type { GRANTS } from "~/contract/identity.ts";

/**
 * A capability a principal carries — the engine's `iam.Grant` vocabulary,
 * which the `viewer` answer reports in `grants`. A closed union so a section
 * naming a grant the engine does not have is a compile error rather than a
 * lock nobody can open — drawn from `contract/identity.ts`, the one list a gate
 * holds against the engine's.
 */
export type Grant = (typeof GRANTS)[number];

/** The workspaces: the sidebar's navigation rows, and the reader's own Account. */
export type Workspace =
  | "home"
  | "inbox"
  | "me"
  | "work"
  | "agents"
  | "live"
  | "knowledge"
  | "spend"
  | "settings"
  | "account";

/**
 * How a workspace draws its sections: `tabs` in the page header, a `column`
 * of them beside the screen (Settings), or `none`, where the workspace's own
 * screens lead to its sections (Home, Inbox, and Knowledge, whose landing
 * links its two sections under "For the agents").
 *
 * `lenses` is Work's. Its sections are the lenses of the scope the trail
 * names, All projects, drawn beside the trail's switcher the way a project
 * draws its own lenses, so the bar reads one way in either scope and Work
 * keeps no row of tabs of its own.
 */
export type SectionRenderer = "tabs" | "lenses" | "column" | "none";

/**
 * Where a workspace's row sits in the sidebar — or `menu`, for the one with no
 * row: the reader's own Account, which the user block at the sidebar's foot
 * links from its name and its menu, since that block already IS the reader.
 */
export type SidebarPlace = "you" | "workspace" | "foot" | "menu";

/**
 * One section of a workspace: a place with its own path.
 *
 * `tab: false` is a section that exists as an address and is reached some
 * other way — the org builder, which the Agents header opens with a button
 * because it is the one section that WRITES, and a reader arrives to look.
 */
export interface Section {
  key: string;
  label: string;
  /**
   * A GLYPH NAME rather than a component: four consumers draw this row — the
   * header's tabs, the Settings column, the palette and the tests — and a
   * table holding components would make each of them carry the whole set.
   * The union keeps a misspelling a compile error.
   */
  icon: GlyphName;
  path: string[];
  /** Shown under the label in the command palette. */
  hint: string;
  /**
   * The WORDS A READER LOOKS FOR THIS BY, beside its label — lowercase, and
   * matched by the command palette after the labels ([Destination.keywords]).
   * Declared here, once, because a reader names a place by what it holds
   * ("password", "2fa") as often as by what it is called, and searching the
   * hint's prose instead would surface every screen whose sentence happens to
   * mention a word: People & access for "tokens", which manages somebody
   * else's.
   */
  keywords?: readonly string[];
  /**
   * The GRANTS that open this section — ANY ONE of them, because the engine
   * admits some reads on either of two (the identity directory answers both
   * `people:manage` and `audit:read`). The navigation draws a lock beside a
   * section the reader holds none of and never hides it, and the frame draws
   * ONE refusal (`GrantRequired`) in place of the screen for a viewer the
   * engine has said holds none — a screen that mounted anyway was refused and
   * drew the refusal as zeros. NAMED rather than a flag, because what a
   * reader is missing is a grant, and "needs an operator credential" sent a
   * signed-in person to look for a token they have no use for. A section only
   * PART of which needs more is not marked: its screen says so where the
   * guarded part is. Every section also needs `state:read`, which the
   * `viewer` read itself does, so nothing here repeats it.
   */
  grants?: readonly Grant[];
  /** Drawn in the renderer (default), or an address reached another way. */
  tab?: boolean;
  /** The Settings column's group heading. */
  group?: string;
  /**
   * A CROSS-LINK: a row in this workspace's renderer whose address belongs to
   * another workspace — Settings › Company lists Budgets, which lives once,
   * under Spend. It is drawn with an arrow and is never this workspace's, and
   * it is only ever built by [crossLink], from the row it leads to.
   */
  elsewhere?: boolean;
  /**
   * THE READER'S OWN: it reads and writes nothing of the company — the
   * reader's Account, how the console looks in this browser — so it is open
   * to every signed-in reader, one the engine serves no company to included
   * (a session without `state:read`, a refused socket). See [sectionOpen].
   */
  personal?: boolean;
}

export interface WorkspaceRow {
  key: Workspace;
  label: string;
  /** A glyph name, for the reason [Section.icon] gives. */
  icon: GlyphName;
  /** The workspace's landing address. */
  path: string[];
  /** Shown under the label in the command palette. */
  hint: string;
  /** The words a reader looks for it by, as [Section.keywords]. */
  keywords?: readonly string[];
  /** The access key in the `g`-prefixed jump chord. */
  chord: string;
  /** Where the sidebar draws the row. */
  place: SidebarPlace;
  renderer: SectionRenderer;
  sections: Section[];
  /**
   * Query keys that belong to the WORKSPACE rather than to one section, and
   * travel with the reader between its sections: whose day My work is reading
   * is the same question on every section of it.
   */
  keep?: string[];
  /** The grants the whole workspace needs, as [Section.grants]: locked, never hidden. */
  grants?: readonly Grant[];
  /** The reader's own, as [Section.personal]: the Account. */
  personal?: boolean;
  /**
   * A sentence at the end of the section tabs that holds for every section —
   * how to read what the workspace draws. It gives way to the tabs: drawn only
   * where the strip has room for it beside every tab (`noteFits` in
   * `header/PageHeader.tsx`), so never on a phone.
   */
  note?: string;
}

/**
 * Budgets, which live ONCE, under Spend — and which Settings › Company lists
 * as a cross-link ([crossLink]), since that is where an operator looks for a
 * ceiling. Declared on its own so the cross-link is derived from it rather
 * than written beside it.
 */
const BUDGETS: Section = {
  key: "budgets",
  label: "Budgets",
  icon: "sliders-vertical",
  path: ["spend", "budgets"],
  hint: "Daily, weekly and monthly token limits",
};

/**
 * THE READER'S OWN, AND NO ROW: the user block at the sidebar's foot is the
 * reader, and its name and its menu are where this is opened — a row beside it
 * would be the same person twice, and the block takes the row's current mark
 * instead. Settings lists it too, as a cross-link ([crossLink]), since that is
 * where a person looks for their own security. A workspace all the same, so
 * the palette and `g u` reach it. No grant, and PERSONAL: every signed-in
 * reader has an account, the page reads nothing of the company, and it says
 * what a credential that is not a person's can do there.
 */
const ACCOUNT: WorkspaceRow = {
  key: "account",
  label: "Account",
  icon: "user",
  path: ["account"],
  hint: "Your profile, password, two-step verification, sessions and personal access tokens",
  // WHAT A PERSON TYPES WHEN THEY LOOK FOR IT. Only "account" found it, so
  // a reader searching for their password or their second factor was told
  // nothing matched — while the hint under the row named both.
  keywords: [
    "my account",
    "profile",
    "password",
    "two-step verification",
    "2fa",
    "mfa",
    "second factor",
    "authenticator",
    "recovery codes",
    "security",
    "sessions",
    "sign out",
    "personal access token",
    "token",
  ],
  chord: "u",
  place: "menu",
  personal: true,
  renderer: "none",
  sections: [],
};

export const WORKSPACES: WorkspaceRow[] = [
  {
    key: "home",
    label: "Home",
    icon: "house",
    path: ["home"],
    hint: "What needs you and how the company is doing",
    chord: "h",
    place: "you",
    renderer: "none",
    sections: [],
  },
  {
    key: "inbox",
    label: "Inbox",
    icon: "inbox",
    path: ["inbox"],
    hint: "What reached you, and what needs a person",
    chord: "i",
    place: "you",
    renderer: "none",
    sections: [],
  },
  {
    key: "me",
    label: "My work",
    icon: "circle-check",
    path: ["me"],
    hint: "Your queue, your asks, and what became workable",
    chord: "m",
    place: "you",
    renderer: "tabs",
    keep: ["handle"],
    sections: [
      {
        key: "queue",
        label: "Queue",
        icon: "list",
        path: ["me"],
        hint: "Their tasks, by due date or priority",
      },
      {
        key: "asked-of-me",
        label: "Asked of me",
        icon: "message-square",
        path: ["me", "asked-of-me"],
        hint: "Questions waiting on this person",
      },
      {
        key: "asked-by-me",
        label: "Asked by me",
        icon: "send",
        path: ["me", "asked-by-me"],
        hint: "Questions they asked that are still open",
      },
      {
        key: "unblocked",
        label: "Unblocked",
        icon: "zap",
        path: ["me", "unblocked"],
        hint: "Work whose blockers have all finished",
      },
      {
        key: "collaborating",
        label: "Collaborating",
        icon: "users",
        path: ["me", "collaborating"],
        hint: "Tasks this person was brought onto without owning",
      },
      {
        key: "watching",
        label: "Watching",
        icon: "eye",
        path: ["me", "watching"],
        hint: "Followed work that changed recently",
      },
      {
        key: "checklist",
        label: "Checklist",
        icon: "check",
        path: ["me", "checklist"],
        hint: "Their checklist items on other people’s tasks",
      },
    ],
  },
  {
    key: "work",
    label: "Work",
    icon: "square-kanban",
    path: ["work"],
    hint: "Projects and tasks",
    chord: "w",
    place: "workspace",
    // THE SCOPE'S LENSES, beside the switcher in the trail: All projects'
    // lenses are these three sections, a project's are its own `lens=` tabs,
    // and both draw alike (see [SectionRenderer]).
    renderer: "lenses",
    sections: [
      {
        // WORK OPENS ON ITS PROJECTS: the first question about the company's
        // work is where it is filed, and every project is one row away from
        // its own board here.
        key: "projects",
        label: "Projects",
        icon: "columns-3",
        path: ["work"],
        hint: "Every project, its lead and progress",
      },
      {
        key: "tasks",
        label: "Tasks",
        icon: "list",
        path: ["work", "tasks"],
        hint: "Every task in the company",
      },
      {
        key: "history",
        label: "History",
        icon: "chart-no-axes-gantt",
        path: ["work", "history"],
        hint: "Every change to the company’s work",
      },
      // ADDRESSES, NOT LENSES: a saved view is reached from the view strip,
      // Pinned and ⌘K, and ranked search from ⌘K, so neither is a lens of a
      // scope.
      {
        key: "views",
        label: "Saved views",
        icon: "layout-dashboard",
        path: ["work", "views"],
        hint: "Every saved view and its owner",
        tab: false,
      },
      {
        key: "search",
        label: "Search",
        icon: "search",
        path: ["work", "search"],
        hint: "Find tasks",
        tab: false,
      },
    ],
  },
  {
    key: "agents",
    label: "Agents",
    icon: "network",
    path: ["agents"],
    hint: "The org chart and every seat on it",
    chord: "a",
    place: "workspace",
    renderer: "tabs",
    sections: [
      {
        key: "chart",
        label: "Org chart",
        icon: "network",
        path: ["agents"],
        hint: "The hierarchy every seat works inside, and the seat each person is invited onto",
      },
      {
        key: "roster",
        label: "Roster",
        icon: "list",
        path: ["agents", "roster"],
        hint: "Every seat and what it is doing",
      },
      {
        key: "teams",
        label: "Teams",
        icon: "users",
        path: ["agents", "teams"],
        hint: "Every team and its goals",
      },
      {
        key: "schedules",
        label: "Schedules",
        icon: "calendar-clock",
        path: ["agents", "schedules"],
        hint: "Recurring work and when it runs next",
      },
      {
        // THE ONE SECTION THAT WRITES, so it is an address and a button rather
        // than a tab: a reader arrives at Agents to look.
        key: "edit",
        label: "Edit org",
        icon: "pencil",
        path: ["agents", "edit"],
        hint: "Change teams, seats and reporting lines",
        // NO GRANT: the builder answers for itself, saying which of its saves
        // the reader's grants reach, rather than the frame locking it whole —
        // its draft is kept in the tab's storage, and a frame refusal drawn
        // the moment a grant was withdrawn would take the draft off screen
        // while the person who wrote it is still there to copy it out.
        tab: false,
      },
    ],
  },
  {
    key: "live",
    label: "Live",
    icon: "activity",
    path: ["live"],
    hint: "Every turn, what ran and what it cost",
    chord: "l",
    place: "workspace",
    renderer: "tabs",
    sections: [
      {
        key: "now",
        label: "Now running",
        icon: "zap",
        path: ["live"],
        hint: "What is running now",
      },
      {
        key: "turns",
        label: "Turns",
        icon: "brain",
        path: ["live", "turns"],
        hint: "Every turn the seats ran, round by round",
        grants: ["audit:read"],
      },
      {
        key: "runs",
        label: "Coding runs",
        icon: "square-terminal",
        path: ["live", "runs"],
        hint: "Coding runs, live and finished",
      },
      {
        key: "a2a",
        label: "Agent-to-agent",
        icon: "link",
        path: ["live", "a2a"],
        hint: "The private channels seats opened with each other",
        grants: ["audit:read"],
      },
      {
        key: "events",
        label: "Event log",
        icon: "list",
        path: ["live", "events"],
        hint: "Every engine event",
        grants: ["audit:read"],
      },
    ],
  },
  {
    key: "knowledge",
    label: "Knowledge",
    icon: "book-open",
    path: ["knowledge"],
    hint: "The company’s documentation library and knowledge base",
    chord: "k",
    place: "workspace",
    // NO COLUMN, the approved Knowledge Home: the spaces are a row at the top
    // of every Knowledge screen and the search sits in the page bar, so a
    // page is read at the full width of the screen. The sections are reached
    // from the landing, the palette and the trail.
    renderer: "none",
    sections: [
      {
        key: "search",
        label: "Search",
        icon: "search",
        path: ["knowledge"],
        hint: "Search and browse the knowledge base",
      },
      {
        key: "skills",
        label: "Agent skills",
        icon: "wand-sparkles",
        path: ["knowledge", "skills"],
        hint: "Skills agents use and learn",
      },
      {
        key: "diaries",
        label: "Agent diaries",
        icon: "brain",
        path: ["knowledge", "diaries"],
        hint: "What each agent remembers",
        grants: ["audit:read"],
      },
    ],
  },
  {
    key: "spend",
    label: "Spend",
    icon: "coins",
    path: ["spend"],
    hint: "Token spend and budgets",
    chord: "t",
    place: "workspace",
    renderer: "tabs",
    // THE WINDOW IS THE WORKSPACE'S QUESTION: Overview and Expensive tasks
    // both read it, and a tab that dropped it opened the next section on the
    // default while the overview's own "All" link carried it — one section
    // reached two ways on two windows. Budgets reads its own calendar windows
    // and ignores it, but carries it, so the way back keeps the reader's.
    keep: ["window"],
    sections: [
      {
        key: "overview",
        label: "Overview",
        icon: "coins",
        path: ["spend"],
        hint: "Tokens over time, by phase, agent, model and team",
      },
      {
        key: "tasks",
        label: "Expensive tasks",
        icon: "layers",
        path: ["spend", "tasks"],
        hint: "Tasks that spent the most tokens",
      },
      BUDGETS,
    ],
  },
  {
    // LAST AND LOCKED, and the row is never hidden: a section that vanishes
    // when a credential is absent is indistinguishable from one that does not
    // exist, so an operator on a fresh browser would conclude the product has
    // nowhere to configure anything. General is the one section every
    // reader can read — the charter is the org projection's — so the
    // workspace names no grant as a whole; each other section names its own.
    key: "settings",
    label: "Settings",
    icon: "sliders-vertical",
    path: ["settings"],
    hint: "Everything an operator configures, in one place",
    chord: "s",
    place: "foot",
    renderer: "column",
    sections: [
      // THE READER'S OWN, FIRST — the order argument above — and a
      // CROSS-LINK, because Account has one address and it is not under
      // Settings. Settings is where a person looks for their password and
      // their second factor, and a reader holding none of the company's
      // grants found only locks here and concluded there was no two-step
      // verification to set up.
      crossLink(ACCOUNT, { key: "account", group: "You" }),
      {
        // THE READER'S OWN TOO, beside their Account: how the console looks
        // in this browser, which no operator configures and nobody else
        // sees. ONE GROUP for both, first, because a person's own settings
        // are what they look for first. No grant, because it reads and writes
        // nothing of the company's.
        key: "appearance",
        label: "Appearance",
        icon: "palette",
        path: ["settings", "appearance"],
        hint: "Mode, density and theme, in this browser only",
        group: "You",
        personal: true,
      },
      {
        key: "general",
        label: "General",
        icon: "house",
        path: ["settings"],
        hint: "The charter: mission, vision and the standing policies",
        group: "Company",
      },
      {
        key: "people",
        label: "People & access",
        icon: "users",
        path: ["settings", "people"],
        hint: "People, their credentials and sessions, and this node's API tokens",
        group: "Company",
        grants: ["people:manage", "audit:read"],
      },
      crossLink(BUDGETS, { key: "budgets", group: "Company" }),
      {
        key: "integrations",
        label: "Integrations",
        icon: "plug",
        path: ["settings", "integrations"],
        hint: "Connected tools and their status",
        group: "Connect",
        grants: ["config:read"],
      },
      {
        key: "tools",
        label: "Tools & MCP",
        icon: "wrench",
        path: ["settings", "tools"],
        hint: "Agent tools and MCP servers",
        group: "Connect",
        // NO GRANT: the registry is the `tools` push every reader gets. Only
        // an MCP server's template reads the configuration (`config:read`),
        // and that panel says so itself — a lock here told a reader the list
        // they were looking at was closed to them.
      },
      {
        key: "models",
        label: "Models & keys",
        icon: "cpu",
        path: ["settings", "models"],
        hint: "Models and their API keys",
        group: "Connect",
        grants: ["config:read"],
      },
      {
        key: "secrets",
        label: "Secrets",
        icon: "key",
        path: ["settings", "secrets"],
        hint: "The company’s stored credentials",
        group: "Connect",
        grants: ["config:read"],
      },
      {
        key: "nodes",
        label: "Nodes",
        icon: "server",
        path: ["settings", "nodes"],
        hint: "Engine nodes, the broker and file storage",
        group: "Engine",
        grants: ["fleet:operate"],
      },
      {
        key: "config",
        label: "Configuration",
        icon: "code",
        path: ["settings", "config"],
        hint: "The active company revision, its history and its diffs",
        group: "Engine",
        grants: ["config:read"],
      },
      {
        key: "backups",
        label: "Backups & retention",
        icon: "database",
        path: ["settings", "backups"],
        hint: "Backups and how long records are kept",
        group: "Engine",
        grants: ["fleet:operate"],
      },
      {
        key: "audit",
        label: "Audit log",
        icon: "shield",
        path: ["settings", "audit"],
        hint: "Every change made by people and tokens",
        group: "Engine",
        grants: ["audit:read"],
      },
    ],
  },
  ACCOUNT,
];

/** The row for a workspace, or undefined for a route nothing owns. */
export function workspaceRow(key: Workspace | ""): WorkspaceRow | undefined {
  return WORKSPACES.find((row) => row.key === key);
}

/**
 * Which workspace a route belongs to, so the sidebar marks the right row.
 *
 * The FIRST SEGMENT decides, and each workspace owns exactly one — its own
 * key. The empty fragment is Home, which is the landing screen. A head no
 * workspace owns answers "", never the first row: a sidebar that marked a
 * workspace for a path it does not hold would tell the reader they are
 * somewhere they are not.
 */
export function workspaceOf(path: string[]): Workspace | "" {
  const head = path[0] ?? "";
  if (!head) return "home";
  return WORKSPACES.find((row) => row.key === head)?.key ?? "";
}

/**
 * The routes drawn OUTSIDE the frame: signing in, redeeming an invitation,
 * setting a password from a reset link, and enrolling the second factor a
 * deployment requires before anything else.
 *
 * NOT WORKSPACES, and not rows anywhere. Each is somewhere a browser is before
 * it holds a session the frame can use — nobody, or a session that may do
 * nothing but enrol — so the sidebar, the page header, the palette and the
 * state bar would each be a surface drawn over questions the engine will
 * refuse. The frame's own tables (`WORKSPACES`, `DESTINATIONS`) are therefore
 * the wrong place for them, and this list is what the frame's dispatch
 * (`App.tsx`), the route table (`routes.ts`, which resolves each to a screen
 * of its own) and the sign-in routing read instead.
 *
 * Every head is lowercase and none is a key the engine mints, which
 * `router.test.ts` holds as it holds the reserved segments.
 */
export const FRAMELESS = ["login", "invite", "reset", "enrol"] as const;

export type FramelessRoute = (typeof FRAMELESS)[number];

/** The frameless route a path is, or null for one the frame draws. */
export function framelessOf(path: readonly string[]): FramelessRoute | null {
  const head = path[0];
  return (FRAMELESS as readonly string[]).includes(head ?? "") ? (head as FramelessRoute) : null;
}

/**
 * Every lowercase segment the route table spells, held apart from the keys the
 * engine mints.
 *
 * Project and container keys are UPPERCASE, item keys are `KEY-n`, everything
 * else the engine mints is a uuid — and every segment here is lowercase. That
 * is what makes `#/work/views` a list of saved views rather than a project
 * called VIEWS, without a single escape character in the route table. A
 * handle is lowercase too, which is why handles live ONLY under `seats/`,
 * unit keys only under `teams/` and domains only under `backups/`: none of
 * them is ever read at a position where a reserved word is. (A unit is
 * addressed by its KEY — the engine's `Unit.Key`, its declared `id:` or its
 * name where it declares none — never by a name two units may share.)
 */
export const RESERVED_SEGMENTS: string[] = [
  "tasks",
  "views",
  "history",
  "search",
  "asked-of-me",
  "asked-by-me",
  "unblocked",
  "collaborating",
  "watching",
  "checklist",
  "roster",
  "teams",
  "schedules",
  "edit",
  "seats",
  "turns",
  "runs",
  "a2a",
  "traces",
  "events",
  "pages",
  "budgets",
  "people",
  "integrations",
  "tools",
  "servers",
  "models",
  "secrets",
  "nodes",
  "config",
  "revisions",
  "backups",
  "audit",
  "appearance",
];

/**
 * Every fixed destination — a workspace's landing page and each of its own
 * sections — for the command palette and for the tests that walk the product.
 *
 * DERIVED FROM [WORKSPACES], never written beside it: a palette that listed
 * its own copy of the sections is a second table that is wrong the day a
 * section moves. A section whose path IS its workspace's landing page is one
 * destination under the workspace's name, and a cross-link (Settings' Budgets
 * row) is the destination of the workspace that owns the address.
 */
export interface Destination {
  key: string;
  workspace: Workspace;
  label: string;
  icon: GlyphName;
  path: string[];
  hint: string;
  /**
   * The words a reader looks for it by, beside its label — the workspace's
   * own for its landing, the section's for a section ([Section.keywords]).
   */
  keywords?: readonly string[];
  /** What opens it, as [Section.grants]. */
  grants?: readonly Grant[];
}

export const DESTINATIONS: Destination[] = WORKSPACES.flatMap((row) => {
  const own = row.sections.find((s) => !s.elsewhere && samePath(s.path, row.path));
  const landing: Destination = {
    key: row.key,
    workspace: row.key,
    label: row.label,
    icon: row.icon,
    path: row.path,
    hint: row.hint,
    // A LANDING PAGE IS ALSO ITS LANDING SECTION, so it is found by that
    // section's words as well as the workspace's, and locked by that
    // section's grants where the workspace names none of its own.
    ...keywordsOf([...(row.keywords ?? []), ...(own?.keywords ?? [])]),
    ...grantsOf(row.grants ?? own?.grants),
  };
  const sections = row.sections
    .filter((s) => !s.elsewhere && !samePath(s.path, row.path))
    .map((s): Destination => ({
      key: `${row.key}-${s.key}`,
      workspace: row.key,
      label: s.label,
      icon: s.icon,
      path: s.path,
      hint: s.hint,
      ...keywordsOf(s.keywords),
      ...grantsOf(s.grants),
    }));
  return [landing, ...sections];
});

/**
 * The section a path is in: the section whose path is the LONGEST prefix of
 * it. `#/live/turns/abc` is in Turns, `#/live` is in Now running, and a path
 * under no section (a project, an item) is in the workspace's landing section
 * only where that section's path is a prefix of it: `#/work/ENG` is in
 * Projects, which is where the reader reached it from.
 */
export function sectionOf(path: string[]): Section | undefined {
  const row = workspaceRow(workspaceOf(path));
  if (!row) return undefined;
  let best: Section | undefined;
  for (const section of row.sections) {
    if (section.elsewhere) continue;
    if (section.path.length > path.length) continue;
    if (!section.path.every((seg, i) => seg === path[i])) continue;
    if (!best || section.path.length > best.path.length) best = section;
  }
  return best;
}

/**
 * Whether a reader holding `held` may open what `needs` names: anything when
 * it names nothing, and otherwise when they hold ANY ONE of them.
 */
export function grantsOpen(needs: readonly Grant[] | undefined, held: readonly string[]): boolean {
  if (!needs || needs.length === 0) return true;
  return needs.some((grant) => held.includes(grant));
}

/** What a lock says it needs: `config:read`, or `people:manage or audit:read`. */
export function grantWords(needs: readonly Grant[]): string {
  return needs.join(" or ");
}

function grantsOf(needs: readonly Grant[] | undefined): { grants?: readonly Grant[] } {
  return needs && needs.length > 0 ? { grants: needs } : {};
}

function keywordsOf(words: readonly string[] | undefined): { keywords?: readonly string[] } {
  return words && words.length > 0 ? { keywords: words } : {};
}

function samePath(a: readonly string[], b: readonly string[]): boolean {
  return a.length === b.length && a.every((seg, i) => seg === b[i]);
}

/**
 * A CROSS-LINK to `target` — a section of another workspace, or another
 * workspace's landing — drawn in this workspace's renderer at `at`.
 *
 * DERIVED, NEVER RESTATED: its label, glyph, address, hint, grants and whether
 * it is the reader's own are the row it leads to, so one place has one
 * description wherever it is offered. Written out by hand, Settings' Account
 * row said "two-step verification" where the palette and the user block's
 * popover said "second factor", and its Budgets row said nothing but where
 * Budgets live.
 */
function crossLink(target: Section | WorkspaceRow, at: { key: string; group: string }): Section {
  return {
    key: at.key,
    group: at.group,
    label: target.label,
    icon: target.icon,
    path: target.path,
    hint: target.hint,
    elsewhere: true,
    ...grantsOf(target.grants),
    ...(target.personal ? { personal: true } : {}),
  };
}

/**
 * Whether an address IS a sidebar row: the landing of a workspace drawn as a
 * row — any place but the menu — the empty fragment being Home's.
 *
 * THE ONE ANSWER to "is this already in the sidebar?", which the page bar's
 * star asks. It was "is the path one segment long", written twice, and the
 * reader's Account is one segment long with no row at all: its star was
 * disabled as "Already in the sidebar" on the one page a reader looking for
 * where Account lives most needs a way to keep.
 */
export function isSidebarRow(path: readonly string[]): boolean {
  const at = path.length === 0 ? ["home"] : path;
  return WORKSPACES.some((row) => row.place !== "menu" && samePath(row.path, at));
}

/**
 * The grant every company surface reads the company through: the live socket
 * needs it (`internal/api/stream`), and with it every screen that is not the
 * reader's own. `lib/frameSession.ts` names it for the socket's sake.
 */
export const COMPANY_GRANT: Grant = "state:read";

/**
 * Whether a reader holding `held` may open a section: anything that is the
 * reader's own ([Section.personal]), and otherwise the company's grant AND
 * any one of the section's own.
 *
 * ONE RULE for every lock the navigation draws and every refusal the frame
 * dispatches: the sidebar's rows, the Settings column and `App.tsx`'s screen
 * all ask it. The sidebar's Settings row used to carry a key for any section
 * the reader could not open, so a person holding nothing but their own
 * Account was told Settings was closed to them — while its first group, You,
 * needs nothing at all.
 */
export function sectionOpen(
  section: Pick<Section, "grants" | "personal">,
  held: readonly string[],
): boolean {
  if (section.personal) return true;
  return held.includes(COMPANY_GRANT) && grantsOpen(section.grants, held);
}

/**
 * What a reader holding `held` lacks to open a section, as its lock says it:
 * nothing; the company's grant, first, since nothing else opens without it; or
 * the section's own.
 */
export function grantsMissing(
  section: Pick<Section, "grants" | "personal">,
  held: readonly string[],
): readonly Grant[] {
  if (sectionOpen(section, held)) return [];
  if (!held.includes(COMPANY_GRANT)) return [COMPANY_GRANT];
  return section.grants ?? [];
}

/** A workspace's own sections — never a cross-link, which is another workspace's. */
function ownSections(row: WorkspaceRow): Section[] {
  return row.sections.filter((s) => !s.elsewhere);
}

/**
 * Whether a workspace is open to a reader holding `held`: when it is the
 * reader's own, when any of its own sections is, or — where it has none — when
 * its landing is; and a workspace that names grants of its own needs one of
 * them too, beside whatever its section needs. A workspace only part of which
 * is closed is not locked as a whole: its column or its screens draw the lock
 * where the closed part is.
 */
export function workspaceOpen(row: WorkspaceRow, held: readonly string[]): boolean {
  if (row.personal) return true;
  const own = ownSections(row);
  if (own.some((s) => s.personal)) return true;
  if (!sectionOpen({ grants: row.grants }, held)) return false;
  return own.length === 0 || own.some((s) => sectionOpen(s, held));
}

/**
 * What a closed workspace's lock names, as [grantsMissing] does for a
 * section: nothing when it is open; the company's grant first; then the
 * workspace's own; then every grant that would open one of its sections, once
 * each.
 */
export function workspaceMissing(row: WorkspaceRow, held: readonly string[]): readonly Grant[] {
  if (workspaceOpen(row, held)) return [];
  const whole = grantsMissing({ grants: row.grants }, held);
  if (whole.length > 0) return whole;
  return [...new Set(ownSections(row).flatMap((s) => grantsMissing(s, held)))];
}

/**
 * Whether an address is the READER'S OWN — the Account, Settings › Appearance
 * — which the frame draws for every signed-in reader, the engine serving them
 * no company included.
 */
export function personalPath(path: string[]): boolean {
  if (workspaceRow(workspaceOf(path))?.personal) return true;
  return sectionOf(path)?.personal === true;
}
