/**
 * The information architecture: TWO SHELLS in one table, and three nouns that
 * keep each of them one sidebar.
 *
 * # Two shells, chosen by the address
 *
 * The PERSON SHELL is where people work: their day, their inbox, their work,
 * the company's work, its team and what it knows. The ADMIN CONSOLE is where
 * the machine is run: what is running, how each agent is set up, what it
 * spends, the company's configuration, its connections and its engine. The
 * console lives under `#/admin/…` and everything else is the person shell —
 * [shellOf] reads the FIRST SEGMENT and nothing else.
 *
 * NOT A TOGGLE, AND NEVER THE ROLE. A toggle would put two renderings behind
 * one URL, so a link pasted to a colleague would open whichever one their
 * browser last chose; and a shell chosen by the role would show an admin who
 * is also a teammate the console on the screen where they answer a question.
 * The address picks the shell, the shell picks the voice, and an admin crosses
 * through a labelled door ([adminPathOf], [personPathOf]) that keeps their
 * place. A member following a console address is told it is an admin's, by
 * the frame (`frame/AdminRequired.tsx`) — never Not Found, which would say the
 * address is wrong when it is the reader's key that does not reach it.
 *
 * ONE TABLE, NOT TWO. The shells share a router, a resolver, a palette, a
 * socket and the chord namespace, so a second table would be a second place a
 * workspace's chord, its path or its phone placement lives — and the two
 * would disagree the first time a row moved. Each row says which shell it is
 * in; the sidebars, the phone's tab bar, the chords, the palette and the chunk
 * list are all DERIVED from that column.
 *
 * # The three nouns, stated once and asserted in router.test.ts
 *
 *   - A SIDEBAR ROW is a workspace, or a shortcut the reader keeps: a project
 *     under Work, a favourite, a running turn in the console.
 *   - A SECTION is a PATH segment inside a workspace, drawn by one renderer —
 *     `tabs` in the page header, or `tree` for Knowledge, whose column also
 *     holds its spaces and pages.
 *   - An OBJECT TAB is a `tab=` query on an object's own page, bound to 1–9.
 *
 * Nothing is two of those. A section is never a sidebar row and never a
 * query; an object tab is never a path. A filter — a reason, a scope, a
 * status — is none of the three and lives in the page's own bar.
 *
 * # The order is the argument
 *
 * In the person shell, you first — your day, what reached you, what is yours
 * — then the company's work, its team and what it knows. In the console, the
 * overview first, then what is running, how it is set up and what it spends,
 * then the company, what it is connected to, and the engine last.
 */

import type { GlyphName } from "@crewlethq/icons/glyphs";

/** Which of the two shells a workspace — and so an address — is in. */
export type Shell = "person" | "admin";

/** The workspaces, which are the two sidebars' navigation rows. */
export type Workspace =
  // The person shell.
  | "today"
  | "inbox"
  | "me"
  | "work"
  | "team"
  | "knowledge"
  // The admin console.
  | "overview"
  | "live"
  | "setup"
  | "spend"
  | "company"
  | "connect"
  | "engine";

/**
 * How a workspace draws its sections: `tabs` in the page header, a `tree` —
 * Knowledge, whose column is its spaces and pages as well as its sections,
 * drawn by the workspace's own chunk (`routes/knowledge/KnowledgeTree.tsx`) —
 * or `none` for a workspace that is one page.
 */
export type SectionRenderer = "tabs" | "tree" | "none";

/**
 * Where a workspace's row sits in its sidebar: the person shell's unlabelled
 * "you" rows, its company group under the company's name, or the console's
 * one run of sections.
 */
export type SidebarPlace = "you" | "company" | "console";

/**
 * Where a workspace sits on a phone: a TAB of the bar along the bottom, a row
 * of the More sheet behind its last tab, or nowhere — the console has no
 * phone bar, because nobody runs an engine from a phone's tab bar.
 */
export type PhonePlace = "tab" | "more" | "none";

