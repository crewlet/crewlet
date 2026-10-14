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
 * a column, because its own row in WORKSPACES holds more sections, in three
 * groups, than a tab strip can — a row nobody reads to the end of), and its
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

/** The workspaces, which are the sidebar's navigation rows. */
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
  | "settings";

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

/** Where a workspace's row sits in the sidebar. */
export type SidebarPlace = "you" | "workspace" | "foot";

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
   * Everything under this section needs an operator credential: the
   * navigation draws its key mark beside it and never hides it, and the frame
   * draws ONE refusal (`OperatorRequired`) in place of the screen for a viewer
   * the engine has said holds none — a screen that mounted anyway was refused
   * and drew the refusal as zeros. A section only PART of which is guarded is
   * not: its screen says so where the guarded part is.
   */
  guarded?: boolean;
  /**
   * The SCREEN answers the refusal rather than the frame. One section does:
   * Edit org, whose draft is kept in the tab's storage and FORGOTTEN when the
   * token changes under it, which only a mounted builder hears — replacing it
   * with the frame's refusal the moment a token was cleared would leave the
   * draft for whoever sets the next one.
   */
  answersRefusal?: boolean;
  /** Drawn in the renderer (default), or an address reached another way. */
  tab?: boolean;
  /** The Settings column's group heading. */
  group?: string;
  /**
   * A CROSS-LINK: a row in this workspace's renderer whose address belongs to
   * another workspace — Settings › Company lists Budgets, which lives once,
   * under Spend. It is drawn with an arrow and is never this workspace's.
   */
  elsewhere?: boolean;
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
   * The whole workspace is operator-scoped: its sidebar row is drawn with a
   * lock, never hidden, and the palette says its landing needs a token. Every
   * one of its sections is guarded too (`router.test.ts`), because the
   * frame's refusal reads the SECTION a path is in, never the row.
   */
  guarded?: boolean;
  /**
   * A sentence at the end of the section tabs that holds for every section —
   * how to read what the workspace draws. It gives way to the tabs: drawn only
   * where the strip has room for it beside every tab (`noteFits` in
   * `header/PageHeader.tsx`), so never on a phone.
   */
  note?: string;
}

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
        hint: "The hierarchy every seat works inside",
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
        guarded: true,
        answersRefusal: true,
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
    //
    // GUARDED WHOLE, reads included: every chat question is answered only
    // for a credential, because the seat a reader is is resolved FROM it —
    // there is no anonymous chat, not even of a public room. So the frame
    // draws its one refusal in place of every chat page for a reader the
    // engine says holds none, as it does for Settings' guarded sections, and
    // the screen answers only what is chat's own: a token no seat is bound to.
    key: "chat",
    label: "Chat",
    icon: "message-square",
    path: ["chat"],
    hint: "The company's own rooms, threads and direct messages",
    chord: "c",
    place: "workspace",
    renderer: "rail",
    guarded: true,
    sections: [
      {
        key: "rooms",
        label: "Rooms",
        icon: "hash",
        path: ["chat"],
        hint: "Your rooms, newest activity first",
        guarded: true,
      },
      {
        key: "mentions",
        label: "Mentions",
        icon: "bell",
        path: ["chat", "mentions"],
        hint: "Every message that named you — the whole away story, because nothing is sent to you",
        guarded: true,
      },
      {
        key: "search",
        label: "Search chat",
        icon: "search",
        path: ["chat", "search"],
        hint: "Rank what has been said in every room you can read",
        guarded: true,
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
      },
      {
        key: "events",
        label: "Event log",
        icon: "list",
        path: ["live", "events"],
        hint: "Every engine event",
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
      {
        key: "budgets",
        label: "Budgets",
        icon: "sliders-vertical",
        path: ["spend", "budgets"],
        hint: "Daily, weekly and monthly token limits",
      },
    ],
  },
  {
    // LAST AND LOCKED, and the row is never hidden: a section that vanishes
    // when a credential is absent is indistinguishable from one that does not
    // exist, so an operator on a fresh browser would conclude the product has
    // nowhere to configure anything. General is the one section a reader without a
    // credential can read — the charter is the org projection's, which is
    // public — so the workspace is not guarded as a whole; its operator
    // sections are.
    key: "settings",
    label: "Settings",
    icon: "sliders-vertical",
    path: ["settings"],
    hint: "Everything an operator configures, in one place",
    chord: "s",
    place: "foot",
    renderer: "column",
    sections: [
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
        hint: "People, their contacts and API tokens",
        group: "Company",
        guarded: true,
      },
      {
        key: "budgets",
        label: "Budgets",
        icon: "sliders-vertical",
        path: ["spend", "budgets"],
        hint: "Opens Spend › Budgets",
        group: "Company",
        elsewhere: true,
      },
      {
        key: "integrations",
        label: "Integrations",
        icon: "plug",
        path: ["settings", "integrations"],
        hint: "Connected tools and their status",
        group: "Connect",
        guarded: true,
      },
      {
        key: "tools",
        label: "Tools & MCP",
        icon: "wrench",
        path: ["settings", "tools"],
        hint: "Agent tools and MCP servers",
        group: "Connect",
        // NOT GUARDED: the registry is the `tools` push every reader gets. Only
        // an MCP server's template reads the guarded configuration, and that
        // panel says so itself — a key mark here told an anonymous reader the
        // list they were looking at was closed to them.
      },
      {
        key: "models",
        label: "Models & keys",
        icon: "cpu",
        path: ["settings", "models"],
        hint: "Models and their API keys",
        group: "Connect",
        guarded: true,
      },
      {
        key: "secrets",
        label: "Secrets",
        icon: "key",
        path: ["settings", "secrets"],
        hint: "The company’s stored credentials",
        group: "Connect",
        guarded: true,
      },
      {
        key: "nodes",
        label: "Nodes",
        icon: "server",
        path: ["settings", "nodes"],
        hint: "Engine nodes, the broker and file storage",
        group: "Engine",
        guarded: true,
      },
      {
        key: "config",
        label: "Configuration",
        icon: "code",
        path: ["settings", "config"],
        hint: "The active company revision, its history and its diffs",
        group: "Engine",
        guarded: true,
      },
      {
        key: "backups",
        label: "Backups & retention",
        icon: "database",
        path: ["settings", "backups"],
        hint: "Backups and how long records are kept",
        group: "Engine",
        guarded: true,
      },
      {
        key: "audit",
        label: "Audit log",
        icon: "shield",
        path: ["settings", "audit"],
        hint: "Every change made by people and tokens",
        group: "Engine",
        guarded: true,
      },
      {
        // THE READER'S OWN, at the foot: how the console looks in this
        // browser, which no operator configures and nobody else sees. Not
        // guarded, because it reads and writes nothing of the company's.
        key: "appearance",
        label: "Appearance",
        icon: "palette",
        path: ["settings", "appearance"],
        hint: "Mode, density and theme, in this browser only",
        group: "Personal",
      },
    ],
  },
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
 * Every lowercase segment the route table spells, held apart from the keys the
 * engine mints.
 *
 * Project and container keys are UPPERCASE, item keys are `KEY-n`, everything
 * else the engine mints is a uuid — and every segment here is lowercase. That
 * is what makes `#/work/views` a list of saved views rather than a project
 * called VIEWS, without a single escape character in the route table. A
 * handle is lowercase too, which is why handles live ONLY under `seats/`,
 * unit names only under `teams/` and domains only under `backups/`: none of
 * them is ever read at a position where a reserved word is. (A unit is
 * addressed by its NAME — its `id:` is a guarded config field.)
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
 */
export interface Destination {
  key: string;
  workspace: Workspace;
  label: string;
  icon: GlyphName;
  path: string[];
  hint: string;
  guarded?: boolean;
}

export const DESTINATIONS: Destination[] = WORKSPACES.flatMap((row) => {
  const landing: Destination = {
    key: row.key,
    workspace: row.key,
    label: row.label,
    icon: row.icon,
    path: row.path,
    hint: row.hint,
    ...(row.guarded ? { guarded: true } : {}),
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
      ...(s.guarded ? { guarded: true } : {}),
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

function samePath(a: string[], b: string[]): boolean {
  return a.length === b.length && a.every((seg, i) => seg === b[i]);
}
