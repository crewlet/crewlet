/**
 * The starting points a written kind offers: a doc's opening text, a sheet's
 * columns and first rows. Blank is always first, because the commonest start
 * is nothing at all, and a template is a shortcut rather than a gate.
 *
 * PROPOSED, NOT ENGINE DATA: the engine starts a doc and a deck from one
 * starter each and a sheet from two schemas (`lib/artifacts/schema.ts`). What
 * a proposal offers beyond those is a change the engine would carry as its
 * own templates, served here so the flows can be clicked through.
 */

import type { DataCollection } from "~/protocol/index.ts";
import { blankSheetSchema, optionID, reviewSheetSchema } from "~/lib/artifacts/schema.ts";

export interface DocTemplate {
  id: string;
  title: string;
  detail: string;
  /** The title the doc starts with, which the person replaces. */
  suggested: string;
  markdown: string;
}

export const DOC_TEMPLATES: readonly DocTemplate[] = [
  {
    id: "blank",
    title: "Blank",
    detail: "An empty page under its title.",
    suggested: "",
    markdown: "",
  },
  {
    id: "meeting",
    title: "Meeting notes",
    detail: "Who came, what was decided, and who does what next.",
    suggested: "Weekly sync",
    markdown: [
      "## Attendees",
      "",
      "- Miles",
      "- Crewlet-PM",
      "",
      "## Decisions",
      "",
      "- ",
      "",
      "## Actions",
      "",
      "- [ ] Owner: what, by when",
      "",
    ].join("\n"),
  },
  {
    id: "decision",
    title: "Decision record",
    detail: "The question, the options weighed, and the call made.",
    suggested: "Decision: settle-up limits",
    markdown: [
      "## The question",
      "",
      "What are we deciding, and by when?",
      "",
      "## Options",
      "",
      "1. **Option A.** What it costs and what it buys.",
      "2. **Option B.** What it costs and what it buys.",
      "",
      "## Recommendation",
      "",
      "Which option, and why.",
      "",
      "## Who is informed",
      "",
      "- Product and Engineering",
      "",
    ].join("\n"),
  },
  {
    id: "launch",
    title: "Launch plan",
    detail: "Goal, audience, the plan by week, and the risks.",
    suggested: "Beta launch plan",
    markdown: [
      "## Goal",
      "",
      "One sentence on what a successful launch looks like.",
      "",
      "## Audience",
      "",
      "Who it is for and how they will hear about it.",
      "",
      "## Plan",
      "",
      "| Week | What ships | Owner |",
      "| --- | --- | --- |",
      "| 1 | Private beta invites | Crewlet-PM |",
      "| 2 | Feedback round | Miles |",
      "",
      "## Risks",
      "",
      "- ",
      "",
    ].join("\n"),
  },
];

export interface SheetTemplateDef {
  id: string;
  title: string;
  detail: string;
  suggested: string;
  schema: () => DataCollection[];
  /** First rows the sheet is created with, by column key. */
  rows: Record<string, unknown>[];
}

function trackerSchema(): DataCollection[] {
  const statuses: string[] = [];
  const status = (label: string, category: string, color: string) => {
    const id = optionID(label, statuses);
    statuses.push(id);
    return { id, label, category, color };
  };
  return [
    {
      name: "items",
      title: "Tracker",
      preset: "shared",
      read: "all",
      create: "editor",
      delete: "editor",
      columns: [
        { key: "item", type: "text", label: "Item", primary: true, required: true },
        { key: "owner", type: "person", label: "Owner" },
        {
          key: "status",
          type: "status",
          label: "Status",
          options: [
            status("Not started", "todo", "neutral"),
            status("In progress", "in_progress", "warning"),
            status("Done", "done", "success"),
            status("Blocked", "blocked", "danger"),
          ],
          default: "not_started",
        },
        { key: "due", type: "date", label: "Due" },
        { key: "notes", type: "longtext", label: "Notes" },
      ],
    },
  ];
}

function budgetSchema(): DataCollection[] {
  return [
    {
      name: "lines",
      title: "Budget",
      preset: "shared",
      read: "all",
      create: "editor",
      delete: "editor",
      columns: [
        { key: "line", type: "text", label: "Line", primary: true, required: true },
        { key: "planned", type: "number", label: "Planned" },
        { key: "actual", type: "number", label: "Actual" },
        { key: "notes", type: "longtext", label: "Notes" },
      ],
    },
  ];
}

export const SHEET_TEMPLATE_DEFS: readonly SheetTemplateDef[] = [
  {
    id: "blank",
    title: "Blank",
    detail: "One column to start from. Add more as you go.",
    suggested: "",
    schema: blankSheetSchema,
    rows: [{}, {}, {}],
  },
  {
    id: "review",
    title: "Review",
    detail: "Items to review, a reviewer each, and a status they set.",
    suggested: "Integration requirements review",
    schema: reviewSheetSchema,
    rows: [
      { item: "Single sign-on for teams", details: "SAML and OIDC" },
      { item: "Export to CSV", details: "Per group, per month" },
      { item: "Receipts on expenses", details: "Photo and PDF" },
    ],
  },
  {
    id: "tracker",
    title: "Tracker",
    detail: "Items, an owner, a status and a due date.",
    suggested: "Beta launch tracker",
    schema: trackerSchema,
    rows: [
      { item: "Invite the first 50 groups", status: "in_progress" },
      { item: "Feedback form" },
      { item: "Pricing page copy" },
    ],
  },
  {
    id: "budget",
    title: "Budget",
    detail: "Lines with planned and actual amounts.",
    suggested: "Q4 budget",
    schema: budgetSchema,
    rows: [
      { line: "Model spend", planned: 1200, actual: 940 },
      { line: "Hosting", planned: 400, actual: 410 },
      { line: "Design contractor", planned: 2000 },
    ],
  },
];