/**
 * One section of a workspace: a place with its own path.
 *
 * `tab: false` is a section that exists as an address and is reached some
 * other way.
 */
export interface Section {
  key: string;
  label: string;
  /**
   * A GLYPH NAME rather than a component: four consumers draw this row — the
   * header's tabs, the palette, the sidebars and the tests — and a table
   * holding components would make each of them carry the whole set. The
   * union keeps a misspelling a compile error.
   */
  icon: GlyphName;
  path: string[];
  /** Shown under the label in the command palette. */
  hint: string;
  /**
   * The SCREEN answers the console's refusal rather than the frame. One
   * section does: Edit org, whose draft is kept in the tab's storage and
   * FORGOTTEN when the token changes under it, which only a mounted builder
   * hears — replacing it with the frame's refusal the moment a token was
   * cleared would leave the draft for whoever sets the next one.
   */
  answersRefusal?: boolean;
  /** Drawn in the renderer (default), or an address reached another way. */
  tab?: boolean;
}

export interface WorkspaceRow {
  key: Workspace;
  /** The shell the workspace is in — and so the first segment of its path. */
  shell: Shell;
  label: string;
  /** A glyph name, for the reason [Section.icon] gives. */
  icon: GlyphName;
  /** The workspace's landing address. */
  path: string[];
  /** Shown under the label in the command palette. */
  hint: string;
  /**
   * The access key in the `g`-prefixed jump chord. UNIQUE ACROSS BOTH SHELLS:
   * an admin presses the console's chords from the person shell and the
   * person shell's from the console, and one letter meaning two places
   * depending on which shell you stood in would be a key nobody could learn.
   */
  chord: string;
  /** Where the sidebar draws the row. */
  place: SidebarPlace;
  /** Where a phone draws it. */
  phone: PhonePlace;
  renderer: SectionRenderer;
  sections: Section[];
  /**
   * Query keys that belong to the WORKSPACE rather than to one section, and
   * travel with the reader between its sections: whose day My work is reading
   * is the same question on every section of it.
   */
  keep?: string[];
  /**
   * A sentence at the end of the section tabs that holds for every section —
   * how to read what the workspace draws. It gives way to the tabs: drawn only
   * where the strip has room for it beside every tab (`noteFits` in
   * `header/PageHeader.tsx`), so never on a phone.
   */
  note?: string;
}

