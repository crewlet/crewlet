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
 *     `column` for Settings, `rail` for Chat, whose screen draws them beside
 *     its rooms, or `none` where the workspace's own screens lead to it
 *     (Knowledge's landing).
 *   - An OBJECT TAB is a `tab=` query on an object's own page, bound to 1–9.
 *
 * Nothing is two of those. A section is never a sidebar row and never a
 * query; an object tab is never a path. A filter — a reason, a scope, a
 * status — is none of the three and lives in the page's own bar.
 *
 * # The order is the argument
 *
 * You first — what needs you, what reached you, what is yours — then the
 * company's work, its people, what they say to each other, what is running,
 * what it knows and what it spent, and the machine last. A founder opening
 * this meets their own decisions first and the engine last, because the
 * company is the product and the engine is what runs it.
 */

import type { GlyphName } from "@crewlethq/icons/glyphs";
import { holdsAny, type Grant } from "~/lib/access.ts";

/** The workspaces: the sidebar's navigation rows, and the reader's own Account. */
export type Workspace =
  | "home"
  | "inbox"
  | "me"
  | "work"
  | "agents"
  | "chat"
  | "live"
  | "knowledge"
  | "spend"
  | "settings"
  | "account";

/**
 * How a workspace draws its sections: `tabs` in the page header, a `column`
 * of them beside the screen (Settings), a `rail`, or `none`, where the
 * workspace's own screens lead to its sections (Home, Inbox, and Knowledge,
 * whose landing links its two sections under "For the agents").
 *
 * `lenses` is Work's. Its sections are the lenses of the scope the trail
 * names, All projects, drawn beside the trail's switcher the way a project
 * draws its own lenses, so the bar reads one way in either scope and Work
 * keeps no row of tabs of its own.
 *
 * `rail` is Chat's, whose SCREEN draws its sections at the head of the list
 * of its objects, the rooms (`routes/chat/Chat.tsx`), from this table. The
 * frame draws nothing for a rail, neither a strip nor a column, because the
 * tabs are drawn on a section's own page and never on an object's, and a room
 * is the object page a chat reader spends their time on. It is not `none`
 * because the screen that draws it reads the renderer: Chat's rail is drawn
 * from the row that says `rail`, every section of it, and a `none` row says
 * no frame or screen owes it a strip.
 */
export type SectionRenderer = "tabs" | "lenses" | "column" | "rail" | "none";

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
   * The PERMISSIONS that open this section — ANY ONE of them (`lib/access.ts`,
   * which counts every app an admin carries): `admin` for what runs the
   * company and the engine and for the company-wide transcripts, an app's
   * permission for what reads that app. A section naming none is the
   * OVERVIEW, which every signed-in principal reads — the org chart, agents'
   * status, spend, schedules — and the socket is dialled for all of them. The
   * navigation draws a lock beside a section the reader holds none of and
   * never hides it, and the frame draws ONE refusal (`GrantRequired`) in place
   * of the screen for a viewer the engine has said holds none — a screen that
   * mounted anyway was refused and drew the refusal as zeros. NAMED rather
   * than a flag, because the lock says which access to ask an admin for. A
   * section only PART of which needs more is not marked: its screen says so
   * where the guarded part is.
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
   * (a refused socket: a person whose seat was taken out of the chart). See
   * [sectionOpen].
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
  /**
   * The permissions the whole workspace needs, as [Section.grants]: locked,
   * never hidden. An APP's workspace names its app — Inbox, My work and Work
   * are the tracker's, Knowledge the knowledge base's — because every screen
   * in it reads that app; the one address under one that the app does not
   * gate is a work item somebody was ASKED about (`App.tsx`), which its own
   * read decides.
   */
  grants?: readonly Grant[];
  /** The reader's own, as [Section.personal]: the Account. */
  personal?: boolean;
  /**
   * NO STAR IN THE BAR on this workspace's pages. A workspace's own page is a
   * sidebar row already, and the frame draws its star there unavailable only
   * so the bar holds still between the workspace's sections; a workspace with
   * no sections has nothing to hold still for, and its bar carries its own
   * controls instead (Home's Edit Home).
   */
  starless?: boolean;
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
    starless: true,
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
    // THE TRACKER'S: the inbox is the notices the tracker routed to the
    // reader's record, and nothing else lands there.
    grants: ["tracker"],
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
    // THE TRACKER'S: every tab is a question about somebody's work items.
    grants: ["tracker"],
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
    // THE TRACKER'S, every lens and every project — but not an item somebody
    // was ASKED about, which `App.tsx` lets them open: the engine answers the
    // person an open ask names without the tracker, so they can read what they
    // were asked and answer it.
    grants: ["tracker"],
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
        // NO PERMISSION: the builder answers for itself, saying which of its
        // saves the reader reaches — an admin's whole company, a lead's own
        // units — rather than the frame locking it whole. Its draft is kept in
        // the tab's storage, and a frame refusal drawn the moment admin was
        // withdrawn would take the draft off screen while the person who
        // wrote it is still there to copy it out.
        tab: false,
      },
    ],
  },
  {
    // AFTER THE PEOPLE AND BEFORE WHAT THEY KNOW, which is the order the
    // sidebar argues: the company, then the talking, then what the talking
    // produced.
    //
    // A RAIL, and the screen draws it. Chat's rooms are its own object list,
    // drawn beside the transcript because they carry unread counts, mute
    // state and a preview that no tab can hold — and a room is an OBJECT
    // page, where the header draws no section tabs, so tabs would leave the
    // mention feed two presses from every room. The two lists that are not a
    // room sit at the head of the rail instead, drawn from the sections
    // below: the landing's label heads the rooms, and every other section is
    // a link under the name it has here.
    key: "chat",
    label: "Chat",
    icon: "message-square",
    path: ["chat"],
    hint: "The company's own rooms, threads and direct messages",
    keywords: ["messages", "rooms", "channels", "direct message", "dm"],
    chord: "c",
    place: "workspace",
    renderer: "rail",
    // THE CHAT APP'S, every section and every room: each reads somebody's
    // rooms as the seat the session holds, so the frame draws its one refusal
    // in place of every chat page for a reader without it. What the screen
    // answers is only what is chat's own: a company whose chat is elsewhere,
    // and a credential acting as no seat.
    grants: ["chat"],
    sections: [
      {
        key: "rooms",
        label: "Rooms",
        icon: "hash",
        path: ["chat"],
        hint: "Your rooms, newest activity first",
      },
      {
        key: "mentions",
        label: "Mentions",
        icon: "bell",
        path: ["chat", "mentions"],
        hint: "Every message that named you — the whole away story, because nothing is sent to you",
      },
      {
        key: "search",
        label: "Search chat",
        icon: "search",
        path: ["chat", "search"],
        hint: "Rank what has been said in every room you can read",
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
        // THE COMPANY-WIDE TRAIL is admin's: a lead reads their own seats'
        // turns on each seat's page, never everybody's here.
        grants: ["admin"],
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
        grants: ["admin"],
      },
      {
        key: "events",
        label: "Event log",
        icon: "list",
        path: ["live", "events"],
        hint: "Every engine event",
        grants: ["admin"],
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
    // THE KNOWLEDGE BASE'S: every section reads its pages.
    grants: ["knowledge"],
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
        // EVERY SEAT'S MEMORY AT ONCE is admin's; a lead reads their own
        // seats' memory on each seat's page.
        grants: ["admin"],
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
        // A TASK'S KEY AND TITLE are the tracker's; the overview's spend is
        // everybody's.
        grants: ["tracker"],
      },
      BUDGETS,
    ],
  },
  {
    // LAST AND LOCKED, and the row is never hidden: a section that vanishes
    // when a credential is absent is indistinguishable from one that does not
    // exist, so an operator on a fresh browser would conclude the product has
    // nowhere to configure anything. General is the one company section every
    // reader can read — the charter is the org projection's — so the
    // workspace names no permission as a whole; every other company section
    // is admin's, and says so on its own row.
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
        // are what they look for first. No permission, because it reads and
        // writes nothing of the company's.
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
        grants: ["admin"],
      },
      crossLink(BUDGETS, { key: "budgets", group: "Company" }),
      {
        key: "integrations",
        label: "Integrations",
        icon: "plug",
        path: ["settings", "integrations"],
        hint: "Connected tools and their status",
        group: "Connect",
        grants: ["admin"],
      },
      {
        key: "tools",
        label: "Tools & MCP",
        icon: "wrench",
        path: ["settings", "tools"],
        hint: "Agent tools and MCP servers",
        group: "Connect",
        // ADMIN'S, like every company setting: the page is where MCP servers
        // are added and their templates read, and the overview's own `tools`
        // push already says which tools each seat has on its page.
        grants: ["admin"],
      },
      {
        key: "models",
        label: "Models & keys",
        icon: "cpu",
        path: ["settings", "models"],
        hint: "Models and their API keys",
        group: "Connect",
        grants: ["admin"],
      },
      {
        key: "secrets",
        label: "Secrets",
        icon: "key",
        path: ["settings", "secrets"],
        hint: "The company’s stored credentials",
        group: "Connect",
        grants: ["admin"],
      },
      {
        key: "nodes",
        label: "Nodes",
        icon: "server",
        path: ["settings", "nodes"],
        hint: "Engine nodes, the broker and file storage",
        group: "Engine",
        grants: ["admin"],
      },
      {
        key: "config",
        label: "Configuration",
        icon: "code",
        path: ["settings", "config"],
        hint: "The active company revision, its history and its diffs",
        group: "Engine",
        grants: ["admin"],
      },
      {
        key: "backups",
        label: "Backups & retention",
        icon: "database",
        path: ["settings", "backups"],
        hint: "Backups and how long records are kept",
        group: "Engine",
        grants: ["admin"],
      },
      {
        key: "audit",
        label: "Audit log",
        icon: "shield",
        path: ["settings", "audit"],
        hint: "Every change made by people and tokens",
        group: "Engine",
        grants: ["admin"],
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
  "mentions",
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
 *
 * IT CARRIES NO GRANTS: what closes one is its ADDRESS's, decided by
 * [closedBy] — the workspace's permissions first, then the section's — the
 * rule the frame's refusal and the sidebar's locks read. A section's own
 * list said nothing of the workspace around it, so every section of Work,
 * My work and Chat was offered with no lock to a reader its workspace was
 * closed to, and the press opened onto the refusal.
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
    // section's words as well as the workspace's.
    ...keywordsOf([...(row.keywords ?? []), ...(own?.keywords ?? [])]),
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
 * Whether a reader holding `held` may open a section: anything that is the
 * reader's own ([Section.personal]), and otherwise any one of the section's
 * permissions — through `lib/access.ts`'s `holdsAny`, so every app an admin
 * carries opens an app's section. A section naming none is the overview, and
 * open to every signed-in reader.
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
  return holdsAny(held, section.grants);
}

