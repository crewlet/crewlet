/**
 * The information architecture: ONE sidebar, and three nouns that keep it one.
 *
 * # One sidebar, in four runs
 *
 * The sidebar reads top to bottom as the reader meets the product:
 *
 *   - YOU, unlabelled: Home, Inbox and My work, what needs the reader, what
 *     reached them and what is theirs.
 *   - COMPANY: what the company decides. Principles (its mission, its vision
 *     and the standing policies every agent reads), Brand (how everything its
 *     agents make looks and sounds), the Org chart, Schedules, Integrations
 *     (the services built into Crewlet above the third-party apps it
 *     connects) and Budgets, last.
 *   - WORKSPACE: what the company does with it. Worklet, Chatlet, Doclet,
 *     Artifacts and Live.
 *   - The FOOT: Settings, the engine's state and the reader. Account has no
 *     row: the user block IS the reader, and opens it from its name and menu.
 *
 * Settings is what the ENGINE runs, and nothing the company decides: a column
 * of four groups, Runtime (Models, MCP Tools, Secrets, Limits), Spend
 * (the readings, never the ceilings), Access (People, Service accounts, API
 * tokens, Sign-in policy, Audit log) and Engine (Nodes, Configuration,
 * Backups & retention, File storage). It lists only what lives in it: a row
 * that left for Company is found there, by the sidebar and the palette, and
 * never again as a second row here leading out.
 *
 * # A sidebar row owns an address PREFIX
 *
 * Every Company row lives under `#/company`, so the first segment cannot say
 * which row a page is in. A row owns a prefix (`owns`, its landing unless it
 * says otherwise) and an address is in the row owning the LONGEST prefix of
 * it: `#/company/org/roster` is the Org chart's, `#/company` alone is
 * Principles'. Settings lands on its first section, `#/settings/models`, and
 * owns all of `#/settings`.
 *
 * # Addresses that moved
 *
 * Moved outright, with no redirect table (see `router.tsx`): no release has
 * shipped a route from this tree, so nobody holds an old link, and an old
 * address is Not Found. Every link the dashboard draws, and every address the
 * docs print, names where the page is now (`app/links.test.ts`).
 *
 * # The three nouns, stated once and asserted in router.test.ts
 *
 *   - A SIDEBAR ROW is a workspace, or a live shortcut to a pinned view, a
 *     starred page or a running turn. A project is not one: it is a scope of
 *     Worklet, switched where the trail names it.
 *   - A SECTION is a PATH segment inside a workspace, drawn by one renderer:
 *     `tabs` in the page header, `lenses` beside Worklet's scope in the trail,
 *     `column` for Settings, `rail` for Chatlet, whose screen draws them beside
 *     its rooms, or `none` where the workspace's own screens lead to it
 *     (Doclet's landing).
 *   - An OBJECT TAB is a `tab=` query on an object's own page, bound to 1-9.
 *
 * Nothing is two of those. A section is never a sidebar row and never a
 * query; an object tab is never a path. A filter (a reason, a scope, a
 * status) is none of the three and lives in the page's own bar.
 *
 * # Hidden for a service the company does not run, locked for access
 *
 * A row or section that reads one of Crewlet's own services (its tracker,
 * Worklet, its knowledge base, Doclet, or its chat, Chatlet) names it
 * ([WorkspaceRow.service], [Section.service]). Where the company runs that
 * service elsewhere or not at all, the place is HIDDEN, never locked: there is
 * nothing to ask an admin for, and its addresses say so in place, to every
 * reader (`ServiceNotice.tsx`, [serviceShown], `lib/services.ts`). Access
 * the reader lacks is the opposite case and keeps its rule: locked, never
 * hidden ([sectionOpen]).
 */

import type { GlyphName } from "@crewlethq/icons/glyphs";
import { holdsAny, type Grant } from "~/lib/access.ts";
import { KIND_LIST, MAKE_ORDER, NEW_KIND_TITLES, type MakeKind } from "~/lib/artifacts/words.ts";

/** The workspaces: the sidebar's navigation rows, and the reader's own Account. */
export type Workspace =
  | "home"
  | "inbox"
  | "me"
  | "company"
  | "brand"
  | "org"
  | "schedules"
  | "integrations"
  | "budgets"
  | "worklet"
  | "chatlet"
  | "doclet"
  | "artifacts"
  | "live"
  | "settings"
  | "account";

/**
 * A service built into Crewlet that a company may run elsewhere or not at
 * all: `tracker` is Worklet (projects, tasks, the Inbox's updates and My
 * work), `knowledge` is Doclet (spaces and pages), `chat` is
 * Chatlet (rooms, threads and direct messages). Named here because the
 * navigation hides what reads one the company does not run.
 */
export type Service = "tracker" | "knowledge" | "chat";

