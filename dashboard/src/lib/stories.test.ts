/**
 * A story's words: what each line says, and what it must not.
 */

import { expect, test } from "vitest";

import type { TurnStory } from "~/protocol/index.ts";
import { OUTCOME_WORDS, involvedLine, linkLine, whyLine } from "./stories.ts";
import { STORY_OUTCOMES } from "~/contract/stories.ts";

const story = (over: Partial<TurnStory> = {}): TurnStory => ({
  turn_id: "run-1",
  seat: "swe",
  started_at: "2031-04-15T16:26:00Z",
  duration_ms: 0,
  why: { origin: "task" },
  did: [],
  left_behind: [],
  outcome: "approved",
  helpers: 0,
  consulted: [],
  ...over,
});

const names: Record<string, string> = { ana: "Ana Diaz", cto: "CTO", swe: "SWE" };
const nameOf = (h: string) => names[h] ?? h;

// THE TRIGGER'S OWN WORDS FIRST, then the origin's — and NOTHING for a private
// one: a session told only because it is on the work says nothing about what
// set it going, not even that something private did.
test("why a session started is its trigger's label, its origin's words, or nothing", () => {
  expect(
    whyLine(story({ why: { origin: "chat", summary: "Message from Ana: the numbers" } }), nameOf),
  ).toBe("Message from Ana: the numbers");
  expect(whyLine(story({ why: { origin: "colleague", by: "cto" } }), nameOf)).toBe(
    "CTO asked for help",
  );
  expect(whyLine(story({ why: { origin: "schedule" } }), nameOf)).toBe("A routine on the schedule");
  expect(whyLine(story({ why: { origin: "page", by: "ana" } }), nameOf)).toBe(
    "Ana Diaz changed a page",
  );
  expect(whyLine(story({ why: { origin: "private", by: "ana" } }), nameOf)).toBe("");
});

// A HELPER IS A COUNT and a colleague a name — never what either was told.
test("who else a session involved is counts and names", () => {
  expect(involvedLine(story(), nameOf)).toBe("");
  expect(involvedLine(story({ helpers: 1 }), nameOf)).toBe("Handed part of the work to a helper");
  expect(involvedLine(story({ consulted: ["cto", "ana"] }), nameOf)).toBe("Asked CTO and Ana Diaz");
});

// A TASK'S CHANGE IS WORDED AS ITS OWN HISTORY WORDS IT, a status move names
// the status, and a page's kind the build does not know still reads as a word.
test("a change it left behind is worded as the item's own history words it", () => {
  const at = "2031-04-15T16:39:00Z";
  expect(linkLine({ kind: "task", id: "t", key: "ENG-1", title: "", change: "comment", at })).toBe(
    "commented",
  );
  expect(linkLine({ kind: "task", id: "t", title: "", change: "status", status: "done", at })).toBe(
    "moved it to Done",
  );
  expect(linkLine({ kind: "page", id: "p", title: "", change: "renamed", at })).toBe(
    "renamed the page",
  );
  expect(linkLine({ kind: "page", id: "p", title: "", change: "new_kind", at })).toBe("new kind");
});

test("every ending the engine sends is worded", () => {
  for (const outcome of STORY_OUTCOMES) {
    expect(OUTCOME_WORDS[outcome].label, outcome).not.toBe("");
  }
});
