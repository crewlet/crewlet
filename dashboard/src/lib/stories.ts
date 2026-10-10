/**
 * A turn told as a story, in words a teammate reads (`turn_stories`,
 * `turn_story`).
 *
 * A story is the one account of an agent's session a member is served — the
 * published facts and nothing the machine processed — so everything here
 * words what the engine already decided may be said. Nothing here can widen
 * it: a field the story does not carry is a sentence this module cannot write.
 *
 * The closed sets are the engine's (`contract/stories.ts`, held to it by a Go
 * gate), and each table below is a `Record` over one of them, so a value the
 * engine adds is a compile error here rather than a card with a bare word.
 */

import type {
  StoryAction,
  StoryLink,
  StoryOutcome,
  TurnStory,
  WorkStatusDef,
} from "~/protocol/index.ts";
import { changePhrase, statusLabel } from "~/lib/work.ts";

/** What each ending is called on a card, and the tone its tag wears. */
export const OUTCOME_WORDS: Record<
  StoryOutcome,
  { label: string; tone: "success" | "warning" | "danger" | "info" | "neutral" }
> = {
  approved: { label: "Done", tone: "success" },
  sent_back: { label: "Sent back for another pass", tone: "warning" },
  failed: { label: "Did not finish", tone: "danger" },
  // NOT "waiting on you": the question is put to whoever the decision names,
  // and the card is read by everybody on the task.
  waiting_on_you: { label: "Waiting on an answer", tone: "info" },
  running: { label: "Working", tone: "info" },
};

/**
 * Why the session started, as one line — or "" where the story may not say.
 *
 * THE TRIGGER'S OWN LABEL FIRST, which the engine quotes only for a surface
 * the company published ("jane assigned ENG-412", "Message from Ana: the
 * numbers"). Without one the origin is worded; a `private` origin is a
 * session told only because it is charged to this work, and says nothing
 * about what set it going — not even that something private did.
 */
export function whyLine(story: TurnStory, nameOf: (handle: string) => string): string {
  const { origin, summary, by } = story.why;
  if (summary) return summary;
  const who = by ? nameOf(by) : "";
  switch (origin) {
    case "task":
      return who ? `${who} changed the task` : "A change on the task";
    case "page":
      return who ? `${who} changed a page` : "A change on a page";
    case "chat":
      return who ? `${who} wrote in a channel` : "A message in a channel";
    case "schedule":
      return "A routine on the schedule";
    case "colleague":
      return who ? `${who} asked for help` : "A colleague asked for help";
    case "private":
      return "";
  }
}

/** One thing the session did, as a sentence. */
export function actionLine(action: StoryAction): string {
  switch (action.kind) {
    case "read_page": {
      const titles = (action.pages ?? []).map((p) => p.title || "an untitled page");
      return titles.length ? `Read ${listOf(titles)}` : "Read a page";
    }
    case "coding_started":
      return "Started a coding job";
    case "coding_asked":
      return "Its coding job stopped to ask a question";
    case "coding_answered":
      return "An answer reached the coding job";
    case "coding_finished":
      return "The coding job finished";
  }
}

/**
 * What one change it left behind was — "commented", "moved it to In review",
 * "edited the page" — said after the thing it changed.
 */
export function linkLine(link: StoryLink, statuses?: WorkStatusDef[]): string {
  if (link.kind === "task") {
    if (link.status) return `moved it to ${statusLabel(link.status, statuses)}`;
    return changePhrase(link.change);
  }
  return PAGE_PHRASES[link.change] ?? link.change.replaceAll("_", " ");
}

/**
 * The pages' change kinds (`pages.ChangeKind`), worded as `CHANGES` words the
 * tracker's. Not a contract row: a kind this build does not know falls back to
 * its own word, which on a page's change reads as one.
 */
const PAGE_PHRASES: Record<string, string> = {
  created: "created the page",
  saved: "edited the page",
  renamed: "renamed the page",
  moved: "moved the page",
  status: "changed the page's status",
  comment: "commented on the page",
  comment_edited: "edited a comment on the page",
  labels: "changed the page's labels",
  watchers: "changed the page's watchers",
  removed: "removed the page",
};

/**
 * Who else the session involved: the helpers it handed parts of the work to,
 * and the colleagues it asked — counts and names, never what was said.
 */
export function involvedLine(story: TurnStory, nameOf: (handle: string) => string): string {
  const parts: string[] = [];
  if (story.helpers > 0) {
    parts.push(
      story.helpers === 1
        ? "handed part of the work to a helper"
        : `handed parts of the work to ${story.helpers} helpers`,
    );
  }
  if (story.consulted.length > 0) parts.push(`asked ${listOf(story.consulted.map(nameOf))}`);
  if (parts.length === 0) return "";
  const line = listOf(parts);
  return line.charAt(0).toUpperCase() + line.slice(1);
}

/** What became of the reader's own note to the session. */
export function noteLine(note: NonNullable<TurnStory["your_note"]>): string {
  return note.status === "delivered" ? "It read your note" : "It ended before it read your note";
}

/** "a", "a and b", "a, b and c". */
function listOf(items: string[]): string {
  if (items.length <= 1) return items[0] ?? "";
  return `${items.slice(0, -1).join(", ")} and ${items[items.length - 1]}`;
}