/** Whether the company runs each of Crewlet's own services: what `lib/services.ts` reads off the wire. */
export type ServicesRun = Readonly<Record<Service, boolean>>;

/**
 * How a workspace draws its sections: `tabs` in the page header, a `column`
 * of them beside the screen (Settings), a `rail`, or `none`, where the
 * workspace's own screens lead to its sections (Home, Inbox, and Doclet,
 * whose landing links
 * its two sections under "For the agents").
 *
 * `lenses` is Worklet's. Its sections are the lenses of the scope the trail
 * names, All projects, drawn beside the trail's switcher the way a project
 * draws its own lenses, so the bar reads one way in either scope and Worklet
 * keeps no row of tabs of its own.
 *
 * `rail` is Chatlet's, whose SCREEN draws its sections at the head of the list
 * of its objects, the rooms (`routes/chatlet/Chat.tsx`), from this table. The
 * frame draws nothing for a rail, neither a strip nor a column, because the
 * tabs are drawn on a section's own page and never on an object's, and a room
 * is the object page a chat reader spends their time on. It is not `none`
 * because the screen that draws it reads the renderer: Chatlet's rail is drawn
 * from the row that says `rail`, every section of it, and a `none` row says
 * no frame or screen owes it a strip.
 */
export type SectionRenderer = "tabs" | "lenses" | "column" | "rail" | "none";

/**
 * Where a workspace's row sits in the sidebar: the unlabelled run at the top
 * (`you`), the Company and Workspace groups, the foot, or `menu` for the one
 * with no row: the reader's own Account, which the menu the reader's name
 * opens in the user block at the sidebar's foot links to, since that block
 * already IS the reader.
 */
export type SidebarPlace = "you" | "company" | "workspace" | "foot" | "menu";

/**
 * The sidebar's labelled groups, in the order they are drawn, the words over
 * each, and a named group's glyph: the ONE tile the compact sidebar folds the
 * group into (`sidebar/Sidebar.tsx`), whose flyout lists its places.
 */
export const SIDEBAR_GROUPS: readonly { place: SidebarPlace; label: string; icon?: GlyphName }[] = [
  { place: "you", label: "" },
  { place: "company", label: "Company", icon: "building-complex" },
  { place: "workspace", label: "Workspace", icon: "layout-grid" },
];

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
   * mention a word: Service accounts for "seats", whose sentence names the
   * seats its accounts act as.
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
   * The service built into Crewlet this section reads, where the rest of its
   * workspace reads none: Expensive tasks names its tasks by Worklet's keys
   * and titles, and New design's one way to finish is asking an agent,
   * which files a task. Hidden where the company does not run it
   * ([serviceShown]).
   */
  service?: Service;
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
  /**
   * The address prefix the row holds, where it is not the landing: Settings
   * lands on its first section and holds all of `#/settings`. See "A sidebar
   * row owns an address PREFIX" above.
   */
  owns?: string[];
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
   * never hidden. An APP's workspace names its app (Inbox, My work and the
   * Worklet workspace name Worklet, the Doclet workspace names Doclet),
   * because every screen in it reads that app; the one address under one
   * that the app does not
   * gate is a work item somebody was ASKED about (`App.tsx`), which its own
   * read decides.
   */
  grants?: readonly Grant[];
  /** The reader's own, as [Section.personal]: the Account. */
  personal?: boolean;
  /**
   * The service built into Crewlet the whole workspace reads, as
   * [Section.service]: the Worklet workspace and My work read Worklet, the
   * Doclet workspace reads Doclet, and Chatlet reads Chatlet.
   */
  service?: Service;
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
   * where the strip has room for it beside every tab (the kit's `Tabs` note,
   * with `overflow="menu"`), so never on a phone.
   */
  note?: string;
}

/**
 * What the palette says about each kind's create page, and the words a reader
 * finds it by beside its title.
 */
const NEW_ARTIFACT_PAGES: Readonly<
  Record<MakeKind, { icon: GlyphName; hint: string; keywords: readonly string[] }>
> = {
  doc: {
    icon: "file-text",
    hint: "Write a doc in Markdown, edit it in place and print it",
    keywords: ["document", "write", "markdown"],
  },
  slides: {
    icon: "presentation",
    hint: "Make a slide deck to edit in place, present full screen and print",
    keywords: ["new deck", "slides", "presentation"],
  },
  design: {
    icon: "palette",
    hint: "Ask an agent for screens and boards laid out on a canvas",
    keywords: ["canvas", "mockup", "screens"],
  },
  dashboard: {
    icon: "layout-dashboard",
    hint: "Ask an agent for live figures from your company's data",
    keywords: ["figures", "metrics", "chart"],
  },
  html: {
    icon: "code",
    hint: "Ask an agent for any page, run in a sandboxed frame",
    keywords: ["page", "web page", "interactive"],
  },
  sheet: {
    icon: "table",
    hint: "Start a sheet, blank or from the Review sheet template",
    keywords: ["spreadsheet", "review sheet", "template", "table"],
  },
};

