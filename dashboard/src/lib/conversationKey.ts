/**
 * A conversation key as a row prints it.
 *
 * # A module of its own, out of `format.ts`
 *
 * A conversation key is printed by a seat's Memory tab — its episodes' column
 * and the subtitle of a session's transcript — by a turn's page and by the
 * phase cards a turn, a seat and an event draw, and every one of them is drawn
 * by a lazily loaded chunk. `format.ts` is in the shell's initial load, which
 * every screen pays for before its first paint, so a label only lazy screens
 * print, and the decoding of a schedule's key it carries, belong with them
 * rather than there.
 */

import { SCHEDULE_CONVERSATION_PREFIX } from "~/contract/sessions.ts";

/** A uuid anywhere inside an identifier. */
const UUID_IN = /[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/gi;

/** One escaped part of a schedule's key as written, or as sent where it does not decode. */
function unescapedPart(part: string): string {
  try {
    return decodeURIComponent(part);
  } catch {
    return part;
  }
}

/**
 * A conversation key as a row prints it: every uuid inside it cut to its first
 * eight characters — `work:task:4d631f6d` — and everything else as it is, so
 * `work:ENG-32` and `slack:C9:1718.001` are untouched; and a schedule's key
 * with its parts as they were written — `schedule/unit/eng/weekly report`, not
 * `weekly%20report` — a role schedule's seat id cut like any other uuid.
 *
 * THE KEY IS THE CONVERSATION'S ONLY IDENTITY on a row, and a Worklet task
 * nobody gave a key is `work:task:` and a uuid — 46 unbreakable characters that
 * no row has room for, so a list of them read `work:task…` five times over.
 * The head of a uuid is how this product names one everywhere a row is narrow
 * (`workItemLabel`'s "task 4d631f6d"); the whole key belongs on the title, and
 * every caller puts it there. A schedule's key escapes its parts so that a
 * slash in a name cannot merge two schedules, which a reader needs no more than
 * a `%20` for every space; a part that does not decode is shown as sent.
 */
export function conversationLabel(key: string): string {
  const raw = key ?? "";
  const prefix = SCHEDULE_CONVERSATION_PREFIX;
  const shown = raw.startsWith(prefix)
    ? prefix + raw.slice(prefix.length).split("/").map(unescapedPart).join("/")
    : raw;
  return shown.replace(UUID_IN, (id) => id.slice(0, 8));
}

/**
 * Whether a conversation key is a schedule's: the conversation every fire of
 * one schedule is on the seat that runs it (`notify.ScheduleConversation`),
 * which no surface holds and nobody replies into — so a screen that prints a
 * turn's conversation says the turn was a fire of that schedule, rather than
 * calling the key the external conversation the turn served.
 */
export function isScheduleConversation(key: string): boolean {
  return (key ?? "").startsWith(SCHEDULE_CONVERSATION_PREFIX);
}

/**
 * What a turn's conversation key names, in the words a screen prints beside
 * it: the schedule the turn was a fire of, or the external conversation it
 * served.
 */
export function conversationRole(key: string): string {
  return isScheduleConversation(key)
    ? "the schedule this turn was a fire of"
    : "the external conversation this turn served";
}
