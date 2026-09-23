/**
 * The eighteen wake reasons, as English, written once.
 *
 * The applier records, per change and per recipient, the ONE reason of eighteen
 * under which that person heard about it — `internal/tracker/recipients.go`,
 * in the precedence order that decided it. Nothing has ever drawn it, and it
 * is the fact no commercial tracker records: Linear, Jira and ClickUp can all
 * tell you that you were notified, and none of them can tell you why.
 *
 * ONE TABLE, because two would disagree. `asked` on an inbox row and `asked`
 * on an item's routing tab must mean the same thing in the same words, and
 * eighteen raw snake_case values rendered directly read like a log file rather
 * than like a sentence about a person.
 *
 * A reason this build does not know renders as ITSELF rather than vanishing,
 * for the reason the event registry gives: a rolling upgrade puts values a
 * newer node writes in front of an older one's screen.
 */

/*
 * THERE IS NO COPY OF THE PRIMARY SPLIT HERE, deliberately.
 *
 * Which reasons lead an inbox is a property of the PERSON — their own record
 * may override the shipped eight — so the only correct answer is the one the
 * engine states per read, on `primary_reasons`. A constant here would be a
 * second answer to a question the wire already answers, and it would be wrong
 * for anybody who set a preference. It was written that way once, out of the
 * WRONG eight: the engine had two lists of eight both called "primary", one
 * being the reasons a wake obliges a seat to answer. That one is now
 * `Reason.Addressed`.
 */

import { PHRASES } from "~/contract/reasons.ts";

/**
 * Two or three words for a chip beside a row.
 *
 * AN ABSENT REASON IS SAID OUT LOUD rather than rendered as an empty chip. The
 * applier writes one on every notice, so a row carrying none is a row that
 * lost it somewhere — and an empty chip is indistinguishable from a chip that
 * was never drawn, which turns the one fact this surface exists to show into
 * a blank nobody reports.
 */
export function reasonPhrase(reason: string): string {
  if (reason === "") return "no reason recorded";
  return PHRASES[reason]?.short ?? reason.replace(/_/g, " ");
}

/**
 * The same reason in the THIRD person, for a surface about somebody else.
 *
 * A change's routing lists colleagues, and [reasonPhrase]'s "assigned to you"
 * beside a colleague's name is a sentence about the wrong person. Falls back
 * to the raw value for the same reason the other does: a newer node's reason
 * must render as itself rather than vanish.
 */
export function reasonAbout(reason: string): string {
  if (reason === "") return "no reason recorded";
  return PHRASES[reason]?.about ?? reason.replace(/_/g, " ");
}

/** A sentence for the detail pane: why this reached this person. */
export function reasonWhy(reason: string): string {
  if (reason === "") {
    return "this notice carries no reason, and the engine writes one on every notice";
  }
  return PHRASES[reason]?.why ?? `the engine recorded the reason “${reason}”`;
}