/**
 * What a reader holding `held` lacks to open a section, as its lock says it:
 * nothing, or the section's own permissions.
 */
export function grantsMissing(
  section: Pick<Section, "grants" | "personal">,
  held: readonly string[],
): readonly Grant[] {
  if (sectionOpen(section, held)) return [];
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
 * section: nothing when it is open; the workspace's own permissions; then
 * every permission that would open one of its sections, once each.
 */
export function workspaceMissing(row: WorkspaceRow, held: readonly string[]): readonly Grant[] {
  if (workspaceOpen(row, held)) return [];
  const whole = grantsMissing({ grants: row.grants }, held);
  if (whole.length > 0) return whole;
  return [...new Set(ownSections(row).flatMap((s) => grantsMissing(s, held)))];
}

/**
 * What closes an address to a reader holding `held`, as the frame's refusal
 * says it — the WORKSPACE's permissions first, since a reader who cannot open
 * Work cannot open any lens of it, then the SECTION's — or null where nothing
 * does. `what` is the label of whichever closed it.
 *
 * The caller decides what is exempt: a work item somebody was ASKED about is
 * opened whatever this says (`App.tsx`), because the engine answers the person
 * an open ask names without the tracker.
 */
export function closedBy(
  path: string[],
  held: readonly string[],
): { what: string; needs: readonly Grant[] } | null {
  const row = workspaceRow(workspaceOf(path));
  if (row && !row.personal && !holdsAny(held, row.grants)) {
    return { what: row.label, needs: row.grants ?? [] };
  }
  const section = sectionOf(path);
  if (section && !sectionOpen(section, held)) {
    return { what: section.label, needs: grantsMissing(section, held) };
  }
  return null;
}

/**
 * Whether an address names any permission at all — its workspace's or its
 * section's — so the frame waits for the reader's first answer before drawing
 * a screen that may be closed to them.
 */
export function guardedPath(path: string[]): boolean {
  const row = workspaceRow(workspaceOf(path));
  if (row?.personal) return false;
  return (row?.grants?.length ?? 0) > 0 || (sectionOf(path)?.grants?.length ?? 0) > 0;
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