export const WORKSPACES: WorkspaceRow[] = [
  // ---------------------------------------------------------------- person
  {
    key: "today",
    shell: "person",
    label: "Today",
    icon: "sun",
    path: ["today"],
    hint: "Your day: what needs you, what is yours, and what reached you",
    chord: "t",
    place: "you",
    phone: "tab",
    renderer: "none",
    sections: [],
  },
  {
    key: "inbox",
    shell: "person",
    label: "Inbox",
    icon: "inbox",
    path: ["inbox"],
    hint: "What reached you, and what needs your answer",
    chord: "i",
    place: "you",
    phone: "tab",
    renderer: "none",
    sections: [],
  },
  {
    key: "me",
    shell: "person",
    label: "My work",
    icon: "circle-check",
    path: ["me"],
    hint: "Your queue, your questions, and the work you handed off",
    chord: "m",
    place: "you",
    phone: "tab",
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
        // WHAT YOU FILED FOR SOMEBODY ELSE, still open: the hand-offs a
        // founder follows, and the one list of them — Today's In flight is a
        // short head of this, never a second reading.
        key: "handed-off",
        label: "Handed off",
        icon: "send",
        path: ["me", "handed-off"],
        hint: "Open work this person filed and handed to somebody else",
      },
      {
        key: "ready",
        label: "Ready to start",
        icon: "zap",
        path: ["me", "ready"],
        hint: "Work whose blockers have all finished",
      },
      {
        key: "helping",
        label: "Helping on",
        icon: "users",
        path: ["me", "helping"],
        hint: "Tasks this person was brought onto without owning",
      },
      {
        key: "following",
        label: "Following",
        icon: "eye",
        path: ["me", "following"],
        hint: "Followed work that changed recently",
      },
      {
        key: "checklist",
        label: "Your checklist items",
        icon: "check",
        path: ["me", "checklist"],
        hint: "Checklist items on other people's tasks that name this person",
      },
    ],
  },
  {
    key: "work",
    shell: "person",
    label: "Work",
    icon: "square-kanban",
    path: ["work"],
    hint: "Projects and tasks, with agents as first-class assignees",
    chord: "w",
    place: "company",
    phone: "more",
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
        key: "search",
        label: "Search",
        icon: "search",
        path: ["work", "search"],
        hint: "Rank the company's work against a phrase, the way an agent does",
      },
    ],
  },
  {
    key: "team",
    shell: "person",
    label: "Team",
    icon: "users",
    path: ["team"],
    hint: "Who works here — people and agents — and the units they work in",
    chord: "p",
    place: "company",
    phone: "more",
    renderer: "tabs",
    // THE ONE HUE RULE, said where the colour is: every ring, dot and state
    // line on these sections is what an agent is DOING, and no agent has a
    // colour of its own.
    note: "Colour shows what an agent is doing — never who it is",
    sections: [
      {
        key: "chart",
        label: "Org chart",
        icon: "network",
        path: ["team"],
        hint: "Who reports to whom, and what each agent is doing",
      },
      {
        key: "people",
        label: "People & agents",
        icon: "list",
        path: ["team", "people"],
        hint: "Everybody in the company, what each is doing, and who is carrying how much",
      },
      {
        key: "units",
        label: "Units",
        icon: "users",
        path: ["team", "units"],
        hint: "Every unit, what it is for and the goals it was given",
      },
      {
        key: "about",
        label: "About",
        icon: "compass",
        path: ["team", "about"],
        hint: "The company's mission, vision and the policies every agent is given",
      },
    ],
  },
  {
    key: "knowledge",
    shell: "person",
    label: "Knowledge",
    icon: "book-open",
    path: ["knowledge"],
    hint: "The company's own pages, searched the way an agent searches them",
    chord: "k",
    place: "company",
    phone: "more",
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
    ],
  },
  // ----------------------------------------------------------------- admin
  {
    key: "overview",
    shell: "admin",
    label: "Overview",
    icon: "layout-dashboard",
    path: ["admin"],
    hint: "The company running: what needs an admin, who is working, and what it spends",
    chord: "o",
    place: "console",
    phone: "none",
    renderer: "none",
    sections: [],
  },
  {
    key: "live",
    shell: "admin",
    label: "Live",
    icon: "activity",
    path: ["admin", "live"],
    hint: "Every turn as a trace: what ran, what it cost in tokens, and why",
    chord: "l",
    place: "console",
    phone: "none",
    renderer: "tabs",
    sections: [
      {
        key: "now",
        label: "Now running",
        icon: "zap",
        path: ["admin", "live"],
        hint: "What the company is doing at this moment, and the phases it just finished",
      },
      {
        key: "turns",
        label: "Turns",
        icon: "brain",
        path: ["admin", "live", "turns"],
        hint: "Every turn the seats ran, round by round",
      },
      {
        key: "runs",
        label: "Coding runs",
        icon: "square-terminal",
        path: ["admin", "live", "runs"],
        hint: "Detached sandbox runs, live and finished",
      },
      {
        key: "a2a",
        label: "Agent-to-agent",
        icon: "link",
        path: ["admin", "live", "a2a"],
        hint: "The private channels seats opened with each other",
      },
      {
        key: "events",
        label: "Event log",
        icon: "list",
        path: ["admin", "live", "events"],
        hint: "Everything the engine published, filterable and paged",
      },
    ],
  },
  {
    key: "setup",
    shell: "admin",
    label: "Agent setup",
    icon: "sliders-vertical",
    path: ["admin", "agents"],
    hint: "Every seat as it runs: its turns, its memory, its schedules and its settings",
    chord: "u",
    place: "console",
    phone: "none",
    renderer: "tabs",
    sections: [
      {
        // KEYED AS TEAM'S PEOPLE & AGENTS IS, because it draws that screen and
        // the screen publishes its figure under that key (`useTeamCounts`).
        key: "people",
        label: "Seats",
        icon: "list",
        path: ["admin", "agents"],
        hint: "Every seat, what it is doing, and who is carrying how much",
      },
      {
        key: "schedules",
        label: "Schedules",
        icon: "calendar-clock",
        path: ["admin", "agents", "schedules"],
        hint: "Recurring work, when it next fires and how it last went",
      },
      {
        key: "skills",
        label: "Agent skills",
        icon: "wand-sparkles",
        path: ["admin", "agents", "skills"],
        hint: "The tool skills the engine offers agents, who loaded each, and what each agent learned",
      },
      {
        key: "memory",
        label: "Memory",
        icon: "brain",
        path: ["admin", "agents", "memory"],
        hint: "Every agent's diary and episodes, read from the node holding the agent",
      },
    ],
  },
  {
    key: "spend",
    shell: "admin",
    label: "Spend",
    icon: "coins",
    path: ["admin", "spend"],
    hint: "Tokens by phase, agent, model and team, and the budgets they count against",
    chord: "s",
    place: "console",
    phone: "none",
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
        path: ["admin", "spend"],
        hint: "Tokens over time, by phase, agent, model and team",
      },
      {
        key: "tasks",
        label: "Expensive tasks",
        icon: "layers",
        path: ["admin", "spend", "tasks"],
        hint: "The tasks that have spent the most tokens, and what drove each",
      },
      {
        key: "budgets",
        label: "Budgets",
        icon: "sliders-vertical",
        path: ["admin", "spend", "budgets"],
        hint: "Day, week and month ceilings, and what they are refusing",
      },
    ],
  },
  {
    key: "company",
    shell: "admin",
    label: "Company",
    icon: "building-complex",
    path: ["admin", "company"],
    hint: "The charter, the people and their keys, the org chart's shape, and every change made",
    chord: "c",
    place: "console",
    phone: "none",
    renderer: "tabs",
    sections: [
      {
        key: "charter",
        label: "Charter",
        icon: "compass",
        path: ["admin", "company"],
        hint: "The charter: mission, vision and the standing policies",
      },
      {
        key: "people",
        label: "People & access",
        icon: "users",
        path: ["admin", "company", "people"],
        hint: "The people in the chart, how agents reach them, and the API keys that act as them",
      },
      {
        // THE ONE SECTION THAT ANSWERS ITS OWN REFUSAL — see `answersRefusal`.
        key: "org",
        label: "Edit org",
        icon: "pencil",
        path: ["admin", "company", "org"],
        hint: "Change the company's shape: units, seats and who reports to whom",
        answersRefusal: true,
      },
      {
        key: "audit",
        label: "Audit log",
        icon: "shield",
        path: ["admin", "company", "audit"],
        hint: "Every write a person or a key made, across all four subsystems",
      },
      {
        // EVERY CHANGE TO THE COMPANY'S WORK, BY ANYBODY — the audit log beside
        // it narrows to what PEOPLE wrote; this is the tracker's whole log,
        // agents' commits included. A project's own History lens stays on the
        // project, in the person shell.
        key: "changes",
        label: "Every change",
        icon: "chart-no-axes-gantt",
        path: ["admin", "company", "changes"],
        hint: "Every change to the company's work, over a window you choose",
      },
    ],
  },
  {
    key: "connect",
    shell: "admin",
    label: "Connect",
    icon: "plug",
    path: ["admin", "connect"],
    hint: "The surfaces agents work on, the tools they call, the models they run on, and the secrets",
    chord: "n",
    place: "console",
    phone: "none",
    renderer: "tabs",
    sections: [
      {
        key: "integrations",
        label: "Integrations",
        icon: "plug",
        path: ["admin", "connect"],
        hint: "The surfaces agents work on, and whether traffic is arriving",
      },
      {
        key: "tools",
        label: "Tools & MCP",
        icon: "wrench",
        path: ["admin", "connect", "tools"],
        hint: "Every tool a seat can call, by origin, and the MCP servers behind them",
      },
      {
        key: "models",
        label: "Models & keys",
        icon: "cpu",
        path: ["admin", "connect", "models"],
        hint: "The models seats run on, the keys each rotates through, and which a vendor is refusing now",
      },
      {
        key: "secrets",
        label: "Secrets",
        icon: "key",
        path: ["admin", "connect", "secrets"],
        hint: "The company's credentials — names and provenance, never values",
      },
    ],
  },
  {
    key: "engine",
    shell: "admin",
    label: "Engine",
    icon: "server",
    path: ["admin", "engine"],
    hint: "Nodes, the configuration revisions, and backups",
    chord: "e",
    place: "console",
    phone: "none",
    renderer: "tabs",
    sections: [
      {
        key: "nodes",
        label: "Nodes",
        icon: "server",
        path: ["admin", "engine"],
        hint: "Nodes, seat leases, duties, config rollout, the broker and where the company's files are kept",
      },
      {
        key: "config",
        label: "Configuration",
        icon: "code",
        path: ["admin", "engine", "config"],
        hint: "The active company revision, its history and its diffs",
      },
      {
        key: "backups",
        label: "Backups & retention",
        icon: "database",
        path: ["admin", "engine", "backups"],
        hint: "Each state-log domain, what is holding its trim, and every node's place in it",
      },
    ],
  },
];

