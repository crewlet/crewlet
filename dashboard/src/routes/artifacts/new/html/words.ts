/**
 * What a new HTML page says to the person making it: each capability in plain
 * words, as what the page can do rather than the runtime's name for it, and the
 * starting points an agent can be asked for.
 *
 * PLAIN WORDS HERE, THE ENGINE'S ELSEWHERE: Settings and the Info rail say what
 * a version declares (`CAPABILITY_WORDS`); the create page asks what the page
 * should do, so the same capability is said from the maker's side.
 */

import type { ArtifactCapability } from "~/protocol/index.ts";

/** One capability, as the create page offers it. */
export interface CapabilityChoice {
  capability: ArtifactCapability;
  label: string;
  detail: string;
}

/** Every capability, in the order the page offers them: the most asked for first. */
export const CAPABILITY_CHOICES: readonly CapabilityChoice[] = [
  {
    capability: "db",
    label: "Saves what people enter",
    detail: "Votes, rows and answers are kept and shared with everyone who opens it.",
  },
  {
    capability: "identity",
    label: "Knows who is using it",
    detail: "The page can show the viewer's name and keep one vote per person.",
  },
  {
    capability: "presence",
    label: "Shows who is here right now",
    detail: "Everyone with the page open, and where they are in it.",
  },
  {
    capability: "state",
    label: "Remembers each person's place",
    detail: "Private to each viewer: a step reached, a filter, a draft answer.",
  },
  {
    capability: "assets",
    label: "Shows files added later",
    detail: "Pictures and files uploaded to the page after it is published.",
  },
];

/** A starting point an agent can be asked for. */
export interface Starter {
  key: string;
  label: string;
  /** One line under the label: what it is for. */
  purpose: string;
  /** What the page should be able to do. */
  capabilities: readonly ArtifactCapability[];
  /** The brief it fills an empty field with, for the person to make their own. */
  brief: string;
}

export const STARTERS: readonly Starter[] = [
  {
    key: "poll",
    label: "Poll",
    purpose: "One vote per person, results live",
    capabilities: ["db", "identity"],
    brief:
      "A poll asking: <the question>\n\nOptions:\n- <first option>\n- <second option>\n\nOne vote per person, which they can change. Results update live for everyone.",
  },
  {
    key: "tracker",
    label: "Tracker",
    purpose: "A shared list with owners and status",
    capabilities: ["db", "identity", "presence"],
    brief:
      "A tracker for <what is tracked>.\n\nEach item has a title, an owner, a status (to do, doing, done) and a due date. Anyone can add and update items, and the list shows who is looking at it.",
  },
  {
    key: "form",
    label: "Form",
    purpose: "Answers collected in one place",
    capabilities: ["db", "identity"],
    brief:
      "A form that asks:\n- <first question>\n- <second question>\n\nEach answer is saved with who sent it. The people I share it with as editors see every answer in a table below the form.",
  },
  {
    key: "retro",
    label: "Retro board",
    purpose: "Went well, to improve, actions",
    capabilities: ["db", "identity", "presence"],
    brief:
      "A retrospective board for <team or sprint>.\n\nThree columns: went well, to improve, actions. Everyone adds cards at once and votes on them, and the board shows who is here.",
  },
  {
    key: "calculator",
    label: "Calculator",
    purpose: "Inputs and a live result",
    capabilities: [],
    brief:
      "A calculator for <what it works out>.\n\nInputs: <the inputs>. It shows the result as the inputs change, and explains how it is worked out.",
  },
  {
    key: "explainer",
    label: "Explainer",
    purpose: "One idea, step by step",
    capabilities: ["state"],
    brief:
      "An interactive explainer of <the idea>, in a few short steps a reader moves through, with a small example they can change at each step. It remembers the step each reader reached.",
  },
];