/**
 * The kinds made ONLY by asking an agent: a design's create page lays out its
 * boards and hands them to an agent, which files the work as a task, so where
 * the company runs no Worklet the page has no way to finish and is not
 * offered. Every other kind has a create a person finishes on the page.
 */
const MADE_BY_AN_AGENT: ReadonlySet<MakeKind> = new Set(["design"]);

/**
 * The create page of each kind, as sections of Artifacts reached from its
 * Make row and New menu (`tab: false`), in the order they are offered there.
 */
const NEW_ARTIFACT_SECTIONS: readonly Section[] = MAKE_ORDER.map((kind) => ({
  key: `new-${kind}`,
  label: NEW_KIND_TITLES[kind],
  path: ["artifacts", "new", kind],
  ...NEW_ARTIFACT_PAGES[kind],
  tab: false,
  grants: ["artifacts"],
  ...(MADE_BY_AN_AGENT.has(kind) ? { service: "tracker" as const } : {}),
}));

/**
 * The kinds a company running `runs` is offered a create page for, in the
 * Make row's order: the gallery's tray and its New menu list these, as the
 * palette lists their sections.
 */
export function offeredKinds(runs: ServicesRun): MakeKind[] {
  return MAKE_ORDER.filter((kind) => {
    const section = NEW_ARTIFACT_SECTIONS.find((s) => s.key === `new-${kind}`);
    return section === undefined || serviceShown(section, runs);
  });
}