/** The row for a workspace, or undefined for a route nothing owns. */
export function workspaceRow(key: Workspace | ""): WorkspaceRow | undefined {
  return WORKSPACES.find((row) => row.key === key);
}

/** The workspaces of one shell, in the sidebar's order. */
export function workspacesOf(shell: Shell): WorkspaceRow[] {
  return WORKSPACES.filter((row) => row.shell === shell);
}

/** The console's head: every admin address starts with it, and nothing else does. */
export const ADMIN_HEAD = "admin";

/**
 * Which shell an address is in: THE FIRST SEGMENT, and nothing else — not the
 * role, not a preference, not where the reader came from. The empty fragment
 * is the person shell's, because it is where everybody lands first
 * ([landingFor]).
 */
export function shellOf(path: readonly string[]): Shell {
  return path[0] === ADMIN_HEAD ? "admin" : "person";
}

/**
 * Which workspace a route belongs to, so the sidebar marks the right row.
 *
 * The person shell's FIRST SEGMENT decides, and the console's SECOND — the
 * console's own head is the first. The console's Overview owns `#/admin`
 * exactly and nothing under it: a tail no console workspace names is
 * nothing, and marking Overview for it would tell the reader they were
 * somewhere they are not. The empty fragment is no workspace: it is the
 * landing, which [landingFor] turns into one.
 */
