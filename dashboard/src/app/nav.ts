/**
 * The information architecture: two levels, and the grammar that keeps them
 * apart.
 *
 * # Two levels, not one
 *
 * A **workspace** is a noun with a tree of its own. Work has projects,
 * Company has units, Knowledge has containers, Activity has kinds of run,
 * Cost has scopes, Admin has estates. The RAIL shows the workspaces and
 * nothing else; a workspace's SIDEBAR shows that workspace's tree and nothing
 * else.
 *
 * It replaced a single flat sidebar of six groups and nineteen rows, which is
 * the shape that fails as soon as there is more than one tree: either every
 * tree hides behind disclosure — three clicks to a project — or the column
 * grows to sixty rows and stops being scannable. The tracker had already
 * grown its own rail inside its own screen, which is the same conclusion
 * reached one screen at a time.
 *
 * # The grammar, stated once and asserted in router.test.ts
 *
 *   - A RAIL ROW is a workspace.
 *   - A SIDEBAR ROW is a destination with its OWN PATH — an object, a saved
 *     view, a workspace-level list.
 *   - A TAB is a `tab=` or `view=` query on the path you are already on.
 *
 * Nothing is two of those. A tab never appears as a sidebar row, and a
 * sidebar row never appears as a tab; a filter (a reason, a scope, a status)
 * lives in the page's own filter bar, never in the sidebar. This is the
 * boundary every tool of this shape loses first, and it is lost one pull
 * request at a time — "just one more row under a project" is what turns two
 * levels into three.
 *
 * # The order is the argument
 *
 * You, the work, the people, what they know, what they did, what it cost, the
 * machine. A founder opening this meets their own inbox first and the engine
 * last, because the company is the product and the engine is what runs it.
 */

import type { IconName } from "~/ui/Icon.tsx";

/** The workspaces, which are the rail's rows. */
export type Workspace =
  "inbox" | "me" | "work" | "company" | "knowledge" | "activity" | "cost" | "admin";

export interface RailRow {
  key: Workspace;
  label: string;
  icon: IconName;
  /** Where the row goes. */
  path: string[];
  /** Every first path segment this workspace owns. */
  owns: string[];
  /** Shown under the label in the command palette. */
  hint: string;
  /** Everything under this row needs an operator credential. */
  guarded?: boolean;
  /** The access key in the `g`-prefixed jump chord. */
  chord: string;
}

export const RAIL: RailRow[] = [
  {
    key: "inbox",
    label: "Inbox",
    icon: "inbox",
    path: ["inbox"],
    owns: ["inbox"],
    hint: "What reached you, and what needs a person",
    chord: "i",
  },
  {
    key: "me",
    label: "My work",
    icon: "user",
    path: ["me"],
    owns: ["me"],
    hint: "Your priorities, your asks, and what became workable",
    chord: "m",
  },
  {
    key: "work",
    label: "Work",
    icon: "check",
    path: ["work"],
    owns: ["work", "goals"],
    hint: "Projects, sprints, items and goals",
    chord: "w",
  },
  {
    key: "company",
    label: "Company",
    icon: "users",
    path: ["company"],
    owns: ["company"],
    hint: "The charter, the people and the units",
    chord: "c",
  },
  {
    key: "knowledge",
    label: "Knowledge",
    icon: "book",
    path: ["knowledge"],
    owns: ["knowledge"],
    hint: "The company's own pages, searched the way an agent searches them",
    chord: "k",
  },
  {
    key: "activity",
    label: "Activity",
    icon: "activity",
    path: ["activity"],
    owns: ["activity"],
    hint: "Turns, coding runs, schedules, asks and the event log",
    chord: "a",
  },
  {
    key: "cost",
    label: "Cost",
    icon: "coin",
    path: ["cost"],
    owns: ["cost"],
    hint: "Token spend by seat, model, phase and turn; budget headroom",
    chord: "o",
  },
  {
    // LAST AND LOCKED, and the row is never hidden: a section that vanishes
    // when a credential is absent is indistinguishable from one that does not
    // exist, so an operator on a fresh browser would conclude the product has
    // no configuration screen.
    key: "admin",
    label: "Admin",
    icon: "server",
    path: ["admin"],
    owns: ["admin"],
    hint: "Nodes, integrations, tools, configuration and credentials",
    guarded: true,
    chord: "d",
  },
];

/**
 * Which workspace a route belongs to, so the rail marks the right row.
 *
 * Derived from `owns` rather than from a second table: a screen whose
 * workspace is decided in two places is a screen that loses its place in the
 * rail on exactly the route somebody forgot to add.
 */
export function workspaceOf(path: string[]): Workspace | "" {
  const head = path[0] ?? "";
  if (!head) return "inbox";
  return RAIL.find((row) => row.owns.includes(head))?.key ?? "";
}

/** The rail row for a workspace, or undefined for a route nothing owns. */
export function railRow(key: Workspace | ""): RailRow | undefined {
  return RAIL.find((row) => row.key === key);
}

/**
 * Every reserved path segment, so a test can hold them apart from the keys the
 * engine mints.
 *
 * Project and container keys are UPPERCASE, item keys are `KEY-n`, everything
 * else the engine mints is a uuid — and every segment here is lowercase. That
 * is what makes `#/work/views` a list of saved views rather than a project
 * called VIEWS, without a single escape character in the route table.
 */