/**
 * THE READER'S OWN, AND NO ROW: the user block at the sidebar's foot is the
 * reader, and its name and its menu are where this is opened. A row beside it
 * would be the same person twice, and the block takes the row's current mark
 * instead. A workspace all the same, so the palette and `g u` reach it. No grant, and PERSONAL: every signed-in
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
    // WORKLET'S: the inbox is the notices Worklet routed to the reader's
    // record, beside the seat and budget conditions waiting on a person. NOT
    // HIDDEN with Worklet, because those conditions arrive under every
    // backend: where the company runs no Worklet the screen draws them and
    // says where asks and mentions go instead.
    grants: ["worklet"],
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
    // WORKLET'S: every tab is a question about somebody's work items, so it
    // reads Worklet and is not offered where the company runs none.
    grants: ["worklet"],
    service: "tracker",
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
    // THE COMPANY'S PRINCIPLES: its mission, its vision and the standing
    // policies every agent reads on every turn, in the order an agent reads
    // them, which every signed-in reader reads (the charter is the org
    // projection's). Its name is the Org chart's, renamed on the company's
    // own node, and its clock is the company document's. The Company
    // landing: `#/company` opens it too. The caps the engine holds a turn to
    // are engine settings, and live once, in Settings › Limits.
    key: "company",
    label: "Principles",
    icon: "building-complex",
    path: ["company", "principles"],
    // THE COMPANY LANDING IS THIS PAGE, so the row owns the bare `#/company`
    // too: the longest prefix decides, and every other Company row is longer.
    owns: ["company"],
    hint: "Who the company is and the policies every agent reads on every turn",
    keywords: ["principles", "mission", "vision", "policies", "charter", "values"],
    chord: "c",
    place: "company",
    renderer: "none",
    sections: [],
  },
  {
    // THE COMPANY'S BRAND: its logos, colours, type and voice, which every
    // doc, deck and page its agents make follows. Every reader reads it (the
    // brand is the company's public face); an administrator adopts it. A
    // sibling of Principles rather than a part of it: Principles is what
    // every agent reads word for word each turn, and the brand is applied
    // to what they make.
    key: "brand",
    label: "Brand",
    icon: "swatch-book",
    path: ["company", "brand"],
    hint: "The company's logos, colours, type and voice, which everything its agents make follows",
    keywords: [
      "brand",
      "logo",
      "colours",
      "colors",
      "palette",
      "fonts",
      "typography",
      "voice",
      "tone",
      "style guide",
      "design system",
    ],
    // `g e`: `b` is Budgets' and `r` watches the running turn.
    chord: "e",
    place: "company",
    renderer: "none",
    sections: [],
  },
  {
    key: "org",
    label: "Org chart",
    icon: "network",
    path: ["company", "org"],
    hint: "The org chart and every seat on it",
    keywords: ["agents", "seats", "hierarchy", "reporting lines", "company name"],
    chord: "a",
    place: "company",
    renderer: "tabs",
    sections: [
      {
        key: "chart",
        label: "Chart",
        icon: "network",
        path: ["company", "org"],
        hint: "The hierarchy every seat works inside, and the seat each person is invited onto",
      },
      {
        key: "roster",
        label: "Roster",
        icon: "list",
        path: ["company", "org", "roster"],
        hint: "Every seat and what it is doing",
      },
      {
        key: "teams",
        label: "Teams",
        icon: "users",
        path: ["company", "org", "teams"],
        hint: "Every team and its goals",
      },
      {
        // THE ONE SECTION THAT WRITES, so it is an address and a button rather
        // than a tab: a reader arrives at the org chart to look.
        key: "edit",
        label: "Edit org",
        icon: "pencil",
        path: ["company", "org", "edit"],
        hint: "Change teams, seats and reporting lines",
        // NO PERMISSION: the builder answers for itself, saying which of its
        // saves the reader reaches, an admin's whole company, a lead's own
        // units, rather than the frame locking it whole. Its draft is kept in
        // the tab's storage, and a frame refusal drawn the moment admin was
        // withdrawn would take the draft off screen while the person who
        // wrote it is still there to copy it out.
        tab: false,
      },
    ],
  },
  {
    key: "schedules",
    label: "Schedules",
    icon: "calendar-clock",
    path: ["company", "schedules"],
    hint: "Recurring work and when it runs next",
    keywords: ["cron", "recurring", "standup"],
    chord: "d",
    place: "company",
    renderer: "none",
    sections: [],
  },
  {
    // THE SERVICES BUILT INTO CREWLET beside the third-party apps the company
    // connects, in one directory grouped by need. EVERY READER'S: what the
    // company runs is the overview every reader holds, so a reader without
    // Admin sees the services in full and the apps with no state, while
    // connecting and the apps' own state stay an Admin's.
    key: "integrations",
    label: "Integrations",
    icon: "plug",
    path: ["company", "integrations"],
    hint: "Crewlet's own services and the apps the company connects",
    keywords: ["integrations", "connect", "apps"],
    chord: "n",
    place: "company",
    renderer: "none",
    sections: [],
  },
  {
    // LAST IN COMPANY: the ceilings the company sets on its own and each
    // seat's tokens. The readings they are measured against are Settings ›
    // Spend, since what was spent is the engine's account of a run.
    key: "budgets",
    label: "Budgets",
    icon: "target",
    path: ["company", "budgets"],
    hint: "Token ceilings for the company and each seat, by day, week and month",
    keywords: ["budget", "limit", "spend cap", "ceiling", "tokens"],
    chord: "b",
    place: "company",
    renderer: "none",
    sections: [],
  },
  {
    key: "worklet",
    label: "Worklet",
    icon: "square-kanban",
    path: ["worklet"],
    hint: "Projects and tasks",
    // WHAT IT HOLDS, in the words a reader looks for it by, as Doclet's: its
    // name says neither.
    keywords: ["work tracker", "projects"],
    chord: "w",
    place: "workspace",
    // THE SCOPE'S LENSES, beside the switcher in the trail: All projects'
    // lenses are these three sections, a project's are its own `lens=` tabs,
    // and both draw alike (see [SectionRenderer]).
    renderer: "lenses",
    // WORKLET'S, every lens and every project, but not an item somebody was
    // ASKED about, which `App.tsx` lets them open: the engine answers the
    // person an open ask names without Worklet access, so they can read what
    // they were asked and answer it.
    grants: ["worklet"],
    service: "tracker",
    sections: [
      {
        // WORKLET OPENS ON ITS PROJECTS: the first question about the
        // company's work is where it is filed, and every project is one row
        // away from its own board here.
        key: "projects",
        label: "Projects",
        icon: "columns-3",
        path: ["worklet"],
        hint: "Every project, its lead and progress",
      },
      {
        key: "tasks",
        label: "Tasks",
        icon: "list",
        path: ["worklet", "tasks"],
        hint: "Every task in the company",
      },
      {
        key: "history",
        label: "History",
        icon: "chart-no-axes-gantt",
        path: ["worklet", "history"],
        hint: "Every change to the company’s work",
      },
      // ADDRESSES, NOT LENSES: a saved view is reached from the view strip,
      // Pinned and ⌘K, and ranked search from ⌘K, so neither is a lens of a
      // scope.
      {
        key: "views",
        label: "Saved views",
        icon: "layout-dashboard",
        path: ["worklet", "views"],
        hint: "Every saved view and its owner",
        tab: false,
      },
      {
        key: "search",
        label: "Search",
        icon: "search",
        path: ["worklet", "search"],
        hint: "Find tasks",
        tab: false,
      },
    ],
  },
  {
    // SECOND IN WORKSPACE, after Worklet and before what the company knows: the
    // work, then the talking, then what the talking produced (Doclet and
    // Artifacts).
    //
    // A RAIL, and the screen draws it. Chatlet's rooms are its own object list,
    // drawn beside the transcript because they carry unread counts, mute
    // state and a preview that no tab can hold — and a room is an OBJECT
    // page, where the header draws no section tabs, so tabs would leave the
    // mention feed two presses from every room. The two lists that are not a
    // room sit at the head of the rail instead, drawn from the sections
    // below: the landing's label heads the rooms, and every other section is
    // a link under the name it has here.
    key: "chatlet",
    label: "Chatlet",
    icon: "message-square",
    path: ["chatlet"],
    hint: "The company's own rooms, threads and direct messages",
    keywords: ["messages", "rooms", "channels", "direct message", "dm"],
    // THE LETTER OF CHATLET NO OTHER ROW HOLDS: `g c` is Company's Principles.
    chord: "t",
    place: "workspace",
    renderer: "rail",
    // THE CHATLET APP'S, every section and every room: each reads somebody's
    // rooms as the seat the session holds, so the frame draws its one refusal
    // in place of every chat page for a reader without it. What the screen
    // answers is only what is chat's own: a credential acting as no seat.
    grants: ["chatlet"],
    // AND CHATLET'S: a company talking on Mattermost or Slack, or on
    // nothing, has no room here to read, so the row is hidden as Worklet is on
    // Jira, and its addresses say where the company's chat is instead
    // (`ServiceNotice.tsx`).
    service: "chat",
    sections: [
      {
        key: "rooms",
        label: "Rooms",
        icon: "hash",
        path: ["chatlet"],
        hint: "Your rooms, newest activity first",
      },
      {
        key: "mentions",
        label: "Mentions",
        icon: "bell",
        path: ["chatlet", "mentions"],
        hint: "Every message that named you, kept here since nothing is sent to you",
      },
      {
        key: "search",
        label: "Search chat",
        icon: "search",
        path: ["chatlet", "search"],
        hint: "Rank what has been said in every room you can read",
      },
    ],
  },
  {
    key: "doclet",
    label: "Doclet",
    icon: "book-open",
    path: ["doclet"],
    hint: "The company’s documentation library and knowledge base",
    // WHAT IT HOLDS, in the words a reader looks for it by: a reader after the
    // company's documentation types what it is rather than its name.
    keywords: ["knowledge base", "docs", "documentation"],
    chord: "k",
    place: "workspace",
    // NO COLUMN, the approved Doclet Home: the spaces are a row at the top of
    // every Doclet screen and the search sits in the page bar, so a page is
    // read at the full width of the screen. The sections are reached from the
    // landing, the palette and the trail.
    renderer: "none",
    // DOCLET'S: every section reads its pages.
    grants: ["doclet"],
    service: "knowledge",
    sections: [
      {
        key: "search",
        label: "Search",
        icon: "search",
        path: ["doclet"],
        hint: "Search and browse the knowledge base",
      },
      {
        key: "skills",
        label: "Agent skills",
        icon: "wand-sparkles",
        path: ["doclet", "skills"],
        hint: "Skills agents use and learn",
      },
      {
        key: "diaries",
        label: "Agent diaries",
        icon: "brain",
        path: ["doclet", "diaries"],
        hint: "What each agent remembers",
        // EVERY SEAT'S MEMORY AT ONCE is admin's; a lead reads their own
        // seats' memory on each seat's page.
        grants: ["admin"],
      },
    ],
  },
  {
    // THE COMPANY'S MADE THINGS, after what it knows: pages, documents and
    // sheets that people and agents publish and share, read and filled in
    // together. A workspace of its own rather than a section of Doclet or
    // Worklet, because either would gate it by the wrong permission (D32).
    key: "artifacts",
    label: "Artifacts",
    icon: "layers",
    path: ["artifacts"],
    hint: `${KIND_LIST.charAt(0).toUpperCase()}${KIND_LIST.slice(1)} your company and its agents made`,
    keywords: [
      "artifact",
      "page",
      "document",
      "sheet",
      "review sheet",
      "report",
      "dashboard",
      "shared with me",
    ],
    chord: "f",
    place: "workspace",
    renderer: "tabs",
    // THE GALLERY'S OWN QUESTION travels between its sections: the kind, the
    // phrase and the order a reader chose are the same question on every one.
    // The shape (grid or list) is the browser's own preference, not the
    // address's: see `routes/artifacts/Gallery.tsx`.
    keep: ["kind", "q", "sort"],
    // NO PERMISSION FOR THE WORKSPACE: the artifacts app gates AUTHORING only
    // (D01). A share alone admits a person to open, comment on and fill in
    // what was shared with them, so the gallery and an artifact's page are
    // open to every signed-in reader, and the engine decides each artifact.
    // Starting a sheet is the app's, and says so on its own row.
    sections: [
      {
        key: "all",
        label: "All",
        icon: "layers",
        path: ["artifacts"],
        hint: "Every artifact you can open, the latest activity first, your pins above",
      },
      {
        key: "yours",
        label: "Yours",
        icon: "user",
        path: ["artifacts", "yours"],
        hint: "Artifacts you own, and your trash",
        keywords: ["mine", "my artifacts", "trash"],
      },
      {
        key: "shared",
        label: "Shared with you",
        icon: "share-2",
        path: ["artifacts", "shared"],
        hint: "Artifacts somebody shared with you, your team or the whole company",
        keywords: ["company-wide", "by agents"],
      },
      // THE COMPANY'S FOLDERS, a fourth question beside the three flat ones:
      // where things are kept rather than who made them. A folder is a place
      // and never a permission, so every reader browses them, and a folder's
      // own page (`#/artifacts/folders/<id>`) is in this section.
      {
        key: "folders",
        label: "Folders",
        icon: "folder",
        path: ["artifacts", "folders"],
        hint: "The company's folders you can see, and what is in none of them",
        keywords: ["folder", "directory", "organise", "organize", "move", "file"],
      },
      // EACH KIND'S CREATE PAGE IS AN ADDRESS, not a tab: the gallery's Make
      // row and its New menu open them, and a reader arrives at Artifacts to
      // look. Listed so the palette finds "new doc" and the trail names it.
      ...NEW_ARTIFACT_SECTIONS,
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
    // AT THE FOOT, AND THE ROW IS NEVER HIDDEN: a place that vanishes when a
    // credential is absent is indistinguishable from one that does not
    // exist. Spend's readings are every reader's, so the workspace names no
    // permission as a whole; every other section is admin's, and says so on
    // its own row.
    key: "settings",
    label: "Settings",
    // THE GEAR: what a reader looks for to change how the engine runs.
    icon: "settings",
    path: ["settings", "models"],
    owns: ["settings"],
    hint: "What the engine runs: models, tools, spend, access and its nodes",
    chord: "s",
    place: "foot",
    renderer: "column",
    // THE WINDOW IS SPEND'S QUESTION: Spend and Expensive tasks both read it,
    // and a row that dropped it opened the next section on the default while
    // the readings' own links carried it.
    keep: ["window"],
    sections: [
      {
        key: "models",
        label: "Models",
        icon: "cpu",
        path: ["settings", "models"],
        hint: "Models, the CLIs they run through, and how each signs in",
        keywords: ["models", "keys", "llm", "provider", "cli", "api key", "subscription"],
        group: "Runtime",
        // EVERY MEMBER'S: each model's condition is what tells a member why
        // a seat waits. The reading names no credential, and every action on
        // the page is an admin's (`llm_entries`' two readings).
      },
      {
        key: "mcp-tools",
        label: "MCP Tools",
        icon: "wrench",
        path: ["settings", "mcp-tools"],
        hint: "The MCP servers the engine runs and every tool its agents call",
        keywords: ["mcp", "tools", "mcp servers", "agent tools"],
        group: "Runtime",
        // ADMIN'S: the page is where MCP servers are added and their
        // templates read, and the overview's own `tools` push already says
        // which tools each seat has on its page.
        grants: ["admin"],
      },
      {
        key: "secrets",
        label: "Secrets",
        icon: "key",
        path: ["settings", "secrets"],
        hint: "The company’s stored credentials",
        keywords: ["credentials", "env"],
        group: "Runtime",
        grants: ["admin"],
      },
      {
        key: "limits",
        label: "Limits",
        icon: "gauge",
        path: ["settings", "limits"],
        hint: "Turn guardrails, delegation, notification and timeout caps",
        keywords: ["guardrails", "caps", "thresholds", "timeouts", "rounds", "delegation"],
        group: "Runtime",
        // ADMIN'S: the caps are read from the company document, which only
        // an admin reads.
        grants: ["admin"],
      },
      {
        key: "spend",
        // NOT "SPEND" UNDER SPEND: where the company runs no Worklet the
        // group holds this row alone, and it read "Spend › Spend". The
        // name its Home widget already carries.
        label: "Token spend",
        icon: "coins",
        path: ["settings", "spend"],
        hint: "Tokens over time, by phase, agent, model and team",
        keywords: ["spend", "cost", "tokens", "usage"],
        group: "Spend",
      },
      {
        key: "spend-tasks",
        label: "Expensive tasks",
        icon: "layers",
        path: ["settings", "spend", "tasks"],
        hint: "Tasks that spent the most tokens",
        group: "Spend",
        // A TASK'S KEY AND TITLE are Worklet's; the readings above are
        // everybody's.
        grants: ["worklet"],
        service: "tracker",
      },
      {
        key: "people",
        label: "People",
        icon: "users",
        path: ["settings", "people"],
        hint: "People, their invitations, grants and sessions",
        keywords: ["invite", "members", "grants", "directory"],
        group: "Access",
        grants: ["admin"],
      },
      {
        key: "service-accounts",
        label: "Service accounts",
        icon: "bot",
        path: ["settings", "service-accounts"],
        hint: "Machine principals, what they hold and the seats they act as",
        keywords: ["machine", "bot", "automation"],
        group: "Access",
        grants: ["admin"],
      },
      {
        key: "tokens",
        label: "API tokens",
        icon: "key",
        path: ["settings", "tokens"],
        hint: "This node's API tokens",
        keywords: ["token", "api key", "node token"],
        group: "Access",
        grants: ["admin"],
      },
      {
        key: "sign-in",
        label: "Sign-in policy",
        icon: "shield-user",
        path: ["settings", "sign-in"],
        hint: "How people sign in, and what a session asks of them",
        keywords: ["password", "2fa", "mfa", "session", "step-up", "security"],
        group: "Access",
        grants: ["admin"],
      },
      {
        key: "audit",
        label: "Audit log",
        icon: "shield",
        path: ["settings", "audit"],
        hint: "Every change made by people and tokens",
        group: "Access",
        grants: ["admin"],
      },
      {
        key: "nodes",
        label: "Nodes",
        icon: "server",
        path: ["settings", "nodes"],
        hint: "Engine nodes and the broker",
        group: "Engine",
        grants: ["admin"],
      },
      {
        key: "config",
        label: "Configuration",
        icon: "code",
        path: ["settings", "config"],
        hint: "The active company revision, its history and its diffs",
        keywords: ["company document", "timezone"],
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
        key: "storage",
        label: "File storage",
        icon: "folder",
        path: ["settings", "storage"],
        hint: "Where the engine keeps the company's files, and its collector",
        keywords: ["objects", "bucket", "s3", "files"],
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

/** The address prefix a row holds: its `owns`, or its landing. */
export function ownedBy(row: WorkspaceRow): readonly string[] {
  return row.owns ?? row.path;
}