export function workspaceOf(path: readonly string[]): Workspace | "" {
  if (shellOf(path) === "admin") {
    if (path.length === 1) return "overview";
    return (
      WORKSPACES.find(
        (row) => row.shell === "admin" && row.path.length === 2 && row.path[1] === path[1],
      )?.key ?? ""
    );
  }
  const head = path[0];
  if (head === undefined) return "";
  return WORKSPACES.find((row) => row.shell === "person" && row.path[0] === head)?.key ?? "";
}

/**
 * Every lowercase segment the route table spells below a workspace's head,
 * held apart from the keys the engine mints.
 *
 * Project and container keys are UPPERCASE, item keys are `KEY-n`, everything
 * else the engine mints is a uuid — and every segment here is lowercase. That
 * is what makes `#/work/views` a list of saved views rather than a project
 * called VIEWS, without a single escape character in the route table. A
 * handle is lowercase too, which is why handles live ONLY under `seats/` and
 * `memory/`, unit names only under `units/` and domains only under
 * `backups/`: none of them is ever read at a position where a reserved word
 * is. (A unit is addressed by its NAME — its `id:` is a guarded config field.)
 */
export const RESERVED_SEGMENTS: string[] = [
  // The person shell.
  "projects",
  "views",
  "search",
  "asked-of-me",
  "asked-by-me",
  "handed-off",
  "ready",
  "helping",
  "following",
  "checklist",
  "people",
  "units",
  "about",
  "seats",
  "pages",
  // The console.
  "live",
  "turns",
  "runs",
  "a2a",
  "traces",
  "events",
  "agents",
  "schedules",
  "skills",
  "memory",
  "spend",
  "tasks",
  "budgets",
  "company",
  "org",
  "audit",
  "changes",
  "connect",
  "integrations",
  "tools",
  "servers",
  "models",
  "secrets",
  "engine",
  "config",
  "revisions",
  "backups",
];

