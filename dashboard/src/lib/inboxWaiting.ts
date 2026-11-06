/**
 * What waits in a seat's inbox, in the words a row draws it with.
 *
 * THE NODE HOLDING THE SEAT COUNTS (`seat_inbox_waiting`), leaving out what its
 * running turn is working on, so a seat parked or held by its executor with a
 * backlog behind it says so instead of looking idle. A person waiting is said
 * first and apart from the rest: it is the one a reader may be.
 */

import { plural } from "~/lib/format.ts";
import type { InboxWaiting } from "~/protocol/index.ts";

/** One piece of a seat's waiting line, and whether it is a person waiting. */
export interface WaitingWord {
  text: string;
  person: boolean;
}

/**
 * What waits, as ONE quiet count on a line that must hold still, with the words
 * it folds into its tooltip: `22 waiting`, from "13 people waiting · 9 waiting".
 */
export function waitingCount(
  waiting: InboxWaiting | null | undefined,
): { text: string; detail: string } | null {
  const count = (waiting?.priority ?? 0) + (waiting?.background ?? 0);
  if (count <= 0) return null;
  return {
    text: `${count.toLocaleString()} waiting`,
    detail: waitingWords(waiting)
      .map((word) => word.text)
      .join(" · "),
  };
}

/** "1 person waiting", "4 waiting": nothing for a mailbox with nothing in it. */
export function waitingWords(waiting: InboxWaiting | null | undefined): WaitingWord[] {
  const out: WaitingWord[] = [];
  if (!waiting) return out;
  if (waiting.priority > 0) {
    out.push({ text: `${plural(waiting.priority, "person", "people")} waiting`, person: true });
  }
  if (waiting.background > 0) {
    out.push({ text: `${waiting.background.toLocaleString()} waiting`, person: false });
  }
  return out;
}