/**
 * Which workspace a route belongs to, so the sidebar marks the right row.
 *
 * The row owning the LONGEST PREFIX of the address decides ([ownedBy]):
 * `#/company/org/roster` is the Org chart's, `#/company` Principles'. The
 * empty fragment is Home, which is the landing screen. An address no row owns
 * answers "", never the first row: a sidebar that marked a workspace for a
 * path it does not hold would tell the reader they are somewhere they are not.
 */
export function workspaceOf(path: readonly string[]): Workspace | "" {
  if (path.length === 0) return "home";
  let best: WorkspaceRow | undefined;
  for (const row of WORKSPACES) {
    const owns = ownedBy(row);
    if (!startsWith(path, owns)) continue;
    if (!best || owns.length > ownedBy(best).length) best = row;
  }
  return best?.key ?? "";
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
 * is what makes `#/worklet/views` a list of saved views rather than a project
 * called VIEWS, without a single escape character in the route table. A
 * handle is lowercase too, which is why handles live ONLY under `seats/`,
 * unit keys only under `teams/` and domains only under `backups/`: none of
 * them is ever read at a position where a reserved word is. (A unit is
 * addressed by its KEY — the engine's `Unit.Key`, its declared `id:` or its
 * name where it declares none — never by a name two units may share.)
 */
export const RESERVED_SEGMENTS: string[] = [
  "company",
  "principles",
  "brand",
  "org",
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
  "spend",
  "people",
  "service-accounts",
  "tokens",
  "sign-in",
  "integrations",
  "mcp-tools",
  "servers",
  "models",
  "secrets",
  "limits",
  "nodes",
  "config",
  "revisions",
  "backups",
  "storage",
  "audit",
  "yours",
  "shared",
  "folders",
  "new",
  "setup",
];

/**
 * Every fixed destination — a workspace's landing page and each of its own
 * sections — for the command palette and for the tests that walk the product.
 *
 * DERIVED FROM [WORKSPACES], never written beside it: a palette that listed
 * its own copy of the sections is a second table that is wrong the day a
 * section moves. A section whose path IS its workspace's landing page is one
 * destination under the workspace's name, found by that section's words too:
 * Settings opens on Models, and "models" finds it.
 *
 * IT CARRIES NO GRANTS: what closes one is its ADDRESS's, decided by
 * [closedBy] — the workspace's permissions first, then the section's — the
 * rule the frame's refusal and the sidebar's locks read. A section's own
 * list said nothing of the workspace around it, so every section of Worklet,
 * My work and Chatlet was offered with no lock to a reader its workspace was
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
  /** The service built into Crewlet it reads, as [Section.service]: hidden where it is not run. */
  service?: Service;
}