/**
 * Every fixed destination — a workspace's landing page and each of its own
 * sections — for the command palette and for the tests that walk the product.
 *
 * DERIVED FROM [WORKSPACES], never written beside it: a palette that listed
 * its own copy of the sections is a second table that is wrong the day a
 * section moves. A section whose path IS its workspace's landing page is one
 * destination under the workspace's name.
 */
export interface Destination {
  key: string;
  workspace: Workspace;
  shell: Shell;
  label: string;
  icon: GlyphName;
  path: string[];
  hint: string;
}

export const DESTINATIONS: Destination[] = WORKSPACES.flatMap((row) => {
  const landing: Destination = {
    key: row.key,
    workspace: row.key,
    shell: row.shell,
    label: row.label,
    icon: row.icon,
    path: row.path,
    hint: row.hint,
  };
  const sections = row.sections
    .filter((s) => !samePath(s.path, row.path))
    .map((s): Destination => ({
      key: `${row.key}-${s.key}`,
      workspace: row.key,
      shell: row.shell,
      label: s.label,
      icon: s.icon,
      path: s.path,
      hint: s.hint,
    }));
  return [landing, ...sections];
});

/**
 * The section a path is in: the section whose path is the LONGEST prefix of
 * it. `#/admin/live/turns/abc` is in Turns, `#/admin/live` is in Now running,
 * and a path under no section (a project, an item) is in the workspace's
 * landing section only where that section's path is a prefix of it —
 * `#/work/ENG` is in Tasks, which is where the reader reached it from.
 */
export function sectionOf(path: readonly string[]): Section | undefined {
  const row = workspaceRow(workspaceOf(path));
  if (!row) return undefined;
  let best: Section | undefined;
  for (const section of row.sections) {
    if (section.path.length > path.length) continue;
    if (!section.path.every((seg, i) => seg === path[i])) continue;
    if (!best || section.path.length > best.path.length) best = section;
  }
  return best;
}

/** The workspaces a phone draws as its bottom bar's tabs, in the table's order. */
export function phoneTabs(): WorkspaceRow[] {
  return WORKSPACES.filter((row) => row.phone === "tab");
}

/** The workspaces a phone draws in the More sheet behind its last tab. */
export function phoneMore(): WorkspaceRow[] {
  return WORKSPACES.filter((row) => row.phone === "more");
}

// ---------------------------------------------------------------------------
// Where a reader lands, and how an admin crosses
// ---------------------------------------------------------------------------

/** What [landingFor] needs to know about the reader: the viewer's own answer. */
export interface LandingViewer {
  /** The first read is still out — there is no landing to choose yet. */
  asking: boolean;
  /** No answer is held: the first read is out, or the last one failed. */
  loading: boolean;
  anonymous: boolean;
  admin: boolean;
  /** A human seat links this key. */
  linked: boolean;
}

