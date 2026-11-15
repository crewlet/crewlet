/**
 * What waits in a seat's inbox, in the words a row draws it with.
 *
 * THE NODE HOLDING THE SEAT COUNTS (`seat_inbox_waiting`), leaving out what its
 * running turn is working on, so a seat parked or held by its executor with a
 * backlog behind it says so instead of looking idle.
 *
 * THE COUNTS ARE MESSAGES, NOT PEOPLE. `priority` is the inbox's priority lane:
 * messages a person wrote that the seat owes an answer to (a direct message, a
 * mention, a question put to it). `background` is every other notice: a
 * colleague's mention, a tracker notice, a room's news, a schedule's fire. Four
 * people who wrote 79 messages are 79 messages, so the words say "79 messages
 * from people", never "79 people waiting", which a company of four read as a
 * count of its people. The messages from people are said first and apart from
 * the rest: they are the one a reader may have written.
 */

import { plural } from "~/lib/format.ts";
import type { InboxWaiting } from "~/protocol/index.ts";

/** One piece of a seat's waiting line, and whether it counts messages from people. */
export interface WaitingWord {
  text: string;
  person: boolean;
}

/**
 * What waits, as ONE quiet count on a line that must hold still, with the words
 * it folds into its tooltip: `22 queued`, from "13 messages from people · 9
 * other notices". QUEUED, because a bare "22 waiting" did not say what waits,
 * nor for what: the line is the seat's own inbox, queued behind its turn.
 */
export function waitingCount(
  waiting: InboxWaiting | null | undefined,
): { text: string; detail: string } | null {
  const count = (waiting?.priority ?? 0) + (waiting?.background ?? 0);
  if (count <= 0) return null;
  return {
    text: `${count.toLocaleString()} queued`,
    detail: `Queued in its inbox: ${waitingWords(waiting)
      .map((word) => word.text)
      .join(" · ")}`,
  };
}

/**
 * "1 message from a person", "79 messages from people · 9 other notices",
 * "4 notices": nothing for a mailbox with nothing in it. The rest are OTHER
 * notices only beside messages from people, which they are other than.
 */
export function waitingWords(waiting: InboxWaiting | null | undefined): WaitingWord[] {
  const out: WaitingWord[] = [];
  if (!waiting) return out;
  if (waiting.priority > 0) {
    const from = waiting.priority === 1 ? "from a person" : "from people";
    out.push({ text: `${plural(waiting.priority, "message")} ${from}`, person: true });
  }
  if (waiting.background > 0) {
    const noun = waiting.priority > 0 ? "other notice" : "notice";
    out.push({ text: plural(waiting.background, noun), person: false });
  }
  return out;
}
