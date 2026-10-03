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
 * a column, because it has eight of its own and a cross-link to Budgets, in
 * three groups — a tab strip of nine is a row nobody reads to the end of), and its
 * OBJECTS — a unit, a container, a node — are rows on the section that lists
 * them. What is left for the sidebar is the part every workspace shares: where
 * you can go, and the few objects this reader keeps coming back to.
 *
 * # The three nouns, stated once and asserted in router.test.ts
 *
 *   - A SIDEBAR ROW is a workspace, or a live shortcut to a project, a pinned
 *     view or a starred page.
 *   - A SECTION is a PATH segment inside a workspace, drawn by one renderer —
 *     `tabs` in the page header, `column` for Settings, or `tree` for
 *     Knowledge, whose column also holds its spaces and pages.
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

/** The workspaces, which are the sidebar's navigation rows. */
export type Workspace =
  "home" | "inbox" | "me" | "work" | "agents" | "live" | "knowledge" | "spend" | "settings";

/**
 * How a workspace draws its sections: `tabs` in the page header, a `column`
 * of them beside the screen (Settings), or a `tree` — Knowledge, whose column
 * is its spaces and pages as well as its sections, drawn by the workspace's
 * own chunk (`routes/knowledge/KnowledgeTree.tsx`).
 */
export type SectionRenderer = "tabs" | "column" | "tree" | "none";

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
  /** The whole workspace is operator-scoped: drawn with a lock, never hidden. */
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
    hint: "The first look: what needs you, who is working, how the company is doing",
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
        hint: "What this person holds, by due date or in the order somebody put it",
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
        hint: "Work where this person asked a question still waiting for its answer",
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
        hint: "Checklist items on other people's tasks that name this person",
      },
    ],
  },
  {
    key: "work",
    label: "Work",
    icon: "square-kanban",
    path: ["work"],
    hint: "Projects and tasks, with agents as first-class assignees",
    chord: "w",
    place: "workspace",
    renderer: "tabs",
    sections: [
      {
        key: "tasks",
        label: "Tasks",
        icon: "list",
        path: ["work"],
        hint: "Every task in the company, as a list, a board, a timeline, a calendar or a table",
      },
      {
        // THE CONTAINERS, AS A PLACE OF THEIR OWN. An overview of the projects
        // is a different question from "what is there", and a different
        // question is a different page.
        key: "projects",
        label: "Projects",
        icon: "columns-3",
        path: ["work", "projects"],
        hint: "Every project, who leads it, and how far along it is",
      },
      {
        key: "views",
        label: "Saved views",
        icon: "layout-dashboard",
        path: ["work", "views"],
        hint: "Every saved view, who owns it and which are pinned",
      },
      {
        key: "history",
        label: "Every change",
        icon: "chart-no-axes-gantt",
        path: ["work", "history"],
        hint: "Every change to the company's work, over a window you choose",
      },
      {
        key: "search",
        label: "Search",
        icon: "search",
        path: ["work", "search"],
        hint: "Rank the company's work against a phrase, the way an agent does",
      },
    ],
  },
  {
    key: "agents",
    label: "Agents",
    icon: "network",
    path: ["agents"],
    hint: "The org chart, which is the execution graph, and every seat on it",
    chord: "a",
    place: "workspace",
    renderer: "tabs",
    // THE ONE HUE RULE, said where the colour is: every ring, dot and state
    // line on these sections is what a seat is DOING, and no seat has a
    // colour of its own.
    note: "Colour shows what a seat is doing — never who it is",
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
        hint: "Every seat, what it is doing, and who is carrying how much",
      },
      {
        key: "teams",
        label: "Teams",
        icon: "users",
        path: ["agents", "teams"],
        hint: "Every unit, what it is for and the goals it was given",
      },
      {
        key: "schedules",
        label: "Schedules",
        icon: "calendar-clock",
        path: ["agents", "schedules"],
        hint: "Recurring work, when it next fires and how it last went",
      },
      {
        // THE ONE SECTION THAT WRITES, so it is an address and a button rather
        // than a tab: a reader arrives at Agents to look.
        key: "edit",
        label: "Edit org",
        icon: "pencil",
        path: ["agents", "edit"],
        hint: "Change the company's shape: units, seats and who reports to whom",
        guarded: true,
        answersRefusal: true,
        tab: false,
      },
    ],
  },
  {
    key: "live",
    label: "Live",
    icon: "activity",
    path: ["live"],
    hint: "Every turn as a trace: what ran, what it cost in tokens, and why",
    chord: "l",
    place: "workspace",
    renderer: "tabs",
    sections: [
      {
        key: "now",
        label: "Now running",
        icon: "zap",
        path: ["live"],
        hint: "What the company is doing at this moment, and the phases it just finished",
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
        hint: "Detached sandbox runs, live and finished",
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
        hint: "Everything the engine published, filterable and paged",
      },
    ],
  },
  {
    key: "knowledge",
    label: "Knowledge",
    icon: "book-open",
    path: ["knowledge"],
    hint: "The company's own pages, searched the way an agent searches them",
    chord: "k",
    place: "workspace",
    // A TREE, the approved Knowledge layout: the search, its mode and every
    // space's pages in a column beside whatever is open, so a reader moving
    // from a hit to its neighbours never loses their place in the tree.
    renderer: "tree",
    sections: [
      {
        key: "search",
        label: "Search",
        icon: "search",
        path: ["knowledge"],
        hint: "Search the knowledge base by words, meaning or both, and browse its spaces",
      },
      {
        key: "skills",
        label: "Agent skills",
        icon: "wand-sparkles",
        path: ["knowledge", "skills"],
        hint: "The tool skills the engine offers agents, who loaded each, and what each agent learned",
      },
      {
        key: "diaries",
        label: "Agent diaries",
        icon: "brain",
        path: ["knowledge", "diaries"],
        hint: "Every agent's diary and episodes, read from the node holding the agent",
      },
    ],
  },
  {
    key: "spend",
    label: "Spend",
    icon: "coins",
    path: ["spend"],
    hint: "Tokens by phase, agent, model and team, and the budgets they count against",
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
        hint: "The tasks that have spent the most tokens, and what drove each",
      },
      {
        key: "budgets",
        label: "Budgets",
        icon: "sliders-vertical",
        path: ["spend", "budgets"],
        hint: "Day, week and month ceilings, and what they are refusing",
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
        hint: "The people in the chart, how agents reach them, and the API tokens that act as them",
        group: "Company",
        guarded: true,
      },
      {
        key: "budgets",
        label: "Budgets",
        icon: "sliders-vertical",
        path: ["spend", "budgets"],
        hint: "Budgets have one address, under Spend",
        group: "Company",
        elsewhere: true,
      },
      {
        key: "integrations",
        label: "Integrations",
        icon: "plug",
        path: ["settings", "integrations"],
        hint: "The surfaces agents work on, and whether traffic is arriving",
        group: "Connect",
        guarded: true,
      },
      {
        key: "tools",
        label: "Tools & MCP",
        icon: "wrench",
        path: ["settings", "tools"],
        hint: "Every tool a seat can call, by origin, and the MCP servers behind them",
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
        hint: "The models seats run on, the keys each rotates through, and which a vendor is refusing now",
        group: "Connect",
        guarded: true,
      },
      {
        key: "secrets",
        label: "Secrets",
        icon: "key",
        path: ["settings", "secrets"],
        hint: "The company's credentials — names and provenance, never values",
        group: "Connect",
        guarded: true,
      },
      {
        key: "nodes",
        label: "Nodes",
        icon: "server",
        path: ["settings", "nodes"],
        hint: "Nodes, seat leases, duties and config rollout",
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
        hint: "Each state-log domain, what is holding its trim, and every node's place in it",
        group: "Engine",
        guarded: true,
      },
      {
        key: "audit",
        label: "Audit log",
        icon: "shield",
        path: ["settings", "audit"],
        hint: "Every write a person or a token made, across all four subsystems",
        group: "Engine",
        guarded: true,
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
  "projects",
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
 * only where that section's path is a prefix of it — `#/work/ENG` is in Tasks,
 * which is where the reader reached it from.
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
