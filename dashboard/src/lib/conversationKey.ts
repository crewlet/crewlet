/**
 * A conversation key as a row prints it.
 *
 * # A module of its own, out of `format.ts`
 *
 * Only a seat's Memory tab prints a conversation key — its episodes' column
 * and the subtitle of a session's transcript — and both are drawn by lazily
 * loaded chunks. `format.ts` is in the shell's initial load, which every screen
 * pays for before its first paint, so a label two lazy screens print, and the
 * decoding of a schedule's key it carries, belong with them rather than there.
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