export const DESTINATIONS: Destination[] = WORKSPACES.flatMap((row) => {
  const own = row.sections.find((s) => samePath(s.path, row.path));
  const landing: Destination = {
    key: row.key,
    workspace: row.key,
    label: row.label,
    icon: row.icon,
    path: row.path,
    hint: row.hint,
    // A LANDING PAGE IS ALSO ITS LANDING SECTION, so it is found by that
    // section's words as well as the workspace's. The palette's entry for the
    // workspace is locked as the workspace is, and opens where the reader may
    // read ([landingPath], `palette/hits.ts`).
    ...keywordsOf([...(row.keywords ?? []), ...(own?.keywords ?? [])]),
    ...serviceOf(row.service ?? own?.service),
  };
  const sections = row.sections
    .filter((s) => !samePath(s.path, row.path))
    .map((s): Destination => ({
      key: `${row.key}-${s.key}`,
      workspace: row.key,
      label: s.label,
      icon: s.icon,
      path: s.path,
      hint: s.hint,
      ...keywordsOf(s.keywords),
      ...serviceOf(row.service ?? s.service),
    }));
  return [landing, ...sections];
});

/**
 * The section a path is in: the section whose path is the LONGEST prefix of
 * it. `#/live/turns/abc` is in Turns, `#/live` is in Now running, and a path
 * under no section (a project, an item) is in the workspace's landing section
 * only where that section's path is a prefix of it: `#/worklet/ENG` is in
 * Projects, which is where the reader reached it from.
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

function keywordsOf(words: readonly string[] | undefined): { keywords?: readonly string[] } {
  return words && words.length > 0 ? { keywords: words } : {};
}

function serviceOf(service: Service | undefined): { service?: Service } {
  return service ? { service } : {};
}

function samePath(a: readonly string[], b: readonly string[]): boolean {
  return a.length === b.length && a.every((seg, i) => seg === b[i]);
}

function startsWith(path: readonly string[], prefix: readonly string[]): boolean {
  return prefix.length <= path.length && prefix.every((seg, i) => seg === path[i]);
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
  const own = row.sections;
  if (own.some((s) => s.personal)) return true;
  if (!sectionOpen({ grants: row.grants }, held)) return false;
  return own.length === 0 || own.some((s) => sectionOpen(s, held));
}

/**
 * Where a workspace's row, its `g` chord and its palette entry open for a
 * reader holding `held`: its own path, or, where that landing section is
 * closed to them, the first of its sections they can open and the company
 * runs. A landing closed to the reader sent them to a refusal although other
 * sections of the workspace were theirs. `held` null is a reader not yet read: the
 * workspace's own path, as the frame draws every lock only once it knows.
 */