export const RESERVED_SEGMENTS: string[] = [
  "views",
  "sprints",
  "people",
  "units",
  "turns",
  "runs",
  "schedules",
  "a2a",
  "events",
  "servers",
  "revisions",
  "domains",
  "budgets",
  "fleet",
  "integrations",
  "tools",
  "config",
  "credentials",
  "audit",
  "me",
];

/**
 * Every workspace-level destination, for the command palette and for the
 * tests that walk the product.
 *
 * NOT the sidebar's source. A workspace sidebar is built from live answers —
 * the projects that exist, the units the chart has, the containers this node
 * knows about — and a hand-kept copy of those would be a second, wrong list.
 * What is here is the fixed furniture: the lists and landing pages that exist
 * whatever a company contains.
 */
export interface Destination {
  key: string;
  workspace: Workspace;
  label: string;
  icon: IconName;
  path: string[];
  hint: string;
  guarded?: boolean;
}

export const DESTINATIONS: Destination[] = [
  {
    key: "inbox",
    workspace: "inbox",
    label: "Inbox",
    icon: "inbox",
    path: ["inbox"],
    hint: "What reached you, and what needs a person",
  },
  {
    key: "me",
    workspace: "me",
    label: "My work",
    icon: "user",
    path: ["me"],
    hint: "Your priorities, your asks, and what became workable",
  },
  {
    key: "work",
    workspace: "work",
    label: "All work",
    icon: "check",
    path: ["work"],
    hint: "Every item in the company, in one list",
  },
  {
    key: "work-views",
    workspace: "work",
    label: "Saved views",
    icon: "columns",
    path: ["work", "views"],
    hint: "Every saved view, who owns it and which are pinned",
  },
  {
    key: "goals",
    workspace: "work",
    label: "Goals",
    icon: "target",
    path: ["goals"],
    hint: "The tier above projects, and what its targets say right now",
  },
  {
    key: "company",
    workspace: "company",
    label: "Company",
    icon: "crown",
    path: ["company"],
    hint: "The charter: mission, vision and the standing policies",
  },
  {
    key: "people",
    workspace: "company",
    label: "People",
    icon: "users",
    path: ["company", "people"],
    hint: "Every seat, what it is doing, and why it stopped",
  },
  {
    key: "knowledge",
    workspace: "knowledge",
    label: "Knowledge",
    icon: "book",
    path: ["knowledge"],
    hint: "Search the company knowledge base, and browse its containers",
  },
  {
    key: "activity",
    workspace: "activity",
    label: "Live now",
    icon: "zap",
    path: ["activity"],
    hint: "What the company is doing at this moment",
  },
  {
    key: "turns",
    workspace: "activity",
    label: "Turns",
    icon: "brain",
    path: ["activity", "turns"],
    hint: "Every turn the seats ran, round by round",
  },
  {
    key: "runs",
    workspace: "activity",
    label: "Coding runs",
    icon: "terminal",
    path: ["activity", "runs"],
    hint: "Detached sandbox runs, live and finished",
  },
  {
    key: "schedules",
    workspace: "activity",
    label: "Schedules",
    icon: "calendar",
    path: ["activity", "schedules"],
    hint: "Recurring work, when it next fires and how it last went",
  },
  {
    key: "a2a",
    workspace: "activity",
    label: "Agent-to-agent",
    icon: "link",
    path: ["activity", "a2a"],
    hint: "The private channels seats opened with each other",
  },
  {
    key: "events",
    workspace: "activity",
    label: "Event log",
    icon: "activity",
    path: ["activity", "events"],
    hint: "Everything the engine published, filterable and paged",
  },
  {
    key: "cost",
    workspace: "cost",
    label: "Spend",
    icon: "coin",
    path: ["cost"],
    hint: "Token spend by seat, model, phase and turn",
  },
  {
    key: "budgets",
    workspace: "cost",
    label: "Budgets",
    icon: "shield",
    path: ["cost", "budgets"],
    hint: "Caps, the durable counter behind them, and what is being refused",
  },
  {
    key: "fleet",
    workspace: "admin",
    label: "Infrastructure",
    icon: "server",
    path: ["admin", "fleet"],
    hint: "Nodes, seat leases, domains and config rollout",
    guarded: true,
  },
  {
    key: "integrations",
    workspace: "admin",
    label: "Integrations",
    icon: "plug",
    path: ["admin", "integrations"],
    hint: "The surfaces agents work on, and whether traffic is arriving",
    guarded: true,
  },
  {
    key: "tools",
    workspace: "admin",
    label: "Tools",
    icon: "wrench",
    path: ["admin", "tools"],
    hint: "Every tool a seat can call, by origin",
    guarded: true,
  },
  {
    key: "config",
    workspace: "admin",
    label: "Configuration",
    icon: "sliders",
    path: ["admin", "config"],
    hint: "The active company revision, its history and its diffs",
    guarded: true,
  },
  {
    key: "credentials",
    workspace: "admin",
    label: "Credentials",
    icon: "key",
    path: ["admin", "credentials"],
    hint: "The company's credentials — names and provenance, never values",
    guarded: true,
  },
];

/** The destinations belonging to one workspace, in declaration order. */
export function destinationsOf(workspace: Workspace): Destination[] {
  return DESTINATIONS.filter((d) => d.workspace === workspace);
}
