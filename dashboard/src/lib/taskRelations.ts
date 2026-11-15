/**
 * What one task is to another, as a person links and unlinks them.
 *
 * # Three kinds a person draws, each one of `update_work_item`'s set arguments
 *
 * A link is RELATED (`linked`, no blocking meaning), BLOCKS (`blocking`: the
 * other task waits on this one) or WAITING ON (`waiting_on`: this task waits
 * on the other). The two dependency kinds are one edge seen from its two ends:
 * the tracker stores it on the dependent and mirrors it onto the blocker, and
 * the tool takes either end. A duplicate is not offered here: `duplicate_of`
 * says the task is closed in favour of another, which is Merge into's gesture,
 * where the subtasks move too.
 *
 * # An edge comes off on the end that holds it
 *
 * The task's answer lists both the edges it owns and the ones pointing at it
 * (`derived`). A dependency comes off from either end, through the argument
 * that names that end (`waiting_on` on the dependent, `blocking` on the
 * blocker). A related link and a duplicate are stored on the task that drew
 * them alone, so taking one off from the other end is a change to THAT task:
 * its `linked` without this one, or its `duplicate_of` cleared.
 */

import type { ActionArgs } from "~/protocol/act.ts";
import type { WorkLink } from "~/protocol/index.ts";

/** A kind of link a person draws from this task to another. */
export type LinkKind = "linked" | "blocking" | "waiting_on";

/** The kinds, in the order the link dialog offers them, each as this task's relation. */
export const LINK_KINDS: readonly { value: LinkKind; label: string }[] = [
  { value: "linked", label: "Related to" },
  { value: "blocking", label: "Blocking" },
  { value: "waiting_on", label: "Waiting on" },
];

/**
 * The change that links `other` to the task, as `kind`: a delta on that one
 * argument, never the whole set. A dependency carries the one line saying
 * why, where the person gave one.
 */
export function linkArgs(
  kind: LinkKind,
  other: string,
  note = "",
): Omit<ActionArgs<"update_work_item">, "item"> {
  const why = note.trim();
  return {
    [kind]: { add: [other] },
    ...(kind !== "linked" && why ? { dependency_note: why } : {}),
  };
}

/** The sentence a landed link is confirmed with. */
export function linkedSentence(kind: LinkKind, task: string, other: string): string {
  switch (kind) {
    case "blocking":
      return `${task} blocks ${other} now`;
    case "waiting_on":
      return `${task} waits on ${other} now`;
    default:
      return `Linked ${other} to ${task}`;
  }
}

/**
 * The change that takes one link off: the task it is made on (this one, or
 * the other end where only that end holds the edge) and its arguments, or
 * null for a link no gesture here takes off (a page, an atlas entry).
 *
 * `self` is this task's id; `other` names the far end by its id, which the
 * engine resolves whatever its key has become.
 */
export function unlinkChange(
  link: Pick<WorkLink, "kind" | "other" | "derived">,
  self: { id: string; address: string },
  otherAddress: string,
): { item: string; args: Omit<ActionArgs<"update_work_item">, "item"> } | null {
  switch (link.kind) {
    case "waiting_on":
      return link.derived
        ? { item: self.address, args: { blocking: { remove: [link.other] } } }
        : { item: self.address, args: { waiting_on: { remove: [link.other] } } };
    case "linked":
      return link.derived
        ? { item: otherAddress, args: { linked: { remove: [self.id] } } }
        : { item: self.address, args: { linked: { remove: [link.other] } } };
    case "duplicates":
      // AN ITEM IS A DUPLICATE OF ONE OTHER, so its link is cleared rather
      // than named: on this task where it is the duplicate, on the other
      // where that one is.
      return { item: link.derived ? otherAddress : self.address, args: { duplicate_of: null } };
    default:
      return null;
  }
}

/** The sentence a link taken off is confirmed with, worded as the rail heads it. */
export function unlinkedSentence(
  link: Pick<WorkLink, "kind" | "derived">,
  task: string,
  other: string,
): string {
  switch (link.kind) {
    case "waiting_on":
      return link.derived
        ? `${task} no longer blocks ${other}`
        : `${task} no longer waits on ${other}`;
    case "duplicates":
      return link.derived
        ? `${other} is no longer marked a duplicate of ${task}`
        : `${task} is no longer marked a duplicate of ${other}`;
    default:
      return `Unlinked ${other} from ${task}`;
  }
}