export function landingPath(
  row: WorkspaceRow,
  held: readonly string[] | null,
  runs: ServicesRun,
): string[] {
  if (held === null) return row.path;
  const own = row.sections.find((s) => samePath(s.path, row.path));
  if (!own || sectionOpen(own, held)) return row.path;
  return row.sections.find((s) => serviceShown(s, runs) && sectionOpen(s, held))?.path ?? row.path;
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
  return [...new Set(row.sections.flatMap((s) => grantsMissing(s, held)))];
}

/**
 * What closes an address to a reader holding `held`, as the frame's refusal
 * says it: the WORKSPACE's permissions first, since a reader who cannot open
 * Worklet cannot open any lens of it, then the SECTION's, or null where
 * nothing does. `what` is the label of whichever closed it.
 *
 * The caller decides what is exempt: a work item somebody was ASKED about is
 * opened whatever this says (`App.tsx`), because the engine answers the person
 * an open ask names without Worklet access.
 */
export function closedBy(
  path: readonly string[],
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
export function guardedPath(path: readonly string[]): boolean {
  const row = workspaceRow(workspaceOf(path));
  if (row?.personal) return false;
  return (row?.grants?.length ?? 0) > 0 || (sectionOf(path)?.grants?.length ?? 0) > 0;
}

/**
 * Whether an address is the READER'S OWN (the Account), which the frame draws
 * for every signed-in reader, the engine serving them no company included.
 */
export function personalPath(path: readonly string[]): boolean {
  if (workspaceRow(workspaceOf(path))?.personal) return true;
  return sectionOf(path)?.personal === true;
}

/**
 * Whether a place is offered to a company running `runs`: anything that reads
 * no service built into Crewlet, and otherwise only where the company runs
 * that service. HIDDEN, never locked, where it does not: see "Hidden for a
 * service the company does not run" above.
 */
export function serviceShown(place: { service?: Service }, runs: ServicesRun): boolean {
  return place.service === undefined || runs[place.service];
}

/**
 * The service an address reads, if the company may not run it: its
 * workspace's, then its section's. What the router asks before drawing a page
 * a hidden row held, so the reader is told in place why it is not here
 * (`ServiceNotice.tsx`).
 */
export function serviceOfPath(path: readonly string[]): Service | undefined {
  const row = workspaceRow(workspaceOf(path));
  return row?.service ?? sectionOf(path)?.service;
}

/**
 * Whether an address is offered to a company running `runs`, as
 * [serviceShown] decides for a place: what the palette's recents, the
 * sidebar's stars and Home's pinned shelf ask of a stored address.
 */
export function pathShown(path: readonly string[], runs: ServicesRun): boolean {
  const service = serviceOfPath(path);
  return service === undefined || runs[service];
}
