import type { ComponentType } from "react";
import {
  type GlyphProps,
  AccountTreeGlyph,
  Book2Glyph,
  BuildGlyph,
  CableGlyph,
  CalendarClockGlyph,
  DashboardGlyph,
  DnsGlyph,
  GroupGlyph,
  KeyGlyph,
  LinkGlyph,
  ManufacturingGlyph,
  NeurologyGlyph,
  TerminalGlyph,
  TimelineGlyph,
  TokenGlyph,
} from "@crewlethq/icons/glyphs";
/**
 * The information architecture.
 *
 * The sidebar is grouped by WHAT THE READER IS LOOKING AT, in the order the
 * product's own story runs: the company, the work it is doing, the thinking
 * behind that work, what it costs, and the machine underneath. That ordering
 * is the argument: a founder opening this should meet their company first and
 * the engine last, because the company is the product and the engine is the
 * thing that runs it.
 *
 * It replaces a flat list of nine nouns grouped by the KIND OF DATA each held
 * (Dashboard, Agents, Activity, Tokens, Tools, Schedules, Fleet, Configuration),
 * where the questions an operator actually arrives with (*is anything waiting
 * on me? what is my company doing right now? what is this costing?*) each
 * needed three or four screens and a mental join.
 *
 * Every entry resolves to a screen backed by a real answer. Nothing here is a
 * placeholder or a coming-soon stub: an empty screen says which endpoint
 * answered and why it was empty.
 */

export interface NavItem {
  key: string;
  label: string;
  icon: ComponentType<GlyphProps>;
  path: string[];
  /** Shown under the label in the command palette. */
  hint: string;
  /** Auth-gated screens are marked so the palette can say so before you land. */
  guarded?: boolean;
}

export interface NavGroup {
  key: string;
  label: string;
  items: NavItem[];
}

export const NAV: NavGroup[] = [
  {
    key: "now",
    label: "",
    items: [
      {
        key: "overview",
        label: "Overview",
        icon: DashboardGlyph,
        path: [],
        hint: "What the company is doing, and what needs a person",
      },
    ],
  },
  {
    key: "company",
    label: "Company",
    items: [
      {
        key: "people",
        label: "People",
        icon: GroupGlyph,
        path: ["people"],
        hint: "Every seat, what it is doing, and why it stopped",
      },
      {
        key: "org",
        label: "Org chart",
        icon: AccountTreeGlyph,
        path: ["org"],
        hint: "The hierarchy, the directory and the charter",
      },
    ],
  },
  {
    key: "work",
    label: "Work",
    items: [
      {
        key: "runs",
        label: "Coding runs",
        icon: TerminalGlyph,
        path: ["runs"],
        hint: "Detached sandbox runs, live and finished",
      },
      {
        key: "conversations",
        label: "Agent-to-agent",
        icon: LinkGlyph,
        path: ["conversations"],
        hint: "The private channels seats opened with each other",
      },
      {
        key: "schedules",
        label: "Schedules",
        icon: CalendarClockGlyph,
        path: ["schedules"],
        hint: "Recurring work, when it next fires and how it last went",
      },
    ],
  },
  {
    key: "intelligence",
    label: "Intelligence",
    items: [
      {
        key: "model",
        label: "Model activity",
        icon: NeurologyGlyph,
        path: ["model"],
        hint: "Every phase the models ran, round by round",
      },
      {
        key: "activity",
        label: "Event log",
        icon: TimelineGlyph,
        path: ["activity"],
        hint: "Everything the engine published, filterable and paged",
      },
      {
        key: "knowledge",
        label: "Knowledge",
        icon: Book2Glyph,
        path: ["knowledge"],
        hint: "Search the company knowledge base and what seats have learned",
      },
    ],
  },
  {
    key: "cost",
    label: "Cost",
    items: [
      {
        key: "spend",
        label: "Spend & budgets",
        icon: TokenGlyph,
        path: ["spend"],
        hint: "Token spend by seat, model, phase and turn; budget headroom",
      },
    ],
  },
  {
    key: "operations",
    label: "Operations",
    items: [
      {
        key: "fleet",
        label: "Fleet",
        icon: DnsGlyph,
        path: ["fleet"],
        hint: "Nodes, seat leases and config rollout",
      },
      {
        key: "integrations",
        label: "Integrations",
        icon: CableGlyph,
        path: ["integrations"],
        hint: "The surfaces agents work on, and whether traffic is arriving",
      },
      {
        key: "tools",
        label: "Tools",
        icon: BuildGlyph,
        path: ["tools"],
        hint: "Every tool a seat can call, by origin",
      },
      {
        key: "config",
        label: "Configuration",
        icon: ManufacturingGlyph,
        path: ["config"],
        hint: "The active company revision, its history and its diffs",
        guarded: true,
      },
      {
        key: "secrets",
        label: "Secrets",
        icon: KeyGlyph,
        path: ["secrets"],
        hint: "The company's credentials: names and provenance, never values",
        guarded: true,
      },
    ],
  },
];

export const ALL_NAV: NavItem[] = NAV.flatMap((g) => g.items);

/** Screens reachable from a row rather than from the nav. */
export const DETAIL_TITLES: Record<string, string> = {
  seats: "Seat",
  traces: "Trace",
  events: "Event",
  turns: "Turn",
};

/** Which nav entry a route belongs to, so the sidebar marks the right row. */
export function activeNavKey(path: string[]): string {
  const head = path[0] ?? "";
  if (!head) return "overview";
  // A seat page belongs to People, a trace and a turn to Model activity, one
  // event to the log. Otherwise the reader loses their place in the sidebar
  // the moment they open a detail.
  if (head === "seats") return "people";
  if (head === "traces" || head === "turns") return "model";
  if (head === "events") return "activity";
  return ALL_NAV.find((i) => i.path[0] === head)?.key ?? "";
}

export function titleFor(path: string[]): string {
  const head = path[0] ?? "";
  if (!head) return "Overview";
  const item = ALL_NAV.find((i) => i.path[0] === head);
  if (item) return item.label;
  return DETAIL_TITLES[head] ?? "Crewlet";
}
