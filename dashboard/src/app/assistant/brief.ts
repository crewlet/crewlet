/**
 * What a brief is of, in the words the drawer says it: the page, or the one
 * record the page has open.
 *
 * THE PAGE CONTEXT DECIDES, never the drawer: a brief is of the selection the
 * screen reported (`context.ts`: the row it has open, the peeked row, or the
 * record a record's screen is of), and of the page where there is none. The
 * engine reads the same selection to limit the brief, so what the row says and
 * what the brief covers cannot disagree.
 */

import type {
  AssistantPageContext,
  AssistantRecordNoun,
  AssistantResourceRef,
} from "~/contract/assistant.ts";

/** A record's noun as the person reads it. */
const NOUN_WORDS: Record<AssistantRecordNoun, string> = {
  decision: "decision",
  update: "update",
  task: "task",
  project: "project",
  agent: "agent",
  artifact: "artifact",
  page: "page",
  turn: "turn",
  run: "coding run",
  room: "room",
};

/** What a record of each kind is called, where the screen named no noun for it. */
const KIND_WORDS: Record<AssistantResourceRef["kind"], string> = {
  task: "task",
  project: "project",
  seat: "agent",
  unit: "team",
  page: "page",
  container: "space",
  artifact: "artifact",
  turn: "turn",
  run: "coding run",
  schedule: "schedule",
  view: "view",
  room: "room",
};

/** What a selected record is called: the screen's noun, or its kind's. */
export function recordWord(ref: AssistantResourceRef): string {
  return ref.noun ? NOUN_WORDS[ref.noun] : KIND_WORDS[ref.kind];
}

/** What a brief asked now covers: the open record's word, or "page". */
export function briefSubject(context: AssistantPageContext): string {
  return context.selection ? recordWord(context.selection) : "page";
}

/** The question a brief is asked with, and the row that asks it: "Brief me on this decision". */
export function briefQuestion(context: AssistantPageContext): string {
  return `Brief me on this ${briefSubject(context)}`;
}
