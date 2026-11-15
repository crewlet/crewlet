/**
 * A refused write of the company document, in one sentence a person can act
 * on: the one wording every settings screen that writes `/config` shows.
 *
 * THE ENGINE'S OWN WORDS WHERE IT WROTE ANY. A refusal of the document names
 * each failure in a sentence of its own (`problems`), a node that is draining
 * or starting says so, and a managed document names who manages it; this
 * adds only what the answer cannot know: that a conflict is cleared by
 * reading again, and that a write nobody answered may have landed.
 */

import { managedSentence, unsettledSentence, type ConfigRefusal } from "~/protocol/configAnswer.ts";
import { configGuardedReason } from "./useWriteAccess.ts";

/** A refusal as a screen says it. */
export interface RefusalWords {
  readonly message: string;
  /** Reading the document again is the way forward. */
  readonly reload: boolean;
  /** The engine's own advice beside the refusal, "" where it gave none. */
  readonly hint: string;
}

/**
 * What a refusal says. `reread` is the sentence a conflict ends with, in the
 * screen's own words for what it draws ("Reload to see the limits as they
 * are now.").
 */
export function configRefusalWords(
  refusal: ConfigRefusal | { readonly kind: "none" },
  reread: string,
): RefusalWords {
  switch (refusal.kind) {
    case "guarded":
      return { message: configGuardedReason(refusal.code), reload: false, hint: "" };
    case "managed":
      return { message: managedSentence(refusal.managedBy), reload: false, hint: "" };
    case "conflict":
      return {
        message: `The company's configuration changed since this was read, so nothing was saved. ${reread}`,
        reload: true,
        hint: "",
      };
    case "draining":
      return {
        message: `This node is draining and stored nothing${refusal.detail ? `: ${refusal.detail}` : "."}`,
        reload: false,
        hint: "",
      };
    case "starting":
      return {
        message: `This node is still starting and stored nothing${refusal.detail ? `: ${refusal.detail}` : "."}`,
        reload: false,
        hint: "",
      };
    case "unavailable":
      // The engine's own sentence: what stopped it, that nothing was
      // stored, and that waiting clears it.
      return { message: refusal.detail, reload: false, hint: "" };
    case "unsettled":
      // A WRITE THAT STORED ITS REVISION and lost the answer to its
      // activation names it: sending the change again would make a second
      // revision over it if it landed, so the sentence says how to look.
      if (refusal.storedRevisionId) {
        return {
          message: storedUnconfirmedSentence(refusal.storedRevisionId, refusal.node ?? ""),
          reload: true,
          hint: "",
        };
      }
      return {
        message: `${unsettledSentence(refusal)} The change may or may not have landed. Reload before trying again.`,
        reload: true,
        hint: "",
      };
    case "none":
      return { message: "No company is configured yet.", reload: false, hint: "" };
    case "problems":
      return {
        message: refusal.problems.map((p) => p.message).join(" ") || "The engine refused it.",
        reload: false,
        hint: refusal.hint,
      };
  }
}

/**
 * What a write that stored `revision` and got no answer to its activation
 * says: the engine's `503` with `outcome: "unknown"`, in the words of its own
 * remedy. The revision is in the history of the node that stored it alone,
 * so that is where it is reverted to if it is not the company.
 */
export function storedUnconfirmedSentence(revision: string, node: string): string {
  const id = revision.slice(0, 10);
  const where = node ? ` on node ${node}` : "";
  return (
    `The change was stored as revision ${id}${where}, and the fleet did not confirm it, ` +
    `so it may or may not be running. Don't send it again. Reload: if the active revision ` +
    `is not ${id}, revert to ${id} from the history${node ? `, on node ${node}` : ""}.`
  );
}