/**
 * Where `#/` takes this reader — or null while nobody has said who they are.
 *
 * ONE TESTED FUNCTION, because each reader has a different first useful
 * screen and a landing chosen anywhere else is chosen twice:
 *
 *  - ANONYMOUS lands on Today, which is the sign-in panel for them: the
 *    company's name and mission, and where a key goes.
 *  - A LINKED key — member or admin — lands on Today: a person with an admin
 *    key is still a person first, and their own questions are on Today, not
 *    in the console.
 *  - An UNLINKED MEMBER key lands on Work: it is somebody's key but no seat
 *    names it, so it has no day, no inbox and no queue — and the company's
 *    work is what it can read.
 *  - An UNLINKED ADMIN key lands on the console: it is a pipeline's or an
 *    operator's credential, and there is no person for the person shell to
 *    be about.
 *
 * NULL WHILE THE FIRST READ IS OUT rather than a guess: a guess is a
 * navigation, and a reader moved to the wrong shell before the answer arrived
 * would be moved again a moment later. A read that FAILED is no answer either,
 * and waiting on it would hold the reader on a placeholder until the next
 * poll — so they land on Today, which is the person shell's own first screen
 * and says itself what it could and could not read.
 */
export function landingFor(viewer: LandingViewer): string[] | null {
  if (viewer.asking) return null;
  if (viewer.loading) return ["today"];
  if (viewer.anonymous) return ["today"];
  if (viewer.linked) return ["today"];
  return viewer.admin ? [ADMIN_HEAD] : ["work"];
}

/**
 * The console's twin of a person-shell address, keeping the reader's place —
 * or the console's Overview where the place has no twin.
 *
 * A TABLE OF TWINS, held against [personPathOf] in both directions by
 * router.test.ts: Today ↔ Overview, a teammate's profile ↔ their seat in
 * Agent setup, a task ↔ its turns in Live, a unit ↔ its node in Edit org, the
 * people ↔ the seats, About ↔ the Charter. A place with no twin (a project,
 * a page) crosses to the Overview rather than to a console screen that would
 * read as being about it.
 */
export function adminPathOf(path: readonly string[]): Crossing {
  const [head, first, second] = path;
  switch (head) {
    case "today":
    case undefined:
      return { path: [ADMIN_HEAD] };
    case "team":
      if (first === "seats" && second) return { path: ["admin", "agents", "seats", second] };
      if (first === "people") return { path: ["admin", "agents"] };
      if (first === "about") return { path: ["admin", "company"] };
      if (first === "units" && second) {
        return { path: ["admin", "company", "org"], query: { unit: second } };
      }
      return { path: ["admin", "company", "org"] };
    case "work":
      // A TASK'S TURNS: the ones charged to it, in Live's list.
      if (first && /^[A-Z][A-Z0-9_]*-\d+$/.test(first) && path.length === 2) {
        return { path: ["admin", "live", "turns"], query: { item: first } };
      }
      return { path: [ADMIN_HEAD] };
    default:
      return { path: [ADMIN_HEAD] };
  }
}

/**
 * The person shell's twin of a console address — [adminPathOf] read backwards
 * — or Today where the place has no twin: what is open in the console is the
 * machine, and the person shell's own first screen is the honest way back.
 */
export function personPathOf(path: readonly string[], query?: URLSearchParams): Crossing {
  const [, first, second, third] = path;
  switch (first) {
    case undefined:
      return { path: ["today"] };
    case "agents":
      if (second === "seats" && third) return { path: ["team", "seats", third] };
      if (second === undefined) return { path: ["team", "people"] };
      return { path: ["today"] };
    case "company": {
      if (second === undefined) return { path: ["team", "about"] };
      if (second === "org") {
        const unit = query?.get("unit");
        const seat = query?.get("seat");
        if (unit) return { path: ["team", "units", unit] };
        if (seat) return { path: ["team", "seats", seat] };
        return { path: ["team"] };
      }
      return { path: ["today"] };
    }
    case "live": {
      const item = query?.get("item");
      if (second === "turns" && third === undefined && item) return { path: ["work", item] };
      return { path: ["today"] };
    }
    default:
      return { path: ["today"] };
  }
}

/** Where a door between the shells goes: an address, and the query that keeps the place. */
export interface Crossing {
  path: string[];
  query?: Record<string, string>;
}

function samePath(a: readonly string[], b: readonly string[]): boolean {
  return a.length === b.length && a.every((seg, i) => seg === b[i]);
}
