/**
 * What every proposal says about the six kinds: their order on a Make row,
 * who makes each, the words a tile and a create screen use, and the starting
 * points each offers. One table, so the five proposals differ in how they
 * present a kind and never in what they say about it.
 */

import type { ArtifactKind } from "~/protocol/index.ts";

/** The Make row's order: what people write first, then what they lay out, then data. */
export const MAKE_ORDER: readonly ArtifactKind[] = [
  "doc",
  "slides",
  "sheet",
  "design",
  "dashboard",
  "html",
];

/** The kinds a person writes in place; the rest are an agent's to build. */
export type WrittenKind = "doc" | "slides" | "sheet";
export type BuiltKind = "design" | "dashboard" | "html";

export function isWritten(kind: ArtifactKind): kind is WrittenKind {
  return kind === "doc" || kind === "slides" || kind === "sheet";
}

export interface KindWords {
  /** The tile's name: "Docs". */
  tile: string;
  /** One of it, in a sentence: "a doc". */
  one: string;
  /** The create screen's heading: "New doc". */
  heading: string;
  /** One line under a tile: who makes it. */
  hint: string;
  /** A sentence on what it is, for a create screen. */
  about: string;
  /** The primary action's words. */
  action: string;
  /** A keyboard key that starts one, where a proposal offers keys. */
  key: string;
}

export const KIND: Readonly<Record<ArtifactKind, KindWords>> = {
  doc: {
    tile: "Docs",
    one: "a doc",
    heading: "New doc",
    hint: "You write it",
    about: "A document in tabs that you write and edit in place, share and print.",
    action: "Create doc",
    key: "D",
  },
  slides: {
    tile: "Slides",
    one: "a deck",
    heading: "New deck",
    hint: "You write it",
    about: "A slide deck you edit in place and present full screen.",
    action: "Create deck",
    key: "S",
  },
  sheet: {
    tile: "Sheet",
    one: "a sheet",
    heading: "New sheet",
    hint: "Your team fills it in",
    about: "A sheet everyone you share it with fills in together, live.",
    action: "Create sheet",
    key: "T",
  },
  design: {
    tile: "Design",
    one: "a design",
    heading: "New design",
    hint: "An agent designs it",
    about: "Screens and boards laid out on a canvas you can comment on and share.",
    action: "Ask for the design",
    key: "G",
  },
  dashboard: {
    tile: "Dashboard",
    one: "a dashboard",
    heading: "New dashboard",
    hint: "An agent builds it",
    about: "Live figures from your company's data, read as whoever opens it.",
    action: "Ask for the dashboard",
    key: "B",
  },
  html: {
    tile: "HTML",
    one: "a page",
    heading: "New HTML page",
    hint: "An agent builds it",
    about: "Any page, from a calculator to an interactive explainer, run in a sandbox.",
    action: "Ask for the page",
    key: "H",
  },
};

/** A brief a person can start from, for a kind an agent builds. */
export interface Starter {
  id: string;
  label: string;
  prompt: string;
  /** An artifact of the company's an agent built from a brief like this one, by title. */
  example?: string;
}

export const STARTERS: Readonly<Record<BuiltKind, readonly Starter[]>> = {
  dashboard: [
    {
      id: "spend",
      label: "Weekly spend by agent",
      prompt:
        "Build a dashboard of this week's model spend by agent, with the daily total as a line and each agent's share against its budget.",
    },
    {
      id: "beta",
      example: "Beta health",
      label: "Beta health at a glance",
      prompt:
        "Build a dashboard for the beta: sign-ups per day, active groups, settle-ups completed, and the open bugs by severity.",
    },
    {
      id: "work",
      example: "Sprint delivery",
      label: "Open work by owner",
      prompt:
        "Build a dashboard of open FAIR work: counts by status, by owner, and what has been waiting longest.",
    },
  ],
  design: [
    {
      id: "settle",
      example: "Mobile companion concept",
      label: "Settle-up flow, mobile",
      prompt:
        "Design the mobile settle-up flow: the balance summary, choosing who to pay, confirming, and the done state. Follow our existing app's style.",
    },
    {
      id: "states",
      example: "Settle-up screen states",
      label: "Empty and error states",
      prompt:
        "Design the empty, loading and error states for the expenses list, on mobile and desktop.",
    },
    {
      id: "onboarding",
      label: "First-run onboarding",
      prompt:
        "Design three onboarding screens that explain groups, adding an expense and settling up.",
    },
  ],
  html: [
    {
      id: "calculator",
      example: "Split calculator",
      label: "Split calculator",
      prompt:
        "Build an interactive split calculator: enter an amount and people, choose equal or by share, and show who owes what.",
    },
    {
      id: "explainer",
      example: "Fewest payments, explained",
      label: "How settling works",
      prompt:
        "Build a short interactive explainer of how FairSplit settles a group's debts with the fewest payments.",
    },
    {
      id: "changelog",
      label: "Release notes page",
      prompt: "Build a release notes page for the beta from the work closed in the last two weeks.",
    },
  ],
};

/** What happens after a person asks, in order, as a proposal's next-steps list says it. */
export const NEXT_STEPS: readonly { id: string; title: string; detail: string }[] = [
  {
    id: "task",
    title: "A task is filed to the agent",
    detail: "Your brief becomes its description, so you can follow it in Work.",
  },
  {
    id: "draft",
    title: "The agent drafts it",
    detail: "It may ask you a question first. You will see it in your inbox.",
  },
  {
    id: "publish",
    title: "It is published here",
    detail: "Private to you until you share it, with every version kept.",
  },
];
