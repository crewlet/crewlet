/**
 * What a reader can put on their Home beyond what the sections offer: the
 * layout widgets, and the ready-made boards a Home can start from.
 */

import type { ReactNode } from "react";
import {
  GroupGlyph,
  HeadingGlyph,
  LinkGlyph,
  SeparatorHorizontalGlyph,
  StickyNoteGlyph,
} from "@crewlethq/icons/glyphs";
import type { KpiId } from "../Kpis.tsx";
import type { SectionId } from "../sections.tsx";
import { contrib, newId, type Board, type Leaf, type Widget } from "./model.ts";

export type LayoutCategory = "organize" | "annotate";

/** The tray's own sections, drawn before the ones the dashboard's sections offer. */
export const LAYOUT_CATEGORIES: Record<LayoutCategory, { label: string; hint: string }> = {
  organize: { label: "Organize", hint: "Groups, headings and dividers" },
  annotate: { label: "Annotate", hint: "Notes and links for you and your team" },
};

export interface Entry {
  key: string;
  category: LayoutCategory;
  label: string;
  description: string;
  icon: ReactNode;
  make: () => Widget;
}

const section = (id: SectionId, w: number, h: number | null = null): Leaf =>
  contrib(`home.${id}`, w, h);
const figure = (f: KpiId, w = 3): Leaf => ({
  ...contrib("home.figure", w),
  params: { figure: f },
});

export const ENTRIES: Entry[] = [
  {
    key: "group",
    category: "organize",
    label: "Group",
    description: "A titled band that holds widgets and folds away",
    icon: <GroupGlyph size="sm" />,
    make: () => ({
      id: newId("group"),
      kind: "group",
      title: "New group",
      tone: "blue",
      collapsed: false,
      w: 12,
      h: null,
      children: [],
    }),
  },
  {
    key: "heading",
    category: "organize",
    label: "Heading",
    description: "A title for the widgets that follow",
    icon: <HeadingGlyph size="sm" />,
    make: () => ({
      id: newId("heading"),
      kind: "heading",
      text: "Heading",
      size: "md",
      align: "start",
      w: 12,
      h: null,
    }),
  },
  {
    key: "divider",
    category: "organize",
    label: "Divider",
    description: "A rule between parts of the page",
    icon: <SeparatorHorizontalGlyph size="sm" />,
    make: () => ({
      id: newId("divider"),
      kind: "divider",
      label: "",
      align: "start",
      w: 12,
      h: null,
    }),
  },
  {
    key: "note",
    category: "annotate",
    label: "Note",
    description: "A colored note for goals and reminders, in Markdown",
    icon: <StickyNoteGlyph size="sm" />,
    make: () => ({
      id: newId("note"),
      kind: "note",
      text: "Write a note. **Markdown** works, and so do [links](#/inbox).",
      tone: "amber",
      align: "start",
      size: "md",
      w: 4,
      h: null,
    }),
  },
  {
    key: "links",
    category: "annotate",
    label: "Links",
    description: "Links you open often",
    icon: <LinkGlyph size="sm" />,
    make: () => ({
      id: newId("links"),
      kind: "links",
      title: "Links",
      links: [
        { label: "Inbox", href: "#/inbox" },
        { label: "Live view", href: "#/live" },
      ],
      w: 4,
      h: null,
    }),
  },
];

// ---- templates ------------------------------------------------------------------

export interface Template {
  id: string;
  name: string;
  description: string;
  make: () => Board;
}

/** Home as it shipped before it could be arranged, and every reader's board until they change it. */
export const shippedBoard = (): Board => ({
  widgets: [
    section("kpis", 12),
    section("decisions", 8),
    section("live", 4),
    // HIDDEN, and the row closes up over it, where the company hosts no code.
    section("code", 12),
    section("completed", 4),
    section("tokens", 4),
    section("projects", 4),
    section("feed", 12),
  ],
});

/** The shipped cards, grouped by job. */
const teamBoard = (): Board => ({
  widgets: [
    {
      id: newId("group"),
      kind: "group",
      title: "Needs attention",
      tone: "rose",
      collapsed: false,
      w: 12,
      h: null,
      children: [
        figure("waiting", 6),
        figure("agents", 6),
        section("decisions", 8),
        section("live", 4),
      ],
    },
    {
      id: newId("group"),
      kind: "group",
      title: "Delivery",
      tone: "blue",
      collapsed: false,
      w: 8,
      h: null,
      children: [figure("progress", 6), figure("completed", 6), section("completed", 12)],
    },
    {
      id: newId("group"),
      kind: "group",
      title: "Spend",
      tone: "purple",
      collapsed: false,
      w: 4,
      h: null,
      children: [figure("tokens", 12), section("tokens", 12)],
    },
    {
      id: newId("group"),
      kind: "group",
      title: "Projects and shortcuts",
      tone: "green",
      collapsed: false,
      w: 12,
      h: null,
      children: [
        section("projects", 5),
        section("pinned", 4),
        {
          id: newId("links"),
          kind: "links",
          title: "Runbooks",
          links: [
            { label: "Open decisions in the inbox", href: "#/inbox" },
            { label: "Who is working now", href: "#/live" },
            { label: "Spend by task", href: "#/settings/spend" },
            { label: "Connect a tool", href: "#/company/integrations" },
          ],
          w: 3,
          h: null,
        },
      ],
    },
    { id: newId("divider"), kind: "divider", label: "History", align: "start", w: 12, h: null },
    section("feed", 12, 22),
  ],
});

export const TEMPLATES: Template[] = [
  {
    id: "team",
    name: "Team board",
    description: "Cards grouped by job: attention, delivery, spend and shortcuts",
    make: teamBoard,
  },
  {
    id: "shipped",
    name: "As shipped",
    description: "Home as Crewlet ships it",
    make: shippedBoard,
  },
  {
    id: "focus",
    name: "Focus",
    description: "What waits on you and what you pinned",
    make: () => ({
      widgets: [
        figure("waiting", 4),
        figure("agents", 4),
        figure("progress", 4),
        section("decisions", 12),
        section("pinned", 12),
      ],
    }),
  },
  {
    id: "executive",
    name: "Executive",
    description: "Throughput, spend and the projects behind them",
    make: () => ({
      widgets: [
        {
          id: newId("heading"),
          kind: "heading",
          text: "Throughput and spend",
          size: "lg",
          align: "start",
          w: 12,
          h: null,
        },
        figure("completed", 4),
        figure("tokens", 4),
        figure("agents", 4),
        section("completed", 6, 16),
        section("tokens", 6, 16),
        section("projects", 12),
      ],
    }),
  },
];

/** What a board holds, in a line: "4 groups · 15 widgets". */
export function summary(board: Board): string {
  let groups = 0;
  let widgets = 0;
  for (const w of board.widgets) {
    if (w.kind === "group") {
      groups += 1;
      widgets += w.children.length;
    } else widgets += 1;
  }
  const g = groups === 0 ? "" : groups === 1 ? "1 group · " : `${groups} groups · `;
  return `${g}${widgets === 1 ? "1 widget" : `${widgets} widgets`}`;
}
