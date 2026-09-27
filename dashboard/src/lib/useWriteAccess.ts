/**
 * Whether this browser may make one change, and if not, the one sentence that
 * says why.
 *
 * A WRITE CONTROL IS NEVER HIDDEN. A button that appears only for some readers
 * teaches everybody else that the product cannot do the thing — the founder
 * reading on a phone with no token, the colleague whose token is not bound
 * yet — and says nothing about what would change that. So every control asks
 * here, is drawn for everybody, and is DISABLED with the reason when it cannot
 * act: the sentence is the instruction.
 *
 * FIVE REASONS, in the order a person has to clear them:
 *
 *  - OFFLINE — the socket is down. A change is never queued for later: a
 *    queued write is one the person has walked away from believing it
 *    happened, and the company may have moved under it by the time it goes;
 *  - LOADING — nobody has said who this browser is yet;
 *  - ANONYMOUS — no token, so there is no name to record the change under;
 *  - UNBOUND — a token no seat binds: a credential, not a person, and the
 *    dashboard acts only as a person (ADR-0024);
 *  - NOT SERVED — the engine does not make this change for this person at
 *    all (a company whose tracker is Jira has no native writer): `viewer.acts`
 *    is the engine's own list, never a guess from the tool's name.
 */

import { useConnection } from "./store-hooks.ts";
import { useViewer, type ViewerState } from "./viewer.ts";
import type { ActionTool } from "~/protocol/act.ts";

/** Why a control cannot act, as a value a test can name. */
export type WriteBlock = "offline" | "loading" | "anonymous" | "unbound" | "not_served";

export type WriteAccess =
  | {
      can: true;
      /** The seat the change will be recorded under. */
      as: string;
      /** Every change the engine makes for this person. */
      acts: readonly string[];
    }
  | { can: false; block: WriteBlock; reason: string };

/** The sentence each block is shown as, second person, with its remedy. */
export const WRITE_REASONS: Readonly<Record<WriteBlock, string>> = {
  offline: "Offline — reconnect to make changes. Nothing is queued while you are away.",
  loading: "Checking who you are before anything can be changed.",
  anonymous: "Set an API token to act — every change is recorded under your name.",
  unbound:
    "This token is not bound to a person — set contact.crewlet_operator_id on your seat to act as yourself.",
  not_served: "This engine does not make this change for you.",
};

/** The decision itself, over values — what the hook reads, and what a test pins. */
export function writeAccess(
  tool: ActionTool,
  viewer: ViewerState,
  connected: boolean,
): WriteAccess {
  const blocked = (block: WriteBlock): WriteAccess => ({
    can: false,
    block,
    reason: WRITE_REASONS[block],
  });
  if (!connected) return blocked("offline");
  if (viewer.loading) return blocked("loading");
  if (viewer.anonymous) return blocked("anonymous");
  if (viewer.unbound || viewer.handle === "") return blocked("unbound");
  if (!viewer.acts.includes(tool)) return blocked("not_served");
  return { can: true, as: viewer.handle, acts: viewer.acts };
}

/** Whether this browser may make `tool`'s change now, and why not. */
export function useWriteAccess(tool: ActionTool): WriteAccess {
  const viewer = useViewer();
  const { connected } = useConnection();
  return writeAccess(tool, viewer, connected);
}
